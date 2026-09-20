package worker_test

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/storage/sqlite"
	"github.com/wyze69-sys/SpecCouncil/internal/worker"
)

// 1. One attempt: normal attempt invoked exactly once and its result returned.
func TestProcess_OneAttempt(t *testing.T) {
	ctx := context.Background()

	// 1a. One-shot RunAttempt
	var attempts atomic.Int32
	attempt := func(ctx context.Context) (string, error) {
		attempts.Add(1)
		return "attempt-result-1", nil
	}

	res, err := worker.RunAttempt(ctx, attempt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != "attempt-result-1" {
		t.Fatalf("expected 'attempt-result-1', got %q", res)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("expected exactly 1 attempt, got %d", got)
	}

	// 1b. Reusable ProcessSupervisor
	supervisor := worker.NewProcessSupervisor[string]()
	res2, err2 := supervisor.Run(ctx, attempt)
	if err2 != nil {
		t.Fatalf("unexpected supervisor error: %v", err2)
	}
	if res2 != "attempt-result-1" {
		t.Fatalf("expected 'attempt-result-1', got %q", res2)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("expected 2 total attempts across runs, got %d", got)
	}
}

// 2. Nil/invalid input: rejected before any work starts using typed error.
func TestProcess_NilAttempt(t *testing.T) {
	ctx := context.Background()

	// 2a. RunAttempt with nil attempt
	res, err := worker.RunAttempt[string](ctx, nil)
	if res != "" {
		t.Fatalf("expected zero result, got %q", res)
	}
	if !errors.Is(err, worker.ErrNilAttempt) {
		t.Fatalf("expected ErrNilAttempt, got %v", err)
	}

	// 2b. ProcessSupervisor with nil attempt
	supervisor := worker.NewProcessSupervisor[string]()
	res2, err2 := supervisor.Run(ctx, nil)
	if res2 != "" {
		t.Fatalf("expected zero result, got %q", res2)
	}
	if !errors.Is(err2, worker.ErrNilAttempt) {
		t.Fatalf("expected ErrNilAttempt, got %v", err2)
	}
	if supervisor.Running() {
		t.Fatalf("supervisor should not be running after rejected nil attempt")
	}
}

// 3. Already canceled: canceled context invokes no attempt and returns context error.
func TestProcess_AlreadyCanceled(t *testing.T) {
	var invoked atomic.Bool
	attempt := func(ctx context.Context) (string, error) {
		invoked.Store(true)
		return "should-not-run", nil
	}

	// 3a. Pre-canceled context
	ctxCanceled, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := worker.RunAttempt(ctxCanceled, attempt)
	if res != "" {
		t.Fatalf("expected empty result, got %q", res)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if invoked.Load() {
		t.Fatalf("attempt must not be invoked on pre-canceled context")
	}

	// 3b. Pre-expired deadline context
	ctxExpired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-100*time.Millisecond))
	defer cancelExpired()

	invoked.Store(false)
	res, err = worker.RunAttempt(ctxExpired, attempt)
	if res != "" {
		t.Fatalf("expected empty result, got %q", res)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
	if invoked.Load() {
		t.Fatalf("attempt must not be invoked on pre-expired context")
	}

	// 3c. ProcessSupervisor on pre-canceled context
	supervisor := worker.NewProcessSupervisor[string]()
	invoked.Store(false)
	res, err = supervisor.Run(ctxCanceled, attempt)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled from supervisor, got %v", err)
	}
	if invoked.Load() {
		t.Fatalf("supervisor must not invoke attempt on pre-canceled context")
	}
	if supervisor.Running() {
		t.Fatalf("supervisor must not record running state on canceled context")
	}
}

// 4. Cancellation delivery: blocked cooperative attempt observes cancellation and returns.
func TestProcess_CancellationDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	attemptStarted := make(chan struct{})
	attemptSawCancel := make(chan struct{})

	attempt := func(attemptCtx context.Context) (string, error) {
		close(attemptStarted)
		select {
		case <-attemptCtx.Done():
			close(attemptSawCancel)
			return "", attemptCtx.Err()
		}
	}

	doneCh := make(chan error, 1)
	go func() {
		_, err := worker.RunAttempt(ctx, attempt)
		doneCh <- err
	}()

	<-attemptStarted
	cancel()
	<-attemptSawCancel

	err := <-doneCh
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// 5. Wait-before-return: W6 does not return until the attempt has finished.
func TestProcess_WaitBeforeReturn(t *testing.T) {
	ctx := context.Background()

	attemptStarted := make(chan struct{})
	releaseAttempt := make(chan struct{})
	attemptFinished := make(chan struct{})
	supervisorReturned := make(chan struct{})

	var finishedTimestamp time.Time
	var supervisorTimestamp time.Time

	attempt := func(attemptCtx context.Context) (string, error) {
		close(attemptStarted)
		<-releaseAttempt
		finishedTimestamp = time.Now()
		close(attemptFinished)
		return "finished", nil
	}

	go func() {
		_, _ = worker.RunAttempt(ctx, attempt)
		supervisorTimestamp = time.Now()
		close(supervisorReturned)
	}()

	<-attemptStarted

	// Verify supervisor has not returned while attempt is still blocked
	select {
	case <-supervisorReturned:
		t.Fatalf("supervisor returned prematurely while attempt was still blocked")
	default:
	}

	// Release attempt and verify ordering
	close(releaseAttempt)
	<-attemptFinished
	<-supervisorReturned

	if supervisorTimestamp.Before(finishedTimestamp) {
		t.Fatalf("supervisor returned at %v before attempt finished at %v",
			supervisorTimestamp, finishedTimestamp)
	}
}

// 6. No replacement: cancellation causes zero additional attempts.
func TestProcess_NoReplacement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var attempts atomic.Int32
	attemptStarted := make(chan struct{})

	attempt := func(attemptCtx context.Context) (string, error) {
		attempts.Add(1)
		close(attemptStarted)
		<-attemptCtx.Done()
		return "", attemptCtx.Err()
	}

	doneCh := make(chan error, 1)
	go func() {
		_, err := worker.RunAttempt(ctx, attempt)
		doneCh <- err
	}()

	<-attemptStarted
	cancel()

	err := <-doneCh
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	if got := attempts.Load(); got != 1 {
		t.Fatalf("expected exactly 1 attempt invoked, got %d", got)
	}
}

// 7. No-work preservation: fake attempt returns successful no-work result; W6 returns success without rewriting.
func TestProcess_NoWorkPreservation(t *testing.T) {
	ctx := context.Background()

	type fakeNoWorkResult struct {
		Claimed      bool
		NoWorkReason string
	}

	noWorkVal := fakeNoWorkResult{
		Claimed:      false,
		NoWorkReason: "NoWorkNoQueuedSession",
	}

	attempt := func(attemptCtx context.Context) (fakeNoWorkResult, error) {
		return noWorkVal, nil
	}

	res, err := worker.RunAttempt(ctx, attempt)
	if err != nil {
		t.Fatalf("unexpected error on no-work attempt: %v", err)
	}
	if res.Claimed {
		t.Fatalf("expected Claimed=false, got true")
	}
	if res.NoWorkReason != "NoWorkNoQueuedSession" {
		t.Fatalf("expected NoWorkNoQueuedSession, got %q", res.NoWorkReason)
	}
}

// 8. Error identity: sentinel or typed worker error is preserved with errors.Is and errors.As.
func TestProcess_ErrorIdentity(t *testing.T) {
	ctx := context.Background()

	var errSentinel = errors.New("custom sentinel error")

	var customTyped = &typedError{Code: 42, Msg: "typed failure"}

	// 8a. Sentinel error preservation
	_, err := worker.RunAttempt(ctx, func(c context.Context) (string, error) {
		return "", errSentinel
	})
	if !errors.Is(err, errSentinel) {
		t.Fatalf("expected errors.Is match for errSentinel, got %v", err)
	}

	// 8b. Typed error preservation
	_, err = worker.RunAttempt(ctx, func(c context.Context) (string, error) {
		return "", customTyped
	})
	var target *typedError
	if !errors.As(err, &target) {
		t.Fatalf("expected errors.As match for *typedError, got %v", err)
	}
	if target.Code != 42 || target.Msg != "typed failure" {
		t.Fatalf("typed error contents mismatch: %+v", target)
	}

	// 8c. worker.ErrAlreadyOwned preservation
	_, err = worker.RunAttempt(ctx, func(c context.Context) (string, error) {
		return "", worker.ErrAlreadyOwned
	})
	if !errors.Is(err, worker.ErrAlreadyOwned) {
		t.Fatalf("expected errors.Is match for ErrAlreadyOwned, got %v", err)
	}
}

type typedError struct {
	Code int
	Msg  string
}

func (e *typedError) Error() string {
	return e.Msg
}

// 9. Deadline identity: deadline error is not rewritten as generic failure.
func TestProcess_DeadlineIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	attempt := func(attemptCtx context.Context) (string, error) {
		select {
		case <-attemptCtx.Done():
			return "", attemptCtx.Err()
		}
	}

	res, err := worker.RunAttempt(ctx, attempt)
	if res != "" {
		t.Fatalf("expected empty result on deadline expiry, got %q", res)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
}

// 10. Concurrent-call behavior:
// 10a. One-shot function: independent callers do not share state.
// 10b. Supervisor object: rejects concurrent attempt with typed error, preserves active attempt, allows sequential reuse.
func TestProcess_ConcurrentCalls(t *testing.T) {
	ctx := context.Background()

	// 10a. One-shot RunAttempt independence: concurrent callers run distinct attempts safely.
	const oneShotCallers = 8
	var wg sync.WaitGroup
	var completedCount atomic.Int32

	for i := 0; i < oneShotCallers; i++ {
		wg.Add(1)
		callerID := i
		go func() {
			defer wg.Done()
			res, err := worker.RunAttempt(ctx, func(c context.Context) (int, error) {
				return callerID, nil
			})
			if err != nil || res != callerID {
				t.Errorf("caller %d failed: res=%d, err=%v", callerID, res, err)
			}
			completedCount.Add(1)
		}()
	}
	wg.Wait()
	if got := completedCount.Load(); got != oneShotCallers {
		t.Fatalf("expected %d one-shot attempts completed, got %d", oneShotCallers, got)
	}

	// 10b. ProcessSupervisor rejects overlapping attempts.
	supervisor := worker.NewProcessSupervisor[string]()

	attempt1Started := make(chan struct{})
	releaseAttempt1 := make(chan struct{})
	var attempt2Invoked atomic.Bool

	attempt1 := func(c context.Context) (string, error) {
		close(attempt1Started)
		<-releaseAttempt1
		return "attempt-1-success", nil
	}

	attempt2 := func(c context.Context) (string, error) {
		attempt2Invoked.Store(true)
		return "attempt-2-should-not-run", nil
	}

	attempt1Done := make(chan error, 1)
	var attempt1Res string
	go func() {
		var aErr error
		attempt1Res, aErr = supervisor.Run(ctx, attempt1)
		attempt1Done <- aErr
	}()

	<-attempt1Started

	if !supervisor.Running() {
		t.Fatalf("expected supervisor.Running() == true while attempt 1 is active")
	}

	// Concurrent caller 2 tries to run while attempt 1 is active.
	res2, err2 := supervisor.Run(ctx, attempt2)
	if res2 != "" {
		t.Fatalf("expected empty result from rejected caller 2, got %q", res2)
	}
	if !errors.Is(err2, worker.ErrAttemptInProgress) {
		t.Fatalf("expected ErrAttemptInProgress for concurrent caller, got %v", err2)
	}
	if attempt2Invoked.Load() {
		t.Fatalf("attempt 2 must never be invoked when concurrent attempt is active")
	}

	// Release attempt 1 and verify it completed normally without being canceled by caller 2.
	close(releaseAttempt1)
	err1 := <-attempt1Done
	if err1 != nil {
		t.Fatalf("unexpected error for attempt 1: %v", err1)
	}
	if attempt1Res != "attempt-1-success" {
		t.Fatalf("expected 'attempt-1-success', got %q", attempt1Res)
	}

	if supervisor.Running() {
		t.Fatalf("expected supervisor.Running() == false after attempt 1 completed")
	}

	// Subsequent sequential call is explicitly permitted and succeeds.
	var attempt3Invoked atomic.Bool
	attempt3 := func(c context.Context) (string, error) {
		attempt3Invoked.Store(true)
		return "attempt-3-success", nil
	}

	res3, err3 := supervisor.Run(ctx, attempt3)
	if err3 != nil {
		t.Fatalf("unexpected error for sequential attempt 3: %v", err3)
	}
	if res3 != "attempt-3-success" {
		t.Fatalf("expected 'attempt-3-success', got %q", res3)
	}
	if !attempt3Invoked.Load() {
		t.Fatalf("attempt 3 was not invoked")
	}
}

// 11. Lock boundary: integration with released attempt path proves W6 does not acquire
// a second lock and existing lock contention remains typed.
func TestProcess_LockBoundary(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "lock_boundary_test.db")
	lockPath := filepath.Join(tmpDir, "worker.lock")

	store, err := sqlite.Open(sqlite.Config{
		Path:        dbPath,
		BusyTimeout: 5000 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("sqlite.Open failed: %v", err)
	}
	defer store.Close()

	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("store.Migrate failed: %v", err)
	}

	timingPolicy := sqlite.TimingPolicy{
		DispatchCutoff:      5 * time.Second,
		CallTimeout:         2 * time.Second,
		SessionHardDeadline: 10 * time.Second,
	}

	cfg := worker.NewRunConfig(
		lockPath,
		store,
		timingPolicy,
		time.Now().UTC().Add(-time.Hour),
	)

	ctx := context.Background()

	// 11a. RunAttempt executes RunOnceAttempt: acquires and releases lock, returns no-work on clean db.
	attempt := worker.RunOnceAttempt(cfg)
	res, err := worker.RunAttempt(ctx, attempt)
	if err != nil {
		t.Fatalf("unexpected error on RunAttempt(RunOnceAttempt): %v", err)
	}
	if res == nil {
		t.Fatalf("expected non-nil RunResult")
	}
	if !res.IsNoWork() {
		t.Fatalf("expected clean DB claim to be no-work")
	}

	// 11b. Lock contention test: hold lock outside, verify RunAttempt surfaces ErrAlreadyOwned.
	outsideLock, err := worker.AcquireProcessLock(ctx, lockPath)
	if err != nil {
		t.Fatalf("AcquireProcessLock failed: %v", err)
	}

	resContention, errContention := worker.RunAttempt(ctx, worker.RunOnceAttempt(cfg))
	if resContention != nil {
		t.Fatalf("expected nil result on contention, got %v", resContention)
	}
	if !errors.Is(errContention, worker.ErrAlreadyOwned) {
		t.Fatalf("expected ErrAlreadyOwned on lock contention, got %v", errContention)
	}

	// Release outside lock; subsequent attempt must acquire successfully.
	if err := outsideLock.Release(); err != nil {
		t.Fatalf("outsideLock.Release failed: %v", err)
	}

	resAfter, errAfter := worker.RunAttempt(ctx, worker.RunOnceAttempt(cfg))
	if errAfter != nil {
		t.Fatalf("expected success after lock released, got %v", errAfter)
	}
	if resAfter == nil || !resAfter.IsNoWork() {
		t.Fatalf("expected valid no-work result after lock release")
	}
}

// 12. Repeated lifecycle: repeated calls across success, no-work, cancellation, and error
// do not leak goroutines or channels.
func TestProcess_RepeatedLifecycle(t *testing.T) {
	ctx := context.Background()
	supervisor := worker.NewProcessSupervisor[int]()

	// Settle goroutines
	runtime.GC()
	initialGoroutines := runtime.NumGoroutine()

	const iterations = 50
	for i := 0; i < iterations; i++ {
		step := i % 4
		switch step {
		case 0:
			// Success
			val := i
			res, err := worker.RunAttempt(ctx, func(c context.Context) (int, error) {
				return val, nil
			})
			if err != nil || res != val {
				t.Fatalf("success step failed: res=%d, err=%v", res, err)
			}
		case 1:
			// No-work representation
			res, err := supervisor.Run(ctx, func(c context.Context) (int, error) {
				return 0, nil
			})
			if err != nil || res != 0 {
				t.Fatalf("no-work step failed: res=%d, err=%v", res, err)
			}
		case 2:
			// Pre-canceled
			cCtx, cancel := context.WithCancel(ctx)
			cancel()
			_, err := worker.RunAttempt(cCtx, func(c context.Context) (int, error) {
				return -1, nil
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled step failed: %v", err)
			}
		case 3:
			// Errored attempt
			sentinel := errors.New("cycle failure")
			_, err := supervisor.Run(ctx, func(c context.Context) (int, error) {
				return -1, sentinel
			})
			if !errors.Is(err, sentinel) {
				t.Fatalf("error step failed: %v", err)
			}
		}
	}

	runtime.GC()
	finalGoroutines := runtime.NumGoroutine()
	// Allow small runtime variance (<= 4), but no systematic leaks.
	if finalGoroutines > initialGoroutines+4 {
		t.Fatalf("possible goroutine leak: initial=%d, final=%d", initialGoroutines, finalGoroutines)
	}
}

// 13. Race safety: concurrent attempts and concurrent contention under race detector.
func TestProcess_RaceStress(t *testing.T) {
	ctx := context.Background()
	supervisor := worker.NewProcessSupervisor[int]()

	const workers = 16
	const rounds = 20

	var wg sync.WaitGroup
	wg.Add(workers)

	for w := 0; w < workers; w++ {
		go func(workerID int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				// One-shot function run
				val := workerID*rounds + r
				res, err := worker.RunAttempt(ctx, func(c context.Context) (int, error) {
					return val, nil
				})
				if err != nil || res != val {
					t.Errorf("one-shot race failed for worker %d: %v", workerID, err)
				}

				// Contention on supervisor
				_, sErr := supervisor.Run(ctx, func(c context.Context) (int, error) {
					return val, nil
				})
				if sErr != nil && !errors.Is(sErr, worker.ErrAttemptInProgress) {
					t.Errorf("unexpected supervisor error: %v", sErr)
				}
			}
		}(w)
	}

	wg.Wait()
}
