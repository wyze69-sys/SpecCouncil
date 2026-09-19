package sqlite

import (
	"errors"
	"fmt"
	"strings"

	driverSqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	// sqlitePrimaryCodeMask masks the primary SQLite result code from an extended result code.
	// As documented by SQLite, the least significant 8 bits represent the primary result code.
	sqlitePrimaryCodeMask = 0xff
)

// PersistenceUnavailable is returned when write transactions exhaust all retry attempts
// due to SQLite busy or locked contention.
type PersistenceUnavailable struct {
	Op       string
	Attempts int
	Err      error
}

// Error formats the persistence unavailable error without exposing DSNs, credentials, or raw SQL.
func (e *PersistenceUnavailable) Error() string {
	if e == nil {
		return "<nil>"
	}
	op := e.Op
	if op == "" {
		op = "immediate_tx"
	}
	var cause string
	if e.Err != nil {
		cause = sanitizeMessage(e.Err.Error())
	} else {
		cause = "unknown driver error"
	}
	return fmt.Sprintf("persistence unavailable: operation %q exhausted after %d attempts: %s", op, e.Attempts, cause)
}

// Unwrap exposes the underlying driver error for errors.Is and errors.As traversal.
func (e *PersistenceUnavailable) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Is reports whether target matches this error or the underlying driver error.
func (e *PersistenceUnavailable) Is(target error) bool {
	if target == nil {
		return e == nil
	}
	if _, ok := target.(*PersistenceUnavailable); ok {
		return true
	}
	return errors.Is(e.Err, target)
}

// As delegates to the underlying driver error for type assertions.
func (e *PersistenceUnavailable) As(target any) bool {
	if target == nil {
		return false
	}
	if p, ok := target.(**PersistenceUnavailable); ok {
		*p = e
		return true
	}
	return errors.As(e.Err, target)
}

// isBusyOrLocked reports whether err represents a structured SQLite SQLITE_BUSY or
// SQLITE_LOCKED contention error, including extended result codes.
// It extracts the primary result code using the 8-bit mask and never relies on substring matching.
func isBusyOrLocked(err error) bool {
	if err == nil {
		return false
	}
	var sqliteErr *driverSqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	primary := sqliteErr.Code() & sqlitePrimaryCodeMask
	return primary == sqlite3.SQLITE_BUSY || primary == sqlite3.SQLITE_LOCKED
}

// sanitizeMessage redacts sensitive tokens, URIs, and potential credentials from an error string.
func sanitizeMessage(msg string) string {
	if msg == "" {
		return ""
	}
	// Redact SQLite URIs or file DSNs if present.
	if strings.Contains(msg, "file:") {
		idx := strings.Index(msg, "file:")
		end := strings.IndexAny(msg[idx:], " \t\r\n;\"'")
		if end == -1 {
			msg = msg[:idx] + "[REDACTED_URI]"
		} else {
			msg = msg[:idx] + "[REDACTED_URI]" + msg[idx+end:]
		}
	}
	return msg
}
