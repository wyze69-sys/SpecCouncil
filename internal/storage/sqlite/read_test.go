package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
)

// Helper to seed a complete review session with terminal state and findings.
func seedTerminalSession(t *testing.T, writer *sql.DB, sessID, projID, snapID string, status domain.SessionStatus, reason domain.TerminalReason, cancelReq bool, completedCount int) {
	t.Helper()
	ctx := context.Background()

	now := time.Now().UTC()
	createdStr := now.Add(-10 * time.Minute).Format(time.RFC3339Nano)
	claimedStr := now.Add(-8 * time.Minute).Format(time.RFC3339Nano)
	cutoffStr := now.Add(5 * time.Minute).Format(time.RFC3339Nano)
	deadlineStr := now.Add(10 * time.Minute).Format(time.RFC3339Nano)
	termStr := now.Add(-1 * time.Minute).Format(time.RFC3339Nano)

	cancelInt := 0
	if cancelReq {
		cancelInt = 1
	}
	incompleteCount := domain.RoleCount - completedCount

	// Insert session row directly
	_, err := writer.ExecContext(ctx, `INSERT INTO sessions (
		id, project_id, idempotency_key, request_hash, snapshot_id,
		status, cancel_requested, dispatch_cutoff_at, hard_deadline_at, terminal_reason,
		completed_role_count, incomplete_role_count, created_at, claimed_at, terminal_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);`,
		sessID, projID, "idem-"+sessID, testValidHash, snapID,
		string(status), cancelInt, cutoffStr, deadlineStr, string(reason),
		completedCount, incompleteCount, createdStr, claimedStr, termStr,
	)
	if err != nil {
		t.Fatalf("seed session %s: %v", sessID, err)
	}
}

// 1. Queued and reviewing status reads
func TestReadStatus_QueuedAndReviewing(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_p5_1")
	params := SubmitParams{
		SessionID:      "sess_p5_queued",
		ProjectID:      "proj_p5",
		IdempotencyKey: "idem_p5_q",
		Title:          "Read Models Test",
		Content:        "Design spec content",
		Snapshot:       snap,
	}

	res, err := store.Submit(ctx, params)
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	// Read status of newly submitted (queued) session
	st, err := store.ReadStatus(ctx, res.SessionID)
	if err != nil {
		t.Fatalf("ReadStatus(queued) failed: %v", err)
	}

	if st.SessionID != res.SessionID {
		t.Errorf("SessionID = %q, want %q", st.SessionID, res.SessionID)
	}
	if st.ProjectID != params.ProjectID {
		t.Errorf("ProjectID = %q, want %q", st.ProjectID, params.ProjectID)
	}
	if st.IdempotencyKey != params.IdempotencyKey {
		t.Errorf("IdempotencyKey = %q, want %q", st.IdempotencyKey, params.IdempotencyKey)
	}
	if st.RequestHash != res.RequestHash {
		t.Errorf("RequestHash = %q, want %q", st.RequestHash, res.RequestHash)
	}
	if st.SnapshotID != snap.ID {
		t.Errorf("SnapshotID = %q, want %q", st.SnapshotID, snap.ID)
	}
	if st.SnapshotHash != snap.Hash {
		t.Errorf("SnapshotHash = %q, want %q", st.SnapshotHash, snap.Hash)
	}
	if st.Status != domain.SessionQueued {
		t.Errorf("Status = %q, want %q", st.Status, domain.SessionQueued)
	}
	if st.CancelRequested {
		t.Errorf("CancelRequested should be false")
	}
	if st.CompletedRoleCount != 0 || st.IncompleteRoleCount != 4 {
		t.Errorf("Role counts = (%d, %d), want (0, 4)", st.CompletedRoleCount, st.IncompleteRoleCount)
	}
	if st.TerminalReason != "" {
		t.Errorf("TerminalReason = %q, want empty", st.TerminalReason)
	}
	if st.ClaimedAt != nil || st.DispatchCutoffAt != nil || st.HardDeadlineAt != nil || st.TerminalAt != nil {
		t.Errorf("expected null timing fields for queued session")
	}
	if st.CreatedAt.IsZero() {
		t.Errorf("expected non-zero CreatedAt")
	}
	if len(st.Roles) != 4 {
		t.Fatalf("len(Roles) = %d, want 4", len(st.Roles))
	}
	for i, expectedRole := range domain.Roles {
		r := st.Roles[i]
		if r.Role != expectedRole {
			t.Errorf("Roles[%d].Role = %q, want %q", i, r.Role, expectedRole)
		}
		if r.Status != domain.RolePending {
			t.Errorf("Roles[%d].Status = %q, want %q", i, r.Status, domain.RolePending)
		}
		if r.CallCount != 0 {
			t.Errorf("Roles[%d].CallCount = %d, want 0", i, r.CallCount)
		}
		if r.StartedAt != nil || r.CompletedAt != nil {
			t.Errorf("Roles[%d] has unexpected timestamps", i)
		}
	}

	// Scoped read status matching project
	stScoped, err := store.ReadStatusScoped(ctx, params.ProjectID, res.SessionID)
	if err != nil {
		t.Fatalf("ReadStatusScoped matched failed: %v", err)
	}
	if !reflect.DeepEqual(st, stScoped) {
		t.Errorf("ReadStatus and ReadStatusScoped produced different results")
	}

	// Scoped read status mismatched project -> returns SessionNotFoundError
	_, err = store.ReadStatusScoped(ctx, "foreign_project", res.SessionID)
	if err == nil {
		t.Fatalf("expected error for mismatched project")
	}
	var notFoundErr *SessionNotFoundError
	if !errors.As(err, &notFoundErr) {
		t.Errorf("expected SessionNotFoundError, got %T: %v", err, err)
	}
	if !errors.Is(err, ErrNotFound) || !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("expected errors.Is(ErrNotFound), got %v", err)
	}
	if notFoundErr.ProjectID != "foreign_project" || notFoundErr.SessionID != res.SessionID {
		t.Errorf("notFoundErr fields: (%q, %q)", notFoundErr.ProjectID, notFoundErr.SessionID)
	}

	// Transition session to reviewing
	now := time.Now().UTC()
	claimedStr := now.Format(time.RFC3339Nano)
	cutoffStr := now.Add(5 * time.Minute).Format(time.RFC3339Nano)
	deadlineStr := now.Add(10 * time.Minute).Format(time.RFC3339Nano)

	_, err = writer.ExecContext(ctx, `UPDATE sessions SET
		status = 'reviewing', claimed_at = ?, dispatch_cutoff_at = ?, hard_deadline_at = ?
		WHERE id = ?;`,
		claimedStr, cutoffStr, deadlineStr, res.SessionID,
	)
	if err != nil {
		t.Fatalf("transition to reviewing: %v", err)
	}

	// Update first role to in_flight
	reqRunID := fmt.Sprintf("%s:%s", res.SessionID, domain.RoleRequirements)
	_, err = writer.ExecContext(ctx, `UPDATE role_runs SET
		status = 'in_flight', started_at = ? WHERE id = ?;`,
		claimedStr, reqRunID,
	)
	if err != nil {
		t.Fatalf("transition role to in_flight: %v", err)
	}

	stRev, err := store.ReadStatus(ctx, res.SessionID)
	if err != nil {
		t.Fatalf("ReadStatus(reviewing) failed: %v", err)
	}
	if stRev.Status != domain.SessionReviewing {
		t.Errorf("Status = %q, want %q", stRev.Status, domain.SessionReviewing)
	}
	if stRev.ClaimedAt == nil || stRev.DispatchCutoffAt == nil || stRev.HardDeadlineAt == nil {
		t.Errorf("expected non-nil timing fields for reviewing session")
	}
	if stRev.Roles[0].Status != domain.RoleInFlight {
		t.Errorf("Requirements role status = %q, want %q", stRev.Roles[0].Status, domain.RoleInFlight)
	}
	if stRev.Roles[0].StartedAt == nil {
		t.Errorf("Requirements role started_at should be non-nil")
	}
}

// 2. Terminal complete, partial, and failed status reads
func TestReadStatus_TerminalStates(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_term")
	submitParams := SubmitParams{
		SessionID:      "sess_term_base",
		ProjectID:      "proj_term",
		IdempotencyKey: "idem_term",
		Title:          "Terminal Test",
		Content:        "Spec content",
		Snapshot:       snap,
	}
	if _, err := store.Submit(ctx, submitParams); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// 2a. Terminal Complete
	seedTerminalSession(t, writer, "sess_complete", "proj_term", snap.ID,
		domain.SessionComplete, domain.ReasonAllRolesComplete, false, 4)
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	for _, role := range domain.Roles {
		rrID := fmt.Sprintf("sess_complete:%s", role)
		_, err := writer.ExecContext(ctx, `INSERT INTO role_runs (
			id, session_id, role, status, call_count, started_at, completed_at, created_at
		) VALUES (?, 'sess_complete', ?, 'complete', 1, ?, ?, ?);`,
			rrID, role.String(), nowStr, nowStr, nowStr,
		)
		if err != nil {
			t.Fatalf("insert complete role %s: %v", role, err)
		}
	}

	stComp, err := store.ReadStatus(ctx, "sess_complete")
	if err != nil {
		t.Fatalf("ReadStatus(complete): %v", err)
	}
	if stComp.Status != domain.SessionComplete {
		t.Errorf("Status = %q, want complete", stComp.Status)
	}
	if stComp.TerminalReason != domain.ReasonAllRolesComplete {
		t.Errorf("TerminalReason = %q, want %q", stComp.TerminalReason, domain.ReasonAllRolesComplete)
	}
	if stComp.CompletedRoleCount != 4 || stComp.IncompleteRoleCount != 0 {
		t.Errorf("counts = (%d, %d), want (4, 0)", stComp.CompletedRoleCount, stComp.IncompleteRoleCount)
	}
	if stComp.TerminalAt == nil {
		t.Errorf("expected non-nil TerminalAt")
	}

	// 2b. Terminal Partial (user_cancelled)
	seedTerminalSession(t, writer, "sess_partial", "proj_term", snap.ID,
		domain.SessionPartial, domain.ReasonUserCancelled, true, 2)
	// Roles: 2 complete, 2 interrupted
	for i, role := range domain.Roles {
		rrID := fmt.Sprintf("sess_partial:%s", role)
		if i < 2 {
			_, err := writer.ExecContext(ctx, `INSERT INTO role_runs (
				id, session_id, role, status, call_count, started_at, completed_at, created_at
			) VALUES (?, 'sess_partial', ?, 'complete', 1, ?, ?, ?);`,
				rrID, role.String(), nowStr, nowStr, nowStr,
			)
			if err != nil {
				t.Fatalf("insert role %s: %v", role, err)
			}
		} else {
			_, err := writer.ExecContext(ctx, `INSERT INTO role_runs (
				id, session_id, role, status, cause, call_count, completed_at, created_at
			) VALUES (?, 'sess_partial', ?, 'interrupted', 'user_cancelled', 0, ?, ?);`,
				rrID, role.String(), nowStr, nowStr,
			)
			if err != nil {
				t.Fatalf("insert role %s: %v", role, err)
			}
		}
	}

	stPart, err := store.ReadStatus(ctx, "sess_partial")
	if err != nil {
		t.Fatalf("ReadStatus(partial): %v", err)
	}
	if stPart.Status != domain.SessionPartial {
		t.Errorf("Status = %q, want partial", stPart.Status)
	}
	if !stPart.CancelRequested {
		t.Errorf("CancelRequested should be true")
	}
	if stPart.TerminalReason != domain.ReasonUserCancelled {
		t.Errorf("TerminalReason = %q, want %q", stPart.TerminalReason, domain.ReasonUserCancelled)
	}
	if stPart.CompletedRoleCount != 2 || stPart.IncompleteRoleCount != 2 {
		t.Errorf("counts = (%d, %d), want (2, 2)", stPart.CompletedRoleCount, stPart.IncompleteRoleCount)
	}

	// 2c. Terminal Failed (role_failures)
	seedTerminalSession(t, writer, "sess_failed", "proj_term", snap.ID,
		domain.SessionFailed, domain.ReasonRoleFailures, false, 0)
	for _, role := range domain.Roles {
		rrID := fmt.Sprintf("sess_failed:%s", role)
		_, err := writer.ExecContext(ctx, `INSERT INTO role_runs (
			id, session_id, role, status, error_category, call_count, completed_at, created_at
		) VALUES (?, 'sess_failed', ?, 'failed', 'timeout', 1, ?, ?);`,
			rrID, role.String(), nowStr, nowStr,
		)
		if err != nil {
			t.Fatalf("insert failed role %s: %v", role, err)
		}
	}

	stFail, err := store.ReadStatus(ctx, "sess_failed")
	if err != nil {
		t.Fatalf("ReadStatus(failed): %v", err)
	}
	if stFail.Status != domain.SessionFailed {
		t.Errorf("Status = %q, want failed", stFail.Status)
	}
	if stFail.TerminalReason != domain.ReasonRoleFailures {
		t.Errorf("TerminalReason = %q, want %q", stFail.TerminalReason, domain.ReasonRoleFailures)
	}
	if stFail.CompletedRoleCount != 0 || stFail.IncompleteRoleCount != 4 {
		t.Errorf("counts = (%d, %d), want (0, 4)", stFail.CompletedRoleCount, stFail.IncompleteRoleCount)
	}
}

// 3. Unknown session and non-terminal report errors
func TestReadReport_TerminalGateAndErrors(t *testing.T) {
	store, _ := openGuardTestStore(t)
	ctx := context.Background()

	// 3a. Unknown session for ReadReport
	_, err := store.ReadReport(ctx, "non_existent_session")
	if err == nil {
		t.Fatalf("expected error for non-existent session")
	}
	var notFoundErr *SessionNotFoundError
	if !errors.As(err, &notFoundErr) {
		t.Errorf("expected SessionNotFoundError, got %T: %v", err, err)
	}
	if !errors.Is(err, ErrNotFound) || !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("expected errors.Is(ErrNotFound), got %v", err)
	}

	// 3b. Non-terminal report request: Queued
	snap := createTestFrozenSnapshot(t, "snap_gate")
	res, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_gate_queued",
		ProjectID:      "proj_gate",
		IdempotencyKey: "idem_gate_q",
		Title:          "Gate Test",
		Content:        "Gate content",
		Snapshot:       snap,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	_, err = store.ReadReport(ctx, res.SessionID)
	if err == nil {
		t.Fatalf("expected error when reading report for queued session")
	}
	var notTerminalErr *SessionNotTerminalError
	if !errors.As(err, &notTerminalErr) {
		t.Errorf("expected SessionNotTerminalError, got %T: %v", err, err)
	}
	if !errors.Is(err, ErrNotTerminal) || !errors.Is(err, ErrNotFinished) {
		t.Errorf("expected errors.Is(ErrNotTerminal), got %v", err)
	}
	if notTerminalErr.Status != domain.SessionQueued {
		t.Errorf("reported status = %q, want queued", notTerminalErr.Status)
	}

	// Package-level alias functions
	_, err = ReadReport(ctx, store, res.SessionID)
	if !errors.Is(err, ErrNotTerminal) {
		t.Errorf("ReadReport package function expected ErrNotTerminal, got %v", err)
	}
	_, err = ReadTerminalReport(ctx, store, res.SessionID)
	if !errors.Is(err, ErrNotTerminal) {
		t.Errorf("ReadTerminalReport package function expected ErrNotTerminal, got %v", err)
	}
}

// 4. Deterministic role order, finding order, and basis ordinal order despite insertion order
func TestReadReport_DeterministicOrdering(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	// Create snapshot with units: eu-alpha, eu-beta, eu-gamma
	units := []evidence.Unit{
		{ID: "eu-alpha", Kind: evidence.UnitRequirement, Text: "Alpha requirement"},
		{ID: "eu-beta", Kind: evidence.UnitComponent, Text: "Beta component"},
		{ID: "eu-gamma", Kind: evidence.UnitConstraint, Text: "Gamma constraint"},
		{ID: "eu-delta", Kind: evidence.UnitDataRule, Text: "Delta rule"},
	}
	snap, err := evidence.Freeze("snap_order", units)
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}

	// Insert snapshot and units
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = writer.ExecContext(ctx, `INSERT INTO snapshots (
		id, hash, project_id, title, content, normalization_version, created_at
	) VALUES (?, ?, 'proj_ord', 'Order Test', 'Content', 1, ?);`, snap.ID, snap.Hash, nowStr)
	if err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}
	for i, u := range snap.Units {
		euPK := fmt.Sprintf("%s:%s", snap.ID, u.ID)
		_, err = writer.ExecContext(ctx, `INSERT INTO evidence_units (
			id, snapshot_id, unit_id, ordinal, kind, text
		) VALUES (?, ?, ?, ?, ?, ?);`, euPK, snap.ID, u.ID, i, string(u.Kind), u.Text)
		if err != nil {
			t.Fatalf("insert evidence unit: %v", err)
		}
	}

	sessID := "sess_order_test"
	seedTerminalSession(t, writer, sessID, "proj_ord", snap.ID,
		domain.SessionComplete, domain.ReasonAllRolesComplete, false, 4)

	// Insert roles in reverse order: security, qa, architecture, requirements (in_flight to accept findings)
	reverseRoles := []domain.Role{domain.RoleSecurity, domain.RoleQA, domain.RoleArchitecture, domain.RoleRequirements}
	for _, r := range reverseRoles {
		rrID := fmt.Sprintf("%s:%s", sessID, r)
		_, err := writer.ExecContext(ctx, `INSERT INTO role_runs (
			id, session_id, role, status, call_count, started_at, created_at
		) VALUES (?, ?, ?, 'in_flight', 1, ?, ?);`, rrID, sessID, r.String(), nowStr, nowStr)
		if err != nil {
			t.Fatalf("insert role %s: %v", r, err)
		}
	}

	// Insert findings with scrambled properties to test total order:
	// Total order: severity rank -> role rank -> primary basis_ref -> category -> finding id
	//
	// We will create 5 findings:
	// Finding A: Role Architecture, Severity High, category "perf", id "find-01", basis [eu-beta]
	// Finding B: Role Requirements, Severity High, category "sec",  id "find-02", basis [eu-alpha]
	// Finding C: Role Security,     Severity Critical, category "auth", id "find-03", basis [eu-gamma]
	// Finding D: Role Requirements, Severity High, category "sec",  id "find-04", basis [eu-alpha] (same primary basis ref, same role, same cat -> ties broken by ID)
	// Finding E: Role QA,           Severity Low,  category "test", id "find-05", basis [eu-delta, eu-alpha] (basis refs inserted in reverse ordinal: ordinal 2 then 1)

	reqRunID := fmt.Sprintf("%s:%s", sessID, domain.RoleRequirements)
	archRunID := fmt.Sprintf("%s:%s", sessID, domain.RoleArchitecture)
	qaRunID := fmt.Sprintf("%s:%s", sessID, domain.RoleQA)
	secRunID := fmt.Sprintf("%s:%s", sessID, domain.RoleSecurity)

	insertFinding := func(fPK, rrID, fID, sev, cat, issue, rec string) {
		_, err := writer.ExecContext(ctx, `INSERT INTO findings (
			id, role_run_id, finding_id, severity, category, issue, recommendation, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?);`, fPK, rrID, fID, sev, cat, issue, rec, nowStr)
		if err != nil {
			t.Fatalf("insert finding %s: %v", fID, err)
		}
	}

	insertBasisRef := func(refPK, fPK, unitID string, ordinal int) {
		euPK := fmt.Sprintf("%s:%s", snap.ID, unitID)
		_, err := writer.ExecContext(ctx, `INSERT INTO finding_basis_refs (
			id, finding_id, evidence_unit_id, ordinal
		) VALUES (?, ?, ?, ?);`, refPK, fPK, euPK, ordinal)
		if err != nil {
			t.Fatalf("insert basis ref for %s (ord %d): %v", fPK, ordinal, err)
		}
	}

	// Insert in non-sorted order:
	// 1. Finding A (Arch, High)
	insertFinding("f_A", archRunID, "find-01", "high", "perf", "Issue A", "Rec A")
	insertBasisRef("fbr_A1", "f_A", "eu-beta", 1)

	// 2. Finding E (QA, Low) with basis refs inserted out-of-order: ordinal 2 inserted first, then ordinal 1!
	insertFinding("f_E", qaRunID, "find-05", "low", "test", "Issue E", "Rec E")
	insertBasisRef("fbr_E2", "f_E", "eu-delta", 2)
	insertBasisRef("fbr_E1", "f_E", "eu-alpha", 1)

	// 3. Finding C (Sec, Critical)
	insertFinding("f_C", secRunID, "find-03", "critical", "auth", "Issue C", "Rec C")
	insertBasisRef("fbr_C1", "f_C", "eu-gamma", 1)

	// 4. Finding D (Req, High, "find-04")
	insertFinding("f_D", reqRunID, "find-04", "high", "sec", "Issue D", "Rec D")
	insertBasisRef("fbr_D1", "f_D", "eu-alpha", 1)

	// 5. Finding B (Req, High, "find-02")
	insertFinding("f_B", reqRunID, "find-02", "high", "sec", "Issue B", "Rec B")
	insertBasisRef("fbr_B1", "f_B", "eu-alpha", 1)

	// Transition all roles to complete now that findings are inserted
	_, err = writer.ExecContext(ctx, `UPDATE role_runs SET status = 'complete', completed_at = ? WHERE session_id = ?;`, nowStr, sessID)
	if err != nil {
		t.Fatalf("transition roles to complete: %v", err)
	}

	// Read terminal report
	rep, err := store.ReadReport(ctx, sessID)
	if err != nil {
		t.Fatalf("ReadReport failed: %v", err)
	}

	// 1. Check deterministic role order: Requirements, Architecture, QA, Security
	if len(rep.Roles) != 4 {
		t.Fatalf("expected 4 roles, got %d", len(rep.Roles))
	}
	expectedRoles := []domain.Role{domain.RoleRequirements, domain.RoleArchitecture, domain.RoleQA, domain.RoleSecurity}
	for i, r := range rep.Roles {
		if r.Role != expectedRoles[i] {
			t.Errorf("rep.Roles[%d].Role = %q, want %q", i, r.Role, expectedRoles[i])
		}
	}

	// 2. Check basis refs for Finding E were sorted strictly by ordinal ASC despite out-of-order insert:
	// ordinal 1: eu-alpha, ordinal 2: eu-delta
	var findE *domain.Finding
	for _, rf := range rep.Findings {
		if rf.Finding.ID == "find-05" {
			findE = &rf.Finding
			break
		}
	}
	if findE == nil {
		t.Fatalf("find-05 not found in report findings")
	}
	if len(findE.BasisRefs) != 2 || findE.BasisRefs[0] != "eu-alpha" || findE.BasisRefs[1] != "eu-delta" {
		t.Errorf("find-05 BasisRefs = %v, want [eu-alpha, eu-delta]", findE.BasisRefs)
	}

	// 3. Check total order of findings:
	// Expected order:
	// 1st: find-03 (Critical, Sec)
	// 2nd: find-02 (High, Req, basis eu-alpha, cat sec, id find-02)
	// 3rd: find-04 (High, Req, basis eu-alpha, cat sec, id find-04) -- tie with find-02 broken by finding id
	// 4th: find-01 (High, Arch, basis eu-beta, cat perf) -- role order (Req before Arch) puts it after find-02/04
	// 5th: find-05 (Low, QA)
	if len(rep.Findings) != 5 {
		t.Fatalf("expected 5 findings, got %d", len(rep.Findings))
	}
	expectedOrder := []string{"find-03", "find-02", "find-04", "find-01", "find-05"}
	for i, wantID := range expectedOrder {
		gotID := rep.Findings[i].Finding.ID
		if gotID != wantID {
			t.Errorf("Finding[%d] = %q, want %q", i, gotID, wantID)
		}
	}
}

// 5. Failed/interrupted roles contribute no findings
func TestReadReport_FailedAndInterruptedRolesOmitFindings(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_filter")
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = writer.ExecContext(ctx, `INSERT INTO snapshots (id, hash, project_id, title, content, normalization_version, created_at)
		VALUES (?, ?, 'proj_fil', 'Title', 'Content', 1, ?);`, snap.ID, snap.Hash, nowStr)
	for i, u := range snap.Units {
		euPK := fmt.Sprintf("%s:%s", snap.ID, u.ID)
		_, _ = writer.ExecContext(ctx, `INSERT INTO evidence_units (id, snapshot_id, unit_id, ordinal, kind, text)
			VALUES (?, ?, ?, ?, ?, ?);`, euPK, snap.ID, u.ID, i, string(u.Kind), u.Text)
	}

	sessID := "sess_filter_findings"
	seedTerminalSession(t, writer, sessID, "proj_fil", snap.ID,
		domain.SessionPartial, domain.ReasonUserCancelled, true, 1)

	// Roles:
	// Requirements: complete
	// Architecture: failed
	// QA: interrupted
	// Security: interrupted
	reqRunID := fmt.Sprintf("%s:%s", sessID, domain.RoleRequirements)
	archRunID := fmt.Sprintf("%s:%s", sessID, domain.RoleArchitecture)
	qaRunID := fmt.Sprintf("%s:%s", sessID, domain.RoleQA)
	secRunID := fmt.Sprintf("%s:%s", sessID, domain.RoleSecurity)

	_, _ = writer.ExecContext(ctx, `INSERT INTO role_runs (id, session_id, role, status, call_count, started_at, created_at)
		VALUES (?, ?, 'requirements', 'in_flight', 1, ?, ?);`, reqRunID, sessID, nowStr, nowStr)
	_, _ = writer.ExecContext(ctx, `INSERT INTO role_runs (id, session_id, role, status, call_count, started_at, created_at)
		VALUES (?, ?, 'architecture', 'in_flight', 1, ?, ?);`, archRunID, sessID, nowStr, nowStr)
	_, _ = writer.ExecContext(ctx, `INSERT INTO role_runs (id, session_id, role, status, cause, call_count, completed_at, created_at)
		VALUES (?, ?, 'qa', 'interrupted', 'user_cancelled', 0, ?, ?);`, qaRunID, sessID, nowStr, nowStr)
	_, _ = writer.ExecContext(ctx, `INSERT INTO role_runs (id, session_id, role, status, cause, call_count, completed_at, created_at)
		VALUES (?, ?, 'security', 'interrupted', 'user_cancelled', 0, ?, ?);`, secRunID, sessID, nowStr, nowStr)

	// Add 1 finding to Requirements (while in_flight)
	euPK := fmt.Sprintf("%s:%s", snap.ID, snap.Units[0].ID)
	_, _ = writer.ExecContext(ctx, `INSERT INTO findings (id, role_run_id, finding_id, severity, category, issue, recommendation, created_at)
		VALUES ('f_req', ?, 'find-req-1', 'high', 'req', 'Req issue', 'Req rec', ?);`, reqRunID, nowStr)
	_, _ = writer.ExecContext(ctx, `INSERT INTO finding_basis_refs (id, finding_id, evidence_unit_id, ordinal)
		VALUES ('fbr_req', 'f_req', ?, 1);`, euPK)

	// Insert findings to Architecture while in_flight before failing (simulating findings on a role that failed)
	_, _ = writer.ExecContext(ctx, `INSERT INTO findings (id, role_run_id, finding_id, severity, category, issue, recommendation, created_at)
		VALUES ('f_arch_rogue', ?, 'find-arch-rogue', 'critical', 'arch', 'Arch issue', 'Arch rec', ?);`, archRunID, nowStr)
	_, _ = writer.ExecContext(ctx, `INSERT INTO finding_basis_refs (id, finding_id, evidence_unit_id, ordinal)
		VALUES ('fbr_arch', 'f_arch_rogue', ?, 1);`, euPK)

	// Transition requirements to complete and architecture to failed
	_, _ = writer.ExecContext(ctx, `UPDATE role_runs SET status = 'complete', completed_at = ? WHERE id = ?;`, nowStr, reqRunID)
	_, _ = writer.ExecContext(ctx, `UPDATE role_runs SET status = 'failed', completed_at = ?, error_category = 'timeout' WHERE id = ?;`, nowStr, archRunID)

	rep, err := store.ReadReport(ctx, sessID)
	if err != nil {
		t.Fatalf("ReadReport: %v", err)
	}

	// Finding count should be exactly 1 (only Requirements)
	if len(rep.Findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(rep.Findings))
	}
	if rep.Findings[0].Finding.ID != "find-req-1" {
		t.Errorf("Finding ID = %q, want find-req-1", rep.Findings[0].Finding.ID)
	}

	// Architecture role summary finding count should be 0
	for _, r := range rep.Roles {
		if r.Role == domain.RoleArchitecture {
			if r.FindingCount != 0 {
				t.Errorf("failed Architecture role has FindingCount = %d, want 0", r.FindingCount)
			}
		}
	}
}

// 6. Persisted cancellation and count preservation
func TestRead_PersistedFieldsPreserved(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_fields")
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = writer.ExecContext(ctx, `INSERT INTO snapshots (id, hash, project_id, title, content, normalization_version, created_at)
		VALUES (?, ?, 'proj_pres', 'Title', 'Content', 1, ?);`, snap.ID, snap.Hash, nowStr)

	// Persist a session where cancel_requested is true, but session reached complete with reason all_roles_complete
	// (e.g. cancel arrived after dispatch completed). The reader must preserve exactly what is persisted.
	seedTerminalSession(t, writer, "sess_cancel_complete", "proj_pres", snap.ID,
		domain.SessionComplete, domain.ReasonAllRolesComplete, true, 4)

	for _, r := range domain.Roles {
		rrID := fmt.Sprintf("sess_cancel_complete:%s", r)
		_, _ = writer.ExecContext(ctx, `INSERT INTO role_runs (id, session_id, role, status, call_count, started_at, completed_at, created_at)
			VALUES (?, 'sess_cancel_complete', ?, 'complete', 1, ?, ?, ?);`, rrID, r.String(), nowStr, nowStr, nowStr)
	}

	// 1. Status read preserves fields
	st, err := store.ReadStatus(ctx, "sess_cancel_complete")
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if !st.CancelRequested {
		t.Errorf("CancelRequested = false, want true")
	}
	if st.Status != domain.SessionComplete {
		t.Errorf("Status = %q, want complete", st.Status)
	}
	if st.TerminalReason != domain.ReasonAllRolesComplete {
		t.Errorf("TerminalReason = %q, want all_roles_complete", st.TerminalReason)
	}
	if st.CompletedRoleCount != 4 || st.IncompleteRoleCount != 0 {
		t.Errorf("Counts = (%d, %d), want (4, 0)", st.CompletedRoleCount, st.IncompleteRoleCount)
	}

	// 2. Report read preserves fields
	rep, err := store.ReadReport(ctx, "sess_cancel_complete")
	if err != nil {
		t.Fatalf("ReadReport: %v", err)
	}
	if !rep.CancelRequested {
		t.Errorf("CancelRequested = false, want true")
	}
	if rep.Status != domain.SessionComplete {
		t.Errorf("Status = %q, want complete", rep.Status)
	}
	if rep.Reason != domain.ReasonAllRolesComplete {
		t.Errorf("Reason = %q, want all_roles_complete", rep.Reason)
	}
	if rep.CompletedRoleCount != 4 || rep.IncompleteRoleCount != 0 {
		t.Errorf("Counts = (%d, %d), want (4, 0)", rep.CompletedRoleCount, rep.IncompleteRoleCount)
	}
}

// 7. Malformed persisted timestamps/enums return errors
func TestRead_MalformedDataReturnsError(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_mal")
	res, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_mal",
		ProjectID:      "proj_mal",
		IdempotencyKey: "idem_mal",
		Title:          "Malformed Test",
		Content:        "Spec content",
		Snapshot:       snap,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	t.Run("timestamp missing Z suffix fails closed", func(t *testing.T) {
		// Disable trigger temporarily or update allowed column
		// created_at requires LIKE '%Z' and length >= 20. If we update to invalid format that somehow passed or corrupted:
		// SQLite CHECK enforces LIKE '%Z', but let's test parseUTCTimestamp directly and with corrupt DB row.
		_, err := parseUTCTimestamp("2026-09-19T12:00:00+00:00")
		if err == nil {
			t.Errorf("expected error for timestamp without 'Z'")
		}
		_, err = parseUTCTimestamp("invalid-time-string-ZZZ")
		if err == nil {
			t.Errorf("expected error for unparseable timestamp")
		}
	})

	t.Run("corrupted snapshot hash returns ErrSnapshotCorrupted", func(t *testing.T) {
		const corruptSnapID = "snap_corrupt"
		const badHash = "0000000000000000000000000000000000000000000000000000000000000000"
		nowStr := time.Now().UTC().Format(time.RFC3339Nano)
		_, err := writer.ExecContext(ctx, `INSERT INTO snapshots (
			id, hash, project_id, title, content, normalization_version, created_at
		) VALUES (?, ?, 'proj_corrupt', 'Corrupt Title', 'Content', 1, ?);`, corruptSnapID, badHash, nowStr)
		if err != nil {
			t.Fatalf("insert corrupt snapshot: %v", err)
		}
		_, err = writer.ExecContext(ctx, `INSERT INTO evidence_units (
			id, snapshot_id, unit_id, ordinal, kind, text
		) VALUES ('corrupt_eu1', ?, 'unit-1', 0, 'requirement', 'text');`, corruptSnapID)
		if err != nil {
			t.Fatalf("insert corrupt unit: %v", err)
		}

		_, err = store.ReadSnapshot(ctx, corruptSnapID)
		if err == nil {
			t.Fatalf("expected error for corrupted snapshot hash")
		}
		if !errors.Is(err, ErrSnapshotCorrupted) {
			t.Errorf("expected ErrSnapshotCorrupted, got: %v", err)
		}
	})

	t.Run("missing canonical role returns ErrMalformedData", func(t *testing.T) {
		// Delete one role run for the session
		secID := fmt.Sprintf("%s:%s", res.SessionID, domain.RoleSecurity)
		_, err := writer.ExecContext(ctx, `DELETE FROM role_runs WHERE id = ?;`, secID)
		if err != nil {
			t.Fatalf("delete role: %v", err)
		}

		_, err = store.ReadStatus(ctx, res.SessionID)
		if err == nil {
			t.Fatalf("expected error when canonical role is missing")
		}
		if !errors.Is(err, ErrMalformedData) {
			t.Errorf("expected ErrMalformedData, got: %v", err)
		}
	})
}

// 8. All reads work through the read-only pool
func TestRead_ReadOnlyPoolIsolation(t *testing.T) {
	store, _ := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_ro_test")
	res, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_ro_iso",
		ProjectID:      "proj_ro",
		IdempotencyKey: "idem_ro",
		Title:          "RO Isolation",
		Content:        "Spec content",
		Snapshot:       snap,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// 1. Close writer pool to guarantee writer cannot be called
	store.mu.Lock()
	_ = store.writer.Close()
	store.mu.Unlock()

	// 2. ReadStatus succeeds using only reader pool
	st, err := store.ReadStatus(ctx, res.SessionID)
	if err != nil {
		t.Fatalf("ReadStatus failed after writer closed: %v", err)
	}
	if st.SessionID != res.SessionID {
		t.Errorf("SessionID = %q, want %q", st.SessionID, res.SessionID)
	}

	// 3. ReadSnapshot succeeds using only reader pool
	snapRead, err := store.ReadSnapshot(ctx, snap.ID)
	if err != nil {
		t.Fatalf("ReadSnapshot failed after writer closed: %v", err)
	}
	if snapRead.Hash != snap.Hash {
		t.Errorf("Snapshot hash mismatch: %q != %q", snapRead.Hash, snap.Hash)
	}

	// 4. Prove writes attempted on reader pool fail with read-only database error
	reader, err := store.readerDB()
	if err != nil {
		t.Fatalf("readerDB: %v", err)
	}
	_, err = reader.ExecContext(ctx, `INSERT INTO snapshots (id, hash, project_id, title, content, normalization_version, created_at)
		VALUES ('rogue', '1111111111111111111111111111111111111111111111111111111111111111', 'p', 't', 'c', 1, '2026-09-19T12:00:00Z');`)
	if err == nil {
		t.Fatalf("expected write on reader pool to fail")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "readonly") {
		t.Errorf("expected readonly error, got: %v", err)
	}
}

// 9. Prove reads do not mutate any table, timestamp, or row count
func TestRead_ProvesNoMutation(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_nomut")
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = writer.ExecContext(ctx, `INSERT INTO snapshots (id, hash, project_id, title, content, normalization_version, created_at)
		VALUES (?, ?, 'proj_nomut', 'Title', 'Content', 1, ?);`, snap.ID, snap.Hash, nowStr)
	for i, u := range snap.Units {
		euPK := fmt.Sprintf("%s:%s", snap.ID, u.ID)
		_, _ = writer.ExecContext(ctx, `INSERT INTO evidence_units (id, snapshot_id, unit_id, ordinal, kind, text)
			VALUES (?, ?, ?, ?, ?, ?);`, euPK, snap.ID, u.ID, i, string(u.Kind), u.Text)
	}

	sessID := "sess_nomut"
	seedTerminalSession(t, writer, sessID, "proj_nomut", snap.ID,
		domain.SessionComplete, domain.ReasonAllRolesComplete, false, 4)
	for _, r := range domain.Roles {
		rrID := fmt.Sprintf("%s:%s", sessID, r)
		_, _ = writer.ExecContext(ctx, `INSERT INTO role_runs (id, session_id, role, status, call_count, started_at, completed_at, created_at)
			VALUES (?, ?, ?, 'complete', 1, ?, ?, ?);`, rrID, sessID, r.String(), nowStr, nowStr, nowStr)
	}

	tables := []string{"snapshots", "evidence_units", "sessions", "role_runs", "findings", "finding_basis_refs", "schema_migrations"}
	countAll := func() map[string]int {
		counts := make(map[string]int)
		for _, tbl := range tables {
			var cnt int
			_ = writer.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s;", tbl)).Scan(&cnt)
			counts[tbl] = cnt
		}
		return counts
	}

	var dataVerBefore int
	_ = writer.QueryRowContext(ctx, "PRAGMA data_version;").Scan(&dataVerBefore)
	countsBefore := countAll()

	// Execute reads repeatedly
	for i := 0; i < 10; i++ {
		_, err := store.ReadStatus(ctx, sessID)
		if err != nil {
			t.Fatalf("ReadStatus %d: %v", i, err)
		}
		_, err = store.ReadReport(ctx, sessID)
		if err != nil {
			t.Fatalf("ReadReport %d: %v", i, err)
		}
		_, err = store.ReadSnapshot(ctx, snap.ID)
		if err != nil {
			t.Fatalf("ReadSnapshot %d: %v", i, err)
		}
	}

	var dataVerAfter int
	_ = writer.QueryRowContext(ctx, "PRAGMA data_version;").Scan(&dataVerAfter)
	countsAfter := countAll()

	if dataVerBefore != dataVerAfter {
		t.Errorf("PRAGMA data_version changed from %d to %d", dataVerBefore, dataVerAfter)
	}
	if !reflect.DeepEqual(countsBefore, countsAfter) {
		t.Errorf("Row counts changed: before=%v, after=%v", countsBefore, countsAfter)
	}
}

// 10. Repeated reads return identical models
func TestRead_RepeatedReadsIdentical(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_repeat")
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = writer.ExecContext(ctx, `INSERT INTO snapshots (id, hash, project_id, title, content, normalization_version, created_at)
		VALUES (?, ?, 'proj_rep', 'Title', 'Content', 1, ?);`, snap.ID, snap.Hash, nowStr)
	for i, u := range snap.Units {
		euPK := fmt.Sprintf("%s:%s", snap.ID, u.ID)
		_, _ = writer.ExecContext(ctx, `INSERT INTO evidence_units (id, snapshot_id, unit_id, ordinal, kind, text)
			VALUES (?, ?, ?, ?, ?, ?);`, euPK, snap.ID, u.ID, i, string(u.Kind), u.Text)
	}

	sessID := "sess_repeat"
	seedTerminalSession(t, writer, sessID, "proj_rep", snap.ID,
		domain.SessionComplete, domain.ReasonAllRolesComplete, false, 4)
	for _, r := range domain.Roles {
		rrID := fmt.Sprintf("%s:%s", sessID, r)
		_, _ = writer.ExecContext(ctx, `INSERT INTO role_runs (id, session_id, role, status, call_count, started_at, completed_at, created_at)
			VALUES (?, ?, ?, 'complete', 1, ?, ?, ?);`, rrID, sessID, r.String(), nowStr, nowStr, nowStr)
	}

	st1, err := store.ReadStatus(ctx, sessID)
	if err != nil {
		t.Fatalf("ReadStatus 1: %v", err)
	}
	st2, err := store.ReadStatus(ctx, sessID)
	if err != nil {
		t.Fatalf("ReadStatus 2: %v", err)
	}
	if !reflect.DeepEqual(st1, st2) {
		t.Errorf("repeated ReadStatus calls produced different models")
	}

	rep1, err := store.ReadReport(ctx, sessID)
	if err != nil {
		t.Fatalf("ReadReport 1: %v", err)
	}
	rep2, err := store.ReadReport(ctx, sessID)
	if err != nil {
		t.Fatalf("ReadReport 2: %v", err)
	}
	if !reflect.DeepEqual(rep1, rep2) {
		t.Errorf("repeated ReadReport calls produced different models")
	}
}

// 11. Nil, closed store, and cancelled context handling
func TestRead_DefensiveEdgeCases(t *testing.T) {
	var nilStore *Store
	ctx := context.Background()

	if _, err := nilStore.ReadStatus(ctx, "sess"); err == nil {
		t.Errorf("expected error reading status from nil store")
	}
	if _, err := nilStore.ReadReport(ctx, "sess"); err == nil {
		t.Errorf("expected error reading report from nil store")
	}
	if _, err := nilStore.ReadSnapshot(ctx, "snap"); err == nil {
		t.Errorf("expected error reading snapshot from nil store")
	}

	store, _ := openGuardTestStore(t)
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := store.ReadStatus(ctx, "sess"); !errors.Is(err, ErrClosed) {
		t.Errorf("expected ErrClosed for ReadStatus on closed store, got %v", err)
	}
	if _, err := store.ReadReport(ctx, "sess"); !errors.Is(err, ErrClosed) {
		t.Errorf("expected ErrClosed for ReadReport on closed store, got %v", err)
	}
	if _, err := store.ReadSnapshot(ctx, "snap"); !errors.Is(err, ErrClosed) {
		t.Errorf("expected ErrClosed for ReadSnapshot on closed store, got %v", err)
	}

	store2, _ := openGuardTestStore(t)
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := store2.ReadStatus(cancelCtx, "sess"); !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}
