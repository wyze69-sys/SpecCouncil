package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
)

// CancelResult represents the authoritative outcome of a cancellation request mutation.
type CancelResult struct {
	Effective       bool                  `json:"effective"`
	AlreadyTerminal bool                  `json:"already_terminal,omitempty"`
	NoOp            bool                  `json:"no_op,omitempty"`
	SessionID       string                `json:"session_id"`
	ProjectID       string                `json:"project_id"`
	Status          domain.SessionStatus  `json:"status"`
	CancelRequested bool                  `json:"cancel_requested"`
	TerminalReason  domain.TerminalReason `json:"terminal_reason,omitempty"`
	Session         *SessionStatus        `json:"session,omitempty"`
}

// IsEffective reports whether this request effectively changed cancel_requested from 0 to 1.
func (r *CancelResult) IsEffective() bool {
	return r != nil && r.Effective
}

// IsAlreadyTerminal reports whether the session was already in a terminal state.
func (r *CancelResult) IsAlreadyTerminal() bool {
	return r != nil && r.AlreadyTerminal
}

// IsNoOp reports whether the operation was a no-op (either already terminal or repeated request).
func (r *CancelResult) IsNoOp() bool {
	return r != nil && r.NoOp
}

// SessionStatus returns the underlying committed session read model.
func (r *CancelResult) SessionStatus() *SessionStatus {
	if r == nil {
		return nil
	}
	return r.Session
}

var (
	// cancelBeforeCommitHook allows deterministic test verification of rollback on injected failure.
	cancelBeforeCommitHook func(ctx context.Context, conn *sql.Conn) error

	// cancelBeforeUpdateHook allows deterministic test verification of conflict / races before update.
	cancelBeforeUpdateHook func(ctx context.Context, conn *sql.Conn, sessionID string) error
)

// RequestCancellation attempts to atomically set cancel_requested from 0 to 1 for the given session.
// It executes within a dedicated immediate transaction using withImmediate.
//
// Rules enforced:
//  1. Unknown session returns a typed SessionNotFoundError matching ErrNotFound.
//  2. queued and reviewing sessions atomically change cancel_requested from 0 to 1
//     and return Effective: true along with the committed session state.
//  3. Repeating the request is idempotent: 1 -> 1 succeeds without changing other fields
//     or timestamps, returning Effective: false.
//  4. Terminal sessions (complete, partial, failed) are not reopened, have no role states changed,
//     and return a typed already-terminal/no-op result (Effective: false, AlreadyTerminal: true, NoOp: true)
//     according to the existing read model contract.
//  5. The mutation never changes status, role runs, terminal reason, counts, timing,
//     or findings.
func (s *Store) RequestCancellation(ctx context.Context, sessionID string) (*CancelResult, error) {
	return s.RequestCancellationScoped(ctx, "", sessionID)
}

// RequestCancellationScoped executes RequestCancellation, scoping the session lookup to projectID when non-empty.
func (s *Store) RequestCancellationScoped(ctx context.Context, projectID, sessionID string) (*CancelResult, error) {
	if s == nil {
		return nil, errors.New("cannot request cancellation from nil store")
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

	retryPolicy := DefaultRetryPolicy("request_cancellation")
	var (
		effective       bool
		alreadyTerminal bool
	)

	err = withImmediate(ctx, writer, retryPolicy, func(conn *sql.Conn) error {
		// 1. Query existing session state within the immediate transaction.
		var (
			dbProjectID     string
			statusStr       string
			cancelRequested int
		)
		query := `SELECT project_id, status, cancel_requested FROM sessions WHERE id = ?;`
		err := conn.QueryRowContext(ctx, query, sessionID).Scan(&dbProjectID, &statusStr, &cancelRequested)
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

		// 2. Terminal sessions are not reopened, have no role states changed, and return no-op.
		if sessionStatus.IsTerminal() {
			effective = false
			alreadyTerminal = true
			return nil
		}

		// 3. Repeating the request is idempotent: 1 -> 1 succeeds without changing other fields or timestamps.
		if cancelRequested == 1 {
			effective = false
			alreadyTerminal = false
			return nil
		}

		// Optional test hook for update.
		if cancelBeforeUpdateHook != nil {
			if hookErr := cancelBeforeUpdateHook(ctx, conn, sessionID); hookErr != nil {
				return hookErr
			}
		}

		// 4. Update queued or reviewing session: 0 -> 1.
		res, err := conn.ExecContext(ctx, `
			UPDATE sessions
			SET cancel_requested = 1
			WHERE id = ? AND cancel_requested = 0 AND status IN ('queued', 'reviewing');
		`, sessionID)
		if err != nil {
			return fmt.Errorf("update cancel_requested for session %q: %w", sessionID, sanitizeError(err))
		}

		rowsAffected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("check rows affected for session %q: %w", sessionID, sanitizeError(err))
		}

		if rowsAffected == 1 {
			effective = true
		} else {
			// Concurrently updated by another transaction.
			effective = false
		}

		// Optional test hook for commit.
		if cancelBeforeCommitHook != nil {
			if hookErr := cancelBeforeCommitHook(ctx, conn); hookErr != nil {
				return hookErr
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// 5. Read committed session state according to read model contract.
	sessionStatus, err := s.ReadStatusScoped(ctx, projectID, sessionID)
	if err != nil {
		return nil, err
	}

	return &CancelResult{
		Effective:       effective,
		AlreadyTerminal: alreadyTerminal,
		NoOp:            alreadyTerminal || !effective,
		SessionID:       sessionStatus.SessionID,
		ProjectID:       sessionStatus.ProjectID,
		Status:          sessionStatus.Status,
		CancelRequested: sessionStatus.CancelRequested,
		TerminalReason:  sessionStatus.TerminalReason,
		Session:         sessionStatus,
	}, nil
}

// CancelSession is an alias for RequestCancellation.
func (s *Store) CancelSession(ctx context.Context, sessionID string) (*CancelResult, error) {
	return s.RequestCancellation(ctx, sessionID)
}

// Cancel is an alias for RequestCancellation.
func (s *Store) Cancel(ctx context.Context, sessionID string) (*CancelResult, error) {
	return s.RequestCancellation(ctx, sessionID)
}

// Package-level functions delegating to *Store.

// RequestCancellation package-level function delegating to s.RequestCancellation.
func RequestCancellation(ctx context.Context, s *Store, sessionID string) (*CancelResult, error) {
	if s == nil {
		return nil, errors.New("cannot request cancellation from nil store")
	}
	return s.RequestCancellation(ctx, sessionID)
}

// RequestCancellationScoped package-level function delegating to s.RequestCancellationScoped.
func RequestCancellationScoped(ctx context.Context, s *Store, projectID, sessionID string) (*CancelResult, error) {
	if s == nil {
		return nil, errors.New("cannot request cancellation from nil store")
	}
	return s.RequestCancellationScoped(ctx, projectID, sessionID)
}

// CancelSession package-level function delegating to s.CancelSession.
func CancelSession(ctx context.Context, s *Store, sessionID string) (*CancelResult, error) {
	return RequestCancellation(ctx, s, sessionID)
}

// Cancel package-level function delegating to s.Cancel.
func Cancel(ctx context.Context, s *Store, sessionID string) (*CancelResult, error) {
	return RequestCancellation(ctx, s, sessionID)
}
