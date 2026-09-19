package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	driverSqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func setupTestStore(t *testing.T, busyTimeout time.Duration) (*Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "immediate_test.db")
	if busyTimeout <= 0 {
		busyTimeout = 100 * time.Millisecond
	}
	s, err := Open(Config{
		Path:        dbPath,
		BusyTimeout: busyTimeout,
	})
	if err != nil {
		t.Fatalf("Open test store: %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
	})
	return s, dbPath
}

func createProbeTable(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec("CREATE TABLE probe (id INTEGER PRIMARY KEY, val TEXT);")
	if err != nil {
		t.Fatalf("create probe table: %v", err)
	}
}

func countProbeRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM probe;").Scan(&count)
	if err != nil {
		t.Fatalf("count probe rows: %v", err)
	}
	return count
}

func resetHooks() {
	sleepWithContext = defaultSleepWithContext
	rollbackHook = nil
	commitHook = nil
	attemptHook = nil
}

func fastPolicy(op string, retries int) RetryPolicy {
	return RetryPolicy{
		Op:             op,
		MaxRetries:     retries,
		InitialBackoff: 0,
		MaxBackoff:     0,
		BackoffFactor:  1.0,
	}
}

// TestDedicatedConnectionAndLiteralBeginImmediate proves that withImmediate
// acquires a dedicated *sql.Conn and executes literal BEGIN IMMEDIATE, acquiring
// the RESERVED lock immediately before any write statements.
func TestDedicatedConnectionAndLiteralBeginImmediate(t *testing.T) {
	defer resetHooks()
	store, dbPath := setupTestStore(t, 200*time.Millisecond)
	createProbeTable(t, store.writer)

	// Direct separate connection to test contention against literal BEGIN IMMEDIATE
	dsn := buildDSN(dbPath, 50, false)
	directDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open directDB: %v", err)
	}
	defer directDB.Close()

	w1Locked := make(chan struct{})
	w1Release := make(chan struct{})

	errCh := make(chan error, 1)

	// Goroutine 1: runs withImmediate
	go func() {
		errCh <- store.withImmediate(context.Background(), fastPolicy("test_begin_immediate", 0), func(conn *sql.Conn) error {
			// Probe table insert inside callback
			if _, err := conn.ExecContext(context.Background(), "INSERT INTO probe (id, val) VALUES (1, 'from_w1');"); err != nil {
				return err
			}
			close(w1Locked)
			<-w1Release
			return nil
		})
	}()

	<-w1Locked

	// Goroutine 2 / Main: A separate connection attempts literal BEGIN IMMEDIATE.
	// Because Goroutine 1 holds BEGIN IMMEDIATE, directDB must fail with SQLITE_BUSY!
	conn2, err := directDB.Conn(context.Background())
	if err != nil {
		t.Fatalf("directDB.Conn: %v", err)
	}
	defer conn2.Close()

	_, err = conn2.ExecContext(context.Background(), "BEGIN IMMEDIATE;")
	if err == nil {
		_, _ = conn2.ExecContext(context.Background(), "ROLLBACK;")
		close(w1Release)
		t.Fatalf("expected BEGIN IMMEDIATE to fail due to lock contention, but it succeeded")
	}

	if !isBusyOrLocked(err) {
		t.Errorf("expected isBusyOrLocked(err) = true for lock contention, got %v", err)
	}

	// Release Writer 1
	close(w1Release)
	if err := <-errCh; err != nil {
		t.Fatalf("withImmediate failed: %v", err)
	}

	if count := countProbeRows(t, store.writer); count != 1 {
		t.Errorf("probe rows count = %d, want 1", count)
	}
}

// TestTwoWritersContendingWithRetry proves that when two writers contend on one file,
// busy/locked error is retried and succeeds once the contending lock is released.
func TestTwoWritersContendingWithRetry(t *testing.T) {
	defer resetHooks()
	store, dbPath := setupTestStore(t, 20*time.Millisecond)
	createProbeTable(t, store.writer)

	// Writer 1 (direct connection) will hold BEGIN IMMEDIATE
	dsn := buildDSN(dbPath, 20, false)
	w1DB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open w1DB: %v", err)
	}
	defer w1DB.Close()

	w1Conn, err := w1DB.Conn(context.Background())
	if err != nil {
		t.Fatalf("w1DB.Conn: %v", err)
	}
	defer w1Conn.Close()

	if _, err := w1Conn.ExecContext(context.Background(), "BEGIN IMMEDIATE;"); err != nil {
		t.Fatalf("w1 BEGIN IMMEDIATE: %v", err)
	}

	attempt1Failed := make(chan struct{})
	var attempts atomic.Int32

	attemptHook = func(a int) {
		attempts.Store(int32(a))
		if a == 1 {
			// Signal that attempt 1 is about to or has run
		}
	}

	// Override sleepWithContext to release w1Conn when backoff begins after attempt 1
	sleepWithContext = func(ctx context.Context, d time.Duration) error {
		select {
		case attempt1Failed <- struct{}{}:
		default:
		}
		// Release w1 write lock so attempt 2 can succeed
		_, _ = w1Conn.ExecContext(context.Background(), "ROLLBACK;")
		return nil
	}

	policy := RetryPolicy{
		Op:             "contention_retry",
		MaxRetries:     2,
		InitialBackoff: 1 * time.Millisecond,
		MaxBackoff:     1 * time.Millisecond,
		BackoffFactor:  1.0,
	}

	err = store.withImmediate(context.Background(), policy, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(context.Background(), "INSERT INTO probe (id, val) VALUES (2, 'retry_success');")
		return err
	})
	if err != nil {
		t.Fatalf("withImmediate failed: %v", err)
	}

	if attempts.Load() != 2 {
		t.Errorf("expected exactly 2 attempts, got %d", attempts.Load())
	}

	if count := countProbeRows(t, store.writer); count != 1 {
		t.Errorf("probe rows count = %d, want 1", count)
	}
}

// TestRetryableErrorsFromBeginCallbackAndCommit tests retryable busy/locked errors
// from all three possible transaction phases: begin, callback SQL, and commit.
func TestRetryableErrorsFromBeginCallbackAndCommit(t *testing.T) {
	defer resetHooks()

	t.Run("retryable_from_callback_sql", func(t *testing.T) {
		defer resetHooks()
		store, _ := setupTestStore(t, 50*time.Millisecond)
		createProbeTable(t, store.writer)

		var attempts atomic.Int32
		attemptHook = func(a int) {
			attempts.Store(int32(a))
		}

		policy := fastPolicy("callback_retry", 2)
		err := store.withImmediate(context.Background(), policy, func(conn *sql.Conn) error {
			if attempts.Load() == 1 {
				// Simulate retryable SQL error in callback
				return makeDriverError(sqlite3.SQLITE_BUSY, "simulated busy in callback")
			}
			_, err := conn.ExecContext(context.Background(), "INSERT INTO probe (id, val) VALUES (10, 'ok');")
			return err
		})
		if err != nil {
			t.Fatalf("withImmediate callback retry failed: %v", err)
		}
		if attempts.Load() != 2 {
			t.Errorf("attempts = %d, want 2", attempts.Load())
		}
		if count := countProbeRows(t, store.writer); count != 1 {
			t.Errorf("probe count = %d, want 1", count)
		}
	})

	t.Run("retryable_from_commit", func(t *testing.T) {
		defer resetHooks()
		store, _ := setupTestStore(t, 50*time.Millisecond)
		createProbeTable(t, store.writer)

		var attempts atomic.Int32
		attemptHook = func(a int) {
			attempts.Store(int32(a))
		}

		commitHook = func(ctx context.Context, conn *sql.Conn) error {
			if attempts.Load() == 1 {
				// Simulate commit failing with SQLITE_BUSY
				return makeDriverError(sqlite3.SQLITE_BUSY, "database is locked on commit")
			}
			_, err := conn.ExecContext(ctx, "COMMIT;")
			return err
		}

		policy := fastPolicy("commit_retry", 2)
		err := store.withImmediate(context.Background(), policy, func(conn *sql.Conn) error {
			_, err := conn.ExecContext(context.Background(), "INSERT INTO probe (id, val) VALUES (20, 'commit_ok');")
			return err
		})
		if err != nil {
			t.Fatalf("withImmediate commit retry failed: %v", err)
		}
		if attempts.Load() != 2 {
			t.Errorf("attempts = %d, want 2", attempts.Load())
		}
		if count := countProbeRows(t, store.writer); count != 1 {
			t.Errorf("probe count = %d, want 1", count)
		}
	})
}

// TestCallbackDomainErrorsNotRetried verifies that domain/validation/conflict
// errors are returned immediately without retrying.
func TestCallbackDomainErrorsNotRetried(t *testing.T) {
	defer resetHooks()
	store, _ := setupTestStore(t, 50*time.Millisecond)
	createProbeTable(t, store.writer)

	var attempts atomic.Int32
	attemptHook = func(a int) {
		attempts.Add(1)
	}

	errDomain := errors.New("domain validation error: conflict")
	policy := fastPolicy("domain_test", 5)

	err := store.withImmediate(context.Background(), policy, func(conn *sql.Conn) error {
		_, _ = conn.ExecContext(context.Background(), "INSERT INTO probe (id, val) VALUES (1, 'discarded');")
		return errDomain
	})

	if !errors.Is(err, errDomain) {
		t.Fatalf("expected errDomain, got %v", err)
	}
	var pu *PersistenceUnavailable
	if errors.As(err, &pu) {
		t.Errorf("domain error was wrapped in PersistenceUnavailable")
	}
	if attempts.Load() != 1 {
		t.Errorf("attempts = %d, want exactly 1 (no retries)", attempts.Load())
	}
	if count := countProbeRows(t, store.writer); count != 0 {
		t.Errorf("probe count = %d, want 0 (rolled back)", count)
	}
}

// TestContextCancellationStopsAttempts tests cancellation before begin and during backoff.
func TestContextCancellationStopsAttempts(t *testing.T) {
	defer resetHooks()

	t.Run("cancellation_before_begin", func(t *testing.T) {
		defer resetHooks()
		store, _ := setupTestStore(t, 50*time.Millisecond)

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately before call

		var attempts atomic.Int32
		attemptHook = func(a int) {
			attempts.Add(1)
		}

		err := store.withImmediate(ctx, fastPolicy("cancelled_before", 3), func(conn *sql.Conn) error {
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
		if attempts.Load() != 0 {
			t.Errorf("attempts = %d, want 0", attempts.Load())
		}
	})

	t.Run("cancellation_during_backoff", func(t *testing.T) {
		defer resetHooks()
		store, _ := setupTestStore(t, 50*time.Millisecond)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var attempts atomic.Int32
		attemptHook = func(a int) {
			attempts.Add(1)
		}

		inBackoff := make(chan struct{})
		sleepWithContext = func(sCtx context.Context, d time.Duration) error {
			close(inBackoff)
			cancel() // Cancel during backoff sleep
			return sCtx.Err()
		}

		policy := RetryPolicy{
			Op:             "cancel_in_backoff",
			MaxRetries:     3,
			InitialBackoff: 10 * time.Millisecond,
			MaxBackoff:     10 * time.Millisecond,
			BackoffFactor:  1.0,
		}

		err := store.withImmediate(ctx, policy, func(conn *sql.Conn) error {
			return makeDriverError(sqlite3.SQLITE_BUSY, "busy error")
		})

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
		if attempts.Load() != 1 {
			t.Errorf("attempts = %d, want 1 (stopped during backoff)", attempts.Load())
		}
	})
}

// TestExactAttemptCountAndNoOverflow verifies that total attempts equal 1 + DB_RETRIES
// and policy bounds prevent overflow.
func TestExactAttemptCountAndNoOverflow(t *testing.T) {
	defer resetHooks()
	store, _ := setupTestStore(t, 50*time.Millisecond)

	for _, maxRetries := range []int{0, 1, 3, 5} {
		var attempts atomic.Int32
		attemptHook = func(a int) {
			attempts.Add(1)
		}

		policy := fastPolicy("attempt_count_test", maxRetries)
		err := store.withImmediate(context.Background(), policy, func(conn *sql.Conn) error {
			return makeDriverError(sqlite3.SQLITE_BUSY, "busy error")
		})

		wantAttempts := 1 + maxRetries
		if int(attempts.Load()) != wantAttempts {
			t.Errorf("MaxRetries=%d: attempts = %d, want %d", maxRetries, attempts.Load(), wantAttempts)
		}

		var pu *PersistenceUnavailable
		if !errors.As(err, &pu) {
			t.Fatalf("expected *PersistenceUnavailable, got %v", err)
		}
		if pu.Attempts != wantAttempts {
			t.Errorf("pu.Attempts = %d, want %d", pu.Attempts, wantAttempts)
		}
		if pu.Op != "attempt_count_test" {
			t.Errorf("pu.Op = %q, want attempt_count_test", pu.Op)
		}
	}

	// Policy validation tests against overflow and invalid values
	invalidPolicies := []RetryPolicy{
		{MaxRetries: -1},
		{MaxRetries: 101},
		{MaxRetries: 1000},
		{InitialBackoff: -1},
		{InitialBackoff: 10 * time.Second, MaxBackoff: 1 * time.Second},
		{MaxBackoff: 31 * time.Second},
		{BackoffFactor: -0.5},
	}

	for _, p := range invalidPolicies {
		if err := p.Validate(); err == nil {
			t.Errorf("policy %+v expected validation error, got nil", p)
		}
	}
}

// TestExhaustionPersistenceUnavailable verifies that exhaustion returns PersistenceUnavailable
// preserving operation, attempt count, and driver error for errors.Is/errors.As.
func TestExhaustionPersistenceUnavailable(t *testing.T) {
	defer resetHooks()
	store, _ := setupTestStore(t, 50*time.Millisecond)

	driverErr := makeDriverError(sqlite3.SQLITE_BUSY, "database is locked (5)")
	policy := fastPolicy("save_findings", 2)

	err := store.withImmediate(context.Background(), policy, func(conn *sql.Conn) error {
		return driverErr
	})

	var pu *PersistenceUnavailable
	if !errors.As(err, &pu) {
		t.Fatalf("errors.As(err, &pu) failed: %v", err)
	}

	if pu.Op != "save_findings" {
		t.Errorf("pu.Op = %q, want 'save_findings'", pu.Op)
	}
	if pu.Attempts != 3 {
		t.Errorf("pu.Attempts = %d, want 3", pu.Attempts)
	}
	if !errors.Is(err, driverErr) {
		t.Errorf("errors.Is(err, driverErr) = false, want true")
	}

	var asDriver *driverSqlite.Error
	if !errors.As(err, &asDriver) {
		t.Fatalf("errors.As(err, &asDriver) = false, want true")
	}
	if asDriver.Code() != sqlite3.SQLITE_BUSY {
		t.Errorf("asDriver.Code() = %d, want %d", asDriver.Code(), sqlite3.SQLITE_BUSY)
	}
}

// TestRollbackAfterCallbackErrorPanicCancellationCommitFailure verifies rollback across all failure modes.
func TestRollbackAfterCallbackErrorPanicCancellationCommitFailure(t *testing.T) {
	defer resetHooks()

	t.Run("rollback_after_callback_error", func(t *testing.T) {
		defer resetHooks()
		store, _ := setupTestStore(t, 50*time.Millisecond)
		createProbeTable(t, store.writer)

		_ = store.withImmediate(context.Background(), fastPolicy("test", 0), func(conn *sql.Conn) error {
			_, err := conn.ExecContext(context.Background(), "INSERT INTO probe (id, val) VALUES (1, 'error_val');")
			if err != nil {
				return err
			}
			return errors.New("callback error")
		})

		if count := countProbeRows(t, store.writer); count != 0 {
			t.Errorf("probe rows = %d, want 0 (rolled back)", count)
		}
	})

	t.Run("rollback_after_panic", func(t *testing.T) {
		defer resetHooks()
		store, _ := setupTestStore(t, 50*time.Millisecond)
		createProbeTable(t, store.writer)

		defer func() {
			r := recover()
			if r == nil {
				t.Fatalf("expected panic to be repanicked")
			}
			if r != "panic_payload" {
				t.Errorf("recovered panic = %v, want panic_payload", r)
			}
			if count := countProbeRows(t, store.writer); count != 0 {
				t.Errorf("probe rows after panic = %d, want 0", count)
			}
		}()

		_ = store.withImmediate(context.Background(), fastPolicy("test", 0), func(conn *sql.Conn) error {
			_, _ = conn.ExecContext(context.Background(), "INSERT INTO probe (id, val) VALUES (2, 'panic_val');")
			panic("panic_payload")
		})
	})

	t.Run("rollback_after_cancellation_before_commit", func(t *testing.T) {
		defer resetHooks()
		store, _ := setupTestStore(t, 50*time.Millisecond)
		createProbeTable(t, store.writer)

		ctx, cancel := context.WithCancel(context.Background())

		err := store.withImmediate(ctx, fastPolicy("test", 0), func(conn *sql.Conn) error {
			_, err := conn.ExecContext(context.Background(), "INSERT INTO probe (id, val) VALUES (3, 'cancel_val');")
			if err != nil {
				return err
			}
			cancel() // Cancel before commit check
			return nil
		})

		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", err)
		}
		if count := countProbeRows(t, store.writer); count != 0 {
			t.Errorf("probe rows after cancel = %d, want 0", count)
		}
	})

	t.Run("rollback_after_commit_failure", func(t *testing.T) {
		defer resetHooks()
		store, _ := setupTestStore(t, 50*time.Millisecond)
		createProbeTable(t, store.writer)

		commitErr := errors.New("simulated commit error")
		commitHook = func(ctx context.Context, conn *sql.Conn) error {
			return commitErr
		}

		err := store.withImmediate(context.Background(), fastPolicy("test", 0), func(conn *sql.Conn) error {
			_, err := conn.ExecContext(context.Background(), "INSERT INTO probe (id, val) VALUES (4, 'commit_fail_val');")
			return err
		})

		if !errors.Is(err, commitErr) {
			t.Errorf("expected commitErr, got %v", err)
		}
		if count := countProbeRows(t, store.writer); count != 0 {
			t.Errorf("probe rows after commit failure = %d, want 0", count)
		}
	})
}

// TestUncertainRollbackPoisonsConnection verifies that when rollback cannot be confirmed,
// the physical connection is poisoned (via driver.ErrBadConn) and discarded from the pool.
func TestUncertainRollbackPoisonsConnection(t *testing.T) {
	defer resetHooks()
	store, _ := setupTestStore(t, 50*time.Millisecond)
	createProbeTable(t, store.writer)

	// Injected rollback failure
	rollbackHook = func(ctx context.Context, conn *sql.Conn) error {
		return errors.New("simulated rollback failure")
	}

	// Trigger rollback failure
	err := store.withImmediate(context.Background(), fastPolicy("poison_test", 0), func(conn *sql.Conn) error {
		_, _ = conn.ExecContext(context.Background(), "INSERT INTO probe (id, val) VALUES (1, 'val');")
		return errors.New("trigger rollback")
	})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}

	// Reset hook
	rollbackHook = nil

	// Because connection was poisoned, the pool must have discarded it.
	// A subsequent operation must get a fresh, healthy connection and succeed without issues.
	err = store.withImmediate(context.Background(), fastPolicy("clean_test", 0), func(conn *sql.Conn) error {
		_, err := conn.ExecContext(context.Background(), "INSERT INTO probe (id, val) VALUES (2, 'fresh_conn_ok');")
		return err
	})
	if err != nil {
		t.Fatalf("subsequent immediate tx on pool failed after poison: %v", err)
	}

	if count := countProbeRows(t, store.writer); count != 1 {
		t.Errorf("probe rows = %d, want 1", count)
	}
}

// TestDedicatedConnectionClosesOnAllPaths verifies that dedicated connections
// are closed on success, failure, retry, and panic.
func TestDedicatedConnectionClosesOnAllPaths(t *testing.T) {
	defer resetHooks()
	store, _ := setupTestStore(t, 50*time.Millisecond)
	createProbeTable(t, store.writer)

	// 1. Success path
	err := store.withImmediate(context.Background(), fastPolicy("test_success", 0), func(conn *sql.Conn) error {
		return nil
	})
	if err != nil {
		t.Fatalf("success failed: %v", err)
	}
	if inUse := store.writer.Stats().InUse; inUse != 0 {
		t.Errorf("inUse after success = %d, want 0", inUse)
	}

	// 2. Failure path
	err = store.withImmediate(context.Background(), fastPolicy("test_fail", 0), func(conn *sql.Conn) error {
		return errors.New("fail")
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	if inUse := store.writer.Stats().InUse; inUse != 0 {
		t.Errorf("inUse after failure = %d, want 0", inUse)
	}

	// 3. Retry exhaustion path
	err = store.withImmediate(context.Background(), fastPolicy("test_retry", 2), func(conn *sql.Conn) error {
		return makeDriverError(sqlite3.SQLITE_BUSY, "busy")
	})
	if err == nil {
		t.Fatalf("expected exhaustion error")
	}
	if inUse := store.writer.Stats().InUse; inUse != 0 {
		t.Errorf("inUse after exhaustion = %d, want 0", inUse)
	}

	// 4. Panic path
	func() {
		defer func() {
			_ = recover()
		}()
		_ = store.withImmediate(context.Background(), fastPolicy("test_panic", 0), func(conn *sql.Conn) error {
			panic("panic for test")
		})
	}()
	if inUse := store.writer.Stats().InUse; inUse != 0 {
		t.Errorf("inUse after panic = %d, want 0", inUse)
	}
}

// TestAuthoritativeCommitEvenIfCancelledImmediatelyAfter verifies that once
// COMMIT succeeds, the result is authoritative success even if cancellation occurs right after.
func TestAuthoritativeCommitEvenIfCancelledImmediatelyAfter(t *testing.T) {
	defer resetHooks()
	store, _ := setupTestStore(t, 50*time.Millisecond)
	createProbeTable(t, store.writer)

	ctx, cancel := context.WithCancel(context.Background())

	commitHook = func(cCtx context.Context, conn *sql.Conn) error {
		// Execute real commit
		if _, err := conn.ExecContext(cCtx, "COMMIT;"); err != nil {
			return err
		}
		// Immediately cancel context after commit succeeds
		cancel()
		return nil
	}

	err := store.withImmediate(ctx, fastPolicy("authoritative_test", 0), func(conn *sql.Conn) error {
		_, err := conn.ExecContext(context.Background(), "INSERT INTO probe (id, val) VALUES (1, 'authoritative');")
		return err
	})

	// Must be success!
	if err != nil {
		t.Fatalf("expected success from authoritative commit, got %v", err)
	}

	if count := countProbeRows(t, store.writer); count != 1 {
		t.Errorf("probe rows = %d, want 1", count)
	}
}

// TestNilStoreAndInvalidParameters verifies defensive validation for nil and invalid inputs.
func TestNilStoreAndInvalidParameters(t *testing.T) {
	var nilStore *Store
	err := nilStore.withImmediate(context.Background(), fastPolicy("test", 0), func(conn *sql.Conn) error {
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "nil store") {
		t.Errorf("expected nil store error, got %v", err)
	}

	store, _ := setupTestStore(t, 50*time.Millisecond)

	err = withImmediate(context.Background(), nil, fastPolicy("test", 0), func(conn *sql.Conn) error {
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "database handle cannot be nil") {
		t.Errorf("expected nil db error, got %v", err)
	}

	err = store.withImmediate(context.Background(), fastPolicy("test", 0), nil)
	if err == nil || !strings.Contains(err.Error(), "callback cannot be nil") {
		t.Errorf("expected nil callback error, got %v", err)
	}

	invalidPolicy := RetryPolicy{MaxRetries: -5}
	err = store.withImmediate(context.Background(), invalidPolicy, func(conn *sql.Conn) error {
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "validate retry policy") {
		t.Errorf("expected policy validation error, got %v", err)
	}
}
