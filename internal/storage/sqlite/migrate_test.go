package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/storage/sqlite/migrations"
)

const exactMetadataTableSQL = `CREATE TABLE schema_migrations (
  version INTEGER PRIMARY KEY CHECK (version BETWEEN 1 AND 999),
  name TEXT NOT NULL UNIQUE,
  checksum TEXT NOT NULL CHECK (length(checksum) = 64),
  applied_at TEXT NOT NULL
);`

func openTestStore(t *testing.T) *Store {
	t.Helper()
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "migrate_test.db")
	cfg := Config{
		Path:        dbPath,
		BusyTimeout: 5000 * time.Millisecond,
	}
	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
	})
	return s
}

func queryAppliedMigrations(t *testing.T, db *sql.DB) []AppliedMigration {
	t.Helper()
	rows, err := db.Query("SELECT version, name, checksum, applied_at FROM schema_migrations ORDER BY version ASC;")
	if err != nil {
		t.Fatalf("failed to query schema_migrations: %v", err)
	}
	defer rows.Close()

	var list []AppliedMigration
	for rows.Next() {
		var m AppliedMigration
		if err := rows.Scan(&m.Version, &m.Name, &m.Checksum, &m.AppliedAt); err != nil {
			t.Fatalf("failed to scan schema_migrations row: %v", err)
		}
		list = append(list, m)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("error iterating schema_migrations: %v", err)
	}
	return list
}

func tableExists(t *testing.T, db *sql.DB, tableName string) bool {
	t.Helper()
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?;", tableName).Scan(&count)
	if err != nil {
		t.Fatalf("failed to check table existence for %q: %v", tableName, err)
	}
	return count > 0
}

func stringContains(s, substr string) bool {
	return strings.Contains(s, substr)
}

func TestProductionEmbedding_EmbedsCompleteDirectory(t *testing.T) {
	// Verify that migrations.FS contains the entire directory including embed.go
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("read migrations.FS: %v", err)
	}

	foundEmbedGo := false
	found001SQL := false
	found002SQL := false
	found003SQL := false
	found004SQL := false
	for _, e := range entries {
		if e.Name() == "embed.go" {
			foundEmbedGo = true
		}
		if e.Name() == "001_migration_metadata.sql" {
			found001SQL = true
		}
		if e.Name() == "002_core_schema.sql" {
			found002SQL = true
		}
		if e.Name() == "003_state_guards.sql" {
			found003SQL = true
		}
		if e.Name() == "004_findings_insert_guard.sql" {
			found004SQL = true
		}
	}

	if !foundEmbedGo {
		t.Fatalf("expected embed.go to be embedded in migrations.FS")
	}
	if !found001SQL {
		t.Fatalf("expected 001_migration_metadata.sql to be embedded in migrations.FS")
	}
	if !found002SQL {
		t.Fatalf("expected 002_core_schema.sql to be embedded in migrations.FS")
	}
	if !found003SQL {
		t.Fatalf("expected 003_state_guards.sql to be embedded in migrations.FS")
	}
	if !found004SQL {
		t.Fatalf("expected 004_findings_insert_guard.sql to be embedded in migrations.FS")
	}

	// Verify discoverManifest walks the tree, ignores embed.go, and discovers production migrations
	manifest, err := discoverManifest(migrations.FS)
	if err != nil {
		t.Fatalf("discoverManifest on production FS: %v", err)
	}

	expectedMigrations := []struct {
		version int
		name    string
	}{
		{version: 1, name: "migration_metadata"},
		{version: 2, name: "core_schema"},
		{version: 3, name: "state_guards"},
		{version: 4, name: "findings_insert_guard"},
	}

	if len(manifest) != len(expectedMigrations) {
		t.Fatalf("expected %d migrations in production FS, got %d", len(expectedMigrations), len(manifest))
	}

	for i, exp := range expectedMigrations {
		if manifest[i].Version != exp.version {
			t.Errorf("manifest[%d].Version = %d, want %d", i, manifest[i].Version, exp.version)
		}
		if manifest[i].Name != exp.name {
			t.Errorf("manifest[%d].Name = %q, want %q", i, manifest[i].Name, exp.name)
		}
		if len(manifest[i].Checksum) != 64 {
			t.Errorf("manifest[%d].Checksum length = %d, want 64", i, len(manifest[i].Checksum))
		}
		if len(manifest[i].SQL) == 0 {
			t.Errorf("manifest[%d].SQL is empty", i)
		}
	}
}

func TestManifest_FilenameAcceptanceAndRejection(t *testing.T) {
	tests := []struct {
		name      string
		fsys      fstest.MapFS
		wantErr   bool
		errSubstr string
	}{
		{
			name: "valid filenames single",
			fsys: fstest.MapFS{
				"001_migration_metadata.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
			},
			wantErr: false,
		},
		{
			name: "valid filenames multiple",
			fsys: fstest.MapFS{
				"001_init.sql":        &fstest.MapFile{Data: []byte("SELECT 1;")},
				"002_create_user.sql": &fstest.MapFile{Data: []byte("SELECT 2;")},
				"999_final_step.sql":  &fstest.MapFile{Data: []byte("SELECT 3;")},
				"010_short.sql":       &fstest.MapFile{Data: []byte("SELECT 4;")},
				"011_a1_b2_c3.sql":    &fstest.MapFile{Data: []byte("SELECT 5;")},
			},
			wantErr: false,
		},
		{
			name: "version 000 is invalid",
			fsys: fstest.MapFS{
				"000_zero_invalid.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
			},
			wantErr:   true,
			errSubstr: "invalid migration version",
		},
		{
			name: "single digit version rejected",
			fsys: fstest.MapFS{
				"1_single_digit.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
			},
			wantErr:   true,
			errSubstr: "does not match migration filename pattern",
		},
		{
			name: "two digits version rejected",
			fsys: fstest.MapFS{
				"01_two_digits.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
			},
			wantErr:   true,
			errSubstr: "does not match migration filename pattern",
		},
		{
			name: "four digits version rejected",
			fsys: fstest.MapFS{
				"0001_four_digits.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
			},
			wantErr:   true,
			errSubstr: "does not match migration filename pattern",
		},
		{
			name: "empty name after underscore rejected",
			fsys: fstest.MapFS{
				"001_.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
			},
			wantErr:   true,
			errSubstr: "does not match migration filename pattern",
		},
		{
			name: "name starting with digit rejected",
			fsys: fstest.MapFS{
				"001_1start.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
			},
			wantErr:   true,
			errSubstr: "does not match migration filename pattern",
		},
		{
			name: "name with uppercase rejected",
			fsys: fstest.MapFS{
				"001_Upper_case.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
			},
			wantErr:   true,
			errSubstr: "does not match migration filename pattern",
		},
		{
			name: "name with hyphen rejected",
			fsys: fstest.MapFS{
				"001_has-hyphen.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
			},
			wantErr:   true,
			errSubstr: "does not match migration filename pattern",
		},
		{
			name: "name with space rejected",
			fsys: fstest.MapFS{
				"001_has space.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
			},
			wantErr:   true,
			errSubstr: "does not match migration filename pattern",
		},
		{
			name: "malformed top-level SQL rejected",
			fsys: fstest.MapFS{
				"create_tables.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
			},
			wantErr:   true,
			errSubstr: "does not match migration filename pattern",
		},
		{
			name: "non-SQL files such as README.md and embed.go are ignored",
			fsys: fstest.MapFS{
				"README.md":                  &fstest.MapFile{Data: []byte("# Readme")},
				"notes.txt":                  &fstest.MapFile{Data: []byte("notes")},
				".gitkeep":                   &fstest.MapFile{Data: []byte("")},
				"embed.go":                   &fstest.MapFile{Data: []byte("package migrations")},
				"001_migration_metadata.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
			},
			wantErr: false,
		},
		{
			name: "duplicate versions rejected even if names differ",
			fsys: fstest.MapFS{
				"001_first.sql":  &fstest.MapFile{Data: []byte("SELECT 1;")},
				"001_second.sql": &fstest.MapFile{Data: []byte("SELECT 2;")},
			},
			wantErr:   true,
			errSubstr: "duplicate migration version 001",
		},
		{
			name: "nested directory with SQL file rejected",
			fsys: fstest.MapFS{
				"001_init.sql":       &fstest.MapFile{Data: []byte("SELECT 1;")},
				"nested/002_sub.sql": &fstest.MapFile{Data: []byte("SELECT 2;")},
			},
			wantErr:   true,
			errSubstr: "nested migrations are rejected",
		},
		{
			name: "nested directory with non-SQL file rejected",
			fsys: fstest.MapFS{
				"001_init.sql":     &fstest.MapFile{Data: []byte("SELECT 1;")},
				"subdir/README.md": &fstest.MapFile{Data: []byte("docs")},
			},
			wantErr:   true,
			errSubstr: "nested migrations are rejected",
		},
		{
			name: "deeply nested directory rejected",
			fsys: fstest.MapFS{
				"001_init.sql":            &fstest.MapFile{Data: []byte("SELECT 1;")},
				"a/b/c/nested_script.sql": &fstest.MapFile{Data: []byte("SELECT 2;")},
			},
			wantErr:   true,
			errSubstr: "nested migrations are rejected",
		},
		{
			name: "empty SQL file rejected",
			fsys: fstest.MapFS{
				"001_empty.sql": &fstest.MapFile{Data: []byte("")},
			},
			wantErr:   true,
			errSubstr: "is empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest, err := discoverManifest(tt.fsys)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.errSubstr)
				}
				if tt.errSubstr != "" && !errors.Is(err, os.ErrNotExist) {
					if !stringContains(err.Error(), tt.errSubstr) {
						t.Fatalf("expected error containing %q, got: %v", tt.errSubstr, err)
					}
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if len(manifest) == 0 {
					t.Fatalf("expected non-empty manifest")
				}
			}
		})
	}
}

func TestManifest_NumericOrderingIndependentOfEnumeration(t *testing.T) {
	fsys := fstest.MapFS{
		"003_third.sql":  &fstest.MapFile{Data: []byte("CREATE TABLE t3 (id INT);")},
		"001_first.sql":  &fstest.MapFile{Data: []byte(exactMetadataTableSQL)},
		"002_second.sql": &fstest.MapFile{Data: []byte("CREATE TABLE t2 (id INT);")},
	}

	manifest, err := discoverManifest(fsys)
	if err != nil {
		t.Fatalf("discoverManifest: %v", err)
	}

	if len(manifest) != 3 {
		t.Fatalf("expected 3 migrations, got %d", len(manifest))
	}

	expectedOrder := []struct {
		version int
		name    string
	}{
		{1, "first"},
		{2, "second"},
		{3, "third"},
	}

	for i, exp := range expectedOrder {
		if manifest[i].Version != exp.version {
			t.Errorf("manifest[%d].Version = %d, want %d", i, manifest[i].Version, exp.version)
		}
		if manifest[i].Name != exp.name {
			t.Errorf("manifest[%d].Name = %q, want %q", i, manifest[i].Name, exp.name)
		}
	}
}

func TestManifest_ExactSHA256Checksum_NewlineMatters(t *testing.T) {
	dataWithoutNewline := []byte("SELECT 1;")
	dataWithNewline := []byte("SELECT 1;\n")

	hashWithout := sha256.Sum256(dataWithoutNewline)
	expectedWithout := hex.EncodeToString(hashWithout[:])

	hashWith := sha256.Sum256(dataWithNewline)
	expectedWith := hex.EncodeToString(hashWith[:])

	if expectedWithout == expectedWith {
		t.Fatalf("expected different checksums when trailing newline differs")
	}

	fsys1 := fstest.MapFS{
		"001_query.sql": &fstest.MapFile{Data: dataWithoutNewline},
	}
	m1, err := discoverManifest(fsys1)
	if err != nil {
		t.Fatalf("m1 discover: %v", err)
	}
	if m1[0].Checksum != expectedWithout {
		t.Fatalf("checksum mismatch: got %q, want %q", m1[0].Checksum, expectedWithout)
	}

	fsys2 := fstest.MapFS{
		"001_query.sql": &fstest.MapFile{Data: dataWithNewline},
	}
	m2, err := discoverManifest(fsys2)
	if err != nil {
		t.Fatalf("m2 discover: %v", err)
	}
	if m2[0].Checksum != expectedWith {
		t.Fatalf("checksum mismatch: got %q, want %q", m2[0].Checksum, expectedWith)
	}
}

func TestMigrate_FreshDatabaseAppliesPending(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("store.Migrate failed: %v", err)
	}

	writer, err := store.writerDB()
	if err != nil {
		t.Fatalf("writerDB: %v", err)
	}

	if !tableExists(t, writer, "schema_migrations") {
		t.Fatalf("expected schema_migrations table to exist")
	}

	applied := queryAppliedMigrations(t, writer)
	expectedApplied := []struct {
		version int
		name    string
	}{
		{version: 1, name: "migration_metadata"},
		{version: 2, name: "core_schema"},
		{version: 3, name: "state_guards"},
		{version: 4, name: "findings_insert_guard"},
	}

	if len(applied) != len(expectedApplied) {
		t.Fatalf("expected exactly %d applied migrations, got %d", len(expectedApplied), len(applied))
	}

	for i, exp := range expectedApplied {
		if applied[i].Version != exp.version {
			t.Errorf("applied[%d].Version = %d, want %d", i, applied[i].Version, exp.version)
		}
		if applied[i].Name != exp.name {
			t.Errorf("applied[%d].Name = %q, want %q", i, applied[i].Name, exp.name)
		}
		if len(applied[i].Checksum) != 64 {
			t.Errorf("applied[%d] invalid checksum length %d", i, len(applied[i].Checksum))
		}
		if err := validateAppliedAt(applied[i].AppliedAt); err != nil {
			t.Errorf("applied[%d] invalid applied_at: %v (%s)", i, err, applied[i].AppliedAt)
		}
	}
}

func TestMigrate_AtomicAppearanceOfSQLAndMetadata(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	testFS := fstest.MapFS{
		"001_metadata.sql": &fstest.MapFile{
			Data: []byte(exactMetadataTableSQL),
		},
		"002_create_atomic.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE atomic_test (id INTEGER PRIMARY KEY, val TEXT NOT NULL);"),
		},
	}

	if err := store.migrateFS(ctx, testFS); err != nil {
		t.Fatalf("migrateFS: %v", err)
	}

	writer, err := store.writerDB()
	if err != nil {
		t.Fatalf("writerDB: %v", err)
	}

	if !tableExists(t, writer, "atomic_test") {
		t.Fatalf("expected atomic_test table to exist")
	}

	applied := queryAppliedMigrations(t, writer)
	if len(applied) != 2 {
		t.Fatalf("expected 2 applied rows, got %d", len(applied))
	}
	if applied[1].Version != 2 || applied[1].Name != "create_atomic" {
		t.Fatalf("unexpected row 2: %+v", applied[1])
	}
}

func TestMigrate_IdempotentRerunPreservesTimestamps(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}

	writer, err := store.writerDB()
	if err != nil {
		t.Fatalf("writerDB: %v", err)
	}

	initialApplied := queryAppliedMigrations(t, writer)
	if len(initialApplied) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(initialApplied))
	}
	originalTimestamps := make(map[int]string)
	for _, m := range initialApplied {
		originalTimestamps[m.Version] = m.AppliedAt
	}

	origClock := clock
	clock = func() time.Time {
		return time.Now().Add(10 * time.Hour)
	}
	defer func() { clock = origClock }()

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	afterSecond := queryAppliedMigrations(t, writer)
	if len(afterSecond) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(afterSecond))
	}
	for _, m := range afterSecond {
		if m.AppliedAt != originalTimestamps[m.Version] {
			t.Fatalf("version %d applied_at changed on rerun: got %s, want %s", m.Version, m.AppliedAt, originalTimestamps[m.Version])
		}
	}

	storePath := store.canonicalPath
	storeTimeout := store.cfg.BusyTimeout

	reopenedStore, err := Open(Config{Path: storePath, BusyTimeout: storeTimeout})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopenedStore.Close()

	if err := reopenedStore.Migrate(ctx); err != nil {
		t.Fatalf("reopened Migrate: %v", err)
	}

	reopenedWriter, err := reopenedStore.writerDB()
	if err != nil {
		t.Fatalf("reopened writerDB: %v", err)
	}

	afterReopen := queryAppliedMigrations(t, reopenedWriter)
	if len(afterReopen) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(afterReopen))
	}
	for _, m := range afterReopen {
		if m.AppliedAt != originalTimestamps[m.Version] {
			t.Fatalf("version %d applied_at changed on reopen: got %s, want %s", m.Version, m.AppliedAt, originalTimestamps[m.Version])
		}
	}
}

func TestMigrate_ChangedSQLFailsClosed(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	initialFS := fstest.MapFS{
		"001_metadata.sql": &fstest.MapFile{
			Data: []byte(exactMetadataTableSQL),
		},
		"002_create_items.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE items (id INTEGER PRIMARY KEY);"),
		},
	}

	if err := store.migrateFS(ctx, initialFS); err != nil {
		t.Fatalf("initial migrateFS: %v", err)
	}

	writer, err := store.writerDB()
	if err != nil {
		t.Fatalf("writerDB: %v", err)
	}
	beforeRows := queryAppliedMigrations(t, writer)

	tamperedFS := fstest.MapFS{
		"001_metadata.sql": &fstest.MapFile{
			Data: []byte(exactMetadataTableSQL),
		},
		"002_create_items.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE items (id INTEGER PRIMARY KEY); -- tampered"),
		},
	}

	err = store.migrateFS(ctx, tamperedFS)
	if err == nil {
		t.Fatalf("expected migrateFS to fail closed on changed SQL, but succeeded")
	}
	if !stringContains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected error containing 'checksum mismatch', got: %v", err)
	}

	afterRows := queryAppliedMigrations(t, writer)
	if len(afterRows) != len(beforeRows) {
		t.Fatalf("metadata rows count changed: before=%d, after=%d", len(beforeRows), len(afterRows))
	}
	for i := range beforeRows {
		if afterRows[i] != beforeRows[i] {
			t.Fatalf("metadata row %d was modified", i)
		}
	}
}

func TestMigrate_ChangedNameFailsClosed(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	initialFS := fstest.MapFS{
		"001_metadata.sql": &fstest.MapFile{
			Data: []byte(exactMetadataTableSQL),
		},
		"002_create_items.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE items (id INTEGER PRIMARY KEY);"),
		},
	}

	if err := store.migrateFS(ctx, initialFS); err != nil {
		t.Fatalf("initial migrateFS: %v", err)
	}

	writer, err := store.writerDB()
	if err != nil {
		t.Fatalf("writerDB: %v", err)
	}
	beforeRows := queryAppliedMigrations(t, writer)

	renamedFS := fstest.MapFS{
		"001_metadata.sql": &fstest.MapFile{
			Data: []byte(exactMetadataTableSQL),
		},
		"002_create_products.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE items (id INTEGER PRIMARY KEY);"),
		},
	}

	err = store.migrateFS(ctx, renamedFS)
	if err == nil {
		t.Fatalf("expected migrateFS to fail closed on changed name, but succeeded")
	}
	if !stringContains(err.Error(), "name mismatch") {
		t.Fatalf("expected error containing 'name mismatch', got: %v", err)
	}

	afterRows := queryAppliedMigrations(t, writer)
	if len(afterRows) != len(beforeRows) {
		t.Fatalf("metadata rows count changed")
	}
}

func TestMigrate_MissingAppliedVersionFailsClosed(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	initialFS := fstest.MapFS{
		"001_metadata.sql": &fstest.MapFile{
			Data: []byte(exactMetadataTableSQL),
		},
		"002_create_items.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE items (id INTEGER PRIMARY KEY);"),
		},
	}

	if err := store.migrateFS(ctx, initialFS); err != nil {
		t.Fatalf("initial migrateFS: %v", err)
	}

	writer, err := store.writerDB()
	if err != nil {
		t.Fatalf("writerDB: %v", err)
	}

	missingFS := fstest.MapFS{
		"001_metadata.sql": &fstest.MapFile{
			Data: []byte(exactMetadataTableSQL),
		},
	}

	err = store.migrateFS(ctx, missingFS)
	if err == nil {
		t.Fatalf("expected migrateFS to fail closed on missing applied version, but succeeded")
	}
	if !stringContains(err.Error(), "missing from manifest") {
		t.Fatalf("expected error containing 'missing from manifest', got: %v", err)
	}

	afterRows := queryAppliedMigrations(t, writer)
	if len(afterRows) != 2 {
		t.Fatalf("metadata was deleted on failed verification")
	}
}

func TestMigrate_PendingMigrationFailureLeavesNeitherSchemaNorMetadata(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	fsys := fstest.MapFS{
		"001_metadata.sql": &fstest.MapFile{
			Data: []byte(exactMetadataTableSQL),
		},
		"002_fail.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE doomed_table (id INT); INVALID SQL SYNTAX ERROR;"),
		},
	}

	err := store.migrateFS(ctx, fsys)
	if err == nil {
		t.Fatalf("expected migration failure, got nil")
	}

	writer, err := store.writerDB()
	if err != nil {
		t.Fatalf("writerDB: %v", err)
	}

	if !tableExists(t, writer, "schema_migrations") {
		t.Fatalf("expected schema_migrations to exist")
	}
	if tableExists(t, writer, "doomed_table") {
		t.Fatalf("doomed_table should NOT exist after rollback")
	}

	applied := queryAppliedMigrations(t, writer)
	if len(applied) != 1 {
		t.Fatalf("expected 1 applied migration, got %d", len(applied))
	}
	if applied[0].Version != 1 {
		t.Fatalf("expected only version 1, got %d", applied[0].Version)
	}
}

func TestMigrate_MetadataInsertFailureRollsBackSchemaEffect(t *testing.T) {
	t.Run("rollback via database trigger", func(t *testing.T) {
		store := openTestStore(t)
		ctx := context.Background()

		initialFS := fstest.MapFS{
			"001_metadata.sql": &fstest.MapFile{
				Data: []byte(exactMetadataTableSQL),
			},
		}
		if err := store.migrateFS(ctx, initialFS); err != nil {
			t.Fatalf("initial migrate: %v", err)
		}

		writer, err := store.writerDB()
		if err != nil {
			t.Fatalf("writerDB: %v", err)
		}

		_, err = writer.ExecContext(ctx, "CREATE TRIGGER block_v2 BEFORE INSERT ON schema_migrations WHEN NEW.version = 2 BEGIN SELECT RAISE(ABORT, 'injected trigger failure'); END;")
		if err != nil {
			t.Fatalf("create trigger: %v", err)
		}

		step2FS := fstest.MapFS{
			"001_metadata.sql": &fstest.MapFile{
				Data: []byte(exactMetadataTableSQL),
			},
			"002_create_doomed.sql": &fstest.MapFile{
				Data: []byte("CREATE TABLE trigger_doomed (id INT PRIMARY KEY);"),
			},
		}

		err = store.migrateFS(ctx, step2FS)
		if err == nil {
			t.Fatalf("expected migrateFS to fail on metadata insert, got nil")
		}

		if tableExists(t, writer, "trigger_doomed") {
			t.Fatalf("trigger_doomed table was NOT rolled back!")
		}

		applied := queryAppliedMigrations(t, writer)
		if len(applied) != 1 {
			t.Fatalf("expected 1 applied migration, got %d", len(applied))
		}
	})

	t.Run("rollback via hook failure", func(t *testing.T) {
		store := openTestStore(t)
		ctx := context.Background()

		initialFS := fstest.MapFS{
			"001_metadata.sql": &fstest.MapFile{
				Data: []byte(exactMetadataTableSQL),
			},
		}
		if err := store.migrateFS(ctx, initialFS); err != nil {
			t.Fatalf("initial migrate: %v", err)
		}

		writer, err := store.writerDB()
		if err != nil {
			t.Fatalf("writerDB: %v", err)
		}

		hookErr := errors.New("simulated hook failure")
		beforeMetadataInsertHook = func(ctx context.Context, tx *sql.Tx, m Migration, appliedAt string) error {
			if m.Version == 2 {
				return hookErr
			}
			return nil
		}
		defer func() { beforeMetadataInsertHook = nil }()

		step2FS := fstest.MapFS{
			"001_metadata.sql": &fstest.MapFile{
				Data: []byte(exactMetadataTableSQL),
			},
			"002_create_hook_doomed.sql": &fstest.MapFile{
				Data: []byte("CREATE TABLE hook_doomed (id INT PRIMARY KEY);"),
			},
		}

		err = store.migrateFS(ctx, step2FS)
		if err == nil {
			t.Fatalf("expected migrateFS to fail via hook, got nil")
		}
		if !errors.Is(err, hookErr) {
			t.Fatalf("expected hookErr, got %v", err)
		}

		if tableExists(t, writer, "hook_doomed") {
			t.Fatalf("hook_doomed table was NOT rolled back!")
		}

		applied := queryAppliedMigrations(t, writer)
		if len(applied) != 1 {
			t.Fatalf("expected 1 applied migration, got %d", len(applied))
		}
	})
}

func TestMigrate_ContextCancellationBoundaries(t *testing.T) {
	t.Run("cancelled before start", func(t *testing.T) {
		store := openTestStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := store.Migrate(ctx)
		if err == nil {
			t.Fatalf("expected cancellation error, got nil")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected errors.Is(err, context.Canceled), got: %v", err)
		}

		writer, err := store.writerDB()
		if err != nil {
			t.Fatalf("writerDB: %v", err)
		}
		if tableExists(t, writer, "schema_migrations") {
			t.Fatalf("no migration should have run when context was cancelled before start")
		}
	})

	t.Run("cancelled before commit", func(t *testing.T) {
		store := openTestStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		fsys := fstest.MapFS{
			"001_metadata.sql": &fstest.MapFile{
				Data: []byte(exactMetadataTableSQL),
			},
			"002_step.sql": &fstest.MapFile{
				Data: []byte("CREATE TABLE step2 (id INT);"),
			},
		}

		// Cancel immediately during metadata insert hook (before Commit)
		beforeMetadataInsertHook = func(ctx context.Context, tx *sql.Tx, m Migration, appliedAt string) error {
			if m.Version == 2 {
				cancel()
			}
			return nil
		}
		defer func() { beforeMetadataInsertHook = nil }()

		err := store.migrateFS(ctx, fsys)
		if err == nil {
			t.Fatalf("expected cancellation error, got nil")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}

		writer, err := store.writerDB()
		if err != nil {
			t.Fatalf("writerDB: %v", err)
		}

		// step 2 must have rolled back
		if tableExists(t, writer, "step2") {
			t.Fatalf("step2 table should not exist after cancellation before commit")
		}
	})
}

func TestMigrate_ExactSchemaValidation(t *testing.T) {
	tests := []struct {
		name      string
		schemaSQL string
		errSubstr string
	}{
		{
			name: "extra column rejected",
			schemaSQL: `CREATE TABLE schema_migrations (
  version INTEGER PRIMARY KEY CHECK (version BETWEEN 1 AND 999),
  name TEXT NOT NULL UNIQUE,
  checksum TEXT NOT NULL CHECK (length(checksum) = 64),
  applied_at TEXT NOT NULL,
  extra_col TEXT
);`,
			errSubstr: "want exactly 4",
		},
		{
			name: "missing column rejected",
			schemaSQL: `CREATE TABLE schema_migrations (
  version INTEGER PRIMARY KEY CHECK (version BETWEEN 1 AND 999),
  name TEXT NOT NULL UNIQUE,
  checksum TEXT NOT NULL CHECK (length(checksum) = 64)
);`,
			errSubstr: "want exactly 4",
		},
		{
			name: "wrong column order: name before version",
			schemaSQL: `CREATE TABLE schema_migrations (
  name TEXT NOT NULL UNIQUE,
  version INTEGER PRIMARY KEY CHECK (version BETWEEN 1 AND 999),
  checksum TEXT NOT NULL CHECK (length(checksum) = 64),
  applied_at TEXT NOT NULL
);`,
			errSubstr: "column 0 is \"name\", want 'version'",
		},
		{
			name: "wrong type: version TEXT",
			schemaSQL: `CREATE TABLE schema_migrations (
  version TEXT PRIMARY KEY CHECK (version BETWEEN 1 AND 999),
  name TEXT NOT NULL UNIQUE,
  checksum TEXT NOT NULL CHECK (length(checksum) = 64),
  applied_at TEXT NOT NULL
);`,
			errSubstr: "must have type INTEGER",
		},
		{
			name: "wrong type: name INTEGER",
			schemaSQL: `CREATE TABLE schema_migrations (
  version INTEGER PRIMARY KEY CHECK (version BETWEEN 1 AND 999),
  name INTEGER NOT NULL UNIQUE,
  checksum TEXT NOT NULL CHECK (length(checksum) = 64),
  applied_at TEXT NOT NULL
);`,
			errSubstr: "must have type TEXT",
		},
		{
			name: "wrong type: checksum BLOB",
			schemaSQL: `CREATE TABLE schema_migrations (
  version INTEGER PRIMARY KEY CHECK (version BETWEEN 1 AND 999),
  name TEXT NOT NULL UNIQUE,
  checksum BLOB NOT NULL CHECK (length(checksum) = 64),
  applied_at TEXT NOT NULL
);`,
			errSubstr: "must have type TEXT",
		},
		{
			name: "wrong type: applied_at INTEGER",
			schemaSQL: `CREATE TABLE schema_migrations (
  version INTEGER PRIMARY KEY CHECK (version BETWEEN 1 AND 999),
  name TEXT NOT NULL UNIQUE,
  checksum TEXT NOT NULL CHECK (length(checksum) = 64),
  applied_at INTEGER NOT NULL
);`,
			errSubstr: "must have type TEXT",
		},
		{
			name: "version not PRIMARY KEY",
			schemaSQL: `CREATE TABLE schema_migrations (
  version INTEGER CHECK (version BETWEEN 1 AND 999),
  name TEXT NOT NULL UNIQUE,
  checksum TEXT NOT NULL CHECK (length(checksum) = 64),
  applied_at TEXT NOT NULL
);`,
			errSubstr: "must be PRIMARY KEY",
		},
		{
			name: "name nullable",
			schemaSQL: `CREATE TABLE schema_migrations (
  version INTEGER PRIMARY KEY CHECK (version BETWEEN 1 AND 999),
  name TEXT UNIQUE,
  checksum TEXT NOT NULL CHECK (length(checksum) = 64),
  applied_at TEXT NOT NULL
);`,
			errSubstr: "must be NOT NULL",
		},
		{
			name: "checksum nullable",
			schemaSQL: `CREATE TABLE schema_migrations (
  version INTEGER PRIMARY KEY CHECK (version BETWEEN 1 AND 999),
  name TEXT NOT NULL UNIQUE,
  checksum TEXT CHECK (length(checksum) = 64),
  applied_at TEXT NOT NULL
);`,
			errSubstr: "must be NOT NULL",
		},
		{
			name: "applied_at nullable",
			schemaSQL: `CREATE TABLE schema_migrations (
  version INTEGER PRIMARY KEY CHECK (version BETWEEN 1 AND 999),
  name TEXT NOT NULL UNIQUE,
  checksum TEXT NOT NULL CHECK (length(checksum) = 64),
  applied_at TEXT
);`,
			errSubstr: "must be NOT NULL",
		},
		{
			name: "name not UNIQUE",
			schemaSQL: `CREATE TABLE schema_migrations (
  version INTEGER PRIMARY KEY CHECK (version BETWEEN 1 AND 999),
  name TEXT NOT NULL,
  checksum TEXT NOT NULL CHECK (length(checksum) = 64),
  applied_at TEXT NOT NULL
);`,
			errSubstr: "must enforce UNIQUE constraint on 'name'",
		},
		{
			name: "missing CHECK constraint on version",
			schemaSQL: `CREATE TABLE schema_migrations (
  version INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  checksum TEXT NOT NULL CHECK (length(checksum) = 64),
  applied_at TEXT NOT NULL
);`,
			errSubstr: "missing required CHECK constraint on version",
		},
		{
			name: "missing CHECK constraint on checksum",
			schemaSQL: `CREATE TABLE schema_migrations (
  version INTEGER PRIMARY KEY CHECK (version BETWEEN 1 AND 999),
  name TEXT NOT NULL UNIQUE,
  checksum TEXT NOT NULL,
  applied_at TEXT NOT NULL
);`,
			errSubstr: "missing required CHECK constraint on checksum",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openTestStore(t)
			writer, err := store.writerDB()
			if err != nil {
				t.Fatalf("writerDB: %v", err)
			}
			if _, err := writer.Exec(tt.schemaSQL); err != nil {
				t.Fatalf("create invalid table: %v", err)
			}

			err = store.Migrate(context.Background())
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.errSubstr)
			}
			if !stringContains(err.Error(), tt.errSubstr) {
				t.Fatalf("expected error containing %q, got: %v", tt.errSubstr, err)
			}
		})
	}
}

func TestMigrate_AppliedAtValidation(t *testing.T) {
	tests := []struct {
		name      string
		timestamp string
		wantErr   bool
		errSubstr string
	}{
		{
			name:      "valid UTC with nanoseconds and Z suffix",
			timestamp: "2026-09-19T12:00:00.123456789Z",
			wantErr:   false,
		},
		{
			name:      "valid UTC whole seconds with Z suffix",
			timestamp: "2026-09-19T12:00:00Z",
			wantErr:   false,
		},
		{
			name:      "rejected timezone offset +00:00",
			timestamp: "2026-09-19T12:00:00+00:00",
			wantErr:   true,
			errSubstr: "must end with 'Z' suffix",
		},
		{
			name:      "rejected timezone offset +07:00",
			timestamp: "2026-09-19T12:00:00+07:00",
			wantErr:   true,
			errSubstr: "must end with 'Z' suffix",
		},
		{
			name:      "rejected timezone offset -05:00",
			timestamp: "2026-09-19T12:00:00-05:00",
			wantErr:   true,
			errSubstr: "must end with 'Z' suffix",
		},
		{
			name:      "rejected space separated date time",
			timestamp: "2026-09-19 12:00:00Z",
			wantErr:   true,
			errSubstr: "is not valid RFC3339Nano",
		},
		{
			name:      "rejected arbitrary string",
			timestamp: "not-a-timestamp",
			wantErr:   true,
			errSubstr: "must end with 'Z' suffix",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAppliedAt(tt.timestamp)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.errSubstr)
				}
				if !stringContains(err.Error(), tt.errSubstr) {
					t.Fatalf("expected error containing %q, got: %v", tt.errSubstr, err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestMigrate_MetadataRowIntegrityRejection(t *testing.T) {
	ctx := context.Background()

	t.Run("uppercase hex checksum rejected", func(t *testing.T) {
		store := openTestStore(t)
		if err := store.Migrate(ctx); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		writer, _ := store.writerDB()
		_, err := writer.ExecContext(ctx, "UPDATE schema_migrations SET checksum = 'E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855' WHERE version = 1;")
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		err = store.Migrate(ctx)
		if err == nil {
			t.Fatalf("expected error on uppercase checksum in metadata")
		}
		if !stringContains(err.Error(), "lowercase hexadecimal") {
			t.Fatalf("expected 'lowercase hexadecimal' in error, got: %v", err)
		}
	})

	t.Run("non-hex checksum rejected", func(t *testing.T) {
		store := openTestStore(t)
		if err := store.Migrate(ctx); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		writer, _ := store.writerDB()
		_, err := writer.ExecContext(ctx, "UPDATE schema_migrations SET checksum = 'g3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85z' WHERE version = 1;")
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		err = store.Migrate(ctx)
		if err == nil {
			t.Fatalf("expected error on non-hex checksum in metadata")
		}
		if !stringContains(err.Error(), "lowercase hexadecimal") {
			t.Fatalf("expected 'lowercase hexadecimal' in error, got: %v", err)
		}
	})

	t.Run("offset timestamp in table rejected", func(t *testing.T) {
		store := openTestStore(t)
		if err := store.Migrate(ctx); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		writer, _ := store.writerDB()
		_, err := writer.ExecContext(ctx, "UPDATE schema_migrations SET applied_at = '2026-09-19T12:00:00+00:00' WHERE version = 1;")
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		err = store.Migrate(ctx)
		if err == nil {
			t.Fatalf("expected error on offset timestamp in metadata")
		}
		if !stringContains(err.Error(), "must end with 'Z' suffix") {
			t.Fatalf("expected 'must end with 'Z' suffix' in error, got: %v", err)
		}
	})
}

// TestMigrate_CoreProductSchemaCreatedByP2 verifies that running production migrations
// creates exactly the schema_migrations metadata table and the six core production tables
// (snapshots, evidence_units, sessions, role_runs, findings, finding_basis_refs),
// and no unexpected tables or triggers. Replaces obsolete TestMigrate_NoProductSchemaCreatedByP1B.
func TestMigrate_CoreProductSchemaCreatedByP2(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	writer, err := store.writerDB()
	if err != nil {
		t.Fatalf("writerDB: %v", err)
	}

	rows, err := writer.Query("SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name ASC;")
	if err != nil {
		t.Fatalf("query tables: %v", err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		tables = append(tables, name)
	}

	expectedTables := []string{
		"evidence_units",
		"finding_basis_refs",
		"findings",
		"role_runs",
		"schema_migrations",
		"sessions",
		"snapshots",
	}

	if len(tables) != len(expectedTables) {
		t.Fatalf("expected tables %v, got: %v", expectedTables, tables)
	}
	for i, expected := range expectedTables {
		if tables[i] != expected {
			t.Errorf("tables[%d] = %q, want %q", i, tables[i], expected)
		}
	}

	// Verify state and citation guard triggers exist after P3 migration
	var triggerCount int
	err = writer.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger';").Scan(&triggerCount)
	if err != nil {
		t.Fatalf("query triggers: %v", err)
	}
	if triggerCount == 0 {
		t.Errorf("expected state and citation guard triggers in P3 schema, found 0")
	}

	forbiddenTables := []string{
		"session",
		"roles", "role",
		"snapshot",
		"snapshot_units", "units",
		"finding",
		"basis_references", "refs",
	}

	for _, tbl := range forbiddenTables {
		if tableExists(t, writer, tbl) {
			t.Errorf("forbidden/non-canonical table %q was created!", tbl)
		}
	}
}

func TestMigrate_TwoSeparateDatabasesNeverShareState(t *testing.T) {
	store1 := openTestStore(t)
	store2 := openTestStore(t)
	ctx := context.Background()

	fsys1 := fstest.MapFS{
		"001_metadata.sql": &fstest.MapFile{
			Data: []byte(exactMetadataTableSQL),
		},
		"002_db1_table.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE db1_table (id INT);"),
		},
	}

	if err := store1.migrateFS(ctx, fsys1); err != nil {
		t.Fatalf("store1 migrate: %v", err)
	}

	writer1, _ := store1.writerDB()
	writer2, _ := store2.writerDB()

	if !tableExists(t, writer1, "schema_migrations") || !tableExists(t, writer1, "db1_table") {
		t.Fatalf("store1 tables missing")
	}

	if tableExists(t, writer2, "schema_migrations") || tableExists(t, writer2, "db1_table") {
		t.Fatalf("store2 saw store1's tables!")
	}

	fsys2 := fstest.MapFS{
		"001_metadata.sql": &fstest.MapFile{
			Data: []byte(exactMetadataTableSQL),
		},
		"002_db2_table.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE db2_table (id INT);"),
		},
	}
	if err := store2.migrateFS(ctx, fsys2); err != nil {
		t.Fatalf("store2 migrate: %v", err)
	}

	if !tableExists(t, writer2, "db2_table") {
		t.Fatalf("store2 missing db2_table")
	}
	if tableExists(t, writer1, "db2_table") {
		t.Fatalf("store1 saw store2's db2_table!")
	}
}

func TestMigrate_EdgeCases(t *testing.T) {
	ctx := context.Background()

	t.Run("nil store", func(t *testing.T) {
		var s *Store
		if err := s.Migrate(ctx); err == nil {
			t.Fatalf("expected error on nil store")
		}
	})

	t.Run("closed store", func(t *testing.T) {
		store := openTestStore(t)
		_ = store.Close()
		err := store.Migrate(ctx)
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("expected ErrClosed, got %v", err)
		}
	})

	t.Run("fresh database without 001 metadata migration", func(t *testing.T) {
		store := openTestStore(t)
		badFS := fstest.MapFS{
			"002_something.sql": &fstest.MapFile{
				Data: []byte("CREATE TABLE foo (id INT);"),
			},
		}
		err := store.migrateFS(ctx, badFS)
		if err == nil {
			t.Fatalf("expected error when fresh database does not start with 001, got nil")
		}
		if !stringContains(err.Error(), "version 001") {
			t.Fatalf("expected error mentioning version 001, got: %v", err)
		}
	})

	t.Run("unapplied migration lower than max applied version fails closed", func(t *testing.T) {
		store := openTestStore(t)
		gapFS := fstest.MapFS{
			"001_metadata.sql": &fstest.MapFile{
				Data: []byte(exactMetadataTableSQL),
			},
			"003_three.sql": &fstest.MapFile{
				Data: []byte("CREATE TABLE t3 (id INT);"),
			},
		}
		if err := store.migrateFS(ctx, gapFS); err != nil {
			t.Fatalf("gapFS migrate: %v", err)
		}

		with002FS := fstest.MapFS{
			"001_metadata.sql": &fstest.MapFile{
				Data: []byte(exactMetadataTableSQL),
			},
			"002_two.sql": &fstest.MapFile{
				Data: []byte("CREATE TABLE t2 (id INT);"),
			},
			"003_three.sql": &fstest.MapFile{
				Data: []byte("CREATE TABLE t3 (id INT);"),
			},
		}
		err := store.migrateFS(ctx, with002FS)
		if err == nil {
			t.Fatalf("expected error when unapplied migration is lower than max applied, got nil")
		}
		if !stringContains(err.Error(), "lower than max applied version") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("multiple SQL statements in single migration", func(t *testing.T) {
		store := openTestStore(t)
		multiFS := fstest.MapFS{
			"001_metadata.sql": &fstest.MapFile{
				Data: []byte(exactMetadataTableSQL),
			},
			"002_multi.sql": &fstest.MapFile{
				Data: []byte("CREATE TABLE t_multi_1 (id INT); CREATE TABLE t_multi_2 (id INT);"),
			},
		}
		if err := store.migrateFS(ctx, multiFS); err != nil {
			t.Fatalf("multiFS migrate: %v", err)
		}
		writer, _ := store.writerDB()
		if !tableExists(t, writer, "t_multi_1") || !tableExists(t, writer, "t_multi_2") {
			t.Fatalf("expected both t_multi_1 and t_multi_2 to exist")
		}
	})
}
