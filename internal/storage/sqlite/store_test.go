package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type testCloser struct {
	err   error
	count *int
}

func (tc *testCloser) Close() error {
	if tc.count != nil {
		*tc.count++
	}
	return tc.err
}

func TestConfigValidation_Paths(t *testing.T) {
	tempDir := t.TempDir()
	validTimeout := 5000 * time.Millisecond

	tests := []struct {
		name string
		path string
	}{
		{"empty path", ""},
		{"whitespace path", "   \t\n"},
		{"relative path bare", "test.db"},
		{"relative path dot-slash", "./test.db"},
		{"relative path parent", "../test.db"},
		{"relative path nested", "nested/test.db"},
		{"memory path standard", ":memory:"},
		{"memory path URI prefix", "file::memory:"},
		{"memory path mode query", "file:test.db?mode=memory"},
		{"file URI prefix", "file:///C:/test.db"},
		{"directory as path", tempDir},
		{"missing parent directory", filepath.Join(tempDir, "nonexistent", "sub", "test.db")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				Path:        tt.path,
				BusyTimeout: validTimeout,
			}
			s, err := Open(cfg)
			if err == nil {
				_ = s.Close()
				t.Fatalf("expected error for path %q, got nil", tt.path)
			}
		})
	}

	t.Run("parent path is a file not a directory", func(t *testing.T) {
		filePath := filepath.Join(tempDir, "a_file.txt")
		if err := os.WriteFile(filePath, []byte("hello"), 0600); err != nil {
			t.Fatalf("failed to create dummy file: %v", err)
		}
		badPath := filepath.Join(filePath, "child.db")
		cfg := Config{
			Path:        badPath,
			BusyTimeout: validTimeout,
		}
		s, err := Open(cfg)
		if err == nil {
			_ = s.Close()
			t.Fatalf("expected error when parent path is a file, got nil")
		}
	})
}

func TestConfigValidation_BusyTimeout(t *testing.T) {
	tempDir := t.TempDir()
	validPath := filepath.Join(tempDir, "valid.db")

	tests := []struct {
		name    string
		timeout time.Duration
	}{
		{"zero timeout", 0},
		{"negative timeout 1ms", -1 * time.Millisecond},
		{"negative timeout large", -5000 * time.Millisecond},
		{"fractional millisecond 500us", 500 * time.Microsecond},
		{"fractional millisecond 1500us", 1500 * time.Microsecond},
		{"fractional millisecond 1ns", 1 * time.Nanosecond},
		{"overflow MaxInt32 + 1ms", (time.Duration(math.MaxInt32) + 1) * time.Millisecond},
		{"overflow MaxInt64", time.Duration(math.MaxInt64)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				Path:        validPath,
				BusyTimeout: tt.timeout,
			}
			s, err := Open(cfg)
			if err == nil {
				_ = s.Close()
				t.Fatalf("expected error for busy timeout %v, got nil", tt.timeout)
			}
		})
	}

	t.Run("boundary values", func(t *testing.T) {
		// Min allowed: 1ms
		minCfg := Config{
			Path:        filepath.Join(tempDir, "min.db"),
			BusyTimeout: 1 * time.Millisecond,
		}
		sMin, err := Open(minCfg)
		if err != nil {
			t.Fatalf("expected 1ms to succeed, got %v", err)
		}
		_ = sMin.Close()

		// Max allowed: math.MaxInt32 * time.Millisecond (2147483647ms)
		maxCfg := Config{
			Path:        filepath.Join(tempDir, "max.db"),
			BusyTimeout: time.Duration(math.MaxInt32) * time.Millisecond,
		}
		sMax, err := Open(maxCfg)
		if err != nil {
			t.Fatalf("expected 2147483647ms to succeed, got %v", err)
		}
		_ = sMax.Close()
	})
}

func TestOpen_WindowsRelevantPathsWithSpacesAndPound(t *testing.T) {
	tempDir := t.TempDir()
	subDir := filepath.Join(tempDir, "my space #1 dir")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("failed to create subdir: %v", err)
	}

	dbPath := filepath.Join(subDir, "special #db test 1.sqlite")
	cfg := Config{
		Path:        dbPath,
		BusyTimeout: 5000 * time.Millisecond,
	}

	store, err := Open(cfg)
	if err != nil {
		t.Fatalf("failed to open database with spaces and pound sign: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("error closing store: %v", err)
		}
	}()

	// Verify file was created on filesystem
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("database file was not created at %q: %v", dbPath, err)
	}
}

func TestWriterConnectionLimitAndPragmas(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "writer_test.db")
	timeout := 4200 * time.Millisecond

	cfg := Config{
		Path:        dbPath,
		BusyTimeout: timeout,
	}

	store, err := Open(cfg)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	// 1. Verify writer pool has at most 1 connection by holding 1 connection and attempting a 2nd
	ctx := context.Background()
	conn1, err := store.writer.Conn(ctx)
	if err != nil {
		t.Fatalf("failed to acquire first writer connection: %v", err)
	}
	defer conn1.Close()

	ctxTimeout, cancel := context.WithTimeout(ctx, 60*time.Millisecond)
	defer cancel()
	_, err2 := store.writer.Conn(ctxTimeout)
	if !errors.Is(err2, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded when acquiring second writer connection, got %v", err2)
	}

	// 2. Verify live writer pragmas
	var fk int
	if err := conn1.QueryRowContext(ctx, "PRAGMA foreign_keys;").Scan(&fk); err != nil {
		t.Fatalf("query writer foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Fatalf("expected writer foreign_keys = 1, got %d", fk)
	}

	var jm string
	if err := conn1.QueryRowContext(ctx, "PRAGMA journal_mode;").Scan(&jm); err != nil {
		t.Fatalf("query writer journal_mode: %v", err)
	}
	if !strings.EqualFold(jm, "wal") {
		t.Fatalf("expected writer journal_mode = 'wal', got %q", jm)
	}

	var bt int
	if err := conn1.QueryRowContext(ctx, "PRAGMA busy_timeout;").Scan(&bt); err != nil {
		t.Fatalf("query writer busy_timeout: %v", err)
	}
	if bt != 4200 {
		t.Fatalf("expected writer busy_timeout = 4200, got %d", bt)
	}
}

func TestReaderPragmasAndPooledConnections(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "reader_test.db")
	timeout := 3500 * time.Millisecond

	cfg := Config{
		Path:        dbPath,
		BusyTimeout: timeout,
	}

	store, err := Open(cfg)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Verify pragmas on two simultaneously held reader connections
	conn1, err := store.reader.Conn(ctx)
	if err != nil {
		t.Fatalf("failed to acquire reader connection 1: %v", err)
	}
	defer conn1.Close()

	conn2, err := store.reader.Conn(ctx)
	if err != nil {
		t.Fatalf("failed to acquire reader connection 2: %v", err)
	}
	defer conn2.Close()

	for i, conn := range []*sql.Conn{conn1, conn2} {
		var fk int
		if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys;").Scan(&fk); err != nil {
			t.Fatalf("conn %d: query foreign_keys: %v", i+1, err)
		}
		if fk != 1 {
			t.Fatalf("conn %d: expected foreign_keys = 1, got %d", i+1, fk)
		}

		var jm string
		if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode;").Scan(&jm); err != nil {
			t.Fatalf("conn %d: query journal_mode: %v", i+1, err)
		}
		if !strings.EqualFold(jm, "wal") {
			t.Fatalf("conn %d: expected journal_mode = 'wal', got %q", i+1, jm)
		}

		var bt int
		if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout;").Scan(&bt); err != nil {
			t.Fatalf("conn %d: query busy_timeout: %v", i+1, err)
		}
		if bt != 3500 {
			t.Fatalf("conn %d: expected busy_timeout = 3500, got %d", i+1, bt)
		}
	}

	// Close both and acquire replacement connection to verify replacement pooled connection
	// inherits the connection-local pragmas from the driver DSN.
	_ = conn1.Close()
	_ = conn2.Close()

	conn3, err := store.reader.Conn(ctx)
	if err != nil {
		t.Fatalf("failed to acquire replacement reader connection: %v", err)
	}
	defer conn3.Close()

	var fk3 int
	if err := conn3.QueryRowContext(ctx, "PRAGMA foreign_keys;").Scan(&fk3); err != nil {
		t.Fatalf("replacement conn: query foreign_keys: %v", err)
	}
	if fk3 != 1 {
		t.Fatalf("replacement conn: expected foreign_keys = 1, got %d", fk3)
	}

	var bt3 int
	if err := conn3.QueryRowContext(ctx, "PRAGMA busy_timeout;").Scan(&bt3); err != nil {
		t.Fatalf("replacement conn: query busy_timeout: %v", err)
	}
	if bt3 != 3500 {
		t.Fatalf("replacement conn: expected busy_timeout = 3500, got %d", bt3)
	}
}

func TestCrossPoolVisibilityAndReadOnlyWriteRejection(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "crosspool_test.db")
	cfg := Config{
		Path:        dbPath,
		BusyTimeout: 5000 * time.Millisecond,
	}

	store, err := Open(cfg)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// 1. Writer creates a probe table and inserts data
	if _, err := store.writer.ExecContext(ctx, "CREATE TABLE probe (id INTEGER PRIMARY KEY, msg TEXT NOT NULL);"); err != nil {
		t.Fatalf("writer failed to create probe table: %v", err)
	}
	if _, err := store.writer.ExecContext(ctx, "INSERT INTO probe (id, msg) VALUES (1, 'hello from writer');"); err != nil {
		t.Fatalf("writer failed to insert probe data: %v", err)
	}

	// 2. Reader reads the probe data and sees committed writer data
	var msg string
	if err := store.reader.QueryRowContext(ctx, "SELECT msg FROM probe WHERE id = 1;").Scan(&msg); err != nil {
		t.Fatalf("reader failed to read probe row: %v", err)
	}
	if msg != "hello from writer" {
		t.Fatalf("reader saw %q, want %q", msg, "hello from writer")
	}

	// 3. Write through reader MUST fail
	_, writeErr := store.reader.ExecContext(ctx, "INSERT INTO probe (id, msg) VALUES (2, 'write from reader');")
	if writeErr == nil {
		t.Fatalf("expected write through read-only pool to fail, but succeeded")
	}

	_, ddlErr := store.reader.ExecContext(ctx, "CREATE TABLE ro_probe (id INT);")
	if ddlErr == nil {
		t.Fatalf("expected DDL through read-only pool to fail, but succeeded")
	}

	// 4. Verify writer data is unchanged
	var count int
	if err := store.writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM probe;").Scan(&count); err != nil {
		t.Fatalf("writer failed to query probe count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected probe count = 1, got %d", count)
	}
}

func TestFailureAfterWriterOpenDoesNotLeak(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "leak_test.db")
	cfg := Config{
		Path:        dbPath,
		BusyTimeout: 5000 * time.Millisecond,
	}

	var openedWriter *sql.DB
	callCount := 0
	origOpener := sqlOpen
	defer func() { sqlOpen = origOpener }()

	injectedErr := errors.New("simulated reader open failure")
	sqlOpen = func(driverName, dataSourceName string) (*sql.DB, error) {
		callCount++
		if callCount == 1 {
			// Writer open: call real sql.Open
			db, err := origOpener(driverName, dataSourceName)
			openedWriter = db
			return db, err
		}
		// Reader open: simulate failure
		return nil, injectedErr
	}

	store, err := Open(cfg)
	if err == nil {
		_ = store.Close()
		t.Fatalf("expected open failure, got nil")
	}

	if !errors.Is(err, injectedErr) {
		t.Fatalf("expected injected error %v, got %v", injectedErr, err)
	}

	// Verify opened writer was cleaned up (closed) and not leaked
	if openedWriter == nil {
		t.Fatalf("expected openedWriter to be captured")
	}

	// A closed sql.DB returns an error on Ping
	if pingErr := openedWriter.Ping(); pingErr == nil {
		t.Fatalf("expected writer to be closed, but Ping succeeded")
	}
}

func TestClose_Idempotent_ConcurrencySafe_ErrorJoining(t *testing.T) {
	tempDir := t.TempDir()

	t.Run("first close attempts both pools and joins errors", func(t *testing.T) {
		dbPath := filepath.Join(tempDir, "close_join_test.db")
		store, err := Open(Config{Path: dbPath, BusyTimeout: 5000 * time.Millisecond})
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		defer func() {
			_ = store.writer.Close()
			_ = store.reader.Close()
		}()

		errWriter := errors.New("writer close failure")
		errReader := errors.New("reader close failure")

		writerCalls := 0
		readerCalls := 0

		store.writerCloser = &testCloser{err: errWriter, count: &writerCalls}
		store.readerCloser = &testCloser{err: errReader, count: &readerCalls}

		// First close
		closeErr := store.Close()
		if closeErr == nil {
			t.Fatalf("expected close error, got nil")
		}

		if !errors.Is(closeErr, errWriter) {
			t.Fatalf("expected errors.Is(closeErr, errWriter)")
		}
		if !errors.Is(closeErr, errReader) {
			t.Fatalf("expected errors.Is(closeErr, errReader)")
		}

		if writerCalls != 1 {
			t.Fatalf("expected writer closer called 1 time, got %d", writerCalls)
		}
		if readerCalls != 1 {
			t.Fatalf("expected reader closer called 1 time, got %d", readerCalls)
		}

		// Second close: returns exact same error without calling closers again
		secondErr := store.Close()
		if secondErr != closeErr {
			t.Fatalf("expected second close to return identical error, got %v vs %v", secondErr, closeErr)
		}

		if writerCalls != 1 || readerCalls != 1 {
			t.Fatalf("expected no additional closer calls on second Close (writer=%d, reader=%d)", writerCalls, readerCalls)
		}
	})

	t.Run("real pools are unusable after close", func(t *testing.T) {
		dbPath := filepath.Join(tempDir, "close_unusable_test.db")
		store, err := Open(Config{Path: dbPath, BusyTimeout: 5000 * time.Millisecond})
		if err != nil {
			t.Fatalf("open store: %v", err)
		}

		writerPool := store.writer
		readerPool := store.reader

		if err := store.Close(); err != nil {
			t.Fatalf("close failed: %v", err)
		}

		// Subsequent close returns nil again without error
		if err := store.Close(); err != nil {
			t.Fatalf("repeated close failed: %v", err)
		}

		// Unexported accessors must return ErrClosed
		if _, err := store.writerDB(); !errors.Is(err, ErrClosed) {
			t.Fatalf("expected ErrClosed from writerDB(), got %v", err)
		}
		if _, err := store.readerDB(); !errors.Is(err, ErrClosed) {
			t.Fatalf("expected ErrClosed from readerDB(), got %v", err)
		}

		// Real sql.DB pools must be closed
		if err := writerPool.Ping(); err == nil {
			t.Fatalf("expected writer pool to be closed, Ping succeeded")
		}
		if err := readerPool.Ping(); err == nil {
			t.Fatalf("expected reader pool to be closed, Ping succeeded")
		}
	})

	t.Run("concurrency-safe close", func(t *testing.T) {
		dbPath := filepath.Join(tempDir, "close_concurrency_test.db")
		store, err := Open(Config{Path: dbPath, BusyTimeout: 5000 * time.Millisecond})
		if err != nil {
			t.Fatalf("open store: %v", err)
		}

		const goroutines = 20
		var wg sync.WaitGroup
		errs := make([]error, goroutines)

		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				errs[idx] = store.Close()
			}(i)
		}

		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("goroutine %d close error: %v", i, err)
			}
		}
	})
}

func TestTwoIndependentDatabasesNeverShareState(t *testing.T) {
	tempDir := t.TempDir()
	db1Path := filepath.Join(tempDir, "db1.sqlite")
	db2Path := filepath.Join(tempDir, "db2.sqlite")

	store1, err := Open(Config{Path: db1Path, BusyTimeout: 5000 * time.Millisecond})
	if err != nil {
		t.Fatalf("open store1: %v", err)
	}
	defer store1.Close()

	store2, err := Open(Config{Path: db2Path, BusyTimeout: 5000 * time.Millisecond})
	if err != nil {
		t.Fatalf("open store2: %v", err)
	}
	defer store2.Close()

	ctx := context.Background()

	// Store1 creates table_one and inserts a row
	if _, err := store1.writer.ExecContext(ctx, "CREATE TABLE table_one (id INT); INSERT INTO table_one VALUES (101);"); err != nil {
		t.Fatalf("store1 exec: %v", err)
	}

	// Store2 reader must not see table_one
	var val int
	if err := store2.reader.QueryRowContext(ctx, "SELECT id FROM table_one;").Scan(&val); err == nil {
		t.Fatalf("store2 saw store1's table_one, databases are sharing state!")
	}

	// Store2 creates table_two and inserts a row
	if _, err := store2.writer.ExecContext(ctx, "CREATE TABLE table_two (id INT); INSERT INTO table_two VALUES (202);"); err != nil {
		t.Fatalf("store2 exec: %v", err)
	}

	// Store1 reader must not see table_two
	if err := store1.reader.QueryRowContext(ctx, "SELECT id FROM table_two;").Scan(&val); err == nil {
		t.Fatalf("store1 saw store2's table_two, databases are sharing state!")
	}

	// Both stores can read their own data
	var val1, val2 int
	if err := store1.reader.QueryRowContext(ctx, "SELECT id FROM table_one;").Scan(&val1); err != nil || val1 != 101 {
		t.Fatalf("store1 failed reading table_one: val=%d err=%v", val1, err)
	}
	if err := store2.reader.QueryRowContext(ctx, "SELECT id FROM table_two;").Scan(&val2); err != nil || val2 != 202 {
		t.Fatalf("store2 failed reading table_two: val=%d err=%v", val2, err)
	}
}

func TestNoDSNLeakageInErrors(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "sensitive_db.sqlite")

	origOpener := sqlOpen
	defer func() { sqlOpen = origOpener }()

	sqlOpen = func(driverName, dataSourceName string) (*sql.DB, error) {
		return nil, fmt.Errorf("connection failed to %s", dataSourceName)
	}

	cfg := Config{
		Path:        dbPath,
		BusyTimeout: 5000 * time.Millisecond,
	}

	_, err := Open(cfg)
	if err == nil {
		t.Fatalf("expected open error")
	}

	if strings.Contains(err.Error(), "file://") {
		t.Fatalf("error leaked file URI DSN: %s", err.Error())
	}
	if strings.Contains(err.Error(), "_pragma=") {
		t.Fatalf("error leaked DSN pragmas: %s", err.Error())
	}
}
