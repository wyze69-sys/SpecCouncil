package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
	"github.com/wyze69-sys/SpecCouncil/internal/review"
)

var (
	// ErrNotFound is the sentinel error for missing sessions.
	ErrNotFound = errors.New("session not found")

	// ErrSessionNotFound is an alias for ErrNotFound.
	ErrSessionNotFound = ErrNotFound

	// ErrNotTerminal is returned when a terminal report is requested on a non-terminal session.
	ErrNotTerminal = errors.New("session is not terminal")

	// ErrNotFinished is an alias for ErrNotTerminal matching the canonical flow terminology.
	ErrNotFinished = ErrNotTerminal

	// ErrSnapshotCorrupted is returned when persisted snapshot data cannot be verified against its hash.
	ErrSnapshotCorrupted = errors.New("persisted snapshot hash mismatch")

	// ErrMalformedData is returned when persisted database rows contain invalid enums or corrupted timestamps.
	ErrMalformedData = errors.New("malformed persisted database state")
)

// SessionNotFoundError is returned when a session lookup fails.
// It supports scoping by ProjectID for future HTTP 404 project authorization.
type SessionNotFoundError struct {
	SessionID string
	ProjectID string
}

// Error formats the not-found details.
func (e *SessionNotFoundError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.ProjectID != "" {
		return fmt.Sprintf("session %q not found in project %q", e.SessionID, e.ProjectID)
	}
	return fmt.Sprintf("session %q not found", e.SessionID)
}

// Is reports whether target matches ErrNotFound or ErrSessionNotFound.
func (e *SessionNotFoundError) Is(target error) bool {
	return target == ErrNotFound || target == ErrSessionNotFound
}

// SessionNotTerminalError is returned when a terminal report is requested
// before the session reaches a terminal state (queued or reviewing).
type SessionNotTerminalError struct {
	SessionID string
	Status    domain.SessionStatus
}

// Error formats the non-terminal rejection details.
func (e *SessionNotTerminalError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("session %q is not terminal (current status: %s)", e.SessionID, e.Status)
}

// Is reports whether target matches ErrNotTerminal or ErrNotFinished.
func (e *SessionNotTerminalError) Is(target error) bool {
	return target == ErrNotTerminal || target == ErrNotFinished
}

// RoleStatus represents the deterministic read model for a single role run within a session.
type RoleStatus struct {
	Role          domain.Role           `json:"role"`
	Status        domain.RoleStatus     `json:"status"`
	Cause         domain.InterruptCause `json:"cause,omitempty"`
	ErrorCategory domain.ErrorCategory  `json:"error_category,omitempty"`
	ErrorMessage  string                `json:"error_message,omitempty"`
	CallCount     int                   `json:"call_count"`
	StartedAt     *time.Time            `json:"started_at,omitempty"`
	CompletedAt   *time.Time            `json:"completed_at,omitempty"`
	CreatedAt     time.Time             `json:"created_at"`
}

// SessionStatus represents the deterministic read model for a review session's progress and state.
type SessionStatus struct {
	SessionID           string                `json:"session_id"`
	ProjectID           string                `json:"project_id"`
	IdempotencyKey      string                `json:"idempotency_key"`
	RequestHash         string                `json:"request_hash"`
	SnapshotID          string                `json:"snapshot_id"`
	SnapshotHash        string                `json:"snapshot_hash"`
	Status              domain.SessionStatus  `json:"status"`
	CancelRequested     bool                  `json:"cancel_requested"`
	CompletedRoleCount  int                   `json:"completed_role_count"`
	IncompleteRoleCount int                   `json:"incomplete_role_count"`
	TerminalReason      domain.TerminalReason `json:"terminal_reason,omitempty"`
	CreatedAt           time.Time             `json:"created_at"`
	ClaimedAt           *time.Time            `json:"claimed_at,omitempty"`
	DispatchCutoffAt    *time.Time            `json:"dispatch_cutoff_at,omitempty"`
	HardDeadlineAt      *time.Time            `json:"hard_deadline_at,omitempty"`
	TerminalAt          *time.Time            `json:"terminal_at,omitempty"`
	Roles               []RoleStatus          `json:"roles"`
}

// Type aliases integrating canonical domain report types.
type (
	// SessionReport aliases review.Report as the canonical terminal report model.
	SessionReport = review.Report
	// Report aliases review.Report as the canonical terminal report model.
	Report = review.Report
	// RoleSummary aliases review.RoleSummary for role-level report output.
	RoleSummary = review.RoleSummary
	// ReportFinding aliases review.ReportFinding for finding-level report output.
	ReportFinding = review.ReportFinding
)

// ReadStatus reads the deterministic status model for sessionID using only the read-only pool.
func (s *Store) ReadStatus(ctx context.Context, sessionID string) (*SessionStatus, error) {
	return s.ReadStatusScoped(ctx, "", sessionID)
}

// ReadStatusScoped reads the deterministic status model for sessionID, verifying projectID when non-empty.
func (s *Store) ReadStatusScoped(ctx context.Context, projectID, sessionID string) (*SessionStatus, error) {
	if s == nil {
		return nil, errors.New("cannot read status from nil store")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, &SessionNotFoundError{SessionID: sessionID, ProjectID: projectID}
	}

	reader, err := s.readerDB()
	if err != nil {
		return nil, err
	}

	return readSessionStatus(ctx, reader, projectID, sessionID)
}

// ReadReport reads the deterministic terminal report for sessionID using only the read-only pool.
// It returns a typed SessionNotTerminalError if the session is queued or reviewing.
func (s *Store) ReadReport(ctx context.Context, sessionID string) (*review.Report, error) {
	return s.ReadReportScoped(ctx, "", sessionID)
}

// ReadReportScoped reads the deterministic terminal report for sessionID, verifying projectID when non-empty.
func (s *Store) ReadReportScoped(ctx context.Context, projectID, sessionID string) (*review.Report, error) {
	if s == nil {
		return nil, errors.New("cannot read report from nil store")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, &SessionNotFoundError{SessionID: sessionID, ProjectID: projectID}
	}

	reader, err := s.readerDB()
	if err != nil {
		return nil, err
	}

	return readTerminalReport(ctx, reader, projectID, sessionID)
}

// ReadTerminalReport is an alias for ReadReport.
func (s *Store) ReadTerminalReport(ctx context.Context, sessionID string) (*review.Report, error) {
	return s.ReadReport(ctx, sessionID)
}

// ReadSnapshot reconstructs the frozen evidence.Snapshot for snapshotID and verifies its stored hash.
func (s *Store) ReadSnapshot(ctx context.Context, snapshotID string) (*evidence.Snapshot, error) {
	if s == nil {
		return nil, errors.New("cannot read snapshot from nil store")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(snapshotID) == "" {
		return nil, errors.New("snapshot id must not be empty")
	}

	reader, err := s.readerDB()
	if err != nil {
		return nil, err
	}

	return readSnapshot(ctx, reader, snapshotID)
}

// Package-level read functions delegating to *Store.
func ReadStatus(ctx context.Context, s *Store, sessionID string) (*SessionStatus, error) {
	return s.ReadStatus(ctx, sessionID)
}

func ReadReport(ctx context.Context, s *Store, sessionID string) (*review.Report, error) {
	return s.ReadReport(ctx, sessionID)
}

func ReadTerminalReport(ctx context.Context, s *Store, sessionID string) (*review.Report, error) {
	return s.ReadTerminalReport(ctx, sessionID)
}

func ReadSnapshot(ctx context.Context, s *Store, snapshotID string) (*evidence.Snapshot, error) {
	return s.ReadSnapshot(ctx, snapshotID)
}

// ----------------------------------------------------------------------------
// Internal read implementation (SELECT-only on read-only pool)
// ----------------------------------------------------------------------------

type rawSessionRow struct {
	id                  string
	projectID           string
	idempotencyKey      string
	requestHash         string
	snapshotID          string
	snapshotHash        string
	status              string
	cancelRequested     int
	completedRoleCount  int
	incompleteRoleCount int
	terminalReason      sql.NullString
	createdAt           string
	claimedAt           sql.NullString
	dispatchCutoffAt    sql.NullString
	hardDeadlineAt      sql.NullString
	terminalAt          sql.NullString
}

func querySessionRow(ctx context.Context, reader *sql.DB, projectID, sessionID string) (*rawSessionRow, error) {
	var row rawSessionRow
	query := `SELECT
		s.id, s.project_id, s.idempotency_key, s.request_hash, s.snapshot_id, sn.hash,
		s.status, s.cancel_requested, s.completed_role_count, s.incomplete_role_count,
		s.terminal_reason, s.created_at, s.claimed_at, s.dispatch_cutoff_at, s.hard_deadline_at, s.terminal_at
	FROM sessions s
	JOIN snapshots sn ON s.snapshot_id = sn.id
	WHERE s.id = ?;`

	err := reader.QueryRowContext(ctx, query, sessionID).Scan(
		&row.id, &row.projectID, &row.idempotencyKey, &row.requestHash, &row.snapshotID, &row.snapshotHash,
		&row.status, &row.cancelRequested, &row.completedRoleCount, &row.incompleteRoleCount,
		&row.terminalReason, &row.createdAt, &row.claimedAt, &row.dispatchCutoffAt, &row.hardDeadlineAt, &row.terminalAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &SessionNotFoundError{SessionID: sessionID, ProjectID: projectID}
		}
		return nil, fmt.Errorf("query session %q: %w", sessionID, sanitizeError(err))
	}

	if projectID != "" && row.projectID != projectID {
		return nil, &SessionNotFoundError{SessionID: sessionID, ProjectID: projectID}
	}

	return &row, nil
}

func readSessionStatus(ctx context.Context, reader *sql.DB, projectID, sessionID string) (*SessionStatus, error) {
	raw, err := querySessionRow(ctx, reader, projectID, sessionID)
	if err != nil {
		return nil, err
	}

	// Validate and decode session status enum.
	sessionStatus := domain.SessionStatus(raw.status)
	if !isValidSessionStatus(sessionStatus) {
		return nil, fmt.Errorf("%w: invalid session status %q", ErrMalformedData, raw.status)
	}

	// Validate counts.
	if raw.completedRoleCount < 0 || raw.completedRoleCount > 4 ||
		raw.incompleteRoleCount < 0 || raw.incompleteRoleCount > 4 ||
		raw.completedRoleCount+raw.incompleteRoleCount != domain.RoleCount {
		return nil, fmt.Errorf("%w: invalid role counts (completed=%d, incomplete=%d)",
			ErrMalformedData, raw.completedRoleCount, raw.incompleteRoleCount)
	}

	// Validate cancel requested boolean.
	if raw.cancelRequested != 0 && raw.cancelRequested != 1 {
		return nil, fmt.Errorf("%w: invalid cancel_requested value %d", ErrMalformedData, raw.cancelRequested)
	}

	// Decode terminal reason.
	var termReason domain.TerminalReason
	if raw.terminalReason.Valid && raw.terminalReason.String != "" {
		termReason = domain.TerminalReason(raw.terminalReason.String)
		if !domain.IsValidTerminalReason(termReason) {
			return nil, fmt.Errorf("%w: invalid terminal reason %q", ErrMalformedData, raw.terminalReason.String)
		}
	}

	// Parse timestamps in strict UTC RFC3339Nano format.
	createdAt, err := parseUTCTimestamp(raw.createdAt)
	if err != nil {
		return nil, fmt.Errorf("%w: created_at: %w", ErrMalformedData, err)
	}
	claimedAt, err := parseNullableUTCTimestamp(raw.claimedAt)
	if err != nil {
		return nil, fmt.Errorf("%w: claimed_at: %w", ErrMalformedData, err)
	}
	dispatchCutoffAt, err := parseNullableUTCTimestamp(raw.dispatchCutoffAt)
	if err != nil {
		return nil, fmt.Errorf("%w: dispatch_cutoff_at: %w", ErrMalformedData, err)
	}
	hardDeadlineAt, err := parseNullableUTCTimestamp(raw.hardDeadlineAt)
	if err != nil {
		return nil, fmt.Errorf("%w: hard_deadline_at: %w", ErrMalformedData, err)
	}
	terminalAt, err := parseNullableUTCTimestamp(raw.terminalAt)
	if err != nil {
		return nil, fmt.Errorf("%w: terminal_at: %w", ErrMalformedData, err)
	}

	// Query roles for this session.
	roles, err := queryRoleStatuses(ctx, reader, sessionID)
	if err != nil {
		return nil, err
	}

	return &SessionStatus{
		SessionID:           raw.id,
		ProjectID:           raw.projectID,
		IdempotencyKey:      raw.idempotencyKey,
		RequestHash:         raw.requestHash,
		SnapshotID:          raw.snapshotID,
		SnapshotHash:        raw.snapshotHash,
		Status:              sessionStatus,
		CancelRequested:     raw.cancelRequested == 1,
		CompletedRoleCount:  raw.completedRoleCount,
		IncompleteRoleCount: raw.incompleteRoleCount,
		TerminalReason:      termReason,
		CreatedAt:           createdAt,
		ClaimedAt:           claimedAt,
		DispatchCutoffAt:    dispatchCutoffAt,
		HardDeadlineAt:      hardDeadlineAt,
		TerminalAt:          terminalAt,
		Roles:               roles,
	}, nil
}

func readTerminalReport(ctx context.Context, reader *sql.DB, projectID, sessionID string) (*review.Report, error) {
	raw, err := querySessionRow(ctx, reader, projectID, sessionID)
	if err != nil {
		return nil, err
	}

	sessionStatus := domain.SessionStatus(raw.status)
	if !sessionStatus.IsTerminal() {
		return nil, &SessionNotTerminalError{
			SessionID: sessionID,
			Status:    sessionStatus,
		}
	}

	// Validate terminal reason is present for terminal session.
	var termReason domain.TerminalReason
	if raw.terminalReason.Valid && raw.terminalReason.String != "" {
		termReason = domain.TerminalReason(raw.terminalReason.String)
		if !domain.IsValidTerminalReason(termReason) {
			return nil, fmt.Errorf("%w: invalid terminal reason %q", ErrMalformedData, raw.terminalReason.String)
		}
	} else {
		return nil, fmt.Errorf("%w: terminal session %q lacks terminal_reason", ErrMalformedData, sessionID)
	}

	// Load canonical roles.
	roleStatuses, err := queryRoleStatuses(ctx, reader, sessionID)
	if err != nil {
		return nil, err
	}

	// Load findings and citations for completed roles only.
	// Failed or interrupted roles contribute NO findings.
	findingsByRole, err := queryCompletedRoleFindings(ctx, reader, sessionID)
	if err != nil {
		return nil, err
	}

	// Build RoleSummary slice strictly in domain.Roles order.
	roleSummaries := make([]review.RoleSummary, 0, len(domain.Roles))
	var allReportFindings []review.ReportFinding

	for _, rs := range roleStatuses {
		fList := findingsByRole[rs.Role]
		fCount := len(fList)
		if rs.Status != domain.RoleComplete {
			fCount = 0
		}

		roleSummaries = append(roleSummaries, review.RoleSummary{
			Role:           rs.Role,
			Status:         rs.Status,
			ErrorCategory:  rs.ErrorCategory,
			InterruptCause: rs.Cause,
			FindingCount:   fCount,
			CallCount:      rs.CallCount,
		})

		// Findings are included only from completed roles.
		if rs.Status == domain.RoleComplete {
			for _, f := range fList {
				allReportFindings = append(allReportFindings, review.ReportFinding{
					Role:    rs.Role,
					Finding: f,
				})
			}
		}
	}

	// Deterministic total ordering for findings:
	// severity rank, role rank, primary basis_ref, category, finding id
	sort.SliceStable(allReportFindings, func(i, j int) bool {
		return findingLess(allReportFindings[i], allReportFindings[j])
	})

	if allReportFindings == nil {
		allReportFindings = make([]review.ReportFinding, 0)
	}

	return &review.Report{
		SessionID:           raw.id,
		SnapshotID:          raw.snapshotID,
		SnapshotHash:        raw.snapshotHash,
		Status:              sessionStatus,
		Reason:              termReason,
		CancelRequested:     raw.cancelRequested == 1,
		CompletedRoleCount:  raw.completedRoleCount,
		IncompleteRoleCount: raw.incompleteRoleCount,
		Roles:               roleSummaries,
		Findings:            allReportFindings,
	}, nil
}

func queryRoleStatuses(ctx context.Context, reader *sql.DB, sessionID string) ([]RoleStatus, error) {
	query := `SELECT
		role, status, cause, error_category, error_message, call_count,
		started_at, completed_at, created_at
	FROM role_runs
	WHERE session_id = ?;`

	rows, err := reader.QueryContext(ctx, query, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query role_runs for session %q: %w", sessionID, sanitizeError(err))
	}
	defer rows.Close()

	byRole := make(map[domain.Role]RoleStatus, domain.RoleCount)

	for rows.Next() {
		var (
			roleStr      string
			statusStr    string
			causeStr     sql.NullString
			errCatStr    sql.NullString
			errMsg       sql.NullString
			callCount    int
			startedAtStr sql.NullString
			complAtStr   sql.NullString
			createdAtStr string
		)
		if err := rows.Scan(&roleStr, &statusStr, &causeStr, &errCatStr, &errMsg, &callCount,
			&startedAtStr, &complAtStr, &createdAtStr); err != nil {
			return nil, fmt.Errorf("scan role_run for session %q: %w", sessionID, sanitizeError(err))
		}

		rRole := domain.Role(roleStr)
		if !domain.IsValidRole(rRole) {
			return nil, fmt.Errorf("%w: unknown role %q", ErrMalformedData, roleStr)
		}
		if _, dup := byRole[rRole]; dup {
			return nil, fmt.Errorf("%w: duplicate role row %q in session %q", ErrMalformedData, roleStr, sessionID)
		}

		rStatus := domain.RoleStatus(statusStr)
		if !domain.IsValidRoleStatus(rStatus) {
			return nil, fmt.Errorf("%w: role %q has invalid status %q", ErrMalformedData, roleStr, statusStr)
		}

		var cause domain.InterruptCause
		if causeStr.Valid && causeStr.String != "" {
			cause = domain.InterruptCause(causeStr.String)
			if !domain.IsValidInterruptCause(cause) {
				return nil, fmt.Errorf("%w: role %q has invalid interrupt cause %q", ErrMalformedData, roleStr, causeStr.String)
			}
		}

		var errCat domain.ErrorCategory
		if errCatStr.Valid && errCatStr.String != "" {
			errCat = domain.ErrorCategory(errCatStr.String)
			if !isValidErrorCategory(errCat) {
				return nil, fmt.Errorf("%w: role %q has invalid error category %q", ErrMalformedData, roleStr, errCatStr.String)
			}
		}

		createdAt, err := parseUTCTimestamp(createdAtStr)
		if err != nil {
			return nil, fmt.Errorf("%w: role %q created_at: %w", ErrMalformedData, roleStr, err)
		}
		startedAt, err := parseNullableUTCTimestamp(startedAtStr)
		if err != nil {
			return nil, fmt.Errorf("%w: role %q started_at: %w", ErrMalformedData, roleStr, err)
		}
		completedAt, err := parseNullableUTCTimestamp(complAtStr)
		if err != nil {
			return nil, fmt.Errorf("%w: role %q completed_at: %w", ErrMalformedData, roleStr, err)
		}

		var errMessage string
		if errMsg.Valid {
			errMessage = errMsg.String
		}

		byRole[rRole] = RoleStatus{
			Role:          rRole,
			Status:        rStatus,
			Cause:         cause,
			ErrorCategory: errCat,
			ErrorMessage:  errMessage,
			CallCount:     callCount,
			StartedAt:     startedAt,
			CompletedAt:   completedAt,
			CreatedAt:     createdAt,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error for session %q role_runs: %w", sessionID, sanitizeError(err))
	}

	if len(byRole) != domain.RoleCount {
		return nil, fmt.Errorf("%w: session %q has %d roles, expected %d", ErrMalformedData, sessionID, len(byRole), domain.RoleCount)
	}

	// Reconstruct deterministic role order from domain.Roles, NOT SQL row order.
	ordered := make([]RoleStatus, 0, len(domain.Roles))
	for _, role := range domain.Roles {
		st, ok := byRole[role]
		if !ok {
			return nil, fmt.Errorf("%w: session %q missing canonical role %q", ErrMalformedData, sessionID, role)
		}
		ordered = append(ordered, st)
	}

	return ordered, nil
}

func queryCompletedRoleFindings(ctx context.Context, reader *sql.DB, sessionID string) (map[domain.Role][]domain.Finding, error) {
	// 1. Query findings for completed roles only.
	queryFindings := `SELECT
		f.id, rr.role, f.finding_id, f.severity, f.category, f.issue, f.recommendation
	FROM findings f
	JOIN role_runs rr ON f.role_run_id = rr.id
	WHERE rr.session_id = ? AND rr.status = 'complete';`

	fRows, err := reader.QueryContext(ctx, queryFindings, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query findings for session %q: %w", sessionID, sanitizeError(err))
	}
	defer fRows.Close()

	type rawFindingItem struct {
		dbID           string
		role           domain.Role
		findingID      string
		severity       domain.Severity
		category       string
		issue          string
		recommendation string
	}

	var rawFindings []rawFindingItem
	for fRows.Next() {
		var item rawFindingItem
		var roleStr, sevStr string
		if err := fRows.Scan(&item.dbID, &roleStr, &item.findingID, &sevStr, &item.category,
			&item.issue, &item.recommendation); err != nil {
			return nil, fmt.Errorf("scan finding: %w", sanitizeError(err))
		}
		item.role = domain.Role(roleStr)
		item.severity = domain.Severity(sevStr)
		if !domain.IsValidSeverity(item.severity) {
			return nil, fmt.Errorf("%w: invalid finding severity %q", ErrMalformedData, sevStr)
		}
		rawFindings = append(rawFindings, item)
	}
	if err := fRows.Err(); err != nil {
		return nil, fmt.Errorf("findings rows: %w", sanitizeError(err))
	}

	if len(rawFindings) == 0 {
		return make(map[domain.Role][]domain.Finding), nil
	}

	// 2. Query citations for completed roles ordered strictly by ordinal ASC.
	queryRefs := `SELECT
		fbr.finding_id, eu.unit_id, fbr.ordinal
	FROM finding_basis_refs fbr
	JOIN findings f ON fbr.finding_id = f.id
	JOIN role_runs rr ON f.role_run_id = rr.id
	JOIN evidence_units eu ON fbr.evidence_unit_id = eu.id
	WHERE rr.session_id = ? AND rr.status = 'complete'
	ORDER BY fbr.finding_id, fbr.ordinal ASC;`

	rRows, err := reader.QueryContext(ctx, queryRefs, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query basis refs for session %q: %w", sessionID, sanitizeError(err))
	}
	defer rRows.Close()

	refsByFindingDBID := make(map[string][]string)
	for rRows.Next() {
		var fID, uID string
		var ordinal int
		if err := rRows.Scan(&fID, &uID, &ordinal); err != nil {
			return nil, fmt.Errorf("scan basis ref: %w", sanitizeError(err))
		}
		if ordinal < domain.MinBasisRefsPerFinding || ordinal > domain.MaxBasisRefsPerFinding {
			return nil, fmt.Errorf("%w: invalid basis ref ordinal %d", ErrMalformedData, ordinal)
		}
		refsByFindingDBID[fID] = append(refsByFindingDBID[fID], uID)
	}
	if err := rRows.Err(); err != nil {
		return nil, fmt.Errorf("basis refs rows: %w", sanitizeError(err))
	}

	// 3. Assemble findings grouped by role.
	result := make(map[domain.Role][]domain.Finding)
	for _, rf := range rawFindings {
		refs := refsByFindingDBID[rf.dbID]
		if refs == nil {
			refs = make([]string, 0)
		}
		finding := domain.Finding{
			ID:             rf.findingID,
			Severity:       rf.severity,
			Category:       rf.category,
			Issue:          rf.issue,
			Recommendation: rf.recommendation,
			BasisRefs:      refs,
		}
		result[rf.role] = append(result[rf.role], finding)
	}

	return result, nil
}

func readSnapshot(ctx context.Context, reader *sql.DB, snapshotID string) (*evidence.Snapshot, error) {
	var (
		storedID   string
		storedHash string
	)
	err := reader.QueryRowContext(ctx,
		"SELECT id, hash FROM snapshots WHERE id = ?;",
		snapshotID,
	).Scan(&storedID, &storedHash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("snapshot %q not found", snapshotID)
		}
		return nil, fmt.Errorf("query snapshot %q: %w", snapshotID, sanitizeError(err))
	}

	// Query units in strict ordinal order.
	rows, err := reader.QueryContext(ctx,
		"SELECT unit_id, ordinal, kind, text FROM evidence_units WHERE snapshot_id = ? ORDER BY ordinal ASC;",
		snapshotID,
	)
	if err != nil {
		return nil, fmt.Errorf("query evidence_units for %q: %w", snapshotID, sanitizeError(err))
	}
	defer rows.Close()

	var units []evidence.Unit
	expectedOrdinal := 0
	for rows.Next() {
		var (
			unitID  string
			ordinal int
			kindStr string
			text    string
		)
		if err := rows.Scan(&unitID, &ordinal, &kindStr, &text); err != nil {
			return nil, fmt.Errorf("scan evidence unit: %w", sanitizeError(err))
		}
		if ordinal != expectedOrdinal {
			return nil, fmt.Errorf("%w: evidence unit %q ordinal gap (got %d, want %d)",
				ErrMalformedData, unitID, ordinal, expectedOrdinal)
		}
		expectedOrdinal++

		uKind := evidence.UnitKind(kindStr)
		if !evidence.IsValidUnitKind(uKind) {
			return nil, fmt.Errorf("%w: evidence unit %q has invalid kind %q", ErrMalformedData, unitID, kindStr)
		}

		units = append(units, evidence.Unit{
			ID:   unitID,
			Kind: uKind,
			Text: text,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("evidence_units rows: %w", sanitizeError(err))
	}

	// Reconstruct and verify hash via evidence.Freeze.
	snap, err := evidence.Freeze(storedID, units)
	if err != nil {
		return nil, fmt.Errorf("%w: freeze snapshot %q: %w", ErrMalformedData, snapshotID, err)
	}
	if snap.Hash != storedHash {
		return nil, fmt.Errorf("%w: snapshot %q stored hash %s != recomputed hash %s",
			ErrSnapshotCorrupted, snapshotID, storedHash, snap.Hash)
	}

	return &snap, nil
}

// ----------------------------------------------------------------------------
// Deterministic sorting and parsing helpers
// ----------------------------------------------------------------------------

func findingLess(a, b review.ReportFinding) bool {
	as, bs := domain.SeverityRank(a.Finding.Severity), domain.SeverityRank(b.Finding.Severity)
	if as != bs {
		return as < bs
	}
	ar, br := domain.RoleOrder(a.Role), domain.RoleOrder(b.Role)
	if ar != br {
		return ar < br
	}
	ap, bp := primaryRef(a.Finding), primaryRef(b.Finding)
	if ap != bp {
		return ap < bp
	}
	if a.Finding.Category != b.Finding.Category {
		return a.Finding.Category < b.Finding.Category
	}
	return a.Finding.ID < b.Finding.ID
}

func primaryRef(f domain.Finding) string {
	if len(f.BasisRefs) == 0 {
		return ""
	}
	lowest := f.BasisRefs[0]
	for _, ref := range f.BasisRefs[1:] {
		if ref < lowest {
			lowest = ref
		}
	}
	return lowest
}

func parseUTCTimestamp(s string) (time.Time, error) {
	if !strings.HasSuffix(s, "Z") || len(s) < 20 {
		return time.Time{}, fmt.Errorf("timestamp %q must end in 'Z' with length >= 20", s)
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

func parseNullableUTCTimestamp(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return nil, nil
	}
	t, err := parseUTCTimestamp(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func isValidSessionStatus(s domain.SessionStatus) bool {
	switch s {
	case domain.SessionQueued, domain.SessionReviewing, domain.SessionComplete, domain.SessionPartial, domain.SessionFailed:
		return true
	}
	return false
}

func isValidErrorCategory(c domain.ErrorCategory) bool {
	switch c {
	case domain.ErrTransport, domain.ErrTimeout, domain.ErrProviderRejected,
		domain.ErrBudgetExhausted, domain.ErrInvalidJSON, domain.ErrSchemaInvalid, domain.ErrInvalidBasisRef:
		return true
	}
	return false
}

func sanitizeError(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(sanitizeMessage(err.Error()))
}
