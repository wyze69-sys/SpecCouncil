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

// Helper to set up a reviewing session ready for dispatch tests.
func setupReviewingSession(t *testing.T, sessionID, projectID string, cutoffOffset time.Duration) (*Store, *sql.DB, time.Time) {
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
		Title:          "Dispatch Test " + sessionID,
		Content:        "Spec content",
		Snapshot:       snap,
		CreatedAt:      t0.Add(-5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("submit %s: %v", sessionID, err)
	}

	policy := TimingPolicy{
		DispatchCutoff:      cutoffOffset,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: cutoffOffset + 15*time.Minute,
	}

	claimRes, err := store.ClaimSession(ctx, policy)
	if err != nil || !claimRes.Claimed {
		t.Fatalf("claim %s: %v", sessionID, err)
	}

	return store, writer, t0
}

// 1. Capacity 0, 1, 2 and MAX_IN_FLIGHT = 2 Invariant
func TestReserveRole_Capacity_0_1_2(t *testing.T) {
	store, writer, t0 := setupReviewingSession(t, "sess_cap", "proj_cap", 30*time.Minute)
	ctx := context.Background()

	// Initial capacity check: 0 in_flight
	var initialInFlight int
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM role_runs WHERE session_id = 'sess_cap' AND status = 'in_flight';").Scan(&initialInFlight)
	if initialInFlight != 0 {
		t.Fatalf("expected 0 in_flight initially, got %d", initialInFlight)
	}

	// First reservation: capacity 0 -> 1 (Requirements)
	res1, err := store.ReserveRoleWithNow(ctx, "sess_cap", t0.Add(1*time.Minute))
	if err != nil {
		t.Fatalf("reserve 1 failed: %v", err)
	}
	if !res1.Reserved {
		t.Fatalf("expected res1.Reserved == true, got false (reason: %s)", res1.NoWorkReason)
	}
	if res1.Role != domain.RoleRequirements {
		t.Errorf("expected role requirements, got %s", res1.Role)
	}
	if res1.Status != domain.RoleInFlight {
		t.Errorf("expected status in_flight, got %s", res1.Status)
	}
	if res1.InFlightCount != 1 {
		t.Errorf("expected InFlightCount == 1, got %d", res1.InFlightCount)
	}
	if res1.CallCount != 0 {
		t.Errorf("expected CallCount == 0, got %d", res1.CallCount)
	}
	if res1.StartedAt == nil || !res1.StartedAt.Equal(t0.Add(1*time.Minute)) {
		t.Errorf("unexpected StartedAt: %v", res1.StartedAt)
	}

	// Verify database has 1 in_flight
	var count1 int
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM role_runs WHERE session_id = 'sess_cap' AND status = 'in_flight';").Scan(&count1)
	if count1 != 1 {
		t.Fatalf("database has %d in_flight, want 1", count1)
	}

	// Second reservation: capacity 1 -> 2 (Architecture)
	res2, err := store.ReserveRoleWithNow(ctx, "sess_cap", t0.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("reserve 2 failed: %v", err)
	}
	if !res2.Reserved {
		t.Fatalf("expected res2.Reserved == true, got false (reason: %s)", res2.NoWorkReason)
	}
	if res2.Role != domain.RoleArchitecture {
		t.Errorf("expected role architecture, got %s", res2.Role)
	}
	if res2.InFlightCount != 2 {
		t.Errorf("expected InFlightCount == 2, got %d", res2.InFlightCount)
	}

	// Verify database has exactly 2 in_flight (MAX_IN_FLIGHT reached)
	var count2 int
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM role_runs WHERE session_id = 'sess_cap' AND status = 'in_flight';").Scan(&count2)
	if count2 != MaxInFlight {
		t.Fatalf("database has %d in_flight, want %d", count2, MaxInFlight)
	}

	// Third reservation: capacity 2 -> refused (CapacityFull)
	res3, err := store.ReserveRoleWithNow(ctx, "sess_cap", t0.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("reserve 3 returned unexpected error: %v", err)
	}
	if res3.Reserved {
		t.Fatalf("expected res3.Reserved == false when at max capacity, but reserved %s", res3.Role)
	}
	if res3.NoWorkReason != DispatchNoWorkCapacityFull {
		t.Errorf("expected NoWorkReason == capacity_full, got %q", res3.NoWorkReason)
	}

	// Verify database STILL has exactly 2 in_flight (invariant holds)
	var count3 int
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM role_runs WHERE session_id = 'sess_cap' AND status = 'in_flight';").Scan(&count3)
	if count3 != MaxInFlight {
		t.Fatalf("database has %d in_flight, want %d", count3, MaxInFlight)
	}

	// Simulate completion of Requirements role: in_flight -> complete
	_, err = writer.ExecContext(ctx, `
		UPDATE role_runs
		SET status = 'complete',
			completed_at = ?,
			call_count = 1
		WHERE session_id = 'sess_cap' AND role = 'requirements';
	`, formatUTCTimestamp(t0.Add(4*time.Minute)))
	if err != nil {
		t.Fatalf("complete requirements: %v", err)
	}

	// Database now has 1 in_flight (Architecture)
	var countAfterComplete int
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM role_runs WHERE session_id = 'sess_cap' AND status = 'in_flight';").Scan(&countAfterComplete)
	if countAfterComplete != 1 {
		t.Fatalf("database has %d in_flight, want 1", countAfterComplete)
	}

	// Fourth reservation: capacity 1 -> 2 (QA)
	res4, err := store.ReserveRoleWithNow(ctx, "sess_cap", t0.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("reserve 4 failed: %v", err)
	}
	if !res4.Reserved || res4.Role != domain.RoleQA {
		t.Fatalf("expected reserved QA, got reserved=%v, role=%s, reason=%s", res4.Reserved, res4.Role, res4.NoWorkReason)
	}
	if res4.InFlightCount != 2 {
		t.Errorf("expected InFlightCount == 2, got %d", res4.InFlightCount)
	}

	// Complete Architecture and QA
	_, err = writer.ExecContext(ctx, `
		UPDATE role_runs
		SET status = 'complete',
			completed_at = ?,
			call_count = 1
		WHERE session_id = 'sess_cap' AND role = 'architecture';
	`, formatUTCTimestamp(t0.Add(6*time.Minute)))
	if err != nil {
		t.Fatalf("complete architecture: %v", err)
	}
	_, err = writer.ExecContext(ctx, `
		UPDATE role_runs
		SET status = 'complete',
			completed_at = ?,
			call_count = 1
		WHERE session_id = 'sess_cap' AND role = 'qa';
	`, formatUTCTimestamp(t0.Add(7*time.Minute)))
	if err != nil {
		t.Fatalf("complete qa: %v", err)
	}

	// Fifth reservation: capacity 0 -> 1 (Security)
	res5, err := store.ReserveRoleWithNow(ctx, "sess_cap", t0.Add(8*time.Minute))
	if err != nil {
		t.Fatalf("reserve 5 failed: %v", err)
	}
	if !res5.Reserved || res5.Role != domain.RoleSecurity {
		t.Fatalf("expected reserved Security, got reserved=%v, role=%s", res5.Reserved, res5.Role)
	}

	// Complete Security
	_, err = writer.ExecContext(ctx, `
		UPDATE role_runs
		SET status = 'complete',
			completed_at = ?,
			call_count = 1
		WHERE session_id = 'sess_cap' AND role = 'security';
	`, formatUTCTimestamp(t0.Add(9*time.Minute)))
	if err != nil {
		t.Fatalf("complete security: %v", err)
	}

	// Sixth reservation: no pending roles remaining
	res6, err := store.ReserveRoleWithNow(ctx, "sess_cap", t0.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("reserve 6 returned unexpected error: %v", err)
	}
	if res6.Reserved {
		t.Fatalf("expected reserved == false when all roles terminal")
	}
	if res6.NoWorkReason != DispatchNoWorkNoPendingRole {
		t.Errorf("expected NoWorkReason == no_pending_role, got %q", res6.NoWorkReason)
	}
}

// 2. Canonical Role Order Determinism (even when rows are physically scrambled in DB)
func TestReserveRole_CanonicalRoleOrder(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	origClock := clock
	clock = func() time.Time { return t0 }
	t.Cleanup(func() { clock = origClock })

	snap := createTestFrozenSnapshot(t, "snap_scrambled")
	// Insert snapshot and units
	insertSnapshotAndUnit(t, writer, snap.ID, "u1")

	// Insert reviewing session directly
	sessionID := "sess_scrambled"
	cutoffStr := formatUTCTimestamp(t0.Add(30 * time.Minute))
	deadlineStr := formatUTCTimestamp(t0.Add(45 * time.Minute))
	claimedStr := formatUTCTimestamp(t0)
	createdStr := formatUTCTimestamp(t0.Add(-5 * time.Minute))

	_, err := writer.ExecContext(ctx, `
		INSERT INTO sessions (
			id, project_id, idempotency_key, request_hash, snapshot_id,
			status, cancel_requested, completed_role_count, incomplete_role_count,
			claimed_at, dispatch_cutoff_at, hard_deadline_at, created_at
		) VALUES (
			?, 'proj_scrambled', 'key_scrambled', ?, ?,
			'reviewing', 0, 0, 4,
			?, ?, ?, ?
		);
	`, sessionID, testValidHash, snap.ID, claimedStr, cutoffStr, deadlineStr, createdStr)
	if err != nil {
		t.Fatalf("insert reviewing session: %v", err)
	}

	// Insert role runs in reverse/scrambled order: security, qa, architecture, requirements
	scrambledRoles := []domain.Role{domain.RoleSecurity, domain.RoleQA, domain.RoleArchitecture, domain.RoleRequirements}
	for _, r := range scrambledRoles {
		roleRunID := fmt.Sprintf("%s:%s", sessionID, r)
		_, err := writer.ExecContext(ctx, `
			INSERT INTO role_runs (id, session_id, role, status, call_count, created_at)
			VALUES (?, ?, ?, 'pending', 0, ?);
		`, roleRunID, sessionID, r.String(), createdStr)
		if err != nil {
			t.Fatalf("insert role %s: %v", r, err)
		}
	}

	// Reserve 1: MUST select Requirements first despite being inserted last in DB
	res1, err := store.ReserveRoleWithNow(ctx, sessionID, t0.Add(1*time.Minute))
	if err != nil || !res1.Reserved {
		t.Fatalf("reserve 1 failed: %v (reason: %s)", err, res1.NoWorkReason)
	}
	if res1.Role != domain.RoleRequirements {
		t.Fatalf("expected role requirements, got %s", res1.Role)
	}

	// Reserve 2: MUST select Architecture second
	res2, err := store.ReserveRoleWithNow(ctx, sessionID, t0.Add(2*time.Minute))
	if err != nil || !res2.Reserved {
		t.Fatalf("reserve 2 failed: %v (reason: %s)", err, res2.NoWorkReason)
	}
	if res2.Role != domain.RoleArchitecture {
		t.Fatalf("expected role architecture, got %s", res2.Role)
	}

	// Transition Requirements to complete
	_, err = writer.ExecContext(ctx, `
		UPDATE role_runs SET status = 'complete', completed_at = ?, call_count = 1
		WHERE session_id = ? AND role = 'requirements';
	`, formatUTCTimestamp(t0.Add(3*time.Minute)), sessionID)
	if err != nil {
		t.Fatalf("complete requirements: %v", err)
	}

	// Reserve 3: MUST select QA third
	res3, err := store.ReserveRoleWithNow(ctx, sessionID, t0.Add(4*time.Minute))
	if err != nil || !res3.Reserved {
		t.Fatalf("reserve 3 failed: %v (reason: %s)", err, res3.NoWorkReason)
	}
	if res3.Role != domain.RoleQA {
		t.Fatalf("expected role qa, got %s", res3.Role)
	}

	// Transition Architecture to complete
	_, err = writer.ExecContext(ctx, `
		UPDATE role_runs SET status = 'complete', completed_at = ?, call_count = 1
		WHERE session_id = ? AND role = 'architecture';
	`, formatUTCTimestamp(t0.Add(5*time.Minute)), sessionID)
	if err != nil {
		t.Fatalf("complete architecture: %v", err)
	}

	// Reserve 4: MUST select Security fourth
	res4, err := store.ReserveRoleWithNow(ctx, sessionID, t0.Add(6*time.Minute))
	if err != nil || !res4.Reserved {
		t.Fatalf("reserve 4 failed: %v (reason: %s)", err, res4.NoWorkReason)
	}
	if res4.Role != domain.RoleSecurity {
		t.Fatalf("expected role security, got %s", res4.Role)
	}
}

// 3. Cutoff Boundary: now < cutoff vs now == cutoff vs now > cutoff
func TestReserveRole_CutoffBoundary(t *testing.T) {
	cutoffOffset := 20 * time.Minute
	ctx := context.Background()

	t.Run("now strictly before cutoff succeeds", func(t *testing.T) {
		store, _, t0 := setupReviewingSession(t, "sess_cutoff_before", "proj_cb", cutoffOffset)
		cutoffTime := t0.Add(cutoffOffset)

		res, err := store.ReserveRoleWithNow(ctx, "sess_cutoff_before", cutoffTime.Add(-1*time.Nanosecond))
		if err != nil {
			t.Fatalf("reserve before cutoff failed: %v", err)
		}
		if !res.Reserved {
			t.Fatalf("expected Reserved == true, got false (reason: %s)", res.NoWorkReason)
		}
		if res.Role != domain.RoleRequirements {
			t.Errorf("expected role requirements, got %s", res.Role)
		}
	})

	t.Run("now exactly at cutoff fails with cutoff reason", func(t *testing.T) {
		store, writer, t0 := setupReviewingSession(t, "sess_cutoff_at", "proj_ca", cutoffOffset)
		cutoffTime := t0.Add(cutoffOffset)

		res, err := store.ReserveRoleWithNow(ctx, "sess_cutoff_at", cutoffTime)
		if err != nil {
			t.Fatalf("reserve at cutoff returned error: %v", err)
		}
		if res.Reserved {
			t.Fatalf("expected Reserved == false at cutoff boundary")
		}
		if res.NoWorkReason != DispatchNoWorkCutoff {
			t.Errorf("expected NoWorkReason == cutoff, got %q", res.NoWorkReason)
		}

		// Verify no role was transitioned to in_flight
		var inFlightCount int
		_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM role_runs WHERE session_id = 'sess_cutoff_at' AND status = 'in_flight';").Scan(&inFlightCount)
		if inFlightCount != 0 {
			t.Errorf("expected 0 in_flight, got %d", inFlightCount)
		}
	})

	t.Run("now after cutoff fails with cutoff reason", func(t *testing.T) {
		store, _, t0 := setupReviewingSession(t, "sess_cutoff_after", "proj_cafter", cutoffOffset)
		cutoffTime := t0.Add(cutoffOffset)

		res, err := store.ReserveRoleWithNow(ctx, "sess_cutoff_after", cutoffTime.Add(1*time.Minute))
		if err != nil {
			t.Fatalf("reserve after cutoff returned error: %v", err)
		}
		if res.Reserved {
			t.Fatalf("expected Reserved == false after cutoff")
		}
		if res.NoWorkReason != DispatchNoWorkCutoff {
			t.Errorf("expected NoWorkReason == cutoff, got %q", res.NoWorkReason)
		}
	})

	t.Run("cutoff boundary race rechecked in authoritative UPDATE", func(t *testing.T) {
		store, _, t0 := setupReviewingSession(t, "sess_cutoff_race", "proj_crace", cutoffOffset)

		// Test that even if fast-path check passed, if cutoff arrives before UPDATE,
		// the authoritative UPDATE's WHERE clause rejects the reservation.
		dispatchBeforeUpdateHook = func(ctx context.Context, conn *sql.Conn, sID string, role domain.Role) error {
			// Advance cutoff timestamp in DB to be in the past relative to now
			pastCutoff := formatUTCTimestamp(t0.Add(-10 * time.Minute))
			_, err := conn.ExecContext(ctx, `
				UPDATE sessions SET dispatch_cutoff_at = ? WHERE id = ?;
			`, pastCutoff, sID)
			return err
		}
		t.Cleanup(func() { dispatchBeforeUpdateHook = nil })

		res, err := store.ReserveRoleWithNow(ctx, "sess_cutoff_race", t0.Add(5*time.Minute))
		if err != nil {
			t.Fatalf("reserve returned unexpected error: %v", err)
		}
		if res.Reserved {
			t.Fatalf("expected reservation to be prevented by guarded UPDATE")
		}
		if res.NoWorkReason != DispatchNoWorkGuardConflict {
			t.Errorf("expected NoWorkReason == guard_conflict, got %q", res.NoWorkReason)
		}
	})
}

// 4. Cancellation Race Protection
func TestReserveRole_Cancellation(t *testing.T) {
	t.Run("refuses reservation when cancel_requested = 1", func(t *testing.T) {
		store, _, t0 := setupReviewingSession(t, "sess_canc_1", "proj_c1", 30*time.Minute)
		ctx := context.Background()

		// Request cancellation on the reviewing session
		cancelRes, err := store.RequestCancellation(ctx, "sess_canc_1")
		if err != nil || !cancelRes.Effective {
			t.Fatalf("cancel failed: %v", err)
		}

		// Attempt reservation: MUST be refused with cancelled reason
		res, err := store.ReserveRoleWithNow(ctx, "sess_canc_1", t0.Add(1*time.Minute))
		if err != nil {
			t.Fatalf("reserve returned error: %v", err)
		}
		if res.Reserved {
			t.Fatalf("expected Reserved == false on cancelled session")
		}
		if res.NoWorkReason != DispatchNoWorkCancelled {
			t.Errorf("expected NoWorkReason == cancelled, got %q", res.NoWorkReason)
		}
	})

	t.Run("concurrent cancellation committing before guarded update prevents reservation", func(t *testing.T) {
		store, _, t0 := setupReviewingSession(t, "sess_canc_race", "proj_cr", 30*time.Minute)
		ctx := context.Background()

		// Inject cancellation right before the guarded UPDATE runs
		dispatchBeforeUpdateHook = func(ctx context.Context, conn *sql.Conn, sID string, role domain.Role) error {
			// Update cancel_requested within the same immediate transaction before the role UPDATE
			_, err := conn.ExecContext(ctx, `
				UPDATE sessions SET cancel_requested = 1 WHERE id = ?;
			`, sID)
			return err
		}
		t.Cleanup(func() { dispatchBeforeUpdateHook = nil })

		res, err := store.ReserveRoleWithNow(ctx, "sess_canc_race", t0.Add(1*time.Minute))
		if err != nil {
			t.Fatalf("reserve returned error: %v", err)
		}
		if res.Reserved {
			t.Fatalf("expected reservation to be prevented by concurrent cancellation")
		}
		if res.NoWorkReason != DispatchNoWorkGuardConflict {
			t.Errorf("expected NoWorkReason == guard_conflict, got %q", res.NoWorkReason)
		}

		// Verify database state: no role is in_flight
		writer, _ := store.writerDB()
		var inFlightCount int
		_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM role_runs WHERE session_id = 'sess_canc_race' AND status = 'in_flight';").Scan(&inFlightCount)
		if inFlightCount != 0 {
			t.Errorf("expected 0 in_flight roles, got %d", inFlightCount)
		}
	})

	t.Run("reservation committing first is legitimately in_flight when cancellation follows", func(t *testing.T) {
		store, writer, t0 := setupReviewingSession(t, "sess_canc_drain", "proj_cd", 30*time.Minute)
		ctx := context.Background()

		// 1. Reservation commits first
		res, err := store.ReserveRoleWithNow(ctx, "sess_canc_drain", t0.Add(1*time.Minute))
		if err != nil || !res.Reserved {
			t.Fatalf("reservation 1 failed: %v", err)
		}
		if res.Role != domain.RoleRequirements {
			t.Fatalf("expected requirements, got %s", res.Role)
		}

		// 2. Cancellation follows and commits
		cancelRes, err := store.RequestCancellation(ctx, "sess_canc_drain")
		if err != nil || !cancelRes.Effective {
			t.Fatalf("cancel failed: %v", err)
		}

		// 3. Verify in-flight role is STILL legitimately in_flight
		var roleStatus string
		var startedAt sql.NullString
		err = writer.QueryRowContext(ctx, `
			SELECT status, started_at FROM role_runs WHERE session_id = 'sess_canc_drain' AND role = 'requirements';
		`).Scan(&roleStatus, &startedAt)
		if err != nil {
			t.Fatalf("query requirements role: %v", err)
		}
		if roleStatus != "in_flight" {
			t.Errorf("expected role status in_flight, got %q", roleStatus)
		}
		if !startedAt.Valid || startedAt.String == "" {
			t.Errorf("expected valid started_at")
		}

		// 4. Subsequent reservation attempts are rejected because cancel_requested = 1
		res2, err := store.ReserveRoleWithNow(ctx, "sess_canc_drain", t0.Add(2*time.Minute))
		if err != nil {
			t.Fatalf("reserve 2 returned error: %v", err)
		}
		if res2.Reserved {
			t.Fatalf("expected subsequent reservation to fail")
		}
		if res2.NoWorkReason != DispatchNoWorkCancelled {
			t.Errorf("expected NoWorkReason == cancelled, got %q", res2.NoWorkReason)
		}
	})
}

// 5. Two Concurrent Reservations with One Winner Per Slot
func TestReserveRole_Concurrency_OneWinnerPerSlot(t *testing.T) {
	t.Run("one remaining slot has exactly one winner", func(t *testing.T) {
		store, writer, t0 := setupReviewingSession(t, "sess_conc_slot1", "proj_cs1", 30*time.Minute)
		ctx := context.Background()

		// First, reserve slot 1 legitimately
		res1, err := store.ReserveRoleWithNow(ctx, "sess_conc_slot1", t0.Add(1*time.Minute))
		if err != nil || !res1.Reserved {
			t.Fatalf("initial reservation failed: %v", err)
		}

		// Verify 1 in-flight
		var count1 int
		_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM role_runs WHERE session_id = 'sess_conc_slot1' AND status = 'in_flight';").Scan(&count1)
		if count1 != 1 {
			t.Fatalf("expected 1 in_flight, got %d", count1)
		}

		// Now 2 concurrent goroutines race for the 1 remaining slot
		const numRacers = 2
		startBarrier := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(numRacers)

		var (
			winnerCount atomic.Int32
			noWorkCount atomic.Int32
			errCount    atomic.Int32
		)

		for i := 0; i < numRacers; i++ {
			go func() {
				defer wg.Done()
				<-startBarrier

				res, err := store.ReserveRoleWithNow(ctx, "sess_conc_slot1", t0.Add(2*time.Minute))
				if err != nil {
					errCount.Add(1)
					return
				}
				if res.Reserved {
					winnerCount.Add(1)
				} else {
					noWorkCount.Add(1)
				}
			}()
		}

		close(startBarrier)
		wg.Wait()

		if errs := errCount.Load(); errs > 0 {
			t.Fatalf("%d callers returned errors", errs)
		}
		if winners := winnerCount.Load(); winners != 1 {
			t.Fatalf("expected exactly 1 winner for the 1 remaining slot, got %d", winners)
		}
		if noWork := noWorkCount.Load(); noWork != 1 {
			t.Fatalf("expected exactly 1 no-work, got %d", noWork)
		}

		// Total in-flight in database MUST be exactly 2
		var finalCount int
		_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM role_runs WHERE session_id = 'sess_conc_slot1' AND status = 'in_flight';").Scan(&finalCount)
		if finalCount != MaxInFlight {
			t.Fatalf("database has %d in_flight, want %d", finalCount, MaxInFlight)
		}
	})

	t.Run("ten concurrent racers for two slots yield exactly two winners", func(t *testing.T) {
		store, writer, t0 := setupReviewingSession(t, "sess_conc_multi", "proj_cm", 30*time.Minute)
		ctx := context.Background()

		const numRacers = 10
		startBarrier := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(numRacers)

		var (
			winnerCount atomic.Int32
			noWorkCount atomic.Int32
			errCount    atomic.Int32
			rolesSeen   sync.Map
		)

		for i := 0; i < numRacers; i++ {
			go func() {
				defer wg.Done()
				<-startBarrier

				res, err := store.ReserveRoleWithNow(ctx, "sess_conc_multi", t0.Add(1*time.Minute))
				if err != nil {
					errCount.Add(1)
					return
				}
				if res.Reserved {
					winnerCount.Add(1)
					rolesSeen.Store(res.Role, true)
				} else {
					noWorkCount.Add(1)
				}
			}()
		}

		close(startBarrier)
		wg.Wait()

		if errs := errCount.Load(); errs > 0 {
			t.Fatalf("%d racers returned errors", errs)
		}
		if winners := winnerCount.Load(); winners != MaxInFlight {
			t.Fatalf("expected exactly %d winners, got %d", MaxInFlight, winners)
		}
		if noWork := noWorkCount.Load(); noWork != numRacers-MaxInFlight {
			t.Fatalf("expected %d no-work results, got %d", numRacers-MaxInFlight, noWork)
		}

		// Database MUST have exactly 2 in_flight roles
		var inFlightCount int
		_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM role_runs WHERE session_id = 'sess_conc_multi' AND status = 'in_flight';").Scan(&inFlightCount)
		if inFlightCount != MaxInFlight {
			t.Fatalf("expected %d in_flight in database, got %d", MaxInFlight, inFlightCount)
		}

		// The two reserved roles MUST be Requirements and Architecture
		if _, ok := rolesSeen.Load(domain.RoleRequirements); !ok {
			t.Errorf("expected Requirements to be one of the reserved roles")
		}
		if _, ok := rolesSeen.Load(domain.RoleArchitecture); !ok {
			t.Errorf("expected Architecture to be one of the reserved roles")
		}
	})
}

// 6. Duplicate Reservation Prevention
func TestReserveRole_DuplicateReservationPrevention(t *testing.T) {
	store, writer, t0 := setupReviewingSession(t, "sess_dup", "proj_dup", 30*time.Minute)
	ctx := context.Background()

	// 1st reservation reserves Requirements
	res1, err := store.ReserveRoleWithNow(ctx, "sess_dup", t0.Add(1*time.Minute))
	if err != nil || !res1.Reserved {
		t.Fatalf("reserve 1 failed: %v", err)
	}
	if res1.Role != domain.RoleRequirements {
		t.Fatalf("expected requirements, got %s", res1.Role)
	}

	// 2nd reservation reserves Architecture, NEVER Requirements again
	res2, err := store.ReserveRoleWithNow(ctx, "sess_dup", t0.Add(2*time.Minute))
	if err != nil || !res2.Reserved {
		t.Fatalf("reserve 2 failed: %v", err)
	}
	if res2.Role != domain.RoleArchitecture {
		t.Fatalf("expected architecture, got %s", res2.Role)
	}

	// Verify both roles in DB
	var reqStatus, archStatus string
	_ = writer.QueryRowContext(ctx, "SELECT status FROM role_runs WHERE session_id = 'sess_dup' AND role = 'requirements';").Scan(&reqStatus)
	_ = writer.QueryRowContext(ctx, "SELECT status FROM role_runs WHERE session_id = 'sess_dup' AND role = 'architecture';").Scan(&archStatus)
	if reqStatus != "in_flight" || archStatus != "in_flight" {
		t.Errorf("expected both in_flight, got req=%q, arch=%q", reqStatus, archStatus)
	}
}

// 7. Rollback on Injected Failure
func TestReserveRole_RollbackOnInjectedFailure(t *testing.T) {
	store, writer, t0 := setupReviewingSession(t, "sess_rb", "proj_rb", 30*time.Minute)
	ctx := context.Background()

	injectedErr := errors.New("injected commit failure")
	dispatchBeforeCommitHook = func(ctx context.Context, conn *sql.Conn) error {
		return injectedErr
	}
	t.Cleanup(func() { dispatchBeforeCommitHook = nil })

	res, err := store.ReserveRoleWithNow(ctx, "sess_rb", t0.Add(1*time.Minute))
	if err == nil {
		t.Fatalf("expected error from injected failure, got nil")
	}
	if !errors.Is(err, injectedErr) {
		t.Fatalf("expected errors.Is(err, injectedErr), got: %v", err)
	}
	if res != nil {
		t.Fatalf("expected nil result on error, got %v", res)
	}

	// Verify complete rollback in database: all 4 roles remain pending, started_at is NULL, call_count is 0
	var pendingCount, inFlightCount int
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM role_runs WHERE session_id = 'sess_rb' AND status = 'pending';").Scan(&pendingCount)
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM role_runs WHERE session_id = 'sess_rb' AND status = 'in_flight';").Scan(&inFlightCount)
	if pendingCount != 4 {
		t.Errorf("expected 4 pending roles after rollback, got %d", pendingCount)
	}
	if inFlightCount != 0 {
		t.Errorf("expected 0 in_flight roles after rollback, got %d", inFlightCount)
	}

	var startedAt sql.NullString
	_ = writer.QueryRowContext(ctx, "SELECT started_at FROM role_runs WHERE session_id = 'sess_rb' AND role = 'requirements';").Scan(&startedAt)
	if startedAt.Valid {
		t.Errorf("expected started_at to be NULL after rollback, got %s", startedAt.String)
	}
}

// 8. Untouched Session and Other-Role Fields
func TestReserveRole_UntouchedFields(t *testing.T) {
	store, writer, t0 := setupReviewingSession(t, "sess_untouched", "proj_untouched", 30*time.Minute)
	ctx := context.Background()

	// Read initial session snapshot
	var (
		sStatus          string
		sCancelRequested int
		sClaimedAt       string
		sCutoffAt        string
		sDeadlineAt      string
		sCompletedCount  int
		sIncompleteCount int
		sTermAt          sql.NullString
		sTermReason      sql.NullString
	)
	err := writer.QueryRowContext(ctx, `
		SELECT status, cancel_requested, claimed_at, dispatch_cutoff_at, hard_deadline_at,
		       completed_role_count, incomplete_role_count, terminal_at, terminal_reason
		FROM sessions WHERE id = 'sess_untouched';
	`).Scan(&sStatus, &sCancelRequested, &sClaimedAt, &sCutoffAt, &sDeadlineAt,
		&sCompletedCount, &sIncompleteCount, &sTermAt, &sTermReason)
	if err != nil {
		t.Fatalf("query initial session: %v", err)
	}

	// Reserve 1 role
	res, err := store.ReserveRoleWithNow(ctx, "sess_untouched", t0.Add(1*time.Minute))
	if err != nil || !res.Reserved {
		t.Fatalf("reserve failed: %v", err)
	}

	// Re-verify session fields are 100% untouched
	var (
		sStatus2          string
		sCancelRequested2 int
		sClaimedAt2       string
		sCutoffAt2        string
		sDeadlineAt2      string
		sCompletedCount2  int
		sIncompleteCount2 int
		sTermAt2          sql.NullString
		sTermReason2      sql.NullString
	)
	err = writer.QueryRowContext(ctx, `
		SELECT status, cancel_requested, claimed_at, dispatch_cutoff_at, hard_deadline_at,
		       completed_role_count, incomplete_role_count, terminal_at, terminal_reason
		FROM sessions WHERE id = 'sess_untouched';
	`).Scan(&sStatus2, &sCancelRequested2, &sClaimedAt2, &sCutoffAt2, &sDeadlineAt2,
		&sCompletedCount2, &sIncompleteCount2, &sTermAt2, &sTermReason2)
	if err != nil {
		t.Fatalf("query session after reserve: %v", err)
	}

	if sStatus2 != sStatus {
		t.Errorf("session status changed: %q -> %q", sStatus, sStatus2)
	}
	if sCancelRequested2 != sCancelRequested {
		t.Errorf("cancel_requested changed: %d -> %d", sCancelRequested, sCancelRequested2)
	}
	if sClaimedAt2 != sClaimedAt {
		t.Errorf("claimed_at changed: %q -> %q", sClaimedAt, sClaimedAt2)
	}
	if sCutoffAt2 != sCutoffAt {
		t.Errorf("dispatch_cutoff_at changed: %q -> %q", sCutoffAt, sCutoffAt2)
	}
	if sDeadlineAt2 != sDeadlineAt {
		t.Errorf("hard_deadline_at changed: %q -> %q", sDeadlineAt, sDeadlineAt2)
	}
	if sCompletedCount2 != sCompletedCount || sIncompleteCount2 != sIncompleteCount {
		t.Errorf("role counts changed: (%d, %d) -> (%d, %d)", sCompletedCount, sIncompleteCount, sCompletedCount2, sIncompleteCount2)
	}
	if sTermAt2 != sTermAt || sTermReason2 != sTermReason {
		t.Errorf("terminal fields changed")
	}

	// Verify other roles (Architecture, QA, Security) remain untouched in pending status
	for _, otherRole := range []domain.Role{domain.RoleArchitecture, domain.RoleQA, domain.RoleSecurity} {
		var oStatus string
		var oStartedAt, oCompletedAt, oCause, oErrCat, oErrMsg sql.NullString
		var oCallCount int
		err := writer.QueryRowContext(ctx, `
			SELECT status, started_at, completed_at, cause, error_category, error_message, call_count
			FROM role_runs WHERE session_id = 'sess_untouched' AND role = ?;
		`, otherRole.String()).Scan(&oStatus, &oStartedAt, &oCompletedAt, &oCause, &oErrCat, &oErrMsg, &oCallCount)
		if err != nil {
			t.Fatalf("query role %s: %v", otherRole, err)
		}
		if oStatus != "pending" {
			t.Errorf("role %s status = %q, want pending", otherRole, oStatus)
		}
		if oStartedAt.Valid || oCompletedAt.Valid || oCause.Valid || oErrCat.Valid || oErrMsg.Valid {
			t.Errorf("role %s has unexpected non-null fields", otherRole)
		}
		if oCallCount != 0 {
			t.Errorf("role %s call_count = %d, want 0", otherRole, oCallCount)
		}
	}
}

// 9. Non-Reviewing Sessions and Missing Session
func TestReserveRole_NonReviewingSessions(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	origClock := clock
	clock = func() time.Time { return t0 }
	t.Cleanup(func() { clock = origClock })

	snap := createTestFrozenSnapshot(t, "snap_nonrev")

	t.Run("queued session returns not_reviewing no-work", func(t *testing.T) {
		_, err := store.Submit(ctx, SubmitParams{
			SessionID:      "sess_queued",
			ProjectID:      "proj_nr",
			IdempotencyKey: "key_q",
			Title:          "Queued",
			Content:        "Content",
			Snapshot:       snap,
			CreatedAt:      t0.Add(-5 * time.Minute),
		})
		if err != nil {
			t.Fatalf("submit: %v", err)
		}

		res, err := store.ReserveRole(ctx, "sess_queued")
		if err != nil {
			t.Fatalf("reserve returned error: %v", err)
		}
		if res.Reserved {
			t.Fatalf("expected Reserved == false for queued session")
		}
		if res.NoWorkReason != DispatchNoWorkNotReviewing {
			t.Errorf("expected NoWorkReason == not_reviewing, got %q", res.NoWorkReason)
		}
	})

	t.Run("terminal session returns not_reviewing no-work", func(t *testing.T) {
		// Setup a reviewing session then complete it
		insertSnapshotAndUnit(t, writer, "snap_term", "u_term")
		sessID := "sess_term"
		cutoffStr := formatUTCTimestamp(t0.Add(30 * time.Minute))
		deadlineStr := formatUTCTimestamp(t0.Add(45 * time.Minute))
		claimedStr := formatUTCTimestamp(t0)
		createdStr := formatUTCTimestamp(t0.Add(-5 * time.Minute))

		_, err := writer.ExecContext(ctx, `
			INSERT INTO sessions (
				id, project_id, idempotency_key, request_hash, snapshot_id,
				status, cancel_requested, completed_role_count, incomplete_role_count,
				claimed_at, dispatch_cutoff_at, hard_deadline_at, created_at,
				terminal_at, terminal_reason
			) VALUES (
				?, 'proj_term', 'key_term', ?, 'snap_term',
				'complete', 0, 4, 0,
				?, ?, ?, ?,
				?, 'all_roles_complete'
			);
		`, sessID, testValidHash, claimedStr, cutoffStr, deadlineStr, createdStr, claimedStr)
		if err != nil {
			t.Fatalf("insert terminal session: %v", err)
		}

		res, err := store.ReserveRole(ctx, sessID)
		if err != nil {
			t.Fatalf("reserve returned error: %v", err)
		}
		if res.Reserved {
			t.Fatalf("expected Reserved == false for terminal session")
		}
		if res.NoWorkReason != DispatchNoWorkNotReviewing {
			t.Errorf("expected NoWorkReason == not_reviewing, got %q", res.NoWorkReason)
		}
	})

	t.Run("unknown session returns SessionNotFoundError", func(t *testing.T) {
		res, err := store.ReserveRole(ctx, "non_existent_sess")
		if err == nil {
			t.Fatalf("expected error for unknown session, got nil")
		}
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("expected errors.Is(err, ErrNotFound), got: %v", err)
		}
		var notFound *SessionNotFoundError
		if !errors.As(err, &notFound) {
			t.Errorf("expected errors.As(err, &SessionNotFoundError)")
		} else if notFound.SessionID != "non_existent_sess" {
			t.Errorf("SessionID = %q, want non_existent_sess", notFound.SessionID)
		}
		if res != nil {
			t.Errorf("expected nil result on error, got %v", res)
		}
	})

	t.Run("empty session ID returns SessionNotFoundError", func(t *testing.T) {
		_, err := store.ReserveRole(ctx, "")
		if err == nil || !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound for empty sessionID, got: %v", err)
		}
	})

	t.Run("project mismatch returns SessionNotFoundError", func(t *testing.T) {
		store, _, _ := setupReviewingSession(t, "sess_scope", "proj_correct", 30*time.Minute)
		_, err := store.ReservePendingRoleScoped(ctx, "proj_wrong", "sess_scope", time.Time{})
		if err == nil || !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound for mismatched project, got: %v", err)
		}
	})
}

// 10. Authoritative Predicates Recheck in UPDATE Guard
func TestReserveRole_AuthoritativePredicates(t *testing.T) {
	cutoffOffset := 30 * time.Minute
	ctx := context.Background()

	t.Run("session status changed away from reviewing before UPDATE causes guard conflict", func(t *testing.T) {
		store, _, t0 := setupReviewingSession(t, "sess_auth_status", "proj_auth1", cutoffOffset)

		// Hook changes session status right before role UPDATE
		dispatchBeforeUpdateHook = func(ctx context.Context, conn *sql.Conn, sID string, role domain.Role) error {
			termAt := formatUTCTimestamp(t0.Add(5 * time.Minute))
			_, err := conn.ExecContext(ctx, `
				UPDATE sessions
				SET status = 'failed',
					terminal_at = ?,
					terminal_reason = 'role_failures'
				WHERE id = ?;
			`, termAt, sID)
			return err
		}
		t.Cleanup(func() { dispatchBeforeUpdateHook = nil })

		res, err := store.ReserveRoleWithNow(ctx, "sess_auth_status", t0.Add(1*time.Minute))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Reserved {
			t.Fatalf("expected reservation refused by authoritative predicate")
		}
		if res.NoWorkReason != DispatchNoWorkGuardConflict {
			t.Errorf("expected guard_conflict, got %q", res.NoWorkReason)
		}
	})

	t.Run("in_flight capacity reached before UPDATE causes guard conflict", func(t *testing.T) {
		store, _, t0 := setupReviewingSession(t, "sess_auth_cap", "proj_auth2", cutoffOffset)

		// Hook changes two OTHER roles to in_flight right before this role UPDATE
		dispatchBeforeUpdateHook = func(ctx context.Context, conn *sql.Conn, sID string, role domain.Role) error {
			st := formatUTCTimestamp(t0.Add(2 * time.Minute))
			_, err := conn.ExecContext(ctx, `
				UPDATE role_runs
				SET status = 'in_flight', started_at = ?
				WHERE session_id = ? AND role IN ('qa', 'security');
			`, st, sID)
			return err
		}
		t.Cleanup(func() { dispatchBeforeUpdateHook = nil })

		res, err := store.ReserveRoleWithNow(ctx, "sess_auth_cap", t0.Add(1*time.Minute))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Reserved {
			t.Fatalf("expected reservation refused because in_flight capacity was reached")
		}
		if res.NoWorkReason != DispatchNoWorkGuardConflict {
			t.Errorf("expected guard_conflict, got %q", res.NoWorkReason)
		}
	})

	t.Run("target role status changed from pending before UPDATE causes guard conflict", func(t *testing.T) {
		store, _, t0 := setupReviewingSession(t, "sess_auth_role", "proj_auth3", cutoffOffset)

		// Hook interrupts the target role right before the UPDATE
		dispatchBeforeUpdateHook = func(ctx context.Context, conn *sql.Conn, sID string, role domain.Role) error {
			completedAt := formatUTCTimestamp(t0.Add(2 * time.Minute))
			_, err := conn.ExecContext(ctx, `
				UPDATE role_runs
				SET status = 'interrupted', completed_at = ?, cause = 'user_cancelled'
				WHERE session_id = ? AND role = ?;
			`, completedAt, sID, role.String())
			return err
		}
		t.Cleanup(func() { dispatchBeforeUpdateHook = nil })

		res, err := store.ReserveRoleWithNow(ctx, "sess_auth_role", t0.Add(1*time.Minute))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Reserved {
			t.Fatalf("expected reservation refused because role was already interrupted")
		}
		if res.NoWorkReason != DispatchNoWorkGuardConflict {
			t.Errorf("expected guard_conflict, got %q", res.NoWorkReason)
		}
	})
}

// 11. No Provider or Goroutine Scope (Strict Database-Only Scope)
func TestReserveRole_NoProviderOrGoroutineScope(t *testing.T) {
	store, _, t0 := setupReviewingSession(t, "sess_scope_pure", "proj_sp", 30*time.Minute)
	ctx := context.Background()

	// Reservation must execute synchronously, modify only DB rows, and return.
	res, err := store.ReserveRoleWithNow(ctx, "sess_scope_pure", t0.Add(1*time.Minute))
	if err != nil || !res.Reserved {
		t.Fatalf("reserve failed: %v", err)
	}

	if res.Role != domain.RoleRequirements {
		t.Errorf("expected requirements, got %s", res.Role)
	}
	if res.Status != domain.RoleInFlight {
		t.Errorf("expected status in_flight, got %s", res.Status)
	}
}

func TestReserveRole_SubMillisecondCutoffBeforeBoundary(t *testing.T) {
	store, _, t0 := setupReviewingSession(t, "sess_cutoff_subms", "proj_subms", 1*time.Second)
	cutoff := t0.Add(1 * time.Second)

	res, err := store.ReserveRoleWithNow(context.Background(), "sess_cutoff_subms", cutoff.Add(-1*time.Microsecond))
	if err != nil {
		t.Fatalf("reserve just before cutoff failed: %v", err)
	}
	if !res.Reserved {
		t.Fatalf("expected reservation before cutoff, got no-work reason %q", res.NoWorkReason)
	}
}
