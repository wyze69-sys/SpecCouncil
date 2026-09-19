package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"
)

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
			name: "non-SQL files such as README.md are ignored",
			fsys: fstest.MapFS{
				"README.md":                  &fstest.MapFile{Data: []byte("# Readme")},
				"notes.txt":                  &fstest.MapFile{Data: []byte("notes")},
				".gitkeep":                   &fstest.MapFile{Data: []byte("")},
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
			name: "subdirectories with files are rejected",
			fsys: fstest.MapFS{
				"001_init.sql":       &fstest.MapFile{Data: []byte("SELECT 1;")},
				"nested/002_sub.sql": &fstest.MapFile{Data: []byte("SELECT 2;")},
			},
			wantErr:   true,
			errSubstr: "nested migrations are rejected",
		},
		{
			name: "subdirectories with non-sql files are rejected",
			fsys: fstest.MapFS{
				"001_init.sql":     &fstest.MapFile{Data: []byte("SELECT 1;")},
				"subdir/README.md": &fstest.MapFile{Data: []byte("docs")},
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
					// verify error text
					errMsg := err.Error()
					found := false
					for _, part := range []string{tt.errSubstr} {
						if len(errMsg) >= len(part) && (errMsg == part || stringContains(errMsg, part)) {
							found = true
							break
						}
					}
					if !found {
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

func stringContains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || (len(substr) > 0 && len(s) > 0 && func() bool {
		for i := 0; i+len(substr) <= len(s); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	}()))
}

func TestManifest_NumericOrderingIndependentOfEnumeration(t *testing.T) {
	// Provide entries in scrambled order
	fsys := fstest.MapFS{
		"003_third.sql":  &fstest.MapFile{Data: []byte("CREATE TABLE t3 (id INT);")},
		"001_first.sql":  &fstest.MapFile{Data: []byte("CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TEXT NOT NULL);")},
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

	// Migrate with embedded production migrations
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
	if len(applied) != 1 {
		t.Fatalf("expected exactly 1 applied migration, got %d", len(applied))
	}

	if applied[0].Version != 1 {
		t.Errorf("applied version = %d, want 1", applied[0].Version)
	}
	if applied[0].Name != "migration_metadata" {
		t.Errorf("applied name = %q, want 'migration_metadata'", applied[0].Name)
	}
	if len(applied[0].Checksum) != 64 {
		t.Errorf("invalid checksum length %d", len(applied[0].Checksum))
	}
	if _, err := time.Parse(time.RFC3339Nano, applied[0].AppliedAt); err != nil {
		t.Errorf("applied_at is not valid RFC3339Nano: %v (%s)", err, applied[0].AppliedAt)
	}
}

func TestMigrate_AtomicAppearanceOfSQLAndMetadata(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	testFS := fstest.MapFS{
		"001_metadata.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TEXT NOT NULL);"),
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
	if len(initialApplied) != 1 {
		t.Fatalf("expected 1 row, got %d", len(initialApplied))
	}
	originalTimestamp := initialApplied[0].AppliedAt

	// Advance controlled clock if clock is called, to prove rerun does not touch applied_at
	origClock := clock
	clock = func() time.Time {
		return time.Now().Add(10 * time.Hour)
	}
	defer func() { clock = origClock }()

	// Rerun 1: immediately on the same store instance
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	afterSecond := queryAppliedMigrations(t, writer)
	if len(afterSecond) != 1 {
		t.Fatalf("expected 1 row, got %d", len(afterSecond))
	}
	if afterSecond[0].AppliedAt != originalTimestamp {
		t.Fatalf("applied_at changed on rerun: got %s, want %s", afterSecond[0].AppliedAt, originalTimestamp)
	}

	// Rerun 2: reopen store from the same path and rerun
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
	if len(afterReopen) != 1 {
		t.Fatalf("expected 1 row, got %d", len(afterReopen))
	}
	if afterReopen[0].AppliedAt != originalTimestamp {
		t.Fatalf("applied_at changed on reopen: got %s, want %s", afterReopen[0].AppliedAt, originalTimestamp)
	}
}

func TestMigrate_ChangedSQLFailsClosed(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	initialFS := fstest.MapFS{
		"001_metadata.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TEXT NOT NULL);"),
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

	// Modify SQL of version 2 (even whitespace change)
	tamperedFS := fstest.MapFS{
		"001_metadata.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TEXT NOT NULL);"),
		},
		"002_create_items.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE items (id INTEGER PRIMARY KEY); -- modified comment"),
		},
	}

	err = store.migrateFS(ctx, tamperedFS)
	if err == nil {
		t.Fatalf("expected migrateFS to fail closed on changed SQL, but succeeded")
	}

	if !stringContains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected error containing 'checksum mismatch', got: %v", err)
	}

	// Verify metadata in DB is untouched
	afterRows := queryAppliedMigrations(t, writer)
	if len(afterRows) != len(beforeRows) {
		t.Fatalf("metadata rows count changed: before=%d, after=%d", len(beforeRows), len(afterRows))
	}
	for i := range beforeRows {
		if afterRows[i] != beforeRows[i] {
			t.Fatalf("metadata row %d was modified: %+v vs %+v", i, afterRows[i], beforeRows[i])
		}
	}
}

func TestMigrate_ChangedNameFailsClosed(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	initialFS := fstest.MapFS{
		"001_metadata.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TEXT NOT NULL);"),
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

	// Rename version 2 file
	renamedFS := fstest.MapFS{
		"001_metadata.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TEXT NOT NULL);"),
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

	// Verify metadata was not deleted or altered
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

func TestMigrate_MissingAppliedVersionFailsClosed(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	initialFS := fstest.MapFS{
		"001_metadata.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TEXT NOT NULL);"),
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

	// Missing version 2 from manifest
	missingFS := fstest.MapFS{
		"001_metadata.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TEXT NOT NULL);"),
		},
	}

	err = store.migrateFS(ctx, missingFS)
	if err == nil {
		t.Fatalf("expected migrateFS to fail closed on missing applied version, but succeeded")
	}

	if !stringContains(err.Error(), "missing from manifest") {
		t.Fatalf("expected error containing 'missing from manifest', got: %v", err)
	}

	// Verify metadata was not deleted
	afterRows := queryAppliedMigrations(t, writer)
	if len(afterRows) != len(beforeRows) {
		t.Fatalf("metadata rows count changed: before=%d, after=%d", len(beforeRows), len(afterRows))
	}
}

func TestMigrate_PendingMigrationFailureLeavesNeitherSchemaNorMetadata(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	fsys := fstest.MapFS{
		"001_metadata.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TEXT NOT NULL);"),
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

	// 001 succeeded
	if !tableExists(t, writer, "schema_migrations") {
		t.Fatalf("expected schema_migrations to exist")
	}

	// 002 schema effect rolled back
	if tableExists(t, writer, "doomed_table") {
		t.Fatalf("doomed_table should NOT exist after rollback")
	}

	// 002 metadata row does NOT exist
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
				Data: []byte("CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TEXT NOT NULL);"),
			},
		}
		if err := store.migrateFS(ctx, initialFS); err != nil {
			t.Fatalf("initial migrate: %v", err)
		}

		writer, err := store.writerDB()
		if err != nil {
			t.Fatalf("writerDB: %v", err)
		}

		// Create trigger on schema_migrations that aborts insert for version 2
		_, err = writer.ExecContext(ctx, "CREATE TRIGGER block_v2 BEFORE INSERT ON schema_migrations WHEN NEW.version = 2 BEGIN SELECT RAISE(ABORT, 'injected trigger failure'); END;")
		if err != nil {
			t.Fatalf("create trigger: %v", err)
		}

		step2FS := fstest.MapFS{
			"001_metadata.sql": &fstest.MapFile{
				Data: []byte("CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TEXT NOT NULL);"),
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
				Data: []byte("CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TEXT NOT NULL);"),
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
				Data: []byte("CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TEXT NOT NULL);"),
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

func TestMigrate_ContextCancellation(t *testing.T) {
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

	t.Run("cancelled before second migration", func(t *testing.T) {
		store := openTestStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		fsys := fstest.MapFS{
			"001_metadata.sql": &fstest.MapFile{
				Data: []byte("CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TEXT NOT NULL);"),
			},
			"002_step.sql": &fstest.MapFile{
				Data: []byte("CREATE TABLE step2 (id INT);"),
			},
		}

		origClock := clock
		clock = func() time.Time {
			cancel() // cancel during execution of step 1
			return time.Now()
		}
		defer func() { clock = origClock }()

		err := store.migrateFS(ctx, fsys)
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}

		writer, err := store.writerDB()
		if err != nil {
			t.Fatalf("writerDB: %v", err)
		}

		// step 2 should NOT have run
		if tableExists(t, writer, "step2") {
			t.Fatalf("step2 table should not exist")
		}
	})
}

func TestMigrate_NoProductSchemaCreatedByP1B(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	writer, err := store.writerDB()
	if err != nil {
		t.Fatalf("writerDB: %v", err)
	}

	rows, err := writer.Query("SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%';")
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

	if len(tables) != 1 || tables[0] != "schema_migrations" {
		t.Fatalf("expected ONLY [schema_migrations] table, got: %v", tables)
	}

	// Explicitly check known product tables
	forbiddenTables := []string{
		"sessions", "session",
		"roles", "role", "role_runs",
		"snapshots", "snapshot",
		"snapshot_units", "units",
		"findings", "finding",
		"basis_references", "refs",
	}

	for _, tbl := range forbiddenTables {
		if tableExists(t, writer, tbl) {
			t.Errorf("forbidden product table %q was created by P1B!", tbl)
		}
	}
}

func TestMigrate_TwoSeparateDatabasesNeverShareState(t *testing.T) {
	store1 := openTestStore(t)
	store2 := openTestStore(t)
	ctx := context.Background()

	fsys1 := fstest.MapFS{
		"001_metadata.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TEXT NOT NULL);"),
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

	// Store 1 has schema_migrations and db1_table
	if !tableExists(t, writer1, "schema_migrations") || !tableExists(t, writer1, "db1_table") {
		t.Fatalf("store1 tables missing")
	}

	// Store 2 has NO tables
	if tableExists(t, writer2, "schema_migrations") || tableExists(t, writer2, "db1_table") {
		t.Fatalf("store2 saw store1's tables!")
	}

	// Now migrate store 2 with independent tables
	fsys2 := fstest.MapFS{
		"001_metadata.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TEXT NOT NULL);"),
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

func TestMigrate_EdgeCasesAndInvalidMetadata(t *testing.T) {
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
		// Apply 001 and 003
		gapFS := fstest.MapFS{
			"001_metadata.sql": &fstest.MapFile{
				Data: []byte("CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TEXT NOT NULL);"),
			},
			"003_three.sql": &fstest.MapFile{
				Data: []byte("CREATE TABLE t3 (id INT);"),
			},
		}
		if err := store.migrateFS(ctx, gapFS); err != nil {
			t.Fatalf("gapFS migrate: %v", err)
		}

		// Now add 002
		with002FS := fstest.MapFS{
			"001_metadata.sql": &fstest.MapFile{
				Data: []byte("CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TEXT NOT NULL);"),
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

	t.Run("corrupted metadata in table - invalid name", func(t *testing.T) {
		store := openTestStore(t)
		if err := store.Migrate(ctx); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		writer, _ := store.writerDB()
		_, err := writer.ExecContext(ctx, "UPDATE schema_migrations SET name = 'Invalid-Name' WHERE version = 1;")
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		err = store.Migrate(ctx)
		if err == nil {
			t.Fatalf("expected error on invalid migration name in metadata")
		}
	})

	t.Run("corrupted metadata in table - invalid checksum", func(t *testing.T) {
		store := openTestStore(t)
		if err := store.Migrate(ctx); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		writer, _ := store.writerDB()
		_, err := writer.ExecContext(ctx, "UPDATE schema_migrations SET checksum = 'not-a-valid-hex' WHERE version = 1;")
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		err = store.Migrate(ctx)
		if err == nil {
			t.Fatalf("expected error on invalid checksum in metadata")
		}
	})

	t.Run("corrupted metadata in table - invalid timestamp", func(t *testing.T) {
		store := openTestStore(t)
		if err := store.Migrate(ctx); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		writer, _ := store.writerDB()
		_, err := writer.ExecContext(ctx, "UPDATE schema_migrations SET applied_at = 'not-a-timestamp' WHERE version = 1;")
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		err = store.Migrate(ctx)
		if err == nil {
			t.Fatalf("expected error on invalid timestamp in metadata")
		}
	})

	t.Run("schema_migrations table missing required column", func(t *testing.T) {
		store := openTestStore(t)
		writer, _ := store.writerDB()
		_, err := writer.ExecContext(ctx, "CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY);")
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		err = store.Migrate(ctx)
		if err == nil {
			t.Fatalf("expected error when schema_migrations misses columns")
		}
		if !stringContains(err.Error(), "missing required column") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("multiple SQL statements in single migration", func(t *testing.T) {
		store := openTestStore(t)
		multiFS := fstest.MapFS{
			"001_metadata.sql": &fstest.MapFile{
				Data: []byte("CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TEXT NOT NULL);"),
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
