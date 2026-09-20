package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	minBusyTimeout = 1 * time.Millisecond
	maxBusyTimeout = time.Duration(math.MaxInt32) * time.Millisecond // 2147483647ms
)

// ErrClosed is returned when an operation is attempted on a closed store.
var ErrClosed = errors.New("sqlite store is closed")

// closer abstracts an entity that can be closed, allowing deterministic close error injection in tests.
type closer interface {
	Close() error
}

// sqlOpener abstracts sql.Open for deterministic failure tests.
type sqlOpener func(driverName, dataSourceName string) (*sql.DB, error)

var sqlOpen sqlOpener = sql.Open

// Config defines the validated configuration for opening the SQLite store.
type Config struct {
	Path        string
	BusyTimeout time.Duration
}

// Store encapsulates the writer connection and read-only pool for an SQLite database.
type Store struct {
	cfg           Config
	canonicalPath string

	writer *sql.DB
	reader *sql.DB

	mu       sync.Mutex
	closed   bool
	closeErr error

	writerCloser closer
	readerCloser closer
}

// Open validates cfg and opens the SQLite store with a single-connection writer
// and a distinct read-only reader pool.
func Open(cfg Config) (*Store, error) {
	return openStore(context.Background(), cfg, sqlOpen)
}

func openStore(ctx context.Context, cfg Config, opener sqlOpener) (store *Store, retErr error) {
	cleanPath, err := validateConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	busyMillis := cfg.BusyTimeout.Milliseconds()

	writerDSN := buildDSN(cleanPath, busyMillis, false)
	writerDB, err := opener("sqlite", writerDSN)
	if err != nil {
		return nil, fmt.Errorf("open writer pool: %w", sanitizeErr(err, writerDSN))
	}

	var (
		readerDB *sql.DB
		success  bool
	)
	defer func() {
		if !success {
			var closeErr error
			if readerDB != nil {
				closeErr = errors.Join(closeErr, readerDB.Close())
			}
			if writerDB != nil {
				closeErr = errors.Join(closeErr, writerDB.Close())
			}
			retErr = errors.Join(retErr, closeErr)
		}
	}()

	// Exactly one open and one idle writer connection.
	writerDB.SetMaxOpenConns(1)
	writerDB.SetMaxIdleConns(1)

	if err := writerDB.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping writer connection: %w", sanitizeErr(err, writerDSN))
	}

	// Configure writer pragmas.
	if _, err := writerDB.ExecContext(ctx, "PRAGMA journal_mode = WAL;"); err != nil {
		return nil, fmt.Errorf("configure writer journal_mode: %w", sanitizeErr(err, writerDSN))
	}
	if _, err := writerDB.ExecContext(ctx, "PRAGMA foreign_keys = ON;"); err != nil {
		return nil, fmt.Errorf("configure writer foreign_keys: %w", sanitizeErr(err, writerDSN))
	}
	if _, err := writerDB.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout = %d;", busyMillis)); err != nil {
		return nil, fmt.Errorf("configure writer busy_timeout: %w", sanitizeErr(err, writerDSN))
	}

	// Verify writer live pragmas.
	var fk int
	if err := writerDB.QueryRowContext(ctx, "PRAGMA foreign_keys;").Scan(&fk); err != nil {
		return nil, fmt.Errorf("query writer foreign_keys: %w", sanitizeErr(err, writerDSN))
	}
	if fk != 1 {
		return nil, fmt.Errorf("writer foreign_keys pragma did not take effect (got %d, want 1)", fk)
	}

	var jm string
	if err := writerDB.QueryRowContext(ctx, "PRAGMA journal_mode;").Scan(&jm); err != nil {
		return nil, fmt.Errorf("query writer journal_mode: %w", sanitizeErr(err, writerDSN))
	}
	if !strings.EqualFold(jm, "wal") {
		return nil, fmt.Errorf("writer journal_mode pragma did not take effect (got %q, want wal)", jm)
	}

	var bt int
	if err := writerDB.QueryRowContext(ctx, "PRAGMA busy_timeout;").Scan(&bt); err != nil {
		return nil, fmt.Errorf("query writer busy_timeout: %w", sanitizeErr(err, writerDSN))
	}
	if bt != int(busyMillis) {
		return nil, fmt.Errorf("writer busy_timeout pragma did not take effect (got %d, want %d)", bt, busyMillis)
	}

	// Open distinct read-only reader pool.
	readerDSN := buildDSN(cleanPath, busyMillis, true)
	readerDB, err = opener("sqlite", readerDSN)
	if err != nil {
		return nil, fmt.Errorf("open reader pool: %w", sanitizeErr(err, readerDSN))
	}

	if err := readerDB.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping reader connection: %w", sanitizeErr(err, readerDSN))
	}

	// Verify reader live pragmas.
	var rfk int
	if err := readerDB.QueryRowContext(ctx, "PRAGMA foreign_keys;").Scan(&rfk); err != nil {
		return nil, fmt.Errorf("query reader foreign_keys: %w", sanitizeErr(err, readerDSN))
	}
	if rfk != 1 {
		return nil, fmt.Errorf("reader foreign_keys pragma did not take effect (got %d, want 1)", rfk)
	}

	var rjm string
	if err := readerDB.QueryRowContext(ctx, "PRAGMA journal_mode;").Scan(&rjm); err != nil {
		return nil, fmt.Errorf("query reader journal_mode: %w", sanitizeErr(err, readerDSN))
	}
	if !strings.EqualFold(rjm, "wal") {
		return nil, fmt.Errorf("reader journal_mode pragma did not take effect (got %q, want wal)", rjm)
	}

	var rbt int
	if err := readerDB.QueryRowContext(ctx, "PRAGMA busy_timeout;").Scan(&rbt); err != nil {
		return nil, fmt.Errorf("query reader busy_timeout: %w", sanitizeErr(err, readerDSN))
	}
	if rbt != int(busyMillis) {
		return nil, fmt.Errorf("reader busy_timeout pragma did not take effect (got %d, want %d)", rbt, busyMillis)
	}

	success = true

	store = &Store{
		cfg:           cfg,
		canonicalPath: cleanPath,
		writer:        writerDB,
		reader:        readerDB,
		writerCloser:  writerDB,
		readerCloser:  readerDB,
	}

	return store, nil
}

func validateConfig(cfg Config) (string, error) {
	if cfg.BusyTimeout <= 0 {
		return "", errors.New("busy timeout must be positive")
	}
	if cfg.BusyTimeout%time.Millisecond != 0 {
		return "", errors.New("busy timeout must be an exact whole number of milliseconds")
	}
	if cfg.BusyTimeout < minBusyTimeout || cfg.BusyTimeout > maxBusyTimeout {
		return "", fmt.Errorf("busy timeout must be between %v and %v", minBusyTimeout, maxBusyTimeout)
	}

	if strings.TrimSpace(cfg.Path) == "" {
		return "", errors.New("database path cannot be empty")
	}

	if strings.HasPrefix(cfg.Path, "file:") || strings.Contains(cfg.Path, ":memory:") || strings.Contains(cfg.Path, "mode=memory") {
		return "", errors.New("memory or URI database paths are not allowed")
	}

	if !filepath.IsAbs(cfg.Path) {
		return "", errors.New("database path must be an absolute path")
	}

	cleaned := filepath.Clean(cfg.Path)
	if !filepath.IsAbs(cleaned) {
		return "", errors.New("cleaned database path must be an absolute path")
	}

	if fi, err := os.Stat(cleaned); err == nil && fi.IsDir() {
		return "", errors.New("database path cannot be an existing directory")
	}

	parent := filepath.Dir(cleaned)
	pfi, err := os.Stat(parent)
	if err != nil {
		return "", fmt.Errorf("parent directory of database path does not exist: %w", err)
	}
	if !pfi.IsDir() {
		return "", fmt.Errorf("parent path %q is not a directory", parent)
	}

	return cleaned, nil
}

func buildDSN(cleanPath string, busyMillis int64, readOnly bool) string {
	slashPath := filepath.ToSlash(cleanPath)
	if !strings.HasPrefix(slashPath, "/") {
		slashPath = "/" + slashPath
	}

	u := &url.URL{
		Scheme: "file",
		Path:   slashPath,
	}

	q := u.Query()
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyMillis))
	if readOnly {
		q.Set("mode", "ro")
	}
	u.RawQuery = q.Encode()

	return u.String()
}

func sanitizeErr(err error, dsn string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if dsn != "" && strings.Contains(msg, dsn) {
		return errors.New(strings.ReplaceAll(msg, dsn, "[REDACTED_DSN]"))
	}
	return err
}

// Close is idempotent and concurrency-safe. On its first call it attempts to close
// both reader and writer even if one close fails, combines all real close errors
// with errors.Join, and leaves future operations unusable. Later calls return
// the same stored result without closing again.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return s.closeErr
	}
	s.closed = true

	var errs []error
	if s.writerCloser != nil {
		if err := s.writerCloser.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close writer: %w", err))
		}
	}
	if s.readerCloser != nil {
		if err := s.readerCloser.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close reader: %w", err))
		}
	}

	s.closeErr = errors.Join(errs...)
	return s.closeErr
}

// writerDB returns the writer pool for package-internal use, or ErrClosed if the store is closed.
func (s *Store) writerDB() (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, ErrClosed
	}
	return s.writer, nil
}

// readerDB returns the reader pool for package-internal use, or ErrClosed if the store is closed.
func (s *Store) readerDB() (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, ErrClosed
	}
	return s.reader, nil
}
