package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
)

// ErrInvalidTimingPolicy is returned when a TimingPolicy violates timing constraints,
// has non-positive durations, or overflows duration arithmetic.
var ErrInvalidTimingPolicy = errors.New("invalid timing policy")

// TimingPolicy defines validated durations for session execution and dispatch boundaries.
// Numeric values must be configuration-driven; no production defaults are assumed.
type TimingPolicy struct {
	DispatchCutoff      time.Duration
	CallTimeout         time.Duration
	SessionHardDeadline time.Duration
}

// Validate verifies that all durations are positive, bounded against overflow,
// and satisfy SessionHardDeadline >= DispatchCutoff + CallTimeout.
func (p TimingPolicy) Validate() error {
	if p.DispatchCutoff <= 0 {
		return fmt.Errorf("%w: dispatch cutoff must be positive (got %v)", ErrInvalidTimingPolicy, p.DispatchCutoff)
	}
	if p.CallTimeout <= 0 {
		return fmt.Errorf("%w: call timeout must be positive (got %v)", ErrInvalidTimingPolicy, p.CallTimeout)
	}
	if p.SessionHardDeadline <= 0 {
		return fmt.Errorf("%w: session hard deadline must be positive (got %v)", ErrInvalidTimingPolicy, p.SessionHardDeadline)
	}
	// Detect int64 duration overflow when adding DispatchCutoff + CallTimeout.
	if p.DispatchCutoff > math.MaxInt64-p.CallTimeout {
		return fmt.Errorf("%w: dispatch cutoff and call timeout overflow duration limits", ErrInvalidTimingPolicy)
	}
	minDeadline := p.DispatchCutoff + p.CallTimeout
	if p.SessionHardDeadline < minDeadline {
		return fmt.Errorf("%w: session hard deadline (%v) must be >= dispatch cutoff (%v) + call timeout (%v) = %v",
			ErrInvalidTimingPolicy, p.SessionHardDeadline, p.DispatchCutoff, p.CallTimeout, minDeadline)
	}
	return nil
}

// NoWorkReason describes why a claim attempt found no session to claim.
type NoWorkReason string

const (
	NoWorkNone            NoWorkReason = ""
	NoWorkActiveReviewing NoWorkReason = "active_session_reviewing"
	NoWorkNoQueuedSession NoWorkReason = "no_queued_session"
	NoWorkGuardConflict   NoWorkReason = "guard_conflict"

	// Aliases for convenience.
	NoWorkAlreadyActive = NoWorkActiveReviewing
	NoWorkEmptyQueue    = NoWorkNoQueuedSession
)

// ClaimResult represents the authoritative outcome of an atomic session claim attempt.
type ClaimResult struct {
	Claimed          bool                 `json:"claimed"`
	NoWorkReason     NoWorkReason         `json:"no_work_reason,omitempty"`
	SessionID        string               `json:"session_id,omitempty"`
	ProjectID        string               `json:"project_id,omitempty"`
	IdempotencyKey   string               `json:"idempotency_key,omitempty"`
	RequestHash      string               `json:"request_hash,omitempty"`
	SnapshotID       string               `json:"snapshot_id,omitempty"`
	Status           domain.SessionStatus `json:"status,omitempty"`
	CancelRequested  bool                 `json:"cancel_requested,omitempty"`
	ClaimedAt        time.Time            `json:"claimed_at,omitempty"`
	DispatchCutoffAt time.Time            `json:"dispatch_cutoff_at,omitempty"`
	HardDeadlineAt   time.Time            `json:"hard_deadline_at,omitempty"`
}

// IsNoWork reports whether the claim attempt resulted in no work.
func (r *ClaimResult) IsNoWork() bool {
	return r == nil || !r.Claimed
}

// HasWork reports whether a session was successfully claimed.
func (r *ClaimResult) HasWork() bool {
	return r != nil && r.Claimed
}

// ClaimedAtPtr returns a pointer to ClaimedAt if claimed, or nil.
func (r *ClaimResult) ClaimedAtPtr() *time.Time {
	if r == nil || !r.Claimed {
		return nil
	}
	t := r.ClaimedAt
	return &t
}

// DispatchCutoffAtPtr returns a pointer to DispatchCutoffAt if claimed, or nil.
func (r *ClaimResult) DispatchCutoffAtPtr() *time.Time {
	if r == nil || !r.Claimed {
		return nil
	}
	t := r.DispatchCutoffAt
	return &t
}

// HardDeadlineAtPtr returns a pointer to HardDeadlineAt if claimed, or nil.
func (r *ClaimResult) HardDeadlineAtPtr() *time.Time {
	if r == nil || !r.Claimed {
		return nil
	}
	t := r.HardDeadlineAt
	return &t
}

// claimBeforeCommitHook allows deterministic test verification of rollback on injected failure.
var claimBeforeCommitHook func(ctx context.Context, conn *sql.Conn) error

// claimBeforeUpdateHook allows deterministic test verification of zero rows affected / conflict.
var claimBeforeUpdateHook func(ctx context.Context, conn *sql.Conn, targetID string) error

// ClaimSession attempts to atomically claim the oldest queued session according to FIFO ordering
// (created_at ASC, id ASC) within a single immediate transaction.
//
// Rules enforced:
//  1. Refuses to claim while any session in the database is in 'reviewing' status; returns a typed
//     no-work result (Claimed: false, NoWorkReason: NoWorkActiveReviewing), not an error.
//  2. Selects the oldest queued session by created_at ASC, id ASC (including sessions with cancel_requested = true).
//  3. Atomically updates exactly one queued session to 'reviewing' and records:
//     - claimed_at = now
//     - dispatch_cutoff_at = now + DispatchCutoff
//     - hard_deadline_at = now + SessionHardDeadline
//  4. Sets no role to in_flight and performs no provider/worker/API work.
//  5. If the guarded update affects zero rows, returns typed no-work and leaves state unchanged.
//  6. Uses UTC RFC3339Nano with 'Z' suffix representation and package-level clock.
//  7. Never claims terminal, malformed, or already reviewing sessions.
func (s *Store) ClaimSession(ctx context.Context, policy TimingPolicy) (*ClaimResult, error) {
	return s.ClaimSessionWithNow(ctx, policy, time.Time{})
}

// Claim is an alias for ClaimSession on Store.
func (s *Store) Claim(ctx context.Context, policy TimingPolicy) (*ClaimResult, error) {
	return s.ClaimSession(ctx, policy)
}

// ClaimSession package-level function delegating to s.ClaimSession.
func ClaimSession(ctx context.Context, s *Store, policy TimingPolicy) (*ClaimResult, error) {
	if s == nil {
		return nil, errors.New("cannot claim session from nil store")
	}
	return s.ClaimSession(ctx, policy)
}

// Claim package-level function delegating to s.Claim.
func Claim(ctx context.Context, s *Store, policy TimingPolicy) (*ClaimResult, error) {
	return ClaimSession(ctx, s, policy)
}

// ClaimSessionWithNow executes the claim logic using an explicit claimTime (or clock().UTC() if zero).
func (s *Store) ClaimSessionWithNow(ctx context.Context, policy TimingPolicy, claimTime time.Time) (*ClaimResult, error) {
	if s == nil {
		return nil, errors.New("cannot claim session from nil store")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}

	writer, err := s.writerDB()
	if err != nil {
		return nil, err
	}

	var now time.Time
	if claimTime.IsZero() {
		now = clock().UTC()
	} else {
		now = claimTime.UTC()
	}

	retryPolicy := DefaultRetryPolicy("claim_session")
	var result *ClaimResult

	err = withImmediate(ctx, writer, retryPolicy, func(conn *sql.Conn) error {
		// 1. Check if any session is already in 'reviewing' state across the database.
		var reviewingCount int
		err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions WHERE status = 'reviewing';").Scan(&reviewingCount)
		if err != nil {
			return fmt.Errorf("check reviewing sessions: %w", sanitizeError(err))
		}
		if reviewingCount > 0 {
			result = &ClaimResult{
				Claimed:      false,
				NoWorkReason: NoWorkActiveReviewing,
			}
			return nil
		}

		// 2. Select FIFO by created_at ASC, id ASC.
		var (
			targetID            string
			projectID           string
			idempotencyKey      string
			requestHash         string
			snapshotID          string
			cancelRequested     int
			completedRoleCount  int
			incompleteRoleCount int
			createdAtStr        string
			claimedAtRaw        sql.NullString
			dispatchCutoffRaw   sql.NullString
			hardDeadlineRaw     sql.NullString
			terminalAtRaw       sql.NullString
			terminalReasonRaw   sql.NullString
		)
		err = conn.QueryRowContext(ctx, `
			SELECT
				id, project_id, idempotency_key, request_hash, snapshot_id,
				cancel_requested, completed_role_count, incomplete_role_count, created_at,
				claimed_at, dispatch_cutoff_at, hard_deadline_at, terminal_at, terminal_reason
			FROM sessions
			WHERE status = 'queued'
			ORDER BY created_at ASC, id ASC
			LIMIT 1;
		`).Scan(
			&targetID, &projectID, &idempotencyKey, &requestHash, &snapshotID,
			&cancelRequested, &completedRoleCount, &incompleteRoleCount, &createdAtStr,
			&claimedAtRaw, &dispatchCutoffRaw, &hardDeadlineRaw, &terminalAtRaw, &terminalReasonRaw,
		)

		if errors.Is(err, sql.ErrNoRows) {
			result = &ClaimResult{
				Claimed:      false,
				NoWorkReason: NoWorkNoQueuedSession,
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("select next queued session: %w", sanitizeError(err))
		}

		// Validate selected session fields to never claim malformed sessions.
		if strings.TrimSpace(targetID) == "" {
			return fmt.Errorf("%w: selected queued session has empty id", ErrMalformedData)
		}
		if cancelRequested != 0 && cancelRequested != 1 {
			return fmt.Errorf("%w: session %q has invalid cancel_requested value %d", ErrMalformedData, targetID, cancelRequested)
		}
		if completedRoleCount != 0 || incompleteRoleCount != 4 {
			return fmt.Errorf("%w: queued session %q has invalid role counts (completed=%d, incomplete=%d)",
				ErrMalformedData, targetID, completedRoleCount, incompleteRoleCount)
		}
		if claimedAtRaw.Valid || dispatchCutoffRaw.Valid || hardDeadlineRaw.Valid || terminalAtRaw.Valid || terminalReasonRaw.Valid {
			return fmt.Errorf("%w: queued session %q has non-null timing or terminal fields", ErrMalformedData, targetID)
		}

		createdAt, err := parseUTCTimestamp(createdAtStr)
		if err != nil {
			return fmt.Errorf("%w: session %q has malformed created_at: %w", ErrMalformedData, targetID, err)
		}
		if createdAt.After(now) {
			return fmt.Errorf("%w: session %q created_at %s is after claim time %s",
				ErrMalformedData, targetID, createdAtStr, formatUTCTimestamp(now))
		}

		// Verify that all canonical role runs exist and are in pending status.
		var (
			roleCount           int
			nonPendingRoleCount int
		)
		err = conn.QueryRowContext(ctx, `
			SELECT
				COUNT(*),
				COALESCE(SUM(CASE WHEN status != 'pending' THEN 1 ELSE 0 END), 0)
			FROM role_runs
			WHERE session_id = ?;
		`, targetID).Scan(&roleCount, &nonPendingRoleCount)
		if err != nil {
			return fmt.Errorf("check role_runs for session %q: %w", targetID, sanitizeError(err))
		}
		if roleCount != domain.RoleCount {
			return fmt.Errorf("%w: queued session %q has %d role_runs, expected %d",
				ErrMalformedData, targetID, roleCount, domain.RoleCount)
		}
		if nonPendingRoleCount > 0 {
			return fmt.Errorf("%w: queued session %q has %d non-pending role_runs",
				ErrMalformedData, targetID, nonPendingRoleCount)
		}

		// Compute atomic timestamps in UTC RFC3339Nano.
		claimedAt := now
		dispatchCutoffAt := now.Add(policy.DispatchCutoff)
		hardDeadlineAt := now.Add(policy.SessionHardDeadline)

		claimedAtStr := formatUTCTimestamp(claimedAt)
		cutoffStr := formatUTCTimestamp(dispatchCutoffAt)
		deadlineStr := formatUTCTimestamp(hardDeadlineAt)

		// Optional test hook for verifying guarded update zero rows conflict.
		if claimBeforeUpdateHook != nil {
			if hookErr := claimBeforeUpdateHook(ctx, conn, targetID); hookErr != nil {
				return hookErr
			}
		}

		// 3. Update exactly one queued session to reviewing.
		res, err := conn.ExecContext(ctx, `
			UPDATE sessions
			SET status = 'reviewing',
				claimed_at = ?,
				dispatch_cutoff_at = ?,
				hard_deadline_at = ?
			WHERE id = ? AND status = 'queued';
		`, claimedAtStr, cutoffStr, deadlineStr, targetID)
		if err != nil {
			return fmt.Errorf("update session %q to reviewing: %w", targetID, sanitizeError(err))
		}

		rowsAffected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("check rows affected for session %q: %w", targetID, sanitizeError(err))
		}
		if rowsAffected == 0 {
			// Guarded update affected 0 rows (e.g. state changed concurrently). Return no-work.
			result = &ClaimResult{
				Claimed:      false,
				NoWorkReason: NoWorkGuardConflict,
			}
			return nil
		}

		// Optional test hook for verifying transaction rollback atomicity.
		if claimBeforeCommitHook != nil {
			if hookErr := claimBeforeCommitHook(ctx, conn); hookErr != nil {
				return hookErr
			}
		}

		result = &ClaimResult{
			Claimed:          true,
			SessionID:        targetID,
			ProjectID:        projectID,
			IdempotencyKey:   idempotencyKey,
			RequestHash:      requestHash,
			SnapshotID:       snapshotID,
			Status:           domain.SessionReviewing,
			CancelRequested:  cancelRequested == 1,
			ClaimedAt:        claimedAt,
			DispatchCutoffAt: dispatchCutoffAt,
			HardDeadlineAt:   hardDeadlineAt,
		}
		return nil
	})

	if err != nil {
		return nil, err
	}
	return result, nil
}

func formatUTCTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000000Z")
}
