package sqlite

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
)

func setupPredicateReviewingSession(t *testing.T, sessionID, projectID string, t0 time.Time, policy TimingPolicy) (*Store, *sql.DB) {
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
		Title:          "Predicate Test " + sessionID,
		Content:        "Spec content for predicate test",
		Snapshot:       snap,
		CreatedAt:      t0.Add(-10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("submit %s: %v", sessionID, err)
	}

	claimRes, err := store.ClaimSessionWithNow(ctx, policy, t0)
	if err != nil || !claimRes.Claimed {
		t.Fatalf("claim %s: %v", sessionID, err)
	}

	return store, writer
}

// 1. SweepCancellation on a reviewing session with cancel_requested=0 leaves all 4 roles pending (NoOp, 0 interrupted).
func TestSweepPredicate_1_Cancellation_UncancelledIsNoOp(t *testing.T) {
	t0 := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	policy := TimingPolicy{
		DispatchCutoff:      30 * time.Minute,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: 45 * time.Minute,
	}
	store, _ := setupPredicateReviewingSession(t, "sess_pred_uncanc", "proj_pred_uncanc", t0, policy)
	ctx := context.Background()

	// cancel_requested is 0 on a freshly claimed session.
	res, err := store.SweepCancellationWithNow(ctx, "sess_pred_uncanc", t0.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("SweepCancellationWithNow failed: %v", err)
	}

	if !res.IsNoOp() {
		t.Errorf("expected IsNoOp == true, got false")
	}
	if res.InterruptedCount != 0 {
		t.Errorf("expected 0 interrupted roles, got %d", res.InterruptedCount)
	}
	if len(res.InterruptedRoles) != 0 {
		t.Errorf("expected empty interrupted roles, got %v", res.InterruptedRoles)
	}
	if res.InFlightCount != 0 {
		t.Errorf("expected 0 in-flight roles, got %d", res.InFlightCount)
	}

	// Verify all 4 roles in DB remain pending and untouched.
	status, err := store.ReadStatus(ctx, "sess_pred_uncanc")
	if err != nil {
		t.Fatalf("ReadStatus failed: %v", err)
	}
	for _, r := range status.Roles {
		if r.Status != domain.RolePending {
			t.Errorf("role %s: expected pending, got %s", r.Role, r.Status)
		}
		if r.Cause != "" {
			t.Errorf("role %s: expected empty cause, got %s", r.Role, r.Cause)
		}
		if r.CompletedAt != nil {
			t.Errorf("role %s: expected nil completed_at, got %v", r.Role, r.CompletedAt)
		}
	}
}

// 2. SweepCancellation on a reviewing session with cancel_requested=1 interrupts pending roles
// with cause=user_cancelled and completed_at = nowStr (fixed-width, UTC, Z), in-flight/terminal roles unchanged.
func TestSweepPredicate_2_Cancellation_CancelledInterruptsPending(t *testing.T) {
	t0 := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	policy := TimingPolicy{
		DispatchCutoff:      30 * time.Minute,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: 45 * time.Minute,
	}
	store, writer := setupPredicateReviewingSession(t, "sess_pred_canc", "proj_pred_canc", t0, policy)
	ctx := context.Background()

	// 1. Requirements: reserve -> publish success (terminal complete).
	resReq, err := store.ReservePendingRoleWithNow(ctx, "sess_pred_canc", t0.Add(1*time.Minute))
	if err != nil || !resReq.Reserved {
		t.Fatalf("reserve req: %v", err)
	}
	_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
		SessionID: "sess_pred_canc",
		RoleRunID: resReq.RoleRunID,
		Role:      resReq.Role,
		Findings: []domain.Finding{
			{
				ID:             "f-1",
				Kind:           domain.FindingExisting,
				Severity:       domain.SeverityMedium,
				Category:       "arch",
				Issue:          "Arch issue",
				Recommendation: "Fix arch",
				BasisRefs:      []string{"req-1"},
			},
		},
		CallCount:   1,
		CompletedAt: t0.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("publish req: %v", err)
	}

	// 2. Architecture: reserve -> remains in_flight.
	resArch, err := store.ReservePendingRoleWithNow(ctx, "sess_pred_canc", t0.Add(3*time.Minute))
	if err != nil || !resArch.Reserved {
		t.Fatalf("reserve arch: %v", err)
	}

	// 3. Request cancellation.
	cancRes, err := store.RequestCancellation(ctx, "sess_pred_canc")
	if err != nil || !cancRes.Effective {
		t.Fatalf("RequestCancellation: %v, effective: %v", err, cancRes.Effective)
	}

	// 4. Run SweepCancellation.
	sweepTime := t0.Add(5 * time.Minute)
	res, err := store.SweepCancellationWithNow(ctx, "sess_pred_canc", sweepTime)
	if err != nil {
		t.Fatalf("SweepCancellationWithNow failed: %v", err)
	}

	if res.IsNoOp() {
		t.Errorf("expected IsNoOp == false")
	}
	if res.InterruptedCount != 2 {
		t.Errorf("expected 2 interrupted roles, got %d", res.InterruptedCount)
	}
	if len(res.InterruptedRoles) != 2 {
		t.Fatalf("expected 2 interrupted roles, got %d", len(res.InterruptedRoles))
	}
	if res.InterruptedRoles[0] != domain.RoleQA || res.InterruptedRoles[1] != domain.RoleSecurity {
		t.Errorf("expected [qa, security] interrupted, got %v", res.InterruptedRoles)
	}
	if res.InFlightCount != 1 {
		t.Errorf("expected 1 in-flight role, got %d", res.InFlightCount)
	}

	// Verify database state.
	status, err := store.ReadStatus(ctx, "sess_pred_canc")
	if err != nil {
		t.Fatalf("ReadStatus failed: %v", err)
	}
	for _, r := range status.Roles {
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
			if r.Status != domain.RoleInterrupted {
				t.Errorf("%s: expected interrupted, got %s", r.Role, r.Status)
			}
			if r.Cause != domain.CauseUserCancelled {
				t.Errorf("%s: expected user_cancelled, got %s", r.Role, r.Cause)
			}
			if r.CompletedAt == nil || !r.CompletedAt.Equal(sweepTime) {
				t.Errorf("%s: expected completed_at %v, got %v", r.Role, sweepTime, r.CompletedAt)
			}
		}
	}

	// Verify raw fixed-width completed_at in SQL.
	var rawCompletedAt string
	err = writer.QueryRowContext(ctx, `SELECT completed_at FROM role_runs WHERE session_id = 'sess_pred_canc' AND role = 'qa';`).Scan(&rawCompletedAt)
	if err != nil {
		t.Fatalf("scan raw completed_at: %v", err)
	}
	expectedStr := formatUTCTimestamp(sweepTime)
	if rawCompletedAt != expectedStr {
		t.Errorf("raw completed_at: got %q, want fixed-width %q", rawCompletedAt, expectedStr)
	}
}

// 3. SweepCancellation idempotency: second call on already-swept session is NoOp.
func TestSweepPredicate_3_Cancellation_Idempotency(t *testing.T) {
	t0 := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	policy := TimingPolicy{
		DispatchCutoff:      30 * time.Minute,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: 45 * time.Minute,
	}
	store, _ := setupPredicateReviewingSession(t, "sess_pred_idem", "proj_pred_idem", t0, policy)
	ctx := context.Background()

	_, err := store.RequestCancellation(ctx, "sess_pred_idem")
	if err != nil {
		t.Fatalf("RequestCancellation: %v", err)
	}

	sweepTime1 := t0.Add(5 * time.Minute)
	res1, err := store.SweepCancellationWithNow(ctx, "sess_pred_idem", sweepTime1)
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if res1.InterruptedCount != 4 {
		t.Errorf("first sweep expected 4 interrupted, got %d", res1.InterruptedCount)
	}

	// Second sweep invocation must be NoOp.
	sweepTime2 := t0.Add(6 * time.Minute)
	res2, err := store.SweepCancellationWithNow(ctx, "sess_pred_idem", sweepTime2)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if !res2.IsNoOp() {
		t.Errorf("second sweep expected IsNoOp == true, got false")
	}
	if res2.InterruptedCount != 0 {
		t.Errorf("second sweep expected 0 interrupted, got %d", res2.InterruptedCount)
	}
}

// 4. SweepCancellation with concurrent cancel commit: barrier test proves sweep rechecks
// authoritative predicate (exactly one winner behavior).
func TestSweepPredicate_4_Cancellation_ConcurrentCancelCommitBarrier(t *testing.T) {
	t0 := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	policy := TimingPolicy{
		DispatchCutoff:      30 * time.Minute,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: 45 * time.Minute,
	}
	store, _ := setupPredicateReviewingSession(t, "sess_canc_barrier", "proj_canc_barrier", t0, policy)
	ctx := context.Background()

	// 1 cancel committer + 4 concurrent sweepers behind a single barrier.
	const sweepers = 4
	barrier := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1 + sweepers)

	var cancelRes *CancelResult
	var cancelErr error
	go func() {
		defer wg.Done()
		<-barrier
		cancelRes, cancelErr = store.RequestCancellation(ctx, "sess_canc_barrier")
	}()

	results := make([]*SweepCancellationResult, sweepers)
	errs := make([]error, sweepers)
	for i := 0; i < sweepers; i++ {
		go func(idx int) {
			defer wg.Done()
			<-barrier
			results[idx], errs[idx] = store.SweepCancellationWithNow(ctx, "sess_canc_barrier", t0.Add(5*time.Minute))
		}(i)
	}

	close(barrier)
	wg.Wait()

	if cancelErr != nil {
		t.Fatalf("RequestCancellation: %v", cancelErr)
	}
	if !cancelRes.Effective {
		t.Fatalf("expected cancel effective, got %v", cancelRes.Effective)
	}

	var totalInterrupted int
	var sweepWinners int
	for i := 0; i < sweepers; i++ {
		if errs[i] != nil {
			t.Fatalf("sweeper %d failed: %v", i, errs[i])
		}
		if results[i].InterruptedCount > 0 {
			sweepWinners++
			totalInterrupted += results[i].InterruptedCount
			if results[i].InterruptedCount != 4 {
				t.Errorf("winner sweeper %d interrupted %d roles, want 4", i, results[i].InterruptedCount)
			}
		}
	}

	// At most 1 sweeper could have won during the race (if any ran after cancel commit).
	if sweepWinners > 1 {
		t.Errorf("expected at most 1 sweep winner during concurrent execution, got %d", sweepWinners)
	}

	// Final sweep guarantees that now with cancel_requested = 1, exactly 4 roles are interrupted in total.
	finalRes, err := store.SweepCancellationWithNow(ctx, "sess_canc_barrier", t0.Add(6*time.Minute))
	if err != nil {
		t.Fatalf("final sweep: %v", err)
	}
	totalInterrupted += finalRes.InterruptedCount
	if finalRes.InterruptedCount > 0 {
		sweepWinners++
	}

	if totalInterrupted != 4 {
		t.Errorf("expected exactly 4 total interrupted roles, got %d", totalInterrupted)
	}
	if sweepWinners != 1 {
		t.Errorf("expected exactly 1 winner across concurrent and final sweeps, got %d", sweepWinners)
	}

	// Verify all 4 roles in DB are now interrupted with cause user_cancelled.
	st, err := store.ReadStatus(ctx, "sess_canc_barrier")
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	for _, r := range st.Roles {
		if r.Status != domain.RoleInterrupted || r.Cause != domain.CauseUserCancelled {
			t.Errorf("role %s: expected interrupted/user_cancelled, got %s/%s", r.Role, r.Status, r.Cause)
		}
	}
}

// 5. SweepHardDeadline before hard_deadline_at is NoOp (0 interrupted, pending roles remain pending),
// even with pending roles present.
func TestSweepPredicate_5_HardDeadline_BeforeDeadlineIsNoOp(t *testing.T) {
	t0 := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	policy := TimingPolicy{
		DispatchCutoff:      30 * time.Minute,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: 45 * time.Minute,
	}
	store, _ := setupPredicateReviewingSession(t, "sess_dl_before", "proj_dl_before", t0, policy)
	ctx := context.Background()

	// 1 role in_flight, 3 pending.
	resReq, err := store.ReservePendingRoleWithNow(ctx, "sess_dl_before", t0.Add(1*time.Minute))
	if err != nil || !resReq.Reserved {
		t.Fatalf("reserve req: %v", err)
	}

	// now is at t0 + 30m, which is strictly before hard deadline t0 + 45m.
	sweepTime := t0.Add(30 * time.Minute)
	res, err := store.SweepHardDeadline(ctx, "sess_dl_before", sweepTime)
	if err != nil {
		t.Fatalf("SweepHardDeadline: %v", err)
	}

	if !res.IsNoOp() {
		t.Errorf("expected IsNoOp == true before deadline, got false")
	}
	if res.InterruptedCount != 0 {
		t.Errorf("expected 0 interrupted roles, got %d", res.InterruptedCount)
	}
	if len(res.InFlightRoleIDs) != 1 || res.InFlightRoleIDs[0] != resReq.RoleRunID {
		t.Errorf("expected in-flight role exposed, got %v", res.InFlightRoleIDs)
	}

	// Verify pending roles remain pending in DB.
	st, err := store.ReadStatus(ctx, "sess_dl_before")
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	for _, r := range st.Roles {
		if r.Role == domain.RoleRequirements {
			if r.Status != domain.RoleInFlight {
				t.Errorf("req: expected in_flight, got %s", r.Status)
			}
		} else {
			if r.Status != domain.RolePending {
				t.Errorf("%s: expected pending, got %s", r.Role, r.Status)
			}
		}
	}
}

// 6. SweepHardDeadline at exactly hard_deadline_at interrupts pending roles with cause=deadline_cutoff.
func TestSweepPredicate_6_HardDeadline_ExactDeadlineInterrupts(t *testing.T) {
	t0 := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	policy := TimingPolicy{
		DispatchCutoff:      30 * time.Minute,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: 45 * time.Minute,
	}
	store, _ := setupPredicateReviewingSession(t, "sess_dl_exact", "proj_dl_exact", t0, policy)
	ctx := context.Background()

	exactDeadline := t0.Add(policy.SessionHardDeadline)
	res, err := store.SweepHardDeadline(ctx, "sess_dl_exact", exactDeadline)
	if err != nil {
		t.Fatalf("SweepHardDeadline: %v", err)
	}

	if res.IsNoOp() {
		t.Errorf("expected IsNoOp == false at exact deadline")
	}
	if res.InterruptedCount != 4 {
		t.Errorf("expected 4 interrupted roles, got %d", res.InterruptedCount)
	}

	st, err := store.ReadStatus(ctx, "sess_dl_exact")
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	for _, r := range st.Roles {
		if r.Status != domain.RoleInterrupted {
			t.Errorf("role %s: expected interrupted, got %s", r.Role, r.Status)
		}
		if r.Cause != domain.CauseDeadlineCutoff {
			t.Errorf("role %s: expected deadline_cutoff, got %s", r.Role, r.Cause)
		}
		if r.CompletedAt == nil || !r.CompletedAt.Equal(exactDeadline) {
			t.Errorf("role %s: expected completed_at %v, got %v", r.Role, exactDeadline, r.CompletedAt)
		}
	}
}

// 7. SweepHardDeadline after hard_deadline_at interrupts pending roles and exposes in-flight IDs in canonical order.
func TestSweepPredicate_7_HardDeadline_AfterDeadlineExposesInFlightCanonical(t *testing.T) {
	t0 := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	policy := TimingPolicy{
		DispatchCutoff:      30 * time.Minute,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: 45 * time.Minute,
	}
	store, _ := setupPredicateReviewingSession(t, "sess_dl_after", "proj_dl_after", t0, policy)
	ctx := context.Background()

	// Reserve 2 roles in-flight (Requirements, Architecture).
	resReq, err := store.ReservePendingRoleWithNow(ctx, "sess_dl_after", t0.Add(1*time.Minute))
	if err != nil || !resReq.Reserved {
		t.Fatalf("reserve req: %v", err)
	}
	resArch, err := store.ReservePendingRoleWithNow(ctx, "sess_dl_after", t0.Add(2*time.Minute))
	if err != nil || !resArch.Reserved {
		t.Fatalf("reserve arch: %v", err)
	}

	// Sweep at 50m (> 45m).
	sweepTime := t0.Add(50 * time.Minute)
	res, err := store.SweepHardDeadline(ctx, "sess_dl_after", sweepTime)
	if err != nil {
		t.Fatalf("SweepHardDeadline: %v", err)
	}

	if res.InterruptedCount != 2 {
		t.Errorf("expected 2 interrupted (qa, security), got %d", res.InterruptedCount)
	}
	if len(res.InFlightRoleIDs) != 2 {
		t.Fatalf("expected 2 in-flight role IDs, got %d", len(res.InFlightRoleIDs))
	}
	if res.InFlightRoleIDs[0] != resReq.RoleRunID || res.InFlightRoleIDs[1] != resArch.RoleRunID {
		t.Errorf("expected canonical order [%s, %s], got %v", resReq.RoleRunID, resArch.RoleRunID, res.InFlightRoleIDs)
	}
	if len(res.InFlightRoles) != 2 || res.InFlightRoles[0].Role != domain.RoleRequirements || res.InFlightRoles[1].Role != domain.RoleArchitecture {
		t.Errorf("expected canonical in-flight roles [requirements, architecture], got %v", res.InFlightRoles)
	}
}

// 8. SweepHardDeadline does not rewrite in-flight roles; they remain in_flight and publishable via P9 thereafter.
func TestSweepPredicate_8_HardDeadline_InFlightPublishableViaP9(t *testing.T) {
	t0 := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	policy := TimingPolicy{
		DispatchCutoff:      30 * time.Minute,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: 45 * time.Minute,
	}
	store, _ := setupPredicateReviewingSession(t, "sess_dl_p9", "proj_dl_p9", t0, policy)
	ctx := context.Background()

	resReq, err := store.ReservePendingRoleWithNow(ctx, "sess_dl_p9", t0.Add(1*time.Minute))
	if err != nil || !resReq.Reserved {
		t.Fatalf("reserve req: %v", err)
	}

	// Sweep past hard deadline.
	sweepTime := t0.Add(50 * time.Minute)
	res, err := store.SweepHardDeadline(ctx, "sess_dl_p9", sweepTime)
	if err != nil {
		t.Fatalf("SweepHardDeadline: %v", err)
	}
	if res.InterruptedCount != 3 {
		t.Errorf("expected 3 interrupted, got %d", res.InterruptedCount)
	}

	// Verify requirements is STILL in_flight in DB.
	st, err := store.ReadStatus(ctx, "sess_dl_p9")
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	for _, r := range st.Roles {
		if r.Role == domain.RoleRequirements && r.Status != domain.RoleInFlight {
			t.Errorf("expected requirements to remain in_flight, got %s", r.Status)
		}
	}

	// Publish timeout failure via standard P9.
	pubRes, err := store.PublishRoleFailure(ctx, PublishFailureParams{
		SessionID:     "sess_dl_p9",
		RoleRunID:     resReq.RoleRunID,
		Role:          resReq.Role,
		ErrorCategory: domain.ErrTimeout,
		ErrorMessage:  "hard deadline exceeded",
		CallCount:     1,
		CompletedAt:   sweepTime.Add(1 * time.Minute),
	})
	if err != nil {
		t.Fatalf("PublishRoleFailure: %v", err)
	}
	if pubRes.Status != domain.RoleFailed || pubRes.ErrorCategory != domain.ErrTimeout {
		t.Errorf("expected failed/timeout, got %s/%s", pubRes.Status, pubRes.ErrorCategory)
	}
}

// 9. Both sweeps remain NoOp on queued and terminal sessions.
func TestSweepPredicate_9_BothSweepsNoOpOnQueuedAndTerminal(t *testing.T) {
	t0 := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	ctx := context.Background()

	origClock := clock
	clock = func() time.Time { return t0 }
	t.Cleanup(func() { clock = origClock })

	// --- Queued session ---
	storeQ, _ := openGuardTestStore(t)
	snapQ := createTestFrozenSnapshot(t, "snap_queued")
	_, err := storeQ.Submit(ctx, SubmitParams{
		SessionID:      "sess_pred_queued",
		ProjectID:      "proj_pred_queued",
		IdempotencyKey: "key_pred_queued",
		Title:          "Queued Session",
		Content:        "Spec content",
		Snapshot:       snapQ,
		CreatedAt:      t0,
	})
	if err != nil {
		t.Fatalf("submit queued: %v", err)
	}

	// SweepCancellation on queued must be NoOp.
	cResQ, err := storeQ.SweepCancellation(ctx, "sess_pred_queued")
	if err != nil {
		t.Fatalf("SweepCancellation queued: %v", err)
	}
	if !cResQ.IsNoOp() || cResQ.InterruptedCount != 0 {
		t.Errorf("expected cancellation on queued to be NoOp, got %+v", cResQ)
	}

	// SweepHardDeadline on queued must be NoOp.
	dResQ, err := storeQ.SweepHardDeadline(ctx, "sess_pred_queued", t0.Add(100*time.Hour))
	if err != nil {
		t.Fatalf("SweepHardDeadline queued: %v", err)
	}
	if !dResQ.IsNoOp() || dResQ.InterruptedCount != 0 {
		t.Errorf("expected hard deadline on queued to be NoOp, got %+v", dResQ)
	}

	// --- Terminal session ---
	storeT, _ := openGuardTestStore(t)
	policy := TimingPolicy{
		DispatchCutoff:      30 * time.Minute,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: 45 * time.Minute,
	}
	snapT := createTestFrozenSnapshot(t, "snap_term")
	_, err = storeT.Submit(ctx, SubmitParams{
		SessionID:      "sess_pred_term",
		ProjectID:      "proj_pred_term",
		IdempotencyKey: "key_pred_term",
		Title:          "Terminal Session",
		Content:        "Spec content",
		Snapshot:       snapT,
		CreatedAt:      t0,
	})
	if err != nil {
		t.Fatalf("submit term: %v", err)
	}
	claimResT, err := storeT.ClaimSessionWithNow(ctx, policy, t0)
	if err != nil || !claimResT.Claimed {
		t.Fatalf("claim term: %v", err)
	}

	// Interrupt all 4 roles via RequestCancellation and SweepCancellation.
	_, err = storeT.RequestCancellation(ctx, "sess_pred_term")
	if err != nil {
		t.Fatalf("cancel term: %v", err)
	}
	_, err = storeT.SweepCancellationWithNow(ctx, "sess_pred_term", t0.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("sweep cancel term: %v", err)
	}

	// Compose session to terminal.
	compRes, err := storeT.ComposeSessionWithParams(ctx, ComposeParams{
		SessionID: "sess_pred_term",
		Now:       t0.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("compose term: %v", err)
	}
	if !compRes.Composed || compRes.Status != domain.SessionPartial {
		t.Fatalf("expected composed partial session, got %+v", compRes)
	}

	// SweepCancellation on terminal session must be NoOp.
	cResT, err := storeT.SweepCancellation(ctx, "sess_pred_term")
	if err != nil {
		t.Fatalf("SweepCancellation terminal: %v", err)
	}
	if !cResT.IsNoOp() || cResT.InterruptedCount != 0 {
		t.Errorf("expected cancellation on terminal to be NoOp, got %+v", cResT)
	}

	// SweepHardDeadline on terminal session must be NoOp.
	dResT, err := storeT.SweepHardDeadline(ctx, "sess_pred_term", t0.Add(100*time.Hour))
	if err != nil {
		t.Fatalf("SweepHardDeadline terminal: %v", err)
	}
	if !dResT.IsNoOp() || dResT.InterruptedCount != 0 {
		t.Errorf("expected hard deadline on terminal to be NoOp, got %+v", dResT)
	}
}

// 10. Existing whole-second and subsecond timestamps remain correctly ordered (50ms regression still passes).
func TestSweepPredicate_10_SubsecondTimestampsRemainOrdered(t *testing.T) {
	t0 := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	policy := TimingPolicy{
		DispatchCutoff:      30 * time.Minute,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: 45 * time.Minute,
	}
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	tCreated := t0
	tClaimed := tCreated.Add(50 * time.Millisecond)
	tStarted := tClaimed.Add(50 * time.Millisecond)
	tCompleted := tStarted.Add(50 * time.Millisecond)
	tTerminal := tCompleted.Add(50 * time.Millisecond)

	snap := createTestFrozenSnapshot(t, "snap_pred_50ms")
	_, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_pred_50ms",
		ProjectID:      "proj_pred_50ms",
		IdempotencyKey: "key_pred_50ms",
		Title:          "50ms Ordering",
		Content:        "content",
		Snapshot:       snap,
		CreatedAt:      tCreated,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	claimRes, err := store.ClaimSessionWithNow(ctx, policy, tClaimed)
	if err != nil || !claimRes.Claimed {
		t.Fatalf("claim: %v", err)
	}

	resReq, err := store.ReservePendingRoleWithNow(ctx, "sess_pred_50ms", tStarted)
	if err != nil || !resReq.Reserved {
		t.Fatalf("reserve: %v", err)
	}

	_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
		SessionID:   "sess_pred_50ms",
		RoleRunID:   resReq.RoleRunID,
		Role:        resReq.Role,
		Findings:    nil,
		CallCount:   1,
		CompletedAt: tCompleted,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Cancel remaining and sweep.
	_, err = store.RequestCancellation(ctx, "sess_pred_50ms")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	_, err = store.SweepCancellationWithNow(ctx, "sess_pred_50ms", tCompleted)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	compRes, err := store.ComposeSessionWithParams(ctx, ComposeParams{
		SessionID: "sess_pred_50ms",
		Now:       tTerminal,
	})
	if err != nil || !compRes.Composed {
		t.Fatalf("compose: %v", err)
	}

	// Verify chronological ordering in SQLite TEXT comparisons.
	var orderValid int
	err = writer.QueryRowContext(ctx, `
		SELECT 1 FROM sessions
		WHERE id = 'sess_pred_50ms'
		  AND created_at < claimed_at
		  AND claimed_at < terminal_at;
	`).Scan(&orderValid)
	if err != nil || orderValid != 1 {
		t.Errorf("expected session timestamps to sort chronologically, err: %v", err)
	}
}
