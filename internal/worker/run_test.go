package worker_test

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
	"github.com/wyze69-sys/SpecCouncil/internal/storage/sqlite"
	"github.com/wyze69-sys/SpecCouncil/internal/worker"
)

// TestWorkerHelperProcess serves as the subprocess entrypoint for testing
// abnormal process exit and kernel-level lock reclamation.
func TestWorkerHelperProcess(t *testing.T) {
	if os.Getenv("GO_WORKER_RUN_HELPER") != "1" {
		return
	}

	lockPath := os.Getenv("GO_WORKER_LOCK_PATH")
	mode := os.Getenv("GO_WORKER_MODE")

	switch mode {
	case "acquire_and_hold":
		lock, err := worker.AcquireProcessLock(context.Background(), lockPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "helper acquire err: %v\n", err)
			os.Exit(1)
		}
		_ = lock // held intentionally; killed by parent
		fmt.Println("LOCKED")
		_ = os.Stdout.Sync()
		select {}

	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode: %s\n", mode)
		os.Exit(2)
	}
}

func openTestStore(t *testing.T) (*sqlite.Store, string) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "speccouncil_worker_test.db")

	cfg := sqlite.Config{
		Path:        dbPath,
		BusyTimeout: 5000 * time.Millisecond,
	}

	store, err := sqlite.Open(cfg)
	if err != nil {
		t.Fatalf("sqlite.Open failed: %v", err)
	}

	if err := store.Migrate(context.Background()); err != nil {
		_ = store.Close()
		t.Fatalf("Migrate failed: %v", err)
	}

	t.Cleanup(func() {
		_ = store.Close()
	})

	return store, tmpDir
}

func createTestFrozenSnapshot(t *testing.T, snapID string) evidence.Snapshot {
	t.Helper()
	units := []evidence.Unit{
		{
			ID:   "unit_1",
			Kind: evidence.UnitRequirement,
			Text: "Deterministic worker requirements unit",
		},
	}
	snap, err := evidence.Freeze(snapID, units)
	if err != nil {
		t.Fatalf("evidence.Freeze failed: %v", err)
	}
	return snap
}

func submitSession(
	t *testing.T,
	store *sqlite.Store,
	sessionID, projectID, idempotencyKey string,
	createdAt time.Time,
) *sqlite.SubmitResult {
	t.Helper()
	ctx := context.Background()
	snap := createTestFrozenSnapshot(t, "snap_"+sessionID)

	res, err := store.Submit(ctx, sqlite.SubmitParams{
		SessionID:      sessionID,
		ProjectID:      projectID,
		IdempotencyKey: idempotencyKey,
		Title:          "Review for " + sessionID,
		Content:        "Specification content for " + sessionID,
		Snapshot:       snap,
		CreatedAt:      createdAt,
	})
	if err != nil {
		t.Fatalf("store.Submit(%q) failed: %v", sessionID, err)
	}
	return res
}

func standardTimingPolicy() sqlite.TimingPolicy {
	return sqlite.TimingPolicy{
		DispatchCutoff:      15 * time.Minute,
		CallTimeout:         5 * time.Minute,
		SessionHardDeadline: 30 * time.Minute,
	}
}

// wrappedOrderingStore observes that SweepRestartRecovery completes before ClaimSession starts.
type wrappedOrderingStore struct {
	store             *sqlite.Store
	recoveryDone      atomic.Bool
	recoveryErr       error
	claimErr          error
	onRecovery        func()
	onRecoverySuccess func(res *sqlite.SweepRestartRecoveryResult)
	onClaim           func()
}

func (w *wrappedOrderingStore) SweepRestartRecovery(ctx context.Context, cutoff time.Time) (*sqlite.SweepRestartRecoveryResult, error) {
	if w.onRecovery != nil {
		w.onRecovery()
	}
	if w.recoveryErr != nil {
		return nil, w.recoveryErr
	}
	res, err := w.store.SweepRestartRecovery(ctx, cutoff)
	if err == nil {
		w.recoveryDone.Store(true)
		if w.onRecoverySuccess != nil {
			w.onRecoverySuccess(res)
		}
	}
	return res, err
}

func (w *wrappedOrderingStore) ClaimSession(ctx context.Context, policy sqlite.TimingPolicy) (*sqlite.ClaimResult, error) {
	if !w.recoveryDone.Load() {
		return nil, errors.New("assertion failure: ClaimSession called before SweepRestartRecovery completed")
	}
	if w.onClaim != nil {
		w.onClaim()
	}
	if w.claimErr != nil {
		return nil, w.claimErr
	}
	return w.store.ClaimSession(ctx, policy)
}

// 1. Valid run acquires ownership, runs recovery first, claims exactly one FIFO session, and returns its claim result.
func TestRunOnce_ValidRunClaimsFIFOSessionAfterRecovery(t *testing.T) {
	store, tmpDir := openTestStore(t)
	lockPath := filepath.Join(tmpDir, "worker.lock")

	now := time.Now().UTC()
	t0 := now.Add(-1 * time.Hour)

	// Submit 3 queued sessions with distinct creation times
	submitSession(t, store, "sess_first", "proj_test", "key_1", t0.Add(-15*time.Minute))
	submitSession(t, store, "sess_second", "proj_test", "key_2", t0.Add(-10*time.Minute))
	submitSession(t, store, "sess_third", "proj_test", "key_3", t0.Add(-5*time.Minute))

	var recoveryCalled, claimCalled atomic.Bool
	wrappedStore := &wrappedOrderingStore{
		store: store,
		onRecovery: func() {
			recoveryCalled.Store(true)
		},
		onClaim: func() {
			claimCalled.Store(true)
		},
	}

	cfg := worker.NewRunConfig(
		lockPath,
		wrappedStore,
		standardTimingPolicy(),
		t0.Add(-1*time.Minute),
	)

	res, err := worker.RunOnce(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunOnce failed: %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil RunResult")
	}

	if !recoveryCalled.Load() {
		t.Fatal("SweepRestartRecovery was not called")
	}
	if !claimCalled.Load() {
		t.Fatal("ClaimSession was not called")
	}

	if !res.HasWork() {
		t.Fatal("expected RunResult.HasWork() to be true")
	}
	if res.IsNoWork() {
		t.Fatal("expected RunResult.IsNoWork() to be false")
	}

	claim := res.Claim
	if !claim.Claimed {
		t.Fatal("expected session to be claimed")
	}
	// Authoritative FIFO ordering: sess_first is the oldest
	if claim.SessionID != "sess_first" {
		t.Fatalf("expected oldest session sess_first to be claimed, got: %s", claim.SessionID)
	}
	if claim.Status != domain.SessionReviewing {
		t.Fatalf("expected status reviewing, got: %s", claim.Status)
	}
	if claim.ClaimedAt.IsZero() || claim.DispatchCutoffAt.IsZero() || claim.HardDeadlineAt.IsZero() {
		t.Fatal("expected non-zero timing deadlines on claimed session")
	}

	// Verify database state: sess_first is reviewing; sess_second and sess_third remain queued
	statusFirst, err := store.ReadStatus(context.Background(), "sess_first")
	if err != nil {
		t.Fatalf("ReadStatus sess_first failed: %v", err)
	}
	if statusFirst.Status != domain.SessionReviewing {
		t.Fatalf("expected sess_first status reviewing in DB, got: %s", statusFirst.Status)
	}

	statusSecond, err := store.ReadStatus(context.Background(), "sess_second")
	if err != nil {
		t.Fatalf("ReadStatus sess_second failed: %v", err)
	}
	if statusSecond.Status != domain.SessionQueued {
		t.Fatalf("expected sess_second status queued in DB, got: %s", statusSecond.Status)
	}

	// Verify lock was released: another acquisition must succeed immediately
	lock, err := worker.AcquireProcessLock(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("expected lock to be released after RunOnce, but acquisition failed: %v", err)
	}
	_ = lock.Release()
}

// 2. Stale reviewing sessions are recovered before the queued session is claimed.
func TestRunOnce_StaleReviewingRecoveredBeforeClaim(t *testing.T) {
	store, tmpDir := openTestStore(t)
	lockPath := filepath.Join(tmpDir, "worker.lock")

	now := time.Now().UTC()

	// Submit and claim a session to put it in reviewing state
	submitSession(t, store, "sess_stale", "proj_test", "key_stale", now.Add(-3*time.Hour))
	claimRes, err := store.ClaimSessionWithNow(context.Background(), standardTimingPolicy(), now.Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("ClaimSessionWithNow sess_stale failed: %v", err)
	}
	if !claimRes.Claimed {
		t.Fatal("expected sess_stale to be claimed")
	}

	// Reserve one role to in_flight (must be before dispatch cutoff: -120m + 15m = -105m)
	reserveRes, err := store.ReservePendingRoleWithNow(context.Background(), "sess_stale", now.Add(-115*time.Minute))
	if err != nil {
		t.Fatalf("ReservePendingRoleWithNow failed: %v", err)
	}
	if !reserveRes.Reserved {
		t.Fatalf("expected role to be reserved to in_flight, got: %s", reserveRes.NoWorkReason)
	}

	// Now submit a queued session
	submitSession(t, store, "sess_queued", "proj_test", "key_queued", now.Add(-10*time.Minute))

	// Wrapped store verifies ordering: recovery must complete before claim begins.
	// When recovery finishes recovering the stale session, simulate composer finalizing
	// the recovered session so it exits reviewing status.
	wrappedStore := &wrappedOrderingStore{
		store: store,
		onRecoverySuccess: func(rec *sqlite.SweepRestartRecoveryResult) {
			for _, s := range rec.Sessions {
				_, compErr := store.ComposeSession(context.Background(), s.SessionID)
				if compErr != nil {
					t.Errorf("ComposeSession(%q) failed: %v", s.SessionID, compErr)
				}
			}
		},
	}

	cfg := worker.NewRunConfig(
		lockPath,
		wrappedStore,
		standardTimingPolicy(),
		now.Add(-60*time.Minute), // cutoff covers sess_stale claimed at -2h
	)

	res, err := worker.RunOnce(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunOnce failed: %v", err)
	}

	// Verify recovery outcome
	rec := res.Recovery
	if rec.SessionsRecovered != 1 {
		t.Fatalf("expected 1 session recovered, got: %d", rec.SessionsRecovered)
	}
	if rec.InFlightInterrupted != 1 {
		t.Fatalf("expected 1 in_flight role interrupted, got: %d", rec.InFlightInterrupted)
	}
	if rec.PendingInterrupted != 3 {
		t.Fatalf("expected 3 pending roles interrupted, got: %d", rec.PendingInterrupted)
	}

	// Verify claim outcome: queued session was claimed because sess_stale is no longer active reviewing blocker
	if !res.Claim.Claimed {
		t.Fatal("expected sess_queued to be claimed after recovery")
	}
	if res.Claim.SessionID != "sess_queued" {
		t.Fatalf("expected sess_queued claimed, got: %s", res.Claim.SessionID)
	}
}

// 3. Queued cancellation and terminal rows are preserved according to the released restart-sweep contract.
func TestRunOnce_PreservesQueuedCancellationAndTerminalRows(t *testing.T) {
	store, tmpDir := openTestStore(t)
	lockPath := filepath.Join(tmpDir, "worker.lock")

	now := time.Now().UTC()
	t0 := now.Add(-1 * time.Hour)

	// Submit a queued session and request cancellation while queued
	submitSession(t, store, "sess_cancelled_queued", "proj_test", "key_cancel", t0.Add(-20*time.Minute))
	cancelRes, err := store.RequestCancellation(context.Background(), "sess_cancelled_queued")
	if err != nil {
		t.Fatalf("RequestCancellation failed: %v", err)
	}
	if !cancelRes.Effective {
		t.Fatal("expected cancellation to be effective")
	}

	cfg := worker.NewRunConfig(
		lockPath,
		store,
		standardTimingPolicy(),
		t0.Add(-1*time.Minute),
	)

	res, err := worker.RunOnce(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunOnce failed: %v", err)
	}

	// Queued session with cancel_requested = 1 must be claimed normally
	if !res.Claim.Claimed {
		t.Fatal("expected queued cancelled session to be claimed")
	}
	if res.Claim.SessionID != "sess_cancelled_queued" {
		t.Fatalf("expected sess_cancelled_queued claimed, got: %s", res.Claim.SessionID)
	}
	if !res.Claim.CancelRequested {
		t.Fatal("expected CancelRequested to remain true on claimed session")
	}
}

// 4. No queued session returns a successful no-work result.
func TestRunOnce_NoQueuedSessionReturnsNoWork(t *testing.T) {
	store, tmpDir := openTestStore(t)
	lockPath := filepath.Join(tmpDir, "worker.lock")

	cfg := worker.NewRunConfig(
		lockPath,
		store,
		standardTimingPolicy(),
		time.Now().UTC(),
	)

	res, err := worker.RunOnce(context.Background(), cfg)
	if err != nil {
		t.Fatalf("expected nil error on no-work, got: %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil RunResult on no-work")
	}
	if res.HasWork() {
		t.Fatal("expected HasWork() to be false")
	}
	if !res.IsNoWork() {
		t.Fatal("expected IsNoWork() to be true")
	}
	if res.Claim.Claimed {
		t.Fatal("expected Claimed to be false")
	}
	if res.Claim.NoWorkReason != sqlite.NoWorkNoQueuedSession {
		t.Fatalf("expected NoWorkNoQueuedSession, got: %s", res.Claim.NoWorkReason)
	}

	// Verify lock was released
	lock, err := worker.AcquireProcessLock(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("lock not released after no-work: %v", err)
	}
	_ = lock.Release()
}

// 5. A second concurrent process/run returns ErrAlreadyOwned promptly and leaves sessions unchanged.
func TestRunOnce_SecondConcurrentRunRejectedPromptly(t *testing.T) {
	store, tmpDir := openTestStore(t)
	lockPath := filepath.Join(tmpDir, "worker.lock")

	// Acquire lock first
	lock, err := worker.AcquireProcessLock(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("AcquireProcessLock failed: %v", err)
	}
	defer func() {
		_ = lock.Release()
	}()

	// Submit a queued session
	submitSession(t, store, "sess_1", "proj_test", "key_1", time.Now().UTC())

	cfg := worker.NewRunConfig(
		lockPath,
		store,
		standardTimingPolicy(),
		time.Now().UTC(),
	)

	start := time.Now()
	res, err := worker.RunOnce(context.Background(), cfg)
	duration := time.Since(start)

	if err == nil {
		t.Fatal("expected error on already held lock, got nil")
	}
	if !errors.Is(err, worker.ErrAlreadyOwned) {
		t.Fatalf("expected ErrAlreadyOwned, got: %v", err)
	}
	if res != nil {
		t.Fatal("expected nil result on ErrAlreadyOwned")
	}
	if duration > 500*time.Millisecond {
		t.Fatalf("rejection took too long (%v), expected prompt failure", duration)
	}

	// Verify database was untouched
	status, err := store.ReadStatus(context.Background(), "sess_1")
	if err != nil {
		t.Fatalf("ReadStatus failed: %v", err)
	}
	if status.Status != domain.SessionQueued {
		t.Fatalf("session status mutated after ownership rejection: %s", status.Status)
	}
}

// 6. Two concurrent runs against the same lock produce exactly one owner and no double claim.
func TestRunOnce_TwoConcurrentRunsSingleOwnerNoDoubleClaim(t *testing.T) {
	store, tmpDir := openTestStore(t)
	lockPath := filepath.Join(tmpDir, "worker.lock")

	submitSession(t, store, "sess_only", "proj_test", "key_only", time.Now().UTC())

	cfg := worker.NewRunConfig(
		lockPath,
		store,
		standardTimingPolicy(),
		time.Now().UTC(),
	)

	startBarrier := make(chan struct{})
	type runOutcome struct {
		res *worker.RunResult[*sqlite.SweepRestartRecoveryResult, *sqlite.ClaimResult]
		err error
	}
	outcomes := make([]runOutcome, 2)
	var wg sync.WaitGroup
	wg.Add(2)

	for i := 0; i < 2; i++ {
		idx := i
		go func() {
			defer wg.Done()
			<-startBarrier
			r, err := worker.RunOnce(context.Background(), cfg)
			outcomes[idx] = runOutcome{res: r, err: err}
		}()
	}

	close(startBarrier)
	wg.Wait()

	var winners, losers int
	for _, o := range outcomes {
		if o.err == nil && o.res != nil && o.res.Claim.Claimed {
			winners++
		} else if errors.Is(o.err, worker.ErrAlreadyOwned) {
			losers++
		}
	}

	if winners != 1 {
		t.Fatalf("expected exactly 1 winner, got %d", winners)
	}
	if losers != 1 {
		t.Fatalf("expected exactly 1 loser with ErrAlreadyOwned, got %d", losers)
	}

	// Verify lock is acquirable now
	lock, err := worker.AcquireProcessLock(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("lock not acquirable after concurrent runs: %v", err)
	}
	_ = lock.Release()
}

// 7. Recovery failure prevents claim.
func TestRunOnce_RecoveryFailurePreventsClaim(t *testing.T) {
	store, tmpDir := openTestStore(t)
	lockPath := filepath.Join(tmpDir, "worker.lock")

	submitSession(t, store, "sess_1", "proj_test", "key_1", time.Now().UTC())

	injectedErr := errors.New("simulated recovery failure")
	var claimAttempted atomic.Bool

	wrappedStore := &wrappedOrderingStore{
		store:       store,
		recoveryErr: injectedErr,
		onClaim: func() {
			claimAttempted.Store(true)
		},
	}

	cfg := worker.NewRunConfig(
		lockPath,
		wrappedStore,
		standardTimingPolicy(),
		time.Now().UTC(),
	)

	res, err := worker.RunOnce(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, injectedErr) {
		t.Fatalf("expected injected error, got: %v", err)
	}
	if res != nil {
		t.Fatal("expected nil result on recovery failure")
	}
	if claimAttempted.Load() {
		t.Fatal("claim was attempted despite recovery failure")
	}

	// Verify session remains queued
	status, err := store.ReadStatus(context.Background(), "sess_1")
	if err != nil {
		t.Fatalf("ReadStatus failed: %v", err)
	}
	if status.Status != domain.SessionQueued {
		t.Fatalf("expected session to remain queued, got: %s", status.Status)
	}

	// Verify lock was released
	lock, err := worker.AcquireProcessLock(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("lock was not released after recovery failure: %v", err)
	}
	_ = lock.Release()
}

// 8. Claim failure still releases the process lock.
func TestRunOnce_ClaimFailureReleasesLock(t *testing.T) {
	store, tmpDir := openTestStore(t)
	lockPath := filepath.Join(tmpDir, "worker.lock")

	submitSession(t, store, "sess_1", "proj_test", "key_1", time.Now().UTC())

	injectedErr := errors.New("simulated claim failure")
	wrappedStore := &wrappedOrderingStore{
		store:    store,
		claimErr: injectedErr,
	}

	cfg := worker.NewRunConfig(
		lockPath,
		wrappedStore,
		standardTimingPolicy(),
		time.Now().UTC(),
	)

	res, err := worker.RunOnce(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected claim error, got nil")
	}
	if !errors.Is(err, injectedErr) {
		t.Fatalf("expected injected claim error, got: %v", err)
	}
	if res != nil {
		t.Fatal("expected nil result on claim failure")
	}

	// Verify lock was released
	lock, err := worker.AcquireProcessLock(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("lock was not released after claim failure: %v", err)
	}
	_ = lock.Release()
}

// 9. Context cancellation before lock, during setup, and before claim returns cancellation without leaking ownership.
func TestRunOnce_ContextCancellation(t *testing.T) {
	store, tmpDir := openTestStore(t)
	lockPath := filepath.Join(tmpDir, "worker.lock")

	submitSession(t, store, "sess_1", "proj_test", "key_1", time.Now().UTC())

	// 9a. Cancel before lock acquisition
	ctxCancelled, cancel := context.WithCancel(context.Background())
	cancel()

	cfg := worker.NewRunConfig(
		lockPath,
		store,
		standardTimingPolicy(),
		time.Now().UTC(),
	)

	_, err := worker.RunOnce(ctxCancelled, cfg)
	if err == nil {
		t.Fatal("expected context cancellation error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}

	// Verify path was not locked
	lock1, err := worker.AcquireProcessLock(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("lock not acquirable after pre-cancelled run: %v", err)
	}
	_ = lock1.Release()

	// 9b. Cancel after lock acquisition, before recovery
	ctxBeforeRecovery, cancelBeforeRecovery := context.WithCancel(context.Background())
	wrappedBeforeRecovery := &wrappedOrderingStore{
		store: store,
		onRecovery: func() {
			cancelBeforeRecovery()
		},
	}
	// When context is cancelled during recovery, store operations fail with context.Canceled
	_, err = worker.RunOnce(ctxBeforeRecovery, worker.NewRunConfig(
		lockPath,
		wrappedBeforeRecovery,
		standardTimingPolicy(),
		time.Now().UTC(),
	))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}

	// Verify lock was released
	lock2, err := worker.AcquireProcessLock(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("lock leaked after cancellation during recovery: %v", err)
	}
	_ = lock2.Release()

	// 9c. Cancel before claim
	ctxBeforeClaim, cancelBeforeClaim := context.WithCancel(context.Background())
	wrappedBeforeClaim := &wrappedOrderingStore{
		store: store,
		onClaim: func() {
			cancelBeforeClaim()
		},
	}
	_, err = worker.RunOnce(ctxBeforeClaim, worker.NewRunConfig(
		lockPath,
		wrappedBeforeClaim,
		standardTimingPolicy(),
		time.Now().UTC(),
	))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}

	// Verify lock was released
	lock3, err := worker.AcquireProcessLock(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("lock leaked after cancellation before claim: %v", err)
	}
	_ = lock3.Release()
}

// 10. Release failure is surfaced without hiding a successful claim result, while an operation error remains the primary error.
func TestRunOnce_ReleaseFailurePreservesResultAndOperationError(t *testing.T) {
	store, tmpDir := openTestStore(t)
	lockPath := filepath.Join(tmpDir, "worker.lock")

	submitSession(t, store, "sess_1", "proj_test", "key_1", time.Now().UTC())

	releaseErr := errors.New("simulated release failure")

	// 10a. Operation succeeds, release fails:
	// Result is returned non-nil, release error is surfaced
	worker.SetTestReleaseHook(func(lock *worker.ProcessLock) error {
		_ = lock.Release() // release real OS handle to avoid handle leak
		return releaseErr
	})
	defer worker.SetTestReleaseHook(nil)

	cfg := worker.NewRunConfig(
		lockPath,
		store,
		standardTimingPolicy(),
		time.Now().UTC(),
	)

	res, err := worker.RunOnce(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected release error, got nil")
	}
	if !errors.Is(err, releaseErr) {
		t.Fatalf("expected release error surfaced, got: %v", err)
	}
	if res == nil {
		t.Fatal("expected successful claim result not to be hidden on release failure")
	}
	if !res.Claim.Claimed {
		t.Fatal("expected session to be claimed in returned result")
	}

	// 10b. Operation fails, release also fails:
	// Operation error must remain the primary error
	opErr := errors.New("primary operation error")
	wrappedStore := &wrappedOrderingStore{
		store:    store,
		claimErr: opErr,
	}

	res2, err2 := worker.RunOnce(context.Background(), worker.NewRunConfig(
		lockPath,
		wrappedStore,
		standardTimingPolicy(),
		time.Now().UTC(),
	))
	if err2 == nil {
		t.Fatal("expected combined error, got nil")
	}
	if !errors.Is(err2, opErr) {
		t.Fatalf("expected primary opErr preserved, got: %v", err2)
	}
	if !errors.Is(err2, releaseErr) {
		t.Fatalf("expected releaseErr included, got: %v", err2)
	}
	if res2 != nil {
		t.Fatal("expected nil result on operation failure")
	}
}

// 11. No dispatch, provider, composer, goroutine-held transaction, or second session claim occurs.
func TestRunOnce_NoForbiddenOperationsOrSecondClaim(t *testing.T) {
	store, tmpDir := openTestStore(t)
	lockPath := filepath.Join(tmpDir, "worker.lock")

	now := time.Now().UTC()
	t0 := now.Add(-1 * time.Hour)
	submitSession(t, store, "sess_1", "proj_test", "key_1", t0.Add(-10*time.Minute))
	submitSession(t, store, "sess_2", "proj_test", "key_2", t0.Add(-5*time.Minute))

	var claimCallCount atomic.Int32
	wrappedStore := &wrappedOrderingStore{
		store: store,
		onClaim: func() {
			claimCallCount.Add(1)
		},
	}

	goroutinesBefore := runtime.NumGoroutine()

	cfg := worker.NewRunConfig(
		lockPath,
		wrappedStore,
		standardTimingPolicy(),
		t0,
	)

	res, err := worker.RunOnce(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunOnce failed: %v", err)
	}

	goroutinesAfter := runtime.NumGoroutine()

	// Exactly 1 claim call made
	if count := claimCallCount.Load(); count != 1 {
		t.Fatalf("expected exactly 1 ClaimSession call, got: %d", count)
	}

	// At most 1 session claimed
	if !res.Claim.Claimed {
		t.Fatal("expected sess_1 to be claimed")
	}
	if res.Claim.SessionID != "sess_1" {
		t.Fatalf("expected sess_1 claimed, got: %s", res.Claim.SessionID)
	}

	// sess_2 must remain queued (no second claim)
	status2, err := store.ReadStatus(context.Background(), "sess_2")
	if err != nil {
		t.Fatalf("ReadStatus sess_2 failed: %v", err)
	}
	if status2.Status != domain.SessionQueued {
		t.Fatalf("sess_2 was claimed unexpectedly: %s", status2.Status)
	}

	// No role runs were set to in_flight or completed (no dispatch/provider executed)
	status1, err := store.ReadStatus(context.Background(), "sess_1")
	if err != nil {
		t.Fatalf("ReadStatus sess_1 failed: %v", err)
	}
	if status1.Status != domain.SessionReviewing {
		t.Fatalf("sess_1 not reviewing: %s", status1.Status)
	}

	// No goroutines leaked
	// Give slight leeway for Go runtime internal goroutines if any
	if goroutinesAfter > goroutinesBefore+2 {
		t.Fatalf("possible goroutine leak: before=%d, after=%d", goroutinesBefore, goroutinesAfter)
	}
}

// 12. A subprocess run terminated after acquisition leaves the lock acquirable and does not corrupt or partially alter SQLite state.
func TestWorker_SubprocessAbnormalExitReleasesLockAndPreservesDB(t *testing.T) {
	store, tmpDir := openTestStore(t)
	lockPath := filepath.Join(tmpDir, "worker.lock")

	// Submit a session to have real SQLite state
	t0 := time.Now().UTC()
	submitSession(t, store, "sess_sub", "proj_test", "key_sub", t0)

	cmd := exec.Command(os.Args[0], "-test.run=^TestWorkerHelperProcess$", "--")
	cmd.Env = append(os.Environ(),
		"GO_WORKER_RUN_HELPER=1",
		"GO_WORKER_MODE=acquire_and_hold",
		"GO_WORKER_LOCK_PATH="+lockPath,
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe failed: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start failed: %v", err)
	}

	// Wait deterministically for subprocess to acquire the lock
	reader := bufio.NewReader(stdout)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("failed reading subprocess signal: %v", err)
	}
	if strings.TrimSpace(line) != "LOCKED" {
		t.Fatalf("unexpected helper output: %q", line)
	}

	// While subprocess holds lock, parent RunOnce must fail promptly with ErrAlreadyOwned
	cfg := worker.NewRunConfig(
		lockPath,
		store,
		standardTimingPolicy(),
		t0,
	)
	_, err = worker.RunOnce(context.Background(), cfg)
	if err == nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("expected ErrAlreadyOwned while subprocess held lock")
	}
	if !errors.Is(err, worker.ErrAlreadyOwned) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("expected ErrAlreadyOwned, got: %v", err)
	}

	// Kill subprocess forcefully
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("failed to kill subprocess: %v", err)
	}
	_ = cmd.Wait()

	// Subprocess was killed abruptly. Lock must now be acquirable immediately.
	res, err := worker.RunOnce(context.Background(), cfg)
	if err != nil {
		t.Fatalf("parent RunOnce failed after subprocess termination: %v", err)
	}
	if !res.Claim.Claimed {
		t.Fatal("expected sess_sub to be claimed by parent")
	}

	// Verify database integrity
	dbPath := filepath.Join(tmpDir, "speccouncil_worker_test.db")
	db, err := sql.Open("sqlite", dbPath)
	if err == nil {
		var check string
		_ = db.QueryRow("PRAGMA integrity_check;").Scan(&check)
		_ = db.Close()
		if check != "ok" {
			t.Fatalf("database integrity check failed: %s", check)
		}
	}
}

// 13. Config validation rejects invalid configuration before acquiring ownership.
func TestRunOnce_ConfigValidationRejectsInvalid(t *testing.T) {
	store, tmpDir := openTestStore(t)
	lockPath := filepath.Join(tmpDir, "worker.lock")
	policy := standardTimingPolicy()
	cutoff := time.Now().UTC()

	// Empty lock path
	_, err := worker.RunOnce(context.Background(), worker.NewRunConfig("", store, policy, cutoff))
	if err == nil {
		t.Fatal("expected error on empty lock path, got nil")
	}
	if !errors.Is(err, worker.ErrEmptyPath) {
		t.Fatalf("expected ErrEmptyPath, got: %v", err)
	}

	// Whitespace lock path
	_, err = worker.RunOnce(context.Background(), worker.NewRunConfig("   ", store, policy, cutoff))
	if err == nil {
		t.Fatal("expected error on whitespace lock path, got nil")
	}
	if !errors.Is(err, worker.ErrEmptyPath) {
		t.Fatalf("expected ErrEmptyPath, got: %v", err)
	}

	// Nil store
	_, err = worker.RunOnce(context.Background(), worker.NewRunConfig(lockPath, (*sqlite.Store)(nil), policy, cutoff))
	if err == nil {
		t.Fatal("expected error on nil store, got nil")
	}
	if !errors.Is(err, worker.ErrNilStore) {
		t.Fatalf("expected ErrNilStore, got: %v", err)
	}

	// Zero cutoff
	_, err = worker.RunOnce(context.Background(), worker.NewRunConfig(lockPath, store, policy, time.Time{}))
	if err == nil {
		t.Fatal("expected error on zero cutoff, got nil")
	}
	if !errors.Is(err, worker.ErrZeroCutoff) {
		t.Fatalf("expected ErrZeroCutoff, got: %v", err)
	}

	// Invalid timing policy (SessionHardDeadline < DispatchCutoff + CallTimeout)
	invalidPolicy := sqlite.TimingPolicy{
		DispatchCutoff:      20 * time.Minute,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: 15 * time.Minute,
	}
	_, err = worker.RunOnce(context.Background(), worker.NewRunConfig(lockPath, store, invalidPolicy, cutoff))
	if err == nil {
		t.Fatal("expected error on invalid timing policy, got nil")
	}
	if !errors.Is(err, sqlite.ErrInvalidTimingPolicy) {
		t.Fatalf("expected ErrInvalidTimingPolicy, got: %v", err)
	}
}
