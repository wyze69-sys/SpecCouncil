package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/storage/sqlite/migrations"
)

const (
	migrationMetadataTable = "schema_migrations"
	// Timestamp representation is UTC in RFC3339Nano format (e.g. 2026-09-19T12:00:00.123456789Z).
	timestampFormat = time.RFC3339Nano
)

var (
	migrationFilenameRegex = regexp.MustCompile(`^([0-9]{3})_([a-z][a-z0-9_]*)\.sql$`)
	migrationNameRegex     = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

	// clock allows deterministic time testing without sleeps or wall-clock dependencies.
	clock = time.Now

	// beforeMetadataInsertHook allows injecting a failure right before recording migration metadata in tests.
	beforeMetadataInsertHook func(ctx context.Context, tx *sql.Tx, m Migration, appliedAt string) error
)

// Migration represents a validated, embedded SQL migration file.
type Migration struct {
	Version  int
	Name     string
	Checksum string
	SQL      []byte
}

// AppliedMigration represents an existing row in the schema_migrations metadata table.
type AppliedMigration struct {
	Version   int
	Name      string
	Checksum  string
	AppliedAt string
}

// Migrate executes all pending embedded migrations on the store's writer connection.
func (s *Store) Migrate(ctx context.Context) error {
	return s.migrateFS(ctx, migrations.FS)
}

// migrateFS executes migrations from the provided fs.FS on the store's writer connection.
func (s *Store) migrateFS(ctx context.Context, fsys fs.FS) error {
	if s == nil {
		return errors.New("cannot migrate nil store")
	}
	db, err := s.writerDB()
	if err != nil {
		return err
	}
	return migrateDB(ctx, db, fsys)
}

func migrateDB(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context cancelled before migration: %w", err)
	}
	if db == nil {
		return errors.New("database handle cannot be nil")
	}
	if fsys == nil {
		return errors.New("migration filesystem cannot be nil")
	}

	manifest, err := discoverManifest(fsys)
	if err != nil {
		return fmt.Errorf("discover migration manifest: %w", err)
	}

	tableExists, appliedRows, err := loadAppliedMigrations(ctx, db)
	if err != nil {
		return fmt.Errorf("load applied migrations: %w", err)
	}

	manifestByVersion := make(map[int]Migration, len(manifest))
	for _, m := range manifest {
		manifestByVersion[m.Version] = m
	}

	appliedSet := make(map[int]bool, len(appliedRows))
	maxAppliedVersion := 0
	for _, row := range appliedRows {
		m, ok := manifestByVersion[row.Version]
		if !ok {
			return fmt.Errorf("applied migration version %03d missing from manifest", row.Version)
		}
		if row.Name != m.Name {
			return fmt.Errorf("applied migration version %03d name mismatch: recorded %q, manifest %q", row.Version, row.Name, m.Name)
		}
		if row.Checksum != m.Checksum {
			return fmt.Errorf("applied migration version %03d checksum mismatch: recorded %q, manifest %q", row.Version, row.Checksum, m.Checksum)
		}
		appliedSet[row.Version] = true
		if row.Version > maxAppliedVersion {
			maxAppliedVersion = row.Version
		}
	}

	var pending []Migration
	for _, m := range manifest {
		if !appliedSet[m.Version] {
			if m.Version < maxAppliedVersion {
				return fmt.Errorf("unapplied migration version %03d is lower than max applied version %03d", m.Version, maxAppliedVersion)
			}
			pending = append(pending, m)
		}
	}

	if len(pending) == 0 {
		return nil
	}

	if !tableExists && (len(pending) == 0 || pending[0].Version != 1) {
		return fmt.Errorf("fresh database requires initial metadata migration version 001")
	}

	for _, m := range pending {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("context cancelled before migration %03d (%s): %w", m.Version, m.Name, err)
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return err
		}
	}

	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, m Migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction for migration %03d (%s): %w", m.Version, m.Name, sanitizeErr(err, ""))
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.ExecContext(ctx, string(m.SQL)); err != nil {
		return fmt.Errorf("execute migration %03d (%s): %w", m.Version, m.Name, sanitizeErr(err, ""))
	}

	appliedAt := clock().UTC().Format(timestampFormat)
	if beforeMetadataInsertHook != nil {
		if err := beforeMetadataInsertHook(ctx, tx, m, appliedAt); err != nil {
			return fmt.Errorf("record migration metadata %03d (%s): %w", m.Version, m.Name, sanitizeErr(err, ""))
		}
	}

	const insertQuery = "INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?);"
	if _, err := tx.ExecContext(ctx, insertQuery, m.Version, m.Name, m.Checksum, appliedAt); err != nil {
		return fmt.Errorf("record migration metadata %03d (%s): %w", m.Version, m.Name, sanitizeErr(err, ""))
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %03d (%s): %w", m.Version, m.Name, sanitizeErr(err, ""))
	}
	committed = true
	return nil
}

func loadAppliedMigrations(ctx context.Context, db *sql.DB) (bool, []AppliedMigration, error) {
	var count int
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations';").Scan(&count)
	if err != nil {
		return false, nil, fmt.Errorf("check schema_migrations table: %w", sanitizeErr(err, ""))
	}
	if count == 0 {
		return false, nil, nil
	}

	rows, err := db.QueryContext(ctx, "PRAGMA table_info(schema_migrations);")
	if err != nil {
		return false, nil, fmt.Errorf("query schema_migrations schema: %w", sanitizeErr(err, ""))
	}
	defer rows.Close()

	colFound := make(map[string]bool)
	for rows.Next() {
		var (
			cid     int
			colName string
			colType string
			notNull int
			dfltVal sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &colName, &colType, &notNull, &dfltVal, &pk); err != nil {
			return false, nil, fmt.Errorf("scan table_info: %w", sanitizeErr(err, ""))
		}
		colFound[colName] = true
	}
	if err := rows.Err(); err != nil {
		return false, nil, fmt.Errorf("iterate table_info: %w", sanitizeErr(err, ""))
	}

	requiredCols := []string{"version", "name", "checksum", "applied_at"}
	for _, req := range requiredCols {
		if !colFound[req] {
			return false, nil, fmt.Errorf("schema_migrations table is missing required column %q", req)
		}
	}

	qRows, err := db.QueryContext(ctx, "SELECT version, name, checksum, applied_at FROM schema_migrations ORDER BY version ASC;")
	if err != nil {
		return false, nil, fmt.Errorf("select schema_migrations: %w", sanitizeErr(err, ""))
	}
	defer qRows.Close()

	var applied []AppliedMigration
	seenVersions := make(map[int]bool)
	for qRows.Next() {
		var am AppliedMigration
		if err := qRows.Scan(&am.Version, &am.Name, &am.Checksum, &am.AppliedAt); err != nil {
			return false, nil, fmt.Errorf("scan schema_migrations row: %w", sanitizeErr(err, ""))
		}
		if am.Version < 1 || am.Version > 999 {
			return false, nil, fmt.Errorf("invalid version %d in schema_migrations: must be 001-999", am.Version)
		}
		if seenVersions[am.Version] {
			return false, nil, fmt.Errorf("duplicate version %03d in schema_migrations", am.Version)
		}
		seenVersions[am.Version] = true
		if !migrationNameRegex.MatchString(am.Name) {
			return false, nil, fmt.Errorf("invalid migration name %q in schema_migrations", am.Name)
		}
		if len(am.Checksum) != 64 || !isHex(am.Checksum) {
			return false, nil, fmt.Errorf("invalid checksum %q in schema_migrations: must be 64 lowercase hex characters", am.Checksum)
		}
		if _, err := time.Parse(time.RFC3339Nano, am.AppliedAt); err != nil {
			if _, err := time.Parse(time.RFC3339, am.AppliedAt); err != nil {
				return false, nil, fmt.Errorf("invalid applied_at timestamp %q in schema_migrations: %w", am.AppliedAt, err)
			}
		}
		applied = append(applied, am)
	}
	if err := qRows.Err(); err != nil {
		return false, nil, fmt.Errorf("iterate schema_migrations rows: %w", sanitizeErr(err, ""))
	}

	return true, applied, nil
}

func discoverManifest(fsys fs.FS) ([]Migration, error) {
	if fsys == nil {
		return nil, errors.New("migration filesystem cannot be nil")
	}

	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migration directory: %w", err)
	}

	var manifest []Migration
	seenVersions := make(map[int]string)

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			hasFiles := false
			err := fs.WalkDir(fsys, name, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !d.IsDir() {
					hasFiles = true
					return fs.SkipAll
				}
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("inspect subdirectory %q: %w", name, err)
			}
			if hasFiles {
				return nil, fmt.Errorf("manifest contains non-empty subdirectory %q: nested migrations are rejected", name)
			}
			continue
		}

		if !strings.HasSuffix(name, ".sql") {
			continue
		}

		matches := migrationFilenameRegex.FindStringSubmatch(name)
		if matches == nil {
			return nil, fmt.Errorf("top-level SQL file %q does not match migration filename pattern ^[0-9]{3}_[a-z][a-z0-9_]*\\.sql$", name)
		}

		versionStr := matches[1]
		migName := matches[2]

		version, err := strconv.Atoi(versionStr)
		if err != nil || version < 1 || version > 999 {
			return nil, fmt.Errorf("invalid migration version %q in %q: must be decimal 001 through 999", versionStr, name)
		}

		if existingFile, exists := seenVersions[version]; exists {
			return nil, fmt.Errorf("duplicate migration version %03d: %q and %q", version, existingFile, name)
		}
		seenVersions[version] = name

		content, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("read migration file %q: %w", name, err)
		}
		if len(content) == 0 {
			return nil, fmt.Errorf("migration file %q is empty", name)
		}

		hash := sha256.Sum256(content)
		checksum := hex.EncodeToString(hash[:])

		manifest = append(manifest, Migration{
			Version:  version,
			Name:     migName,
			Checksum: checksum,
			SQL:      content,
		})
	}

	sort.Slice(manifest, func(i, j int) bool {
		return manifest[i].Version < manifest[j].Version
	})

	return manifest, nil
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
