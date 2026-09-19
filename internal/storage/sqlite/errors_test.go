package sqlite

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unsafe"

	driverSqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

type rawDriverError struct {
	msg  string
	code int
}

func makeDriverError(code int, msg string) *driverSqlite.Error {
	raw := &rawDriverError{
		msg:  msg,
		code: code,
	}
	return (*driverSqlite.Error)(unsafe.Pointer(raw))
}

func TestPersistenceUnavailable(t *testing.T) {
	driverErr := makeDriverError(sqlite3.SQLITE_BUSY, "database is locked")
	pu := &PersistenceUnavailable{
		Op:       "submit_review",
		Attempts: 4,
		Err:      driverErr,
	}

	// 1. Error string formatting and safety
	errStr := pu.Error()
	if !strings.Contains(errStr, "submit_review") {
		t.Errorf("Error() missing op: %s", errStr)
	}
	if !strings.Contains(errStr, "4") {
		t.Errorf("Error() missing attempts: %s", errStr)
	}
	if !strings.Contains(errStr, "database is locked") {
		t.Errorf("Error() missing driver message: %s", errStr)
	}

	// 2. Default op when empty
	puDefault := &PersistenceUnavailable{
		Attempts: 1,
		Err:      driverErr,
	}
	if !strings.Contains(puDefault.Error(), "immediate_tx") {
		t.Errorf("Error() default op want immediate_tx, got: %s", puDefault.Error())
	}

	// 3. Sanitizing sensitive DSN
	sensitiveErr := &PersistenceUnavailable{
		Op:       "op",
		Attempts: 1,
		Err:      errors.New("connection to file:/path/to/db.sqlite?_pragma=secret failed"),
	}
	if strings.Contains(sensitiveErr.Error(), "file:/path/to/db.sqlite") {
		t.Errorf("Error() exposed DSN: %s", sensitiveErr.Error())
	}
	if !strings.Contains(sensitiveErr.Error(), "[REDACTED_URI]") {
		t.Errorf("Error() expected [REDACTED_URI], got: %s", sensitiveErr.Error())
	}

	// 4. Unwrap returns driver error
	if !errors.Is(pu, driverErr) {
		t.Errorf("errors.Is(pu, driverErr) = false, want true")
	}

	// 5. errors.As matches *PersistenceUnavailable
	var asPU *PersistenceUnavailable
	if !errors.As(pu, &asPU) {
		t.Fatalf("errors.As(pu, &asPU) = false, want true")
	}
	if asPU.Op != "submit_review" || asPU.Attempts != 4 {
		t.Errorf("unpacked asPU mismatch: %+v", asPU)
	}

	// 6. errors.As matches *driverSqlite.Error through Unwrap
	var asDriverErr *driverSqlite.Error
	if !errors.As(pu, &asDriverErr) {
		t.Fatalf("errors.As(pu, &asDriverErr) = false, want true")
	}
	if asDriverErr.Code() != sqlite3.SQLITE_BUSY {
		t.Errorf("asDriverErr.Code() = %d, want %d", asDriverErr.Code(), sqlite3.SQLITE_BUSY)
	}

	// 7. Nil handling
	var nilPU *PersistenceUnavailable
	if nilPU.Error() != "<nil>" {
		t.Errorf("nilPU.Error() = %q, want <nil>", nilPU.Error())
	}
	if nilPU.Unwrap() != nil {
		t.Errorf("nilPU.Unwrap() != nil")
	}
}

func TestIsBusyOrLocked_Classification(t *testing.T) {
	tests := []struct {
		name       string
		code       int
		wantRetry  bool
		isExtended bool
	}{
		// Primary busy & locked
		{"SQLITE_BUSY", sqlite3.SQLITE_BUSY, true, false},
		{"SQLITE_LOCKED", sqlite3.SQLITE_LOCKED, true, false},

		// Extended busy codes
		{"SQLITE_BUSY_RECOVERY", sqlite3.SQLITE_BUSY_RECOVERY, true, true},
		{"SQLITE_BUSY_SNAPSHOT", sqlite3.SQLITE_BUSY_SNAPSHOT, true, true},
		{"SQLITE_BUSY_TIMEOUT", sqlite3.SQLITE_BUSY_TIMEOUT, true, true},

		// Extended locked codes
		{"SQLITE_LOCKED_SHAREDCACHE", sqlite3.SQLITE_LOCKED_SHAREDCACHE, true, true},
		{"SQLITE_LOCKED_VTAB", sqlite3.SQLITE_LOCKED_VTAB, true, true},

		// Non-retryable SQLite codes
		{"SQLITE_OK", sqlite3.SQLITE_OK, false, false},
		{"SQLITE_ERROR", sqlite3.SQLITE_ERROR, false, false},
		{"SQLITE_ABORT", sqlite3.SQLITE_ABORT, false, false},
		{"SQLITE_INTERRUPT", sqlite3.SQLITE_INTERRUPT, false, false},
		{"SQLITE_IOERR", sqlite3.SQLITE_IOERR, false, false},
		{"SQLITE_CORRUPT", sqlite3.SQLITE_CORRUPT, false, false},
		{"SQLITE_CANTOPEN", sqlite3.SQLITE_CANTOPEN, false, false},
		{"SQLITE_CONSTRAINT", sqlite3.SQLITE_CONSTRAINT, false, false},
		{"SQLITE_MISMATCH", sqlite3.SQLITE_MISMATCH, false, false},
		{"SQLITE_MISUSE", sqlite3.SQLITE_MISUSE, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := makeDriverError(tt.code, fmt.Sprintf("test error %s", tt.name))
			got := isBusyOrLocked(err)
			if got != tt.wantRetry {
				t.Errorf("isBusyOrLocked(%s code=%d) = %v, want %v", tt.name, tt.code, got, tt.wantRetry)
			}

			// Also test wrapped in fmt.Errorf
			wrapped := fmt.Errorf("wrapped context: %w", err)
			gotWrapped := isBusyOrLocked(wrapped)
			if gotWrapped != tt.wantRetry {
				t.Errorf("isBusyOrLocked(wrapped %s) = %v, want %v", tt.name, gotWrapped, tt.wantRetry)
			}
		})
	}
}

func TestIsBusyOrLocked_NeverSubstringMatch(t *testing.T) {
	// Must NEVER match on substrings for non-driver errors
	nonDriverErrors := []error{
		nil,
		errors.New("database is locked (SQLITE_BUSY)"),
		errors.New("table is locked (SQLITE_LOCKED)"),
		errors.New("SQLITE_BUSY_SNAPSHOT"),
		errors.New("5"),
		context.Canceled,
		context.DeadlineExceeded,
		fmt.Errorf("custom error: %w", context.Canceled),
	}

	for _, err := range nonDriverErrors {
		t.Run(fmt.Sprintf("%v", err), func(t *testing.T) {
			if isBusyOrLocked(err) {
				t.Errorf("isBusyOrLocked(%v) = true, want false (substring/non-driver matching must fail)", err)
			}
		})
	}
}
