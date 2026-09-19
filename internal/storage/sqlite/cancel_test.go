package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
)

// 1. Queued session cancellation: 0 -> 1 returns Effective: true.
func TestRequestCancellation_QueuedSession(t *testing.T) {
	store, _ := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_cancel_q")
	submitRes, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_cancel_queued",
		ProjectID:      "proj_cq",
		IdempotencyKey: "key_cq",
		Title:          "Cancel Queued Test",
		Content:        "Spec content",
		Snapshot:       snap,
	})
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}

	// Verify initial status has cancel_requested = false
	initStatus, err := store.ReadStatus(ctx, submitRes.SessionID)
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if initStatus.CancelRequested {
		t.Fatalf("expected initial CancelRequested == false")
	}
	if initStatus.Status != domain.SessionQueued {
		t.Fatalf("expected status queued, got %s", initStatus.Status)
	}

	// Request cancellation
	res, err := store.RequestCancellation(ctx, submitRes.SessionID)
	if err != nil {
		t.Fatalf("RequestCancellation failed: %v", err)
	}

	if !res.Effective {
		t.Errorf("expected Effective == true")
	}
	if res.AlreadyTerminal {
		t.Errorf("expected AlreadyTerminal == false")
	}
	if res.NoOp {
		t.Errorf("expected NoOp == false")
	}
	if !res.CancelRequested {
		t.Errorf("expected CancelRequested == true")
	}
	if res.Status != domain.SessionQueued {
		t.Errorf("expected Status == queued, got %s", res.Status)
	}
	if res.Session == nil {
		t.Fatalf("expected non-nil Session in result")
	}
	if !res.Session.CancelRequested {
		t.Errorf("expected Session.CancelRequested == true")
	}
	if res.Session.Status != domain.SessionQueued {
		t.Errorf("expected Session.Status == queued, got %s", res.Session.Status)
	}

	// Re-verify through read-only pool
	st, err := store.ReadStatus(ctx, submitRes.SessionID)
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if !st.CancelRequested {
		t.Errorf("expected persisted CancelRequested == true")
	}
	if st.Status != domain.SessionQueued {
		t.Errorf("expected persisted Status == queued, got %s", st.Status)
	}
}

// 2. Reviewing session cancellation: 0 -> 1 returns Effective: true.
func TestRequestCancellation_ReviewingSession(t *testing.T) {
	store, _ := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_cancel_rev")
	submitRes, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_cancel_rev",
		ProjectID:      "proj_cr",
		IdempotencyKey: "key_cr",
		Title:          "Cancel Reviewing Test",
		Content:        "Spec content",
		Snapshot:       snap,
	})
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}

	// Claim session so it transitions queued -> reviewing
	policy := TimingPolicy{
		DispatchCutoff:      10 * time.Minute,
		CallTimeout:         5 * time.Minute,
		SessionHardDeadline: 20 * time.Minute,
	}
	claimRes, err := store.ClaimSession(ctx, policy)
	if err != nil {
		t.Fatalf("ClaimSession failed: %v", err)
	}
	if !claimRes.Claimed || claimRes.Status != domain.SessionReviewing {
		t.Fatalf("expected session to be claimed into reviewing status")
	}

	// Request cancellation
	res, err := store.RequestCancellation(ctx, submitRes.SessionID)
	if err != nil {
		t.Fatalf("RequestCancellation failed: %v", err)
	}

	if !res.Effective {
		t.Errorf("expected Effective == true")
	}
	if res.AlreadyTerminal {
		t.Errorf("expected AlreadyTerminal == false")
	}
	if res.NoOp {
		t.Errorf("expected NoOp == false")
	}
	if !res.CancelRequested {
		t.Errorf("expected CancelRequested == true")
	}
	if res.Status != domain.SessionReviewing {
		t.Errorf("expected Status == reviewing, got %s", res.Status)
	}
	if res.Session == nil {
		t.Fatalf("expected non-nil Session in result")
	}
	if !res.Session.CancelRequested {
		t.Errorf("expected Session.CancelRequested == true")
	}
	if res.Session.Status != domain.SessionReviewing {
		t.Errorf("expected Session.Status == reviewing, got %s", res.Session.Status)
	}

	// Re-verify through read-only pool
	st, err := store.ReadStatus(ctx, submitRes.SessionID)
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if !st.CancelRequested {
		t.Errorf("expected persisted CancelRequested == true")
	}
	if st.Status != domain.SessionReviewing {
		t.Errorf("expected persisted Status == reviewing, got %s", st.Status)
	}
}

// 3. Repeating request is idempotent: 1 -> 1 succeeds, returns Effective: false without changing other fields.
func TestRequestCancellation_IdempotentRepeated(t *testing.T) {
	store, _ := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_cancel_idem")
	submitRes, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_cancel_idem",
		ProjectID:      "proj_ci",
		IdempotencyKey: "key_ci",
		Title:          "Cancel Idempotent Test",
		Content:        "Spec content",
		Snapshot:       snap,
	})
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}

	// Claim to set timing timestamps
	policy := TimingPolicy{
		DispatchCutoff:      10 * time.Minute,
		CallTimeout:         5 * time.Minute,
		SessionHardDeadline: 20 * time.Minute,
	}
	_, err = store.ClaimSession(ctx, policy)
	if err != nil {
		t.Fatalf("claim failed: %v", err)
	}

	// First cancellation: 0 -> 1 (effective: true)
	res1, err := store.RequestCancellation(ctx, submitRes.SessionID)
	if err != nil {
		t.Fatalf("first cancellation failed: %v", err)
	}
	if !res1.Effective {
		t.Errorf("first cancellation: expected Effective == true")
	}
	if !res1.CancelRequested {
		t.Errorf("first cancellation: expected CancelRequested == true")
	}

	timeBeforeSecond := time.Now().UTC()

	// Second cancellation: 1 -> 1 (effective: false)
	res2, err := store.RequestCancellation(ctx, submitRes.SessionID)
	if err != nil {
		t.Fatalf("second cancellation failed: %v", err)
	}
	if res2.Effective {
		t.Errorf("second cancellation: expected Effective == false")
	}
	if !res2.CancelRequested {
		t.Errorf("second cancellation: expected CancelRequested == true")
	}
	if res2.AlreadyTerminal {
		t.Errorf("second cancellation: expected AlreadyTerminal == false")
	}
	if !res2.NoOp {
		t.Errorf("second cancellation: expected NoOp == true")
	}

	// Third cancellation: 1 -> 1 (effective: false)
	res3, err := store.RequestCancellation(ctx, submitRes.SessionID)
	if err != nil {
		t.Fatalf("third cancellation failed: %v", err)
	}
	if res3.Effective {
		t.Errorf("third cancellation: expected Effective == false")
	}
	if !res3.CancelRequested {
		t.Errorf("third cancellation: expected CancelRequested == true")
	}

	// Verify all fields remain exactly identical between first and second calls
	s1 := res1.Session
	s2 := res2.Session
	s3 := res3.Session

	if s1.CreatedAt != s2.CreatedAt || s1.CreatedAt != s3.CreatedAt {
		t.Errorf("CreatedAt changed: %v vs %v", s1.CreatedAt, s2.CreatedAt)
	}
	if *s1.ClaimedAt != *s2.ClaimedAt || *s1.ClaimedAt != *s3.ClaimedAt {
		t.Errorf("ClaimedAt changed: %v vs %v", s1.ClaimedAt, s2.ClaimedAt)
	}
	if *s1.DispatchCutoffAt != *s2.DispatchCutoffAt || *s1.DispatchCutoffAt != *s3.DispatchCutoffAt {
		t.Errorf("DispatchCutoffAt changed: %v vs %v", s1.DispatchCutoffAt, s2.DispatchCutoffAt)
	}
	if *s1.HardDeadlineAt != *s2.HardDeadlineAt || *s1.HardDeadlineAt != *s3.HardDeadlineAt {
		t.Errorf("HardDeadlineAt changed: %v vs %v", s1.HardDeadlineAt, s2.HardDeadlineAt)
	}
	if s1.TerminalAt != nil || s2.TerminalAt != nil {
		t.Errorf("TerminalAt should be nil")
	}
	if s1.Status != s2.Status {
		t.Errorf("Status changed: %s vs %s", s1.Status, s2.Status)
	}
	if s1.TerminalReason != s2.TerminalReason {
		t.Errorf("TerminalReason changed: %s vs %s", s1.TerminalReason, s2.TerminalReason)
	}
	if s1.CompletedRoleCount != s2.CompletedRoleCount || s1.IncompleteRoleCount != s2.IncompleteRoleCount {
		t.Errorf("counts changed")
	}

	// Ensure no timestamps are after timeBeforeSecond
	if s2.CreatedAt.After(timeBeforeSecond) {
		t.Errorf("CreatedAt was updated to current time")
	}
}

// 4. Terminal sessions are not reopened, have no role states changed, and return typed already-terminal/no-op result.
func TestRequestCancellation_TerminalSessionNoOp(t *testing.T) {
	ctx := context.Background()

	t.Run("terminal complete session with cancel_requested = false", func(t *testing.T) {
		store, writer := openGuardTestStore(t)
		snap := createTestFrozenSnapshot(t, "snap_term_comp")
		sessID := "sess_term_complete"
		projID := "proj_tc"

		// Persist snapshot via base submission
		_, err := store.Submit(ctx, SubmitParams{
			SessionID:      "sess_base_comp",
			ProjectID:      projID,
			IdempotencyKey: "idem_base_comp",
			Title:          "Base",
			Content:        "Content",
			Snapshot:       snap,
		})
		if err != nil {
			t.Fatalf("submit base failed: %v", err)
		}

		seedTerminalSession(t, writer, sessID, projID, snap.ID,
			domain.SessionComplete, domain.ReasonAllRolesComplete, false, 4)

		nowStr := time.Now().UTC().Format(time.RFC3339Nano)
		for _, role := range domain.Roles {
			rrID := fmt.Sprintf("%s:%s", sessID, role)
			_, err := writer.ExecContext(ctx, `INSERT INTO role_runs (
				id, session_id, role, status, call_count, started_at, completed_at, created_at
			) VALUES (?, ?, ?, 'complete', 1, ?, ?, ?);`,
				rrID, sessID, role.String(), nowStr, nowStr, nowStr,
			)
			if err != nil {
				t.Fatalf("insert complete role %s: %v", role, err)
			}
		}

		res, err := store.RequestCancellation(ctx, sessID)
		if err != nil {
			t.Fatalf("RequestCancellation on terminal complete failed: %v", err)
		}

		if res.Effective {
			t.Errorf("expected Effective == false for terminal session")
		}
		if !res.AlreadyTerminal {
			t.Errorf("expected AlreadyTerminal == true")
		}
		if !res.NoOp {
			t.Errorf("expected NoOp == true")
		}
		if res.CancelRequested {
			t.Errorf("expected CancelRequested == false (unchanged on terminal session)")
		}
		if res.Status != domain.SessionComplete {
			t.Errorf("expected Status == complete, got %s", res.Status)
		}
		if res.TerminalReason != domain.ReasonAllRolesComplete {
			t.Errorf("expected TerminalReason == all_roles_complete, got %s", res.TerminalReason)
		}

		// Re-read status to prove DB was not mutated
		st, err := store.ReadStatus(ctx, sessID)
		if err != nil {
			t.Fatalf("ReadStatus: %v", err)
		}
		if st.CancelRequested {
			t.Errorf("DB cancel_requested was mutated on terminal session")
		}
		if st.Status != domain.SessionComplete {
			t.Errorf("DB status was reopened from complete to %s", st.Status)
		}
		for _, r := range st.Roles {
			if r.Status != domain.RoleComplete {
				t.Errorf("role %s status was mutated to %s", r.Role, r.Status)
			}
		}
	})

	t.Run("terminal partial session with cancel_requested = true", func(t *testing.T) {
		store, writer := openGuardTestStore(t)
		snap := createTestFrozenSnapshot(t, "snap_term_part")
		sessID := "sess_term_partial"
		projID := "proj_tp"

		// Persist snapshot via base submission
		_, err := store.Submit(ctx, SubmitParams{
			SessionID:      "sess_base_part",
			ProjectID:      projID,
			IdempotencyKey: "idem_base_part",
			Title:          "Base",
			Content:        "Content",
			Snapshot:       snap,
		})
		if err != nil {
			t.Fatalf("submit base failed: %v", err)
		}

		seedTerminalSession(t, writer, sessID, projID, snap.ID,
			domain.SessionPartial, domain.ReasonUserCancelled, true, 2)

		nowStr := time.Now().UTC().Format(time.RFC3339Nano)
		for i, role := range domain.Roles {
			rrID := fmt.Sprintf("%s:%s", sessID, role)
			var status string
			var cause sql.NullString
			if i < 2 {
				status = "complete"
			} else {
				status = "interrupted"
				cause = sql.NullString{String: string(domain.CauseUserCancelled), Valid: true}
			}
			_, err := writer.ExecContext(ctx, `INSERT INTO role_runs (
				id, session_id, role, status, cause, call_count, started_at, completed_at, created_at
			) VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?);`,
				rrID, sessID, role.String(), status, cause, nowStr, nowStr, nowStr,
			)
			if err != nil {
				t.Fatalf("insert role %s: %v", role, err)
			}
		}

		res, err := store.RequestCancellation(ctx, sessID)
		if err != nil {
			t.Fatalf("RequestCancellation on terminal partial failed: %v", err)
		}

		if res.Effective {
			t.Errorf("expected Effective == false for terminal session")
		}
		if !res.AlreadyTerminal {
			t.Errorf("expected AlreadyTerminal == true")
		}
		if !res.NoOp {
			t.Errorf("expected NoOp == true")
		}
		if !res.CancelRequested {
			t.Errorf("expected CancelRequested == true")
		}
		if res.Status != domain.SessionPartial {
			t.Errorf("expected Status == partial, got %s", res.Status)
		}

		// Re-read status to prove DB was not mutated
		st, err := store.ReadStatus(ctx, sessID)
		if err != nil {
			t.Fatalf("ReadStatus: %v", err)
		}
		if st.Status != domain.SessionPartial {
			t.Errorf("DB status changed from partial to %s", st.Status)
		}
	})

	t.Run("terminal failed session with cancel_requested = false", func(t *testing.T) {
		store, writer := openGuardTestStore(t)
		snap := createTestFrozenSnapshot(t, "snap_term_fail")
		sessID := "sess_term_failed"
		projID := "proj_tf"

		// Persist snapshot via base submission
		_, err := store.Submit(ctx, SubmitParams{
			SessionID:      "sess_base_fail",
			ProjectID:      projID,
			IdempotencyKey: "idem_base_fail",
			Title:          "Base",
			Content:        "Content",
			Snapshot:       snap,
		})
		if err != nil {
			t.Fatalf("submit base failed: %v", err)
		}

		seedTerminalSession(t, writer, sessID, projID, snap.ID,
			domain.SessionFailed, domain.ReasonRoleFailures, false, 0)

		nowStr := time.Now().UTC().Format(time.RFC3339Nano)
		for _, role := range domain.Roles {
			rrID := fmt.Sprintf("%s:%s", sessID, role)
			_, err := writer.ExecContext(ctx, `INSERT INTO role_runs (
				id, session_id, role, status, error_category, call_count, started_at, completed_at, created_at
			) VALUES (?, ?, ?, 'failed', 'provider_rejected', 1, ?, ?, ?);`,
				rrID, sessID, role.String(), nowStr, nowStr, nowStr,
			)
			if err != nil {
				t.Fatalf("insert failed role %s: %v", role, err)
			}
		}

		res, err := store.RequestCancellation(ctx, sessID)
		if err != nil {
			t.Fatalf("RequestCancellation on terminal failed: %v", err)
		}

		if res.Effective {
			t.Errorf("expected Effective == false for terminal session")
		}
		if !res.AlreadyTerminal {
			t.Errorf("expected AlreadyTerminal == true")
		}
		if !res.NoOp {
			t.Errorf("expected NoOp == true")
		}
		if res.CancelRequested {
			t.Errorf("expected CancelRequested == false")
		}
		if res.Status != domain.SessionFailed {
			t.Errorf("expected Status == failed, got %s", res.Status)
		}
	})
}

// 5. Unknown session and project mismatch return typed SessionNotFoundError matching ErrNotFound.
func TestRequestCancellation_UnknownAndScopedMismatch(t *testing.T) {
	store, _ := openGuardTestStore(t)
	ctx := context.Background()

	t.Run("non-existent session returns typed not-found", func(t *testing.T) {
		_, err := store.RequestCancellation(ctx, "non_existent_session_id")
		if err == nil {
			t.Fatalf("expected error for non-existent session")
		}
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("expected errors.Is(err, ErrNotFound), got %v", err)
		}
		if !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("expected errors.Is(err, ErrSessionNotFound), got %v", err)
		}
		var notFound *SessionNotFoundError
		if !errors.As(err, &notFound) {
			t.Errorf("expected errors.As(err, &SessionNotFoundError), got %T", err)
		} else if notFound.SessionID != "non_existent_session_id" {
			t.Errorf("SessionID = %q, want non_existent_session_id", notFound.SessionID)
		}
	})

	t.Run("empty session id returns typed not-found", func(t *testing.T) {
		_, err := store.RequestCancellation(ctx, "")
		if err == nil || !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound for empty sessionID, got %v", err)
		}
	})

	t.Run("whitespace session id returns typed not-found", func(t *testing.T) {
		_, err := store.RequestCancellation(ctx, "   ")
		if err == nil || !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound for whitespace sessionID, got %v", err)
		}
	})

	t.Run("project scoped mismatch returns typed not-found", func(t *testing.T) {
		snap := createTestFrozenSnapshot(t, "snap_scope_cancel")
		submitRes, err := store.Submit(ctx, SubmitParams{
			SessionID:      "sess_scope_test",
			ProjectID:      "proj_actual",
			IdempotencyKey: "key_scope",
			Title:          "Scope Test",
			Content:        "Spec content",
			Snapshot:       snap,
		})
		if err != nil {
			t.Fatalf("submit failed: %v", err)
		}

		// Request cancellation with mismatched project
		_, err = store.RequestCancellationScoped(ctx, "proj_wrong", submitRes.SessionID)
		if err == nil {
			t.Fatalf("expected error for project mismatch")
		}
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("expected errors.Is(err, ErrNotFound), got %v", err)
		}
		var notFound *SessionNotFoundError
		if !errors.As(err, &notFound) {
			t.Errorf("expected errors.As(err, &SessionNotFoundError), got %T", err)
		} else {
			if notFound.SessionID != submitRes.SessionID {
				t.Errorf("SessionID = %q, want %q", notFound.SessionID, submitRes.SessionID)
			}
			if notFound.ProjectID != "proj_wrong" {
				t.Errorf("ProjectID = %q, want proj_wrong", notFound.ProjectID)
			}
		}

		// Correct project succeeds
		res, err := store.RequestCancellationScoped(ctx, "proj_actual", submitRes.SessionID)
		if err != nil {
			t.Fatalf("expected success with correct project, got %v", err)
		}
		if !res.Effective {
			t.Errorf("expected Effective == true")
		}
	})
}

// 6. Concurrent cancellation safety: 20 goroutines racing on the same session yield exactly one winner.
func TestRequestCancellation_ConcurrentSafety(t *testing.T) {
	t.Run("concurrent cancellation on queued session", func(t *testing.T) {
		store, _ := openGuardTestStore(t)
		ctx := context.Background()

		snap := createTestFrozenSnapshot(t, "snap_conc_q")
		submitRes, err := store.Submit(ctx, SubmitParams{
			SessionID:      "sess_conc_queued",
			ProjectID:      "proj_conc_q",
			IdempotencyKey: "key_conc_q",
			Title:          "Concurrent Queued Test",
			Content:        "Spec content",
			Snapshot:       snap,
		})
		if err != nil {
			t.Fatalf("submit failed: %v", err)
		}

		const racers = 20
		barrier := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(racers)

		var (
			effectiveCount   int64
			ineffectiveCount int64
			errCount         int64
		)

		for i := 0; i < racers; i++ {
			go func() {
				defer wg.Done()
				<-barrier

				res, err := store.RequestCancellation(ctx, submitRes.SessionID)
				if err != nil {
					atomic.AddInt64(&errCount, 1)
					return
				}
				if res.Effective {
					atomic.AddInt64(&effectiveCount, 1)
				} else {
					atomic.AddInt64(&ineffectiveCount, 1)
				}
			}()
		}

		close(barrier)
		wg.Wait()

		if errCount != 0 {
			t.Fatalf("encountered %d errors in concurrent cancellation", errCount)
		}
		if effectiveCount != 1 {
			t.Fatalf("expected exactly 1 effective winner, got %d", effectiveCount)
		}
		if ineffectiveCount != racers-1 {
			t.Fatalf("expected %d ineffective callers, got %d", racers-1, ineffectiveCount)
		}

		// Re-read status
		st, err := store.ReadStatus(ctx, submitRes.SessionID)
		if err != nil {
			t.Fatalf("ReadStatus: %v", err)
		}
		if !st.CancelRequested {
			t.Errorf("expected CancelRequested == true")
		}
		if st.Status != domain.SessionQueued {
			t.Errorf("expected Status == queued, got %s", st.Status)
		}
	})

	t.Run("concurrent cancellation on reviewing session", func(t *testing.T) {
		store, _ := openGuardTestStore(t)
		ctx := context.Background()

		snap := createTestFrozenSnapshot(t, "snap_conc_rev")
		submitRes, err := store.Submit(ctx, SubmitParams{
			SessionID:      "sess_conc_reviewing",
			ProjectID:      "proj_conc_r",
			IdempotencyKey: "key_conc_r",
			Title:          "Concurrent Reviewing Test",
			Content:        "Spec content",
			Snapshot:       snap,
		})
		if err != nil {
			t.Fatalf("submit failed: %v", err)
		}

		policy := TimingPolicy{
			DispatchCutoff:      10 * time.Minute,
			CallTimeout:         5 * time.Minute,
			SessionHardDeadline: 20 * time.Minute,
		}
		_, err = store.ClaimSession(ctx, policy)
		if err != nil {
			t.Fatalf("claim failed: %v", err)
		}

		const racers = 20
		barrier := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(racers)

		var (
			effectiveCount   int64
			ineffectiveCount int64
			errCount         int64
		)

		for i := 0; i < racers; i++ {
			go func() {
				defer wg.Done()
				<-barrier

				res, err := store.RequestCancellation(ctx, submitRes.SessionID)
				if err != nil {
					atomic.AddInt64(&errCount, 1)
					return
				}
				if res.Effective {
					atomic.AddInt64(&effectiveCount, 1)
				} else {
					atomic.AddInt64(&ineffectiveCount, 1)
				}
			}()
		}

		close(barrier)
		wg.Wait()

		if errCount != 0 {
			t.Fatalf("encountered %d errors in concurrent cancellation", errCount)
		}
		if effectiveCount != 1 {
			t.Fatalf("expected exactly 1 effective winner, got %d", effectiveCount)
		}
		if ineffectiveCount != racers-1 {
			t.Fatalf("expected %d ineffective callers, got %d", racers-1, ineffectiveCount)
		}

		st, err := store.ReadStatus(ctx, submitRes.SessionID)
		if err != nil {
			t.Fatalf("ReadStatus: %v", err)
		}
		if !st.CancelRequested {
			t.Errorf("expected CancelRequested == true")
		}
		if st.Status != domain.SessionReviewing {
			t.Errorf("expected Status == reviewing, got %s", st.Status)
		}
	})
}

// 7. Rollback on failure leaves cancel_requested = 0.
func TestRequestCancellation_RollbackOnFailure(t *testing.T) {
	store, _ := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_cancel_rb")
	submitRes, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_cancel_rollback",
		ProjectID:      "proj_crb",
		IdempotencyKey: "key_crb",
		Title:          "Rollback Test",
		Content:        "Spec content",
		Snapshot:       snap,
	})
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}

	// Inject commit failure hook
	errInjected := errors.New("injected cancellation commit failure")
	cancelBeforeCommitHook = func(ctx context.Context, conn *sql.Conn) error {
		return errInjected
	}
	t.Cleanup(func() { cancelBeforeCommitHook = nil })

	_, err = store.RequestCancellation(ctx, submitRes.SessionID)
	if err == nil || !errors.Is(err, errInjected) {
		t.Fatalf("expected injected error, got: %v", err)
	}

	// Verify rollback: cancel_requested must still be false
	st, err := store.ReadStatus(ctx, submitRes.SessionID)
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if st.CancelRequested {
		t.Errorf("expected CancelRequested == false after rollback, got true")
	}

	// Clear hook and verify cancellation succeeds cleanly
	cancelBeforeCommitHook = nil

	res, err := store.RequestCancellation(ctx, submitRes.SessionID)
	if err != nil {
		t.Fatalf("second cancellation attempt failed: %v", err)
	}
	if !res.Effective {
		t.Errorf("expected Effective == true on clean retry")
	}
	if !res.CancelRequested {
		t.Errorf("expected CancelRequested == true")
	}

	stAfter, err := store.ReadStatus(ctx, submitRes.SessionID)
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if !stAfter.CancelRequested {
		t.Errorf("expected CancelRequested == true in DB")
	}
}

// 8. Cancellation never mutates status, role runs, terminal reason, counts, timing, or findings.
func TestRequestCancellation_UnchangedRoleAndSessionFields(t *testing.T) {
	store, _ := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_cancel_unchanged")
	submitRes, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_cancel_unchanged",
		ProjectID:      "proj_unchanged",
		IdempotencyKey: "key_unchanged",
		Title:          "Unchanged Fields Test",
		Content:        "Spec content",
		Snapshot:       snap,
	})
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}

	// Claim session so it has claimed_at, dispatch_cutoff_at, hard_deadline_at
	policy := TimingPolicy{
		DispatchCutoff:      15 * time.Minute,
		CallTimeout:         5 * time.Minute,
		SessionHardDeadline: 30 * time.Minute,
	}
	claimRes, err := store.ClaimSession(ctx, policy)
	if err != nil {
		t.Fatalf("claim failed: %v", err)
	}

	// Read pre-cancellation status
	preStatus, err := store.ReadStatus(ctx, submitRes.SessionID)
	if err != nil {
		t.Fatalf("ReadStatus (pre): %v", err)
	}
	if preStatus.CancelRequested {
		t.Fatalf("preStatus should have cancel_requested = false")
	}

	// Request cancellation
	cancelRes, err := store.RequestCancellation(ctx, submitRes.SessionID)
	if err != nil {
		t.Fatalf("RequestCancellation failed: %v", err)
	}
	if !cancelRes.Effective {
		t.Errorf("expected Effective == true")
	}

	// Read post-cancellation status
	postStatus, err := store.ReadStatus(ctx, submitRes.SessionID)
	if err != nil {
		t.Fatalf("ReadStatus (post): %v", err)
	}

	// Verify ONLY CancelRequested changed
	if !postStatus.CancelRequested {
		t.Errorf("postStatus.CancelRequested should be true")
	}

	// Assert every other field in postStatus is identical to preStatus
	if postStatus.SessionID != preStatus.SessionID {
		t.Errorf("SessionID changed: %s vs %s", postStatus.SessionID, preStatus.SessionID)
	}
	if postStatus.ProjectID != preStatus.ProjectID {
		t.Errorf("ProjectID changed: %s vs %s", postStatus.ProjectID, preStatus.ProjectID)
	}
	if postStatus.IdempotencyKey != preStatus.IdempotencyKey {
		t.Errorf("IdempotencyKey changed: %s vs %s", postStatus.IdempotencyKey, preStatus.IdempotencyKey)
	}
	if postStatus.RequestHash != preStatus.RequestHash {
		t.Errorf("RequestHash changed: %s vs %s", postStatus.RequestHash, preStatus.RequestHash)
	}
	if postStatus.SnapshotID != preStatus.SnapshotID {
		t.Errorf("SnapshotID changed: %s vs %s", postStatus.SnapshotID, preStatus.SnapshotID)
	}
	if postStatus.SnapshotHash != preStatus.SnapshotHash {
		t.Errorf("SnapshotHash changed: %s vs %s", postStatus.SnapshotHash, preStatus.SnapshotHash)
	}
	if postStatus.Status != preStatus.Status {
		t.Errorf("Status changed: %s vs %s", postStatus.Status, preStatus.Status)
	}
	if postStatus.CompletedRoleCount != preStatus.CompletedRoleCount {
		t.Errorf("CompletedRoleCount changed: %d vs %d", postStatus.CompletedRoleCount, preStatus.CompletedRoleCount)
	}
	if postStatus.IncompleteRoleCount != preStatus.IncompleteRoleCount {
		t.Errorf("IncompleteRoleCount changed: %d vs %d", postStatus.IncompleteRoleCount, preStatus.IncompleteRoleCount)
	}
	if postStatus.TerminalReason != preStatus.TerminalReason {
		t.Errorf("TerminalReason changed: %s vs %s", postStatus.TerminalReason, preStatus.TerminalReason)
	}
	if postStatus.CreatedAt != preStatus.CreatedAt {
		t.Errorf("CreatedAt changed: %v vs %v", postStatus.CreatedAt, preStatus.CreatedAt)
	}
	if *postStatus.ClaimedAt != *preStatus.ClaimedAt {
		t.Errorf("ClaimedAt changed: %v vs %v", postStatus.ClaimedAt, preStatus.ClaimedAt)
	}
	if *postStatus.DispatchCutoffAt != *preStatus.DispatchCutoffAt {
		t.Errorf("DispatchCutoffAt changed: %v vs %v", postStatus.DispatchCutoffAt, preStatus.DispatchCutoffAt)
	}
	if *postStatus.HardDeadlineAt != *preStatus.HardDeadlineAt {
		t.Errorf("HardDeadlineAt changed: %v vs %v", postStatus.HardDeadlineAt, preStatus.HardDeadlineAt)
	}
	if postStatus.TerminalAt != nil || preStatus.TerminalAt != nil {
		t.Errorf("TerminalAt should be nil")
	}

	// Verify all 4 roles remain completely identical in status and fields
	if len(postStatus.Roles) != domain.RoleCount {
		t.Fatalf("expected %d roles, got %d", domain.RoleCount, len(postStatus.Roles))
	}
	for i := range postStatus.Roles {
		preR := preStatus.Roles[i]
		postR := postStatus.Roles[i]

		if postR.Role != preR.Role {
			t.Errorf("role[%d].Role changed", i)
		}
		if postR.Status != preR.Status {
			t.Errorf("role[%d].Status changed: %s vs %s", i, postR.Status, preR.Status)
		}
		if postR.Status != domain.RolePending {
			t.Errorf("role[%d].Status should be pending, got %s", i, postR.Status)
		}
		if postR.Cause != preR.Cause {
			t.Errorf("role[%d].Cause changed: %s vs %s", i, postR.Cause, preR.Cause)
		}
		if postR.ErrorCategory != preR.ErrorCategory {
			t.Errorf("role[%d].ErrorCategory changed", i)
		}
		if postR.ErrorMessage != preR.ErrorMessage {
			t.Errorf("role[%d].ErrorMessage changed", i)
		}
		if postR.CallCount != preR.CallCount {
			t.Errorf("role[%d].CallCount changed", i)
		}
		if postR.StartedAt != preR.StartedAt {
			t.Errorf("role[%d].StartedAt changed", i)
		}
		if postR.CompletedAt != preR.CompletedAt {
			t.Errorf("role[%d].CompletedAt changed", i)
		}
		if postR.CreatedAt != preR.CreatedAt {
			t.Errorf("role[%d].CreatedAt changed", i)
		}
	}

	// Claim timing fields must match claimRes
	if !postStatus.ClaimedAt.Equal(claimRes.ClaimedAt) {
		t.Errorf("ClaimedAt %v != claimRes %v", postStatus.ClaimedAt, claimRes.ClaimedAt)
	}
	if !postStatus.DispatchCutoffAt.Equal(claimRes.DispatchCutoffAt) {
		t.Errorf("DispatchCutoffAt %v != claimRes %v", postStatus.DispatchCutoffAt, claimRes.DispatchCutoffAt)
	}
	if !postStatus.HardDeadlineAt.Equal(claimRes.HardDeadlineAt) {
		t.Errorf("HardDeadlineAt %v != claimRes %v", postStatus.HardDeadlineAt, claimRes.HardDeadlineAt)
	}
}

// 9. Context cancellation returns context.Canceled and leaves DB untouched.
func TestRequestCancellation_ContextCancellation(t *testing.T) {
	store, _ := openGuardTestStore(t)
	snap := createTestFrozenSnapshot(t, "snap_cancel_ctx")
	submitRes, err := store.Submit(context.Background(), SubmitParams{
		SessionID:      "sess_cancel_ctx",
		ProjectID:      "proj_cctx",
		IdempotencyKey: "key_cctx",
		Title:          "Context Test",
		Content:        "Spec content",
		Snapshot:       snap,
	})
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}

	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, err = store.RequestCancellation(cancelCtx, submitRes.SessionID)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}

	st, err := store.ReadStatus(context.Background(), submitRes.SessionID)
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if st.CancelRequested {
		t.Errorf("cancel_requested should remain false")
	}
}

// 10. Nil store returns error.
func TestRequestCancellation_NilStore(t *testing.T) {
	var s *Store
	_, err := s.RequestCancellation(context.Background(), "some_id")
	if err == nil {
		t.Fatalf("expected error from nil store")
	}

	_, err = RequestCancellation(context.Background(), nil, "some_id")
	if err == nil {
		t.Fatalf("expected error from nil store via package function")
	}
}

// 11. Aliases and convenience methods.
func TestRequestCancellation_Aliases(t *testing.T) {
	store, _ := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_cancel_aliases")
	submitRes, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_cancel_aliases",
		ProjectID:      "proj_aliases",
		IdempotencyKey: "key_aliases",
		Title:          "Aliases Test",
		Content:        "Spec content",
		Snapshot:       snap,
	})
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}

	// Test CancelSession alias
	res, err := store.CancelSession(ctx, submitRes.SessionID)
	if err != nil {
		t.Fatalf("CancelSession failed: %v", err)
	}
	if !res.IsEffective() {
		t.Errorf("expected IsEffective() == true")
	}
	if res.IsAlreadyTerminal() {
		t.Errorf("expected IsAlreadyTerminal() == false")
	}
	if res.IsNoOp() {
		t.Errorf("expected IsNoOp() == false")
	}
	if res.SessionStatus() == nil {
		t.Errorf("expected non-nil SessionStatus()")
	}

	// Test Cancel alias (should be idempotent now)
	res2, err := store.Cancel(ctx, submitRes.SessionID)
	if err != nil {
		t.Fatalf("Cancel failed: %v", err)
	}
	if res2.IsEffective() {
		t.Errorf("expected IsEffective() == false on repeat")
	}
	if !res2.IsNoOp() {
		t.Errorf("expected IsNoOp() == true on repeat")
	}

	// Test package-level functions
	res3, err := RequestCancellation(ctx, store, submitRes.SessionID)
	if err != nil {
		t.Fatalf("RequestCancellation package function failed: %v", err)
	}
	if res3.IsEffective() {
		t.Errorf("expected IsEffective() == false on repeat")
	}

	res4, err := CancelSession(ctx, store, submitRes.SessionID)
	if err != nil {
		t.Fatalf("CancelSession package function failed: %v", err)
	}
	if res4.IsEffective() {
		t.Errorf("expected IsEffective() == false on repeat")
	}

	res5, err := Cancel(ctx, store, submitRes.SessionID)
	if err != nil {
		t.Fatalf("Cancel package function failed: %v", err)
	}
	if res5.IsEffective() {
		t.Errorf("expected IsEffective() == false on repeat")
	}
}

// 12. Conflict / concurrent update during hook affects 0 rows.
func TestRequestCancellation_ZeroRowsConflictHook(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_cancel_zerorows")
	submitRes, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_cancel_zerorows",
		ProjectID:      "proj_zr",
		IdempotencyKey: "key_zr",
		Title:          "Zero Rows Test",
		Content:        "Spec content",
		Snapshot:       snap,
	})
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}

	// Inject hook before update that updates cancel_requested to 1 on the same connection
	cancelBeforeUpdateHook = func(ctx context.Context, conn *sql.Conn, sessionID string) error {
		_, err := conn.ExecContext(ctx, "UPDATE sessions SET cancel_requested = 1 WHERE id = ?;", sessionID)
		return err
	}
	t.Cleanup(func() { cancelBeforeUpdateHook = nil })

	res, err := store.RequestCancellation(ctx, submitRes.SessionID)
	if err != nil {
		t.Fatalf("RequestCancellation failed: %v", err)
	}

	// The subsequent guarded update affected 0 rows, so Effective must be false
	if res.Effective {
		t.Errorf("expected Effective == false when rowsAffected == 0")
	}
	if !res.CancelRequested {
		t.Errorf("expected CancelRequested == true")
	}
	_ = writer
}
