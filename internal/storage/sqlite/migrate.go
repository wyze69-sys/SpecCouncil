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
	// Timestamp representation is UTC in RFC3339Nano format with 'Z' suffix (e.g. 2026-09-19T12:00:00.123456789Z).
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
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context cancelled before migration %03d (%s): %w", m.Version, m.Name, err)
	}

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

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context cancelled before SQL execution %03d (%s): %w", m.Version, m.Name, err)
	}

	if _, err := tx.ExecContext(ctx, string(m.SQL)); err != nil {
		return fmt.Errorf("execute migration %03d (%s): %w", m.Version, m.Name, sanitizeErr(err, ""))
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context cancelled before metadata insert %03d (%s): %w", m.Version, m.Name, err)
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

	// Check cancellation immediately before Commit starts.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context cancelled before commit %03d (%s): %w", m.Version, m.Name, err)
	}

	// Once Commit starts, its outcome is authoritative.
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %03d (%s): %w", m.Version, m.Name, sanitizeErr(err, ""))
	}
	committed = true
	return nil
}

type tableColInfo struct {
	cid     int
	name    string
	colType string
	notnull int
	pk      int
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

	// 1. Validate exact table DDL constraints from sqlite_master
	var createSQL string
	err = db.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations';").Scan(&createSQL)
	if err != nil {
		return false, nil, fmt.Errorf("query schema_migrations definition: %w", sanitizeErr(err, ""))
	}
	normSQL := strings.ToLower(createSQL)
	if !strings.Contains(normSQL, "version between 1 and 999") && !strings.Contains(normSQL, "version >= 1 and version <= 999") {
		return false, nil, errors.New("schema_migrations table missing required CHECK constraint on version (version BETWEEN 1 AND 999)")
	}
	if !strings.Contains(normSQL, "length(checksum) = 64") && !strings.Contains(normSQL, "length(checksum)=64") {
		return false, nil, errors.New("schema_migrations table missing required CHECK constraint on checksum (length(checksum) = 64)")
	}

	// 2. Validate columns, exact count (4), exact order, types, NOT NULL, and PRIMARY KEY
	rows, err := db.QueryContext(ctx, "PRAGMA table_info(schema_migrations);")
	if err != nil {
		return false, nil, fmt.Errorf("query schema_migrations schema: %w", sanitizeErr(err, ""))
	}

	var cols []tableColInfo
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
			rows.Close()
			return false, nil, fmt.Errorf("scan table_info: %w", sanitizeErr(err, ""))
		}
		cols = append(cols, tableColInfo{
			cid:     cid,
			name:    colName,
			colType: strings.ToUpper(colType),
			notnull: notNull,
			pk:      pk,
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, nil, fmt.Errorf("iterate table_info: %w", sanitizeErr(err, ""))
	}

	if len(cols) != 4 {
		return false, nil, fmt.Errorf("schema_migrations table has %d columns, want exactly 4", len(cols))
	}

	// Column 0: version INTEGER PRIMARY KEY
	if cols[0].name != "version" {
		return false, nil, fmt.Errorf("column 0 is %q, want 'version'", cols[0].name)
	}
	if cols[0].colType != "INTEGER" {
		return false, nil, fmt.Errorf("column 'version' must have type INTEGER, got %q", cols[0].colType)
	}
	if cols[0].pk != 1 {
		return false, nil, errors.New("column 'version' must be PRIMARY KEY")
	}

	// Column 1: name TEXT NOT NULL UNIQUE
	if cols[1].name != "name" {
		return false, nil, fmt.Errorf("column 1 is %q, want 'name'", cols[1].name)
	}
	if cols[1].colType != "TEXT" {
		return false, nil, fmt.Errorf("column 'name' must have type TEXT, got %q", cols[1].colType)
	}
	if cols[1].notnull != 1 {
		return false, nil, errors.New("column 'name' must be NOT NULL")
	}
	if cols[1].pk != 0 {
		return false, nil, errors.New("column 'name' must not be PRIMARY KEY")
	}

	// Column 2: checksum TEXT NOT NULL
	if cols[2].name != "checksum" {
		return false, nil, fmt.Errorf("column 2 is %q, want 'checksum'", cols[2].name)
	}
	if cols[2].colType != "TEXT" {
		return false, nil, fmt.Errorf("column 'checksum' must have type TEXT, got %q", cols[2].colType)
	}
	if cols[2].notnull != 1 {
		return false, nil, errors.New("column 'checksum' must be NOT NULL")
	}
	if cols[2].pk != 0 {
		return false, nil, errors.New("column 'checksum' must not be PRIMARY KEY")
	}

	// Column 3: applied_at TEXT NOT NULL
	if cols[3].name != "applied_at" {
		return false, nil, fmt.Errorf("column 3 is %q, want 'applied_at'", cols[3].name)
	}
	if cols[3].colType != "TEXT" {
		return false, nil, fmt.Errorf("column 'applied_at' must have type TEXT, got %q", cols[3].colType)
	}
	if cols[3].notnull != 1 {
		return false, nil, errors.New("column 'applied_at' must be NOT NULL")
	}
	if cols[3].pk != 0 {
		return false, nil, errors.New("column 'applied_at' must not be PRIMARY KEY")
	}

	// 3. Validate UNIQUE constraint on 'name' via SQLite index list
	type idxEntry struct {
		name   string
		unique bool
	}
	var indexes []idxEntry
	idxRows, err := db.QueryContext(ctx, "PRAGMA index_list(schema_migrations);")
	if err != nil {
		return false, nil, fmt.Errorf("query index_list: %w", sanitizeErr(err, ""))
	}
	for idxRows.Next() {
		var seq int
		var name, origin string
		var unique, partial int
		if err := idxRows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			idxRows.Close()
			return false, nil, fmt.Errorf("scan index_list: %w", sanitizeErr(err, ""))
		}
		indexes = append(indexes, idxEntry{name: name, unique: unique == 1})
	}
	idxRows.Close()
	if err := idxRows.Err(); err != nil {
		return false, nil, fmt.Errorf("iterate index_list: %w", sanitizeErr(err, ""))
	}

	hasUniqueName := false
	for _, idx := range indexes {
		if !idx.unique {
			continue
		}
		infoRows, err := db.QueryContext(ctx, fmt.Sprintf("PRAGMA index_info(%s);", idx.name))
		if err != nil {
			return false, nil, fmt.Errorf("query index_info: %w", sanitizeErr(err, ""))
		}
		var colsInIndex []string
		for infoRows.Next() {
			var seqno, cid int
			var colName string
			if err := infoRows.Scan(&seqno, &cid, &colName); err != nil {
				infoRows.Close()
				return false, nil, fmt.Errorf("scan index_info: %w", sanitizeErr(err, ""))
			}
			colsInIndex = append(colsInIndex, colName)
		}
		infoRows.Close()
		if len(colsInIndex) == 1 && colsInIndex[0] == "name" {
			hasUniqueName = true
			break
		}
	}
	if !hasUniqueName {
		return false, nil, errors.New("schema_migrations table must enforce UNIQUE constraint on 'name'")
	}

	// 4. Load and validate existing metadata rows
	qRows, err := db.QueryContext(ctx, "SELECT version, name, checksum, applied_at FROM schema_migrations ORDER BY version ASC;")
	if err != nil {
		return false, nil, fmt.Errorf("select schema_migrations: %w", sanitizeErr(err, ""))
	}
	defer qRows.Close()

	var applied []AppliedMigration
	seenVersions := make(map[int]bool)
	seenNames := make(map[string]bool)
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
		if seenNames[am.Name] {
			return false, nil, fmt.Errorf("duplicate migration name %q in schema_migrations", am.Name)
		}
		seenNames[am.Name] = true
		if len(am.Checksum) != 64 || !isLowerHex(am.Checksum) {
			return false, nil, fmt.Errorf("invalid checksum %q in schema_migrations: must be exact 64 lowercase hexadecimal characters", am.Checksum)
		}
		if err := validateAppliedAt(am.AppliedAt); err != nil {
			return false, nil, err
		}
		applied = append(applied, am)
	}
	if err := qRows.Err(); err != nil {
		return false, nil, fmt.Errorf("iterate schema_migrations rows: %w", sanitizeErr(err, ""))
	}

	return true, applied, nil
}

func validateAppliedAt(s string) error {
	if !strings.HasSuffix(s, "Z") {
		return fmt.Errorf("applied_at timestamp %q must end with 'Z' suffix (timezone offsets rejected)", s)
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return fmt.Errorf("applied_at timestamp %q is not valid RFC3339Nano: %w", s, err)
	}
	if _, offset := t.Zone(); offset != 0 {
		return fmt.Errorf("applied_at timestamp %q must be in UTC", s)
	}
	return nil
}

func discoverManifest(fsys fs.FS) ([]Migration, error) {
	if fsys == nil {
		return nil, errors.New("migration filesystem cannot be nil")
	}

	var manifest []Migration
	seenVersions := make(map[int]string)

	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == "." {
			return nil
		}

		// Reject any file located inside a nested directory.
		if strings.Contains(path, "/") {
			if !d.IsDir() {
				dir := path[:strings.LastIndex(path, "/")]
				return fmt.Errorf("manifest contains non-empty nested directory %q: nested migrations are rejected", dir)
			}
			return nil
		}

		// Top-level directories are traversed by WalkDir.
		if d.IsDir() {
			return nil
		}

		name := d.Name()
		// Non-SQL files such as README.md or embed.go are ignored at the top level.
		if !strings.HasSuffix(name, ".sql") {
			return nil
		}

		matches := migrationFilenameRegex.FindStringSubmatch(name)
		if matches == nil {
			return fmt.Errorf("top-level SQL file %q does not match migration filename pattern ^[0-9]{3}_[a-z][a-z0-9_]*\\.sql$", name)
		}

		versionStr := matches[1]
		migName := matches[2]

		version, err := strconv.Atoi(versionStr)
		if err != nil || version < 1 || version > 999 {
			return fmt.Errorf("invalid migration version %q in %q: must be decimal 001 through 999", versionStr, name)
		}

		if existingFile, exists := seenVersions[version]; exists {
			return fmt.Errorf("duplicate migration version %03d: %q and %q", version, existingFile, name)
		}
		seenVersions[version] = name

		content, err := fs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("read migration file %q: %w", name, err)
		}
		if len(content) == 0 {
			return fmt.Errorf("migration file %q is empty", name)
		}

		hash := sha256.Sum256(content)
		checksum := hex.EncodeToString(hash[:])

		manifest = append(manifest, Migration{
			Version:  version,
			Name:     migName,
			Checksum: checksum,
			SQL:      content,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("discover migration manifest: %w", err)
	}

	sort.Slice(manifest, func(i, j int) bool {
		return manifest[i].Version < manifest[j].Version
	})

	return manifest, nil
}

func isLowerHex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
