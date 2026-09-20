package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
	driverSqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	// NormalizationVersion is the frozen request hash normalization version.
	NormalizationVersion = 1
)

var (
	// ErrInvalidUTF8 is returned when submission fields contain invalid UTF-8 bytes.
	ErrInvalidUTF8 = errors.New("submission text contains invalid UTF-8")

	// ErrEmptyField is returned when a required string field is empty.
	ErrEmptyField = errors.New("required field must not be empty")

	// ErrNilSnapshot is returned when a supplied snapshot is empty or uninitialized.
	ErrNilSnapshot = errors.New("snapshot must be a frozen, non-empty evidence.Snapshot")

	// ErrIdempotencyConflict is the sentinel target for idempotency conflict errors.
	ErrIdempotencyConflict = errors.New("idempotency conflict")

	// submitBeforeCommitHook allows deterministic test verification of rollback on failure.
	submitBeforeCommitHook func(ctx context.Context, conn *sql.Conn) error
)

// IdempotencyConflictError is returned when a submission matches an existing
// project_id and idempotency_key but carries a different request hash.
type IdempotencyConflictError struct {
	ProjectID      string
	IdempotencyKey string
	ExistingHash   string
	IncomingHash   string
}

// Error formats the conflict details without exposing sensitive body content.
func (e *IdempotencyConflictError) Error() string {
	return fmt.Sprintf("idempotency conflict for project %q and key %q: existing hash %s != incoming hash %s",
		e.ProjectID, e.IdempotencyKey, e.ExistingHash, e.IncomingHash)
}

// Is supports errors.Is traversal matching ErrIdempotencyConflict.
func (e *IdempotencyConflictError) Is(target error) bool {
	return target == ErrIdempotencyConflict
}

// RequestHashV1 computes the deterministic SHA-256 request hash for v1 submission idempotency.
//
// Encoding scheme:
//
//	"normalization_version=1\n" +
//	<byte_len(project_id)> + ":" + project_id + "\n" +
//	<byte_len(title)> + ":" + title + "\n" +
//	<byte_len(content)> + ":" + content
//
// Properties:
//   - strictly validates UTF-8; rejects invalid bytes with ErrInvalidUTF8 before hashing.
//   - rejects empty projectID, title, or content.
//   - preserves submitted line endings (CRLF vs LF) without alteration.
//   - preserves all whitespace without trimming.
//   - does not apply Unicode normalization.
//   - unambiguous decimal byte-length prefixes prevent field boundary shifting.
//   - produces a 64-character lowercase hexadecimal SHA-256 digest.
func RequestHashV1(projectID, title, content string) (string, error) {
	if !utf8.ValidString(projectID) || !utf8.ValidString(title) || !utf8.ValidString(content) {
		return "", ErrInvalidUTF8
	}
	if len(projectID) == 0 {
		return "", fmt.Errorf("%w: project_id", ErrEmptyField)
	}
	if len(title) == 0 {
		return "", fmt.Errorf("%w: title", ErrEmptyField)
	}
	if len(content) == 0 {
		return "", fmt.Errorf("%w: content", ErrEmptyField)
	}

	var buf bytes.Buffer
	buf.WriteString("normalization_version=")
	buf.WriteString(strconv.Itoa(NormalizationVersion))
	buf.WriteByte('\n')

	buf.WriteString(strconv.Itoa(len(projectID)))
	buf.WriteByte(':')
	buf.WriteString(projectID)
	buf.WriteByte('\n')

	buf.WriteString(strconv.Itoa(len(title)))
	buf.WriteByte(':')
	buf.WriteString(title)
	buf.WriteByte('\n')

	buf.WriteString(strconv.Itoa(len(content)))
	buf.WriteByte(':')
	buf.WriteString(content)

	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

// SubmitParams encapsulates the validated input for an atomic session submission.
type SubmitParams struct {
	SessionID      string // optional: defaults to a generated UUIDv4 if empty
	ProjectID      string
	IdempotencyKey string
	Title          string
	Content        string
	Snapshot       evidence.Snapshot
	CreatedAt      time.Time // optional: defaults to clock().UTC() if zero
}

// SubmitResult represents the authoritative outcome of a session submission.
type SubmitResult struct {
	SessionID   string
	SnapshotID  string
	Status      domain.SessionStatus
	RequestHash string
	Replay      bool
}

// Submit atomically creates a new review session or replays an existing session
// within a single BEGIN IMMEDIATE database transaction.
func (s *Store) Submit(ctx context.Context, params SubmitParams) (*SubmitResult, error) {
	if s == nil {
		return nil, errors.New("cannot submit to nil store")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if len(params.ProjectID) == 0 {
		return nil, fmt.Errorf("%w: project_id", ErrEmptyField)
	}
	if len(params.IdempotencyKey) == 0 {
		return nil, fmt.Errorf("%w: idempotency_key", ErrEmptyField)
	}
	if len(params.Title) == 0 {
		return nil, fmt.Errorf("%w: title", ErrEmptyField)
	}
	if len(params.Content) == 0 {
		return nil, fmt.Errorf("%w: content", ErrEmptyField)
	}
	if len(params.Snapshot.ID) == 0 || len(params.Snapshot.Hash) == 0 || len(params.Snapshot.Units) == 0 {
		return nil, ErrNilSnapshot
	}

	frozen, err := evidence.Freeze(params.Snapshot.ID, params.Snapshot.Units)
	if err != nil {
		return nil, fmt.Errorf("invalid snapshot: %w", err)
	}
	if frozen.Hash != params.Snapshot.Hash {
		return nil, fmt.Errorf("snapshot hash mismatch for %q: supplied %s != recomputed %s",
			params.Snapshot.ID, params.Snapshot.Hash, frozen.Hash)
	}

	reqHash, err := RequestHashV1(params.ProjectID, params.Title, params.Content)
	if err != nil {
		return nil, fmt.Errorf("compute request hash: %w", err)
	}

	writer, err := s.writerDB()
	if err != nil {
		return nil, err
	}

	var result *SubmitResult
	policy := DefaultRetryPolicy("submit_session")

	err = withImmediate(ctx, writer, policy, func(conn *sql.Conn) error {
		// 1. Query UNIQUE(project_id, idempotency_key)
		var (
			existingID         string
			existingHash       string
			existingStatus     string
			existingSnapshotID string
		)
		err := conn.QueryRowContext(ctx,
			"SELECT id, request_hash, status, snapshot_id FROM sessions WHERE project_id = ? AND idempotency_key = ?;",
			params.ProjectID, params.IdempotencyKey,
		).Scan(&existingID, &existingHash, &existingStatus, &existingSnapshotID)

		if err == nil {
			// Idempotency key already exists.
			if existingHash == reqHash {
				result = &SubmitResult{
					SessionID:   existingID,
					SnapshotID:  existingSnapshotID,
					Status:      domain.SessionStatus(existingStatus),
					RequestHash: existingHash,
					Replay:      true,
				}
				return nil
			}
			return &IdempotencyConflictError{
				ProjectID:      params.ProjectID,
				IdempotencyKey: params.IdempotencyKey,
				ExistingHash:   existingHash,
				IncomingHash:   reqHash,
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check existing session: %w", err)
		}

		// 2. New submission: determine session ID and timestamp.
		sessionID := params.SessionID
		if sessionID == "" {
			sessionID = uuid.NewString()
		}

		createdAt := params.CreatedAt
		if createdAt.IsZero() {
			createdAt = clock().UTC()
		} else {
			createdAt = createdAt.UTC()
		}
		createdAtStr := formatUTCTimestamp(createdAt)

		// 3. Atomically persist snapshot if not already present.
		var existingSnapHash string
		snapErr := conn.QueryRowContext(ctx,
			"SELECT hash FROM snapshots WHERE id = ?;",
			params.Snapshot.ID,
		).Scan(&existingSnapHash)

		if errors.Is(snapErr, sql.ErrNoRows) {
			_, err = conn.ExecContext(ctx,
				`INSERT INTO snapshots (id, hash, project_id, title, content, normalization_version, created_at)
				 VALUES (?, ?, ?, ?, ?, 1, ?);`,
				params.Snapshot.ID, params.Snapshot.Hash, params.ProjectID, params.Title, params.Content, createdAtStr,
			)
			if err != nil {
				return fmt.Errorf("insert snapshot: %w", err)
			}

			// Insert all ordered evidence units from supplied snapshot.
			for i, u := range params.Snapshot.Units {
				euID := fmt.Sprintf("%s:%s", params.Snapshot.ID, u.ID)
				_, err = conn.ExecContext(ctx,
					`INSERT INTO evidence_units (id, snapshot_id, unit_id, ordinal, kind, text)
					 VALUES (?, ?, ?, ?, ?, ?);`,
					euID, params.Snapshot.ID, u.ID, i, string(u.Kind), u.Text,
				)
				if err != nil {
					return fmt.Errorf("insert evidence unit %q: %w", u.ID, err)
				}
			}
		} else if snapErr != nil {
			return fmt.Errorf("check existing snapshot: %w", snapErr)
		} else if existingSnapHash != params.Snapshot.Hash {
			return fmt.Errorf("snapshot %q exists with mismatched hash (existing: %s, incoming: %s)",
				params.Snapshot.ID, existingSnapHash, params.Snapshot.Hash)
		}

		// 4. Insert session in queued status.
		_, err = conn.ExecContext(ctx,
			`INSERT INTO sessions (
				id, project_id, idempotency_key, request_hash, snapshot_id,
				status, cancel_requested, completed_role_count, incomplete_role_count, created_at
			) VALUES (?, ?, ?, ?, ?, 'queued', 0, 0, 4, ?);`,
			sessionID, params.ProjectID, params.IdempotencyKey, reqHash, params.Snapshot.ID, createdAtStr,
		)
		if err != nil {
			if isUniqueConstraint(err) {
				// A concurrent insert race won: reread the committed row authoritatively.
				var (
					raceID         string
					raceHash       string
					raceStatus     string
					raceSnapshotID string
				)
				rErr := conn.QueryRowContext(ctx,
					"SELECT id, request_hash, status, snapshot_id FROM sessions WHERE project_id = ? AND idempotency_key = ?;",
					params.ProjectID, params.IdempotencyKey,
				).Scan(&raceID, &raceHash, &raceStatus, &raceSnapshotID)
				if rErr == nil {
					if raceHash == reqHash {
						result = &SubmitResult{
							SessionID:   raceID,
							SnapshotID:  raceSnapshotID,
							Status:      domain.SessionStatus(raceStatus),
							RequestHash: raceHash,
							Replay:      true,
						}
						return nil
					}
					return &IdempotencyConflictError{
						ProjectID:      params.ProjectID,
						IdempotencyKey: params.IdempotencyKey,
						ExistingHash:   raceHash,
						IncomingHash:   reqHash,
					}
				}
			}
			return fmt.Errorf("insert session: %w", err)
		}

		// 5. Insert exactly the four canonical role runs in domain.Roles order.
		for _, role := range domain.Roles {
			roleRunID := fmt.Sprintf("%s:%s", sessionID, role)
			_, err = conn.ExecContext(ctx,
				`INSERT INTO role_runs (id, session_id, role, status, call_count, created_at)
				 VALUES (?, ?, ?, 'pending', 0, ?);`,
				roleRunID, sessionID, role.String(), createdAtStr,
			)
			if err != nil {
				return fmt.Errorf("insert role run for role %q: %w", role, err)
			}
		}

		// Optional test hook for verifying transaction rollback atomicity.
		if submitBeforeCommitHook != nil {
			if hookErr := submitBeforeCommitHook(ctx, conn); hookErr != nil {
				return hookErr
			}
		}

		result = &SubmitResult{
			SessionID:   sessionID,
			SnapshotID:  params.Snapshot.ID,
			Status:      domain.SessionQueued,
			RequestHash: reqHash,
			Replay:      false,
		}
		return nil
	})

	if err != nil {
		return nil, err
	}
	return result, nil
}

// isUniqueConstraint reports whether err represents a SQLite unique or primary key constraint violation.
func isUniqueConstraint(err error) bool {
	if err == nil {
		return false
	}
	var sqliteErr *driverSqlite.Error
	if errors.As(err, &sqliteErr) {
		code := sqliteErr.Code()
		primary := code & sqlitePrimaryCodeMask
		if code == sqlite3.SQLITE_CONSTRAINT_UNIQUE || code == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY || primary == sqlite3.SQLITE_CONSTRAINT {
			msg := strings.ToLower(sqliteErr.Error())
			return strings.Contains(msg, "unique") || strings.Contains(msg, "primary key")
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "primary key")
}
