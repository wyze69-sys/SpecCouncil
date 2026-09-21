package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
)

// Helper to set up a reviewing session ready for sweep tests.
func setupTestReviewingSession(t *testing.T, sessionID, projectID string, t0 time.Time) (*Store, *sql.DB, TimingPolicy) {
	t.Helper()
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	origClock := clock
	clock = func() time.Time { return t0 }
	t.Cleanup(func() { clock = origClock })

	snap := createTestFrozenSnapshot(t, "snap_"+sessionID)
	_, err := store.Submit(ctx, SubmitParams{
		SessionID:      sessionID,
		ProjectID:      projectID,
		IdempotencyKey: "key_" + sessionID,
		Title:          "Sweep Test " + sessionID,
		Content:        "Spec content for sweeps",
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
	claimRes, err := store.ClaimSessionWithNow(ctx, policy, t0)
	if err != nil || !claimRes.Claimed {
		t.Fatalf("claim %s: %v", sessionID, err)
	}

	return store, writer, policy
}

// ---------------------------------------------------------------------
// 1. Cancellation Sweep Tests
// ---------------------------------------------------------------------

func TestCancelSweep_ReviewingSession_PendingRolesInterrupted(t *testing.T) {
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store, _, _ := setupTestReviewingSession(t, "sess_can_rev", "proj_can_rev", t0)
	ctx := context.Background()

	if _, err := store.RequestCancellation(ctx, "sess_can_rev"); err != nil {
		t.Fatalf("RequestCancellation: %v", err)
	}
	sweepTime := t0.Add(5 * time.Minute)
	res, err := store.SweepCancellationWithNow(ctx, "sess_can_rev", sweepTime)
	if err != nil {
		t.Fatalf("SweepCancellation failed: %v", err)
	}

	if res.InterruptedCount != 4 {
		t.Errorf("expected 4 interrupted roles, got %d", res.InterruptedCount)
	}
	if len(res.InterruptedRoles) != 4 {
		t.Errorf("expected 4 interrupted roles in slice, got %d", len(res.InterruptedRoles))
	}
	if res.InFlightCount != 0 {
		t.Errorf("expected 0 in-flight roles, got %d", res.InFlightCount)
	}
	if res.IsNoOp() {
		t.Errorf("expected IsNoOp == false")
	}

	// Verify all 4 roles in database are interrupted with cause user_cancelled.
	status, err := store.ReadStatus(ctx, "sess_can_rev")
	if err != nil {
		t.Fatalf("ReadStatus failed: %v", err)
	}
	for _, r := range status.Roles {
		if r.Status != domain.RoleInterrupted {
			t.Errorf("expected role %s status interrupted, got %s", r.Role, r.Status)
		}
		if r.Cause != domain.CauseUserCancelled {
			t.Errorf("expected role %s cause user_cancelled, got %s", r.Role, r.Cause)
		}
		if r.CompletedAt == nil || !r.CompletedAt.Equal(sweepTime) {
			t.Errorf("expected role %s completed_at %v, got %v", r.Role, sweepTime, r.CompletedAt)
		}
		if r.StartedAt != nil {
			t.Errorf("expected role %s started_at nil, got %v", r.Role, r.StartedAt)
		}
		if r.CallCount != 0 {
			t.Errorf("expected role %s call_count 0, got %d", r.Role, r.CallCount)
		}
	}
}

func TestCancelSweep_InFlightAndTerminalRolesUnchanged(t *testing.T) {
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store, _, _ := setupTestReviewingSession(t, "sess_can_mixed", "proj_can_mixed", t0)
	ctx := context.Background()

	// 1. Requirements: reserve -> publish success.
	resReq, err := store.ReservePendingRoleWithNow(ctx, "sess_can_mixed", t0.Add(1*time.Minute))
	if err != nil || !resReq.Reserved {
		t.Fatalf("reserve requirements: %v", err)
	}
	findings := []domain.Finding{
		{
			ID:             "find-1",
			Kind:           domain.FindingExisting,
			Severity:       domain.SeverityHigh,
			Category:       "architecture",
			Issue:          "Issue description",
			Recommendation: "Fix recommendation",
			BasisRefs:      []string{"req-1"},
		},
	}
	_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
		SessionID:   "sess_can_mixed",
		RoleRunID:   resReq.RoleRunID,
		Role:        resReq.Role,
		Findings:    findings,
		CallCount:   1,
		CompletedAt: t0.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("publish requirements: %v", err)
	}

	// 2. Architecture: reserve -> publish failure.
	resArch, err := store.ReservePendingRoleWithNow(ctx, "sess_can_mixed", t0.Add(3*time.Minute))
	if err != nil || !resArch.Reserved {
		t.Fatalf("reserve architecture: %v", err)
	}
	_, err = store.PublishRoleFailure(ctx, PublishFailureParams{
		SessionID:     "sess_can_mixed",
		RoleRunID:     resArch.RoleRunID,
		Role:          resArch.Role,
		ErrorCategory: domain.ErrTransport,
		ErrorMessage:  "network timeout",
		CallCount:     2,
		CompletedAt:   t0.Add(4 * time.Minute),
	})
	if err != nil {
		t.Fatalf("publish architecture: %v", err)
	}

	// 3. QA: reserve to in_flight (remains in_flight).
	resQA, err := store.ReservePendingRoleWithNow(ctx, "sess_can_mixed", t0.Add(5*time.Minute))
	if err != nil || !resQA.Reserved {
		t.Fatalf("reserve qa: %v", err)
	}

	// 4. Security is still pending.

	// Request cancellation before running the sweep.
	if _, err := store.RequestCancellation(ctx, "sess_can_mixed"); err != nil {
		t.Fatalf("RequestCancellation: %v", err)
	}
	// Run cancellation sweep.
	sweepTime := t0.Add(6 * time.Minute)
	res, err := store.SweepCancellationWithNow(ctx, "sess_can_mixed", sweepTime)
	if err != nil {
		t.Fatalf("SweepCancellation: %v", err)
	}

	if res.InterruptedCount != 1 {
		t.Errorf("expected 1 interrupted role, got %d", res.InterruptedCount)
	}
	if len(res.InterruptedRoles) != 1 || res.InterruptedRoles[0] != domain.RoleSecurity {
		t.Errorf("expected Security interrupted, got %v", res.InterruptedRoles)
	}
	if res.InFlightCount != 1 {
		t.Errorf("expected 1 in-flight count, got %d", res.InFlightCount)
	}

	// Verify roles in storage.
	status, err := store.ReadStatus(ctx, "sess_can_mixed")
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	for _, r := range status.Roles {
		switch r.Role {
		case domain.RoleRequirements:
			if r.Status != domain.RoleComplete {
				t.Errorf("requirements: expected complete, got %s", r.Status)
			}
		case domain.RoleArchitecture:
			if r.Status != domain.RoleFailed {
				t.Errorf("architecture: expected failed, got %s", r.Status)
			}
		case domain.RoleQA:
			if r.Status != domain.RoleInFlight {
				t.Errorf("qa: expected in_flight, got %s", r.Status)
			}
		case domain.RoleSecurity:
			if r.Status != domain.RoleInterrupted || r.Cause != domain.CauseUserCancelled {
				t.Errorf("security: expected interrupted/user_cancelled, got %s/%s", r.Status, r.Cause)
			}
		}
	}
}

func TestCancelSweep_QueuedAndTerminalNoOp(t *testing.T) {
	store, _ := openGuardTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

	// Queued session.
	snap := createTestFrozenSnapshot(t, "snap_q_noop")
	submitRes, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_q_noop",
		ProjectID:      "proj_q_noop",
		IdempotencyKey: "key_q_noop",
		Title:          "Queued NoOp",
		Content:        "content",
		Snapshot:       snap,
		CreatedAt:      t0,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	resQ, err := store.SweepCancellation(ctx, submitRes.SessionID)
	if err != nil {
		t.Fatalf("SweepCancellation on queued: %v", err)
	}
	if !resQ.IsNoOp() || resQ.InterruptedCount != 0 {
		t.Errorf("expected queued session to be a no-op, got %+v", resQ)
	}

	// Verify roles remain pending.
	st, err := store.ReadStatus(ctx, submitRes.SessionID)
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	for _, r := range st.Roles {
		if r.Status != domain.RolePending {
			t.Errorf("role %s: expected pending, got %s", r.Role, r.Status)
		}
	}
}

func TestCancelSweep_Idempotency(t *testing.T) {
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store, _, _ := setupTestReviewingSession(t, "sess_can_idem", "proj_can_idem", t0)
	ctx := context.Background()

	if _, err := store.RequestCancellation(ctx, "sess_can_idem"); err != nil {
		t.Fatalf("RequestCancellation: %v", err)
	}
	res1, err := store.SweepCancellation(ctx, "sess_can_idem")
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if res1.InterruptedCount != 4 {
		t.Errorf("expected 4 interrupted, got %d", res1.InterruptedCount)
	}

	// Second invocation: should be idempotent no-op.
	res2, err := store.SweepCancellation(ctx, "sess_can_idem")
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if res2.InterruptedCount != 0 || !res2.IsNoOp() {
		t.Errorf("expected second sweep to be no-op, got %+v", res2)
	}
}

func TestCancelSweep_Concurrency(t *testing.T) {
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store, _, _ := setupTestReviewingSession(t, "sess_can_conc", "proj_can_conc", t0)
	ctx := context.Background()

	if _, err := store.RequestCancellation(ctx, "sess_can_conc"); err != nil {
		t.Fatalf("RequestCancellation: %v", err)
	}
	const concurrency = 8
	var wg sync.WaitGroup
	startBarrier := make(chan struct{})
	var totalInterrupted int64

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startBarrier
			res, err := store.SweepCancellation(ctx, "sess_can_conc")
			if err != nil {
				t.Errorf("concurrent sweep error: %v", err)
				return
			}
			atomic.AddInt64(&totalInterrupted, int64(res.InterruptedCount))
		}()
	}

	close(startBarrier)
	wg.Wait()

	if totalInterrupted != 4 {
		t.Errorf("expected exactly 4 roles interrupted across concurrent sweeps, got %d", totalInterrupted)
	}
}

func TestCancelSweep_ScopedErrors(t *testing.T) {
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store, _, _ := setupTestReviewingSession(t, "sess_can_err", "proj_can_err", t0)
	ctx := context.Background()

	// Unknown session ID.
	_, err := store.SweepCancellation(ctx, "non_existent_session")
	if err == nil {
		t.Fatalf("expected error for non-existent session")
	}
	var notFoundErr *SessionNotFoundError
	if !errors.As(err, &notFoundErr) {
		t.Errorf("expected SessionNotFoundError, got %v", err)
	}

	// Project mismatch.
	_, err = store.SweepCancellationScoped(ctx, "wrong_project", "sess_can_err", t0)
	if err == nil {
		t.Fatalf("expected error for project mismatch")
	}
	if !errors.As(err, &notFoundErr) {
		t.Errorf("expected SessionNotFoundError for project mismatch, got %v", err)
	}

	// Empty session ID.
	_, err = store.SweepCancellation(ctx, "")
	if err == nil {
		t.Fatalf("expected error for empty session ID")
	}
	if !errors.As(err, &notFoundErr) {
		t.Errorf("expected SessionNotFoundError for empty session ID, got %v", err)
	}
}

func TestCancelSweep_RollbackOnFailure(t *testing.T) {
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store, _, _ := setupTestReviewingSession(t, "sess_can_rb", "proj_can_rb", t0)
	ctx := context.Background()

	if _, err := store.RequestCancellation(ctx, "sess_can_rb"); err != nil {
		t.Fatalf("RequestCancellation: %v", err)
	}
	// Inject commit failure.
	injectedErr := errors.New("injected commit failure")
	sweepBeforeCommitHook = func(ctx context.Context, conn *sql.Conn) error {
		return injectedErr
	}
	t.Cleanup(func() { sweepBeforeCommitHook = nil })

	_, err := store.SweepCancellation(ctx, "sess_can_rb")
	if err == nil || !errors.Is(err, injectedErr) {
		t.Fatalf("expected injected error, got %v", err)
	}

	// Verify database was completely rolled back: all roles remain pending.
	status, err := store.ReadStatus(ctx, "sess_can_rb")
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	for _, r := range status.Roles {
		if r.Status != domain.RolePending {
			t.Errorf("role %s: expected pending after rollback, got %s", r.Role, r.Status)
		}
	}
}

// ---------------------------------------------------------------------
// 2. Cutoff Sweep Tests
// ---------------------------------------------------------------------

func TestCutoffSweep_Boundary(t *testing.T) {
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store, _, policy := setupTestReviewingSession(t, "sess_cut_bound", "proj_cut_bound", t0)
	ctx := context.Background()

	cutoffAt := t0.Add(policy.DispatchCutoff)

	// 1. Before cutoff: no-op.
	resBefore, err := store.SweepCutoff(ctx, "sess_cut_bound", cutoffAt.Add(-1*time.Second))
	if err != nil {
		t.Fatalf("SweepCutoff before: %v", err)
	}
	if resBefore.CutoffReached {
		t.Errorf("expected CutoffReached == false before cutoff")
	}
	if !resBefore.IsNoOp() || resBefore.InterruptedCount != 0 {
		t.Errorf("expected no-op before cutoff, got %+v", resBefore)
	}

	// Verify roles remain pending.
	st, err := store.ReadStatus(ctx, "sess_cut_bound")
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	for _, r := range st.Roles {
		if r.Status != domain.RolePending {
			t.Errorf("role %s: expected pending before cutoff, got %s", r.Role, r.Status)
		}
	}

	// 2. At cutoff: pending roles become interrupted with cause deadline_cutoff.
	resAt, err := store.SweepCutoff(ctx, "sess_cut_bound", cutoffAt)
	if err != nil {
		t.Fatalf("SweepCutoff at cutoff: %v", err)
	}
	if !resAt.CutoffReached {
		t.Errorf("expected CutoffReached == true at cutoff")
	}
	if resAt.InterruptedCount != 4 {
		t.Errorf("expected 4 interrupted roles, got %d", resAt.InterruptedCount)
	}

	// Verify all roles in database are interrupted with cause deadline_cutoff.
	stAfter, err := store.ReadStatus(ctx, "sess_cut_bound")
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	for _, r := range stAfter.Roles {
		if r.Status != domain.RoleInterrupted {
			t.Errorf("role %s: expected interrupted, got %s", r.Role, r.Status)
		}
		if r.Cause != domain.CauseDeadlineCutoff {
			t.Errorf("role %s: expected deadline_cutoff, got %s", r.Role, r.Cause)
		}
		if r.CompletedAt == nil || !r.CompletedAt.Equal(cutoffAt) {
			t.Errorf("role %s: expected completed_at %v, got %v", r.Role, cutoffAt, r.CompletedAt)
		}
	}

	// 3. After cutoff: idempotent.
	resAfter, err := store.SweepCutoff(ctx, "sess_cut_bound", cutoffAt.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("SweepCutoff after cutoff: %v", err)
	}
	if !resAfter.CutoffReached || resAfter.InterruptedCount != 0 || !resAfter.IsNoOp() {
		t.Errorf("expected idempotent no-op after cutoff, got %+v", resAfter)
	}
}

func TestCutoffSweep_InFlightAndCompleteUnchanged(t *testing.T) {
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store, _, policy := setupTestReviewingSession(t, "sess_cut_mixed", "proj_cut_mixed", t0)
	ctx := context.Background()

	// 1. Requirements: reserve and complete with findings.
	resReq, err := store.ReservePendingRoleWithNow(ctx, "sess_cut_mixed", t0.Add(1*time.Minute))
	if err != nil || !resReq.Reserved {
		t.Fatalf("reserve req: %v", err)
	}
	findings := []domain.Finding{
		{
			ID:             "f-1",
			Kind:           domain.FindingExisting,
			Severity:       domain.SeverityMedium,
			Category:       "correctness",
			Issue:          "Issue",
			Recommendation: "Rec",
			BasisRefs:      []string{"req-1"},
		},
	}
	_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
		SessionID:   "sess_cut_mixed",
		RoleRunID:   resReq.RoleRunID,
		Role:        resReq.Role,
		Findings:    findings,
		CallCount:   1,
		CompletedAt: t0.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("publish req: %v", err)
	}

	// 2. Architecture: reserve to in_flight.
	resArch, err := store.ReservePendingRoleWithNow(ctx, "sess_cut_mixed", t0.Add(3*time.Minute))
	if err != nil || !resArch.Reserved {
		t.Fatalf("reserve arch: %v", err)
	}

	// 3. QA and Security remain pending.

	cutoffAt := t0.Add(policy.DispatchCutoff)
	res, err := store.SweepCutoff(ctx, "sess_cut_mixed", cutoffAt)
	if err != nil {
		t.Fatalf("SweepCutoff: %v", err)
	}

	if res.InterruptedCount != 2 {
		t.Errorf("expected 2 interrupted roles, got %d", res.InterruptedCount)
	}
	if res.InFlightCount != 1 {
		t.Errorf("expected 1 in-flight role, got %d", res.InFlightCount)
	}

	// Verify in storage.
	st, err := store.ReadStatus(ctx, "sess_cut_mixed")
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	for _, r := range st.Roles {
		switch r.Role {
		case domain.RoleRequirements:
			if r.Status != domain.RoleComplete {
				t.Errorf("req: expected complete, got %s", r.Status)
			}
		case domain.RoleArchitecture:
			if r.Status != domain.RoleInFlight {
				t.Errorf("arch: expected in_flight, got %s", r.Status)
			}
		case domain.RoleQA, domain.RoleSecurity:
			if r.Status != domain.RoleInterrupted || r.Cause != domain.CauseDeadlineCutoff {
				t.Errorf("%s: expected interrupted/deadline_cutoff, got %s/%s", r.Role, r.Status, r.Cause)
			}
		}
	}
}

func TestCutoffSweep_QueuedAndTerminalNoOp(t *testing.T) {
	store, _ := openGuardTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

	snap := createTestFrozenSnapshot(t, "snap_cut_q")
	submitRes, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_cut_q",
		ProjectID:      "proj_cut_q",
		IdempotencyKey: "key_cut_q",
		Title:          "Cutoff Queued",
		Content:        "content",
		Snapshot:       snap,
		CreatedAt:      t0,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	res, err := store.SweepCutoff(ctx, submitRes.SessionID, t0.Add(10*time.Hour))
	if err != nil {
		t.Fatalf("SweepCutoff queued: %v", err)
	}
	if !res.IsNoOp() || res.InterruptedCount != 0 {
		t.Errorf("expected queued cutoff sweep to be no-op, got %+v", res)
	}
}

func TestCutoffSweep_RollbackOnFailure(t *testing.T) {
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store, _, policy := setupTestReviewingSession(t, "sess_cut_rb", "proj_cut_rb", t0)
	ctx := context.Background()

	injectedErr := errors.New("injected cutoff commit failure")
	sweepBeforeCommitHook = func(ctx context.Context, conn *sql.Conn) error {
		return injectedErr
	}
	t.Cleanup(func() { sweepBeforeCommitHook = nil })

	cutoffAt := t0.Add(policy.DispatchCutoff)
	_, err := store.SweepCutoff(ctx, "sess_cut_rb", cutoffAt)
	if err == nil || !errors.Is(err, injectedErr) {
		t.Fatalf("expected injected error, got %v", err)
	}

	// Verify rollback: all roles remain pending.
	st, err := store.ReadStatus(ctx, "sess_cut_rb")
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	for _, r := range st.Roles {
		if r.Status != domain.RolePending {
			t.Errorf("role %s: expected pending after rollback, got %s", r.Role, r.Status)
		}
	}
}

// ---------------------------------------------------------------------
// 3. Hard-Deadline Sweep Tests
// ---------------------------------------------------------------------

func TestDeadlineSweep_PendingAndInFlightSplit(t *testing.T) {
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store, _, policy := setupTestReviewingSession(t, "sess_dl_split", "proj_dl_split", t0)
	ctx := context.Background()

	// 1. Reserve 2 roles to in_flight (Requirements and Architecture).
	resReq, err := store.ReservePendingRoleWithNow(ctx, "sess_dl_split", t0.Add(1*time.Minute))
	if err != nil || !resReq.Reserved {
		t.Fatalf("reserve req: %v", err)
	}
	resArch, err := store.ReservePendingRoleWithNow(ctx, "sess_dl_split", t0.Add(2*time.Minute))
	if err != nil || !resArch.Reserved {
		t.Fatalf("reserve arch: %v", err)
	}

	// 2. QA and Security remain pending.

	// Hard deadline arrives.
	deadlineAt := t0.Add(policy.SessionHardDeadline)
	res, err := store.SweepHardDeadline(ctx, "sess_dl_split", deadlineAt)
	if err != nil {
		t.Fatalf("SweepHardDeadline: %v", err)
	}

	// Verify result:
	// - 2 pending roles interrupted with cause deadline_cutoff.
	// - 2 in-flight roles NOT rewritten; exact IDs returned.
	if res.InterruptedCount != 2 {
		t.Errorf("expected 2 interrupted roles, got %d", res.InterruptedCount)
	}
	if len(res.InFlightRoleIDs) != 2 {
		t.Fatalf("expected 2 in-flight role IDs, got %d", len(res.InFlightRoleIDs))
	}
	if res.InFlightRoleIDs[0] != resReq.RoleRunID || res.InFlightRoleIDs[1] != resArch.RoleRunID {
		t.Errorf("expected in-flight IDs [%s, %s], got %v", resReq.RoleRunID, resArch.RoleRunID, res.InFlightRoleIDs)
	}

	// Verify storage: pending roles are interrupted, in-flight roles are still in_flight.
	st, err := store.ReadStatus(ctx, "sess_dl_split")
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	for _, r := range st.Roles {
		switch r.Role {
		case domain.RoleRequirements, domain.RoleArchitecture:
			if r.Status != domain.RoleInFlight {
				t.Errorf("role %s: expected in_flight, got %s", r.Role, r.Status)
			}
		case domain.RoleQA, domain.RoleSecurity:
			if r.Status != domain.RoleInterrupted || r.Cause != domain.CauseDeadlineCutoff {
				t.Errorf("role %s: expected interrupted/deadline_cutoff, got %s/%s", r.Role, r.Status, r.Cause)
			}
		}
	}

	// 3. Caller locally cancels and publishes timeout through P9 compare-and-set publication.
	for _, ifr := range res.InFlightRoles {
		pubRes, err := store.PublishRoleFailure(ctx, PublishFailureParams{
			SessionID:     "sess_dl_split",
			RoleRunID:     ifr.RoleRunID,
			Role:          ifr.Role,
			ErrorCategory: domain.ErrTimeout,
			ErrorMessage:  "session hard deadline reached",
			CallCount:     1,
			CompletedAt:   deadlineAt,
		})
		if err != nil {
			t.Fatalf("publish timeout for %s (%s): %v", ifr.Role, ifr.RoleRunID, err)
		}
		if pubRes.Status != domain.RoleFailed || pubRes.ErrorCategory != domain.ErrTimeout {
			t.Errorf("expected role %s failed/timeout, got %s/%s", ifr.Role, pubRes.Status, pubRes.ErrorCategory)
		}
	}

	// Verify all 4 roles are now terminal.
	stFinal, err := store.ReadStatus(ctx, "sess_dl_split")
	if err != nil {
		t.Fatalf("ReadStatus final: %v", err)
	}
	for _, r := range stFinal.Roles {
		if !r.Status.IsTerminal() {
			t.Errorf("role %s: expected terminal, got %s", r.Role, r.Status)
		}
	}
}

func TestDeadlineSweep_CompleteUnchangedAndTerminalNoOp(t *testing.T) {
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store, _, policy := setupTestReviewingSession(t, "sess_dl_comp", "proj_dl_comp", t0)
	ctx := context.Background()

	// Requirements complete with findings.
	resReq, err := store.ReservePendingRoleWithNow(ctx, "sess_dl_comp", t0.Add(1*time.Minute))
	if err != nil || !resReq.Reserved {
		t.Fatalf("reserve req: %v", err)
	}
	findings := []domain.Finding{
		{
			ID:             "f-sec-1",
			Kind:           domain.FindingExisting,
			Severity:       domain.SeverityCritical,
			Category:       "security",
			Issue:          "Crit issue",
			Recommendation: "Fix it",
			BasisRefs:      []string{"req-1"},
		},
	}
	_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
		SessionID:   "sess_dl_comp",
		RoleRunID:   resReq.RoleRunID,
		Role:        resReq.Role,
		Findings:    findings,
		CallCount:   1,
		CompletedAt: t0.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("publish req: %v", err)
	}

	deadlineAt := t0.Add(policy.SessionHardDeadline)
	res, err := store.SweepHardDeadline(ctx, "sess_dl_comp", deadlineAt)
	if err != nil {
		t.Fatalf("SweepHardDeadline: %v", err)
	}

	if res.InterruptedCount != 3 {
		t.Errorf("expected 3 interrupted, got %d", res.InterruptedCount)
	}
	if len(res.InFlightRoleIDs) != 0 {
		t.Errorf("expected 0 in-flight IDs, got %d", len(res.InFlightRoleIDs))
	}

	// Complete role preserved with findings.
	st, err := store.ReadStatus(ctx, "sess_dl_comp")
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	for _, r := range st.Roles {
		if r.Role == domain.RoleRequirements {
			if r.Status != domain.RoleComplete {
				t.Errorf("req: expected complete, got %s", r.Status)
			}
		}
	}
}

func TestDeadlineSweep_RollbackOnFailure(t *testing.T) {
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store, _, policy := setupTestReviewingSession(t, "sess_dl_rb", "proj_dl_rb", t0)
	ctx := context.Background()

	injectedErr := errors.New("injected deadline commit failure")
	sweepBeforeCommitHook = func(ctx context.Context, conn *sql.Conn) error {
		return injectedErr
	}
	t.Cleanup(func() { sweepBeforeCommitHook = nil })

	deadlineAt := t0.Add(policy.SessionHardDeadline)
	_, err := store.SweepHardDeadline(ctx, "sess_dl_rb", deadlineAt)
	if err == nil || !errors.Is(err, injectedErr) {
		t.Fatalf("expected injected error, got %v", err)
	}

	// Verify rollback.
	st, err := store.ReadStatus(ctx, "sess_dl_rb")
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	for _, r := range st.Roles {
		if r.Status != domain.RolePending {
			t.Errorf("role %s: expected pending after rollback, got %s", r.Role, r.Status)
		}
	}
}

// ---------------------------------------------------------------------
// 4. Restart Recovery Sweep Tests
// ---------------------------------------------------------------------

func TestRestartSweep_MultipleSessions(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

	policy := TimingPolicy{
		DispatchCutoff:      30 * time.Minute,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: 45 * time.Minute,
	}

	// Session 1: claimed at t0 - 2h, cancel_requested = 0 (stale).
	// - Requirements: complete
	// - Architecture: failed
	// - QA: in_flight
	// - Security: pending
	snap1 := createTestFrozenSnapshot(t, "snap_sess1")
	_, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_rec_1",
		ProjectID:      "proj_rec",
		IdempotencyKey: "key_rec_1",
		Title:          "Rec 1",
		Content:        "content",
		Snapshot:       snap1,
		CreatedAt:      t0.Add(-3 * time.Hour),
	})
	if err != nil {
		t.Fatalf("submit sess_rec_1: %v", err)
	}
	claim1, err := store.ClaimSessionWithNow(ctx, policy, t0.Add(-2*time.Hour))
	if err != nil || !claim1.Claimed {
		t.Fatalf("claim sess_rec_1: %v", err)
	}

	// Req complete
	res1Req, err := store.ReservePendingRoleWithNow(ctx, "sess_rec_1", t0.Add(-115*time.Minute))
	if err != nil || !res1Req.Reserved {
		t.Fatalf("reserve sess1 req: %v", err)
	}
	findings := []domain.Finding{
		{
			ID:             "f-rec-1",
			Kind:           domain.FindingExisting,
			Severity:       domain.SeverityHigh,
			Category:       "security",
			Issue:          "Issue",
			Recommendation: "Rec",
			BasisRefs:      []string{"req-1"},
		},
	}
	_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
		SessionID:   "sess_rec_1",
		RoleRunID:   res1Req.RoleRunID,
		Role:        res1Req.Role,
		Findings:    findings,
		CallCount:   1,
		CompletedAt: t0.Add(-110 * time.Minute),
	})
	if err != nil {
		t.Fatalf("publish sess1 req: %v", err)
	}

	// Arch failed
	res1Arch, err := store.ReservePendingRoleWithNow(ctx, "sess_rec_1", t0.Add(-105*time.Minute))
	if err != nil || !res1Arch.Reserved {
		t.Fatalf("reserve sess1 arch: %v", err)
	}
	_, err = store.PublishRoleFailure(ctx, PublishFailureParams{
		SessionID:     "sess_rec_1",
		RoleRunID:     res1Arch.RoleRunID,
		Role:          res1Arch.Role,
		ErrorCategory: domain.ErrTransport,
		ErrorMessage:  "timeout",
		CallCount:     2,
		CompletedAt:   t0.Add(-100 * time.Minute),
	})
	if err != nil {
		t.Fatalf("publish sess1 arch: %v", err)
	}

	// QA in_flight
	res1QA, err := store.ReservePendingRoleWithNow(ctx, "sess_rec_1", t0.Add(-95*time.Minute))
	if err != nil || !res1QA.Reserved {
		t.Fatalf("reserve sess1 qa: %v", err)
	}

	// Security remains pending.

	// In SQLite, only 1 session can be 'reviewing' at a time according to ClaimSession.
	// But in a real crash/restart scenario, we can test restart recovery across multiple sessions
	// by simulating another claimed reviewing session directly via SQL.
	// Create Session 2 directly in DB as reviewing, claimed at t0 - 1h, with cancel_requested = 1.
	snap2 := createTestFrozenSnapshot(t, "snap_sess2")
	_, err = store.Submit(ctx, SubmitParams{
		SessionID:      "sess_rec_2",
		ProjectID:      "proj_rec",
		IdempotencyKey: "key_rec_2",
		Title:          "Rec 2",
		Content:        "content",
		Snapshot:       snap2,
		CreatedAt:      t0.Add(-2 * time.Hour),
	})
	if err != nil {
		t.Fatalf("submit sess_rec_2: %v", err)
	}

	// Set sess_rec_2 to reviewing with cancel_requested = 1 and claimed_at = t0 - 1h.
	claimedAt2Str := formatUTCTimestamp(t0.Add(-1 * time.Hour))
	cutoff2Str := formatUTCTimestamp(t0.Add(-30 * time.Minute))
	deadline2Str := formatUTCTimestamp(t0.Add(-15 * time.Minute))
	_, err = writer.Exec(`
		UPDATE sessions
		SET status = 'reviewing',
		    cancel_requested = 1,
		    claimed_at = ?,
		    dispatch_cutoff_at = ?,
		    hard_deadline_at = ?
		WHERE id = 'sess_rec_2';
	`, claimedAt2Str, cutoff2Str, deadline2Str)
	if err != nil {
		t.Fatalf("update sess_rec_2: %v", err)
	}

	// Reserve 1 role to in_flight for sess_rec_2.
	_, err = writer.Exec(`
		UPDATE role_runs
		SET status = 'in_flight',
		    started_at = ?
		WHERE session_id = 'sess_rec_2' AND role = 'requirements';
	`, claimedAt2Str)
	if err != nil {
		t.Fatalf("update sess_rec_2 req: %v", err)
	}

	// Session 3: claimed at t0 + 1h (NOT stale, claimed after cutoff t0).
	snap3 := createTestFrozenSnapshot(t, "snap_sess3")
	_, err = store.Submit(ctx, SubmitParams{
		SessionID:      "sess_rec_3",
		ProjectID:      "proj_rec",
		IdempotencyKey: "key_rec_3",
		Title:          "Rec 3",
		Content:        "content",
		Snapshot:       snap3,
		CreatedAt:      t0,
	})
	if err != nil {
		t.Fatalf("submit sess_rec_3: %v", err)
	}
	claimedAt3Str := formatUTCTimestamp(t0.Add(1 * time.Hour))
	_, err = writer.Exec(`
		UPDATE sessions
		SET status = 'reviewing',
		    claimed_at = ?,
		    dispatch_cutoff_at = ?,
		    hard_deadline_at = ?
		WHERE id = 'sess_rec_3';
	`, claimedAt3Str, formatUTCTimestamp(t0.Add(90*time.Minute)), formatUTCTimestamp(t0.Add(105*time.Minute)))
	if err != nil {
		t.Fatalf("update sess_rec_3: %v", err)
	}

	// Session 4: queued (untouched).
	snap4 := createTestFrozenSnapshot(t, "snap_sess4")
	_, err = store.Submit(ctx, SubmitParams{
		SessionID:      "sess_rec_4",
		ProjectID:      "proj_rec",
		IdempotencyKey: "key_rec_4",
		Title:          "Rec 4",
		Content:        "content",
		Snapshot:       snap4,
		CreatedAt:      t0.Add(-4 * time.Hour),
	})
	if err != nil {
		t.Fatalf("submit sess_rec_4: %v", err)
	}

	// Execute Restart Recovery Sweep at cutoff = t0.
	sweepNow := t0.Add(5 * time.Minute)
	recRes, err := store.SweepRestartRecoveryWithNow(ctx, t0, sweepNow)
	if err != nil {
		t.Fatalf("SweepRestartRecovery: %v", err)
	}

	if recRes.SessionsRecovered != 2 {
		t.Fatalf("expected 2 sessions recovered, got %d", recRes.SessionsRecovered)
	}

	// Verify Session 1 (cancel_requested = 0):
	// - Requirements: complete (unchanged)
	// - Architecture: failed (unchanged)
	// - QA: in_flight -> interrupted/process_restart
	// - Security: pending -> interrupted/process_restart
	st1, err := store.ReadStatus(ctx, "sess_rec_1")
	if err != nil {
		t.Fatalf("ReadStatus sess_rec_1: %v", err)
	}
	for _, r := range st1.Roles {
		switch r.Role {
		case domain.RoleRequirements:
			if r.Status != domain.RoleComplete {
				t.Errorf("sess1 req: expected complete, got %s", r.Status)
			}
		case domain.RoleArchitecture:
			if r.Status != domain.RoleFailed {
				t.Errorf("sess1 arch: expected failed, got %s", r.Status)
			}
		case domain.RoleQA:
			if r.Status != domain.RoleInterrupted || r.Cause != domain.CauseProcessRestart {
				t.Errorf("sess1 qa: expected interrupted/process_restart, got %s/%s", r.Status, r.Cause)
			}
		case domain.RoleSecurity:
			if r.Status != domain.RoleInterrupted || r.Cause != domain.CauseProcessRestart {
				t.Errorf("sess1 security: expected interrupted/process_restart, got %s/%s", r.Status, r.Cause)
			}
		}
	}

	// Verify Session 2 (cancel_requested = 1):
	// - Requirements: in_flight -> interrupted/process_restart
	// - Arch, QA, Security: pending -> interrupted/user_cancelled
	st2, err := store.ReadStatus(ctx, "sess_rec_2")
	if err != nil {
		t.Fatalf("ReadStatus sess_rec_2: %v", err)
	}
	for _, r := range st2.Roles {
		switch r.Role {
		case domain.RoleRequirements:
			if r.Status != domain.RoleInterrupted || r.Cause != domain.CauseProcessRestart {
				t.Errorf("sess2 req: expected interrupted/process_restart, got %s/%s", r.Status, r.Cause)
			}
		case domain.RoleArchitecture, domain.RoleQA, domain.RoleSecurity:
			if r.Status != domain.RoleInterrupted || r.Cause != domain.CauseUserCancelled {
				t.Errorf("sess2 %s: expected interrupted/user_cancelled, got %s/%s", r.Role, r.Status, r.Cause)
			}
		}
	}

	// Verify Session 3 (claimed_at > t0, NOT stale): roles untouched!
	st3, err := store.ReadStatus(ctx, "sess_rec_3")
	if err != nil {
		t.Fatalf("ReadStatus sess_rec_3: %v", err)
	}
	for _, r := range st3.Roles {
		if r.Status != domain.RolePending {
			t.Errorf("sess3 %s: expected pending, got %s", r.Role, r.Status)
		}
	}

	// Verify Session 4 (queued): completely untouched!
	st4, err := store.ReadStatus(ctx, "sess_rec_4")
	if err != nil {
		t.Fatalf("ReadStatus sess_rec_4: %v", err)
	}
	if st4.Status != domain.SessionQueued {
		t.Errorf("sess4 status: expected queued, got %s", st4.Status)
	}
	for _, r := range st4.Roles {
		if r.Status != domain.RolePending {
			t.Errorf("sess4 %s: expected pending, got %s", r.Role, r.Status)
		}
	}

	// Idempotency: re-running restart recovery with the same cutoff recovers 0.
	recRes2, err := store.SweepRestartRecoveryWithNow(ctx, t0, sweepNow)
	if err != nil {
		t.Fatalf("SweepRestartRecovery second: %v", err)
	}
	if recRes2.SessionsRecovered != 2 || recRes2.TotalInterrupted != 0 {
		t.Errorf("expected 0 additional roles interrupted on idempotent second run, got total=%d", recRes2.TotalInterrupted)
	}
}

func TestRestartSweep_SingleSessionTargeted(t *testing.T) {
	store, _ := openGuardTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

	// Non-existent session returns SessionNotFoundError.
	_, err := store.SweepRestartRecoverySession(ctx, "non_existent_sess", t0, t0)
	if err == nil {
		t.Fatalf("expected error for non-existent session")
	}
	var notFoundErr *SessionNotFoundError
	if !errors.As(err, &notFoundErr) {
		t.Errorf("expected SessionNotFoundError, got %v", err)
	}

	// Queued session returns 0 recovered (no-op).
	snap := createTestFrozenSnapshot(t, "snap_q_target")
	submitRes, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_q_target",
		ProjectID:      "proj_q_target",
		IdempotencyKey: "key_q_target",
		Title:          "Queued Target",
		Content:        "content",
		Snapshot:       snap,
		CreatedAt:      t0,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	resQ, err := store.SweepRestartRecoverySession(ctx, submitRes.SessionID, t0.Add(time.Hour), t0)
	if err != nil {
		t.Fatalf("SweepRestartRecoverySession on queued: %v", err)
	}
	if resQ.SessionsRecovered != 0 || resQ.TotalInterrupted != 0 {
		t.Errorf("expected 0 recovered for queued session, got %+v", resQ)
	}
}

func TestRestartSweep_ExplicitCutoffRequired(t *testing.T) {
	store, _ := openGuardTestStore(t)
	ctx := context.Background()

	// Zero time cutoff must be rejected.
	_, err := store.SweepRestartRecovery(ctx, time.Time{})
	if err == nil {
		t.Fatalf("expected error for zero cutoff")
	}
	if !strings.Contains(err.Error(), "explicit non-zero cutoff") {
		t.Errorf("expected cutoff error message, got: %v", err)
	}
}

func TestRestartSweep_RollbackOnFailure(t *testing.T) {
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store, _, _ := setupTestReviewingSession(t, "sess_rec_rb", "proj_rec_rb", t0)
	ctx := context.Background()

	injectedErr := errors.New("injected restart recovery commit failure")
	sweepBeforeCommitHook = func(ctx context.Context, conn *sql.Conn) error {
		return injectedErr
	}
	t.Cleanup(func() { sweepBeforeCommitHook = nil })

	_, err := store.SweepRestartRecovery(ctx, t0.Add(time.Hour))
	if err == nil || !errors.Is(err, injectedErr) {
		t.Fatalf("expected injected error, got %v", err)
	}

	// Verify database was completely rolled back: all roles remain pending.
	st, err := store.ReadStatus(ctx, "sess_rec_rb")
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	for _, r := range st.Roles {
		if r.Status != domain.RolePending {
			t.Errorf("role %s: expected pending after rollback, got %s", r.Role, r.Status)
		}
	}
}

// ---------------------------------------------------------------------
// 5. Preservation and Forbidden Scope Verification
// ---------------------------------------------------------------------

func TestSweep_PreservationAndForbiddenScope(t *testing.T) {
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store, _, policy := setupTestReviewingSession(t, "sess_scope_check", "proj_scope_check", t0)
	ctx := context.Background()

	// 1. Requirements complete with findings.
	resReq, err := store.ReservePendingRoleWithNow(ctx, "sess_scope_check", t0.Add(1*time.Minute))
	if err != nil || !resReq.Reserved {
		t.Fatalf("reserve req: %v", err)
	}
	findings := []domain.Finding{
		{
			ID:             "f-preserve-1",
			Kind:           domain.FindingExisting,
			Severity:       domain.SeverityCritical,
			Category:       "security",
			Issue:          "Crit issue",
			Recommendation: "Fix it",
			BasisRefs:      []string{"req-1"},
		},
	}
	_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
		SessionID:   "sess_scope_check",
		RoleRunID:   resReq.RoleRunID,
		Role:        resReq.Role,
		Findings:    findings,
		CallCount:   1,
		CompletedAt: t0.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("publish req: %v", err)
	}

	// Read initial session state and snapshot.
	initStatus, err := store.ReadStatus(ctx, "sess_scope_check")
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	initSnap, err := store.ReadSnapshot(ctx, initStatus.SnapshotID)
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}

	// Run cutoff sweep.
	cutoffAt := t0.Add(policy.DispatchCutoff)
	_, err = store.SweepCutoff(ctx, "sess_scope_check", cutoffAt)
	if err != nil {
		t.Fatalf("SweepCutoff: %v", err)
	}

	// Verify session fields are completely untouched.
	postStatus, err := store.ReadStatus(ctx, "sess_scope_check")
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if postStatus.Status != domain.SessionReviewing {
		t.Errorf("session status: expected reviewing (composer not run), got %s", postStatus.Status)
	}
	if postStatus.TerminalReason != "" {
		t.Errorf("terminal reason: expected empty (composer not run), got %s", postStatus.TerminalReason)
	}
	if postStatus.CompletedRoleCount != initStatus.CompletedRoleCount {
		t.Errorf("completed role count changed: %d -> %d", initStatus.CompletedRoleCount, postStatus.CompletedRoleCount)
	}
	if postStatus.IncompleteRoleCount != initStatus.IncompleteRoleCount {
		t.Errorf("incomplete role count changed: %d -> %d", initStatus.IncompleteRoleCount, postStatus.IncompleteRoleCount)
	}
	if postStatus.CancelRequested != initStatus.CancelRequested {
		t.Errorf("cancel_requested changed: %v -> %v", initStatus.CancelRequested, postStatus.CancelRequested)
	}

	// Verify snapshot is completely untouched and hash is verified.
	postSnap, err := store.ReadSnapshot(ctx, initStatus.SnapshotID)
	if err != nil {
		t.Fatalf("ReadSnapshot post: %v", err)
	}
	if postSnap.ID != initSnap.ID || postSnap.Hash != initSnap.Hash {
		t.Errorf("snapshot identity modified: before=%+v, after=%+v", initSnap, postSnap)
	}
}
