package worker

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"time"
)

var (
	// ErrNilStore is returned when the store provided in RunConfig is nil.
	ErrNilStore = errors.New("worker: store cannot be nil")

	// ErrZeroCutoff is returned when the explicit restart cutoff timestamp is zero.
	ErrZeroCutoff = errors.New("worker: explicit restart cutoff must be non-zero")

	// ErrNilTimingPolicy is returned when the timing policy provided in RunConfig is nil.
	ErrNilTimingPolicy = errors.New("worker: timing policy cannot be nil")
)

// TimingPolicyValidator abstracts any timing policy capable of validating itself.
type TimingPolicyValidator interface {
	Validate() error
}

// SessionStore defines the authoritative persistence operations invoked during a worker run.
// It is directly satisfied by *sqlite.Store without requiring internal/worker to import storage packages.
type SessionStore[TRecovery any, TPolicy any, TClaim any] interface {
	SweepRestartRecovery(ctx context.Context, cutoff time.Time) (TRecovery, error)
	ClaimSession(ctx context.Context, policy TPolicy) (TClaim, error)
}

// RunConfig defines the required configuration parameters for a single worker execution.
type RunConfig[TRecovery any, TPolicy TimingPolicyValidator, TClaim any] struct {
	LockPath     string
	Store        SessionStore[TRecovery, TPolicy, TClaim]
	TimingPolicy TPolicy
	Cutoff       time.Time
}

// NewRunConfig constructs a RunConfig, inferring the generic type parameters from the provided arguments.
func NewRunConfig[TRecovery any, TPolicy TimingPolicyValidator, TClaim any](
	lockPath string,
	store SessionStore[TRecovery, TPolicy, TClaim],
	policy TPolicy,
	cutoff time.Time,
) RunConfig[TRecovery, TPolicy, TClaim] {
	return RunConfig[TRecovery, TPolicy, TClaim]{
		LockPath:     lockPath,
		Store:        store,
		TimingPolicy: policy,
		Cutoff:       cutoff,
	}
}

// RunResult captures the authoritative outcomes of restart recovery and single-session claim.
type RunResult[TRecovery any, TClaim any] struct {
	Recovery TRecovery
	Claim    TClaim
}

type hasWorkInspector interface {
	HasWork() bool
}

type noWorkInspector interface {
	IsNoWork() bool
}

// HasWork reports whether a review session was successfully claimed.
func (r *RunResult[TRecovery, TClaim]) HasWork() bool {
	if r == nil {
		return false
	}
	if hwi, ok := any(r.Claim).(hasWorkInspector); ok {
		return hwi.HasWork()
	}
	return false
}

// IsNoWork reports whether the claim attempt resulted in no work.
func (r *RunResult[TRecovery, TClaim]) IsNoWork() bool {
	if r == nil {
		return true
	}
	if nwi, ok := any(r.Claim).(noWorkInspector); ok {
		return nwi.IsNoWork()
	}
	return false
}

var (
	// testReleaseHook is an internal test seam for simulating lock release failures.
	testReleaseHook func(lock *ProcessLock) error

	// testRecoveryHook is an internal test seam for observing or failing before recovery.
	testRecoveryHook func(ctx context.Context) error

	// testClaimHook is an internal test seam for observing or failing before claim.
	testClaimHook func(ctx context.Context) error
)

// SetTestReleaseHook sets a test seam hook for simulating lock release failures.
func SetTestReleaseHook(hook func(lock *ProcessLock) error) {
	testReleaseHook = hook
}

// RunOnce executes a single worker operation:
//  1. Validates required configuration before acquiring the lock.
//  2. Acquires the OS-backed ProcessLock, returning typed ErrAlreadyOwned promptly on contention.
//  3. Defers lock release on every path, preserving primary operation errors over release failures.
//  4. Executes exactly one SweepRestartRecovery before claiming new work.
//  5. Calls ClaimSession exactly once with the caller's validated timing policy.
//  6. Returns authoritative recovery and claim results without starting loops, goroutines, or provider work.
func RunOnce[TRecovery any, TPolicy TimingPolicyValidator, TClaim any](
	ctx context.Context,
	cfg RunConfig[TRecovery, TPolicy, TClaim],
) (result *RunResult[TRecovery, TClaim], err error) {
	// 1. Validate required configuration before acquiring ownership.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}

	if strings.TrimSpace(cfg.LockPath) == "" {
		return nil, &PathError{
			Op:   "validate_config",
			Path: cfg.LockPath,
			Err:  ErrEmptyPath,
		}
	}

	if isNil(cfg.Store) {
		return nil, ErrNilStore
	}

	if cfg.Cutoff.IsZero() {
		return nil, ErrZeroCutoff
	}

	if isNil(cfg.TimingPolicy) {
		return nil, ErrNilTimingPolicy
	}

	if valErr := cfg.TimingPolicy.Validate(); valErr != nil {
		return nil, valErr
	}

	// 2. Acquire exclusive process ownership.
	lock, lockErr := AcquireProcessLock(ctx, cfg.LockPath)
	if lockErr != nil {
		return nil, lockErr
	}

	// 3. Defer lock release on every path, preserving primary operation error over release error.
	defer func() {
		var relErr error
		if testReleaseHook != nil {
			relErr = testReleaseHook(lock)
		} else {
			relErr = lock.Release()
		}

		if relErr != nil {
			if err != nil {
				err = errors.Join(err, relErr)
			} else {
				err = relErr
			}
		}
	}()

	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}

	// 4. Run exactly one SweepRestartRecovery operation before any claim.
	if testRecoveryHook != nil {
		if hookErr := testRecoveryHook(ctx); hookErr != nil {
			return nil, hookErr
		}
	}

	recoveryResult, recErr := cfg.Store.SweepRestartRecovery(ctx, cfg.Cutoff)
	if recErr != nil {
		return nil, recErr
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}

	// 5. Call ClaimSession exactly once after recovery.
	if testClaimHook != nil {
		if hookErr := testClaimHook(ctx); hookErr != nil {
			return nil, hookErr
		}
	}

	claimResult, claimErr := cfg.Store.ClaimSession(ctx, cfg.TimingPolicy)
	if claimErr != nil {
		return nil, claimErr
	}

	// 6. Return authoritative recovery and claim results (no-work is successful).
	return &RunResult[TRecovery, TClaim]{
		Recovery: recoveryResult,
		Claim:    claimResult,
	}, nil
}

func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	}
	return false
}
