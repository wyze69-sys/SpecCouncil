package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
)

// 1. Timing Policy Validation
func TestTimingPolicy_Validation(t *testing.T) {
	t.Run("valid policy with exact sum", func(t *testing.T) {
		p := TimingPolicy{
			DispatchCutoff:      30 * time.Minute,
			CallTimeout:         10 * time.Minute,
			SessionHardDeadline: 40 * time.Minute,
		}
		if err := p.Validate(); err != nil {
			t.Fatalf("expected valid policy, got: %v", err)
		}
	})

	t.Run("valid policy with deadline greater than sum", func(t *testing.T) {
		p := TimingPolicy{
			DispatchCutoff:      30 * time.Minute,
			CallTimeout:         10 * time.Minute,
			SessionHardDeadline: 60 * time.Minute,
		}
		if err := p.Validate(); err != nil {
			t.Fatalf("expected valid policy, got: %v", err)
		}
	})

	t.Run("zero dispatch cutoff rejected", func(t *testing.T) {
		p := TimingPolicy{
			DispatchCutoff:      0,
			CallTimeout:         10 * time.Minute,
			SessionHardDeadline: 60 * time.Minute,
		}
		err := p.Validate()
		if err == nil || !errors.Is(err, ErrInvalidTimingPolicy) {
			t.Fatalf("expected ErrInvalidTimingPolicy, got: %v", err)
		}
	})

	t.Run("negative dispatch cutoff rejected", func(t *testing.T) {
		p := TimingPolicy{
			DispatchCutoff:      -1 * time.Second,
			CallTimeout:         10 * time.Minute,
			SessionHardDeadline: 60 * time.Minute,
		}
		err := p.Validate()
		if err == nil || !errors.Is(err, ErrInvalidTimingPolicy) {
			t.Fatalf("expected ErrInvalidTimingPolicy, got: %v", err)
		}
	})

	t.Run("zero call timeout rejected", func(t *testing.T) {
		p := TimingPolicy{
			DispatchCutoff:      10 * time.Minute,
			CallTimeout:         0,
			SessionHardDeadline: 60 * time.Minute,
		}
		err := p.Validate()
		if err == nil || !errors.Is(err, ErrInvalidTimingPolicy) {
			t.Fatalf("expected ErrInvalidTimingPolicy, got: %v", err)
		}
	})

	t.Run("negative call timeout rejected", func(t *testing.T) {
		p := TimingPolicy{
			DispatchCutoff:      10 * time.Minute,
			CallTimeout:         -5 * time.Second,
			SessionHardDeadline: 60 * time.Minute,
		}
		err := p.Validate()
		if err == nil || !errors.Is(err, ErrInvalidTimingPolicy) {
			t.Fatalf("expected ErrInvalidTimingPolicy, got: %v", err)
		}
	})

	t.Run("zero session hard deadline rejected", func(t *testing.T) {
		p := TimingPolicy{
			DispatchCutoff:      10 * time.Minute,
			CallTimeout:         5 * time.Minute,
			SessionHardDeadline: 0,
		}
		err := p.Validate()
		if err == nil || !errors.Is(err, ErrInvalidTimingPolicy) {
			t.Fatalf("expected ErrInvalidTimingPolicy, got: %v", err)
		}
	})

	t.Run("negative session hard deadline rejected", func(t *testing.T) {
		p := TimingPolicy{
			DispatchCutoff:      10 * time.Minute,
			CallTimeout:         5 * time.Minute,
			SessionHardDeadline: -10 * time.Minute,
		}
		err := p.Validate()
		if err == nil || !errors.Is(err, ErrInvalidTimingPolicy) {
			t.Fatalf("expected ErrInvalidTimingPolicy, got: %v", err)
		}
	})

	t.Run("impossible relationship deadline less than cutoff plus timeout", func(t *testing.T) {
		p := TimingPolicy{
			DispatchCutoff:      30 * time.Minute,
			CallTimeout:         15 * time.Minute,
			SessionHardDeadline: 40 * time.Minute, // 40 < 30 + 15 = 45
		}
		err := p.Validate()
		if err == nil || !errors.Is(err, ErrInvalidTimingPolicy) {
			t.Fatalf("expected ErrInvalidTimingPolicy, got: %v", err)
		}
	})

	t.Run("duration arithmetic overflow rejected", func(t *testing.T) {
		p := TimingPolicy{
			DispatchCutoff:      time.Duration(math.MaxInt64 - 100),
			CallTimeout:         200 * time.Nanosecond,
			SessionHardDeadline: time.Duration(math.MaxInt64),
		}
		err := p.Validate()
		if err == nil || !errors.Is(err, ErrInvalidTimingPolicy) {
			t.Fatalf("expected ErrInvalidTimingPolicy for duration overflow, got: %v", err)
		}
	})

	t.Run("max int64 dispatch cutoff with positive call timeout rejected", func(t *testing.T) {
		p := TimingPolicy{
			DispatchCutoff:      time.Duration(math.MaxInt64),
			CallTimeout:         1 * time.Nanosecond,
			SessionHardDeadline: time.Duration(math.MaxInt64),
		}
		err := p.Validate()
		if err == nil || !errors.Is(err, ErrInvalidTimingPolicy) {
			t.Fatalf("expected ErrInvalidTimingPolicy for duration overflow, got: %v", err)
		}
	})
}

// 2. One Row Claim and Atomic Timing Fields
func TestClaim_OneRowClaim_AndAtomicTimingFields(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	t0 := time.Date(2026, 9, 19, 14, 0, 0, 500000000, time.UTC)
	origClock := clock
	clock = func() time.Time { return t0 }
	defer func() { clock = origClock }()

	snap := createTestFrozenSnapshot(t, "snap_claim_1")
	// Submit 2 sessions
	_, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_claim_1",
		ProjectID:      "proj_claim",
		IdempotencyKey: "key_1",
		Title:          "Session One",
		Content:        "Spec content 1",
		Snapshot:       snap,
		CreatedAt:      t0.Add(-10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Submit sess_claim_1: %v", err)
	}

	_, err = store.Submit(ctx, SubmitParams{
		SessionID:      "sess_claim_2",
		ProjectID:      "proj_claim",
		IdempotencyKey: "key_2",
		Title:          "Session Two",
		Content:        "Spec content 2",
		Snapshot:       snap,
		CreatedAt:      t0.Add(-5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Submit sess_claim_2: %v", err)
	}

	policy := TimingPolicy{
		DispatchCutoff:      30 * time.Minute,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: 60 * time.Minute,
	}

	// Claim oldest session
	claimTime := t0
	clock = func() time.Time { return claimTime }

	res, err := store.ClaimSession(ctx, policy)
	if err != nil {
		t.Fatalf("ClaimSession failed: %v", err)
	}
	if !res.Claimed {
		t.Fatalf("expected Claimed == true, got false (reason: %s)", res.NoWorkReason)
	}
	if res.SessionID != "sess_claim_1" {
		t.Errorf("expected claimed SessionID = sess_claim_1, got %q", res.SessionID)
	}
	if res.Status != domain.SessionReviewing {
		t.Errorf("expected status reviewing, got %s", res.Status)
	}
	if !res.ClaimedAt.Equal(claimTime) {
		t.Errorf("ClaimedAt = %v, want %v", res.ClaimedAt, claimTime)
	}
	wantCutoff := claimTime.Add(policy.DispatchCutoff)
	if !res.DispatchCutoffAt.Equal(wantCutoff) {
		t.Errorf("DispatchCutoffAt = %v, want %v", res.DispatchCutoffAt, wantCutoff)
	}
	wantDeadline := claimTime.Add(policy.SessionHardDeadline)
	if !res.HardDeadlineAt.Equal(wantDeadline) {
		t.Errorf("HardDeadlineAt = %v, want %v", res.HardDeadlineAt, wantDeadline)
	}

	// Verify database state for claimed session
	var (
		dbStatus       string
		claimedAtStr   string
		dispatchCutoff string
		hardDeadline   string
		complCount     int
		incomplCount   int
		termReason     sql.NullString
		termAt         sql.NullString
	)
	err = writer.QueryRowContext(ctx, `
		SELECT status, claimed_at, dispatch_cutoff_at, hard_deadline_at,
		       completed_role_count, incomplete_role_count, terminal_reason, terminal_at
		FROM sessions WHERE id = 'sess_claim_1';
	`).Scan(&dbStatus, &claimedAtStr, &dispatchCutoff, &hardDeadline, &complCount, &incomplCount, &termReason, &termAt)
	if err != nil {
		t.Fatalf("query sess_claim_1: %v", err)
	}

	if dbStatus != "reviewing" {
		t.Errorf("dbStatus = %q, want reviewing", dbStatus)
	}
	if claimedAtStr != formatUTCTimestamp(claimTime) {
		t.Errorf("claimed_at = %q, want %q", claimedAtStr, formatUTCTimestamp(claimTime))
	}
	if dispatchCutoff != formatUTCTimestamp(wantCutoff) {
		t.Errorf("dispatch_cutoff_at = %q, want %q", dispatchCutoff, formatUTCTimestamp(wantCutoff))
	}
	if hardDeadline != formatUTCTimestamp(wantDeadline) {
		t.Errorf("hard_deadline_at = %q, want %q", hardDeadline, formatUTCTimestamp(wantDeadline))
	}
	if complCount != 0 || incomplCount != 4 {
		t.Errorf("role counts = (%d, %d), want (0, 4)", complCount, incomplCount)
	}
	if termReason.Valid || termAt.Valid {
		t.Errorf("expected NULL terminal fields, got reason=%v, termAt=%v", termReason, termAt)
	}

	// Verify sess_claim_2 is UNTOUCHED and still queued
	var s2Status string
	var s2ClaimedAt sql.NullString
	err = writer.QueryRowContext(ctx, "SELECT status, claimed_at FROM sessions WHERE id = 'sess_claim_2';").Scan(&s2Status, &s2ClaimedAt)
	if err != nil {
		t.Fatalf("query sess_claim_2: %v", err)
	}
	if s2Status != "queued" || s2ClaimedAt.Valid {
		t.Errorf("sess_claim_2 mutated: status=%q, claimed_at=%v", s2Status, s2ClaimedAt)
	}

	// Verify all roles of sess_claim_1 remain pending with no in_flight or call counts
	roleRows, err := writer.QueryContext(ctx, "SELECT role, status, call_count, started_at FROM role_runs WHERE session_id = 'sess_claim_1';")
	if err != nil {
		t.Fatalf("query role_runs: %v", err)
	}
	defer roleRows.Close()

	roleCount := 0
	for roleRows.Next() {
		roleCount++
		var rRole, rStatus string
		var rCallCount int
		var rStartedAt sql.NullString
		if err := roleRows.Scan(&rRole, &rStatus, &rCallCount, &rStartedAt); err != nil {
			t.Fatalf("scan role_run: %v", err)
		}
		if rStatus != "pending" {
			t.Errorf("role %s has status %q, want pending", rRole, rStatus)
		}
		if rCallCount != 0 {
			t.Errorf("role %s has call_count %d, want 0", rRole, rCallCount)
		}
		if rStartedAt.Valid {
			t.Errorf("role %s has started_at %v, want NULL", rRole, rStartedAt)
		}
	}
	if roleCount != 4 {
		t.Errorf("expected 4 roles, got %d", roleCount)
	}

	// Verify ReadStatus works and reflects the claimed state
	readSt, err := store.ReadStatus(ctx, "sess_claim_1")
	if err != nil {
		t.Fatalf("ReadStatus sess_claim_1: %v", err)
	}
	if readSt.Status != domain.SessionReviewing {
		t.Errorf("ReadStatus status = %s, want reviewing", readSt.Status)
	}
	if readSt.ClaimedAt == nil || !readSt.ClaimedAt.Equal(claimTime) {
		t.Errorf("ReadStatus ClaimedAt = %v, want %v", readSt.ClaimedAt, claimTime)
	}
	if readSt.DispatchCutoffAt == nil || !readSt.DispatchCutoffAt.Equal(wantCutoff) {
		t.Errorf("ReadStatus DispatchCutoffAt = %v, want %v", readSt.DispatchCutoffAt, wantCutoff)
	}
	if readSt.HardDeadlineAt == nil || !readSt.HardDeadlineAt.Equal(wantDeadline) {
		t.Errorf("ReadStatus HardDeadlineAt = %v, want %v", readSt.HardDeadlineAt, wantDeadline)
	}
}

// 3. FIFO Ordering
func TestClaim_FIFOOrdering(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	t0 := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	origClock := clock
	clock = func() time.Time { return t0 }
	defer func() { clock = origClock }()

	snap := createTestFrozenSnapshot(t, "snap_fifo")

	// Submit 3 sessions with distinct CreatedAt: S1 < S2 < S3
	for i, id := range []string{"sess_fifo_1", "sess_fifo_2", "sess_fifo_3"} {
		_, err := store.Submit(ctx, SubmitParams{
			SessionID:      id,
			ProjectID:      "proj_fifo",
			IdempotencyKey: fmt.Sprintf("key_%d", i+1),
			Title:          fmt.Sprintf("Title %d", i+1),
			Content:        "Spec content",
			Snapshot:       snap,
			CreatedAt:      t0.Add(time.Duration(i) * time.Minute),
		})
		if err != nil {
			t.Fatalf("Submit %s: %v", id, err)
		}
	}

	policy := TimingPolicy{
		DispatchCutoff:      30 * time.Minute,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: 60 * time.Minute,
	}

	// First claim: should claim sess_fifo_1
	c1Time := t0.Add(10 * time.Minute)
	clock = func() time.Time { return c1Time }
	res1, err := store.ClaimSession(ctx, policy)
	if err != nil || !res1.Claimed {
		t.Fatalf("Claim 1 failed: res=%v, err=%v", res1, err)
	}
	if res1.SessionID != "sess_fifo_1" {
		t.Fatalf("Claim 1 picked %q, want sess_fifo_1", res1.SessionID)
	}

	// Transition sess_fifo_1 to complete so system has 0 reviewing sessions
	termTime := c1Time.Add(5 * time.Minute)
	_, err = writer.ExecContext(ctx, `
		UPDATE sessions
		SET status = 'complete',
			completed_role_count = 4,
			incomplete_role_count = 0,
			terminal_reason = 'all_roles_complete',
			terminal_at = ?
		WHERE id = 'sess_fifo_1';
	`, formatUTCTimestamp(termTime))
	if err != nil {
		t.Fatalf("transition sess_fifo_1 to complete: %v", err)
	}

	// Second claim: should claim sess_fifo_2
	c2Time := c1Time.Add(10 * time.Minute)
	clock = func() time.Time { return c2Time }
	res2, err := store.ClaimSession(ctx, policy)
	if err != nil || !res2.Claimed {
		t.Fatalf("Claim 2 failed: res=%v, err=%v", res2, err)
	}
	if res2.SessionID != "sess_fifo_2" {
		t.Fatalf("Claim 2 picked %q, want sess_fifo_2", res2.SessionID)
	}

	// Transition sess_fifo_2 to complete
	_, err = writer.ExecContext(ctx, `
		UPDATE sessions
		SET status = 'complete',
			completed_role_count = 4,
			incomplete_role_count = 0,
			terminal_reason = 'all_roles_complete',
			terminal_at = ?
		WHERE id = 'sess_fifo_2';
	`, formatUTCTimestamp(c2Time.Add(5*time.Minute)))
	if err != nil {
		t.Fatalf("transition sess_fifo_2 to complete: %v", err)
	}

	// Third claim: should claim sess_fifo_3
	c3Time := c2Time.Add(10 * time.Minute)
	clock = func() time.Time { return c3Time }
	res3, err := store.ClaimSession(ctx, policy)
	if err != nil || !res3.Claimed {
		t.Fatalf("Claim 3 failed: res=%v, err=%v", res3, err)
	}
	if res3.SessionID != "sess_fifo_3" {
		t.Fatalf("Claim 3 picked %q, want sess_fifo_3", res3.SessionID)
	}
}

// 4. FIFO Tie-Breaking by ID ASC
func TestClaim_FIFOTieBreaking(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	t0 := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	snap := createTestFrozenSnapshot(t, "snap_tie")

	// Submit two sessions with identical CreatedAt
	// ID "sess_tie_beta" submitted first, "sess_tie_alpha" submitted second.
	// Since "sess_tie_alpha" < "sess_tie_beta", alpha MUST be claimed first.
	_, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_tie_beta",
		ProjectID:      "proj_tie",
		IdempotencyKey: "key_b",
		Title:          "Beta",
		Content:        "Content",
		Snapshot:       snap,
		CreatedAt:      t0,
	})
	if err != nil {
		t.Fatalf("Submit beta: %v", err)
	}

	_, err = store.Submit(ctx, SubmitParams{
		SessionID:      "sess_tie_alpha",
		ProjectID:      "proj_tie",
		IdempotencyKey: "key_a",
		Title:          "Alpha",
		Content:        "Content",
		Snapshot:       snap,
		CreatedAt:      t0,
	})
	if err != nil {
		t.Fatalf("Submit alpha: %v", err)
	}

	policy := TimingPolicy{
		DispatchCutoff:      30 * time.Minute,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: 60 * time.Minute,
	}

	origClock := clock
	clock = func() time.Time { return t0.Add(5 * time.Minute) }
	defer func() { clock = origClock }()

	// First claim: must pick sess_tie_alpha due to id ASC tie-breaking
	res1, err := store.ClaimSession(ctx, policy)
	if err != nil || !res1.Claimed {
		t.Fatalf("Claim 1 failed: res=%v, err=%v", res1, err)
	}
	if res1.SessionID != "sess_tie_alpha" {
		t.Errorf("expected tie-breaker to pick sess_tie_alpha, got %q", res1.SessionID)
	}

	// Transition sess_tie_alpha to complete
	_, err = writer.ExecContext(ctx, `
		UPDATE sessions
		SET status = 'complete',
			completed_role_count = 4,
			incomplete_role_count = 0,
			terminal_reason = 'all_roles_complete',
			terminal_at = ?
		WHERE id = 'sess_tie_alpha';
	`, formatUTCTimestamp(t0.Add(6*time.Minute)))
	if err != nil {
		t.Fatalf("transition alpha: %v", err)
	}

	// Second claim: must pick sess_tie_beta
	res2, err := store.ClaimSession(ctx, policy)
	if err != nil || !res2.Claimed {
		t.Fatalf("Claim 2 failed: res=%v, err=%v", res2, err)
	}
	if res2.SessionID != "sess_tie_beta" {
		t.Errorf("expected second claim to pick sess_tie_beta, got %q", res2.SessionID)
	}
}

// 5. No-Work when No Queued Sessions or Only Terminal Sessions
func TestClaim_NoWork_NoQueuedSession(t *testing.T) {
	store, _ := openGuardTestStore(t)
	ctx := context.Background()

	policy := TimingPolicy{
		DispatchCutoff:      15 * time.Minute,
		CallTimeout:         5 * time.Minute,
		SessionHardDeadline: 30 * time.Minute,
	}

	t.Run("empty database returns no-work", func(t *testing.T) {
		res, err := store.ClaimSession(ctx, policy)
		if err != nil {
			t.Fatalf("expected nil err, got: %v", err)
		}
		if res.Claimed {
			t.Fatalf("expected Claimed == false, got true")
		}
		if res.NoWorkReason != NoWorkNoQueuedSession {
			t.Errorf("NoWorkReason = %q, want %q", res.NoWorkReason, NoWorkNoQueuedSession)
		}
		if !res.IsNoWork() || res.HasWork() {
			t.Errorf("IsNoWork() = %v, HasWork() = %v", res.IsNoWork(), res.HasWork())
		}
	})

	t.Run("database with only terminal sessions returns no-work", func(t *testing.T) {
		snap := createTestFrozenSnapshot(t, "snap_term_only")
		// Submit and finalize session
		subRes, err := store.Submit(ctx, SubmitParams{
			SessionID:      "sess_term_only",
			ProjectID:      "proj_term",
			IdempotencyKey: "key_term",
			Title:          "Term Only",
			Content:        "Content",
			Snapshot:       snap,
			CreatedAt:      time.Now().UTC().Add(-10 * time.Minute),
		})
		if err != nil {
			t.Fatalf("submit: %v", err)
		}

		// Claim then complete it
		cRes, err := store.ClaimSession(ctx, policy)
		if err != nil || !cRes.Claimed {
			t.Fatalf("claim: %v", err)
		}
		writer, _ := store.writerDB()
		_, err = writer.ExecContext(ctx, `
			UPDATE sessions
			SET status = 'complete',
				completed_role_count = 4,
				incomplete_role_count = 0,
				terminal_reason = 'all_roles_complete',
				terminal_at = ?
			WHERE id = ?;
		`, formatUTCTimestamp(time.Now().UTC()), subRes.SessionID)
		if err != nil {
			t.Fatalf("complete session: %v", err)
		}

		// Claim again: no queued sessions exist
		res, err := store.ClaimSession(ctx, policy)
		if err != nil {
			t.Fatalf("expected nil err, got: %v", err)
		}
		if res.Claimed {
			t.Fatalf("expected Claimed == false, got true")
		}
		if res.NoWorkReason != NoWorkNoQueuedSession {
			t.Errorf("NoWorkReason = %q, want %q", res.NoWorkReason, NoWorkNoQueuedSession)
		}
	})
}

// 6. No-Work when Another Session is Already Reviewing (Single Active Session)
func TestClaim_NoWork_ActiveReviewingSession(t *testing.T) {
	store, _ := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_active_rev")
	// Submit 2 sessions
	_, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_rev_active_1",
		ProjectID:      "proj_rev",
		IdempotencyKey: "key_rev_1",
		Title:          "Session 1",
		Content:        "Content",
		Snapshot:       snap,
		CreatedAt:      time.Now().UTC().Add(-5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("submit 1: %v", err)
	}

	_, err = store.Submit(ctx, SubmitParams{
		SessionID:      "sess_rev_active_2",
		ProjectID:      "proj_rev",
		IdempotencyKey: "key_rev_2",
		Title:          "Session 2",
		Content:        "Content",
		Snapshot:       snap,
		CreatedAt:      time.Now().UTC().Add(-4 * time.Minute),
	})
	if err != nil {
		t.Fatalf("submit 2: %v", err)
	}

	policy := TimingPolicy{
		DispatchCutoff:      20 * time.Minute,
		CallTimeout:         5 * time.Minute,
		SessionHardDeadline: 30 * time.Minute,
	}

	// First claim: claims sess_rev_active_1
	res1, err := store.ClaimSession(ctx, policy)
	if err != nil || !res1.Claimed {
		t.Fatalf("claim 1: %v", err)
	}
	if res1.SessionID != "sess_rev_active_1" {
		t.Fatalf("claimed %q, want sess_rev_active_1", res1.SessionID)
	}

	// Second claim while sess_rev_active_1 is still reviewing:
	// MUST NOT claim sess_rev_active_2, must return typed no-work result, NOT error.
	res2, err := store.ClaimSession(ctx, policy)
	if err != nil {
		t.Fatalf("expected nil err on second claim, got: %v", err)
	}
	if res2.Claimed {
		t.Fatalf("expected Claimed == false, but claimed session %q", res2.SessionID)
	}
	if res2.NoWorkReason != NoWorkActiveReviewing {
		t.Errorf("NoWorkReason = %q, want %q", res2.NoWorkReason, NoWorkActiveReviewing)
	}

	// Third claim: still returns no-work
	res3, err := store.ClaimSession(ctx, policy)
	if err != nil || res3.Claimed {
		t.Fatalf("expected no-work on third claim: res=%v, err=%v", res3, err)
	}

	// Verify sess_rev_active_2 is still queued
	st2, err := store.ReadStatus(ctx, "sess_rev_active_2")
	if err != nil {
		t.Fatalf("ReadStatus sess_rev_active_2: %v", err)
	}
	if st2.Status != domain.SessionQueued {
		t.Errorf("sess_rev_active_2 status = %s, want queued", st2.Status)
	}
}

// 7. Queued Session with cancel_requested = 1 is Claimed
func TestClaim_QueuedSession_WithCancelRequested(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_cancel_queued")
	_, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_cancel_q",
		ProjectID:      "proj_cq",
		IdempotencyKey: "key_cq",
		Title:          "Cancel Queued",
		Content:        "Content",
		Snapshot:       snap,
		CreatedAt:      time.Now().UTC().Add(-10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Simulate cancellation request on queued session (valid transition: cancel_requested 0 -> 1)
	_, err = writer.ExecContext(ctx, "UPDATE sessions SET cancel_requested = 1 WHERE id = 'sess_cancel_q';")
	if err != nil {
		t.Fatalf("update cancel_requested: %v", err)
	}

	policy := TimingPolicy{
		DispatchCutoff:      10 * time.Minute,
		CallTimeout:         5 * time.Minute,
		SessionHardDeadline: 20 * time.Minute,
	}

	res, err := store.ClaimSession(ctx, policy)
	if err != nil {
		t.Fatalf("ClaimSession failed: %v", err)
	}
	if !res.Claimed {
		t.Fatalf("expected Claimed == true, got false")
	}
	if res.SessionID != "sess_cancel_q" {
		t.Errorf("SessionID = %q, want sess_cancel_q", res.SessionID)
	}
	if !res.CancelRequested {
		t.Errorf("expected CancelRequested == true")
	}
	if res.Status != domain.SessionReviewing {
		t.Errorf("Status = %s, want reviewing", res.Status)
	}
}

// 8. Never Claim Malformed Sessions
func TestClaim_NeverClaimMalformedSessions(t *testing.T) {
	policy := TimingPolicy{
		DispatchCutoff:      10 * time.Minute,
		CallTimeout:         5 * time.Minute,
		SessionHardDeadline: 20 * time.Minute,
	}

	t.Run("session with created_at in the future rejected", func(t *testing.T) {
		store, writer := openGuardTestStore(t)
		ctx := context.Background()

		snap := createTestFrozenSnapshot(t, "snap_future")
		t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
		origClock := clock
		clock = func() time.Time { return t0 }
		defer func() { clock = origClock }()

		// Submit with CreatedAt in future relative to claim time
		_, err := store.Submit(ctx, SubmitParams{
			SessionID:      "sess_future",
			ProjectID:      "proj_f",
			IdempotencyKey: "key_f",
			Title:          "Future",
			Content:        "Content",
			Snapshot:       snap,
			CreatedAt:      t0.Add(5 * time.Minute), // future!
		})
		if err != nil {
			t.Fatalf("submit: %v", err)
		}

		// Try to claim at t0 (which is before CreatedAt)
		_, err = store.ClaimSession(ctx, policy)
		if err == nil || !errors.Is(err, ErrMalformedData) {
			t.Fatalf("expected ErrMalformedData for future created_at, got: %v", err)
		}

		// Ensure session is NOT claimed
		var status string
		_ = writer.QueryRowContext(ctx, "SELECT status FROM sessions WHERE id = 'sess_future';").Scan(&status)
		if status != "queued" {
			t.Errorf("status = %q, want queued", status)
		}
	})

	t.Run("session with non-pending role runs rejected", func(t *testing.T) {
		store, writer := openGuardTestStore(t)
		ctx := context.Background()

		snap := createTestFrozenSnapshot(t, "snap_nonpending_roles")
		t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
		origClock := clock
		clock = func() time.Time { return t0 }
		defer func() { clock = origClock }()

		_, err := store.Submit(ctx, SubmitParams{
			SessionID:      "sess_bad_roles",
			ProjectID:      "proj_br",
			IdempotencyKey: "key_br",
			Title:          "Bad Roles",
			Content:        "Content",
			Snapshot:       snap,
			CreatedAt:      t0.Add(-5 * time.Minute),
		})
		if err != nil {
			t.Fatalf("submit: %v", err)
		}

		// Transition one role run to in_flight directly in DB
		_, err = writer.ExecContext(ctx, `
			UPDATE role_runs
			SET status = 'in_flight',
				started_at = ?
			WHERE session_id = 'sess_bad_roles' AND role = 'requirements';
		`, formatUTCTimestamp(t0.Add(-4*time.Minute)))
		if err != nil {
			t.Fatalf("corrupt role: %v", err)
		}

		// Claim should detect malformed non-pending role on queued session
		_, err = store.ClaimSession(ctx, policy)
		if err == nil || !errors.Is(err, ErrMalformedData) {
			t.Fatalf("expected ErrMalformedData for non-pending role in queued session, got: %v", err)
		}
	})

	t.Run("session with missing role runs rejected", func(t *testing.T) {
		store, writer := openGuardTestStore(t)
		ctx := context.Background()

		snap := createTestFrozenSnapshot(t, "snap_missing_roles")
		t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
		origClock := clock
		clock = func() time.Time { return t0 }
		defer func() { clock = origClock }()

		_, err := store.Submit(ctx, SubmitParams{
			SessionID:      "sess_missing_roles",
			ProjectID:      "proj_mr",
			IdempotencyKey: "key_mr",
			Title:          "Missing Roles",
			Content:        "Content",
			Snapshot:       snap,
			CreatedAt:      t0.Add(-5 * time.Minute),
		})
		if err != nil {
			t.Fatalf("submit: %v", err)
		}

		// Delete one role run directly
		_, err = writer.ExecContext(ctx, "DELETE FROM role_runs WHERE session_id = 'sess_missing_roles' AND role = 'security';")
		if err != nil {
			t.Fatalf("delete role: %v", err)
		}

		_, err = store.ClaimSession(ctx, policy)
		if err == nil || !errors.Is(err, ErrMalformedData) {
			t.Fatalf("expected ErrMalformedData for missing role, got: %v", err)
		}
	})
}

// 9. Rollback on Injected Failure
func TestClaim_RollbackOnInjectedFailure(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	origClock := clock
	clock = func() time.Time { return t0 }
	defer func() { clock = origClock }()

	snap := createTestFrozenSnapshot(t, "snap_rollback")
	_, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_rollback",
		ProjectID:      "proj_rb",
		IdempotencyKey: "key_rb",
		Title:          "Rollback Session",
		Content:        "Content",
		Snapshot:       snap,
		CreatedAt:      t0.Add(-5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	policy := TimingPolicy{
		DispatchCutoff:      10 * time.Minute,
		CallTimeout:         5 * time.Minute,
		SessionHardDeadline: 20 * time.Minute,
	}

	// Inject error before commit
	injectedErr := errors.New("simulated failure before commit")
	claimBeforeCommitHook = func(ctx context.Context, conn *sql.Conn) error {
		return injectedErr
	}
	defer func() { claimBeforeCommitHook = nil }()

	_, err = store.ClaimSession(ctx, policy)
	if err == nil || !errors.Is(err, injectedErr) {
		t.Fatalf("expected injectedErr, got: %v", err)
	}

	// Verify database was completely rolled back: session remains queued with NULL timing fields
	var (
		status       string
		claimedAt    sql.NullString
		cutoffAt     sql.NullString
		hardDeadline sql.NullString
	)
	err = writer.QueryRowContext(ctx, `
		SELECT status, claimed_at, dispatch_cutoff_at, hard_deadline_at
		FROM sessions WHERE id = 'sess_rollback';
	`).Scan(&status, &claimedAt, &cutoffAt, &hardDeadline)
	if err != nil {
		t.Fatalf("query session: %v", err)
	}
	if status != "queued" {
		t.Errorf("status = %q, want queued after rollback", status)
	}
	if claimedAt.Valid || cutoffAt.Valid || hardDeadline.Valid {
		t.Errorf("timing fields were not rolled back: claimed=%v, cutoff=%v, deadline=%v",
			claimedAt, cutoffAt, hardDeadline)
	}

	// Clear hook and verify session can now be claimed successfully
	claimBeforeCommitHook = nil
	res, err := store.ClaimSession(ctx, policy)
	if err != nil || !res.Claimed {
		t.Fatalf("ClaimSession after clearing hook failed: res=%v, err=%v", res, err)
	}
	if res.SessionID != "sess_rollback" {
		t.Errorf("SessionID = %q, want sess_rollback", res.SessionID)
	}
}

// 10. Concurrent Claim Protection (Barrier-based race test yielding exactly one winner)
func TestClaim_Concurrency_SingleWinner(t *testing.T) {
	store, _ := openGuardTestStore(t)
	ctx := context.Background()

	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	origClock := clock
	clock = func() time.Time { return t0 }
	defer func() { clock = origClock }()

	snap := createTestFrozenSnapshot(t, "snap_conc_single")
	_, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_conc_1",
		ProjectID:      "proj_conc",
		IdempotencyKey: "key_conc",
		Title:          "Conc Session",
		Content:        "Content",
		Snapshot:       snap,
		CreatedAt:      t0.Add(-5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	policy := TimingPolicy{
		DispatchCutoff:      10 * time.Minute,
		CallTimeout:         5 * time.Minute,
		SessionHardDeadline: 20 * time.Minute,
	}

	const numClaimers = 10
	startBarrier := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(numClaimers)

	var (
		claimWinnerCount atomic.Int32
		noWorkCount      atomic.Int32
		errorCount       atomic.Int32
	)

	for i := 0; i < numClaimers; i++ {
		go func() {
			defer wg.Done()
			<-startBarrier

			res, err := store.ClaimSession(ctx, policy)
			if err != nil {
				errorCount.Add(1)
				return
			}
			if res.Claimed {
				claimWinnerCount.Add(1)
			} else {
				noWorkCount.Add(1)
			}
		}()
	}

	// Release all claimers simultaneously
	close(startBarrier)
	wg.Wait()

	if errs := errorCount.Load(); errs > 0 {
		t.Fatalf("%d claimers returned unexpected errors", errs)
	}
	if winners := claimWinnerCount.Load(); winners != 1 {
		t.Fatalf("expected exactly 1 claim winner, got %d", winners)
	}
	if noWork := noWorkCount.Load(); noWork != numClaimers-1 {
		t.Fatalf("expected %d no-work outcomes, got %d", numClaimers-1, noWork)
	}

	// Verify database has exactly 1 reviewing session
	writer, _ := store.writerDB()
	var reviewingCount int
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions WHERE status = 'reviewing';").Scan(&reviewingCount)
	if reviewingCount != 1 {
		t.Errorf("reviewingCount = %d, want 1", reviewingCount)
	}
}

// 11. Concurrent Claim with Multiple Queued Sessions
// Even when multiple sessions are queued, only 1 session is claimed because a reviewing
// session blocks all subsequent claims.
func TestClaim_Concurrency_MultipleQueuedSessions(t *testing.T) {
	store, _ := openGuardTestStore(t)
	ctx := context.Background()

	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	origClock := clock
	clock = func() time.Time { return t0 }
	defer func() { clock = origClock }()

	snap := createTestFrozenSnapshot(t, "snap_conc_multi")
	for i := 1; i <= 5; i++ {
		_, err := store.Submit(ctx, SubmitParams{
			SessionID:      fmt.Sprintf("sess_multi_%d", i),
			ProjectID:      "proj_multi",
			IdempotencyKey: fmt.Sprintf("key_multi_%d", i),
			Title:          fmt.Sprintf("Multi %d", i),
			Content:        "Content",
			Snapshot:       snap,
			CreatedAt:      t0.Add(-time.Duration(10-i) * time.Minute),
		})
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}

	policy := TimingPolicy{
		DispatchCutoff:      10 * time.Minute,
		CallTimeout:         5 * time.Minute,
		SessionHardDeadline: 20 * time.Minute,
	}

	const numClaimers = 10
	startBarrier := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(numClaimers)

	var (
		winners atomic.Int32
		noWork  atomic.Int32
		errs    atomic.Int32
	)

	for i := 0; i < numClaimers; i++ {
		go func() {
			defer wg.Done()
			<-startBarrier

			res, err := store.ClaimSession(ctx, policy)
			if err != nil {
				errs.Add(1)
				return
			}
			if res.Claimed {
				winners.Add(1)
			} else {
				noWork.Add(1)
			}
		}()
	}

	close(startBarrier)
	wg.Wait()

	if e := errs.Load(); e > 0 {
		t.Fatalf("%d claimers returned errors", e)
	}
	if w := winners.Load(); w != 1 {
		t.Fatalf("expected exactly 1 winner, got %d", w)
	}
	if nw := noWork.Load(); nw != numClaimers-1 {
		t.Fatalf("expected %d no-work results, got %d", numClaimers-1, nw)
	}

	// Verify sess_multi_1 (the oldest) won
	st1, err := store.ReadStatus(ctx, "sess_multi_1")
	if err != nil {
		t.Fatalf("ReadStatus sess_multi_1: %v", err)
	}
	if st1.Status != domain.SessionReviewing {
		t.Errorf("sess_multi_1 status = %s, want reviewing", st1.Status)
	}

	// All other 4 sessions must remain queued
	for i := 2; i <= 5; i++ {
		st, err := store.ReadStatus(ctx, fmt.Sprintf("sess_multi_%d", i))
		if err != nil {
			t.Fatalf("ReadStatus %d: %v", i, err)
		}
		if st.Status != domain.SessionQueued {
			t.Errorf("sess_multi_%d status = %s, want queued", i, st.Status)
		}
	}
}

// 12. Forbidden Scope Inspection
// Proves roles remain pending, no provider/worker/API code is introduced.
func TestClaim_ForbiddenScope(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_forbid")
	_, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_forbid",
		ProjectID:      "proj_forbid",
		IdempotencyKey: "key_forbid",
		Title:          "Forbid Test",
		Content:        "Content",
		Snapshot:       snap,
		CreatedAt:      time.Now().UTC().Add(-5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	policy := TimingPolicy{
		DispatchCutoff:      10 * time.Minute,
		CallTimeout:         5 * time.Minute,
		SessionHardDeadline: 20 * time.Minute,
	}

	res, err := store.ClaimSession(ctx, policy)
	if err != nil || !res.Claimed {
		t.Fatalf("claim: %v", err)
	}

	// Assert no role runs transitioned to in_flight or complete
	var inFlightCount int
	err = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM role_runs WHERE session_id = 'sess_forbid' AND status != 'pending';").Scan(&inFlightCount)
	if err != nil {
		t.Fatalf("query non-pending roles: %v", err)
	}
	if inFlightCount != 0 {
		t.Errorf("found %d non-pending roles after claim; roles must remain pending", inFlightCount)
	}

	// Assert no findings inserted
	var findingCount int
	err = writer.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM findings f
		JOIN role_runs rr ON f.role_run_id = rr.id
		WHERE rr.session_id = 'sess_forbid';
	`).Scan(&findingCount)
	if err != nil {
		t.Fatalf("query findings: %v", err)
	}
	if findingCount != 0 {
		t.Errorf("found %d findings; claim must not insert findings", findingCount)
	}
}

// 13. Guarded Update Zero Rows Conflict
func TestClaim_GuardedUpdateZeroRows(t *testing.T) {
	store, _ := openGuardTestStore(t)
	ctx := context.Background()

	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	origClock := clock
	clock = func() time.Time { return t0 }
	defer func() { clock = origClock }()

	snap := createTestFrozenSnapshot(t, "snap_zero_rows")
	_, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_zero_rows",
		ProjectID:      "proj_zr",
		IdempotencyKey: "key_zr",
		Title:          "Zero Rows",
		Content:        "Content",
		Snapshot:       snap,
		CreatedAt:      t0.Add(-5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	policy := TimingPolicy{
		DispatchCutoff:      10 * time.Minute,
		CallTimeout:         5 * time.Minute,
		SessionHardDeadline: 20 * time.Minute,
	}

	// Transition the session to 'reviewing' directly inside the hook so the guarded
	// UPDATE ... WHERE id = ? AND status = 'queued' matches 0 rows.
	claimBeforeUpdateHook = func(ctx context.Context, conn *sql.Conn, targetID string) error {
		tClaim := t0.Add(-1 * time.Minute)
		_, err := conn.ExecContext(ctx, `
			UPDATE sessions
			SET status = 'reviewing',
				claimed_at = ?,
				dispatch_cutoff_at = ?,
				hard_deadline_at = ?
			WHERE id = ? AND status = 'queued';
		`, formatUTCTimestamp(tClaim), formatUTCTimestamp(tClaim.Add(10*time.Minute)), formatUTCTimestamp(tClaim.Add(20*time.Minute)), targetID)
		return err
	}
	defer func() { claimBeforeUpdateHook = nil }()

	res, err := store.ClaimSession(ctx, policy)
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if res.Claimed {
		t.Fatalf("expected Claimed == false, got true")
	}
	if res.NoWorkReason != NoWorkGuardConflict {
		t.Errorf("NoWorkReason = %q, want %q", res.NoWorkReason, NoWorkGuardConflict)
	}
}
