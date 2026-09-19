package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/storage/sqlite/migrations"
)

func openMigratedStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	store, _ := setupTestStore(t, 100*time.Millisecond)
	// Apply migrations through 002 to test the core P2 schema fixture in isolation
	p2FS := fstest.MapFS{}
	for _, name := range []string{"001_migration_metadata.sql", "002_core_schema.sql"} {
		data, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		p2FS[name] = &fstest.MapFile{Data: data}
	}
	if err := store.migrateFS(context.Background(), p2FS); err != nil {
		t.Fatalf("migrate P2 schema fixture: %v", err)
	}
	writer, err := store.writerDB()
	if err != nil {
		t.Fatalf("writerDB: %v", err)
	}
	return store, writer
}

// TestCoreSchema_TablesAndColumnsExist verifies that all 6 core production tables
// and their expected columns exist after running migration 002.
func TestCoreSchema_TablesAndColumnsExist(t *testing.T) {
	_, writer := openMigratedStore(t)
	ctx := context.Background()

	expectedTables := map[string][]string{
		"snapshots": {
			"id", "hash", "project_id", "title", "content", "normalization_version", "created_at",
		},
		"evidence_units": {
			"id", "snapshot_id", "unit_id", "ordinal", "kind", "text",
		},
		"sessions": {
			"id", "project_id", "idempotency_key", "request_hash", "snapshot_id",
			"status", "cancel_requested", "dispatch_cutoff_at", "hard_deadline_at",
			"terminal_reason", "completed_role_count", "incomplete_role_count",
			"created_at", "claimed_at", "terminal_at",
		},
		"role_runs": {
			"id", "session_id", "role", "status", "cause", "error_category",
			"error_message", "call_count", "started_at", "completed_at", "created_at",
		},
		"findings": {
			"id", "role_run_id", "finding_id", "severity", "category",
			"issue", "recommendation", "created_at",
		},
		"finding_basis_refs": {
			"id", "finding_id", "evidence_unit_id", "ordinal",
		},
	}

	for table, cols := range expectedTables {
		t.Run("table_"+table, func(t *testing.T) {
			var count int
			err := writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?;", table).Scan(&count)
			if err != nil || count != 1 {
				t.Fatalf("table %q does not exist in database (err: %v, count: %d)", table, err, count)
			}

			rows, err := writer.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s);", table))
			if err != nil {
				t.Fatalf("pragma table_info(%s): %v", table, err)
			}
			defer rows.Close()

			actualCols := make(map[string]bool)
			for rows.Next() {
				var cid, notnull, pk int
				var name, colType string
				var dfltVal sql.NullString
				if err := rows.Scan(&cid, &name, &colType, &notnull, &dfltVal, &pk); err != nil {
					t.Fatalf("scan col info: %v", err)
				}
				actualCols[name] = true
			}

			for _, wantCol := range cols {
				if !actualCols[wantCol] {
					t.Errorf("table %q missing expected column %q", table, wantCol)
				}
			}
		})
	}
}

// TestCoreSchema_ForeignKeys verifies foreign-key rejection on orphan records.
func TestCoreSchema_ForeignKeys(t *testing.T) {
	_, writer := openMigratedStore(t)
	ctx := context.Background()

	t.Run("orphan_evidence_unit", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, "INSERT INTO evidence_units (id, snapshot_id, unit_id, ordinal, kind, text) VALUES ('eu1', 'nonexistent_snap', 'u1', 0, 'brief', 'text');")
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
			t.Errorf("expected foreign key constraint error, got: %v", err)
		}
	})

	t.Run("orphan_session", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `INSERT INTO sessions (id, project_id, idempotency_key, request_hash, snapshot_id, status, created_at)
			VALUES ('s1', 'p1', 'k1', '0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef', 'nonexistent_snap', 'queued', '2026-09-19T12:00:00Z');`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
			t.Errorf("expected foreign key constraint error, got: %v", err)
		}
	})

	t.Run("orphan_role_run", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `INSERT INTO role_runs (id, session_id, role, status, created_at)
			VALUES ('rr1', 'nonexistent_session', 'requirements', 'pending', '2026-09-19T12:00:00Z');`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
			t.Errorf("expected foreign key constraint error, got: %v", err)
		}
	})

	t.Run("orphan_finding", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `INSERT INTO findings (id, role_run_id, finding_id, severity, category, issue, recommendation, created_at)
			VALUES ('f1', 'nonexistent_rr', 'find-1', 'high', 'security', 'issue text', 'rec text', '2026-09-19T12:00:00Z');`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
			t.Errorf("expected foreign key constraint error, got: %v", err)
		}
	})

	t.Run("orphan_finding_basis_ref_finding", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `INSERT INTO finding_basis_refs (id, finding_id, evidence_unit_id, ordinal)
			VALUES ('fbr1', 'nonexistent_finding', 'eu1', 1);`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
			t.Errorf("expected foreign key constraint error, got: %v", err)
		}
	})
}

// TestCoreSchema_UniquenessConstraints verifies rejection of duplicate keys.
func TestCoreSchema_UniquenessConstraints(t *testing.T) {
	_, writer := openMigratedStore(t)
	ctx := context.Background()

	const hash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	// 1. Snapshot uniqueness
	_, err := writer.ExecContext(ctx, `INSERT INTO snapshots (id, hash, project_id, title, content, normalization_version, created_at)
		VALUES ('snap1', ?, 'proj1', 'Title', 'Content', 1, '2026-09-19T12:00:00Z');`, hash)
	if err != nil {
		t.Fatalf("insert snap1: %v", err)
	}

	_, err = writer.ExecContext(ctx, `INSERT INTO snapshots (id, hash, project_id, title, content, normalization_version, created_at)
		VALUES ('snap1', ?, 'proj1', 'Title2', 'Content2', 1, '2026-09-19T12:00:00Z');`, hash)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Errorf("expected unique constraint error on duplicate snapshot id, got: %v", err)
	}

	// 2. Evidence unit uniqueness: (snapshot_id, unit_id) and (snapshot_id, ordinal)
	_, err = writer.ExecContext(ctx, `INSERT INTO evidence_units (id, snapshot_id, unit_id, ordinal, kind, text)
		VALUES ('eu1', 'snap1', 'req-1', 0, 'requirement', 'text 1');`)
	if err != nil {
		t.Fatalf("insert eu1: %v", err)
	}

	// Duplicate unit_id for same snapshot
	_, err = writer.ExecContext(ctx, `INSERT INTO evidence_units (id, snapshot_id, unit_id, ordinal, kind, text)
		VALUES ('eu2', 'snap1', 'req-1', 1, 'requirement', 'text 2');`)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Errorf("expected unique error on duplicate (snapshot_id, unit_id), got: %v", err)
	}

	// Duplicate ordinal for same snapshot
	_, err = writer.ExecContext(ctx, `INSERT INTO evidence_units (id, snapshot_id, unit_id, ordinal, kind, text)
		VALUES ('eu3', 'snap1', 'req-2', 0, 'requirement', 'text 3');`)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Errorf("expected unique error on duplicate (snapshot_id, ordinal), got: %v", err)
	}

	// 3. Sessions uniqueness: UNIQUE(project_id, idempotency_key)
	_, err = writer.ExecContext(ctx, `INSERT INTO sessions (id, project_id, idempotency_key, request_hash, snapshot_id, status, created_at)
		VALUES ('sess1', 'proj1', 'idem-key-1', ?, 'snap1', 'queued', '2026-09-19T12:00:00Z');`, hash)
	if err != nil {
		t.Fatalf("insert sess1: %v", err)
	}

	// Duplicate idempotency_key for same project_id
	_, err = writer.ExecContext(ctx, `INSERT INTO sessions (id, project_id, idempotency_key, request_hash, snapshot_id, status, created_at)
		VALUES ('sess2', 'proj1', 'idem-key-1', ?, 'snap1', 'queued', '2026-09-19T12:00:00Z');`, hash)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Errorf("expected unique error on duplicate (project_id, idempotency_key), got: %v", err)
	}

	// Different project_id can reuse idempotency_key
	_, err = writer.ExecContext(ctx, `INSERT INTO sessions (id, project_id, idempotency_key, request_hash, snapshot_id, status, created_at)
		VALUES ('sess3', 'proj2', 'idem-key-1', ?, 'snap1', 'queued', '2026-09-19T12:00:00Z');`, hash)
	if err != nil {
		t.Errorf("different project reusing idempotency key should succeed, got: %v", err)
	}

	// 4. Role runs uniqueness: UNIQUE(session_id, role)
	_, err = writer.ExecContext(ctx, `INSERT INTO role_runs (id, session_id, role, status, created_at)
		VALUES ('rr1', 'sess1', 'requirements', 'pending', '2026-09-19T12:00:00Z');`)
	if err != nil {
		t.Fatalf("insert rr1: %v", err)
	}

	_, err = writer.ExecContext(ctx, `INSERT INTO role_runs (id, session_id, role, status, created_at)
		VALUES ('rr2', 'sess1', 'requirements', 'pending', '2026-09-19T12:00:00Z');`)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Errorf("expected unique error on duplicate (session_id, role), got: %v", err)
	}

	// 5. Findings uniqueness: UNIQUE(role_run_id, finding_id)
	_, err = writer.ExecContext(ctx, `INSERT INTO findings (id, role_run_id, finding_id, severity, category, issue, recommendation, created_at)
		VALUES ('f1', 'rr1', 'find-1', 'high', 'security', 'issue text', 'rec text', '2026-09-19T12:00:00Z');`)
	if err != nil {
		t.Fatalf("insert f1: %v", err)
	}

	_, err = writer.ExecContext(ctx, `INSERT INTO findings (id, role_run_id, finding_id, severity, category, issue, recommendation, created_at)
		VALUES ('f2', 'rr1', 'find-1', 'low', 'security', 'issue 2', 'rec 2', '2026-09-19T12:00:00Z');`)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Errorf("expected unique error on duplicate (role_run_id, finding_id), got: %v", err)
	}

	// 6. Finding basis refs uniqueness: UNIQUE(finding_id, evidence_unit_id) and UNIQUE(finding_id, ordinal)
	_, err = writer.ExecContext(ctx, `INSERT INTO finding_basis_refs (id, finding_id, evidence_unit_id, ordinal)
		VALUES ('fbr1', 'f1', 'eu1', 1);`)
	if err != nil {
		t.Fatalf("insert fbr1: %v", err)
	}

	// Duplicate evidence_unit_id for same finding
	_, err = writer.ExecContext(ctx, `INSERT INTO finding_basis_refs (id, finding_id, evidence_unit_id, ordinal)
		VALUES ('fbr2', 'f1', 'eu1', 2);`)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Errorf("expected unique error on duplicate (finding_id, evidence_unit_id), got: %v", err)
	}

	// Duplicate ordinal for same finding
	// Add a second evidence unit
	_, _ = writer.ExecContext(ctx, `INSERT INTO evidence_units (id, snapshot_id, unit_id, ordinal, kind, text)
		VALUES ('eu2_alt', 'snap1', 'req-alt', 1, 'requirement', 'text alt');`)
	_, err = writer.ExecContext(ctx, `INSERT INTO finding_basis_refs (id, finding_id, evidence_unit_id, ordinal)
		VALUES ('fbr3', 'f1', 'eu2_alt', 1);`)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Errorf("expected unique error on duplicate (finding_id, ordinal), got: %v", err)
	}
}

// TestCoreSchema_CanonicalEnumChecks tests acceptance and rejection of canonical domain enums.
func TestCoreSchema_CanonicalEnumChecks(t *testing.T) {
	_, writer := openMigratedStore(t)
	ctx := context.Background()
	const hash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	_, _ = writer.ExecContext(ctx, `INSERT INTO snapshots (id, hash, project_id, title, content, normalization_version, created_at)
		VALUES ('snap_enum', ?, 'p1', 'Title', 'Content', 1, '2026-09-19T12:00:00Z');`, hash)

	// 1. Evidence unit kind
	validKinds := []string{"brief", "requirement", "component", "flow", "constraint", "data_rule"}
	for i, k := range validKinds {
		_, err := writer.ExecContext(ctx, `INSERT INTO evidence_units (id, snapshot_id, unit_id, ordinal, kind, text)
			VALUES (?, 'snap_enum', ?, ?, ?, 'text');`, fmt.Sprintf("eu_k_%d", i), fmt.Sprintf("u_%d", i), i, k)
		if err != nil {
			t.Errorf("valid kind %q rejected: %v", k, err)
		}
	}
	_, err := writer.ExecContext(ctx, `INSERT INTO evidence_units (id, snapshot_id, unit_id, ordinal, kind, text)
		VALUES ('eu_bad_k', 'snap_enum', 'u_bad', 99, 'invalid_kind', 'text');`)
	if err == nil {
		t.Errorf("expected error on invalid unit kind")
	}

	// 2. Session status
	validStatuses := []string{"queued", "reviewing", "complete", "partial", "failed"}
	for _, st := range validStatuses {
		var q string
		switch st {
		case "queued":
			q = fmt.Sprintf(`INSERT INTO sessions (id, project_id, idempotency_key, request_hash, snapshot_id, status, created_at)
				VALUES ('s_%s', 'p1', 'k_%s', '%s', 'snap_enum', '%s', '2026-09-19T12:00:00Z');`, st, st, hash, st)
		case "reviewing":
			q = fmt.Sprintf(`INSERT INTO sessions (id, project_id, idempotency_key, request_hash, snapshot_id, status, created_at, claimed_at, dispatch_cutoff_at, hard_deadline_at)
				VALUES ('s_%s', 'p1', 'k_%s', '%s', 'snap_enum', '%s', '2026-09-19T12:00:00Z', '2026-09-19T12:01:00Z', '2026-09-19T12:02:00Z', '2026-09-19T12:05:00Z');`, st, st, hash, st)
		case "complete":
			q = fmt.Sprintf(`INSERT INTO sessions (id, project_id, idempotency_key, request_hash, snapshot_id, status, created_at, claimed_at, dispatch_cutoff_at, hard_deadline_at, terminal_at, terminal_reason, completed_role_count, incomplete_role_count)
				VALUES ('s_%s', 'p1', 'k_%s', '%s', 'snap_enum', '%s', '2026-09-19T12:00:00Z', '2026-09-19T12:01:00Z', '2026-09-19T12:02:00Z', '2026-09-19T12:05:00Z', '2026-09-19T12:03:00Z', 'all_roles_complete', 4, 0);`, st, st, hash, st)
		case "partial":
			q = fmt.Sprintf(`INSERT INTO sessions (id, project_id, idempotency_key, request_hash, snapshot_id, status, created_at, claimed_at, dispatch_cutoff_at, hard_deadline_at, terminal_at, terminal_reason, completed_role_count, incomplete_role_count)
				VALUES ('s_%s', 'p1', 'k_%s', '%s', 'snap_enum', '%s', '2026-09-19T12:00:00Z', '2026-09-19T12:01:00Z', '2026-09-19T12:02:00Z', '2026-09-19T12:05:00Z', '2026-09-19T12:03:00Z', 'user_cancelled', 0, 4);`, st, st, hash, st)
		case "failed":
			q = fmt.Sprintf(`INSERT INTO sessions (id, project_id, idempotency_key, request_hash, snapshot_id, status, created_at, claimed_at, dispatch_cutoff_at, hard_deadline_at, terminal_at, terminal_reason, completed_role_count, incomplete_role_count)
				VALUES ('s_%s', 'p1', 'k_%s', '%s', 'snap_enum', '%s', '2026-09-19T12:00:00Z', '2026-09-19T12:01:00Z', '2026-09-19T12:02:00Z', '2026-09-19T12:05:00Z', '2026-09-19T12:03:00Z', 'role_failures', 0, 4);`, st, st, hash, st)
		}
		if _, err := writer.ExecContext(ctx, q); err != nil {
			t.Errorf("valid session status %q rejected: %v", st, err)
		}
	}

	// Invalid session status
	_, err = writer.ExecContext(ctx, fmt.Sprintf(`INSERT INTO sessions (id, project_id, idempotency_key, request_hash, snapshot_id, status, created_at)
		VALUES ('s_bad', 'p1', 'k_bad', '%s', 'snap_enum', 'cancelled', '2026-09-19T12:00:00Z');`, hash))
	if err == nil {
		t.Errorf("expected error on invalid session status 'cancelled'")
	}

	// 3. Roles
	validRoles := []string{"requirements", "architecture", "qa", "security"}
	for _, r := range validRoles {
		_, err := writer.ExecContext(ctx, fmt.Sprintf(`INSERT INTO role_runs (id, session_id, role, status, created_at)
			VALUES ('rr_%s', 's_queued', '%s', 'pending', '2026-09-19T12:00:00Z');`, r, r))
		if err != nil {
			t.Errorf("valid role %q rejected: %v", r, err)
		}
	}

	// Invalid role
	_, err = writer.ExecContext(ctx, `INSERT INTO role_runs (id, session_id, role, status, created_at)
		VALUES ('rr_bad', 's_queued', 'clarity', 'pending', '2026-09-19T12:00:00Z');`)
	if err == nil {
		t.Errorf("expected error on invalid role 'clarity'")
	}

	// 4. Finding severity
	validSeverities := []string{"critical", "high", "medium", "low"}
	for _, sev := range validSeverities {
		_, err := writer.ExecContext(ctx, fmt.Sprintf(`INSERT INTO findings (id, role_run_id, finding_id, severity, category, issue, recommendation, created_at)
			VALUES ('f_%s', 'rr_requirements', 'fid_%s', '%s', 'cat', 'issue', 'rec', '2026-09-19T12:00:00Z');`, sev, sev, sev))
		if err != nil {
			t.Errorf("valid severity %q rejected: %v", sev, err)
		}
	}

	// Invalid severity
	_, err = writer.ExecContext(ctx, `INSERT INTO findings (id, role_run_id, finding_id, severity, category, issue, recommendation, created_at)
		VALUES ('f_bad_sev', 'rr_requirements', 'fid_bad', 'blocker', 'cat', 'issue', 'rec', '2026-09-19T12:00:00Z');`)
	if err == nil {
		t.Errorf("expected error on invalid severity 'blocker'")
	}
}

// TestCoreSchema_CountAndNullabilityChecks tests count constraints and valid/invalid state combinations.
func TestCoreSchema_CountAndNullabilityChecks(t *testing.T) {
	_, writer := openMigratedStore(t)
	ctx := context.Background()
	const hash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	_, _ = writer.ExecContext(ctx, `INSERT INTO snapshots (id, hash, project_id, title, content, normalization_version, created_at)
		VALUES ('snap_cnt', ?, 'p1', 'Title', 'Content', 1, '2026-09-19T12:00:00Z');`, hash)

	// Count sum must equal 4: completed_role_count + incomplete_role_count = 4
	_, err := writer.ExecContext(ctx, fmt.Sprintf(`INSERT INTO sessions (id, project_id, idempotency_key, request_hash, snapshot_id, status, created_at, completed_role_count, incomplete_role_count)
		VALUES ('s_bad_sum', 'p1', 'k_sum', '%s', 'snap_cnt', 'queued', '2026-09-19T12:00:00Z', 1, 1);`, hash))
	if err == nil {
		t.Errorf("expected error when completed_role_count + incomplete_role_count != 4")
	}

	// Completed role count > 4 rejected
	_, err = writer.ExecContext(ctx, fmt.Sprintf(`INSERT INTO sessions (id, project_id, idempotency_key, request_hash, snapshot_id, status, created_at, completed_role_count, incomplete_role_count)
		VALUES ('s_bad_cnt', 'p1', 'k_cnt', '%s', 'snap_cnt', 'complete', '2026-09-19T12:00:00Z', 5, -1);`, hash))
	if err == nil {
		t.Errorf("expected error when completed_role_count > 4")
	}

	// Queued session with claimed_at set rejected
	_, err = writer.ExecContext(ctx, fmt.Sprintf(`INSERT INTO sessions (id, project_id, idempotency_key, request_hash, snapshot_id, status, created_at, claimed_at)
		VALUES ('s_bad_q', 'p1', 'k_q', '%s', 'snap_cnt', 'queued', '2026-09-19T12:00:00Z', '2026-09-19T12:01:00Z');`, hash))
	if err == nil {
		t.Errorf("expected error when queued session has non-null claimed_at")
	}

	// Complete session with completed_role_count < 4 rejected
	_, err = writer.ExecContext(ctx, fmt.Sprintf(`INSERT INTO sessions (id, project_id, idempotency_key, request_hash, snapshot_id, status, created_at, claimed_at, dispatch_cutoff_at, hard_deadline_at, terminal_at, terminal_reason, completed_role_count, incomplete_role_count)
		VALUES ('s_bad_comp', 'p1', 'k_comp', '%s', 'snap_cnt', 'complete', '2026-09-19T12:00:00Z', '2026-09-19T12:01:00Z', '2026-09-19T12:02:00Z', '2026-09-19T12:05:00Z', '2026-09-19T12:03:00Z', 'all_roles_complete', 3, 1);`, hash))
	if err == nil {
		t.Errorf("expected error when complete session has completed_role_count < 4")
	}

	// Failed session with completed_role_count > 0 rejected
	_, err = writer.ExecContext(ctx, fmt.Sprintf(`INSERT INTO sessions (id, project_id, idempotency_key, request_hash, snapshot_id, status, created_at, claimed_at, dispatch_cutoff_at, hard_deadline_at, terminal_at, terminal_reason, completed_role_count, incomplete_role_count)
		VALUES ('s_bad_fail', 'p1', 'k_fail', '%s', 'snap_cnt', 'failed', '2026-09-19T12:00:00Z', '2026-09-19T12:01:00Z', '2026-09-19T12:02:00Z', '2026-09-19T12:05:00Z', '2026-09-19T12:03:00Z', 'role_failures', 1, 3);`, hash))
	if err == nil {
		t.Errorf("expected error when failed session has completed_role_count > 0")
	}
}

// TestCoreSchema_DeleteCascades verifies ON DELETE CASCADE policy.
func TestCoreSchema_DeleteCascades(t *testing.T) {
	_, writer := openMigratedStore(t)
	ctx := context.Background()
	const hash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	// Insert hierarchy: snapshot -> unit, session -> role_run -> finding -> basis_ref
	_, err := writer.ExecContext(ctx, `INSERT INTO snapshots (id, hash, project_id, title, content, normalization_version, created_at)
		VALUES ('snap_del', ?, 'p1', 'Title', 'Content', 1, '2026-09-19T12:00:00Z');`, hash)
	if err != nil {
		t.Fatalf("insert snap: %v", err)
	}

	_, err = writer.ExecContext(ctx, `INSERT INTO evidence_units (id, snapshot_id, unit_id, ordinal, kind, text)
		VALUES ('eu_del', 'snap_del', 'u1', 0, 'brief', 'text');`)
	if err != nil {
		t.Fatalf("insert unit: %v", err)
	}

	_, err = writer.ExecContext(ctx, fmt.Sprintf(`INSERT INTO sessions (id, project_id, idempotency_key, request_hash, snapshot_id, status, created_at)
		VALUES ('sess_del', 'p1', 'k_del', '%s', 'snap_del', 'queued', '2026-09-19T12:00:00Z');`, hash))
	if err != nil {
		t.Fatalf("insert sess: %v", err)
	}

	_, err = writer.ExecContext(ctx, `INSERT INTO role_runs (id, session_id, role, status, created_at)
		VALUES ('rr_del', 'sess_del', 'requirements', 'pending', '2026-09-19T12:00:00Z');`)
	if err != nil {
		t.Fatalf("insert role: %v", err)
	}

	_, err = writer.ExecContext(ctx, `INSERT INTO findings (id, role_run_id, finding_id, severity, category, issue, recommendation, created_at)
		VALUES ('f_del', 'rr_del', 'find-del', 'high', 'qa', 'issue', 'rec', '2026-09-19T12:00:00Z');`)
	if err != nil {
		t.Fatalf("insert finding: %v", err)
	}

	_, err = writer.ExecContext(ctx, `INSERT INTO finding_basis_refs (id, finding_id, evidence_unit_id, ordinal)
		VALUES ('fbr_del', 'f_del', 'eu_del', 1);`)
	if err != nil {
		t.Fatalf("insert basis ref: %v", err)
	}

	// 1. Snapshot deletion blocked while referenced by session (ON DELETE RESTRICT)
	_, err = writer.ExecContext(ctx, "DELETE FROM snapshots WHERE id = 'snap_del';")
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Errorf("expected FK error preventing snapshot deletion when referenced by session, got: %v", err)
	}

	// 2. Deleting session cascades to role_runs, findings, finding_basis_refs
	_, err = writer.ExecContext(ctx, "DELETE FROM sessions WHERE id = 'sess_del';")
	if err != nil {
		t.Fatalf("delete session failed: %v", err)
	}

	var count int
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM role_runs WHERE id = 'rr_del';").Scan(&count)
	if count != 0 {
		t.Errorf("role_run survived session deletion")
	}

	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM findings WHERE id = 'f_del';").Scan(&count)
	if count != 0 {
		t.Errorf("finding survived session deletion")
	}

	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM finding_basis_refs WHERE id = 'fbr_del';").Scan(&count)
	if count != 0 {
		t.Errorf("finding_basis_ref survived finding deletion")
	}

	// 3. Now snapshot can be deleted, cascading to evidence_units
	_, err = writer.ExecContext(ctx, "DELETE FROM snapshots WHERE id = 'snap_del';")
	if err != nil {
		t.Fatalf("delete snapshot failed: %v", err)
	}

	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM evidence_units WHERE id = 'eu_del';").Scan(&count)
	if count != 0 {
		t.Errorf("evidence_unit survived snapshot deletion")
	}
}
