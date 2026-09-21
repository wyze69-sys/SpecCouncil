package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/review"
)

var (
	// ErrSessionNotReady is returned when composition is attempted on a session whose roles are not all terminal.
	ErrSessionNotReady = errors.New("session is not ready for composition")

	// ErrCompositionNotReady is an alias for ErrSessionNotReady.
	ErrCompositionNotReady = ErrSessionNotReady

	// ErrNotReady is an alias for ErrSessionNotReady.
	ErrNotReady = ErrSessionNotReady
)

// SessionNotReadyError is returned when a session is not ready for terminal composition.
type SessionNotReadyError struct {
	SessionID     string
	ProjectID     string
	Status        domain.SessionStatus
	PendingRoles  []domain.Role
	InFlightRoles []domain.Role
	Message       string
}

func (e *SessionNotReadyError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Message != "" {
		return fmt.Sprintf("session %q not ready for composition: %s", e.SessionID, e.Message)
	}
	if len(e.PendingRoles) > 0 || len(e.InFlightRoles) > 0 {
		return fmt.Sprintf("session %q not ready for composition: pending=%v in_flight=%v",
			e.SessionID, e.PendingRoles, e.InFlightRoles)
	}
	return fmt.Sprintf("session %q not ready for composition (status: %s)", e.SessionID, e.Status)
}

func (e *SessionNotReadyError) Is(target error) bool {
	return target == ErrSessionNotReady || target == ErrCompositionNotReady || target == ErrNotReady
}

// CompositionNotReadyError is an alias for SessionNotReadyError.
type CompositionNotReadyError = SessionNotReadyError

var (
	// composeBeforeCommitHook allows deterministic test verification of rollback on injected failure.
	composeBeforeCommitHook func(ctx context.Context, conn *sql.Conn) error

	// composeBeforeUpdateHook allows deterministic test verification of conflict / races before update.
	composeBeforeUpdateHook func(ctx context.Context, conn *sql.Conn, sessionID string) error
)

// ComposeParams encapsulates options for session composition.
type ComposeParams struct {
	ProjectID string    // optional: validated against session if non-empty
	SessionID string    // required
	Now       time.Time // optional: defaults to clock().UTC() if zero
}

// ComposeResult represents the authoritative outcome of a session composition.
type ComposeResult struct {
	Composed         bool                  `json:"composed"`
	AlreadyTerminal  bool                  `json:"already_terminal,omitempty"`
	AlreadyFinalized bool                  `json:"already_finalized,omitempty"`
	SessionID        string                `json:"session_id"`
	ProjectID        string                `json:"project_id"`
	Status           domain.SessionStatus  `json:"status"`
	TerminalReason   domain.TerminalReason `json:"terminal_reason,omitempty"`
	TerminalAt       time.Time             `json:"terminal_at"`
	Report           *review.Report        `json:"report,omitempty"`
}

// IsComposed reports whether this execution performed the terminal composition.
func (r *ComposeResult) IsComposed() bool {
	return r != nil && r.Composed
}

// IsAlreadyTerminal reports whether the session was already in a terminal state.
func (r *ComposeResult) IsAlreadyTerminal() bool {
	return r != nil && (r.AlreadyTerminal || r.AlreadyFinalized)
}

// IsAlreadyFinalized is an alias for IsAlreadyTerminal.
func (r *ComposeResult) IsAlreadyFinalized() bool {
	return r.IsAlreadyTerminal()
}

// TerminalReport returns the deterministic terminal report.
func (r *ComposeResult) TerminalReport() *review.Report {
	if r == nil {
		return nil
	}
	return r.Report
}

// ComposeSession composes a terminal session exactly once from committed role outcomes.
func (s *Store) ComposeSession(ctx context.Context, sessionID string) (*ComposeResult, error) {
	return s.ComposeSessionWithParams(ctx, ComposeParams{SessionID: sessionID})
}

// ComposeSessionScoped composes a terminal session with project scoping.
func (s *Store) ComposeSessionScoped(ctx context.Context, projectID, sessionID string) (*ComposeResult, error) {
	return s.ComposeSessionWithParams(ctx, ComposeParams{ProjectID: projectID, SessionID: sessionID})
}

// Compose is an alias for ComposeSession.
func (s *Store) Compose(ctx context.Context, sessionID string) (*ComposeResult, error) {
	return s.ComposeSession(ctx, sessionID)
}

// ComposeScoped is an alias for ComposeSessionScoped.
func (s *Store) ComposeScoped(ctx context.Context, projectID, sessionID string) (*ComposeResult, error) {
	return s.ComposeSessionScoped(ctx, projectID, sessionID)
}

// FinalizeSession is an alias for ComposeSession.
func (s *Store) FinalizeSession(ctx context.Context, sessionID string) (*ComposeResult, error) {
	return s.ComposeSession(ctx, sessionID)
}

// FinalizeSessionScoped is an alias for ComposeSessionScoped.
func (s *Store) FinalizeSessionScoped(ctx context.Context, projectID, sessionID string) (*ComposeResult, error) {
	return s.ComposeSessionScoped(ctx, projectID, sessionID)
}

// Finalize is an alias for ComposeSession.
func (s *Store) Finalize(ctx context.Context, sessionID string) (*ComposeResult, error) {
	return s.ComposeSession(ctx, sessionID)
}

// ComposeSessionWithParams executes the transactional session composition.
//
// Rules enforced:
//  1. Reads all four committed role runs, findings, and basis references inside one BEGIN IMMEDIATE transaction.
//  2. Refuses composition unless all four roles are terminal and no role is pending or in-flight;
//     returns typed SessionNotReadyError and makes no writes.
//  3. Applies the frozen review.Compose verdict rules and deterministic role, finding, and citation ordering.
//  4. Updates the session atomically to complete, partial, or failed, with exact terminal reason,
//     completed/incomplete counts, and UTC RFC3339Nano terminal_at.
//  5. Uses compare-and-set predicates requiring the session to remain 'reviewing'; duplicate composition
//     is an idempotent read of the committed terminal result, while a stale concurrent composer loses
//     without overwriting terminal state.
//  6. Never changes immutable snapshot/evidence/finding/reference rows, role rows, cancellation state,
//     claim/cutoff/deadline timestamps, or persisted role call metadata.
//  7. Performs no provider call and holds no transaction while doing provider work.
//  8. Returns the same deterministic terminal report through the read-only path; non-terminal report
//     requests return the existing typed SessionNotTerminalError.
//  9. Rolls back entirely on injected commit failure or malformed persisted role data; no partial
//     terminal session is left behind.
//  10. Preserves project scoping and existing not-found semantics.
func (s *Store) ComposeSessionWithParams(ctx context.Context, params ComposeParams) (*ComposeResult, error) {
	if s == nil {
		return nil, errors.New("cannot compose session on nil store")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sessionID := strings.TrimSpace(params.SessionID)
	if sessionID == "" {
		return nil, &SessionNotFoundError{SessionID: params.SessionID, ProjectID: params.ProjectID}
	}

	writer, err := s.writerDB()
	if err != nil {
		return nil, err
	}
	retryPolicy := DefaultRetryPolicy("compose_session")

	var (
		composed        bool
		alreadyTerminal bool
		finalStatus     domain.SessionStatus
		finalReason     domain.TerminalReason
		finalTerminalAt time.Time
		finalReport     *review.Report
		resolvedProjID  string
	)

	err = withImmediate(ctx, writer, retryPolicy, func(conn *sql.Conn) error {
		// 1. Read session row within the immediate transaction.
		var (
			dbProjectID     string
			dbSnapshotID    string
			dbSnapshotHash  string
			statusStr       string
			cancelRequested int
			compCount       int
			incompCount     int
			termReasonStr   sql.NullString
			createdAtStr    string
			claimedAtStr    sql.NullString
			dispCutoffStr   sql.NullString
			hardDeadStr     sql.NullString
			termAtStr       sql.NullString
		)
		querySess := `SELECT
			s.project_id, s.snapshot_id, sn.hash, s.status, s.cancel_requested,
			s.completed_role_count, s.incomplete_role_count, s.terminal_reason,
			s.created_at, s.claimed_at, s.dispatch_cutoff_at, s.hard_deadline_at, s.terminal_at
		FROM sessions s
		JOIN snapshots sn ON s.snapshot_id = sn.id
		WHERE s.id = ?;`

		err := conn.QueryRowContext(ctx, querySess, sessionID).Scan(
			&dbProjectID, &dbSnapshotID, &dbSnapshotHash, &statusStr, &cancelRequested,
			&compCount, &incompCount, &termReasonStr,
			&createdAtStr, &claimedAtStr, &dispCutoffStr, &hardDeadStr, &termAtStr,
		)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return &SessionNotFoundError{SessionID: sessionID, ProjectID: params.ProjectID}
			}
			return fmt.Errorf("query session %q: %w", sessionID, sanitizeError(err))
		}

		if params.ProjectID != "" && dbProjectID != params.ProjectID {
			return &SessionNotFoundError{SessionID: sessionID, ProjectID: params.ProjectID}
		}
		resolvedProjID = dbProjectID

		sessStatus := domain.SessionStatus(statusStr)
		if !isValidSessionStatus(sessStatus) {
			return fmt.Errorf("%w: invalid session status %q", ErrMalformedData, statusStr)
		}

		// Check if session is already terminal: duplicate composition is an idempotent read.
		if sessStatus.IsTerminal() {
			composed = false
			alreadyTerminal = true
			finalStatus = sessStatus
			if termReasonStr.Valid && termReasonStr.String != "" {
				finalReason = domain.TerminalReason(termReasonStr.String)
			}
			if termAtStr.Valid && termAtStr.String != "" {
				t, parseErr := parseUTCTimestamp(termAtStr.String)
				if parseErr == nil {
					finalTerminalAt = t
				}
			}
			return nil
		}

		// Refuse composition unless reviewing (e.g. queued).
		if sessStatus != domain.SessionReviewing {
			return &SessionNotReadyError{
				SessionID: sessionID,
				ProjectID: dbProjectID,
				Status:    sessStatus,
				Message:   fmt.Sprintf("session is in status %q, not reviewing", sessStatus),
			}
		}

		var claimedAt *time.Time
		if claimedAtStr.Valid && claimedAtStr.String != "" {
			ca, parseErr := parseUTCTimestamp(claimedAtStr.String)
			if parseErr != nil {
				return fmt.Errorf("%w: claimed_at: %w", ErrMalformedData, parseErr)
			}
			claimedAt = &ca
		}

		// 2. Query all four committed role runs inside the transaction.
		queryRoles := `SELECT
			id, role, status, cause, error_category, error_message, call_count,
			started_at, completed_at, created_at
		FROM role_runs
		WHERE session_id = ?;`

		rows, err := conn.QueryContext(ctx, queryRoles, sessionID)
		if err != nil {
			return fmt.Errorf("query role_runs for %q: %w", sessionID, sanitizeError(err))
		}
		defer rows.Close()

		type roleInfo struct {
			id            string
			role          domain.Role
			status        domain.RoleStatus
			cause         domain.InterruptCause
			errorCategory domain.ErrorCategory
			errorMessage  string
			callCount     int
			startedAt     *time.Time
			completedAt   *time.Time
			createdAt     time.Time
		}

		byRole := make(map[domain.Role]roleInfo, domain.RoleCount)
		var pendingRoles []domain.Role
		var inFlightRoles []domain.Role

		for rows.Next() {
			var (
				rID       string
				roleStr   string
				statStr   string
				causeStr  sql.NullString
				errCatStr sql.NullString
				errMsgStr sql.NullString
				callCnt   int
				startStr  sql.NullString
				compStr   sql.NullString
				creatStr  string
			)
			if err := rows.Scan(&rID, &roleStr, &statStr, &causeStr, &errCatStr, &errMsgStr,
				&callCnt, &startStr, &compStr, &creatStr); err != nil {
				return fmt.Errorf("scan role_run: %w", sanitizeError(err))
			}

			rRole := domain.Role(roleStr)
			if !domain.IsValidRole(rRole) {
				return fmt.Errorf("%w: unknown role %q", ErrMalformedData, roleStr)
			}
			if _, dup := byRole[rRole]; dup {
				return fmt.Errorf("%w: duplicate role %q", ErrMalformedData, roleStr)
			}

			rStatus := domain.RoleStatus(statStr)
			if !domain.IsValidRoleStatus(rStatus) {
				return fmt.Errorf("%w: invalid role status %q for %s", ErrMalformedData, statStr, rRole)
			}

			var cause domain.InterruptCause
			if causeStr.Valid && causeStr.String != "" {
				cause = domain.InterruptCause(causeStr.String)
				if !domain.IsValidInterruptCause(cause) {
					return fmt.Errorf("%w: invalid cause %q for %s", ErrMalformedData, causeStr.String, rRole)
				}
			}

			var errCat domain.ErrorCategory
			if errCatStr.Valid && errCatStr.String != "" {
				errCat = domain.ErrorCategory(errCatStr.String)
				if !isValidErrorCategory(errCat) {
					return fmt.Errorf("%w: invalid error category %q for %s", ErrMalformedData, errCatStr.String, rRole)
				}
			}

			startedAt, err := parseNullableUTCTimestamp(startStr)
			if err != nil {
				return fmt.Errorf("%w: role %s started_at: %w", ErrMalformedData, rRole, err)
			}
			completedAt, err := parseNullableUTCTimestamp(compStr)
			if err != nil {
				return fmt.Errorf("%w: role %s completed_at: %w", ErrMalformedData, rRole, err)
			}
			createdAt, err := parseUTCTimestamp(creatStr)
			if err != nil {
				return fmt.Errorf("%w: role %s created_at: %w", ErrMalformedData, rRole, err)
			}

			var errMsg string
			if errMsgStr.Valid {
				errMsg = errMsgStr.String
			}

			info := roleInfo{
				id:            rID,
				role:          rRole,
				status:        rStatus,
				cause:         cause,
				errorCategory: errCat,
				errorMessage:  errMsg,
				callCount:     callCnt,
				startedAt:     startedAt,
				completedAt:   completedAt,
				createdAt:     createdAt,
			}
			byRole[rRole] = info

			if rStatus == domain.RolePending {
				pendingRoles = append(pendingRoles, rRole)
			} else if rStatus == domain.RoleInFlight {
				inFlightRoles = append(inFlightRoles, rRole)
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("role_runs error: %w", sanitizeError(err))
		}

		if len(byRole) != domain.RoleCount {
			return fmt.Errorf("%w: session %q has %d roles, expected %d",
				ErrMalformedData, sessionID, len(byRole), domain.RoleCount)
		}

		// Refuse composition unless all four roles are terminal and zero in-flight/pending.
		if len(pendingRoles) > 0 || len(inFlightRoles) > 0 {
			return &SessionNotReadyError{
				SessionID:     sessionID,
				ProjectID:     dbProjectID,
				Status:        sessStatus,
				PendingRoles:  pendingRoles,
				InFlightRoles: inFlightRoles,
			}
		}

		// 3. Query findings for completed roles inside the transaction.
		queryFindings := `SELECT
			f.id, rr.role, f.finding_id, f.kind, f.severity, f.category, f.issue,
			f.recommendation, COALESCE(eu.unit_id, '')
		FROM findings f
		JOIN role_runs rr ON f.role_run_id = rr.id
		LEFT JOIN evidence_units eu ON eu.id = f.anchor_unit_id
		WHERE rr.session_id = ? AND rr.status = 'complete';`

		fRows, err := conn.QueryContext(ctx, queryFindings, sessionID)
		if err != nil {
			return fmt.Errorf("query findings for %q: %w", sessionID, sanitizeError(err))
		}
		defer fRows.Close()

		type rawFinding struct {
			dbID           string
			role           domain.Role
			findingID      string
			kind           domain.FindingKind
			severity       domain.Severity
			category       string
			issue          string
			recommendation string
			anchorRef      string
		}

		var rawFindings []rawFinding
		for fRows.Next() {
			var rf rawFinding
			var roleStr, kindStr, sevStr string
			if err := fRows.Scan(&rf.dbID, &roleStr, &rf.findingID, &kindStr, &sevStr, &rf.category,
				&rf.issue, &rf.recommendation, &rf.anchorRef); err != nil {
				return fmt.Errorf("scan finding: %w", sanitizeError(err))
			}
			rf.role = domain.Role(roleStr)
			rf.kind = domain.FindingKind(kindStr)
			if !domain.IsValidFindingKind(rf.kind) {
				return fmt.Errorf("%w: invalid finding kind %q", ErrMalformedData, kindStr)
			}
			rf.severity = domain.Severity(sevStr)
			if !domain.IsValidSeverity(rf.severity) {
				return fmt.Errorf("%w: invalid severity %q", ErrMalformedData, sevStr)
			}
			rawFindings = append(rawFindings, rf)
		}
		if err := fRows.Err(); err != nil {
			return fmt.Errorf("findings rows: %w", sanitizeError(err))
		}

		// 4. Query finding_basis_refs for completed roles inside the transaction.
		refsByFindingDBID := make(map[string][]string)
		if len(rawFindings) > 0 {
			queryRefs := `SELECT
				fbr.finding_id, eu.unit_id, fbr.ordinal
			FROM finding_basis_refs fbr
			JOIN findings f ON fbr.finding_id = f.id
			JOIN role_runs rr ON f.role_run_id = rr.id
			JOIN evidence_units eu ON fbr.evidence_unit_id = eu.id
			WHERE rr.session_id = ? AND rr.status = 'complete'
			ORDER BY fbr.finding_id, fbr.ordinal ASC;`

			rRows, err := conn.QueryContext(ctx, queryRefs, sessionID)
			if err != nil {
				return fmt.Errorf("query basis refs for %q: %w", sessionID, sanitizeError(err))
			}
			defer rRows.Close()

			for rRows.Next() {
				var fID, uID string
				var ord int
				if err := rRows.Scan(&fID, &uID, &ord); err != nil {
					return fmt.Errorf("scan basis ref: %w", sanitizeError(err))
				}
				if ord < domain.MinBasisRefsPerFinding || ord > domain.MaxBasisRefsPerFinding {
					return fmt.Errorf("%w: invalid basis ref ordinal %d", ErrMalformedData, ord)
				}
				refsByFindingDBID[fID] = append(refsByFindingDBID[fID], uID)
			}
			if err := rRows.Err(); err != nil {
				return fmt.Errorf("basis refs rows: %w", sanitizeError(err))
			}
		}

		// Assemble findings map by canonical role.
		findingsByRole := make(map[domain.Role][]domain.Finding)
		for _, rf := range rawFindings {
			refs := refsByFindingDBID[rf.dbID]
			if refs == nil {
				refs = make([]string, 0)
			}
			findingsByRole[rf.role] = append(findingsByRole[rf.role], domain.Finding{
				ID:             rf.findingID,
				Kind:           rf.kind,
				Severity:       rf.severity,
				Category:       rf.category,
				Issue:          rf.issue,
				Recommendation: rf.recommendation,
				BasisRefs:      refs,
				AnchorRef:      rf.anchorRef,
			})
		}

		// 5. Apply frozen review.Compose verdict rules.
		composerRoles := make([]review.RoleRow, 0, domain.RoleCount)
		for _, role := range domain.Roles {
			rInfo, ok := byRole[role]
			if !ok {
				return fmt.Errorf("%w: missing canonical role %q", ErrMalformedData, role)
			}
			composerRoles = append(composerRoles, review.RoleRow{
				Role:           rInfo.role,
				Status:         rInfo.status,
				InterruptCause: rInfo.cause,
			})
		}

		verdict, err := review.Compose(review.ComposerInput{Roles: composerRoles})
		if err != nil {
			return fmt.Errorf("%w: %v", ErrMalformedData, err)
		}

		// 6. Test hook before update.
		if composeBeforeUpdateHook != nil {
			if hookErr := composeBeforeUpdateHook(ctx, conn, sessionID); hookErr != nil {
				return hookErr
			}
		}

		// 7. Compute terminal_at in strict UTC RFC3339Nano.
		now := params.Now
		if now.IsZero() {
			now = clock().UTC()
		} else {
			now = now.UTC()
		}
		if claimedAt != nil && now.Before(*claimedAt) {
			now = *claimedAt
		}
		terminalAtStr := formatUTCTimestamp(now)

		// 8. Execute compare-and-set UPDATE requiring session to remain 'reviewing'.
		updateSQL := `UPDATE sessions
		SET status = ?,
			terminal_reason = ?,
			completed_role_count = ?,
			incomplete_role_count = ?,
			terminal_at = ?
		WHERE id = ? AND status = 'reviewing';`

		res, err := conn.ExecContext(ctx, updateSQL,
			string(verdict.Status),
			string(verdict.Reason),
			verdict.CompletedRoleCount,
			verdict.IncompleteRoleCount,
			terminalAtStr,
			sessionID,
		)
		if err != nil {
			return fmt.Errorf("update session %q: %w", sessionID, sanitizeError(err))
		}

		rowsAffected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("check rows affected: %w", sanitizeError(err))
		}

		if rowsAffected == 1 {
			composed = true
			alreadyTerminal = false
			finalStatus = verdict.Status
			finalReason = verdict.Reason
			finalTerminalAt = now
		} else {
			// Compare-and-set lost race: re-verify if session is now terminal.
			var curStat, curReason, curTermAt sql.NullString
			checkErr := conn.QueryRowContext(ctx,
				"SELECT status, terminal_reason, terminal_at FROM sessions WHERE id = ?;", sessionID).
				Scan(&curStat, &curReason, &curTermAt)
			if checkErr != nil {
				return fmt.Errorf("recheck session %q: %w", sessionID, sanitizeError(checkErr))
			}
			sSt := domain.SessionStatus(curStat.String)
			if sSt.IsTerminal() {
				composed = false
				alreadyTerminal = true
				finalStatus = sSt
				if curReason.Valid {
					finalReason = domain.TerminalReason(curReason.String)
				}
				if curTermAt.Valid {
					if t, pErr := parseUTCTimestamp(curTermAt.String); pErr == nil {
						finalTerminalAt = t
					}
				}
			} else {
				return fmt.Errorf("session %q compare-and-set failed (current status: %q)", sessionID, curStat.String)
			}
		}

		// 9. Assemble outcomes and build report using review.BuildReport.
		outcomes := make([]review.RoleOutcome, 0, domain.RoleCount)
		for _, role := range domain.Roles {
			rInfo := byRole[role]
			outcomes = append(outcomes, review.RoleOutcome{
				Role:           rInfo.role,
				Status:         rInfo.status,
				Findings:       findingsByRole[rInfo.role],
				ErrorCategory:  rInfo.errorCategory,
				InterruptCause: rInfo.cause,
				CallCount:      rInfo.callCount,
			})
		}

		reportVerdict := verdict
		if !composed && alreadyTerminal {
			reportVerdict = review.Verdict{
				Status:              finalStatus,
				Reason:              finalReason,
				CompletedRoleCount:  verdict.CompletedRoleCount,
				IncompleteRoleCount: verdict.IncompleteRoleCount,
			}
		}

		rep := review.BuildReport(
			sessionID,
			dbSnapshotID,
			dbSnapshotHash,
			outcomes,
			reportVerdict,
			cancelRequested == 1,
		)
		if rep.Findings == nil {
			rep.Findings = make([]review.ReportFinding, 0)
		}
		finalReport = &rep

		// 10. Test hook before commit.
		if composeBeforeCommitHook != nil {
			if hookErr := composeBeforeCommitHook(ctx, conn); hookErr != nil {
				return hookErr
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if alreadyTerminal {
		// Idempotent read of committed terminal state.
		st, err := s.ReadStatusScoped(ctx, resolvedProjID, sessionID)
		if err != nil {
			return nil, err
		}
		rep, err := s.ReadReportScoped(ctx, resolvedProjID, sessionID)
		if err != nil {
			return nil, err
		}
		var termAt time.Time
		if st.TerminalAt != nil {
			termAt = *st.TerminalAt
		}
		return &ComposeResult{
			Composed:         false,
			AlreadyTerminal:  true,
			AlreadyFinalized: true,
			SessionID:        st.SessionID,
			ProjectID:        st.ProjectID,
			Status:           st.Status,
			TerminalReason:   st.TerminalReason,
			TerminalAt:       termAt,
			Report:           rep,
		}, nil
	}

	return &ComposeResult{
		Composed:         true,
		AlreadyTerminal:  false,
		AlreadyFinalized: false,
		SessionID:        sessionID,
		ProjectID:        resolvedProjID,
		Status:           finalStatus,
		TerminalReason:   finalReason,
		TerminalAt:       finalTerminalAt,
		Report:           finalReport,
	}, nil
}

// Package-level functions delegating to *Store.

// ComposeSession package-level function delegating to s.ComposeSession.
func ComposeSession(ctx context.Context, s *Store, sessionID string) (*ComposeResult, error) {
	if s == nil {
		return nil, errors.New("cannot compose session on nil store")
	}
	return s.ComposeSession(ctx, sessionID)
}

// ComposeSessionScoped package-level function delegating to s.ComposeSessionScoped.
func ComposeSessionScoped(ctx context.Context, s *Store, projectID, sessionID string) (*ComposeResult, error) {
	if s == nil {
		return nil, errors.New("cannot compose session on nil store")
	}
	return s.ComposeSessionScoped(ctx, projectID, sessionID)
}

// ComposeSessionWithParams package-level function delegating to s.ComposeSessionWithParams.
func ComposeSessionWithParams(ctx context.Context, s *Store, params ComposeParams) (*ComposeResult, error) {
	if s == nil {
		return nil, errors.New("cannot compose session on nil store")
	}
	return s.ComposeSessionWithParams(ctx, params)
}

// Compose package-level function delegating to s.Compose.
func Compose(ctx context.Context, s *Store, sessionID string) (*ComposeResult, error) {
	return ComposeSession(ctx, s, sessionID)
}

// ComposeScoped package-level function delegating to s.ComposeScoped.
func ComposeScoped(ctx context.Context, s *Store, projectID, sessionID string) (*ComposeResult, error) {
	return ComposeSessionScoped(ctx, s, projectID, sessionID)
}

// FinalizeSession package-level function delegating to s.FinalizeSession.
func FinalizeSession(ctx context.Context, s *Store, sessionID string) (*ComposeResult, error) {
	return ComposeSession(ctx, s, sessionID)
}

// FinalizeSessionScoped package-level function delegating to s.FinalizeSessionScoped.
func FinalizeSessionScoped(ctx context.Context, s *Store, projectID, sessionID string) (*ComposeResult, error) {
	return ComposeSessionScoped(ctx, s, projectID, sessionID)
}

// Finalize package-level function delegating to s.Finalize.
func Finalize(ctx context.Context, s *Store, sessionID string) (*ComposeResult, error) {
	return ComposeSession(ctx, s, sessionID)
}
