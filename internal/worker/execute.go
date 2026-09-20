// Package worker provides the worker execution seam for SpecCouncil review roles.
package worker

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
	"github.com/wyze69-sys/SpecCouncil/internal/provider"
	"github.com/wyze69-sys/SpecCouncil/internal/review"
)

var (
	// ErrInvalidRole is returned when the given role is not a canonical v1 role.
	ErrInvalidRole = errors.New("worker: invalid role")

	// ErrEmptySnapshot is returned when the snapshot has no units.
	ErrEmptySnapshot = errors.New("worker: snapshot must have at least one unit")

	// ErrNilProvider is returned when no provider is supplied.
	ErrNilProvider = errors.New("worker: provider cannot be nil")

	// ErrZeroCallTimeout is returned when CallTimeout is zero or negative.
	ErrZeroCallTimeout = errors.New("worker: CallTimeout must be positive")

	// ErrZeroHardDeadline is returned when HardDeadlineAt is a zero time value.
	ErrZeroHardDeadline = errors.New("worker: HardDeadlineAt must be non-zero")
)

// ExecuteConfig carries all inputs required to execute one in-flight role.
//
// No SQLite, publication, composer, dispatcher, or second-role state is
// touched by Execute.
type ExecuteConfig struct {
	// Role is the canonical role to execute (must be a valid domain.Role).
	Role domain.Role

	// Snapshot is the immutable frozen evidence all four roles share.
	Snapshot evidence.Snapshot

	// Provider is the single adapter boundary that makes provider calls.
	Provider provider.Provider

	// Budget controls the prompt token cap.  A zero Budget disables the check.
	Budget review.Budget

	// Policy holds the unresolved engine choices for format-repair failures.
	Policy review.Policy

	// CallTimeout is the maximum duration of one provider attempt.
	// Must be positive.
	CallTimeout time.Duration

	// HardDeadlineAt is the absolute instant after which no new provider
	// attempt may start.  Must be non-zero.
	HardDeadlineAt time.Time

	// BackoffMin is the lower bound of the jitter sleep between retry attempts.
	// Zero means no backoff.
	BackoffMin time.Duration

	// BackoffMax is the upper bound of the jitter sleep between retry attempts.
	BackoffMax time.Duration

	// Now returns the current time.  Nil means time.Now.  Injected for tests.
	Now func() time.Time

	// Sleep performs a bounded backoff sleep.  Nil uses the timer-based
	// default.  Injected for tests.
	Sleep func(ctx context.Context, d time.Duration) error

	// Jitter selects a duration in [BackoffMin, BackoffMax].  Nil uses the
	// midpoint (deterministic, test-safe).
	Jitter func(min, max time.Duration) time.Duration
}

// validate checks that the ExecuteConfig is complete and consistent.
func (c ExecuteConfig) validate() error {
	if !domain.IsValidRole(c.Role) {
		return ErrInvalidRole
	}
	if len(c.Snapshot.Units) == 0 {
		return ErrEmptySnapshot
	}
	if isNil(c.Provider) {
		return ErrNilProvider
	}
	if c.CallTimeout <= 0 {
		return ErrZeroCallTimeout
	}
	if c.HardDeadlineAt.IsZero() {
		return ErrZeroHardDeadline
	}
	return nil
}

// Execute runs the role described by cfg against the provider with per-attempt
// deadline enforcement and the canonical two-call budget.
//
// It returns a terminal RoleOutcome.  It does not persist findings, publish
// results, compose a report, start another role, or open a SQLite transaction.
//
// All per-attempt contexts are child contexts of ctx so that cancellation of
// the parent cancels blocked provider calls without leaking goroutines.
func Execute(ctx context.Context, cfg ExecuteConfig) (review.RoleOutcome, error) {
	if strings.TrimSpace(string(cfg.Role)) == "" {
		return review.RoleOutcome{}, ErrInvalidRole
	}
	if err := cfg.validate(); err != nil {
		return review.RoleOutcome{}, err
	}

	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	// Refuse to start if the hard deadline has already passed.
	if !nowFn().Before(cfg.HardDeadlineAt) {
		return review.RoleOutcome{
			Role:          cfg.Role,
			Status:        domain.RoleFailed,
			ErrorCategory: domain.ErrTimeout,
		}, nil
	}

	timing := review.CallTiming{
		CallTimeout:    cfg.CallTimeout,
		HardDeadlineAt: cfg.HardDeadlineAt,
		BackoffMin:     cfg.BackoffMin,
		BackoffMax:     cfg.BackoffMax,
		Now:            nowFn,
		Sleep:          cfg.Sleep,
		Jitter:         cfg.Jitter,
	}

	out := review.RunRole(
		ctx,
		cfg.Provider,
		cfg.Role,
		cfg.Snapshot,
		cfg.Budget,
		cfg.Policy,
		timing,
	)
	return out, nil
}
