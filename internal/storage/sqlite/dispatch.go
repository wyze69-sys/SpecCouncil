package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
)

// MaxInFlight is the maximum number of roles that may be concurrently in_flight for a session.
const MaxInFlight = 2

// DispatchNoWorkReason describes why a role reservation attempt yielded no role to dispatch.
type DispatchNoWorkReason string

const (
	DispatchNoWorkNone          DispatchNoWorkReason = ""
	DispatchNoWorkNotReviewing  DispatchNoWorkReason = "not_reviewing"
	DispatchNoWorkCancelled     DispatchNoWorkReason = "cancelled"
	DispatchNoWorkCutoff        DispatchNoWorkReason = "cutoff"
	DispatchNoWorkCapacityFull  DispatchNoWorkReason = "capacity_full"
	DispatchNoWorkNoPendingRole DispatchNoWorkReason = "no_pending_role"
	DispatchNoWorkGuardConflict DispatchNoWorkReason = "guard_conflict"

	// Aliases for convenience and compatibility.
	DispatchNoWorkCancelRequested = DispatchNoWorkCancelled
	DispatchNoWorkCutoffReached   = DispatchNoWorkCutoff
	DispatchNoWorkMaxInFlight     = DispatchNoWorkCapacityFull
	DispatchNoWorkNonReviewing    = DispatchNoWorkNotReviewing
)

// DispatchReservationResult represents the outcome of an atomic role reservation attempt.
type DispatchReservationResult struct {
	Reserved      bool                 `json:"reserved"`
	NoWorkReason  DispatchNoWorkReason `json:"no_work_reason,omitempty"`
	SessionID     string               `json:"session_id,omitempty"`
	RoleRunID     string               `json:"role_run_id,omitempty"`
	Role          domain.Role          `json:"role,omitempty"`
	Status        domain.RoleStatus    `json:"status,omitempty"`
	StartedAt     *time.Time           `json:"started_at,omitempty"`
	CallCount     int                  `json:"call_count,omitempty"`
	InFlightCount int                  `json:"in_flight_count,omitempty"`
}

// IsReserved reports whether a pending role was successfully reserved.
func (r *DispatchReservationResult) IsReserved() bool {
	return r != nil && r.Reserved
}

// IsNoWork reports whether the reservation attempt yielded no work.
func (r *DispatchReservationResult) IsNoWork() bool {
	return r == nil || !r.Reserved
}

// HasWork reports whether a role was successfully reserved.
func (r *DispatchReservationResult) HasWork() bool {
	return r != nil && r.Reserved
}

// Type aliases for caller convenience.
type (
	DispatchResult    = DispatchReservationResult
	ReservationResult = DispatchReservationResult
)

var (
	// dispatchBeforeCommitHook allows deterministic test verification of rollback on injected failure.
	dispatchBeforeCommitHook func(ctx context.Context, conn *sql.Conn) error

	// dispatchBeforeUpdateHook allows deterministic test verification of conflict / races before update.
	dispatchBeforeUpdateHook func(ctx context.Context, conn *sql.Conn, sessionID string, role domain.Role) error
)

// ReservePendingRole attempts to atomically reserve the next pending role in canonical order
// for the given session within a single immediate transaction.
//
// Rules enforced:
//  1. Operates only on a session in 'reviewing' status. If session is not reviewing, returns
//     typed no-work (Reserved: false, NoWorkReason: DispatchNoWorkNotReviewing).
//  2. Refuses reservation when cancel_requested = 1 (Reserved: false, NoWorkReason: DispatchNoWorkCancelled).
//  3. Refuses reservation when now >= dispatch_cutoff_at (Reserved: false, NoWorkReason: DispatchNoWorkCutoff).
//  4. Counts persisted in_flight roles inside the same transaction; never reserves when
//     the count is already MAX_IN_FLIGHT = 2 (Reserved: false, NoWorkReason: DispatchNoWorkCapacityFull).
//  5. Selects a pending role deterministically by canonical role order (requirements, architecture, qa, security).
//     If no pending role exists, returns typed no-work (Reserved: false, NoWorkReason: DispatchNoWorkNoPendingRole).
//  6. Atomically changes exactly one pending role to 'in_flight' and sets its started_at = now
//     and initial call metadata (call_count = 0).
//  7. Rechecks every predicate in the authoritative UPDATE statement. If the guarded update affects
//     zero rows, returns typed no-work (Reserved: false, NoWorkReason: DispatchNoWorkGuardConflict).
//  8. Never changes session status or cancel flag.
//  9. Never calls a provider, starts a goroutine, or holds a transaction over work outside the database.
func (s *Store) ReservePendingRole(ctx context.Context, sessionID string) (*DispatchReservationResult, error) {
	return s.ReservePendingRoleScoped(ctx, "", sessionID, time.Time{})
}

// ReservePendingRoleWithNow executes ReservePendingRole with an explicit reservation time.
func (s *Store) ReservePendingRoleWithNow(ctx context.Context, sessionID string, now time.Time) (*DispatchReservationResult, error) {
	return s.ReservePendingRoleScoped(ctx, "", sessionID, now)
}

// ReservePendingRoleScoped executes ReservePendingRole with optional projectID scoping and explicit time.
func (s *Store) ReservePendingRoleScoped(ctx context.Context, projectID, sessionID string, reservationTime time.Time) (*DispatchReservationResult, error) {
	if s == nil {
		return nil, errors.New("cannot reserve role from nil store")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, &SessionNotFoundError{SessionID: sessionID, ProjectID: projectID}
	}

	writer, err := s.writerDB()
	if err != nil {
		return nil, err
	}

	var now time.Time
	if reservationTime.IsZero() {
		now = clock().UTC()
	} else {
		now = reservationTime.UTC()
	}

	retryPolicy := DefaultRetryPolicy("reserve_pending_role")
	var result *DispatchReservationResult

	err = withImmediate(ctx, writer, retryPolicy, func(conn *sql.Conn) error {
		// 1. Query existing session state within the immediate transaction.
		var (
			dbProjectID     string
			statusStr       string
			cancelRequested int
			cutoffRaw       sql.NullString
		)
		query := `SELECT project_id, status, cancel_requested, dispatch_cutoff_at FROM sessions WHERE id = ?;`
		err := conn.QueryRowContext(ctx, query, sessionID).Scan(&dbProjectID, &statusStr, &cancelRequested, &cutoffRaw)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return &SessionNotFoundError{SessionID: sessionID, ProjectID: projectID}
			}
			return fmt.Errorf("query session %q: %w", sessionID, sanitizeError(err))
		}

		if projectID != "" && dbProjectID != projectID {
			return &SessionNotFoundError{SessionID: sessionID, ProjectID: projectID}
		}

		sessionStatus := domain.SessionStatus(statusStr)
		if !isValidSessionStatus(sessionStatus) {
			return fmt.Errorf("%w: invalid session status %q", ErrMalformedData, statusStr)
		}
		if cancelRequested != 0 && cancelRequested != 1 {
			return fmt.Errorf("%w: invalid cancel_requested value %d", ErrMalformedData, cancelRequested)
		}

		// 1. Operates only on a reviewing session.
		if sessionStatus != domain.SessionReviewing {
			result = &DispatchReservationResult{
				Reserved:     false,
				NoWorkReason: DispatchNoWorkNotReviewing,
				SessionID:    sessionID,
			}
			return nil
		}

		// 2. Refuses reservation when cancel_requested = 1.
		if cancelRequested == 1 {
			result = &DispatchReservationResult{
				Reserved:     false,
				NoWorkReason: DispatchNoWorkCancelled,
				SessionID:    sessionID,
			}
			return nil
		}

		// 3. Refuses reservation when now >= dispatch_cutoff_at.
		if !cutoffRaw.Valid || strings.TrimSpace(cutoffRaw.String) == "" {
			return fmt.Errorf("%w: reviewing session %q has null dispatch_cutoff_at", ErrMalformedData, sessionID)
		}
		cutoffTime, err := parseUTCTimestamp(cutoffRaw.String)
		if err != nil {
			return fmt.Errorf("%w: malformed dispatch_cutoff_at %q: %w", ErrMalformedData, cutoffRaw.String, err)
		}
		if !now.Before(cutoffTime) { // now >= cutoffTime
			result = &DispatchReservationResult{
				Reserved:     false,
				NoWorkReason: DispatchNoWorkCutoff,
				SessionID:    sessionID,
			}
			return nil
		}

		// 4. Counts persisted in_flight roles inside the same transaction; never reserves when count >= MAX_IN_FLIGHT (2).
		var inFlightCount int
		err = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM role_runs WHERE session_id = ? AND status = 'in_flight';`, sessionID).Scan(&inFlightCount)
		if err != nil {
			return fmt.Errorf("count in_flight roles for session %q: %w", sessionID, sanitizeError(err))
		}
		if inFlightCount >= MaxInFlight {
			result = &DispatchReservationResult{
				Reserved:      false,
				NoWorkReason:  DispatchNoWorkCapacityFull,
				SessionID:     sessionID,
				InFlightCount: inFlightCount,
			}
			return nil
		}

		// 5. Selects next pending role deterministically by canonical role order.
		var (
			targetRoleRunID string
			targetRoleStr   string
		)
		err = conn.QueryRowContext(ctx, `
			SELECT id, role
			FROM role_runs
			WHERE session_id = ? AND status = 'pending'
			ORDER BY CASE role
				WHEN 'requirements' THEN 0
				WHEN 'architecture' THEN 1
				WHEN 'qa' THEN 2
				WHEN 'security' THEN 3
				ELSE 99 END ASC
			LIMIT 1;
		`, sessionID).Scan(&targetRoleRunID, &targetRoleStr)

		if errors.Is(err, sql.ErrNoRows) {
			result = &DispatchReservationResult{
				Reserved:      false,
				NoWorkReason:  DispatchNoWorkNoPendingRole,
				SessionID:     sessionID,
				InFlightCount: inFlightCount,
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("select next pending role for session %q: %w", sessionID, sanitizeError(err))
		}

		targetRole := domain.Role(targetRoleStr)
		if !domain.IsValidRole(targetRole) {
			return fmt.Errorf("%w: invalid role %q in role_run %q", ErrMalformedData, targetRoleStr, targetRoleRunID)
		}

		// Optional test hook before update.
		if dispatchBeforeUpdateHook != nil {
			if hookErr := dispatchBeforeUpdateHook(ctx, conn, sessionID, targetRole); hookErr != nil {
				return hookErr
			}
		}

		// 6. & 7. Atomically change exactly one pending role to in_flight, setting started_at and call_count = 0,
		// while rechecking every authoritative predicate in the UPDATE WHERE clause.
		startedAtStr := formatUTCTimestamp(now)

		res, err := conn.ExecContext(ctx, `
			UPDATE role_runs
			SET status = 'in_flight',
				started_at = ?,
				call_count = 0
			WHERE id = ?
			  AND status = 'pending'
			  AND (
				SELECT status = 'reviewing'
				   AND cancel_requested = 0
				   AND julianday(dispatch_cutoff_at) > julianday(?)
				FROM sessions
				WHERE id = ?
			  )
			  AND (
				SELECT COUNT(*)
				FROM role_runs
				WHERE session_id = ? AND status = 'in_flight'
			  ) < ?;
		`, startedAtStr, targetRoleRunID, startedAtStr, sessionID, sessionID, MaxInFlight)
		if err != nil {
			return fmt.Errorf("guarded update role_run %q to in_flight: %w", targetRoleRunID, sanitizeError(err))
		}

		rowsAffected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("check rows affected for role_run %q: %w", targetRoleRunID, sanitizeError(err))
		}

		if rowsAffected == 0 {
			// Predicate failed or raced concurrently. Return typed no-work guard conflict.
			result = &DispatchReservationResult{
				Reserved:     false,
				NoWorkReason: DispatchNoWorkGuardConflict,
				SessionID:    sessionID,
				RoleRunID:    targetRoleRunID,
				Role:         targetRole,
			}
			return nil
		}

		// Optional test hook before commit.
		if dispatchBeforeCommitHook != nil {
			if hookErr := dispatchBeforeCommitHook(ctx, conn); hookErr != nil {
				return hookErr
			}
		}

		result = &DispatchReservationResult{
			Reserved:      true,
			NoWorkReason:  DispatchNoWorkNone,
			SessionID:     sessionID,
			RoleRunID:     targetRoleRunID,
			Role:          targetRole,
			Status:        domain.RoleInFlight,
			StartedAt:     &now,
			CallCount:     0,
			InFlightCount: inFlightCount + 1,
		}
		return nil
	})

	if err != nil {
		return nil, err
	}
	return result, nil
}

// Aliases on Store.

// ReserveRole is an alias for ReservePendingRole.
func (s *Store) ReserveRole(ctx context.Context, sessionID string) (*DispatchReservationResult, error) {
	return s.ReservePendingRole(ctx, sessionID)
}

// ReserveRoleWithNow is an alias for ReservePendingRoleWithNow.
func (s *Store) ReserveRoleWithNow(ctx context.Context, sessionID string, now time.Time) (*DispatchReservationResult, error) {
	return s.ReservePendingRoleWithNow(ctx, sessionID, now)
}

// ReserveNextRole is an alias for ReservePendingRole.
func (s *Store) ReserveNextRole(ctx context.Context, sessionID string) (*DispatchReservationResult, error) {
	return s.ReservePendingRole(ctx, sessionID)
}

// ReserveDispatch is an alias for ReservePendingRole.
func (s *Store) ReserveDispatch(ctx context.Context, sessionID string) (*DispatchReservationResult, error) {
	return s.ReservePendingRole(ctx, sessionID)
}

// Package-level functions delegating to Store.

// ReservePendingRole package-level function delegating to s.ReservePendingRole.
func ReservePendingRole(ctx context.Context, s *Store, sessionID string) (*DispatchReservationResult, error) {
	if s == nil {
		return nil, errors.New("cannot reserve role from nil store")
	}
	return s.ReservePendingRole(ctx, sessionID)
}

// ReservePendingRoleWithNow package-level function delegating to s.ReservePendingRoleWithNow.
func ReservePendingRoleWithNow(ctx context.Context, s *Store, sessionID string, now time.Time) (*DispatchReservationResult, error) {
	if s == nil {
		return nil, errors.New("cannot reserve role from nil store")
	}
	return s.ReservePendingRoleWithNow(ctx, sessionID, now)
}

// ReserveRole package-level function delegating to s.ReserveRole.
func ReserveRole(ctx context.Context, s *Store, sessionID string) (*DispatchReservationResult, error) {
	return ReservePendingRole(ctx, s, sessionID)
}

// ReserveNextRole package-level function delegating to s.ReserveNextRole.
func ReserveNextRole(ctx context.Context, s *Store, sessionID string) (*DispatchReservationResult, error) {
	return ReservePendingRole(ctx, s, sessionID)
}

// ReserveDispatch package-level function delegating to s.ReserveDispatch.
func ReserveDispatch(ctx context.Context, s *Store, sessionID string) (*DispatchReservationResult, error) {
	return ReservePendingRole(ctx, s, sessionID)
}
