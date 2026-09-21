package sqlite

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
	"github.com/wyze69-sys/SpecCouncil/internal/review"
)

// 1. Guard test: inserting kind='missing' with anchor_unit_id IS NULL aborts with
// 'missing finding requires an anchor'.
func TestFindingKindAnchor_Guard1_MissingRequiresAnchor(t *testing.T) {
	_, writer := openGuardTestStore(t)
	ctx := context.Background()

	snapID := "snap_g1"
	unitID := "eu_g1"
	sessID := "sess_g1"
	roleID := "rr_g1"

	insertSnapshotAndUnit(t, writer, snapID, unitID)
	insertSession(t, writer, sessID, snapID, "reviewing")
	insertRoleRun(t, writer, roleID, sessID, "requirements", "in_flight")

	_, err := writer.ExecContext(ctx, `
		INSERT INTO findings (id, role_run_id, finding_id, kind, severity, category, issue, recommendation, anchor_unit_id, created_at)
		VALUES ('f_g1', ?, 'F-1', 'missing', 'high', 'cat', 'issue', 'rec', NULL, '2026-09-21T12:00:00Z');
	`, roleID)
	if err == nil {
		t.Fatal("expected insert with kind='missing' and null anchor to fail, but succeeded")
	}
	if !strings.Contains(err.Error(), "missing finding requires an anchor") {
		t.Fatalf("expected error containing 'missing finding requires an anchor', got: %v", err)
	}
}

// 2. Guard test: inserting kind='existing' with a non-null anchor aborts with
// 'anchor is only allowed for a missing finding'.
func TestFindingKindAnchor_Guard2_AnchorOnlyForMissing(t *testing.T) {
	_, writer := openGuardTestStore(t)
	ctx := context.Background()

	snapID := "snap_g2"
	unitID := "eu_g2"
	sessID := "sess_g2"
	roleID := "rr_g2"

	insertSnapshotAndUnit(t, writer, snapID, unitID)
	insertSession(t, writer, sessID, snapID, "reviewing")
	insertRoleRun(t, writer, roleID, sessID, "requirements", "in_flight")

	_, err := writer.ExecContext(ctx, `
		INSERT INTO findings (id, role_run_id, finding_id, kind, severity, category, issue, recommendation, anchor_unit_id, created_at)
		VALUES ('f_g2', ?, 'F-1', 'existing', 'high', 'cat', 'issue', 'rec', ?, '2026-09-21T12:00:00Z');
	`, roleID, unitID)
	if err == nil {
		t.Fatal("expected insert with kind='existing' and non-null anchor to fail, but succeeded")
	}
	if !strings.Contains(err.Error(), "anchor is only allowed for a missing finding") {
		t.Fatalf("expected error containing 'anchor is only allowed for a missing finding', got: %v", err)
	}
}

// 3. Guard test: inserting a missing finding whose anchor belongs to a different
// session's snapshot aborts with 'cross-snapshot anchor rejected'.
func TestFindingKindAnchor_Guard3_CrossSnapshotAnchor(t *testing.T) {
	_, writer := openGuardTestStore(t)
	ctx := context.Background()

	snapID1 := "snap_g3_1"
	unitID1 := "eu_g3_1"
	sessID1 := "sess_g3_1"
	roleID1 := "rr_g3_1"

	snapID2 := "snap_g3_2"
	unitID2 := "eu_g3_2"

	insertSnapshotAndUnit(t, writer, snapID1, unitID1)
	insertSnapshotAndUnit(t, writer, snapID2, unitID2)
	insertSession(t, writer, sessID1, snapID1, "reviewing")
	insertRoleRun(t, writer, roleID1, sessID1, "requirements", "in_flight")

	_, err := writer.ExecContext(ctx, `
		INSERT INTO findings (id, role_run_id, finding_id, kind, severity, category, issue, recommendation, anchor_unit_id, created_at)
		VALUES ('f_g3', ?, 'F-1', 'missing', 'high', 'cat', 'issue', 'rec', ?, '2026-09-21T12:00:00Z');
	`, roleID1, unitID2)
	if err == nil {
		t.Fatal("expected insert with cross-snapshot anchor to fail, but succeeded")
	}
	if !strings.Contains(err.Error(), "cross-snapshot anchor rejected") {
		t.Fatalf("expected error containing 'cross-snapshot anchor rejected', got: %v", err)
	}
}

// 4. Guard test: kind='omission' is rejected by the column CHECK.
func TestFindingKindAnchor_Guard4_InvalidKindRejectedByCheck(t *testing.T) {
	_, writer := openGuardTestStore(t)
	ctx := context.Background()

	snapID := "snap_g4"
	unitID := "eu_g4"
	sessID := "sess_g4"
	roleID := "rr_g4"

	insertSnapshotAndUnit(t, writer, snapID, unitID)
	insertSession(t, writer, sessID, snapID, "reviewing")
	insertRoleRun(t, writer, roleID, sessID, "requirements", "in_flight")

	_, err := writer.ExecContext(ctx, `
		INSERT INTO findings (id, role_run_id, finding_id, kind, severity, category, issue, recommendation, anchor_unit_id, created_at)
		VALUES ('f_g4', ?, 'F-1', 'omission', 'high', 'cat', 'issue', 'rec', NULL, '2026-09-21T12:00:00Z');
	`, roleID)
	if err == nil {
		t.Fatal("expected insert with kind='omission' to fail, but succeeded")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "check constraint failed") {
		t.Fatalf("expected error containing 'check constraint failed', got: %v", err)
	}
}

// 6. Publish round-trip: a missing finding with anchor H-1 and no basis refs
// publishes successfully, and ReadReportScoped returns it with Kind == missing
// and AnchorRef == "H-1"; a conflicting finding with two refs returns
// Kind == conflicting and an empty anchor.
func TestFindingKindAnchor_PublishRoundTrip(t *testing.T) {
	store, _ := openGuardTestStore(t)
	ctx := context.Background()

	sessionID := "sess_roundtrip"
	projectID := "proj_roundtrip"

	snapUnits := []evidence.Unit{
		{ID: "H-1", Kind: evidence.UnitRequirement, Text: "Section heading"},
		{ID: "req-1", Kind: evidence.UnitRequirement, Text: "Requirement 1"},
		{ID: "arch-1", Kind: evidence.UnitComponent, Text: "Architecture component"},
	}
	snap, err := evidence.Freeze("snap_"+sessionID, snapUnits)
	if err != nil {
		t.Fatalf("evidence.Freeze: %v", err)
	}

	t0 := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	origClock := clock
	clock = func() time.Time { return t0 }
	defer func() { clock = origClock }()

	_, err = store.Submit(ctx, SubmitParams{
		SessionID:      sessionID,
		ProjectID:      projectID,
		IdempotencyKey: "key_" + sessionID,
		Title:          "Roundtrip Title",
		Content:        "Roundtrip Content",
		Snapshot:       snap,
		CreatedAt:      t0.Add(-10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	claimRes, err := store.ClaimSession(ctx, TimingPolicy{
		DispatchCutoff:      30 * time.Minute,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: 45 * time.Minute,
	})
	if err != nil || !claimRes.Claimed {
		t.Fatalf("claim: %v", err)
	}

	// Publish requirements with one missing finding and one conflicting finding
	reqFindings := []domain.Finding{
		{
			ID:             "F-MISSING",
			Kind:           domain.FindingMissing,
			Severity:       domain.SeverityHigh,
			Category:       "correctness",
			Issue:          "Missing spec detail",
			Recommendation: "Add the detail",
			BasisRefs:      nil,
			AnchorRef:      "H-1",
		},
		{
			ID:             "F-CONFLICTING",
			Kind:           domain.FindingConflicting,
			Severity:       domain.SeverityMedium,
			Category:       "consistency",
			Issue:          "Contradiction in components",
			Recommendation: "Resolve the conflict",
			BasisRefs:      []string{"req-1", "arch-1"},
			AnchorRef:      "",
		},
	}

	for i, role := range domain.Roles {
		res, err := store.ReservePendingRoleWithNow(ctx, sessionID, t0.Add(time.Duration(i)*time.Minute))
		if err != nil || !res.Reserved {
			t.Fatalf("reserve role %s: %v", role, err)
		}
		var findings []domain.Finding
		if role == domain.RoleRequirements {
			findings = reqFindings
		}
		_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
			SessionID:   sessionID,
			RoleRunID:   res.RoleRunID,
			Role:        role,
			CallCount:   1,
			Findings:    findings,
			CompletedAt: t0.Add(time.Duration(i+1) * time.Minute),
		})
		if err != nil {
			t.Fatalf("publish %s: %v", role, err)
		}
	}

	// Compose session
	compRes, err := store.ComposeSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("ComposeSession: %v", err)
	}
	if compRes.Status != domain.SessionComplete {
		t.Fatalf("status = %s, want complete", compRes.Status)
	}

	// ReadReportScoped
	rep, err := store.ReadReportScoped(ctx, projectID, sessionID)
	if err != nil {
		t.Fatalf("ReadReportScoped: %v", err)
	}
	if len(rep.Findings) != 2 {
		t.Fatalf("expected 2 findings in report, got %d", len(rep.Findings))
	}

	var foundMissing, foundConflicting bool
	for _, rf := range rep.Findings {
		f := rf.Finding
		if f.ID == "F-MISSING" {
			foundMissing = true
			if f.Kind != domain.FindingMissing {
				t.Errorf("F-MISSING Kind = %q, want missing", f.Kind)
			}
			if f.AnchorRef != "H-1" {
				t.Errorf("F-MISSING AnchorRef = %q, want H-1", f.AnchorRef)
			}
			if len(f.BasisRefs) != 0 {
				t.Errorf("F-MISSING BasisRefs = %v, want empty", f.BasisRefs)
			}
		}
		if f.ID == "F-CONFLICTING" {
			foundConflicting = true
			if f.Kind != domain.FindingConflicting {
				t.Errorf("F-CONFLICTING Kind = %q, want conflicting", f.Kind)
			}
			if f.AnchorRef != "" {
				t.Errorf("F-CONFLICTING AnchorRef = %q, want empty", f.AnchorRef)
			}
			if len(f.BasisRefs) != 2 {
				t.Errorf("F-CONFLICTING BasisRefs count = %d, want 2", len(f.BasisRefs))
			}
		}
	}
	if !foundMissing {
		t.Error("missing finding not found in report")
	}
	if !foundConflicting {
		t.Error("conflicting finding not found in report")
	}
}

// 7. Publish rejection: each new validation error above is asserted by Field and Message exactly.
func TestFindingKindAnchor_PublishRejection(t *testing.T) {
	cases := []struct {
		name        string
		finding     domain.Finding
		wantField   string
		wantMessage string
	}{
		{
			name: "empty kind",
			finding: domain.Finding{
				ID:             "F-1",
				Kind:           "",
				Severity:       domain.SeverityHigh,
				Category:       "cat",
				Issue:          "issue",
				Recommendation: "rec",
				BasisRefs:      []string{"req-1"},
			},
			wantField:   "findings[0].kind",
			wantMessage: "must not be empty",
		},
		{
			name: "whitespace kind",
			finding: domain.Finding{
				ID:             "F-1",
				Kind:           "  ",
				Severity:       domain.SeverityHigh,
				Category:       "cat",
				Issue:          "issue",
				Recommendation: "rec",
				BasisRefs:      []string{"req-1"},
			},
			wantField:   "findings[0].kind",
			wantMessage: "must not be empty",
		},
		{
			name: "unknown kind",
			finding: domain.Finding{
				ID:             "F-1",
				Kind:           "omission",
				Severity:       domain.SeverityHigh,
				Category:       "cat",
				Issue:          "issue",
				Recommendation: "rec",
				BasisRefs:      []string{"req-1"},
			},
			wantField:   "findings[0].kind",
			wantMessage: `invalid kind "omission"`,
		},
		{
			name: "existing with 0 refs",
			finding: domain.Finding{
				ID:             "F-1",
				Kind:           domain.FindingExisting,
				Severity:       domain.SeverityHigh,
				Category:       "cat",
				Issue:          "issue",
				Recommendation: "rec",
				BasisRefs:      nil,
			},
			wantField:   "findings[0].basis_refs",
			wantMessage: "basis_refs count 0 must be between 1 and 5",
		},
		{
			name: "conflicting with 1 ref",
			finding: domain.Finding{
				ID:             "F-1",
				Kind:           domain.FindingConflicting,
				Severity:       domain.SeverityHigh,
				Category:       "cat",
				Issue:          "issue",
				Recommendation: "rec",
				BasisRefs:      []string{"req-1"},
			},
			wantField:   "findings[0].basis_refs",
			wantMessage: "basis_refs count 1 must be between 2 and 5",
		},
		{
			name: "missing with 6 refs",
			finding: domain.Finding{
				ID:             "F-1",
				Kind:           domain.FindingMissing,
				Severity:       domain.SeverityHigh,
				Category:       "cat",
				Issue:          "issue",
				Recommendation: "rec",
				AnchorRef:      "H-1",
				BasisRefs:      []string{"r1", "r2", "r3", "r4", "r5", "r6"},
			},
			wantField:   "findings[0].basis_refs",
			wantMessage: "basis_refs count 6 must be between 0 and 5",
		},
		{
			name: "existing with anchor",
			finding: domain.Finding{
				ID:             "F-1",
				Kind:           domain.FindingExisting,
				Severity:       domain.SeverityHigh,
				Category:       "cat",
				Issue:          "issue",
				Recommendation: "rec",
				BasisRefs:      []string{"req-1"},
				AnchorRef:      "H-1",
			},
			wantField:   "findings[0].anchor_ref",
			wantMessage: "must be empty unless the finding kind is missing",
		},
		{
			name: "conflicting with anchor",
			finding: domain.Finding{
				ID:             "F-1",
				Kind:           domain.FindingConflicting,
				Severity:       domain.SeverityHigh,
				Category:       "cat",
				Issue:          "issue",
				Recommendation: "rec",
				BasisRefs:      []string{"req-1", "arch-1"},
				AnchorRef:      "H-1",
			},
			wantField:   "findings[0].anchor_ref",
			wantMessage: "must be empty unless the finding kind is missing",
		},
		{
			name: "missing with empty anchor",
			finding: domain.Finding{
				ID:             "F-1",
				Kind:           domain.FindingMissing,
				Severity:       domain.SeverityHigh,
				Category:       "cat",
				Issue:          "issue",
				Recommendation: "rec",
				AnchorRef:      "",
			},
			wantField:   "findings[0].anchor_ref",
			wantMessage: "must not be empty when the finding kind is missing",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := PublishSuccessParams{
				SessionID: "sess-1",
				RoleRunID: "rr-1",
				Role:      domain.RoleRequirements,
				CallCount: 1,
				Findings:  []domain.Finding{tc.finding},
			}
			err := validatePublishSuccessParams(params)
			if err == nil {
				t.Fatalf("%s: expected validation error, got nil", tc.name)
			}
			var pve *PublicationValidationError
			if !errors.As(err, &pve) {
				t.Fatalf("%s: error is not PublicationValidationError: %v", tc.name, err)
			}
			if pve.Field != tc.wantField {
				t.Errorf("%s: Field = %q, want %q", tc.name, pve.Field, tc.wantField)
			}
			if pve.Message != tc.wantMessage {
				t.Errorf("%s: Message = %q, want %q", tc.name, pve.Message, tc.wantMessage)
			}
		})
	}

	t.Run("anchor membership rejection", func(t *testing.T) {
		store, _ := openGuardTestStore(t)
		ctx := context.Background()

		sessionID := "sess_anchor_mem"
		snapUnits := []evidence.Unit{
			{ID: "req-1", Kind: evidence.UnitRequirement, Text: "Req 1"},
		}
		snap, err := evidence.Freeze("snap_"+sessionID, snapUnits)
		if err != nil {
			t.Fatalf("evidence.Freeze: %v", err)
		}

		t0 := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
		origClock := clock
		clock = func() time.Time { return t0 }
		defer func() { clock = origClock }()

		_, err = store.Submit(ctx, SubmitParams{
			SessionID:      sessionID,
			ProjectID:      "proj_anchor_mem",
			IdempotencyKey: "key_" + sessionID,
			Title:          "Title",
			Content:        "Content",
			Snapshot:       snap,
			CreatedAt:      t0.Add(-10 * time.Minute),
		})
		if err != nil {
			t.Fatalf("submit: %v", err)
		}

		claimRes, err := store.ClaimSession(ctx, TimingPolicy{
			DispatchCutoff:      30 * time.Minute,
			CallTimeout:         10 * time.Minute,
			SessionHardDeadline: 45 * time.Minute,
		})
		if err != nil || !claimRes.Claimed {
			t.Fatalf("claim: %v", err)
		}

		dispRes, err := store.ReservePendingRole(ctx, sessionID)
		if err != nil || !dispRes.Reserved {
			t.Fatalf("reserve: %v", err)
		}

		_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
			SessionID: sessionID,
			RoleRunID: dispRes.RoleRunID,
			Role:      dispRes.Role,
			CallCount: 1,
			Findings: []domain.Finding{
				{
					ID:             "F-1",
					Kind:           domain.FindingMissing,
					Severity:       domain.SeverityHigh,
					Category:       "cat",
					Issue:          "issue",
					Recommendation: "rec",
					AnchorRef:      "NOPE",
				},
			},
			CompletedAt: t0.Add(time.Minute),
		})
		if err == nil {
			t.Fatal("expected error for non-existent anchor ref, got nil")
		}
		var pve *PublicationValidationError
		if !errors.As(err, &pve) {
			t.Fatalf("error is not PublicationValidationError: %v", err)
		}
		if pve.Field != "findings[0].anchor_ref" {
			t.Errorf("Field = %q, want findings[0].anchor_ref", pve.Field)
		}
		wantMsg := fmt.Sprintf(`anchor ref "NOPE" does not exist in session snapshot "snap_%s"`, sessionID)
		if pve.Message != wantMsg {
			t.Errorf("Message = %q, want %q", pve.Message, wantMsg)
		}
	})
}

// 8. Ordering: a report containing a missing finding with no refs and an anchor
// sorts deterministically by the anchor as its primary ref — assert the exact
// order of the returned findings, and assert the composed order
// (review.BuildReport) equals the reconstructed order from the store.
func TestFindingKindAnchor_Ordering(t *testing.T) {
	store, _ := openGuardTestStore(t)
	ctx := context.Background()

	sessionID := "sess_ordering"
	projectID := "proj_ordering"

	snapUnits := []evidence.Unit{
		{ID: "A-1", Kind: evidence.UnitRequirement, Text: "Heading A"},
		{ID: "B-1", Kind: evidence.UnitRequirement, Text: "Requirement B"},
		{ID: "C-1", Kind: evidence.UnitRequirement, Text: "Requirement C"},
	}
	snap, err := evidence.Freeze("snap_"+sessionID, snapUnits)
	if err != nil {
		t.Fatalf("evidence.Freeze: %v", err)
	}

	t0 := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	origClock := clock
	clock = func() time.Time { return t0 }
	defer func() { clock = origClock }()

	_, err = store.Submit(ctx, SubmitParams{
		SessionID:      sessionID,
		ProjectID:      projectID,
		IdempotencyKey: "key_" + sessionID,
		Title:          "Ordering Title",
		Content:        "Ordering Content",
		Snapshot:       snap,
		CreatedAt:      t0.Add(-10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	claimRes, err := store.ClaimSession(ctx, TimingPolicy{
		DispatchCutoff:      30 * time.Minute,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: 45 * time.Minute,
	})
	if err != nil || !claimRes.Claimed {
		t.Fatalf("claim: %v", err)
	}

	// Publish findings with same severity, role, and category:
	// F-2 cites B-1 -> primaryRef "B-1"
	// F-1 has Anchor A-1 (no basis refs) -> primaryRef "A-1"
	// F-3 cites C-1 -> primaryRef "C-1"
	reqFindings := []domain.Finding{
		{
			ID:             "F-2",
			Kind:           domain.FindingExisting,
			Severity:       domain.SeverityHigh,
			Category:       "correctness",
			Issue:          "Issue 2",
			Recommendation: "Rec 2",
			BasisRefs:      []string{"B-1"},
		},
		{
			ID:             "F-1",
			Kind:           domain.FindingMissing,
			Severity:       domain.SeverityHigh,
			Category:       "correctness",
			Issue:          "Issue 1",
			Recommendation: "Rec 1",
			BasisRefs:      nil,
			AnchorRef:      "A-1",
		},
		{
			ID:             "F-3",
			Kind:           domain.FindingExisting,
			Severity:       domain.SeverityHigh,
			Category:       "correctness",
			Issue:          "Issue 3",
			Recommendation: "Rec 3",
			BasisRefs:      []string{"C-1"},
		},
	}

	for i, role := range domain.Roles {
		res, err := store.ReservePendingRoleWithNow(ctx, sessionID, t0.Add(time.Duration(i)*time.Minute))
		if err != nil || !res.Reserved {
			t.Fatalf("reserve role %s: %v", role, err)
		}
		var findings []domain.Finding
		if role == domain.RoleRequirements {
			findings = reqFindings
		}
		_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
			SessionID:   sessionID,
			RoleRunID:   res.RoleRunID,
			Role:        role,
			CallCount:   1,
			Findings:    findings,
			CompletedAt: t0.Add(time.Duration(i+1) * time.Minute),
		})
		if err != nil {
			t.Fatalf("publish %s: %v", role, err)
		}
	}

	compRes, err := store.ComposeSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("ComposeSession: %v", err)
	}

	readRep, err := store.ReadReportScoped(ctx, projectID, sessionID)
	if err != nil {
		t.Fatalf("ReadReportScoped: %v", err)
	}

	// 1. Assert exact order of returned findings: F-1, F-2, F-3
	if len(readRep.Findings) != 3 {
		t.Fatalf("expected 3 findings, got %d", len(readRep.Findings))
	}
	wantOrder := []string{"F-1", "F-2", "F-3"}
	for i, wantID := range wantOrder {
		if readRep.Findings[i].Finding.ID != wantID {
			t.Errorf("readRep.Findings[%d].ID = %q, want %q", i, readRep.Findings[i].Finding.ID, wantID)
		}
	}

	// 2. Assert composed order (review.BuildReport) equals reconstructed order
	verdict := review.Verdict{
		Status:              compRes.Report.Status,
		Reason:              compRes.Report.Reason,
		CompletedRoleCount:  compRes.Report.CompletedRoleCount,
		IncompleteRoleCount: compRes.Report.IncompleteRoleCount,
	}
	directRep := review.BuildReport(sessionID, snap.ID, snap.Hash, []review.RoleOutcome{
		{
			Role:     domain.RoleRequirements,
			Status:   domain.RoleComplete,
			Findings: reqFindings,
		},
		{Role: domain.RoleArchitecture, Status: domain.RoleComplete},
		{Role: domain.RoleQA, Status: domain.RoleComplete},
		{Role: domain.RoleSecurity, Status: domain.RoleComplete},
	}, verdict, false)

	if len(directRep.Findings) != len(readRep.Findings) {
		t.Fatalf("directRep findings count %d != read findings count %d", len(directRep.Findings), len(readRep.Findings))
	}
	for i := range readRep.Findings {
		directF := directRep.Findings[i].Finding
		readF := readRep.Findings[i].Finding
		if directF.ID != readF.ID || directF.Kind != readF.Kind || directF.AnchorRef != readF.AnchorRef {
			t.Errorf("finding[%d] mismatch between direct BuildReport (%+v) and read (%+v)", i, directF, readF)
		}
	}
}

// 5. Migration test: exactly 7 applied migrations after Migrate.
func TestFindingKindAnchor_Migration5_AppliedCount(t *testing.T) {
	_, writer := openGuardTestStore(t)
	ctx := context.Background()

	var count int
	err := writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations;").Scan(&count)
	if err != nil {
		t.Fatalf("query schema_migrations count: %v", err)
	}
	if count != 7 {
		t.Fatalf("applied migration count = %d, want 7", count)
	}
}
