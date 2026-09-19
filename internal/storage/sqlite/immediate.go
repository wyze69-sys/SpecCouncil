package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"math"
	"time"
)

const (
	defaultMaxRetries     = 5
	maxAllowedRetries     = 100
	defaultInitialBackoff = 10 * time.Millisecond
	defaultMaxBackoff     = 500 * time.Millisecond
	maxAllowedBackoff     = 30 * time.Second
	defaultCleanupTimeout = 5 * time.Second
)

// RetryPolicy defines validated, bounded retry and backoff behavior for immediate transactions.
type RetryPolicy struct {
	Op             string
	MaxRetries     int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	BackoffFactor  float64
}

// DefaultRetryPolicy returns a sensible default retry policy for an operation.
func DefaultRetryPolicy(op string) RetryPolicy {
	return RetryPolicy{
		Op:             op,
		MaxRetries:     defaultMaxRetries,
		InitialBackoff: defaultInitialBackoff,
		MaxBackoff:     defaultMaxBackoff,
		BackoffFactor:  2.0,
	}
}

// Validate checks that retry bounds cannot overflow and backoff parameters are safe.
func (p RetryPolicy) Validate() error {
	if p.MaxRetries < 0 {
		return errors.New("max retries must be non-negative")
	}
	if p.MaxRetries > maxAllowedRetries {
		return fmt.Errorf("max retries %d exceeds maximum allowed bound of %d", p.MaxRetries, maxAllowedRetries)
	}
	if p.InitialBackoff < 0 {
		return errors.New("initial backoff must be non-negative")
	}
	if p.MaxBackoff < p.InitialBackoff {
		return errors.New("max backoff must be greater than or equal to initial backoff")
	}
	if p.MaxBackoff > maxAllowedBackoff {
		return fmt.Errorf("max backoff %v exceeds maximum allowed bound of %v", p.MaxBackoff, maxAllowedBackoff)
	}
	if p.BackoffFactor < 0 {
		return errors.New("backoff factor must be non-negative")
	}
	return nil
}

func (p RetryPolicy) backoffDuration(retryIndex int) time.Duration {
	if p.InitialBackoff <= 0 || retryIndex <= 0 {
		return 0
	}
	factor := p.BackoffFactor
	if factor <= 0 {
		factor = 2.0
	}
	d := float64(p.InitialBackoff) * math.Pow(factor, float64(retryIndex-1))
	if d > float64(p.MaxBackoff) || d > float64(math.MaxInt64) {
		return p.MaxBackoff
	}
	dur := time.Duration(d)
	if dur > p.MaxBackoff {
		return p.MaxBackoff
	}
	return dur
}

// Package-internal testing seams for deterministic verification without wall-clock sleeps.
var (
	sleepWithContext = defaultSleepWithContext
	rollbackHook     func(ctx context.Context, conn *sql.Conn) error
	commitHook       func(ctx context.Context, conn *sql.Conn) error
	attemptHook      func(attempt int)
)

func defaultSleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// poisonConn marks the underlying connection with driver.ErrBadConn and closes it,
// preventing database/sql from returning an uncertain connection to the pool.
func poisonConn(conn *sql.Conn) {
	if conn == nil {
		return
	}
	_ = conn.Raw(func(driverConn any) error {
		if c, ok := driverConn.(io.Closer); ok {
			_ = c.Close()
		}
		return driver.ErrBadConn
	})
}

// withImmediate executes callback within a dedicated SQLite connection transaction
// using explicit BEGIN IMMEDIATE. It applies structured busy/locked retry up to 1 + DB_RETRIES.
//
// The callback is strictly database-only and must never invoke external providers,
// network services, or non-database work.
func withImmediate(ctx context.Context, db *sql.DB, policy RetryPolicy, callback func(conn *sql.Conn) error) error {
	if db == nil {
		return errors.New("database handle cannot be nil")
	}
	if callback == nil {
		return errors.New("transaction callback cannot be nil")
	}
	if err := policy.Validate(); err != nil {
		return fmt.Errorf("validate retry policy: %w", err)
	}

	op := policy.Op
	if op == "" {
		op = "immediate_tx"
	}

	maxAttempts := 1 + policy.MaxRetries
	var lastDriverErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// 1. Check ctx before each attempt.
		if err := ctx.Err(); err != nil {
			return err
		}
		if attemptHook != nil {
			attemptHook(attempt)
		}

		err := runImmediateAttempt(ctx, db, callback)
		if err == nil {
			return nil
		}

		// Context cancellation or deadline exceeded is never retried.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}

		// 10. Do not retry callback/domain/validation/conflict/stale errors, arbitrary driver errors.
		if !isBusyOrLocked(err) {
			return err
		}

		lastDriverErr = err

		// If this was the last attempt, exhaustion is reached.
		if attempt >= maxAttempts {
			break
		}

		// 11. Apply bounded backoff only between retryable attempts, honoring context.
		backoff := policy.backoffDuration(attempt)
		if err := sleepWithContext(ctx, backoff); err != nil {
			return err
		}
	}

	return &PersistenceUnavailable{
		Op:       op,
		Attempts: maxAttempts,
		Err:      lastDriverErr,
	}
}

func runImmediateAttempt(ctx context.Context, db *sql.DB, callback func(conn *sql.Conn) error) (err error) {
	// Check context before acquiring connection.
	if err := ctx.Err(); err != nil {
		return err
	}

	// 2. Acquire a dedicated *sql.Conn; never use a pooled db.BeginTx for this boundary.
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}

	var (
		begun     bool
		committed bool
		poison    bool
	)

	// 6. Roll back after every post-BEGIN failure: callback error or panic, context cancellation,
	// and commit failure. Use a bounded cleanup context. On panic, clean up and repanic with original value.
	// 7. If rollback cannot be confirmed, poison/discard the physical connection.
	// 8. Close the dedicated connection on every path.
	defer func() {
		if r := recover(); r != nil {
			if begun && !committed {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), defaultCleanupTimeout)
				if rollbackHook != nil {
					if rbErr := rollbackHook(cleanupCtx, conn); rbErr != nil {
						poison = true
					}
				} else if _, rbErr := conn.ExecContext(cleanupCtx, "ROLLBACK;"); rbErr != nil {
					poison = true
				}
				cancel()
			}
			if poison {
				poisonConn(conn)
			}
			_ = conn.Close()
			panic(r)
		}

		if begun && !committed {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), defaultCleanupTimeout)
			if rollbackHook != nil {
				if rbErr := rollbackHook(cleanupCtx, conn); rbErr != nil {
					poison = true
				}
			} else if _, rbErr := conn.ExecContext(cleanupCtx, "ROLLBACK;"); rbErr != nil {
				poison = true
			}
			cancel()
		}
		if poison {
			poisonConn(conn)
		}
		_ = conn.Close()
	}()

	// 3. Execute literal BEGIN IMMEDIATE on that dedicated connection.
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE;"); err != nil {
		return err
	}
	begun = true

	// 4. Run the callback using the transaction/connection owned by that attempt.
	if err := callback(conn); err != nil {
		return err
	}

	// 5. Commit only after callback success and context validation.
	if err := ctx.Err(); err != nil {
		return err
	}

	if commitHook != nil {
		if err := commitHook(ctx, conn); err != nil {
			return err
		}
	} else if _, err := conn.ExecContext(ctx, "COMMIT;"); err != nil {
		return err
	}

	// 12. Return success only after a confirmed commit.
	committed = true
	return nil
}

// withImmediate executes callback within an immediate transaction on the store's writer connection.
// The callback is strictly database-only and must never invoke external providers or non-database work.
func (s *Store) withImmediate(ctx context.Context, policy RetryPolicy, callback func(conn *sql.Conn) error) error {
	if s == nil {
		return errors.New("cannot execute immediate transaction on nil store")
	}
	db, err := s.writerDB()
	if err != nil {
		return err
	}
	return withImmediate(ctx, db, policy, callback)
}

// withImmediateOp is a convenience helper specifying the operation name explicitly.
func (s *Store) withImmediateOp(ctx context.Context, op string, policy RetryPolicy, callback func(conn *sql.Conn) error) error {
	policy.Op = op
	return s.withImmediate(ctx, policy, callback)
}
