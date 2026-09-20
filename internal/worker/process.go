package worker

import (
	"context"
	"errors"
	"sync"
)

var (
	// ErrNilAttempt is returned when a nil attempt function is provided to RunAttempt or ProcessSupervisor.
	ErrNilAttempt = errors.New("worker: attempt function cannot be nil")

	// ErrAttemptInProgress is returned when concurrent execution is attempted on a ProcessSupervisor instance
	// that already has an attempt in progress.
	ErrAttemptInProgress = errors.New("worker: supervisor attempt already in progress")
)

// AttemptFunc represents a single worker attempt function invoked under a caller-owned context.
// It returns an attempt-specific result and an error.
type AttemptFunc[T any] func(ctx context.Context) (T, error)

// RunAttempt executes exactly one worker attempt under a caller-owned context.
//
// Contract:
//   - The caller owns ctx.
//   - If ctx is already canceled before execution starts, RunAttempt returns ctx.Err()
//     immediately without invoking the attempt function.
//   - If attempt is nil, RunAttempt returns ErrNilAttempt immediately.
//   - The attempt is invoked at most once.
//   - When ctx is canceled while the attempt is running, the cancellation is observable
//     by the attempt via ctx.Done() / ctx.Err(). RunAttempt waits synchronously for the
//     attempt function to return before returning itself.
//   - RunAttempt does not start replacement attempts, daemon loops, or automatic retries.
//   - RunAttempt returns the attempt's result and error without modification, preserving
//     typed errors and sentinel identity (e.g. ErrAlreadyOwned, context.Canceled, context.DeadlineExceeded).
//   - RunAttempt does not acquire process locks, sweep recovery, claim sessions, dispatch roles,
//     invoke providers, publish outcomes, or compose reports directly.
//   - As a stateless, one-shot function, overlapping execution on the same supervisor instance is
//     impossible by construction.
func RunAttempt[T any](ctx context.Context, attempt AttemptFunc[T]) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if attempt == nil {
		return zero, ErrNilAttempt
	}
	return attempt(ctx)
}

// ProcessSupervisor provides a reusable boundary for executing worker attempts one at a time.
// It guarantees that concurrent callers cannot execute overlapping worker attempts on the same instance.
type ProcessSupervisor[T any] struct {
	mu      sync.Mutex
	running bool
}

// NewProcessSupervisor creates a new ProcessSupervisor.
func NewProcessSupervisor[T any]() *ProcessSupervisor[T] {
	return &ProcessSupervisor[T]{}
}

// Running reports whether an attempt is currently in progress.
func (s *ProcessSupervisor[T]) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Run executes a single worker attempt on the supervisor.
// If an attempt is already running on this supervisor instance, Run returns ErrAttemptInProgress
// promptly without starting a second attempt or altering the active attempt.
// After an attempt completes, subsequent sequential attempts are explicitly permitted by this API.
func (s *ProcessSupervisor[T]) Run(ctx context.Context, attempt AttemptFunc[T]) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if attempt == nil {
		return zero, ErrNilAttempt
	}

	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return zero, ErrAttemptInProgress
	}
	s.running = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	return RunAttempt(ctx, attempt)
}

// RunOnceAttempt wraps RunOnce into an AttemptFunc suitable for RunAttempt or ProcessSupervisor.
func RunOnceAttempt[TRecovery any, TPolicy TimingPolicyValidator, TClaim any](
	cfg RunConfig[TRecovery, TPolicy, TClaim],
) AttemptFunc[*RunResult[TRecovery, TClaim]] {
	return func(ctx context.Context) (*RunResult[TRecovery, TClaim], error) {
		return RunOnce(ctx, cfg)
	}
}

// SuperviseAttempt wraps Supervise into an AttemptFunc suitable for RunAttempt or ProcessSupervisor.
func SuperviseAttempt[
	TRecovery any,
	TPolicy TimingPolicyValidator,
	TClaim any,
	TReservation any,
	TStatus any,
	TSuccessParams any,
	TFailureParams any,
	TPublishResult any,
	TComposeResult any,
](
	cfg SuperviseWorkerConfig[TRecovery, TPolicy, TClaim, TReservation, TStatus, TSuccessParams, TFailureParams, TPublishResult, TComposeResult],
) AttemptFunc[*SuperviseResult[TComposeResult]] {
	return func(ctx context.Context) (*SuperviseResult[TComposeResult], error) {
		return Supervise(ctx, cfg)
	}
}
