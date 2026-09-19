package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
)

// Helper to set up an in_flight role ready for publication tests.
func setupInFlightRole(t *testing.T, sessionID, projectID string) (*Store, *sql.DB, time.Time, *DispatchReservationResult) {
	t.Helper()
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	origClock := clock
	clock = func() time.Time { return t0 }
	t.Cleanup(func() { clock = origClock })

	snap := createTestFrozenSnapshot(t, "snap_"+sessionID)
	_, err := store.Submit(ctx, SubmitParams{
		SessionID:      sessionID,
		ProjectID:      projectID,
		IdempotencyKey: "key_" + sessionID,
		Title:          "Publish Test " + sessionID,
		Content:        "Spec content for publication",
		Snapshot:       snap,
		CreatedAt:      t0.Add(-10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("submit %s: %v", sessionID, err)
	}

	policy := TimingPolicy{
		DispatchCutoff:      30 * time.Minute,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: 45 * time.Minute,
	}
	claimRes, err := store.ClaimSession(ctx, policy)
	if err != nil || !claimRes.Claimed {
		t.Fatalf("claim %s: %v", sessionID, err)
	}

	// Reserve the first pending role (Requirements) to in_flight.
	reserveTime := t0.Add(1 * time.Minute)
	res, err := store.ReservePendingRoleWithNow(ctx, sessionID, reserveTime)
	if err != nil || !res.Reserved {
		t.Fatalf("reserve pending role for %s: %v", sessionID, err)
	}

	return store, writer, t0, res
}

// 1. Valid Success Publication with Findings and Citations
func TestPublishRoleSuccess_Valid(t *testing.T) {
	store, writer, t0, res := setupInFlightRole(t, "sess_pub_succ", "proj_pub_succ")
	ctx := context.Background()

	completedAt := t0.Add(5 * time.Minute)

	findings := []domain.Finding{
		{
			ID:             "find-sec-1",
			Severity:       domain.SeverityCritical,
			Category:       "security",
			Issue:          "Plaintext credential storage in config file",
			Recommendation: "Use environment variables or encrypted secrets store",
			BasisRefs:      []string{"req-1", "arch-1"},
		},
		{
			ID:             "find-qa-1",
			Severity:       domain.SeverityLow,
			Category:       "testing",
			Issue:          "Missing boundary value tests for timeout parameter",
			Recommendation: "Add negative test cases for timeout = 0 and max value",
			BasisRefs:      []string{"flow-1"},
		},
	}

	pubRes, err := store.PublishRoleSuccess(ctx, PublishSuccessParams{
		SessionID:   res.SessionID,
		RoleRunID:   res.RoleRunID,
		Role:        res.Role,
		Findings:    findings,
		CallCount:   1,
		CompletedAt: completedAt,
	})
	if err != nil {
		t.Fatalf("PublishRoleSuccess failed: %v", err)
	}

	// Verify publish result.
	if pubRes.SessionID != res.SessionID {
		t.Errorf("got sessionID %q, want %q", pubRes.SessionID, res.SessionID)
	}
	if pubRes.RoleRunID != res.RoleRunID {
		t.Errorf("got roleRunID %q, want %q", pubRes.RoleRunID, res.RoleRunID)
	}
	if pubRes.Role != res.Role {
		t.Errorf("got role %q, want %q", pubRes.Role, res.Role)
	}
	if pubRes.Status != domain.RoleComplete {
		t.Errorf("got status %q, want %q", pubRes.Status, domain.RoleComplete)
	}
	if pubRes.CallCount != 1 {
		t.Errorf("got call_count %d, want 1", pubRes.CallCount)
	}
	if pubRes.FindingCount != 2 {
		t.Errorf("got finding_count %d, want 2", pubRes.FindingCount)
	}
	if !pubRes.CompletedAt.Equal(completedAt) {
		t.Errorf("got completed_at %v, want %v", pubRes.CompletedAt, completedAt)
	}

	// Verify role_runs row in database.
	var (
		dbStatus    string
		dbCallCount int
		dbComplAt   string
		dbCause     sql.NullString
		dbErrCat    sql.NullString
		dbErrMsg    sql.NullString
	)
	err = writer.QueryRowContext(ctx, `
		SELECT status, call_count, completed_at, cause, error_category, error_message
		FROM role_runs WHERE id = ?;
	`, res.RoleRunID).Scan(&dbStatus, &dbCallCount, &dbComplAt, &dbCause, &dbErrCat, &dbErrMsg)
	if err != nil {
		t.Fatalf("query role_run: %v", err)
	}
	if dbStatus != "complete" {
		t.Errorf("db status = %q, want 'complete'", dbStatus)
	}
	if dbCallCount != 1 {
		t.Errorf("db call_count = %d, want 1", dbCallCount)
	}
	if dbCause.Valid || dbErrCat.Valid || dbErrMsg.Valid {
		t.Errorf("expected cause/errCat/errMsg to be NULL, got cause=%v errCat=%v errMsg=%v",
			dbCause, dbErrCat, dbErrMsg)
	}
	if !strings.HasSuffix(dbComplAt, "Z") {
		t.Errorf("completed_at %q must end in 'Z'", dbComplAt)
	}

	// Verify findings rows in database.
	rows, err := writer.QueryContext(ctx, `
		SELECT finding_id, severity, category, issue, recommendation, created_at
		FROM findings WHERE role_run_id = ?
		ORDER BY finding_id ASC;
	`, res.RoleRunID)
	if err != nil {
		t.Fatalf("query findings: %v", err)
	}
	defer rows.Close()

	type dbFinding struct {
		id, sev, cat, issue, rec, createdAt string
	}
	var dbFindings []dbFinding
	for rows.Next() {
		var f dbFinding
		if err := rows.Scan(&f.id, &f.sev, &f.cat, &f.issue, &f.rec, &f.createdAt); err != nil {
			t.Fatalf("scan finding: %v", err)
		}
		dbFindings = append(dbFindings, f)
	}
	if len(dbFindings) != 2 {
		t.Fatalf("expected 2 findings in db, got %d", len(dbFindings))
	}
	if dbFindings[0].id != "find-qa-1" || dbFindings[0].sev != "low" {
		t.Errorf("finding 0 mismatch: %+v", dbFindings[0])
	}
	if dbFindings[1].id != "find-sec-1" || dbFindings[1].sev != "critical" {
		t.Errorf("finding 1 mismatch: %+v", dbFindings[1])
	}

	// Verify basis refs in database.
	refRows, err := writer.QueryContext(ctx, `
		SELECT f.finding_id, eu.unit_id, fbr.ordinal
		FROM finding_basis_refs fbr
		JOIN findings f ON fbr.finding_id = f.id
		JOIN evidence_units eu ON fbr.evidence_unit_id = eu.id
		WHERE f.role_run_id = ?
		ORDER BY f.finding_id ASC, fbr.ordinal ASC;
	`, res.RoleRunID)
	if err != nil {
		t.Fatalf("query basis refs: %v", err)
	}
	defer refRows.Close()

	type dbRef struct {
		findingID, unitID string
		ordinal           int
	}
	var dbRefs []dbRef
	for refRows.Next() {
		var r dbRef
		if err := refRows.Scan(&r.findingID, &r.unitID, &r.ordinal); err != nil {
			t.Fatalf("scan ref: %v", err)
		}
		dbRefs = append(dbRefs, r)
	}
	if len(dbRefs) != 3 {
		t.Fatalf("expected 3 basis refs in db, got %d", len(dbRefs))
	}
	// find-qa-1 has 1 ref: flow-1 (ordinal 1)
	if dbRefs[0].findingID != "find-qa-1" || dbRefs[0].unitID != "flow-1" || dbRefs[0].ordinal != 1 {
		t.Errorf("ref 0 mismatch: %+v", dbRefs[0])
	}
	// find-sec-1 has 2 refs: req-1 (ordinal 1), arch-1 (ordinal 2)
	if dbRefs[1].findingID != "find-sec-1" || dbRefs[1].unitID != "req-1" || dbRefs[1].ordinal != 1 {
		t.Errorf("ref 1 mismatch: %+v", dbRefs[1])
	}
	if dbRefs[2].findingID != "find-sec-1" || dbRefs[2].unitID != "arch-1" || dbRefs[2].ordinal != 2 {
		t.Errorf("ref 2 mismatch: %+v", dbRefs[2])
	}

	// Verify ReadStatus shows the complete role and UNCHANGED session.
	st, err := store.ReadStatus(ctx, res.SessionID)
	if err != nil {
		t.Fatalf("ReadStatus failed: %v", err)
	}
	if st.Status != domain.SessionReviewing {
		t.Errorf("session status = %s, want reviewing", st.Status)
	}
	if st.CompletedRoleCount != 0 || st.IncompleteRoleCount != 4 {
		t.Errorf("counts modified prematurely: completed=%d incomplete=%d", st.CompletedRoleCount, st.IncompleteRoleCount)
	}
	if st.TerminalReason != "" {
		t.Errorf("terminal reason modified prematurely: %s", st.TerminalReason)
	}
	// Role 0 (Requirements) should be complete.
	if st.Roles[0].Status != domain.RoleComplete || st.Roles[0].CallCount != 1 {
		t.Errorf("role 0 status = %s, callCount = %d", st.Roles[0].Status, st.Roles[0].CallCount)
	}
	// Role 1 (Architecture) should still be pending.
	if st.Roles[1].Status != domain.RolePending {
		t.Errorf("role 1 status = %s, want pending", st.Roles[1].Status)
	}
}

// 2. Valid Success with Empty Findings Slice
func TestPublishRoleSuccess_EmptyFindings(t *testing.T) {
	store, writer, t0, res := setupInFlightRole(t, "sess_empty_f", "proj_empty_f")
	ctx := context.Background()

	completedAt := t0.Add(3 * time.Minute)

	pubRes, err := store.PublishRoleSuccess(ctx, PublishSuccessParams{
		SessionID:   res.SessionID,
		RoleRunID:   res.RoleRunID,
		Role:        res.Role,
		Findings:    []domain.Finding{}, // Valid empty slice
		CallCount:   2,
		CompletedAt: completedAt,
	})
	if err != nil {
		t.Fatalf("PublishRoleSuccess with empty findings failed: %v", err)
	}

	if pubRes.Status != domain.RoleComplete || pubRes.FindingCount != 0 || pubRes.CallCount != 2 {
		t.Fatalf("unexpected publish result: %+v", pubRes)
	}

	// Verify 0 findings in db.
	var fCount int
	err = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM findings WHERE role_run_id = ?;", res.RoleRunID).Scan(&fCount)
	if err != nil || fCount != 0 {
		t.Fatalf("expected 0 findings in db, got %d (err: %v)", fCount, err)
	}
}

// 3. Valid Failure Publication with All Canonical Categories
func TestPublishRoleFailure_AllCanonicalCategories(t *testing.T) {
	categories := []struct {
		cat       domain.ErrorCategory
		callCount int
		msg       string
	}{
		{domain.ErrTransport, 2, "network connection reset by peer"},
		{domain.ErrTimeout, 2, "provider request deadline exceeded"},
		{domain.ErrProviderRejected, 1, "model rate limit exceeded with 429"},
		{domain.ErrBudgetExhausted, 0, "prompt exceeded token budget before call"},
		{domain.ErrInvalidJSON, 2, "unexpected EOF in json body"},
		{domain.ErrSchemaInvalid, 2, "missing required issue field"},
		{domain.ErrInvalidBasisRef, 2, "cited non-existent evidence unit"},
	}

	for _, tc := range categories {
		t.Run(string(tc.cat), func(t *testing.T) {
			sessionID := "sess_fail_" + string(tc.cat)
			store, writer, t0, res := setupInFlightRole(t, sessionID, "proj_fail")
			ctx := context.Background()

			completedAt := t0.Add(4 * time.Minute)

			pubRes, err := store.PublishRoleFailure(ctx, PublishFailureParams{
				SessionID:     res.SessionID,
				RoleRunID:     res.RoleRunID,
				Role:          res.Role,
				ErrorCategory: tc.cat,
				ErrorMessage:  tc.msg,
				CallCount:     tc.callCount,
				CompletedAt:   completedAt,
			})
			if err != nil {
				t.Fatalf("PublishRoleFailure(%s) failed: %v", tc.cat, err)
			}

			if pubRes.Status != domain.RoleFailed {
				t.Errorf("got status %s, want failed", pubRes.Status)
			}
			if pubRes.ErrorCategory != tc.cat {
				t.Errorf("got error category %s, want %s", pubRes.ErrorCategory, tc.cat)
			}
			if pubRes.ErrorMessage != tc.msg {
				t.Errorf("got error message %q, want %q", pubRes.ErrorMessage, tc.msg)
			}
			if pubRes.CallCount != tc.callCount {
				t.Errorf("got call count %d, want %d", pubRes.CallCount, tc.callCount)
			}
			if pubRes.FindingCount != 0 {
				t.Errorf("got finding count %d, want 0", pubRes.FindingCount)
			}

			// Verify role_runs row in database.
			var (
				dbStatus  string
				dbCallCnt int
				dbComplAt string
				dbErrCat  string
				dbErrMsg  string
				dbCause   sql.NullString
			)
			err = writer.QueryRowContext(ctx, `
				SELECT status, call_count, completed_at, error_category, error_message, cause
				FROM role_runs WHERE id = ?;
			`, res.RoleRunID).Scan(&dbStatus, &dbCallCnt, &dbComplAt, &dbErrCat, &dbErrMsg, &dbCause)
			if err != nil {
				t.Fatalf("query role_run: %v", err)
			}
			if dbStatus != "failed" {
				t.Errorf("db status = %q, want failed", dbStatus)
			}
			if dbCallCnt != tc.callCount {
				t.Errorf("db call count = %d, want %d", dbCallCnt, tc.callCount)
			}
			if dbErrCat != string(tc.cat) {
				t.Errorf("db error_category = %q, want %s", dbErrCat, tc.cat)
			}
			if dbErrMsg != tc.msg {
				t.Errorf("db error_message = %q, want %q", dbErrMsg, tc.msg)
			}
			if dbCause.Valid {
				t.Errorf("db cause should be NULL on failure, got %v", dbCause)
			}

			// Verify zero findings or basis refs were inserted.
			var fCount int
			err = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM findings WHERE role_run_id = ?;", res.RoleRunID).Scan(&fCount)
			if err != nil || fCount != 0 {
				t.Fatalf("expected 0 findings, got %d", fCount)
			}

			// Verify session fields completely untouched.
			st, err := store.ReadStatus(ctx, res.SessionID)
			if err != nil {
				t.Fatalf("ReadStatus failed: %v", err)
			}
			if st.Status != domain.SessionReviewing || st.CompletedRoleCount != 0 || st.IncompleteRoleCount != 4 {
				t.Errorf("session fields mutated: status=%s comp=%d incomp=%d",
					st.Status, st.CompletedRoleCount, st.IncompleteRoleCount)
			}
		})
	}
}

// 4. Compare-and-Set Rejects Pending Roles
func TestPublish_CompareAndSet_RejectsPendingRole(t *testing.T) {
	store, writer, t0, res := setupInFlightRole(t, "sess_pending_cas", "proj_cas")
	ctx := context.Background()

	// res.Role is requirements (in_flight).
	// Architecture is still pending:
	archRoleRunID := res.SessionID + ":architecture"

	// Verify it is indeed pending in DB.
	var status string
	_ = writer.QueryRowContext(ctx, "SELECT status FROM role_runs WHERE id = ?;", archRoleRunID).Scan(&status)
	if status != "pending" {
		t.Fatalf("expected architecture to be pending, got %q", status)
	}

	// Attempt success publish on pending role.
	_, err := store.PublishRoleSuccess(ctx, PublishSuccessParams{
		SessionID:   res.SessionID,
		RoleRunID:   archRoleRunID,
		Role:        domain.RoleArchitecture,
		CallCount:   1,
		CompletedAt: t0.Add(2 * time.Minute),
	})
	if err == nil {
		t.Fatal("expected error publishing success on pending role, got nil")
	}
	var conflictErr *PublicationConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("expected *PublicationConflictError, got %T: %v", err, err)
	}
	if !errors.Is(err, ErrPublicationConflict) {
		t.Errorf("expected errors.Is(err, ErrPublicationConflict) to be true")
	}
	if conflictErr.ActualStatus != domain.RolePending {
		t.Errorf("conflict error actual status = %q, want 'pending'", conflictErr.ActualStatus)
	}

	// Attempt failure publish on pending role.
	_, err = store.PublishRoleFailure(ctx, PublishFailureParams{
		SessionID:     res.SessionID,
		RoleRunID:     archRoleRunID,
		Role:          domain.RoleArchitecture,
		ErrorCategory: domain.ErrTransport,
		CallCount:     1,
		CompletedAt:   t0.Add(2 * time.Minute),
	})
	if err == nil {
		t.Fatal("expected error publishing failure on pending role, got nil")
	}
	if !errors.As(err, &conflictErr) {
		t.Fatalf("expected *PublicationConflictError, got %T: %v", err, err)
	}

	// Verify architecture role remains pending in DB.
	_ = writer.QueryRowContext(ctx, "SELECT status FROM role_runs WHERE id = ?;", archRoleRunID).Scan(&status)
	if status != "pending" {
		t.Errorf("role status changed: %q, want 'pending'", status)
	}
}

// 5. Duplicate Publication Protection
func TestPublish_DuplicatePublication(t *testing.T) {
	store, writer, t0, res := setupInFlightRole(t, "sess_dup_pub", "proj_dup")
	ctx := context.Background()

	// 1st publish: success.
	pub1, err := store.PublishRoleSuccess(ctx, PublishSuccessParams{
		SessionID:   res.SessionID,
		RoleRunID:   res.RoleRunID,
		Role:        res.Role,
		Findings:    []domain.Finding{},
		CallCount:   1,
		CompletedAt: t0.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("first publish failed: %v", err)
	}
	if pub1.Status != domain.RoleComplete {
		t.Fatalf("expected status complete, got %s", pub1.Status)
	}

	// 2nd publish: duplicate success attempt on the same role run.
	_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
		SessionID:   res.SessionID,
		RoleRunID:   res.RoleRunID,
		Role:        res.Role,
		Findings:    []domain.Finding{},
		CallCount:   1,
		CompletedAt: t0.Add(3 * time.Minute),
	})
	if err == nil {
		t.Fatal("expected error on duplicate success publication, got nil")
	}
	var conflictErr *PublicationConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("expected *PublicationConflictError on duplicate, got %T: %v", err, err)
	}
	if conflictErr.ActualStatus != domain.RoleComplete {
		t.Errorf("conflict error actual status = %q, want 'complete'", conflictErr.ActualStatus)
	}

	// 3rd publish: duplicate failure attempt on the already complete role run.
	_, err = store.PublishRoleFailure(ctx, PublishFailureParams{
		SessionID:     res.SessionID,
		RoleRunID:     res.RoleRunID,
		Role:          res.Role,
		ErrorCategory: domain.ErrTransport,
		CallCount:     1,
		CompletedAt:   t0.Add(4 * time.Minute),
	})
	if err == nil {
		t.Fatal("expected error on duplicate failure publication, got nil")
	}
	if !errors.As(err, &conflictErr) {
		t.Fatalf("expected *PublicationConflictError on duplicate failure, got %T: %v", err, err)
	}

	// Verify role is still in its original complete state.
	var status string
	_ = writer.QueryRowContext(ctx, "SELECT status FROM role_runs WHERE id = ?;", res.RoleRunID).Scan(&status)
	if status != "complete" {
		t.Errorf("role status changed: got %q, want 'complete'", status)
	}
}

// 6. Late Provider Result Protection on Terminal Role Runs
func TestPublish_LateProviderResult_AlreadyTerminal(t *testing.T) {
	// Case A: Role was already failed.
	t.Run("AlreadyFailed", func(t *testing.T) {
		store, _, t0, res := setupInFlightRole(t, "sess_late_fail", "proj_late")
		ctx := context.Background()

		// Publish failure first.
		_, err := store.PublishRoleFailure(ctx, PublishFailureParams{
			SessionID:     res.SessionID,
			RoleRunID:     res.RoleRunID,
			Role:          res.Role,
			ErrorCategory: domain.ErrTimeout,
			CallCount:     1,
			CompletedAt:   t0.Add(2 * time.Minute),
		})
		if err != nil {
			t.Fatalf("initial failure publish failed: %v", err)
		}

		// Late result arrives with success -> must be rejected with conflict!
		_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
			SessionID:   res.SessionID,
			RoleRunID:   res.RoleRunID,
			Role:        res.Role,
			CallCount:   2,
			CompletedAt: t0.Add(3 * time.Minute),
		})
		if err == nil {
			t.Fatal("expected conflict for late result on failed role, got nil")
		}
		var conflictErr *PublicationConflictError
		if !errors.As(err, &conflictErr) {
			t.Fatalf("expected *PublicationConflictError, got %T: %v", err, err)
		}
		if conflictErr.ActualStatus != domain.RoleFailed {
			t.Errorf("got actual status %s, want failed", conflictErr.ActualStatus)
		}
	})

	// Case B: Role was interrupted.
	t.Run("AlreadyInterrupted", func(t *testing.T) {
		store, writer, t0, res := setupInFlightRole(t, "sess_late_int", "proj_late")
		ctx := context.Background()

		// Manually transition role to interrupted (simulating cutoff/cancellation sweep).
		interruptedAt := t0.Add(3 * time.Minute).Format(time.RFC3339Nano)
		_, err := writer.ExecContext(ctx, `
			UPDATE role_runs
			SET status = 'interrupted',
				cause = 'user_cancelled',
				completed_at = ?
			WHERE id = ?;
		`, interruptedAt, res.RoleRunID)
		if err != nil {
			t.Fatalf("update to interrupted: %v", err)
		}

		// Late provider result arrives -> must be rejected with conflict!
		_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
			SessionID:   res.SessionID,
			RoleRunID:   res.RoleRunID,
			Role:        res.Role,
			CallCount:   1,
			CompletedAt: t0.Add(4 * time.Minute),
		})
		if err == nil {
			t.Fatal("expected conflict for late result on interrupted role, got nil")
		}
		var conflictErr *PublicationConflictError
		if !errors.As(err, &conflictErr) {
			t.Fatalf("expected *PublicationConflictError, got %T: %v", err, err)
		}
		if conflictErr.ActualStatus != domain.RoleInterrupted {
			t.Errorf("got actual status %s, want interrupted", conflictErr.ActualStatus)
		}
	})
}

// 7. Rollback on Invalid Findings
func TestPublish_Rollback_InvalidFindings(t *testing.T) {
	testCases := []struct {
		name     string
		findings []domain.Finding
		field    string
	}{
		{
			name: "TooManyFindings",
			findings: func() []domain.Finding {
				fList := make([]domain.Finding, 16)
				for i := range fList {
					fList[i] = domain.Finding{
						ID:             fmt.Sprintf("f-%d", i),
						Severity:       domain.SeverityMedium,
						Category:       "cat",
						Issue:          "issue",
						Recommendation: "rec",
						BasisRefs:      []string{"req-1"},
					}
				}
				return fList
			}(),
			field: "findings",
		},
		{
			name: "EmptyFindingID",
			findings: []domain.Finding{
				{
					ID:             "",
					Severity:       domain.SeverityMedium,
					Category:       "cat",
					Issue:          "issue",
					Recommendation: "rec",
					BasisRefs:      []string{"req-1"},
				},
			},
			field: "findings[0].id",
		},
		{
			name: "DuplicateFindingID",
			findings: []domain.Finding{
				{
					ID:             "dup-id",
					Severity:       domain.SeverityMedium,
					Category:       "cat",
					Issue:          "issue 1",
					Recommendation: "rec 1",
					BasisRefs:      []string{"req-1"},
				},
				{
					ID:             "dup-id",
					Severity:       domain.SeverityHigh,
					Category:       "cat",
					Issue:          "issue 2",
					Recommendation: "rec 2",
					BasisRefs:      []string{"req-1"},
				},
			},
			field: "findings[1].id",
		},
		{
			name: "InvalidSeverity",
			findings: []domain.Finding{
				{
					ID:             "f-bad-sev",
					Severity:       domain.Severity("catastrophic"),
					Category:       "cat",
					Issue:          "issue",
					Recommendation: "rec",
					BasisRefs:      []string{"req-1"},
				},
			},
			field: "findings[0].severity",
		},
		{
			name: "EmptyCategory",
			findings: []domain.Finding{
				{
					ID:             "f-bad-cat",
					Severity:       domain.SeverityLow,
					Category:       "   ",
					Issue:          "issue",
					Recommendation: "rec",
					BasisRefs:      []string{"req-1"},
				},
			},
			field: "findings[0].category",
		},
		{
			name: "EmptyIssue",
			findings: []domain.Finding{
				{
					ID:             "f-bad-iss",
					Severity:       domain.SeverityLow,
					Category:       "cat",
					Issue:          "",
					Recommendation: "rec",
					BasisRefs:      []string{"req-1"},
				},
			},
			field: "findings[0].issue",
		},
		{
			name: "IssueExceeds1000Chars",
			findings: []domain.Finding{
				{
					ID:             "f-long-iss",
					Severity:       domain.SeverityLow,
					Category:       "cat",
					Issue:          strings.Repeat("a", 1001),
					Recommendation: "rec",
					BasisRefs:      []string{"req-1"},
				},
			},
			field: "findings[0].issue",
		},
		{
			name: "EmptyRecommendation",
			findings: []domain.Finding{
				{
					ID:             "f-bad-rec",
					Severity:       domain.SeverityLow,
					Category:       "cat",
					Issue:          "issue",
					Recommendation: "",
					BasisRefs:      []string{"req-1"},
				},
			},
			field: "findings[0].recommendation",
		},
		{
			name: "RecommendationExceeds1000Chars",
			findings: []domain.Finding{
				{
					ID:             "f-long-rec",
					Severity:       domain.SeverityLow,
					Category:       "cat",
					Issue:          "issue",
					Recommendation: strings.Repeat("b", 1001),
					BasisRefs:      []string{"req-1"},
				},
			},
			field: "findings[0].recommendation",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			store, writer, t0, res := setupInFlightRole(t, "sess_rb_f_"+tc.name, "proj_rb")
			ctx := context.Background()

			_, err := store.PublishRoleSuccess(ctx, PublishSuccessParams{
				SessionID:   res.SessionID,
				RoleRunID:   res.RoleRunID,
				Role:        res.Role,
				Findings:    tc.findings,
				CallCount:   1,
				CompletedAt: t0.Add(2 * time.Minute),
			})
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
			var valErr *PublicationValidationError
			if !errors.As(err, &valErr) {
				t.Fatalf("expected *PublicationValidationError, got %T: %v", err, err)
			}
			if !errors.Is(err, ErrInvalidPublication) {
				t.Errorf("expected errors.Is(err, ErrInvalidPublication)")
			}

			// Verify rollback: role is STILL in_flight!
			var status string
			_ = writer.QueryRowContext(ctx, "SELECT status FROM role_runs WHERE id = ?;", res.RoleRunID).Scan(&status)
			if status != "in_flight" {
				t.Errorf("role status changed despite error: %q, want 'in_flight'", status)
			}

			// Verify 0 findings in db.
			var fCount int
			_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM findings WHERE role_run_id = ?;", res.RoleRunID).Scan(&fCount)
			if fCount != 0 {
				t.Errorf("findings inserted despite error: %d", fCount)
			}
		})
	}
}

// 8. Rollback on Invalid Basis References
func TestPublish_Rollback_InvalidBasisRefs(t *testing.T) {
	testCases := []struct {
		name      string
		basisRefs []string
	}{
		{
			name:      "ZeroBasisRefs",
			basisRefs: []string{}, // Less than MinBasisRefsPerFinding (1)
		},
		{
			name:      "SixBasisRefs",
			basisRefs: []string{"req-1", "arch-1", "flow-1", "const-1", "data-1", "brief-1"}, // More than MaxBasisRefsPerFinding (5)
		},
		{
			name:      "DuplicateBasisRefs",
			basisRefs: []string{"req-1", "req-1"},
		},
		{
			name:      "UnknownBasisRefNotInSnapshot",
			basisRefs: []string{"non-existent-unit-999"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			store, writer, t0, res := setupInFlightRole(t, "sess_rb_b_"+tc.name, "proj_rb_b")
			ctx := context.Background()

			findings := []domain.Finding{
				{
					ID:             "find-basis-test",
					Severity:       domain.SeverityHigh,
					Category:       "architecture",
					Issue:          "Test issue",
					Recommendation: "Test recommendation",
					BasisRefs:      tc.basisRefs,
				},
			}

			_, err := store.PublishRoleSuccess(ctx, PublishSuccessParams{
				SessionID:   res.SessionID,
				RoleRunID:   res.RoleRunID,
				Role:        res.Role,
				Findings:    findings,
				CallCount:   1,
				CompletedAt: t0.Add(2 * time.Minute),
			})
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
			var valErr *PublicationValidationError
			if !errors.As(err, &valErr) {
				t.Fatalf("expected *PublicationValidationError, got %T: %v", err, err)
			}

			// Verify role remains in_flight.
			var status string
			_ = writer.QueryRowContext(ctx, "SELECT status FROM role_runs WHERE id = ?;", res.RoleRunID).Scan(&status)
			if status != "in_flight" {
				t.Errorf("role status changed: got %q, want 'in_flight'", status)
			}

			// Verify 0 findings or refs inserted.
			var fCount int
			_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM findings WHERE role_run_id = ?;", res.RoleRunID).Scan(&fCount)
			if fCount != 0 {
				t.Errorf("findings inserted despite basis error: %d", fCount)
			}
		})
	}
}

// 9. Rollback on Cross-Snapshot Evidence Citation
func TestPublish_Rollback_CrossSnapshotCitation(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

	// Session 1 with Snapshot 1
	snap1 := createTestFrozenSnapshot(t, "snap_cross_1")
	_, _ = store.Submit(ctx, SubmitParams{
		SessionID:      "sess_cross_1",
		ProjectID:      "proj_cross",
		IdempotencyKey: "key_cross_1",
		Title:          "Cross 1",
		Content:        "Content 1",
		Snapshot:       snap1,
		CreatedAt:      t0.Add(-5 * time.Minute),
	})
	policy := TimingPolicy{DispatchCutoff: 30 * time.Minute, CallTimeout: 10 * time.Minute, SessionHardDeadline: 45 * time.Minute}
	_, _ = store.ClaimSession(ctx, policy)
	res1, _ := store.ReservePendingRole(ctx, "sess_cross_1")

	// Session 2 with Snapshot 2
	snap2Units := []evidence.Unit{
		{ID: "cross-unit-2", Kind: evidence.UnitRequirement, Text: "Second snapshot unit"},
	}
	snap2, err := evidence.Freeze("snap_cross_2", snap2Units)
	if err != nil {
		t.Fatalf("freeze snap2: %v", err)
	}

	// Insert snap2 directly into database so it exists in the database but under a DIFFERENT snapshot!
	_, err = writer.ExecContext(ctx, `
		INSERT INTO snapshots (id, hash, project_id, title, content, normalization_version, created_at)
		VALUES ('snap_cross_2', ?, 'proj_cross', 'Title 2', 'Content 2', 1, '2026-09-19T12:00:00Z');
	`, snap2.Hash)
	if err != nil {
		t.Fatalf("insert snap2: %v", err)
	}
	_, err = writer.ExecContext(ctx, `
		INSERT INTO evidence_units (id, snapshot_id, unit_id, ordinal, kind, text)
		VALUES ('snap_cross_2:cross-unit-2', 'snap_cross_2', 'cross-unit-2', 0, 'requirement', 'Second snapshot unit');
	`)
	if err != nil {
		t.Fatalf("insert unit2: %v", err)
	}

	// Now try to publish finding in sess_cross_1 citing unit from snap2 ("cross-unit-2")!
	findings := []domain.Finding{
		{
			ID:             "find-cross",
			Severity:       domain.SeverityHigh,
			Category:       "security",
			Issue:          "Cross snapshot citation attempt",
			Recommendation: "None",
			BasisRefs:      []string{"cross-unit-2"},
		},
	}

	_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
		SessionID:   res1.SessionID,
		RoleRunID:   res1.RoleRunID,
		Role:        res1.Role,
		Findings:    findings,
		CallCount:   1,
		CompletedAt: t0.Add(2 * time.Minute),
	})
	if err == nil {
		t.Fatal("expected cross-snapshot citation to be rejected, got nil")
	}

	// Verify role remains in_flight.
	var status string
	_ = writer.QueryRowContext(ctx, "SELECT status FROM role_runs WHERE id = ?;", res1.RoleRunID).Scan(&status)
	if status != "in_flight" {
		t.Errorf("role status changed: %q, want 'in_flight'", status)
	}
}

// 10. Rollback on Injected Failure (Commit and Update Hooks)
func TestPublish_Rollback_InjectedHooks(t *testing.T) {
	// Hook before commit failure.
	t.Run("BeforeCommitHook", func(t *testing.T) {
		store, writer, t0, res := setupInFlightRole(t, "sess_inj_commit", "proj_inj")
		ctx := context.Background()

		injectedErr := errors.New("simulated commit crash")
		publishBeforeCommitHook = func(ctx context.Context, conn *sql.Conn) error {
			return injectedErr
		}
		t.Cleanup(func() { publishBeforeCommitHook = nil })

		_, err := store.PublishRoleSuccess(ctx, PublishSuccessParams{
			SessionID:   res.SessionID,
			RoleRunID:   res.RoleRunID,
			Role:        res.Role,
			Findings:    []domain.Finding{},
			CallCount:   1,
			CompletedAt: t0.Add(2 * time.Minute),
		})
		if !errors.Is(err, injectedErr) {
			t.Fatalf("expected injected error %v, got %v", injectedErr, err)
		}

		// Verify transaction rolled back: role is still in_flight!
		var status string
		_ = writer.QueryRowContext(ctx, "SELECT status FROM role_runs WHERE id = ?;", res.RoleRunID).Scan(&status)
		if status != "in_flight" {
			t.Errorf("role status changed after commit failure: %q, want in_flight", status)
		}
	})

	// Hook before update failure on failure publish.
	t.Run("BeforeUpdateHook_FailurePublish", func(t *testing.T) {
		store, writer, t0, res := setupInFlightRole(t, "sess_inj_upd", "proj_inj")
		ctx := context.Background()

		injectedErr := errors.New("simulated pre-update error")
		publishBeforeUpdateHook = func(ctx context.Context, conn *sql.Conn, sessionID, roleRunID string) error {
			return injectedErr
		}
		t.Cleanup(func() { publishBeforeUpdateHook = nil })

		_, err := store.PublishRoleFailure(ctx, PublishFailureParams{
			SessionID:     res.SessionID,
			RoleRunID:     res.RoleRunID,
			Role:          res.Role,
			ErrorCategory: domain.ErrTransport,
			CallCount:     1,
			CompletedAt:   t0.Add(2 * time.Minute),
		})
		if !errors.Is(err, injectedErr) {
			t.Fatalf("expected injected error %v, got %v", injectedErr, err)
		}

		var status string
		_ = writer.QueryRowContext(ctx, "SELECT status FROM role_runs WHERE id = ?;", res.RoleRunID).Scan(&status)
		if status != "in_flight" {
			t.Errorf("role status changed after update failure: %q, want in_flight", status)
		}
	})
}

// 11. Concurrent Publishers with Exactly One Winner
func TestPublish_ConcurrentPublishers_OneWinner(t *testing.T) {
	store, writer, t0, res := setupInFlightRole(t, "sess_concurrent", "proj_conc")
	ctx := context.Background()

	const concurrentCount = 20
	var (
		wg         sync.WaitGroup
		startGate  = make(chan struct{})
		succWinner int64
		failWinner int64
		conflicts  int64
		otherErrs  int64
	)

	wg.Add(concurrentCount)
	for i := 0; i < concurrentCount; i++ {
		workerID := i
		go func() {
			defer wg.Done()
			<-startGate

			if workerID%2 == 0 {
				// Success publisher
				_, err := store.PublishRoleSuccess(ctx, PublishSuccessParams{
					SessionID:   res.SessionID,
					RoleRunID:   res.RoleRunID,
					Role:        res.Role,
					Findings:    []domain.Finding{},
					CallCount:   1,
					CompletedAt: t0.Add(2 * time.Minute),
				})
				if err == nil {
					atomic.AddInt64(&succWinner, 1)
				} else if errors.Is(err, ErrPublicationConflict) {
					atomic.AddInt64(&conflicts, 1)
				} else {
					atomic.AddInt64(&otherErrs, 1)
				}
			} else {
				// Failure publisher
				_, err := store.PublishRoleFailure(ctx, PublishFailureParams{
					SessionID:     res.SessionID,
					RoleRunID:     res.RoleRunID,
					Role:          res.Role,
					ErrorCategory: domain.ErrTimeout,
					CallCount:     1,
					CompletedAt:   t0.Add(2 * time.Minute),
				})
				if err == nil {
					atomic.AddInt64(&failWinner, 1)
				} else if errors.Is(err, ErrPublicationConflict) {
					atomic.AddInt64(&conflicts, 1)
				} else {
					atomic.AddInt64(&otherErrs, 1)
				}
			}
		}()
	}

	close(startGate)
	wg.Wait()

	totalWinners := succWinner + failWinner
	if totalWinners != 1 {
		t.Fatalf("expected exactly 1 winner, got %d (succ=%d, fail=%d)", totalWinners, succWinner, failWinner)
	}
	if conflicts != concurrentCount-1 {
		t.Fatalf("expected %d conflicts, got %d (otherErrs=%d)", concurrentCount-1, conflicts, otherErrs)
	}

	// Verify database state is consistently terminal.
	var finalStatus string
	_ = writer.QueryRowContext(ctx, "SELECT status FROM role_runs WHERE id = ?;", res.RoleRunID).Scan(&finalStatus)
	if succWinner == 1 && finalStatus != "complete" {
		t.Errorf("success won but db status is %q", finalStatus)
	}
	if failWinner == 1 && finalStatus != "failed" {
		t.Errorf("failure won but db status is %q", finalStatus)
	}
}

// 12. Exact Call Count and Timestamp Invariants
func TestPublish_CallCountsAndTimestamps(t *testing.T) {
	// CallCount validation on success
	t.Run("SuccessCallCountBounds", func(t *testing.T) {
		store, _, t0, res := setupInFlightRole(t, "sess_cc_succ", "proj_cc")
		ctx := context.Background()

		// CallCount = 0 is invalid for success (must be 1 or 2).
		_, err := store.PublishRoleSuccess(ctx, PublishSuccessParams{
			SessionID:   res.SessionID,
			RoleRunID:   res.RoleRunID,
			Role:        res.Role,
			CallCount:   0,
			CompletedAt: t0.Add(2 * time.Minute),
		})
		if err == nil {
			t.Error("expected error for success with call_count = 0, got nil")
		}

		// CallCount = 3 exceeds two-call budget ("no third-call behavior").
		_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
			SessionID:   res.SessionID,
			RoleRunID:   res.RoleRunID,
			Role:        res.Role,
			CallCount:   3,
			CompletedAt: t0.Add(2 * time.Minute),
		})
		if err == nil {
			t.Error("expected error for success with call_count = 3, got nil")
		}
	})

	// CallCount validation on failure
	t.Run("FailureCallCountBounds", func(t *testing.T) {
		store, _, t0, res := setupInFlightRole(t, "sess_cc_fail", "proj_cc")
		ctx := context.Background()

		// CallCount = -1 is invalid.
		_, err := store.PublishRoleFailure(ctx, PublishFailureParams{
			SessionID:     res.SessionID,
			RoleRunID:     res.RoleRunID,
			Role:          res.Role,
			ErrorCategory: domain.ErrTransport,
			CallCount:     -1,
			CompletedAt:   t0.Add(2 * time.Minute),
		})
		if err == nil {
			t.Error("expected error for failure with call_count = -1, got nil")
		}

		// CallCount = 3 exceeds budget ("no third-call behavior").
		_, err = store.PublishRoleFailure(ctx, PublishFailureParams{
			SessionID:     res.SessionID,
			RoleRunID:     res.RoleRunID,
			Role:          res.Role,
			ErrorCategory: domain.ErrTransport,
			CallCount:     3,
			CompletedAt:   t0.Add(2 * time.Minute),
		})
		if err == nil {
			t.Error("expected error for failure with call_count = 3, got nil")
		}
	})

	// CompletedAt cannot be before StartedAt
	t.Run("CompletedAtBeforeStartedAt", func(t *testing.T) {
		store, _, t0, res := setupInFlightRole(t, "sess_time_order", "proj_time")
		ctx := context.Background()

		// StartedAt is t0 + 1m. Try completedAt = t0 (before StartedAt).
		_, err := store.PublishRoleSuccess(ctx, PublishSuccessParams{
			SessionID:   res.SessionID,
			RoleRunID:   res.RoleRunID,
			Role:        res.Role,
			CallCount:   1,
			CompletedAt: t0.Add(-1 * time.Minute), // Before started_at!
		})
		if err == nil {
			t.Error("expected error for completed_at before started_at, got nil")
		}
	})
}

// 13. Preservation of Session and Snapshot Fields
func TestPublish_UnchangedSessionAndSnapshot(t *testing.T) {
	store, writer, t0, res := setupInFlightRole(t, "sess_unchanged", "proj_unchanged")
	ctx := context.Background()

	// Query entire session row before publication.
	type sessionSnapshot struct {
		id, projID, idempKey, reqHash, snapID string
		status                                string
		cancelReq                             int
		cutoff, deadline, termReason          sql.NullString
		compCnt, incompCnt                    int
		createdAt, claimedAt, termAt          sql.NullString
	}

	readSess := func() sessionSnapshot {
		var s sessionSnapshot
		err := writer.QueryRowContext(ctx, `
			SELECT id, project_id, idempotency_key, request_hash, snapshot_id,
				status, cancel_requested, dispatch_cutoff_at, hard_deadline_at,
				terminal_reason, completed_role_count, incomplete_role_count,
				created_at, claimed_at, terminal_at
			FROM sessions WHERE id = 'sess_unchanged';
		`).Scan(
			&s.id, &s.projID, &s.idempKey, &s.reqHash, &s.snapID,
			&s.status, &s.cancelReq, &s.cutoff, &s.deadline,
			&s.termReason, &s.compCnt, &s.incompCnt,
			&s.createdAt, &s.claimedAt, &s.termAt,
		)
		if err != nil {
			t.Fatalf("query session: %v", err)
		}
		return s
	}

	before := readSess()

	// Publish success.
	findings := []domain.Finding{
		{
			ID:             "f-pres",
			Severity:       domain.SeverityMedium,
			Category:       "perf",
			Issue:          "Memory allocation spike",
			Recommendation: "Reuse slice buffer",
			BasisRefs:      []string{"req-1"},
		},
	}
	_, err := store.PublishRoleSuccess(ctx, PublishSuccessParams{
		SessionID:   res.SessionID,
		RoleRunID:   res.RoleRunID,
		Role:        res.Role,
		Findings:    findings,
		CallCount:   1,
		CompletedAt: t0.Add(5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	after := readSess()

	// Ensure every single session field is 100% identical.
	if before != after {
		t.Fatalf("session row modified by publication!\nBefore: %+v\nAfter:  %+v", before, after)
	}

	// Verify snapshot hash and units are intact and identical.
	snap, err := store.ReadSnapshot(ctx, before.snapID)
	if err != nil {
		t.Fatalf("ReadSnapshot failed: %v", err)
	}
	if len(snap.Units) == 0 {
		t.Fatal("snapshot units corrupted or empty")
	}
}

// 14. Scoped Lookup, Validation, and Package-Level Functions
func TestPublish_ScopedAndPackageLevel(t *testing.T) {
	store, _, t0, res := setupInFlightRole(t, "sess_scoped", "proj_scoped")
	ctx := context.Background()

	// 1. Wrong project ID returns SessionNotFoundError.
	_, err := store.PublishRoleSuccess(ctx, PublishSuccessParams{
		ProjectID:   "wrong_project",
		SessionID:   res.SessionID,
		RoleRunID:   res.RoleRunID,
		CallCount:   1,
		CompletedAt: t0.Add(2 * time.Minute),
	})
	if err == nil {
		t.Fatal("expected error with wrong project_id, got nil")
	}
	var sessNotFound *SessionNotFoundError
	if !errors.As(err, &sessNotFound) {
		t.Fatalf("expected *SessionNotFoundError, got %T: %v", err, err)
	}

	// 2. Mismatched Role parameter returns validation error.
	_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
		SessionID:   res.SessionID,
		RoleRunID:   res.RoleRunID,
		Role:        domain.RoleSecurity, // res.Role is Requirements!
		CallCount:   1,
		CompletedAt: t0.Add(2 * time.Minute),
	})
	if err == nil {
		t.Fatal("expected error with mismatched role, got nil")
	}
	var valErr *PublicationValidationError
	if !errors.As(err, &valErr) {
		t.Fatalf("expected *PublicationValidationError, got %T: %v", err, err)
	}

	// 3. Package-level function delegates correctly.
	pubRes, err := PublishRoleSuccess(ctx, store, PublishSuccessParams{
		ProjectID:   "proj_scoped",
		SessionID:   res.SessionID,
		RoleRunID:   res.RoleRunID,
		Role:        res.Role,
		Findings:    []domain.Finding{},
		CallCount:   1,
		CompletedAt: t0.Add(3 * time.Minute),
	})
	if err != nil {
		t.Fatalf("PublishRoleSuccess package-level failed: %v", err)
	}
	if pubRes.Status != domain.RoleComplete {
		t.Errorf("got status %s, want complete", pubRes.Status)
	}

	// 4. Nil store returns error.
	_, err = PublishRoleSuccess(ctx, nil, PublishSuccessParams{})
	if err == nil {
		t.Error("expected error for nil store, got nil")
	}
	_, err = PublishRoleFailure(ctx, nil, PublishFailureParams{})
	if err == nil {
		t.Error("expected error for nil store, got nil")
	}
}
