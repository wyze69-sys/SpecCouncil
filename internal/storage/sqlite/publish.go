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

var (
	// ErrPublicationConflict is returned when a role publication attempt fails the compare-and-set
	// guard because the role is not currently in_flight (stale, duplicate, or already terminal).
	ErrPublicationConflict = errors.New("role publication conflict")

	// ErrRoleNotRunning is an alias for ErrPublicationConflict when the role is not in_flight.
	ErrRoleNotRunning = ErrPublicationConflict

	// ErrRoleAlreadyTerminal is an alias for ErrPublicationConflict when the role is already terminal.
	ErrRoleAlreadyTerminal = ErrPublicationConflict

	// ErrInvalidPublication is returned when publication parameters violate validation rules.
	ErrInvalidPublication = errors.New("invalid publication parameters")
)

// PublicationConflictError is the typed error returned when compare-and-set on (session_id, role_run_id, status=in_flight) fails.
type PublicationConflictError struct {
	SessionID      string
	RoleRunID      string
	Role           domain.Role
	ExpectedStatus domain.RoleStatus
	ActualStatus   domain.RoleStatus
	Message        string
}

func (e *PublicationConflictError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Message != "" {
		return fmt.Sprintf("publication conflict for session %q role_run %q: %s", e.SessionID, e.RoleRunID, e.Message)
	}
	return fmt.Sprintf("publication conflict for session %q role_run %q (role %q): expected status %q, found %q",
		e.SessionID, e.RoleRunID, e.Role, e.ExpectedStatus, e.ActualStatus)
}

func (e *PublicationConflictError) Is(target error) bool {
	return target == ErrPublicationConflict || target == ErrRoleNotRunning || target == ErrRoleAlreadyTerminal
}

// PublicationValidationError is returned when publication parameters violate constraints.
type PublicationValidationError struct {
	Field   string
	Message string
}

func (e *PublicationValidationError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("invalid publication %s: %s", e.Field, e.Message)
}

func (e *PublicationValidationError) Is(target error) bool {
	return target == ErrInvalidPublication
}

// PublishSuccessParams encapsulates validated input for a successful role outcome publication.
type PublishSuccessParams struct {
	ProjectID   string      // optional: validated against session if non-empty
	SessionID   string      // required
	RoleRunID   string      // required
	Role        domain.Role // optional: validated against role_run if non-empty
	Findings    []domain.Finding
	CallCount   int       // required: must be 1 or 2
	CompletedAt time.Time // optional: defaults to clock().UTC() if zero
}

// PublishFailureParams encapsulates validated input for a failed role outcome publication.
type PublishFailureParams struct {
	ProjectID     string      // optional: validated against session if non-empty
	SessionID     string      // required
	RoleRunID     string      // required
	Role          domain.Role // optional: validated against role_run if non-empty
	ErrorCategory domain.ErrorCategory
	ErrorMessage  string
	CallCount     int       // required: must be 0, 1, or 2
	CompletedAt   time.Time // optional: defaults to clock().UTC() if zero
}

// PublishResult represents the committed outcome of a role publication.
type PublishResult struct {
	SessionID     string               `json:"session_id"`
	RoleRunID     string               `json:"role_run_id"`
	Role          domain.Role          `json:"role"`
	Status        domain.RoleStatus    `json:"status"`
	CallCount     int                  `json:"call_count"`
	ErrorCategory domain.ErrorCategory `json:"error_category,omitempty"`
	ErrorMessage  string               `json:"error_message,omitempty"`
	FindingCount  int                  `json:"finding_count"`
	CompletedAt   time.Time            `json:"completed_at"`
}

var (
	// publishBeforeCommitHook allows deterministic test verification of rollback on injected failure.
	publishBeforeCommitHook func(ctx context.Context, conn *sql.Conn) error

	// publishBeforeUpdateHook allows deterministic test verification of race conditions before update.
	publishBeforeUpdateHook func(ctx context.Context, conn *sql.Conn, sessionID, roleRunID string) error
)

func validatePublishSuccessParams(params PublishSuccessParams) error {
	if strings.TrimSpace(params.SessionID) == "" {
		return &PublicationValidationError{Field: "session_id", Message: "must not be empty"}
	}
	if strings.TrimSpace(params.RoleRunID) == "" {
		return &PublicationValidationError{Field: "role_run_id", Message: "must not be empty"}
	}
	if params.Role != "" && !domain.IsValidRole(params.Role) {
		return &PublicationValidationError{Field: "role", Message: fmt.Sprintf("invalid canonical role %q", params.Role)}
	}
	if params.CallCount < 1 || params.CallCount > domain.MaxProviderCallsPerRole {
		return &PublicationValidationError{
			Field:   "call_count",
			Message: fmt.Sprintf("successful role call count must be 1 or %d, got %d", domain.MaxProviderCallsPerRole, params.CallCount),
		}
	}
	if len(params.Findings) > domain.MaxFindingsPerRole {
		return &PublicationValidationError{
			Field:   "findings",
			Message: fmt.Sprintf("findings count %d exceeds maximum limit of %d", len(params.Findings), domain.MaxFindingsPerRole),
		}
	}

	seenFindingIDs := make(map[string]struct{}, len(params.Findings))
	for i, f := range params.Findings {
		prefix := fmt.Sprintf("findings[%d]", i)
		if strings.TrimSpace(f.ID) == "" {
			return &PublicationValidationError{Field: prefix + ".id", Message: "must not be empty"}
		}
		if _, exists := seenFindingIDs[f.ID]; exists {
			return &PublicationValidationError{Field: prefix + ".id", Message: fmt.Sprintf("duplicate finding id %q", f.ID)}
		}
		seenFindingIDs[f.ID] = struct{}{}

		if !domain.IsValidSeverity(f.Severity) {
			return &PublicationValidationError{Field: prefix + ".severity", Message: fmt.Sprintf("invalid severity %q", f.Severity)}
		}
		if strings.TrimSpace(f.Category) == "" {
			return &PublicationValidationError{Field: prefix + ".category", Message: "must not be empty"}
		}
		if strings.TrimSpace(f.Issue) == "" {
			return &PublicationValidationError{Field: prefix + ".issue", Message: "must not be empty"}
		}
		if len(f.Issue) > domain.MaxIssueChars {
			return &PublicationValidationError{
				Field:   prefix + ".issue",
				Message: fmt.Sprintf("length %d exceeds maximum limit of %d characters", len(f.Issue), domain.MaxIssueChars),
			}
		}
		if strings.TrimSpace(f.Recommendation) == "" {
			return &PublicationValidationError{Field: prefix + ".recommendation", Message: "must not be empty"}
		}
		if len(f.Recommendation) > domain.MaxRecommendationChars {
			return &PublicationValidationError{
				Field:   prefix + ".recommendation",
				Message: fmt.Sprintf("length %d exceeds maximum limit of %d characters", len(f.Recommendation), domain.MaxRecommendationChars),
			}
		}

		nRefs := len(f.BasisRefs)
		if nRefs < domain.MinBasisRefsPerFinding || nRefs > domain.MaxBasisRefsPerFinding {
			return &PublicationValidationError{
				Field:   prefix + ".basis_refs",
				Message: fmt.Sprintf("basis_refs count %d must be between %d and %d", nRefs, domain.MinBasisRefsPerFinding, domain.MaxBasisRefsPerFinding),
			}
		}

		seenRefs := make(map[string]struct{}, nRefs)
		for j, ref := range f.BasisRefs {
			refPrefix := fmt.Sprintf("%s.basis_refs[%d]", prefix, j)
			if strings.TrimSpace(ref) == "" {
				return &PublicationValidationError{Field: refPrefix, Message: "must not be empty"}
			}
			if _, exists := seenRefs[ref]; exists {
				return &PublicationValidationError{Field: refPrefix, Message: fmt.Sprintf("duplicate basis_ref %q", ref)}
			}
			seenRefs[ref] = struct{}{}
		}
	}

	return nil
}

func validatePublishFailureParams(params PublishFailureParams) error {
	if strings.TrimSpace(params.SessionID) == "" {
		return &PublicationValidationError{Field: "session_id", Message: "must not be empty"}
	}
	if strings.TrimSpace(params.RoleRunID) == "" {
		return &PublicationValidationError{Field: "role_run_id", Message: "must not be empty"}
	}
	if params.Role != "" && !domain.IsValidRole(params.Role) {
		return &PublicationValidationError{Field: "role", Message: fmt.Sprintf("invalid canonical role %q", params.Role)}
	}
	if params.CallCount < 0 || params.CallCount > domain.MaxProviderCallsPerRole {
		return &PublicationValidationError{
			Field:   "call_count",
			Message: fmt.Sprintf("failure call count must be between 0 and %d, got %d", domain.MaxProviderCallsPerRole, params.CallCount),
		}
	}
	if !isValidErrorCategory(params.ErrorCategory) {
		return &PublicationValidationError{
			Field:   "error_category",
			Message: fmt.Sprintf("invalid canonical error category %q", params.ErrorCategory),
		}
	}
	return nil
}

// PublishRoleSuccess atomically publishes successful outcome for one reserved in_flight role:
// enforces compare-and-set on session_id, role_run_id, and status = 'in_flight'; validates findings
// and citations; inserts findings and basis references; transitions role to complete; sets completed_at
// and call metadata; leaves session and snapshot fields untouched.
func (s *Store) PublishRoleSuccess(ctx context.Context, params PublishSuccessParams) (*PublishResult, error) {
	if s == nil {
		return nil, errors.New("cannot publish role from nil store")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validatePublishSuccessParams(params); err != nil {
		return nil, err
	}

	writer, err := s.writerDB()
	if err != nil {
		return nil, err
	}

	var now time.Time
	if params.CompletedAt.IsZero() {
		now = clock().UTC()
	} else {
		now = params.CompletedAt.UTC()
	}

	retryPolicy := DefaultRetryPolicy("publish_role_success")
	var result *PublishResult

	err = withImmediate(ctx, writer, retryPolicy, func(conn *sql.Conn) error {
		// 1. Query role run and session within immediate transaction.
		var (
			dbProjectID  string
			snapshotID   string
			roleStr      string
			statusStr    string
			startedAtStr sql.NullString
		)
		query := `SELECT s.project_id, s.snapshot_id, rr.role, rr.status, rr.started_at
			FROM role_runs rr
			JOIN sessions s ON rr.session_id = s.id
			WHERE rr.id = ? AND rr.session_id = ?;`
		err := conn.QueryRowContext(ctx, query, params.RoleRunID, params.SessionID).Scan(
			&dbProjectID, &snapshotID, &roleStr, &statusStr, &startedAtStr,
		)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				var sessProjectID string
				sErr := conn.QueryRowContext(ctx, "SELECT project_id FROM sessions WHERE id = ?;", params.SessionID).Scan(&sessProjectID)
				if errors.Is(sErr, sql.ErrNoRows) {
					return &SessionNotFoundError{SessionID: params.SessionID, ProjectID: params.ProjectID}
				}
				if params.ProjectID != "" && sessProjectID != params.ProjectID {
					return &SessionNotFoundError{SessionID: params.SessionID, ProjectID: params.ProjectID}
				}
				return &PublicationConflictError{
					SessionID:      params.SessionID,
					RoleRunID:      params.RoleRunID,
					ExpectedStatus: domain.RoleInFlight,
					Message:        "role run not found in session",
				}
			}
			return fmt.Errorf("query role run %q: %w", params.RoleRunID, sanitizeError(err))
		}

		if params.ProjectID != "" && dbProjectID != params.ProjectID {
			return &SessionNotFoundError{SessionID: params.SessionID, ProjectID: params.ProjectID}
		}

		dbRole := domain.Role(roleStr)
		if params.Role != "" && params.Role != dbRole {
			return &PublicationValidationError{
				Field:   "role",
				Message: fmt.Sprintf("parameter role %q does not match role run role %q", params.Role, dbRole),
			}
		}

		actualStatus := domain.RoleStatus(statusStr)
		if actualStatus != domain.RoleInFlight {
			return &PublicationConflictError{
				SessionID:      params.SessionID,
				RoleRunID:      params.RoleRunID,
				Role:           dbRole,
				ExpectedStatus: domain.RoleInFlight,
				ActualStatus:   actualStatus,
				Message:        fmt.Sprintf("role is in status %q, expected %q", actualStatus, domain.RoleInFlight),
			}
		}

		if startedAtStr.Valid && startedAtStr.String != "" {
			startedAt, pErr := parseUTCTimestamp(startedAtStr.String)
			if pErr == nil && now.Before(startedAt) {
				return &PublicationValidationError{
					Field:   "completed_at",
					Message: fmt.Sprintf("completed_at (%s) cannot be before started_at (%s)", formatUTCTimestamp(now), startedAtStr.String),
				}
			}
		}

		unitIDToDBID := make(map[string]string)
		if len(params.Findings) > 0 {
			rows, qErr := conn.QueryContext(ctx, "SELECT id, unit_id FROM evidence_units WHERE snapshot_id = ?;", snapshotID)
			if qErr != nil {
				return fmt.Errorf("query evidence units for snapshot %q: %w", snapshotID, sanitizeError(qErr))
			}
			defer rows.Close()

			for rows.Next() {
				var euID, uID string
				if err := rows.Scan(&euID, &uID); err != nil {
					return fmt.Errorf("scan evidence unit: %w", sanitizeError(err))
				}
				unitIDToDBID[uID] = euID
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("evidence units rows: %w", sanitizeError(err))
			}

			for i, f := range params.Findings {
				for j, ref := range f.BasisRefs {
					if _, ok := unitIDToDBID[ref]; !ok {
						return &PublicationValidationError{
							Field:   fmt.Sprintf("findings[%d].basis_refs[%d]", i, j),
							Message: fmt.Sprintf("basis ref %q does not exist in session snapshot %q", ref, snapshotID),
						}
					}
				}
			}
		}

		// Optional hook before update.
		if publishBeforeUpdateHook != nil {
			if hookErr := publishBeforeUpdateHook(ctx, conn, params.SessionID, params.RoleRunID); hookErr != nil {
				return hookErr
			}
		}

		// Compare-and-set UPDATE.
		completedAtStr := formatUTCTimestamp(now)
		res, uErr := conn.ExecContext(ctx, `
			UPDATE role_runs
			SET status = 'complete',
				call_count = ?,
				completed_at = ?
			WHERE id = ? AND session_id = ? AND status = 'in_flight';
		`, params.CallCount, completedAtStr, params.RoleRunID, params.SessionID)
		if uErr != nil {
			return fmt.Errorf("update role_run %q to complete: %w", params.RoleRunID, sanitizeError(uErr))
		}

		rowsAffected, rErr := res.RowsAffected()
		if rErr != nil {
			return fmt.Errorf("check rows affected for role_run %q: %w", params.RoleRunID, sanitizeError(rErr))
		}
		if rowsAffected == 0 {
			return &PublicationConflictError{
				SessionID:      params.SessionID,
				RoleRunID:      params.RoleRunID,
				Role:           dbRole,
				ExpectedStatus: domain.RoleInFlight,
				ActualStatus:   actualStatus,
				Message:        "compare-and-set failed: role run is no longer in_flight",
			}
		}

		// Insert findings and basis references in the same transaction.
		for _, f := range params.Findings {
			findingPK := fmt.Sprintf("%s:%s", params.RoleRunID, f.ID)
			_, fErr := conn.ExecContext(ctx, `
				INSERT INTO findings (id, role_run_id, finding_id, severity, category, issue, recommendation, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?);
			`, findingPK, params.RoleRunID, f.ID, string(f.Severity), f.Category, f.Issue, f.Recommendation, completedAtStr)
			if fErr != nil {
				return fmt.Errorf("insert finding %q: %w", f.ID, sanitizeError(fErr))
			}

			for ordinalIdx, ref := range f.BasisRefs {
				refPK := fmt.Sprintf("%s:%d", findingPK, ordinalIdx+1)
				euDBID := unitIDToDBID[ref]
				_, bErr := conn.ExecContext(ctx, `
					INSERT INTO finding_basis_refs (id, finding_id, evidence_unit_id, ordinal)
					VALUES (?, ?, ?, ?);
				`, refPK, findingPK, euDBID, ordinalIdx+1)
				if bErr != nil {
					return fmt.Errorf("insert basis ref for finding %q ordinal %d: %w", f.ID, ordinalIdx+1, sanitizeError(bErr))
				}
			}
		}

		// Optional commit hook.
		if publishBeforeCommitHook != nil {
			if hookErr := publishBeforeCommitHook(ctx, conn); hookErr != nil {
				return hookErr
			}
		}

		result = &PublishResult{
			SessionID:    params.SessionID,
			RoleRunID:    params.RoleRunID,
			Role:         dbRole,
			Status:       domain.RoleComplete,
			CallCount:    params.CallCount,
			FindingCount: len(params.Findings),
			CompletedAt:  now,
		}
		return nil
	})

	if err != nil {
		return nil, err
	}
	return result, nil
}

// PublishRoleFailure atomically publishes failed outcome for one reserved in_flight role:
// enforces compare-and-set on session_id, role_run_id, and status = 'in_flight'; validates canonical
// error category and call count; transitions role to failed; sets error category/message, completed_at,
// and call metadata; inserts zero findings/references; leaves session and snapshot fields untouched.
func (s *Store) PublishRoleFailure(ctx context.Context, params PublishFailureParams) (*PublishResult, error) {
	if s == nil {
		return nil, errors.New("cannot publish role from nil store")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validatePublishFailureParams(params); err != nil {
		return nil, err
	}

	writer, err := s.writerDB()
	if err != nil {
		return nil, err
	}

	var now time.Time
	if params.CompletedAt.IsZero() {
		now = clock().UTC()
	} else {
		now = params.CompletedAt.UTC()
	}

	retryPolicy := DefaultRetryPolicy("publish_role_failure")
	var result *PublishResult

	err = withImmediate(ctx, writer, retryPolicy, func(conn *sql.Conn) error {
		// 1. Query role run and session within immediate transaction.
		var (
			dbProjectID  string
			roleStr      string
			statusStr    string
			startedAtStr sql.NullString
		)
		query := `SELECT s.project_id, rr.role, rr.status, rr.started_at
			FROM role_runs rr
			JOIN sessions s ON rr.session_id = s.id
			WHERE rr.id = ? AND rr.session_id = ?;`
		err := conn.QueryRowContext(ctx, query, params.RoleRunID, params.SessionID).Scan(
			&dbProjectID, &roleStr, &statusStr, &startedAtStr,
		)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				var sessProjectID string
				sErr := conn.QueryRowContext(ctx, "SELECT project_id FROM sessions WHERE id = ?;", params.SessionID).Scan(&sessProjectID)
				if errors.Is(sErr, sql.ErrNoRows) {
					return &SessionNotFoundError{SessionID: params.SessionID, ProjectID: params.ProjectID}
				}
				if params.ProjectID != "" && sessProjectID != params.ProjectID {
					return &SessionNotFoundError{SessionID: params.SessionID, ProjectID: params.ProjectID}
				}
				return &PublicationConflictError{
					SessionID:      params.SessionID,
					RoleRunID:      params.RoleRunID,
					ExpectedStatus: domain.RoleInFlight,
					Message:        "role run not found in session",
				}
			}
			return fmt.Errorf("query role run %q: %w", params.RoleRunID, sanitizeError(err))
		}

		if params.ProjectID != "" && dbProjectID != params.ProjectID {
			return &SessionNotFoundError{SessionID: params.SessionID, ProjectID: params.ProjectID}
		}

		dbRole := domain.Role(roleStr)
		if params.Role != "" && params.Role != dbRole {
			return &PublicationValidationError{
				Field:   "role",
				Message: fmt.Sprintf("parameter role %q does not match role run role %q", params.Role, dbRole),
			}
		}

		actualStatus := domain.RoleStatus(statusStr)
		if actualStatus != domain.RoleInFlight {
			return &PublicationConflictError{
				SessionID:      params.SessionID,
				RoleRunID:      params.RoleRunID,
				Role:           dbRole,
				ExpectedStatus: domain.RoleInFlight,
				ActualStatus:   actualStatus,
				Message:        fmt.Sprintf("role is in status %q, expected %q", actualStatus, domain.RoleInFlight),
			}
		}

		if startedAtStr.Valid && startedAtStr.String != "" {
			startedAt, pErr := parseUTCTimestamp(startedAtStr.String)
			if pErr == nil && now.Before(startedAt) {
				return &PublicationValidationError{
					Field:   "completed_at",
					Message: fmt.Sprintf("completed_at (%s) cannot be before started_at (%s)", formatUTCTimestamp(now), startedAtStr.String),
				}
			}
		}

		// Optional hook before update.
		if publishBeforeUpdateHook != nil {
			if hookErr := publishBeforeUpdateHook(ctx, conn, params.SessionID, params.RoleRunID); hookErr != nil {
				return hookErr
			}
		}

		// Compare-and-set UPDATE.
		completedAtStr := formatUTCTimestamp(now)
		var errMsg sql.NullString
		if params.ErrorMessage != "" {
			errMsg = sql.NullString{String: params.ErrorMessage, Valid: true}
		}

		res, uErr := conn.ExecContext(ctx, `
			UPDATE role_runs
			SET status = 'failed',
				error_category = ?,
				error_message = ?,
				call_count = ?,
				completed_at = ?
			WHERE id = ? AND session_id = ? AND status = 'in_flight';
		`, string(params.ErrorCategory), errMsg, params.CallCount, completedAtStr, params.RoleRunID, params.SessionID)
		if uErr != nil {
			return fmt.Errorf("update role_run %q to failed: %w", params.RoleRunID, sanitizeError(uErr))
		}

		rowsAffected, rErr := res.RowsAffected()
		if rErr != nil {
			return fmt.Errorf("check rows affected for role_run %q: %w", params.RoleRunID, sanitizeError(rErr))
		}
		if rowsAffected == 0 {
			return &PublicationConflictError{
				SessionID:      params.SessionID,
				RoleRunID:      params.RoleRunID,
				Role:           dbRole,
				ExpectedStatus: domain.RoleInFlight,
				ActualStatus:   actualStatus,
				Message:        "compare-and-set failed: role run is no longer in_flight",
			}
		}

		// On failure, zero findings or basis refs are inserted.

		// Optional commit hook.
		if publishBeforeCommitHook != nil {
			if hookErr := publishBeforeCommitHook(ctx, conn); hookErr != nil {
				return hookErr
			}
		}

		result = &PublishResult{
			SessionID:     params.SessionID,
			RoleRunID:     params.RoleRunID,
			Role:          dbRole,
			Status:        domain.RoleFailed,
			CallCount:     params.CallCount,
			ErrorCategory: params.ErrorCategory,
			ErrorMessage:  params.ErrorMessage,
			FindingCount:  0,
			CompletedAt:   now,
		}
		return nil
	})

	if err != nil {
		return nil, err
	}
	return result, nil
}

// Aliases on Store.

// PublishSuccess is an alias for PublishRoleSuccess on Store.
func (s *Store) PublishSuccess(ctx context.Context, params PublishSuccessParams) (*PublishResult, error) {
	return s.PublishRoleSuccess(ctx, params)
}

// PublishFailure is an alias for PublishRoleFailure on Store.
func (s *Store) PublishFailure(ctx context.Context, params PublishFailureParams) (*PublishResult, error) {
	return s.PublishRoleFailure(ctx, params)
}

// Package-level functions delegating to Store.

// PublishRoleSuccess package-level function delegating to s.PublishRoleSuccess.
func PublishRoleSuccess(ctx context.Context, s *Store, params PublishSuccessParams) (*PublishResult, error) {
	if s == nil {
		return nil, errors.New("cannot publish role from nil store")
	}
	return s.PublishRoleSuccess(ctx, params)
}

// PublishRoleFailure package-level function delegating to s.PublishRoleFailure.
func PublishRoleFailure(ctx context.Context, s *Store, params PublishFailureParams) (*PublishResult, error) {
	if s == nil {
		return nil, errors.New("cannot publish role from nil store")
	}
	return s.PublishRoleFailure(ctx, params)
}

// PublishSuccess package-level function delegating to s.PublishRoleSuccess.
func PublishSuccess(ctx context.Context, s *Store, params PublishSuccessParams) (*PublishResult, error) {
	return PublishRoleSuccess(ctx, s, params)
}

// PublishFailure package-level function delegating to s.PublishRoleFailure.
func PublishFailure(ctx context.Context, s *Store, params PublishFailureParams) (*PublishResult, error) {
	return PublishRoleFailure(ctx, s, params)
}
