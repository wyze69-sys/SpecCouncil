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

// InFlightRole captures the identity and role name of an in-flight role run.
type InFlightRole struct {
	RoleRunID string      `json:"role_run_id"`
	Role      domain.Role `json:"role"`
}

// SweepCancellationResult represents the authoritative outcome of a cancellation sweep.
type SweepCancellationResult struct {
	SessionID        string        `json:"session_id"`
	ProjectID        string        `json:"project_id"`
	InterruptedCount int           `json:"interrupted_count"`
	InterruptedRoles []domain.Role `json:"interrupted_roles,omitempty"`
	InFlightCount    int           `json:"in_flight_count"`
	NoOp             bool          `json:"no_op,omitempty"`
}

// HasInterrupted reports whether any pending roles were interrupted by the sweep.
func (r *SweepCancellationResult) HasInterrupted() bool {
	return r != nil && r.InterruptedCount > 0
}

// IsNoOp reports whether the sweep was a no-op.
func (r *SweepCancellationResult) IsNoOp() bool {
	return r == nil || r.NoOp
}

// SweepCutoffResult represents the authoritative outcome of a cutoff sweep.
type SweepCutoffResult struct {
	SessionID        string        `json:"session_id"`
	ProjectID        string        `json:"project_id"`
	CutoffReached    bool          `json:"cutoff_reached"`
	InterruptedCount int           `json:"interrupted_count"`
	InterruptedRoles []domain.Role `json:"interrupted_roles,omitempty"`
	InFlightCount    int           `json:"in_flight_count"`
	NoOp             bool          `json:"no_op,omitempty"`
}

// HasInterrupted reports whether any pending roles were interrupted by the sweep.
func (r *SweepCutoffResult) HasInterrupted() bool {
	return r != nil && r.InterruptedCount > 0
}

// IsNoOp reports whether the sweep was a no-op.
func (r *SweepCutoffResult) IsNoOp() bool {
	return r == nil || r.NoOp
}

// SweepHardDeadlineResult represents the authoritative outcome of a hard-deadline sweep.
type SweepHardDeadlineResult struct {
	SessionID        string         `json:"session_id"`
	ProjectID        string         `json:"project_id"`
	InterruptedCount int            `json:"interrupted_count"`
	InterruptedRoles []domain.Role  `json:"interrupted_roles,omitempty"`
	InFlightRoleIDs  []string       `json:"in_flight_role_ids"`
	InFlightRoles    []InFlightRole `json:"in_flight_roles,omitempty"`
	NoOp             bool           `json:"no_op,omitempty"`
}

// HasInterrupted reports whether any pending roles were interrupted by the sweep.
func (r *SweepHardDeadlineResult) HasInterrupted() bool {
	return r != nil && r.InterruptedCount > 0
}

// HasInFlight reports whether any roles are currently in_flight.
func (r *SweepHardDeadlineResult) HasInFlight() bool {
	return r != nil && len(r.InFlightRoleIDs) > 0
}

// IsNoOp reports whether the sweep was a no-op.
func (r *SweepHardDeadlineResult) IsNoOp() bool {
	return r == nil || r.NoOp
}

// RecoveredSessionInfo captures the recovery actions taken on a single stale session.
type RecoveredSessionInfo struct {
	SessionID           string `json:"session_id"`
	ProjectID           string `json:"project_id"`
	InFlightInterrupted int    `json:"in_flight_interrupted"`
	PendingInterrupted  int    `json:"pending_interrupted"`
	TotalInterrupted    int    `json:"total_interrupted"`
	CancelRequested     bool   `json:"cancel_requested"`
	NoOp                bool   `json:"no_op,omitempty"`
}

// RestartRecoveryParams configures a restart recovery sweep.
type RestartRecoveryParams struct {
	ProjectID string    // optional: filter sessions by project ID
	SessionID string    // optional: filter recovery to a single session ID
	Cutoff    time.Time // required: explicit cutoff timestamp for stale sessions
	Now       time.Time // optional: timestamp used for role completed_at (defaults to clock().UTC())
}

// SweepRestartRecoveryResult represents the authoritative outcome of a restart recovery sweep.
type SweepRestartRecoveryResult struct {
	Cutoff              time.Time              `json:"cutoff"`
	SessionsRecovered   int                    `json:"sessions_recovered"`
	Sessions            []RecoveredSessionInfo `json:"sessions,omitempty"`
	TotalInterrupted    int                    `json:"total_interrupted"`
	InFlightInterrupted int                    `json:"in_flight_interrupted"`
	PendingInterrupted  int                    `json:"pending_interrupted"`
}

// HasRecovered reports whether any stale sessions were recovered.
func (r *SweepRestartRecoveryResult) HasRecovered() bool {
	return r != nil && r.SessionsRecovered > 0
}

var (
	// sweepBeforeCommitHook allows deterministic test verification of rollback on injected failure.
	sweepBeforeCommitHook func(ctx context.Context, conn *sql.Conn) error

	// sweepBeforeUpdateHook allows deterministic test verification of conflict / races before update.
	sweepBeforeUpdateHook func(ctx context.Context, conn *sql.Conn, sessionID string) error
)

// SweepCancellation executes a cancellation sweep scoped to one reviewing session.
// Pending roles become interrupted with cause user_cancelled. In-flight and terminal roles remain unchanged.
// Queued or terminal sessions are no-ops.
func (s *Store) SweepCancellation(ctx context.Context, sessionID string) (*SweepCancellationResult, error) {
	return s.SweepCancellationScoped(ctx, "", sessionID, time.Time{})
}

// SweepCancellationWithNow executes SweepCancellation with an explicit timestamp.
func (s *Store) SweepCancellationWithNow(ctx context.Context, sessionID string, now time.Time) (*SweepCancellationResult, error) {
	return s.SweepCancellationScoped(ctx, "", sessionID, now)
}

// SweepCancellationScoped executes SweepCancellation with optional projectID scoping and an explicit timestamp.
func (s *Store) SweepCancellationScoped(ctx context.Context, projectID, sessionID string, now time.Time) (*SweepCancellationResult, error) {
	if s == nil {
		return nil, errors.New("cannot execute sweep on nil store")
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

	if now.IsZero() {
		now = clock().UTC()
	} else {
		now = now.UTC()
	}
	nowStr := formatUTCTimestamp(now)

	retryPolicy := DefaultRetryPolicy("sweep_cancellation")
	var result *SweepCancellationResult

	err = withImmediate(ctx, writer, retryPolicy, func(conn *sql.Conn) error {
		var (
			dbProjectID     string
			statusStr       string
			cancelRequested int
		)
		err := conn.QueryRowContext(ctx, `SELECT project_id, status, cancel_requested FROM sessions WHERE id = ?;`, sessionID).Scan(&dbProjectID, &statusStr, &cancelRequested)
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
			return fmt.Errorf("%w: session %q has invalid cancel_requested value %d", ErrMalformedData, sessionID, cancelRequested)
		}

		// Queued or terminal sessions are no-ops.
		if sessionStatus != domain.SessionReviewing {
			result = &SweepCancellationResult{
				SessionID: sessionID,
				ProjectID: dbProjectID,
				NoOp:      true,
			}
			return nil
		}

		// Count in-flight roles.
		var inFlightCount int
		err = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM role_runs WHERE session_id = ? AND status = 'in_flight';`, sessionID).Scan(&inFlightCount)
		if err != nil {
			return fmt.Errorf("count in_flight roles for session %q: %w", sessionID, sanitizeError(err))
		}

		if cancelRequested != 1 {
			result = &SweepCancellationResult{
				SessionID:     sessionID,
				ProjectID:     dbProjectID,
				InFlightCount: inFlightCount,
				NoOp:          true,
			}
			return nil
		}

		// Query pending roles before update.
		rows, err := conn.QueryContext(ctx, `
			SELECT role FROM role_runs
			WHERE session_id = ? AND status = 'pending'
			ORDER BY CASE role
				WHEN 'requirements' THEN 0
				WHEN 'architecture' THEN 1
				WHEN 'qa' THEN 2
				WHEN 'security' THEN 3
				ELSE 99 END ASC;
		`, sessionID)
		if err != nil {
			return fmt.Errorf("query pending roles for session %q: %w", sessionID, sanitizeError(err))
		}
		defer rows.Close()

		var pendingRoles []domain.Role
		for rows.Next() {
			var rStr string
			if err := rows.Scan(&rStr); err != nil {
				return fmt.Errorf("scan pending role: %w", sanitizeError(err))
			}
			r := domain.Role(rStr)
			if !domain.IsValidRole(r) {
				return fmt.Errorf("%w: invalid role %q in session %q", ErrMalformedData, rStr, sessionID)
			}
			pendingRoles = append(pendingRoles, r)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("rows error pending roles: %w", sanitizeError(err))
		}

		if len(pendingRoles) == 0 {
			result = &SweepCancellationResult{
				SessionID:     sessionID,
				ProjectID:     dbProjectID,
				InFlightCount: inFlightCount,
				NoOp:          true,
			}
			return nil
		}

		if sweepBeforeUpdateHook != nil {
			if hookErr := sweepBeforeUpdateHook(ctx, conn, sessionID); hookErr != nil {
				return hookErr
			}
		}

		// Authoritative update: pending roles become interrupted with cause user_cancelled.
		res, err := conn.ExecContext(ctx, `
			UPDATE role_runs
			SET status = 'interrupted',
			    cause = 'user_cancelled',
			    completed_at = ?
			WHERE session_id = ?
			  AND status = 'pending'
			  AND EXISTS (
			      SELECT 1 FROM sessions
			      WHERE id = role_runs.session_id
			        AND status = 'reviewing'
			        AND cancel_requested = 1
			  );
		`, nowStr, sessionID)
		if err != nil {
			return fmt.Errorf("update pending roles on cancellation sweep for session %q: %w", sessionID, sanitizeError(err))
		}

		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("rows affected cancellation sweep for session %q: %w", sessionID, sanitizeError(err))
		}

		if sweepBeforeCommitHook != nil {
			if hookErr := sweepBeforeCommitHook(ctx, conn); hookErr != nil {
				return hookErr
			}
		}

		interruptedRoles := pendingRoles
		if int(affected) < len(interruptedRoles) {
			interruptedRoles = interruptedRoles[:affected]
		}

		result = &SweepCancellationResult{
			SessionID:        sessionID,
			ProjectID:        dbProjectID,
			InterruptedCount: int(affected),
			InterruptedRoles: interruptedRoles,
			InFlightCount:    inFlightCount,
			NoOp:             affected == 0,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// SweepCutoff executes a cutoff sweep scoped to one reviewing session and an injected now.
// When now >= dispatch_cutoff_at, pending roles become interrupted with cause deadline_cutoff.
// In-flight and terminal roles remain unchanged. Before cutoff, it is a no-op.
// Queued or terminal sessions are no-ops.
func (s *Store) SweepCutoff(ctx context.Context, sessionID string, now time.Time) (*SweepCutoffResult, error) {
	return s.SweepCutoffScoped(ctx, "", sessionID, now)
}

// SweepCutoffScoped executes SweepCutoff with optional projectID scoping.
func (s *Store) SweepCutoffScoped(ctx context.Context, projectID, sessionID string, now time.Time) (*SweepCutoffResult, error) {
	if s == nil {
		return nil, errors.New("cannot execute sweep on nil store")
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

	if now.IsZero() {
		now = clock().UTC()
	} else {
		now = now.UTC()
	}
	nowStr := formatUTCTimestamp(now)

	retryPolicy := DefaultRetryPolicy("sweep_cutoff")
	var result *SweepCutoffResult

	err = withImmediate(ctx, writer, retryPolicy, func(conn *sql.Conn) error {
		var (
			dbProjectID string
			statusStr   string
			cutoffRaw   sql.NullString
		)
		err := conn.QueryRowContext(ctx, `SELECT project_id, status, dispatch_cutoff_at FROM sessions WHERE id = ?;`, sessionID).Scan(&dbProjectID, &statusStr, &cutoffRaw)
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

		// Queued or terminal sessions are no-ops.
		if sessionStatus != domain.SessionReviewing {
			result = &SweepCutoffResult{
				SessionID:     sessionID,
				ProjectID:     dbProjectID,
				CutoffReached: false,
				NoOp:          true,
			}
			return nil
		}

		if !cutoffRaw.Valid || strings.TrimSpace(cutoffRaw.String) == "" {
			return fmt.Errorf("%w: reviewing session %q has null dispatch_cutoff_at", ErrMalformedData, sessionID)
		}

		cutoffTime, err := parseUTCTimestamp(cutoffRaw.String)
		if err != nil {
			return fmt.Errorf("%w: malformed dispatch_cutoff_at %q: %w", ErrMalformedData, cutoffRaw.String, err)
		}

		// Before cutoff it is a no-op.
		if now.Before(cutoffTime) {
			result = &SweepCutoffResult{
				SessionID:     sessionID,
				ProjectID:     dbProjectID,
				CutoffReached: false,
				NoOp:          true,
			}
			return nil
		}

		// Count in-flight roles.
		var inFlightCount int
		err = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM role_runs WHERE session_id = ? AND status = 'in_flight';`, sessionID).Scan(&inFlightCount)
		if err != nil {
			return fmt.Errorf("count in_flight roles for session %q: %w", sessionID, sanitizeError(err))
		}

		// Query pending roles before update.
		rows, err := conn.QueryContext(ctx, `
			SELECT role FROM role_runs
			WHERE session_id = ? AND status = 'pending'
			ORDER BY CASE role
				WHEN 'requirements' THEN 0
				WHEN 'architecture' THEN 1
				WHEN 'qa' THEN 2
				WHEN 'security' THEN 3
				ELSE 99 END ASC;
		`, sessionID)
		if err != nil {
			return fmt.Errorf("query pending roles for session %q: %w", sessionID, sanitizeError(err))
		}
		defer rows.Close()

		var pendingRoles []domain.Role
		for rows.Next() {
			var rStr string
			if err := rows.Scan(&rStr); err != nil {
				return fmt.Errorf("scan pending role: %w", sanitizeError(err))
			}
			r := domain.Role(rStr)
			if !domain.IsValidRole(r) {
				return fmt.Errorf("%w: invalid role %q in session %q", ErrMalformedData, rStr, sessionID)
			}
			pendingRoles = append(pendingRoles, r)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("rows error pending roles: %w", sanitizeError(err))
		}

		if len(pendingRoles) == 0 {
			result = &SweepCutoffResult{
				SessionID:     sessionID,
				ProjectID:     dbProjectID,
				CutoffReached: true,
				InFlightCount: inFlightCount,
				NoOp:          true,
			}
			return nil
		}

		if sweepBeforeUpdateHook != nil {
			if hookErr := sweepBeforeUpdateHook(ctx, conn, sessionID); hookErr != nil {
				return hookErr
			}
		}

		// Authoritative update: pending roles become interrupted with cause deadline_cutoff.
		res, err := conn.ExecContext(ctx, `
			UPDATE role_runs
			SET status = 'interrupted',
			    cause = 'deadline_cutoff',
			    completed_at = ?
			WHERE session_id = ?
			  AND status = 'pending'
			  AND EXISTS (
			      SELECT 1 FROM sessions
			      WHERE id = role_runs.session_id
			        AND status = 'reviewing'
			        AND dispatch_cutoff_at IS NOT NULL
			        AND ? >= dispatch_cutoff_at
			  );
		`, nowStr, sessionID, nowStr)
		if err != nil {
			return fmt.Errorf("update pending roles on cutoff sweep for session %q: %w", sessionID, sanitizeError(err))
		}

		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("rows affected cutoff sweep for session %q: %w", sessionID, sanitizeError(err))
		}

		if sweepBeforeCommitHook != nil {
			if hookErr := sweepBeforeCommitHook(ctx, conn); hookErr != nil {
				return hookErr
			}
		}

		interruptedRoles := pendingRoles
		if int(affected) < len(interruptedRoles) {
			interruptedRoles = interruptedRoles[:affected]
		}

		result = &SweepCutoffResult{
			SessionID:        sessionID,
			ProjectID:        dbProjectID,
			CutoffReached:    true,
			InterruptedCount: int(affected),
			InterruptedRoles: interruptedRoles,
			InFlightCount:    inFlightCount,
			NoOp:             affected == 0,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// SweepHardDeadline executes a hard-deadline sweep scoped to one reviewing session and an injected now.
// Pending roles become interrupted with cause deadline_cutoff.
// In-flight roles are not rewritten by this sweep; the exact set of in-flight role IDs is exposed
// for the caller to cancel locally and publish timeout through P9 compare-and-set publication.
// Queued or terminal sessions are no-ops.
func (s *Store) SweepHardDeadline(ctx context.Context, sessionID string, now time.Time) (*SweepHardDeadlineResult, error) {
	return s.SweepHardDeadlineScoped(ctx, "", sessionID, now)
}

// SweepHardDeadlineWithNow executes SweepHardDeadline with an explicit timestamp.
func (s *Store) SweepHardDeadlineWithNow(ctx context.Context, sessionID string, now time.Time) (*SweepHardDeadlineResult, error) {
	return s.SweepHardDeadlineScoped(ctx, "", sessionID, now)
}

// SweepHardDeadlineScoped executes SweepHardDeadline with optional projectID scoping.
func (s *Store) SweepHardDeadlineScoped(ctx context.Context, projectID, sessionID string, now time.Time) (*SweepHardDeadlineResult, error) {
	if s == nil {
		return nil, errors.New("cannot execute sweep on nil store")
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

	if now.IsZero() {
		now = clock().UTC()
	} else {
		now = now.UTC()
	}
	nowStr := formatUTCTimestamp(now)

	retryPolicy := DefaultRetryPolicy("sweep_hard_deadline")
	var result *SweepHardDeadlineResult

	err = withImmediate(ctx, writer, retryPolicy, func(conn *sql.Conn) error {
		var (
			dbProjectID    string
			statusStr      string
			hardDeadlineAt sql.NullString
		)
		err := conn.QueryRowContext(ctx, `SELECT project_id, status, hard_deadline_at FROM sessions WHERE id = ?;`, sessionID).Scan(&dbProjectID, &statusStr, &hardDeadlineAt)
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

		// Queued or terminal sessions are no-ops.
		if sessionStatus != domain.SessionReviewing {
			result = &SweepHardDeadlineResult{
				SessionID:       sessionID,
				ProjectID:       dbProjectID,
				InFlightRoleIDs: []string{},
				InFlightRoles:   []InFlightRole{},
				NoOp:            true,
			}
			return nil
		}

		// Query in-flight roles (to expose exact IDs; NOT rewritten by this sweep).
		rowsInFlight, err := conn.QueryContext(ctx, `
			SELECT id, role FROM role_runs
			WHERE session_id = ? AND status = 'in_flight'
			ORDER BY CASE role
				WHEN 'requirements' THEN 0
				WHEN 'architecture' THEN 1
				WHEN 'qa' THEN 2
				WHEN 'security' THEN 3
				ELSE 99 END ASC;
		`, sessionID)
		if err != nil {
			return fmt.Errorf("query in_flight roles for session %q: %w", sessionID, sanitizeError(err))
		}
		defer rowsInFlight.Close()

		inFlightRoleIDs := make([]string, 0)
		inFlightRoles := make([]InFlightRole, 0)
		for rowsInFlight.Next() {
			var (
				idStr   string
				roleStr string
			)
			if err := rowsInFlight.Scan(&idStr, &roleStr); err != nil {
				return fmt.Errorf("scan in_flight role: %w", sanitizeError(err))
			}
			r := domain.Role(roleStr)
			if !domain.IsValidRole(r) {
				return fmt.Errorf("%w: invalid role %q in session %q", ErrMalformedData, roleStr, sessionID)
			}
			inFlightRoleIDs = append(inFlightRoleIDs, idStr)
			inFlightRoles = append(inFlightRoles, InFlightRole{RoleRunID: idStr, Role: r})
		}
		if err := rowsInFlight.Err(); err != nil {
			return fmt.Errorf("rows error in_flight roles: %w", sanitizeError(err))
		}

		// If hard_deadline_at is NULL or now < hard_deadline_at, return NoOp without modifying role_runs.
		var deadlineReached bool
		if hardDeadlineAt.Valid && strings.TrimSpace(hardDeadlineAt.String) != "" {
			deadlineTime, err := parseUTCTimestamp(hardDeadlineAt.String)
			if err != nil {
				return fmt.Errorf("%w: malformed hard_deadline_at %q: %w", ErrMalformedData, hardDeadlineAt.String, err)
			}
			deadlineReached = !now.Before(deadlineTime)
		}

		if !deadlineReached {
			result = &SweepHardDeadlineResult{
				SessionID:        sessionID,
				ProjectID:        dbProjectID,
				InterruptedCount: 0,
				InterruptedRoles: nil,
				InFlightRoleIDs:  inFlightRoleIDs,
				InFlightRoles:    inFlightRoles,
				NoOp:             true,
			}
			return nil
		}

		// Query pending roles before update.
		rowsPending, err := conn.QueryContext(ctx, `
			SELECT role FROM role_runs
			WHERE session_id = ? AND status = 'pending'
			ORDER BY CASE role
				WHEN 'requirements' THEN 0
				WHEN 'architecture' THEN 1
				WHEN 'qa' THEN 2
				WHEN 'security' THEN 3
				ELSE 99 END ASC;
		`, sessionID)
		if err != nil {
			return fmt.Errorf("query pending roles for session %q: %w", sessionID, sanitizeError(err))
		}
		defer rowsPending.Close()

		var pendingRoles []domain.Role
		for rowsPending.Next() {
			var rStr string
			if err := rowsPending.Scan(&rStr); err != nil {
				return fmt.Errorf("scan pending role: %w", sanitizeError(err))
			}
			r := domain.Role(rStr)
			if !domain.IsValidRole(r) {
				return fmt.Errorf("%w: invalid role %q in session %q", ErrMalformedData, rStr, sessionID)
			}
			pendingRoles = append(pendingRoles, r)
		}
		if err := rowsPending.Err(); err != nil {
			return fmt.Errorf("rows error pending roles: %w", sanitizeError(err))
		}

		if sweepBeforeUpdateHook != nil {
			if hookErr := sweepBeforeUpdateHook(ctx, conn, sessionID); hookErr != nil {
				return hookErr
			}
		}

		var affected int64
		if len(pendingRoles) > 0 {
			res, err := conn.ExecContext(ctx, `
				UPDATE role_runs
				SET status = 'interrupted',
				    cause = 'deadline_cutoff',
				    completed_at = ?
				WHERE session_id = ?
				  AND status = 'pending'
				  AND EXISTS (
				      SELECT 1 FROM sessions
				      WHERE id = role_runs.session_id
				        AND status = 'reviewing'
				        AND hard_deadline_at IS NOT NULL
				        AND ? >= hard_deadline_at
				  );
			`, nowStr, sessionID, nowStr)
			if err != nil {
				return fmt.Errorf("update pending roles on hard-deadline sweep for session %q: %w", sessionID, sanitizeError(err))
			}
			aff, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("rows affected hard-deadline sweep for session %q: %w", sessionID, sanitizeError(err))
			}
			affected = aff
		}

		if sweepBeforeCommitHook != nil {
			if hookErr := sweepBeforeCommitHook(ctx, conn); hookErr != nil {
				return hookErr
			}
		}

		interruptedRoles := pendingRoles
		if int(affected) < len(interruptedRoles) {
			interruptedRoles = interruptedRoles[:affected]
		}

		result = &SweepHardDeadlineResult{
			SessionID:        sessionID,
			ProjectID:        dbProjectID,
			InterruptedCount: int(affected),
			InterruptedRoles: interruptedRoles,
			InFlightRoleIDs:  inFlightRoleIDs,
			InFlightRoles:    inFlightRoles,
			NoOp:             affected == 0 && len(inFlightRoleIDs) == 0,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// SweepRestartRecovery executes a restart recovery sweep on stale reviewing sessions selected by an explicit cutoff.
// For selected sessions:
//   - in-flight roles become interrupted/process_restart;
//   - pending roles become interrupted/user_cancelled when session cancel_requested = 1, otherwise interrupted/process_restart;
//   - completed, failed, and already interrupted roles remain unchanged;
//   - queued sessions are untouched; terminal sessions are unchanged;
//   - never replays provider work.
func (s *Store) SweepRestartRecovery(ctx context.Context, cutoff time.Time) (*SweepRestartRecoveryResult, error) {
	return s.SweepRestartRecoveryParams(ctx, RestartRecoveryParams{
		Cutoff: cutoff,
	})
}

// SweepRestartRecoveryWithNow executes SweepRestartRecovery with an explicit timestamp for role completed_at.
func (s *Store) SweepRestartRecoveryWithNow(ctx context.Context, cutoff, now time.Time) (*SweepRestartRecoveryResult, error) {
	return s.SweepRestartRecoveryParams(ctx, RestartRecoveryParams{
		Cutoff: cutoff,
		Now:    now,
	})
}

// SweepRestartRecoveryScoped executes SweepRestartRecovery filtered by projectID.
func (s *Store) SweepRestartRecoveryScoped(ctx context.Context, projectID string, cutoff, now time.Time) (*SweepRestartRecoveryResult, error) {
	return s.SweepRestartRecoveryParams(ctx, RestartRecoveryParams{
		ProjectID: projectID,
		Cutoff:    cutoff,
		Now:       now,
	})
}

// SweepRestartRecoverySession executes SweepRestartRecovery targeted at a single session ID.
func (s *Store) SweepRestartRecoverySession(ctx context.Context, sessionID string, cutoff, now time.Time) (*SweepRestartRecoveryResult, error) {
	return s.SweepRestartRecoveryParams(ctx, RestartRecoveryParams{
		SessionID: sessionID,
		Cutoff:    cutoff,
		Now:       now,
	})
}

// SweepRestartRecoveryParams executes SweepRestartRecovery with explicit configuration parameters.
func (s *Store) SweepRestartRecoveryParams(ctx context.Context, params RestartRecoveryParams) (*SweepRestartRecoveryResult, error) {
	if s == nil {
		return nil, errors.New("cannot execute sweep on nil store")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if params.Cutoff.IsZero() {
		return nil, errors.New("restart recovery requires an explicit non-zero cutoff")
	}

	writer, err := s.writerDB()
	if err != nil {
		return nil, err
	}

	cutoff := params.Cutoff.UTC()
	cutoffStr := formatUTCTimestamp(cutoff)

	now := params.Now
	if now.IsZero() {
		now = clock().UTC()
	} else {
		now = now.UTC()
	}
	nowStr := formatUTCTimestamp(now)

	retryPolicy := DefaultRetryPolicy("sweep_restart_recovery")
	var result *SweepRestartRecoveryResult

	err = withImmediate(ctx, writer, retryPolicy, func(conn *sql.Conn) error {
		// If SessionID is specified, verify it exists and matches ProjectID scoping.
		if params.SessionID != "" {
			var (
				dbProjectID string
				statusStr   string
			)
			err := conn.QueryRowContext(ctx, `SELECT project_id, status FROM sessions WHERE id = ?;`, params.SessionID).Scan(&dbProjectID, &statusStr)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return &SessionNotFoundError{SessionID: params.SessionID, ProjectID: params.ProjectID}
				}
				return fmt.Errorf("query session %q: %w", params.SessionID, sanitizeError(err))
			}
			if params.ProjectID != "" && dbProjectID != params.ProjectID {
				return &SessionNotFoundError{SessionID: params.SessionID, ProjectID: params.ProjectID}
			}
			sessionStatus := domain.SessionStatus(statusStr)
			if !isValidSessionStatus(sessionStatus) {
				return fmt.Errorf("%w: invalid session status %q", ErrMalformedData, statusStr)
			}
		}

		// Find stale reviewing sessions where claimed_at <= cutoff.
		query := `
			SELECT id, project_id, cancel_requested, claimed_at
			FROM sessions
			WHERE status = 'reviewing'
			  AND claimed_at IS NOT NULL
			  AND claimed_at <= ?
		`
		var queryArgs []any
		queryArgs = append(queryArgs, cutoffStr)

		if params.ProjectID != "" {
			query += ` AND project_id = ?`
			queryArgs = append(queryArgs, params.ProjectID)
		}
		if params.SessionID != "" {
			query += ` AND id = ?`
			queryArgs = append(queryArgs, params.SessionID)
		}
		query += ` ORDER BY claimed_at ASC, id ASC;`

		rows, err := conn.QueryContext(ctx, query, queryArgs...)
		if err != nil {
			return fmt.Errorf("query stale reviewing sessions: %w", sanitizeError(err))
		}
		defer rows.Close()

		type staleSession struct {
			id              string
			projectID       string
			cancelRequested int
			claimedAtStr    string
		}
		var sessions []staleSession
		for rows.Next() {
			var sess staleSession
			if err := rows.Scan(&sess.id, &sess.projectID, &sess.cancelRequested, &sess.claimedAtStr); err != nil {
				return fmt.Errorf("scan stale reviewing session: %w", sanitizeError(err))
			}
			sessions = append(sessions, sess)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("rows error stale reviewing sessions: %w", sanitizeError(err))
		}

		recoveredInfos := make([]RecoveredSessionInfo, 0)
		totalInFlightInterrupted := 0
		totalPendingInterrupted := 0

		for _, sess := range sessions {
			if sweepBeforeUpdateHook != nil {
				if hookErr := sweepBeforeUpdateHook(ctx, conn, sess.id); hookErr != nil {
					return hookErr
				}
			}

			// 1. In-flight roles become interrupted/process_restart.
			// completed_at must be >= started_at to satisfy the schema check constraint.
			resInFlight, err := conn.ExecContext(ctx, `
				UPDATE role_runs
				SET status = 'interrupted',
				    cause = 'process_restart',
				    completed_at = CASE WHEN ? >= started_at THEN ? ELSE started_at END
				WHERE session_id = ?
				  AND status = 'in_flight'
				  AND EXISTS (
				      SELECT 1 FROM sessions
				      WHERE id = role_runs.session_id
				        AND status = 'reviewing'
				        AND claimed_at <= ?
				  );
			`, nowStr, nowStr, sess.id, cutoffStr)
			if err != nil {
				return fmt.Errorf("update in_flight roles on restart recovery for session %q: %w", sess.id, sanitizeError(err))
			}
			inFlightAff, err := resInFlight.RowsAffected()
			if err != nil {
				return fmt.Errorf("rows affected in_flight restart recovery for session %q: %w", sess.id, sanitizeError(err))
			}

			// 2. Pending roles become interrupted/user_cancelled if cancel_requested = 1, else interrupted/process_restart.
			pendingCause := "process_restart"
			if sess.cancelRequested == 1 {
				pendingCause = "user_cancelled"
			}
			resPending, err := conn.ExecContext(ctx, `
				UPDATE role_runs
				SET status = 'interrupted',
				    cause = ?,
				    completed_at = ?
				WHERE session_id = ?
				  AND status = 'pending'
				  AND EXISTS (
				      SELECT 1 FROM sessions
				      WHERE id = role_runs.session_id
				        AND status = 'reviewing'
				        AND claimed_at <= ?
				  );
			`, pendingCause, nowStr, sess.id, cutoffStr)
			if err != nil {
				return fmt.Errorf("update pending roles on restart recovery for session %q: %w", sess.id, sanitizeError(err))
			}
			pendingAff, err := resPending.RowsAffected()
			if err != nil {
				return fmt.Errorf("rows affected pending restart recovery for session %q: %w", sess.id, sanitizeError(err))
			}

			totalAff := int(inFlightAff + pendingAff)
			totalInFlightInterrupted += int(inFlightAff)
			totalPendingInterrupted += int(pendingAff)

			recoveredInfos = append(recoveredInfos, RecoveredSessionInfo{
				SessionID:           sess.id,
				ProjectID:           sess.projectID,
				InFlightInterrupted: int(inFlightAff),
				PendingInterrupted:  int(pendingAff),
				TotalInterrupted:    totalAff,
				CancelRequested:     sess.cancelRequested == 1,
				NoOp:                totalAff == 0,
			})
		}

		if sweepBeforeCommitHook != nil {
			if hookErr := sweepBeforeCommitHook(ctx, conn); hookErr != nil {
				return hookErr
			}
		}

		result = &SweepRestartRecoveryResult{
			Cutoff:              cutoff,
			SessionsRecovered:   len(recoveredInfos),
			Sessions:            recoveredInfos,
			TotalInterrupted:    totalInFlightInterrupted + totalPendingInterrupted,
			InFlightInterrupted: totalInFlightInterrupted,
			PendingInterrupted:  totalPendingInterrupted,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// Aliases on Store for convenience.

func (s *Store) CancelSweep(ctx context.Context, sessionID string) (*SweepCancellationResult, error) {
	return s.SweepCancellation(ctx, sessionID)
}

func (s *Store) CancelSweepWithNow(ctx context.Context, sessionID string, now time.Time) (*SweepCancellationResult, error) {
	return s.SweepCancellationWithNow(ctx, sessionID, now)
}

func (s *Store) CancelSweepScoped(ctx context.Context, projectID, sessionID string, now time.Time) (*SweepCancellationResult, error) {
	return s.SweepCancellationScoped(ctx, projectID, sessionID, now)
}

func (s *Store) CutoffSweep(ctx context.Context, sessionID string, now time.Time) (*SweepCutoffResult, error) {
	return s.SweepCutoff(ctx, sessionID, now)
}

func (s *Store) CutoffSweepScoped(ctx context.Context, projectID, sessionID string, now time.Time) (*SweepCutoffResult, error) {
	return s.SweepCutoffScoped(ctx, projectID, sessionID, now)
}

func (s *Store) DeadlineSweep(ctx context.Context, sessionID string, now time.Time) (*SweepHardDeadlineResult, error) {
	return s.SweepHardDeadline(ctx, sessionID, now)
}

func (s *Store) HardDeadlineSweep(ctx context.Context, sessionID string, now time.Time) (*SweepHardDeadlineResult, error) {
	return s.SweepHardDeadline(ctx, sessionID, now)
}

func (s *Store) RestartSweep(ctx context.Context, cutoff time.Time) (*SweepRestartRecoveryResult, error) {
	return s.SweepRestartRecovery(ctx, cutoff)
}

func (s *Store) RestartRecoverySweep(ctx context.Context, cutoff time.Time) (*SweepRestartRecoveryResult, error) {
	return s.SweepRestartRecovery(ctx, cutoff)
}

// Package-level functions delegating to *Store.

func SweepCancellation(ctx context.Context, s *Store, sessionID string) (*SweepCancellationResult, error) {
	if s == nil {
		return nil, errors.New("cannot execute sweep on nil store")
	}
	return s.SweepCancellation(ctx, sessionID)
}

func SweepCancellationWithNow(ctx context.Context, s *Store, sessionID string, now time.Time) (*SweepCancellationResult, error) {
	if s == nil {
		return nil, errors.New("cannot execute sweep on nil store")
	}
	return s.SweepCancellationWithNow(ctx, sessionID, now)
}

func SweepCancellationScoped(ctx context.Context, s *Store, projectID, sessionID string, now time.Time) (*SweepCancellationResult, error) {
	if s == nil {
		return nil, errors.New("cannot execute sweep on nil store")
	}
	return s.SweepCancellationScoped(ctx, projectID, sessionID, now)
}

func CancelSweep(ctx context.Context, s *Store, sessionID string) (*SweepCancellationResult, error) {
	return SweepCancellation(ctx, s, sessionID)
}

func SweepCutoff(ctx context.Context, s *Store, sessionID string, now time.Time) (*SweepCutoffResult, error) {
	if s == nil {
		return nil, errors.New("cannot execute sweep on nil store")
	}
	return s.SweepCutoff(ctx, sessionID, now)
}

func SweepCutoffScoped(ctx context.Context, s *Store, projectID, sessionID string, now time.Time) (*SweepCutoffResult, error) {
	if s == nil {
		return nil, errors.New("cannot execute sweep on nil store")
	}
	return s.SweepCutoffScoped(ctx, projectID, sessionID, now)
}

func CutoffSweep(ctx context.Context, s *Store, sessionID string, now time.Time) (*SweepCutoffResult, error) {
	return SweepCutoff(ctx, s, sessionID, now)
}

func SweepHardDeadline(ctx context.Context, s *Store, sessionID string, now time.Time) (*SweepHardDeadlineResult, error) {
	if s == nil {
		return nil, errors.New("cannot execute sweep on nil store")
	}
	return s.SweepHardDeadline(ctx, sessionID, now)
}

func SweepHardDeadlineWithNow(ctx context.Context, s *Store, sessionID string, now time.Time) (*SweepHardDeadlineResult, error) {
	if s == nil {
		return nil, errors.New("cannot execute sweep on nil store")
	}
	return s.SweepHardDeadlineWithNow(ctx, sessionID, now)
}

func SweepHardDeadlineScoped(ctx context.Context, s *Store, projectID, sessionID string, now time.Time) (*SweepHardDeadlineResult, error) {
	if s == nil {
		return nil, errors.New("cannot execute sweep on nil store")
	}
	return s.SweepHardDeadlineScoped(ctx, projectID, sessionID, now)
}

func DeadlineSweep(ctx context.Context, s *Store, sessionID string, now time.Time) (*SweepHardDeadlineResult, error) {
	return SweepHardDeadline(ctx, s, sessionID, now)
}

func HardDeadlineSweep(ctx context.Context, s *Store, sessionID string, now time.Time) (*SweepHardDeadlineResult, error) {
	return SweepHardDeadline(ctx, s, sessionID, now)
}

func SweepRestartRecovery(ctx context.Context, s *Store, cutoff time.Time) (*SweepRestartRecoveryResult, error) {
	if s == nil {
		return nil, errors.New("cannot execute sweep on nil store")
	}
	return s.SweepRestartRecovery(ctx, cutoff)
}

func SweepRestartRecoveryWithNow(ctx context.Context, s *Store, cutoff, now time.Time) (*SweepRestartRecoveryResult, error) {
	if s == nil {
		return nil, errors.New("cannot execute sweep on nil store")
	}
	return s.SweepRestartRecoveryWithNow(ctx, cutoff, now)
}

func SweepRestartRecoveryScoped(ctx context.Context, s *Store, projectID string, cutoff, now time.Time) (*SweepRestartRecoveryResult, error) {
	if s == nil {
		return nil, errors.New("cannot execute sweep on nil store")
	}
	return s.SweepRestartRecoveryScoped(ctx, projectID, cutoff, now)
}

func SweepRestartRecoverySession(ctx context.Context, s *Store, sessionID string, cutoff, now time.Time) (*SweepRestartRecoveryResult, error) {
	if s == nil {
		return nil, errors.New("cannot execute sweep on nil store")
	}
	return s.SweepRestartRecoverySession(ctx, sessionID, cutoff, now)
}

func RestartSweep(ctx context.Context, s *Store, cutoff time.Time) (*SweepRestartRecoveryResult, error) {
	return SweepRestartRecovery(ctx, s, cutoff)
}

func RestartRecoverySweep(ctx context.Context, s *Store, cutoff time.Time) (*SweepRestartRecoveryResult, error) {
	return SweepRestartRecovery(ctx, s, cutoff)
}
