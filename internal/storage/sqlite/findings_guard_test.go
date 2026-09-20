package sqlite

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
)

// 1. Fresh store migrates includes 005_basis_refs_insert_guard.sql (verify schema_migrations count becomes 5 after Migrate()).
func TestFindingsInsertGuard_1_FreshStoreMigrationCount(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	var count int
	err := writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations;").Scan(&count)
	if err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if count != 6 {
		t.Fatalf("expected 6 migrations applied, got %d", count)
	}

	var version int
	var name, checksum string
	err = writer.QueryRowContext(ctx, "SELECT version, name, checksum FROM schema_migrations WHERE version = 5;").Scan(&version, &name, &checksum)
	if err != nil {
		t.Fatalf("query migration 5: %v", err)
	}
	if version != 5 {
		t.Errorf("version = %d, want 5", version)
	}
	if name != "basis_refs_insert_guard" {
		t.Errorf("name = %q, want %q", name, "basis_refs_insert_guard")
	}
	if len(checksum) != 64 {
		t.Errorf("checksum length = %d, want 64", len(checksum))
	}

	var triggerCount int
	err = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name='findings_insert_in_flight_guard';").Scan(&triggerCount)
	if err != nil {
		t.Fatalf("check trigger: %v", err)
	}
	if triggerCount != 1 {
		t.Fatalf("expected findings_insert_in_flight_guard trigger to exist in sqlite_master, got %d", triggerCount)
	}
	_ = store
}

// 2. Direct insert of a finding into a pending role is rejected (error contains "findings may only be inserted while role is in_flight").
func TestFindingsInsertGuard_2_RejectPendingRoleInsert(t *testing.T) {
	_, writer := openGuardTestStore(t)
	ctx := context.Background()

	snapID := "snap_fg_pending"
	unitID := "eu_fg_pending"
	sessID := "sess_fg_pending"
	roleID := "rr_fg_pending"

	insertSnapshotAndUnit(t, writer, snapID, unitID)
	insertSession(t, writer, sessID, snapID, "reviewing")
	insertRoleRun(t, writer, roleID, sessID, "requirements", "pending")

	_, err := writer.ExecContext(ctx, `
		INSERT INTO findings (id, role_run_id, finding_id, severity, category, issue, recommendation, created_at)
		VALUES ('f_pending_1', ?, 'find-1', 'high', 'sec', 'issue', 'rec', '2026-09-19T12:01:00Z');
	`, roleID)
	if err == nil {
		t.Fatalf("expected direct insert into findings for pending role to fail, but succeeded")
	}
	if !strings.Contains(err.Error(), "findings may only be inserted while role is in_flight") {
		t.Fatalf("expected error containing 'findings may only be inserted while role is in_flight', got: %v", err)
	}
}

// 3. Direct insert into complete, failed, and interrupted roles is rejected with the same message.
func TestFindingsInsertGuard_3_RejectTerminalRolesInsert(t *testing.T) {
	_, writer := openGuardTestStore(t)
	ctx := context.Background()

	snapID := "snap_fg_terminal"
	unitID := "eu_fg_terminal"
	sessID := "sess_fg_terminal"

	insertSnapshotAndUnit(t, writer, snapID, unitID)
	insertSession(t, writer, sessID, snapID, "reviewing")

	terminalStatuses := []struct {
		status string
		role   string
		roleID string
	}{
		{status: "complete", role: "requirements", roleID: "rr_fg_complete"},
		{status: "failed", role: "architecture", roleID: "rr_fg_failed"},
		{status: "interrupted", role: "qa", roleID: "rr_fg_interrupted"},
	}

	for _, tc := range terminalStatuses {
		t.Run(tc.status, func(t *testing.T) {
			insertRoleRun(t, writer, tc.roleID, sessID, tc.role, tc.status)

			findingPK := fmt.Sprintf("f_%s", tc.status)
			_, err := writer.ExecContext(ctx, `
				INSERT INTO findings (id, role_run_id, finding_id, severity, category, issue, recommendation, created_at)
				VALUES (?, ?, 'find-1', 'high', 'sec', 'issue', 'rec', '2026-09-19T12:02:00Z');
			`, findingPK, tc.roleID)
			if err == nil {
				t.Fatalf("expected direct insert into findings for %s role to fail, but succeeded", tc.status)
			}
			if !strings.Contains(err.Error(), "findings may only be inserted while role is in_flight") {
				t.Fatalf("expected error containing 'findings may only be inserted while role is in_flight' for %s role, got: %v", tc.status, err)
			}
		})
	}
}

// 4. Direct insert into in_flight role succeeds and the row is readable (count = 1).
func TestFindingsInsertGuard_4_AllowInFlightRoleInsert(t *testing.T) {
	_, writer := openGuardTestStore(t)
	ctx := context.Background()

	snapID := "snap_fg_inflight"
	unitID := "eu_fg_inflight"
	sessID := "sess_fg_inflight"
	roleID := "rr_fg_inflight"

	insertSnapshotAndUnit(t, writer, snapID, unitID)
	insertSession(t, writer, sessID, snapID, "reviewing")
	insertRoleRun(t, writer, roleID, sessID, "requirements", "in_flight")

	_, err := writer.ExecContext(ctx, `
		INSERT INTO findings (id, role_run_id, finding_id, severity, category, issue, recommendation, created_at)
		VALUES ('f_inflight_1', ?, 'find-1', 'high', 'sec', 'Valid issue', 'Valid rec', '2026-09-19T12:01:00Z');
	`, roleID)
	if err != nil {
		t.Fatalf("direct insert into findings for in_flight role failed: %v", err)
	}

	var count int
	if err := writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM findings WHERE id = 'f_inflight_1';").Scan(&count); err != nil {
		t.Fatalf("count findings: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected findings count = 1, got %d", count)
	}

	var findingID, severity, category, issue, rec string
	err = writer.QueryRowContext(ctx, `
		SELECT finding_id, severity, category, issue, recommendation
		FROM findings WHERE id = 'f_inflight_1';
	`).Scan(&findingID, &severity, &category, &issue, &rec)
	if err != nil {
		t.Fatalf("scan finding row: %v", err)
	}
	if findingID != "find-1" || severity != "high" || category != "sec" || issue != "Valid issue" || rec != "Valid rec" {
		t.Fatalf("scanned finding row mismatch: %s, %s, %s, %s, %s", findingID, severity, category, issue, rec)
	}
}

// 5. PublishRoleSuccess for an in_flight role succeeds end-to-end and persists findings.
func TestFindingsInsertGuard_5_PublishRoleSuccessEndToEnd(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_fg_pub_e2e")
	submitRes, err := store.Submit(ctx, SubmitParams{
		ProjectID:      "proj_fg_pub",
		IdempotencyKey: "idem_fg_pub",
		Title:          "Publish Guard Test",
		Content:        "Content for publish guard",
		Snapshot:       snap,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	sessID := submitRes.SessionID

	claimPolicy := TimingPolicy{
		DispatchCutoff:      5 * time.Minute,
		CallTimeout:         1 * time.Minute,
		SessionHardDeadline: 10 * time.Minute,
	}
	claimRes, err := store.ClaimSession(ctx, claimPolicy)
	if err != nil {
		t.Fatalf("ClaimSession: %v", err)
	}
	if !claimRes.Claimed || claimRes.SessionID != sessID {
		t.Fatalf("expected session %s to be claimed, got %+v", sessID, claimRes)
	}

	dispatchRes, err := store.ReservePendingRole(ctx, sessID)
	if err != nil {
		t.Fatalf("ReservePendingRole: %v", err)
	}
	if !dispatchRes.Reserved {
		t.Fatalf("expected role reserved, got %+v", dispatchRes)
	}
	roleRunID := dispatchRes.RoleRunID

	pubRes, err := store.PublishRoleSuccess(ctx, PublishSuccessParams{
		SessionID: sessID,
		RoleRunID: roleRunID,
		CallCount: 1,
		Findings: []domain.Finding{
			{
				ID:             "find-e2e-1",
				Severity:       domain.SeverityHigh,
				Category:       "security",
				Issue:          "Potential injection",
				Recommendation: "Sanitize inputs",
				BasisRefs:      []string{snap.Units[0].ID},
			},
		},
	})
	if err != nil {
		t.Fatalf("PublishRoleSuccess failed: %v", err)
	}
	if pubRes.Status != domain.RoleComplete {
		t.Errorf("PublishRoleSuccess status = %v, want complete", pubRes.Status)
	}
	if pubRes.FindingCount != 1 {
		t.Errorf("PublishRoleSuccess FindingCount = %d, want 1", pubRes.FindingCount)
	}

	// Verify role_runs status is complete
	var roleStatus string
	if err := writer.QueryRowContext(ctx, "SELECT status FROM role_runs WHERE id = ?;", roleRunID).Scan(&roleStatus); err != nil {
		t.Fatalf("query role status: %v", err)
	}
	if roleStatus != "complete" {
		t.Errorf("role_runs status = %q, want complete", roleStatus)
	}

	// Verify findings row exists
	var fCount int
	if err := writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM findings WHERE role_run_id = ?;", roleRunID).Scan(&fCount); err != nil {
		t.Fatalf("count findings: %v", err)
	}
	if fCount != 1 {
		t.Errorf("findings count = %d, want 1", fCount)
	}

	// Verify finding basis refs row exists
	var bCount int
	if err := writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM finding_basis_refs WHERE finding_id = ?;", fmt.Sprintf("%s:find-e2e-1", roleRunID)).Scan(&bCount); err != nil {
		t.Fatalf("count finding_basis_refs: %v", err)
	}
	if bCount != 1 {
		t.Errorf("basis refs count = %d, want 1", bCount)
	}
}

// 6. Migrate() idempotency: second run is a no-op and does not change applied_at checksums.
func TestFindingsInsertGuard_6_MigrateIdempotency(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}

	writer, err := store.writerDB()
	if err != nil {
		t.Fatalf("writerDB: %v", err)
	}

	rowsBefore := queryAppliedMigrations(t, writer)
	if len(rowsBefore) != 6 {
		t.Fatalf("expected 6 applied migrations, got %d", len(rowsBefore))
	}

	// Re-run Migrate on same store
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	rowsAfter := queryAppliedMigrations(t, writer)
	if len(rowsAfter) != 6 {
		t.Fatalf("expected 6 applied migrations after second migrate, got %d", len(rowsAfter))
	}

	for i := range rowsBefore {
		if rowsBefore[i].Version != rowsAfter[i].Version {
			t.Errorf("migration %d version mismatch: %d vs %d", i, rowsBefore[i].Version, rowsAfter[i].Version)
		}
		if rowsBefore[i].Name != rowsAfter[i].Name {
			t.Errorf("migration %d name mismatch: %s vs %s", i, rowsBefore[i].Name, rowsAfter[i].Name)
		}
		if rowsBefore[i].Checksum != rowsAfter[i].Checksum {
			t.Errorf("migration %d checksum mismatch: %s vs %s", i, rowsBefore[i].Checksum, rowsAfter[i].Checksum)
		}
		if rowsBefore[i].AppliedAt != rowsAfter[i].AppliedAt {
			t.Errorf("migration %d applied_at changed on second migrate: %s -> %s", i, rowsBefore[i].AppliedAt, rowsAfter[i].AppliedAt)
		}
	}
}

// 7. Verification that the guard coexists with existing P3 guards (existing state_guard_test.go UPDATE/DELETE checks still pass).
func TestFindingsInsertGuard_7_CoexistWithStateGuards(t *testing.T) {
	_, writer := openGuardTestStore(t)
	ctx := context.Background()

	snapID := "snap_fg_coexist"
	unitID := "eu_fg_coexist"
	sessID := "sess_fg_coexist"
	roleID := "rr_fg_coexist"
	findingID := "f_fg_coexist"

	insertSnapshotAndUnit(t, writer, snapID, unitID)
	insertSession(t, writer, sessID, snapID, "reviewing")
	insertRoleRun(t, writer, roleID, sessID, "requirements", "in_flight")

	// Insert a finding into the in_flight role
	_, err := writer.ExecContext(ctx, `
		INSERT INTO findings (id, role_run_id, finding_id, severity, category, issue, recommendation, created_at)
		VALUES (?, ?, 'find-1', 'medium', 'arch', 'Coexist issue', 'Coexist rec', '2026-09-19T12:01:00Z');
	`, findingID, roleID)
	if err != nil {
		t.Fatalf("insert finding: %v", err)
	}

	// Verify UPDATE on finding is rejected by P3 immutability guard
	_, err = writer.ExecContext(ctx, "UPDATE findings SET issue = 'tampered' WHERE id = ?;", findingID)
	if err == nil {
		t.Fatalf("expected UPDATE on findings to fail, but succeeded")
	}
	if !strings.Contains(err.Error(), "findings are immutable") {
		t.Errorf("expected error to contain 'findings are immutable', got: %v", err)
	}

	// Verify DELETE on finding is rejected by P3 immutability guard
	_, err = writer.ExecContext(ctx, "DELETE FROM findings WHERE id = ?;", findingID)
	if err == nil {
		t.Fatalf("expected DELETE on findings to fail, but succeeded")
	}
	if !strings.Contains(err.Error(), "findings are immutable") {
		t.Errorf("expected error to contain 'findings are immutable', got: %v", err)
	}

	// Verify snapshot immutability still holds
	_, err = writer.ExecContext(ctx, "UPDATE snapshots SET title = 'tampered' WHERE id = ?;", snapID)
	if err == nil {
		t.Fatalf("expected UPDATE on snapshots to fail, but succeeded")
	}
	if !strings.Contains(err.Error(), "snapshots are immutable") {
		t.Errorf("expected error to contain 'snapshots are immutable', got: %v", err)
	}
}

func TestFindingsInsertGuard_8_RejectBasisRefAfterRoleTerminal(t *testing.T) {
	_, writer := openGuardTestStore(t)
	ctx := context.Background()

	insertSnapshotAndUnit(t, writer, "snap_basis_guard", "eu_basis_guard")
	insertSession(t, writer, "sess_basis_guard", "snap_basis_guard", "reviewing")
	insertRoleRun(t, writer, "rr_basis_guard", "sess_basis_guard", "requirements", "in_flight")

	_, err := writer.ExecContext(ctx, `
		INSERT INTO findings (id, role_run_id, finding_id, severity, category, issue, recommendation, created_at)
		VALUES ('f_basis_guard', 'rr_basis_guard', 'find-1', 'high', 'sec', 'issue', 'rec', '2026-09-19T12:01:00Z');
	`)
	if err != nil {
		t.Fatalf("insert finding: %v", err)
	}

	_, err = writer.ExecContext(ctx, `
		INSERT INTO finding_basis_refs (id, finding_id, evidence_unit_id, ordinal)
		VALUES ('ref_basis_guard_1', 'f_basis_guard', 'eu_basis_guard', 1);
	`)
	if err != nil {
		t.Fatalf("insert basis ref while in_flight: %v", err)
	}

	if _, err = writer.ExecContext(ctx, `
		UPDATE role_runs
		SET status = 'complete', completed_at = '2026-09-19T12:02:00Z', call_count = 1
		WHERE id = 'rr_basis_guard';
	`); err != nil {
		t.Fatalf("transition role to complete: %v", err)
	}

	_, err = writer.ExecContext(ctx, `
		INSERT INTO finding_basis_refs (id, finding_id, evidence_unit_id, ordinal)
		VALUES ('ref_basis_guard_2', 'f_basis_guard', 'eu_basis_guard', 2);
	`)
	if err == nil {
		t.Fatal("expected basis-ref insert after terminal transition to fail")
	}
	if !strings.Contains(err.Error(), "finding basis references may only be inserted while role is in_flight") {
		t.Fatalf("unexpected basis-ref guard error: %v", err)
	}
}
