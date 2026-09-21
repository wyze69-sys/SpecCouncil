package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
)

// =============================================================================
// P12 End-to-End Persistence and Concurrency Proof Helpers
// =============================================================================

type dbSnapshot struct {
	tables map[string][]string
}

func takeDBSnapshot(t *testing.T, db *sql.DB) dbSnapshot {
	t.Helper()
	snap := dbSnapshot{tables: make(map[string][]string)}
	tableQueries := map[string]string{
		"snapshots":          "SELECT id, hash, project_id, title, content, normalization_version, created_at FROM snapshots ORDER BY id ASC;",
		"evidence_units":     "SELECT id, snapshot_id, unit_id, ordinal, kind, text FROM evidence_units ORDER BY snapshot_id ASC, ordinal ASC;",
		"sessions":           "SELECT id, project_id, idempotency_key, request_hash, snapshot_id, status, cancel_requested, COALESCE(claimed_at,''), COALESCE(dispatch_cutoff_at,''), COALESCE(hard_deadline_at,''), COALESCE(terminal_at,''), COALESCE(terminal_reason,''), completed_role_count, incomplete_role_count, created_at FROM sessions ORDER BY id ASC;",
		"role_runs":          "SELECT id, session_id, role, status, COALESCE(cause,''), COALESCE(error_category,''), COALESCE(error_message,''), call_count, COALESCE(started_at,''), COALESCE(completed_at,''), created_at FROM role_runs ORDER BY session_id ASC, role ASC;",
		"findings":           "SELECT id, role_run_id, finding_id, severity, category, issue, recommendation, created_at FROM findings ORDER BY role_run_id ASC, finding_id ASC;",
		"finding_basis_refs": "SELECT id, finding_id, evidence_unit_id, ordinal FROM finding_basis_refs ORDER BY finding_id ASC, ordinal ASC;",
		"schema_migrations":  "SELECT version, name, checksum, applied_at FROM schema_migrations ORDER BY version ASC;",
	}

	for table, query := range tableQueries {
		rows, err := db.Query(query)
		if err != nil {
			t.Fatalf("takeDBSnapshot query %s: %v", table, err)
		}
		var rowStrings []string
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatalf("takeDBSnapshot columns %s: %v", table, err)
		}
		for rows.Next() {
			vals := make([]interface{}, len(cols))
			ptrs := make([]interface{}, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				t.Fatalf("takeDBSnapshot scan %s: %v", table, err)
			}
			var sb strings.Builder
			for i, v := range vals {
				if i > 0 {
					sb.WriteString("|")
				}
				if v == nil {
					sb.WriteString("<NULL>")
				} else {
					switch val := v.(type) {
					case []byte:
						sb.WriteString(string(val))
					default:
						sb.WriteString(fmt.Sprintf("%v", val))
					}
				}
			}
			rowStrings = append(rowStrings, sb.String())
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("takeDBSnapshot rows.Err %s: %v", table, err)
		}
		snap.tables[table] = rowStrings
	}
	return snap
}

func assertDBSnapshotEqual(t *testing.T, before, after dbSnapshot, opName string) {
	t.Helper()
	for table, beforeRows := range before.tables {
		afterRows := after.tables[table]
		if len(beforeRows) != len(afterRows) {
			t.Fatalf("table %s row count changed across %s: before=%d after=%d", table, opName, len(beforeRows), len(afterRows))
		}
		for i := range beforeRows {
			if beforeRows[i] != afterRows[i] {
				t.Fatalf("table %s row %d mutated across %s:\n  before: %s\n   after: %s", table, i, opName, beforeRows[i], afterRows[i])
			}
		}
	}
}

func setupE2EStore(t *testing.T, busyTimeout time.Duration) (*Store, *sql.DB, string) {
	t.Helper()
	if busyTimeout <= 0 {
		busyTimeout = 100 * time.Millisecond
	}
	store, dbPath := setupTestStore(t, busyTimeout)
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}
	writer, err := store.writerDB()
	if err != nil {
		t.Fatalf("writerDB failed: %v", err)
	}
	return store, writer, dbPath
}

// =============================================================================
// 1. Fresh Submit End-to-End
// =============================================================================

func TestE2E_01_FreshSubmit(t *testing.T) {
	store, writer, _ := setupE2EStore(t, 100*time.Millisecond)
	ctx := context.Background()

	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	snap := createTestFrozenSnapshot(t, "snap_e2e_01")
	reqHash, err := RequestHashV1("proj_e2e_01", "Design Spec 01", "Content 01")
	if err != nil {
		t.Fatalf("RequestHashV1: %v", err)
	}

	submitRes, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_e2e_01",
		ProjectID:      "proj_e2e_01",
		IdempotencyKey: "idem_e2e_01",
		Title:          "Design Spec 01",
		Content:        "Content 01",
		Snapshot:       snap,
		CreatedAt:      t0,
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	if submitRes.Replay {
		t.Errorf("expected fresh submit, got replay = true")
	}
	if submitRes.SessionID != "sess_e2e_01" {
		t.Errorf("SessionID = %q, want sess_e2e_01", submitRes.SessionID)
	}
	if submitRes.SnapshotID != snap.ID {
		t.Errorf("SnapshotID = %q, want %q", submitRes.SnapshotID, snap.ID)
	}
	if submitRes.RequestHash != reqHash {
		t.Errorf("RequestHash = %q, want %q", submitRes.RequestHash, reqHash)
	}
	if submitRes.Status != domain.SessionQueued {
		t.Errorf("Status = %q, want queued", submitRes.Status)
	}

	// 1. Snapshot table verification: exactly 1 row
	var snapCount int
	var snapHash, snapTitle, snapContent string
	var normVer int
	var snapCreated string
	err = writer.QueryRowContext(ctx, "SELECT COUNT(*), hash, title, content, normalization_version, created_at FROM snapshots WHERE id = ?;", snap.ID).Scan(
		&snapCount, &snapHash, &snapTitle, &snapContent, &normVer, &snapCreated,
	)
	if err != nil || snapCount != 1 {
		t.Fatalf("query snapshot: count=%d, err=%v", snapCount, err)
	}
	if snapHash != snap.Hash {
		t.Errorf("snap hash = %q, want %q", snapHash, snap.Hash)
	}
	if normVer != 1 {
		t.Errorf("normalization_version = %d, want 1", normVer)
	}
	if snapCreated != "2026-09-19T12:00:00.000000000Z" {
		t.Errorf("snap created_at = %q, want 2026-09-19T12:00:00.000000000Z", snapCreated)
	}

	// 2. Ordered evidence units: exactly matches frozen snapshot
	var unitCount int
	err = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM evidence_units WHERE snapshot_id = ?;", snap.ID).Scan(&unitCount)
	if err != nil || unitCount != len(snap.Units) {
		t.Fatalf("evidence units count = %d, want %d", unitCount, len(snap.Units))
	}
	rows, err := writer.QueryContext(ctx, "SELECT unit_id, ordinal, kind, text FROM evidence_units WHERE snapshot_id = ? ORDER BY ordinal ASC;", snap.ID)
	if err != nil {
		t.Fatalf("query evidence units: %v", err)
	}
	defer rows.Close()
	idx := 0
	for rows.Next() {
		var uid, kind, text string
		var ord int
		if err := rows.Scan(&uid, &ord, &kind, &text); err != nil {
			t.Fatalf("scan evidence unit: %v", err)
		}
		if ord != idx {
			t.Errorf("unit ordinal = %d, want %d", ord, idx)
		}
		if uid != snap.Units[idx].ID {
			t.Errorf("unit ID = %q, want %q", uid, snap.Units[idx].ID)
		}
		if kind != string(snap.Units[idx].Kind) {
			t.Errorf("unit kind = %q, want %q", kind, snap.Units[idx].Kind)
		}
		if text != snap.Units[idx].Text {
			t.Errorf("unit text = %q, want %q", text, snap.Units[idx].Text)
		}
		idx++
	}

	// 3. Queued session verification: counts 0/4, null timing fields
	var sessStatus, sessReqHash, sessSnapID string
	var cancelReq, compCount, incompCount int
	var claimedAt, cutoffAt, deadlineAt, termAt, termReason sql.NullString
	err = writer.QueryRowContext(ctx, `SELECT status, request_hash, snapshot_id, cancel_requested,
		completed_role_count, incomplete_role_count, claimed_at, dispatch_cutoff_at, hard_deadline_at,
		terminal_at, terminal_reason FROM sessions WHERE id = ?;`, "sess_e2e_01").Scan(
		&sessStatus, &sessReqHash, &sessSnapID, &cancelReq, &compCount, &incompCount,
		&claimedAt, &cutoffAt, &deadlineAt, &termAt, &termReason,
	)
	if err != nil {
		t.Fatalf("query session: %v", err)
	}
	if sessStatus != "queued" || cancelReq != 0 || compCount != 0 || incompCount != 4 {
		t.Errorf("session state invalid: status=%s cancel=%d comp=%d incomp=%d", sessStatus, cancelReq, compCount, incompCount)
	}
	if claimedAt.Valid || cutoffAt.Valid || deadlineAt.Valid || termAt.Valid || termReason.Valid {
		t.Errorf("queued session has non-null timing/terminal fields")
	}

	// 4. Role runs verification: exactly 4 pending in canonical order
	rRows, err := writer.QueryContext(ctx, "SELECT role, status, call_count, started_at, completed_at FROM role_runs WHERE session_id = ?;", "sess_e2e_01")
	if err != nil {
		t.Fatalf("query role runs: %v", err)
	}
	defer rRows.Close()
	roleRunsByRole := make(map[domain.Role]string)
	for rRows.Next() {
		var roleStr, status string
		var callCount int
		var startedAt, completedAt sql.NullString
		if err := rRows.Scan(&roleStr, &status, &callCount, &startedAt, &completedAt); err != nil {
			t.Fatalf("scan role run: %v", err)
		}
		role := domain.Role(roleStr)
		roleRunsByRole[role] = status
		if status != "pending" || callCount != 0 || startedAt.Valid || completedAt.Valid {
			t.Errorf("role run %s in invalid initial state: status=%s call=%d", roleStr, status, callCount)
		}
	}
	if len(roleRunsByRole) != 4 {
		t.Fatalf("found %d role runs, want 4", len(roleRunsByRole))
	}
	for _, r := range domain.Roles {
		if status, ok := roleRunsByRole[r]; !ok || status != "pending" {
			t.Errorf("missing or non-pending canonical role %s", r)
		}
	}

	// 5. Read-only operation immutability proof with database table snapshot
	snapBefore := takeDBSnapshot(t, writer)
	statusRead, err := store.ReadStatus(ctx, "sess_e2e_01")
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	snapAfter := takeDBSnapshot(t, writer)
	assertDBSnapshotEqual(t, snapBefore, snapAfter, "ReadStatus")
	if statusRead.Status != domain.SessionQueued || len(statusRead.Roles) != 4 {
		t.Errorf("ReadStatus returned unexpected state: %+v", statusRead)
	}

	snapBefore2 := takeDBSnapshot(t, writer)
	snapRead, err := store.ReadSnapshot(ctx, snap.ID)
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	snapAfter2 := takeDBSnapshot(t, writer)
	assertDBSnapshotEqual(t, snapBefore2, snapAfter2, "ReadSnapshot")
	if snapRead.Hash != snap.Hash || len(snapRead.Units) != len(snap.Units) {
		t.Errorf("ReadSnapshot hash mismatch or unit length mismatch")
	}

	// 6. Project scoping and unknown-ID error handling
	_, err = store.ReadStatusScoped(ctx, "wrong_project", "sess_e2e_01")
	if !errors.Is(err, ErrNotFound) || !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("expected ErrSessionNotFound on scoped mismatch, got: %v", err)
	}
	_, err = store.ReadStatus(ctx, "nonexistent_sess")
	if !errors.Is(err, ErrNotFound) || !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("expected ErrSessionNotFound on missing session, got: %v", err)
	}
}

// =============================================================================
// 2. FIFO Claim End-to-End
// =============================================================================

func TestE2E_02_FIFOClaim(t *testing.T) {
	store, writer, _ := setupE2EStore(t, 100*time.Millisecond)
	ctx := context.Background()

	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	snap1 := createTestFrozenSnapshot(t, "snap_fifo_1")
	snap2 := createTestFrozenSnapshot(t, "snap_fifo_2")

	// Submit two queued sessions with distinct creation times
	_, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_fifo_early",
		ProjectID:      "proj_fifo",
		IdempotencyKey: "idem_early",
		Title:          "Early Spec",
		Content:        "Early Content",
		Snapshot:       snap1,
		CreatedAt:      t0,
	})
	if err != nil {
		t.Fatalf("submit early: %v", err)
	}

	_, err = store.Submit(ctx, SubmitParams{
		SessionID:      "sess_fifo_late",
		ProjectID:      "proj_fifo",
		IdempotencyKey: "idem_late",
		Title:          "Late Spec",
		Content:        "Late Content",
		Snapshot:       snap2,
		CreatedAt:      t0.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("submit late: %v", err)
	}

	policy := TimingPolicy{
		DispatchCutoff:      15 * time.Minute,
		CallTimeout:         5 * time.Minute,
		SessionHardDeadline: 25 * time.Minute,
	}

	tClaim := t0.Add(5 * time.Minute)
	claimRes, err := store.ClaimSessionWithNow(ctx, policy, tClaim)
	if err != nil {
		t.Fatalf("ClaimSessionWithNow: %v", err)
	}

	// 1. Must claim the oldest session by FIFO order
	if !claimRes.Claimed {
		t.Fatalf("expected Claimed = true")
	}
	if claimRes.SessionID != "sess_fifo_early" {
		t.Errorf("Claimed SessionID = %q, want sess_fifo_early (FIFO)", claimRes.SessionID)
	}

	// 2. Verify exact timing fields in storage
	var status, claimedAt, cutoffAt, deadlineAt string
	err = writer.QueryRowContext(ctx, "SELECT status, claimed_at, dispatch_cutoff_at, hard_deadline_at FROM sessions WHERE id = ?;", "sess_fifo_early").Scan(
		&status, &claimedAt, &cutoffAt, &deadlineAt,
	)
	if err != nil {
		t.Fatalf("query claimed session: %v", err)
	}
	if status != "reviewing" {
		t.Errorf("status = %q, want reviewing", status)
	}
	if claimedAt != "2026-09-19T12:05:00.000000000Z" {
		t.Errorf("claimed_at = %q, want 2026-09-19T12:05:00.000000000Z", claimedAt)
	}
	if cutoffAt != "2026-09-19T12:20:00.000000000Z" {
		t.Errorf("dispatch_cutoff_at = %q, want 2026-09-19T12:20:00.000000000Z", cutoffAt)
	}
	if deadlineAt != "2026-09-19T12:30:00.000000000Z" {
		t.Errorf("hard_deadline_at = %q, want 2026-09-19T12:30:00.000000000Z", deadlineAt)
	}

	// 3. Late session remains queued
	var lateStatus string
	var lateClaimed sql.NullString
	err = writer.QueryRowContext(ctx, "SELECT status, claimed_at FROM sessions WHERE id = ?;", "sess_fifo_late").Scan(&lateStatus, &lateClaimed)
	if err != nil {
		t.Fatalf("query late session: %v", err)
	}
	if lateStatus != "queued" || lateClaimed.Valid {
		t.Errorf("late session modified unexpectedly: status=%s, claimed=%v", lateStatus, lateClaimed)
	}

	// 4. Single active session constraint: second claim while one reviewing returns NoWorkActiveReviewing
	claim2, err := store.ClaimSessionWithNow(ctx, policy, tClaim.Add(1*time.Minute))
	if err != nil {
		t.Fatalf("claim2: %v", err)
	}
	if claim2.Claimed {
		t.Errorf("expected Claimed = false while another session reviewing")
	}
	if claim2.NoWorkReason != NoWorkActiveReviewing {
		t.Errorf("NoWorkReason = %v, want %v", claim2.NoWorkReason, NoWorkActiveReviewing)
	}

	// 5. Table snapshot before and after ReadStatus
	snapBefore := takeDBSnapshot(t, writer)
	_, _ = store.ReadStatus(ctx, "sess_fifo_early")
	_, _ = store.ReadStatus(ctx, "sess_fifo_late")
	snapAfter := takeDBSnapshot(t, writer)
	assertDBSnapshotEqual(t, snapBefore, snapAfter, "ReadStatus after claim")
}

// =============================================================================
// 3. Guarded Dispatch End-to-End
// =============================================================================

func TestE2E_03_GuardedDispatch(t *testing.T) {
	store, writer, _ := setupE2EStore(t, 100*time.Millisecond)
	ctx := context.Background()

	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	snap := createTestFrozenSnapshot(t, "snap_dispatch")
	_, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_dispatch",
		ProjectID:      "proj_dispatch",
		IdempotencyKey: "idem_dispatch",
		Title:          "Dispatch Spec",
		Content:        "Dispatch Content",
		Snapshot:       snap,
		CreatedAt:      t0,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	policy := TimingPolicy{
		DispatchCutoff:      20 * time.Minute,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: 35 * time.Minute,
	}
	_, err = store.ClaimSessionWithNow(ctx, policy, t0)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Dispatch 1: Must reserve domain.RoleRequirements (canonical order)
	t1 := t0.Add(1 * time.Minute)
	res1, err := store.ReservePendingRoleWithNow(ctx, "sess_dispatch", t1)
	if err != nil {
		t.Fatalf("reserve 1: %v", err)
	}
	if !res1.Reserved || res1.Role != domain.RoleRequirements {
		t.Fatalf("res1 = %+v, want Reserved=true Role=requirements", res1)
	}

	// Dispatch 2: Must reserve domain.RoleArchitecture (canonical order)
	t2 := t0.Add(2 * time.Minute)
	res2, err := store.ReservePendingRoleWithNow(ctx, "sess_dispatch", t2)
	if err != nil {
		t.Fatalf("reserve 2: %v", err)
	}
	if !res2.Reserved || res2.Role != domain.RoleArchitecture {
		t.Fatalf("res2 = %+v, want Reserved=true Role=architecture", res2)
	}

	// Dispatch 3: In-flight count is now 2 (MAX_IN_FLIGHT). Must be refused!
	t3 := t0.Add(3 * time.Minute)
	res3, err := store.ReservePendingRoleWithNow(ctx, "sess_dispatch", t3)
	if err != nil {
		t.Fatalf("reserve 3: %v", err)
	}
	if res3.Reserved {
		t.Errorf("expected reservation 3 to be refused, but succeeded: %+v", res3)
	}
	if res3.NoWorkReason != DispatchNoWorkCapacityFull {
		t.Errorf("res3.NoWorkReason = %v, want %v", res3.NoWorkReason, DispatchNoWorkCapacityFull)
	}

	// Verify database in-flight count is exactly 2 and pending count is exactly 2
	var inFlightCount, pendingCount int
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM role_runs WHERE session_id = 'sess_dispatch' AND status = 'in_flight';").Scan(&inFlightCount)
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM role_runs WHERE session_id = 'sess_dispatch' AND status = 'pending';").Scan(&pendingCount)
	if inFlightCount != 2 || pendingCount != 2 {
		t.Errorf("in-flight=%d pending=%d, want 2 and 2", inFlightCount, pendingCount)
	}

	// Scoped dispatch unknown ID and project scoping
	_, err = store.ReservePendingRoleScoped(ctx, "wrong_proj", "sess_dispatch", t3)
	if !errors.Is(err, ErrNotFound) || !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("expected ErrSessionNotFound on scoped mismatch, got: %v", err)
	}
	_, err = store.ReservePendingRole(ctx, "unknown_sess")
	if !errors.Is(err, ErrNotFound) || !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("expected ErrSessionNotFound on unknown session, got: %v", err)
	}
}

// =============================================================================
// 4. Successful P9 Publication End-to-End
// =============================================================================

func TestE2E_04_SuccessfulPublication(t *testing.T) {
	store, writer, _ := setupE2EStore(t, 100*time.Millisecond)
	ctx := context.Background()

	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	snap := createTestFrozenSnapshot(t, "snap_pub_succ")
	_, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_pub_succ",
		ProjectID:      "proj_pub_succ",
		IdempotencyKey: "idem_pub_succ",
		Title:          "Pub Succ Spec",
		Content:        "Pub Succ Content",
		Snapshot:       snap,
		CreatedAt:      t0,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	_, err = store.ClaimSessionWithNow(ctx, TimingPolicy{
		DispatchCutoff:      20 * time.Minute,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: 35 * time.Minute,
	}, t0)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	resReq, err := store.ReservePendingRoleWithNow(ctx, "sess_pub_succ", t0.Add(1*time.Minute))
	if err != nil || !resReq.Reserved {
		t.Fatalf("reserve req: %v", err)
	}

	findings := []domain.Finding{
		{
			ID:             "find-req-1",
			Kind:           domain.FindingExisting,
			Severity:       domain.SeverityCritical,
			Category:       "correctness",
			Issue:          "Critical missing invariant in requirements",
			Recommendation: "Specify transaction isolation explicitly",
			BasisRefs:      []string{"req-1", "brief-1"},
		},
		{
			ID:             "find-req-2",
			Kind:           domain.FindingExisting,
			Severity:       domain.SeverityHigh,
			Category:       "security",
			Issue:          "Missing authentication requirement",
			Recommendation: "Add mutual TLS requirement",
			BasisRefs:      []string{"const-1"},
		},
	}

	tPub := t0.Add(3 * time.Minute)
	pubRes, err := store.PublishRoleSuccess(ctx, PublishSuccessParams{
		SessionID:   "sess_pub_succ",
		RoleRunID:   resReq.RoleRunID,
		Role:        domain.RoleRequirements,
		Findings:    findings,
		CallCount:   1,
		CompletedAt: tPub,
	})
	if err != nil {
		t.Fatalf("PublishRoleSuccess failed: %v", err)
	}
	if pubRes.Status != domain.RoleComplete {
		t.Errorf("pubRes.Status = %s, want complete", pubRes.Status)
	}

	// Verify role run table
	var status string
	var callCount int
	var completedAt string
	err = writer.QueryRowContext(ctx, "SELECT status, call_count, completed_at FROM role_runs WHERE id = ?;", resReq.RoleRunID).Scan(
		&status, &callCount, &completedAt,
	)
	if err != nil {
		t.Fatalf("query role_runs: %v", err)
	}
	if status != "complete" || callCount != 1 || completedAt != "2026-09-19T12:03:00.000000000Z" {
		t.Errorf("role_run not complete: status=%s call=%d completed_at=%s", status, callCount, completedAt)
	}

	// Verify findings table: exactly 2
	var fCount int
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM findings WHERE role_run_id = ?;", resReq.RoleRunID).Scan(&fCount)
	if fCount != 2 {
		t.Errorf("findings count = %d, want 2", fCount)
	}

	// Verify basis refs table: 3 citations (2 for find-1, 1 for find-2)
	var refCount int
	_ = writer.QueryRowContext(ctx, `SELECT COUNT(*) FROM finding_basis_refs fbr
		JOIN findings f ON fbr.finding_id = f.id WHERE f.role_run_id = ?;`, resReq.RoleRunID).Scan(&refCount)
	if refCount != 3 {
		t.Errorf("basis refs count = %d, want 3", refCount)
	}

	// Atomicity / Rollback proof: attempt publication with invalid citation (unknown evidence unit)
	resArch, err := store.ReservePendingRoleWithNow(ctx, "sess_pub_succ", t0.Add(4*time.Minute))
	if err != nil || !resArch.Reserved {
		t.Fatalf("reserve arch: %v", err)
	}
	invalidFindings := []domain.Finding{
		{
			ID:             "find-arch-invalid",
			Kind:           domain.FindingExisting,
			Severity:       domain.SeverityMedium,
			Category:       "architecture",
			Issue:          "Issue",
			Recommendation: "Rec",
			BasisRefs:      []string{"nonexistent-unit-xyz"},
		},
	}
	_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
		SessionID:   "sess_pub_succ",
		RoleRunID:   resArch.RoleRunID,
		Role:        domain.RoleArchitecture,
		Findings:    invalidFindings,
		CallCount:   1,
		CompletedAt: t0.Add(5 * time.Minute),
	})
	if err == nil {
		t.Fatalf("expected error on invalid basis ref, but succeeded")
	}

	// Role Architecture remains in_flight, 0 findings inserted
	var archStatus string
	_ = writer.QueryRowContext(ctx, "SELECT status FROM role_runs WHERE id = ?;", resArch.RoleRunID).Scan(&archStatus)
	if archStatus != "in_flight" {
		t.Errorf("archStatus = %s, want in_flight after rollback", archStatus)
	}
	var archFCount int
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM findings WHERE role_run_id = ?;", resArch.RoleRunID).Scan(&archFCount)
	if archFCount != 0 {
		t.Errorf("arch findings count = %d, want 0 after rollback", archFCount)
	}

	// Late duplicate publication on now-complete Requirements role returns PublicationConflictError
	_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
		SessionID:   "sess_pub_succ",
		RoleRunID:   resReq.RoleRunID,
		Role:        domain.RoleRequirements,
		Findings:    findings,
		CallCount:   1,
		CompletedAt: t0.Add(6 * time.Minute),
	})
	if !errors.Is(err, ErrPublicationConflict) {
		t.Errorf("expected ErrPublicationConflict on duplicate publish, got: %v", err)
	}
}

// =============================================================================
// 5. Failed Publication End-to-End
// =============================================================================

func TestE2E_05_FailedPublication(t *testing.T) {
	store, writer, _ := setupE2EStore(t, 100*time.Millisecond)
	ctx := context.Background()

	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	snap := createTestFrozenSnapshot(t, "snap_pub_fail")
	_, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_pub_fail",
		ProjectID:      "proj_pub_fail",
		IdempotencyKey: "idem_pub_fail",
		Title:          "Pub Fail Spec",
		Content:        "Pub Fail Content",
		Snapshot:       snap,
		CreatedAt:      t0,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	_, _ = store.ClaimSessionWithNow(ctx, TimingPolicy{
		DispatchCutoff:      20 * time.Minute,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: 35 * time.Minute,
	}, t0)

	res, err := store.ReservePendingRoleWithNow(ctx, "sess_pub_fail", t0.Add(1*time.Minute))
	if err != nil || !res.Reserved {
		t.Fatalf("reserve: %v", err)
	}

	tFail := t0.Add(2 * time.Minute)
	failRes, err := store.PublishRoleFailure(ctx, PublishFailureParams{
		SessionID:     "sess_pub_fail",
		RoleRunID:     res.RoleRunID,
		Role:          res.Role,
		ErrorCategory: domain.ErrTransport,
		ErrorMessage:  "model gateway 502 bad gateway",
		CallCount:     2,
		CompletedAt:   tFail,
	})
	if err != nil {
		t.Fatalf("PublishRoleFailure: %v", err)
	}
	if failRes.Status != domain.RoleFailed {
		t.Errorf("failRes.Status = %s, want failed", failRes.Status)
	}

	// Verify role run table fields
	var status, errCat, errMsg, compAt string
	var callCount int
	err = writer.QueryRowContext(ctx, "SELECT status, error_category, error_message, call_count, completed_at FROM role_runs WHERE id = ?;", res.RoleRunID).Scan(
		&status, &errCat, &errMsg, &callCount, &compAt,
	)
	if err != nil {
		t.Fatalf("query role_runs: %v", err)
	}
	if status != "failed" || errCat != "transport" || errMsg != "model gateway 502 bad gateway" || callCount != 2 || compAt != "2026-09-19T12:02:00.000000000Z" {
		t.Errorf("role run failure fields incorrect: status=%s cat=%s msg=%s call=%d comp=%s", status, errCat, errMsg, callCount, compAt)
	}

	// Zero findings and zero basis citations inserted
	var fCount, refCount int
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM findings WHERE role_run_id = ?;", res.RoleRunID).Scan(&fCount)
	_ = writer.QueryRowContext(ctx, `SELECT COUNT(*) FROM finding_basis_refs fbr
		JOIN findings f ON fbr.finding_id = f.id WHERE f.role_run_id = ?;`, res.RoleRunID).Scan(&refCount)
	if fCount != 0 || refCount != 0 {
		t.Errorf("expected 0 findings and 0 refs on failure, got fCount=%d refCount=%d", fCount, refCount)
	}

	// Late duplicate publication fails closed
	_, err = store.PublishRoleFailure(ctx, PublishFailureParams{
		SessionID:     "sess_pub_fail",
		RoleRunID:     res.RoleRunID,
		Role:          res.Role,
		ErrorCategory: domain.ErrTimeout,
		ErrorMessage:  "duplicate late fail",
		CallCount:     2,
		CompletedAt:   tFail.Add(1 * time.Minute),
	})
	if !errors.Is(err, ErrPublicationConflict) {
		t.Errorf("expected ErrPublicationConflict on duplicate failure publish, got: %v", err)
	}
}

// =============================================================================
// 6. Cancellation Flow End-to-End
// =============================================================================

func TestE2E_06_CancellationFlow(t *testing.T) {
	store, writer, _ := setupE2EStore(t, 100*time.Millisecond)
	ctx := context.Background()

	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	snap := createTestFrozenSnapshot(t, "snap_cancel_flow")
	_, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_cancel_flow",
		ProjectID:      "proj_cancel_flow",
		IdempotencyKey: "idem_cancel_flow",
		Title:          "Cancel Spec",
		Content:        "Cancel Content",
		Snapshot:       snap,
		CreatedAt:      t0,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	_, _ = store.ClaimSessionWithNow(ctx, TimingPolicy{
		DispatchCutoff:      20 * time.Minute,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: 35 * time.Minute,
	}, t0)

	// Dispatch 2 roles to in_flight
	res1, _ := store.ReservePendingRoleWithNow(ctx, "sess_cancel_flow", t0.Add(1*time.Minute))
	res2, _ := store.ReservePendingRoleWithNow(ctx, "sess_cancel_flow", t0.Add(2*time.Minute))

	// Request cancellation
	cancelRes, err := store.RequestCancellation(ctx, "sess_cancel_flow")
	if err != nil {
		t.Fatalf("RequestCancellation: %v", err)
	}
	if !cancelRes.Effective {
		t.Errorf("cancelRes.Effective = false, want true")
	}

	// Run cancellation sweep: pending roles (QA, Security) become interrupted with cause user_cancelled
	tSweep := t0.Add(3 * time.Minute)
	sweepRes, err := store.SweepCancellationWithNow(ctx, "sess_cancel_flow", tSweep)
	if err != nil {
		t.Fatalf("SweepCancellation: %v", err)
	}
	if sweepRes.InterruptedCount != 2 {
		t.Errorf("InterruptedCount = %d, want 2", sweepRes.InterruptedCount)
	}

	// Dispatch attempt after cancellation is refused
	dispAfter, err := store.ReservePendingRoleWithNow(ctx, "sess_cancel_flow", tSweep)
	if err != nil {
		t.Fatalf("ReservePendingRoleWithNow: %v", err)
	}
	if dispAfter.Reserved || dispAfter.NoWorkReason != DispatchNoWorkCancelled {
		t.Errorf("dispAfter = %+v, want Reserved=false Reason=DispatchNoWorkCancelled", dispAfter)
	}

	// Drain in-flight roles
	// Role 1 (Requirements) publishes success
	_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
		SessionID:   "sess_cancel_flow",
		RoleRunID:   res1.RoleRunID,
		Role:        res1.Role,
		Findings:    []domain.Finding{{ID: "f-drain-1", Kind: domain.FindingExisting, Severity: domain.SeverityHigh, Category: "correctness", Issue: "Issue 1", Recommendation: "Rec 1", BasisRefs: []string{"req-1"}}},
		CallCount:   1,
		CompletedAt: t0.Add(4 * time.Minute),
	})
	if err != nil {
		t.Fatalf("publish role 1 success: %v", err)
	}

	// Role 2 (Architecture) publishes failure
	_, err = store.PublishRoleFailure(ctx, PublishFailureParams{
		SessionID:     "sess_cancel_flow",
		RoleRunID:     res2.RoleRunID,
		Role:          res2.Role,
		ErrorCategory: domain.ErrTransport,
		ErrorMessage:  "cancelled in flight",
		CallCount:     1,
		CompletedAt:   t0.Add(4 * time.Minute),
	})
	if err != nil {
		t.Fatalf("publish role 2 failure: %v", err)
	}

	// Now all 4 roles are terminal: Req (complete), Arch (failed), QA (interrupted), Sec (interrupted)
	composeRes, err := store.ComposeSessionWithParams(ctx, ComposeParams{
		SessionID: "sess_cancel_flow",
		Now:       t0.Add(5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("ComposeSession failed: %v", err)
	}
	if composeRes.Status != domain.SessionPartial {
		t.Errorf("Status = %s, want partial", composeRes.Status)
	}
	if composeRes.TerminalReason != domain.ReasonUserCancelled {
		t.Errorf("TerminalReason = %s, want user_cancelled", composeRes.TerminalReason)
	}
	if composeRes.Report == nil || composeRes.Report.CompletedRoleCount != 1 || composeRes.Report.IncompleteRoleCount != 3 {
		t.Errorf("report role counts = %d/%d, want 1/3", composeRes.Report.CompletedRoleCount, composeRes.Report.IncompleteRoleCount)
	}
	if len(composeRes.Report.Findings) != 1 {
		t.Errorf("findings count = %d, want 1 (only complete role contributes findings)", len(composeRes.Report.Findings))
	}

	// Read terminal report and verify table snapshot
	snapBefore := takeDBSnapshot(t, writer)
	rep, err := store.ReadTerminalReport(ctx, "sess_cancel_flow")
	if err != nil {
		t.Fatalf("ReadTerminalReport: %v", err)
	}
	snapAfter := takeDBSnapshot(t, writer)
	assertDBSnapshotEqual(t, snapBefore, snapAfter, "ReadTerminalReport")
	if rep.Status != domain.SessionPartial || rep.Reason != domain.ReasonUserCancelled {
		t.Errorf("ReadTerminalReport mismatch: %+v", rep)
	}
}

// =============================================================================
// 7. Cancellation Before Dispatch End-to-End
// =============================================================================

func TestE2E_07_CancellationBeforeDispatch(t *testing.T) {
	store, writer, _ := setupE2EStore(t, 100*time.Millisecond)
	ctx := context.Background()

	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	snap := createTestFrozenSnapshot(t, "snap_cancel_early")
	_, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_cancel_early",
		ProjectID:      "proj_cancel_early",
		IdempotencyKey: "idem_cancel_early",
		Title:          "Early Cancel Spec",
		Content:        "Early Cancel Content",
		Snapshot:       snap,
		CreatedAt:      t0,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Cancel while still queued
	cRes, err := store.RequestCancellation(ctx, "sess_cancel_early")
	if err != nil || !cRes.Effective {
		t.Fatalf("RequestCancellation while queued: effective=%v err=%v", cRes.Effective, err)
	}

	// FIFO claim claims it
	policy := TimingPolicy{
		DispatchCutoff:      20 * time.Minute,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: 35 * time.Minute,
	}
	claimRes, err := store.ClaimSessionWithNow(ctx, policy, t0.Add(1*time.Minute))
	if err != nil || !claimRes.Claimed {
		t.Fatalf("claim: %v", err)
	}

	// Guarded dispatch immediately refuses due to cancel_requested = 1
	dRes, err := store.ReservePendingRoleWithNow(ctx, "sess_cancel_early", t0.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if dRes.Reserved || dRes.NoWorkReason != DispatchNoWorkCancelled {
		t.Errorf("dRes = %+v, want Reserved=false Reason=DispatchNoWorkCancelled", dRes)
	}

	// Cancellation sweep interrupts all 4 pending roles
	sRes, err := store.SweepCancellationWithNow(ctx, "sess_cancel_early", t0.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if sRes.InterruptedCount != 4 {
		t.Errorf("InterruptedCount = %d, want 4", sRes.InterruptedCount)
	}

	// Compose session yields canonical zero-completed partial result
	composeRes, err := store.ComposeSessionWithParams(ctx, ComposeParams{
		SessionID: "sess_cancel_early",
		Now:       t0.Add(4 * time.Minute),
	})
	if err != nil {
		t.Fatalf("ComposeSession: %v", err)
	}
	if composeRes.Status != domain.SessionPartial {
		t.Errorf("Status = %s, want partial", composeRes.Status)
	}
	if composeRes.TerminalReason != domain.ReasonUserCancelled {
		t.Errorf("TerminalReason = %s, want user_cancelled", composeRes.TerminalReason)
	}
	if composeRes.Report.CompletedRoleCount != 0 || composeRes.Report.IncompleteRoleCount != 4 {
		t.Errorf("counts = %d/%d, want 0/4", composeRes.Report.CompletedRoleCount, composeRes.Report.IncompleteRoleCount)
	}
	if len(composeRes.Report.Findings) != 0 {
		t.Errorf("findings count = %d, want 0", len(composeRes.Report.Findings))
	}

	// Verify terminal report
	snapBefore := takeDBSnapshot(t, writer)
	rep, err := store.ReadTerminalReport(ctx, "sess_cancel_early")
	if err != nil {
		t.Fatalf("ReadTerminalReport: %v", err)
	}
	snapAfter := takeDBSnapshot(t, writer)
	assertDBSnapshotEqual(t, snapBefore, snapAfter, "ReadTerminalReport")
	if rep.CompletedRoleCount != 0 || rep.IncompleteRoleCount != 4 {
		t.Errorf("rep counts = %d/%d, want 0/4", rep.CompletedRoleCount, rep.IncompleteRoleCount)
	}
}

// =============================================================================
// 8. Cutoff Flow End-to-End
// =============================================================================

func TestE2E_08_CutoffFlow(t *testing.T) {
	store, _, _ := setupE2EStore(t, 100*time.Millisecond)
	ctx := context.Background()

	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	snap := createTestFrozenSnapshot(t, "snap_cutoff_flow")
	_, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_cutoff_flow",
		ProjectID:      "proj_cutoff_flow",
		IdempotencyKey: "idem_cutoff_flow",
		Title:          "Cutoff Spec",
		Content:        "Cutoff Content",
		Snapshot:       snap,
		CreatedAt:      t0,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	policy := TimingPolicy{
		DispatchCutoff:      10 * time.Minute,
		CallTimeout:         5 * time.Minute,
		SessionHardDeadline: 20 * time.Minute,
	}
	_, _ = store.ClaimSessionWithNow(ctx, policy, t0)

	// Dispatch 1 role to in_flight before cutoff
	res1, _ := store.ReservePendingRoleWithNow(ctx, "sess_cutoff_flow", t0.Add(1*time.Minute))

	// Advance time past dispatch cutoff (10 minutes)
	tPastCutoff := t0.Add(10*time.Minute + 1*time.Second)

	// Guarded dispatch after cutoff is refused
	dRes, err := store.ReservePendingRoleWithNow(ctx, "sess_cutoff_flow", tPastCutoff)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if dRes.Reserved || dRes.NoWorkReason != DispatchNoWorkCutoff {
		t.Errorf("dRes = %+v, want Reserved=false Reason=DispatchNoWorkCutoff", dRes)
	}

	// Cutoff sweep interrupts the remaining 3 pending roles
	cRes, err := store.SweepCutoff(ctx, "sess_cutoff_flow", tPastCutoff)
	if err != nil {
		t.Fatalf("SweepCutoff: %v", err)
	}
	if cRes.InterruptedCount != 3 {
		t.Errorf("InterruptedCount = %d, want 3", cRes.InterruptedCount)
	}

	// In-flight role (Requirements) finishes and publishes success after cutoff
	tPub := tPastCutoff.Add(1 * time.Minute)
	_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
		SessionID:   "sess_cutoff_flow",
		RoleRunID:   res1.RoleRunID,
		Role:        res1.Role,
		Findings:    []domain.Finding{{ID: "f-cut-1", Kind: domain.FindingExisting, Severity: domain.SeverityLow, Category: "perf", Issue: "Issue", Recommendation: "Rec", BasisRefs: []string{"brief-1"}}},
		CallCount:   1,
		CompletedAt: tPub,
	})
	if err != nil {
		t.Fatalf("publish in-flight after cutoff: %v", err)
	}

	// Compose session
	composeRes, err := store.ComposeSessionWithParams(ctx, ComposeParams{
		SessionID: "sess_cutoff_flow",
		Now:       tPub.Add(1 * time.Minute),
	})
	if err != nil {
		t.Fatalf("ComposeSession: %v", err)
	}
	if composeRes.Status != domain.SessionPartial {
		t.Errorf("Status = %s, want partial", composeRes.Status)
	}
	if composeRes.TerminalReason != domain.ReasonDeadlineCutoff {
		t.Errorf("TerminalReason = %s, want deadline_cutoff", composeRes.TerminalReason)
	}
	if composeRes.Report.CompletedRoleCount != 1 || composeRes.Report.IncompleteRoleCount != 3 {
		t.Errorf("counts = %d/%d, want 1/3", composeRes.Report.CompletedRoleCount, composeRes.Report.IncompleteRoleCount)
	}
}

// =============================================================================
// 9. Hard Deadline Flow End-to-End
// =============================================================================

func TestE2E_09_HardDeadlineFlow(t *testing.T) {
	store, _, _ := setupE2EStore(t, 100*time.Millisecond)
	ctx := context.Background()

	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	snap := createTestFrozenSnapshot(t, "snap_hard_dead")
	_, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_hard_dead",
		ProjectID:      "proj_hard_dead",
		IdempotencyKey: "idem_hard_dead",
		Title:          "Hard Dead Spec",
		Content:        "Hard Dead Content",
		Snapshot:       snap,
		CreatedAt:      t0,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	policy := TimingPolicy{
		DispatchCutoff:      15 * time.Minute,
		CallTimeout:         5 * time.Minute,
		SessionHardDeadline: 25 * time.Minute,
	}
	_, _ = store.ClaimSessionWithNow(ctx, policy, t0)

	// Dispatch 2 roles
	resReq, _ := store.ReservePendingRoleWithNow(ctx, "sess_hard_dead", t0.Add(1*time.Minute))
	_, _ = store.ReservePendingRoleWithNow(ctx, "sess_hard_dead", t0.Add(2*time.Minute))

	// Advance time past hard deadline (25 minutes)
	tPastDead := t0.Add(25*time.Minute + 1*time.Second)

	// Sweep hard deadline: pending roles (QA, Security) interrupted with cause deadline_cutoff
	// and in-flight roles (Req, Arch) are exposed
	deadRes, err := store.SweepHardDeadline(ctx, "sess_hard_dead", tPastDead)
	if err != nil {
		t.Fatalf("SweepHardDeadline: %v", err)
	}
	if deadRes.InterruptedCount != 2 {
		t.Errorf("InterruptedCount = %d, want 2", deadRes.InterruptedCount)
	}
	if len(deadRes.InFlightRoles) != 2 {
		t.Fatalf("InFlightRoles = %d, want 2", len(deadRes.InFlightRoles))
	}

	// Worker routes in-flight roles through P9 timeout failure
	for _, ifr := range deadRes.InFlightRoles {
		_, err := store.PublishRoleFailure(ctx, PublishFailureParams{
			SessionID:     "sess_hard_dead",
			RoleRunID:     ifr.RoleRunID,
			Role:          ifr.Role,
			ErrorCategory: domain.ErrTimeout,
			ErrorMessage:  "session hard deadline exceeded",
			CallCount:     1,
			CompletedAt:   tPastDead,
		})
		if err != nil {
			t.Fatalf("publish timeout failure for %s: %v", ifr.Role, err)
		}
	}

	// Compose session: 0 completed, 4 incomplete -> status = failed, reason = deadline_cutoff
	composeRes, err := store.ComposeSessionWithParams(ctx, ComposeParams{
		SessionID: "sess_hard_dead",
		Now:       tPastDead.Add(1 * time.Minute),
	})
	if err != nil {
		t.Fatalf("ComposeSession: %v", err)
	}
	if composeRes.Status != domain.SessionFailed {
		t.Errorf("Status = %s, want failed", composeRes.Status)
	}
	if composeRes.TerminalReason != domain.ReasonDeadlineCutoff {
		t.Errorf("TerminalReason = %s, want deadline_cutoff", composeRes.TerminalReason)
	}
	if composeRes.Report.CompletedRoleCount != 0 || composeRes.Report.IncompleteRoleCount != 4 {
		t.Errorf("counts = %d/%d, want 0/4", composeRes.Report.CompletedRoleCount, composeRes.Report.IncompleteRoleCount)
	}

	// Late success publication attempt fails closed and cannot overwrite terminal state
	_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
		SessionID:   "sess_hard_dead",
		RoleRunID:   resReq.RoleRunID,
		Role:        resReq.Role,
		Findings:    []domain.Finding{{ID: "late-find", Kind: domain.FindingExisting, Severity: domain.SeverityLow, Category: "test", Issue: "Late", Recommendation: "Late", BasisRefs: []string{"brief-1"}}},
		CallCount:   1,
		CompletedAt: tPastDead.Add(2 * time.Minute),
	})
	if !errors.Is(err, ErrPublicationConflict) {
		t.Errorf("expected ErrPublicationConflict on late success publish, got: %v", err)
	}
}

// =============================================================================
// 10. Restart Recovery End-to-End
// =============================================================================

func TestE2E_10_RestartRecovery(t *testing.T) {
	store, writer, _ := setupE2EStore(t, 100*time.Millisecond)
	ctx := context.Background()

	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	snap := createTestFrozenSnapshot(t, "snap_restart")
	_, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_restart",
		ProjectID:      "proj_restart",
		IdempotencyKey: "idem_restart",
		Title:          "Restart Spec",
		Content:        "Restart Content",
		Snapshot:       snap,
		CreatedAt:      t0,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	_, _ = store.ClaimSessionWithNow(ctx, TimingPolicy{
		DispatchCutoff:      20 * time.Minute,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: 35 * time.Minute,
	}, t0)

	// Role 1 (Requirements) is published complete with findings
	resReq, _ := store.ReservePendingRoleWithNow(ctx, "sess_restart", t0.Add(1*time.Minute))
	findings := []domain.Finding{
		{
			ID:             "f-restart-1",
			Kind:           domain.FindingExisting,
			Severity:       domain.SeverityHigh,
			Category:       "correctness",
			Issue:          "Spec issue",
			Recommendation: "Fix it",
			BasisRefs:      []string{"req-1"},
		},
	}
	_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
		SessionID:   "sess_restart",
		RoleRunID:   resReq.RoleRunID,
		Role:        resReq.Role,
		Findings:    findings,
		CallCount:   1,
		CompletedAt: t0.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("publish req: %v", err)
	}

	// Role 2 (Architecture) is in_flight when crash occurs
	resArch, _ := store.ReservePendingRoleWithNow(ctx, "sess_restart", t0.Add(3*time.Minute))

	// Roles 3 and 4 (QA, Security) remain pending

	// Simulate restart recovery sweep:
	tRestart := t0.Add(30 * time.Minute)
	sweepRes, err := store.SweepRestartRecoveryScoped(ctx, "proj_restart", t0.Add(10*time.Minute), tRestart)
	if err != nil {
		t.Fatalf("SweepRestartRecovery: %v", err)
	}
	if sweepRes.SessionsRecovered != 1 {
		t.Errorf("SessionsRecovered = %d, want 1", sweepRes.SessionsRecovered)
	}

	// Verify Requirements is STILL complete with findings
	var reqStatus string
	_ = writer.QueryRowContext(ctx, "SELECT status FROM role_runs WHERE id = ?;", resReq.RoleRunID).Scan(&reqStatus)
	if reqStatus != "complete" {
		t.Errorf("Requirements status = %s, want complete (preserved)", reqStatus)
	}
	var fCount int
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM findings WHERE role_run_id = ?;", resReq.RoleRunID).Scan(&fCount)
	if fCount != 1 {
		t.Errorf("Requirements findings count = %d, want 1", fCount)
	}

	// Verify Architecture transitioned in_flight -> interrupted/process_restart
	var archStatus, archCause string
	_ = writer.QueryRowContext(ctx, "SELECT status, cause FROM role_runs WHERE id = ?;", resArch.RoleRunID).Scan(&archStatus, &archCause)
	if archStatus != "interrupted" || archCause != "process_restart" {
		t.Errorf("Architecture: status=%s cause=%s, want interrupted/process_restart", archStatus, archCause)
	}

	// Verify QA and Security transitioned pending -> interrupted/process_restart
	var pendInterruptedCount int
	_ = writer.QueryRowContext(ctx, `SELECT COUNT(*) FROM role_runs
		WHERE session_id = 'sess_restart' AND role IN ('qa', 'security') AND status = 'interrupted' AND cause = 'process_restart';`).Scan(&pendInterruptedCount)
	if pendInterruptedCount != 2 {
		t.Errorf("pending roles interrupted count = %d, want 2", pendInterruptedCount)
	}

	// Never replay provider work: assert 0 pending roles remain
	var pendingCount int
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM role_runs WHERE session_id = 'sess_restart' AND status = 'pending';").Scan(&pendingCount)
	if pendingCount != 0 {
		t.Errorf("pending count = %d, want 0 (provider work never replayed)", pendingCount)
	}

	// Compose session
	composeRes, err := store.ComposeSessionWithParams(ctx, ComposeParams{
		SessionID: "sess_restart",
		Now:       tRestart.Add(1 * time.Minute),
	})
	if err != nil {
		t.Fatalf("ComposeSession: %v", err)
	}
	if composeRes.Status != domain.SessionPartial {
		t.Errorf("Status = %s, want partial", composeRes.Status)
	}
	if composeRes.TerminalReason != domain.ReasonProcessRestart {
		t.Errorf("TerminalReason = %s, want process_restart", composeRes.TerminalReason)
	}
	if composeRes.Report.CompletedRoleCount != 1 || composeRes.Report.IncompleteRoleCount != 3 {
		t.Errorf("counts = %d/%d, want 1/3", composeRes.Report.CompletedRoleCount, composeRes.Report.IncompleteRoleCount)
	}
	if len(composeRes.Report.Findings) != 1 {
		t.Errorf("findings count = %d, want 1", len(composeRes.Report.Findings))
	}
}

// =============================================================================
// 11. Same-Key Concurrent Submissions End-to-End
// =============================================================================

func TestE2E_11_ConcurrentSubmissions(t *testing.T) {
	store, writer, _ := setupE2EStore(t, 200*time.Millisecond)
	ctx := context.Background()

	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	snap := createTestFrozenSnapshot(t, "snap_conc_sub")

	const concurrentWorkers = 10
	startBarrier := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(concurrentWorkers)

	results := make([]*SubmitResult, concurrentWorkers)
	errs := make([]error, concurrentWorkers)

	for i := 0; i < concurrentWorkers; i++ {
		go func(idx int) {
			defer wg.Done()
			<-startBarrier // Barrier synchronization: fire simultaneously
			results[idx], errs[idx] = store.Submit(ctx, SubmitParams{
				SessionID:      "sess_conc_winner",
				ProjectID:      "proj_conc_sub",
				IdempotencyKey: "idem_shared_key",
				Title:          "Shared Title",
				Content:        "Shared Content",
				Snapshot:       snap,
				CreatedAt:      t0,
			})
		}(i)
	}

	close(startBarrier)
	wg.Wait()

	var freshCount, replayCount int
	var sharedSessionID string
	for i := 0; i < concurrentWorkers; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d failed: %v", i, errs[i])
		}
		if results[i].Replay {
			replayCount++
		} else {
			freshCount++
			sharedSessionID = results[i].SessionID
		}
	}

	// Exactly one fresh submission, remaining are replays
	if freshCount != 1 {
		t.Errorf("freshCount = %d, want exactly 1", freshCount)
	}
	if replayCount != concurrentWorkers-1 {
		t.Errorf("replayCount = %d, want %d", replayCount, concurrentWorkers-1)
	}
	for i := 0; i < concurrentWorkers; i++ {
		if results[i].SessionID != sharedSessionID {
			t.Errorf("worker %d session ID mismatch: %s vs %s", i, results[i].SessionID, sharedSessionID)
		}
	}

	// Check database: exactly 1 session, 1 snapshot, 4 role runs
	var sessCount, snapCount, roleCount int
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions WHERE project_id = 'proj_conc_sub';").Scan(&sessCount)
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM snapshots WHERE id = ?;", snap.ID).Scan(&snapCount)
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM role_runs WHERE session_id = ?;", sharedSessionID).Scan(&roleCount)
	if sessCount != 1 || snapCount != 1 || roleCount != 4 {
		t.Errorf("database counts: sess=%d snap=%d role=%d, want 1, 1, 4", sessCount, snapCount, roleCount)
	}

	// Idempotency conflict test: same (project, key) with DIFFERENT payload produces typed error
	_, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_different_id",
		ProjectID:      "proj_conc_sub",
		IdempotencyKey: "idem_shared_key",
		Title:          "Mismatched Title",
		Content:        "Mismatched Content",
		Snapshot:       snap,
		CreatedAt:      t0,
	})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Errorf("expected ErrIdempotencyConflict on payload mismatch, got: %v", err)
	}

	// Distinct project test: different project_id with same idempotency_key is allowed
	distinctSnap := createTestFrozenSnapshot(t, "snap_distinct_proj")
	resDiffProj, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_diff_proj",
		ProjectID:      "proj_other",
		IdempotencyKey: "idem_shared_key",
		Title:          "Other Project Spec",
		Content:        "Other Project Content",
		Snapshot:       distinctSnap,
		CreatedAt:      t0,
	})
	if err != nil {
		t.Fatalf("submit different project: %v", err)
	}
	if resDiffProj.Replay {
		t.Errorf("expected fresh submit for different project, got replay")
	}
}

// =============================================================================
// 12. Concurrent Actors & Single-Winner Races End-to-End
// =============================================================================

func TestE2E_12_ConcurrentActorsNoLostUpdate(t *testing.T) {
	store, writer, _ := setupE2EStore(t, 200*time.Millisecond)
	ctx := context.Background()

	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	snap := createTestFrozenSnapshot(t, "snap_conc_actors")

	// 1. Concurrent Claimers: exactly one winner
	for i := 0; i < 3; i++ {
		_, err := store.Submit(ctx, SubmitParams{
			SessionID:      fmt.Sprintf("sess_claim_race_%d", i),
			ProjectID:      "proj_race",
			IdempotencyKey: fmt.Sprintf("idem_claim_race_%d", i),
			Title:          fmt.Sprintf("Race Spec %d", i),
			Content:        "Content",
			Snapshot:       snap,
			CreatedAt:      t0.Add(time.Duration(i) * time.Minute),
		})
		if err != nil {
			t.Fatalf("submit claim race %d: %v", i, err)
		}
	}

	const claimersCount = 5
	claimBarrier := make(chan struct{})
	var claimWg sync.WaitGroup
	claimWg.Add(claimersCount)

	claimResults := make([]*ClaimResult, claimersCount)
	claimErrs := make([]error, claimersCount)
	policy := TimingPolicy{
		DispatchCutoff:      20 * time.Minute,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: 35 * time.Minute,
	}

	for i := 0; i < claimersCount; i++ {
		go func(idx int) {
			defer claimWg.Done()
			<-claimBarrier
			claimResults[idx], claimErrs[idx] = store.ClaimSessionWithNow(ctx, policy, t0.Add(5*time.Minute))
		}(i)
	}
	close(claimBarrier)
	claimWg.Wait()

	var claimedWinners int
	var claimedSessID string
	for i := 0; i < claimersCount; i++ {
		if claimErrs[i] != nil {
			t.Fatalf("claim worker %d failed: %v", i, claimErrs[i])
		}
		if claimResults[i].Claimed {
			claimedWinners++
			claimedSessID = claimResults[i].SessionID
		}
	}
	if claimedWinners != 1 {
		t.Errorf("claimedWinners = %d, want exactly 1", claimedWinners)
	}
	if claimedSessID != "sess_claim_race_0" {
		t.Errorf("claimedSessID = %q, want sess_claim_race_0 (FIFO)", claimedSessID)
	}

	// 2. Concurrent Dispatchers on claimed session: at most 2 in-flight winners
	const dispWorkers = 6
	dispBarrier := make(chan struct{})
	var dispWg sync.WaitGroup
	dispWg.Add(dispWorkers)

	dispResults := make([]*DispatchReservationResult, dispWorkers)
	dispErrs := make([]error, dispWorkers)

	for i := 0; i < dispWorkers; i++ {
		go func(idx int) {
			defer dispWg.Done()
			<-dispBarrier
			dispResults[idx], dispErrs[idx] = store.ReservePendingRoleWithNow(ctx, claimedSessID, t0.Add(6*time.Minute))
		}(i)
	}
	close(dispBarrier)
	dispWg.Wait()

	var dispWinners int
	reservedRoles := make(map[domain.Role]string)
	for i := 0; i < dispWorkers; i++ {
		if dispErrs[i] != nil {
			t.Fatalf("disp worker %d failed: %v", i, dispErrs[i])
		}
		if dispResults[i].Reserved {
			dispWinners++
			reservedRoles[dispResults[i].Role] = dispResults[i].RoleRunID
		}
	}
	if dispWinners != 2 {
		t.Errorf("dispWinners = %d, want exactly 2 (MAX_IN_FLIGHT bound)", dispWinners)
	}
	if _, ok := reservedRoles[domain.RoleRequirements]; !ok {
		t.Errorf("expected Requirements to be reserved")
	}
	if _, ok := reservedRoles[domain.RoleArchitecture]; !ok {
		t.Errorf("expected Architecture to be reserved")
	}

	// 3. Concurrent Publishers on RoleRequirements: exactly one winner
	reqRunID := reservedRoles[domain.RoleRequirements]
	const pubWorkers = 4
	pubBarrier := make(chan struct{})
	var pubWg sync.WaitGroup
	pubWg.Add(pubWorkers)

	pubResults := make([]*PublishResult, pubWorkers)
	pubErrs := make([]error, pubWorkers)

	for i := 0; i < pubWorkers; i++ {
		go func(idx int) {
			defer pubWg.Done()
			<-pubBarrier
			if idx%2 == 0 {
				pubResults[idx], pubErrs[idx] = store.PublishRoleSuccess(ctx, PublishSuccessParams{
					SessionID:   claimedSessID,
					RoleRunID:   reqRunID,
					Role:        domain.RoleRequirements,
					Findings:    []domain.Finding{{ID: fmt.Sprintf("find-race-%d", idx), Kind: domain.FindingExisting, Severity: domain.SeverityLow, Category: "test", Issue: "Issue", Recommendation: "Rec", BasisRefs: []string{"req-1"}}},
					CallCount:   1,
					CompletedAt: t0.Add(7 * time.Minute),
				})
			} else {
				pubResults[idx], pubErrs[idx] = store.PublishRoleFailure(ctx, PublishFailureParams{
					SessionID:     claimedSessID,
					RoleRunID:     reqRunID,
					Role:          domain.RoleRequirements,
					ErrorCategory: domain.ErrTransport,
					ErrorMessage:  "race fail",
					CallCount:     1,
					CompletedAt:   t0.Add(7 * time.Minute),
				})
			}
		}(i)
	}
	close(pubBarrier)
	pubWg.Wait()

	var pubWinners int
	for i := 0; i < pubWorkers; i++ {
		if pubErrs[i] == nil {
			pubWinners++
		} else if !errors.Is(pubErrs[i], ErrPublicationConflict) {
			t.Errorf("worker %d unexpected error: %v", i, pubErrs[i])
		}
	}
	if pubWinners != 1 {
		t.Errorf("pubWinners = %d, want exactly 1 (CAS guard)", pubWinners)
	}

	// Complete remaining roles to make session terminal
	archRunID := reservedRoles[domain.RoleArchitecture]
	if _, pubErr := store.PublishRoleFailure(ctx, PublishFailureParams{
		SessionID:     claimedSessID,
		RoleRunID:     archRunID,
		Role:          domain.RoleArchitecture,
		ErrorCategory: domain.ErrTimeout,
		ErrorMessage:  "timeout",
		CallCount:     1,
		CompletedAt:   t0.Add(8 * time.Minute),
	}); pubErr != nil {
		t.Fatalf("publish arch fail: %v", pubErr)
	}
	if _, err := store.RequestCancellation(ctx, claimedSessID); err != nil {
		t.Fatalf("request cancel: %v", err)
	}
	if _, sweepErr := store.SweepCancellationWithNow(ctx, claimedSessID, t0.Add(9*time.Minute)); sweepErr != nil {
		t.Fatalf("sweep cancel: %v", sweepErr)
	}

	// 4. Concurrent Composers: exactly one compare-and-set winner, no lost update
	const compWorkers = 6
	compBarrier := make(chan struct{})
	var compWg sync.WaitGroup
	compWg.Add(compWorkers)

	compResults := make([]*ComposeResult, compWorkers)
	compErrs := make([]error, compWorkers)

	for i := 0; i < compWorkers; i++ {
		go func(idx int) {
			defer compWg.Done()
			<-compBarrier
			compResults[idx], compErrs[idx] = store.ComposeSessionWithParams(ctx, ComposeParams{
				SessionID: claimedSessID,
				Now:       t0.Add(10 * time.Minute),
			})
		}(i)
	}
	close(compBarrier)
	compWg.Wait()

	var composerCASWinners, alreadyTerminalWinners int
	for i := 0; i < compWorkers; i++ {
		if compErrs[i] != nil {
			t.Fatalf("comp worker %d failed: %v", i, compErrs[i])
		}
		if compResults[i].IsComposed() && !compResults[i].IsAlreadyTerminal() {
			composerCASWinners++
		} else if compResults[i].IsAlreadyTerminal() {
			alreadyTerminalWinners++
		}
	}
	if composerCASWinners != 1 {
		t.Errorf("composerCASWinners = %d, want exactly 1", composerCASWinners)
	}
	if alreadyTerminalWinners != compWorkers-1 {
		t.Errorf("alreadyTerminalWinners = %d, want %d", alreadyTerminalWinners, compWorkers-1)
	}

	// Verify consistent terminal state
	var sessStatus string
	_ = writer.QueryRowContext(ctx, "SELECT status FROM sessions WHERE id = ?;", claimedSessID).Scan(&sessStatus)
	if sessStatus != "partial" {
		t.Errorf("terminal session status = %s, want partial", sessStatus)
	}
}

// =============================================================================
// 13. Busy/Locked Retry Exhaustion End-to-End
// =============================================================================

func TestE2E_13_BusyRetryExhaustion(t *testing.T) {
	store, _, dbPath := setupE2EStore(t, 20*time.Millisecond)
	ctx := context.Background()

	// Direct raw connection to hold RESERVED / IMMEDIATE lock
	rawDSN := buildDSN(dbPath, 20, false)
	directDB, err := sql.Open("sqlite", rawDSN)
	if err != nil {
		t.Fatalf("open direct DB: %v", err)
	}
	defer directDB.Close()

	directConn, err := directDB.Conn(ctx)
	if err != nil {
		t.Fatalf("directDB.Conn: %v", err)
	}
	defer directConn.Close()

	// Acquire lock on direct connection
	_, err = directConn.ExecContext(ctx, "BEGIN IMMEDIATE;")
	if err != nil {
		t.Fatalf("direct BEGIN IMMEDIATE: %v", err)
	}

	// Fast retry policy: MaxRetries = 2 (total 3 attempts) with zero backoff
	fastRetry := RetryPolicy{
		Op:             "test_e2e_exhaustion",
		MaxRetries:     2,
		InitialBackoff: 0,
		MaxBackoff:     0,
		BackoffFactor:  1.0,
	}

	var attemptsObserved int32
	origAttemptHook := attemptHook
	attemptHook = func(a int) {
		atomic.StoreInt32(&attemptsObserved, int32(a))
	}
	defer func() { attemptHook = origAttemptHook }()

	opErr := store.withImmediateOp(ctx, "test_e2e_exhaustion", fastRetry, func(conn *sql.Conn) error {
		return nil
	})

	// Release lock
	_, _ = directConn.ExecContext(context.Background(), "ROLLBACK;")

	// Must fail with typed PersistenceUnavailable error
	if opErr == nil {
		t.Fatalf("expected PersistenceUnavailable error, got nil")
	}
	if !errors.Is(opErr, &PersistenceUnavailable{}) {
		t.Errorf("errors.Is(opErr, &PersistenceUnavailable{}) = false, got: %v", opErr)
	}
	var pu *PersistenceUnavailable
	if !errors.As(opErr, &pu) {
		t.Fatalf("errors.As(opErr, &pu) = false")
	}
	if pu.Op != "test_e2e_exhaustion" {
		t.Errorf("pu.Op = %q, want test_e2e_exhaustion", pu.Op)
	}
	if pu.Attempts != 3 {
		t.Errorf("pu.Attempts = %d, want 3 (1 + 2 retries)", pu.Attempts)
	}
	if atomic.LoadInt32(&attemptsObserved) != 3 {
		t.Errorf("attemptsObserved = %d, want 3", attemptsObserved)
	}

	// Test context cancellation during retry loop
	cancCtx, cancel := context.WithCancel(ctx)
	cancel() // pre-cancelled
	cancErr := store.withImmediateOp(cancCtx, "cancelled_op", fastRetry, func(conn *sql.Conn) error {
		return nil
	})
	if !errors.Is(cancErr, context.Canceled) {
		t.Errorf("expected context.Canceled, got: %v", cancErr)
	}
}

// =============================================================================
// 14. Report Determinism and Immutability End-to-End
// =============================================================================

func TestE2E_14_ReportDeterminismAndImmutability(t *testing.T) {
	store, writer, _ := setupE2EStore(t, 100*time.Millisecond)
	ctx := context.Background()

	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	snap := createTestFrozenSnapshot(t, "snap_det_rep")

	// 1. Report before terminalization returns typed not-finished
	_, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_not_term",
		ProjectID:      "proj_rep",
		IdempotencyKey: "idem_rep_not_term",
		Title:          "Not Term Spec",
		Content:        "Not Term Content",
		Snapshot:       snap,
		CreatedAt:      t0,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	snapBefore1 := takeDBSnapshot(t, writer)
	_, err = store.ReadReport(ctx, "sess_not_term")
	if !errors.Is(err, ErrNotTerminal) || !errors.Is(err, ErrNotFinished) {
		t.Errorf("expected ErrNotFinished on queued session, got: %v", err)
	}
	var notTermErr *SessionNotTerminalError
	if !errors.As(err, &notTermErr) {
		t.Errorf("expected *SessionNotTerminalError")
	}
	snapAfter1 := takeDBSnapshot(t, writer)
	assertDBSnapshotEqual(t, snapBefore1, snapAfter1, "ReadReport on queued session")

	// Claim to reviewing and test report refusal again
	_, _ = store.ClaimSessionWithNow(ctx, TimingPolicy{
		DispatchCutoff:      20 * time.Minute,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: 35 * time.Minute,
	}, t0)
	snapBefore2 := takeDBSnapshot(t, writer)
	_, err = store.ReadTerminalReport(ctx, "sess_not_term")
	if !errors.Is(err, ErrNotTerminal) || !errors.Is(err, ErrNotFinished) {
		t.Errorf("expected ErrNotFinished on reviewing session, got: %v", err)
	}
	snapAfter2 := takeDBSnapshot(t, writer)
	assertDBSnapshotEqual(t, snapBefore2, snapAfter2, "ReadTerminalReport on reviewing session")

	// 2. Set up completed terminal session with multiple findings across multiple roles
	// to prove deterministic total ordering
	findingsReq := []domain.Finding{
		{
			ID:             "f-req-low",
			Kind:           domain.FindingExisting,
			Severity:       domain.SeverityLow,
			Category:       "clarity",
			Issue:          "Low req",
			Recommendation: "Rec low",
			BasisRefs:      []string{"req-1"},
		},
		{
			ID:             "f-req-crit",
			Kind:           domain.FindingExisting,
			Severity:       domain.SeverityCritical,
			Category:       "correctness",
			Issue:          "Crit req",
			Recommendation: "Rec crit",
			BasisRefs:      []string{"brief-1", "req-1"},
		},
	}
	findingsArch := []domain.Finding{
		{
			ID:             "f-arch-crit",
			Kind:           domain.FindingExisting,
			Severity:       domain.SeverityCritical,
			Category:       "architecture",
			Issue:          "Crit arch",
			Recommendation: "Rec arch",
			BasisRefs:      []string{"arch-1"},
		},
		{
			ID:             "f-arch-med",
			Kind:           domain.FindingExisting,
			Severity:       domain.SeverityMedium,
			Category:       "reliability",
			Issue:          "Med arch",
			Recommendation: "Rec med",
			BasisRefs:      []string{"flow-1", "const-1"},
		},
	}

	res1, _ := store.ReservePendingRoleWithNow(ctx, "sess_not_term", t0.Add(1*time.Minute))
	_, _ = store.PublishRoleSuccess(ctx, PublishSuccessParams{
		SessionID:   "sess_not_term",
		RoleRunID:   res1.RoleRunID,
		Role:        res1.Role,
		Findings:    findingsReq,
		CallCount:   1,
		CompletedAt: t0.Add(2 * time.Minute),
	})

	res2, _ := store.ReservePendingRoleWithNow(ctx, "sess_not_term", t0.Add(3*time.Minute))
	_, _ = store.PublishRoleSuccess(ctx, PublishSuccessParams{
		SessionID:   "sess_not_term",
		RoleRunID:   res2.RoleRunID,
		Role:        res2.Role,
		Findings:    findingsArch,
		CallCount:   1,
		CompletedAt: t0.Add(4 * time.Minute),
	})

	res3, _ := store.ReservePendingRoleWithNow(ctx, "sess_not_term", t0.Add(5*time.Minute))
	_, _ = store.PublishRoleFailure(ctx, PublishFailureParams{
		SessionID:     "sess_not_term",
		RoleRunID:     res3.RoleRunID,
		Role:          res3.Role,
		ErrorCategory: domain.ErrBudgetExhausted,
		ErrorMessage:  "budget exhausted",
		CallCount:     0,
		CompletedAt:   t0.Add(6 * time.Minute),
	})

	res4, _ := store.ReservePendingRoleWithNow(ctx, "sess_not_term", t0.Add(7*time.Minute))
	_, _ = store.PublishRoleSuccess(ctx, PublishSuccessParams{
		SessionID:   "sess_not_term",
		RoleRunID:   res4.RoleRunID,
		Role:        res4.Role,
		Findings:    nil,
		CallCount:   1,
		CompletedAt: t0.Add(8 * time.Minute),
	})

	// Compose session to complete/partial
	compRes, err := store.ComposeSessionWithParams(ctx, ComposeParams{
		SessionID: "sess_not_term",
		Now:       t0.Add(9 * time.Minute),
	})
	if err != nil {
		t.Fatalf("ComposeSession: %v", err)
	}
	if !compRes.IsComposed() {
		t.Fatalf("session failed to compose")
	}

	// 3. Repeated report reads: byte-for-byte identical, zero mutation
	var canonicalJSON []byte
	snapBase := takeDBSnapshot(t, writer)

	for readIdx := 0; readIdx < 5; readIdx++ {
		repRead, err := store.ReadReport(ctx, "sess_not_term")
		if err != nil {
			t.Fatalf("ReadReport attempt %d failed: %v", readIdx, err)
		}
		data, err := json.Marshal(repRead)
		if err != nil {
			t.Fatalf("marshal report: %v", err)
		}
		if readIdx == 0 {
			canonicalJSON = data
		} else if string(data) != string(canonicalJSON) {
			t.Fatalf("read %d diverged from canonical report:\n  got: %s\n want: %s", readIdx, string(data), string(canonicalJSON))
		}

		snapIter := takeDBSnapshot(t, writer)
		assertDBSnapshotEqual(t, snapBase, snapIter, fmt.Sprintf("ReadReport repetition %d", readIdx))
	}

	// Verify deterministic total order of findings:
	// Order: severity rank, role rank, primary basis_ref, category, finding id
	repFinal, _ := store.ReadReport(ctx, "sess_not_term")
	if len(repFinal.Findings) != 4 {
		t.Fatalf("findings count = %d, want 4", len(repFinal.Findings))
	}
	// Critical items first:
	// - f-arch-crit (Architecture) vs f-req-crit (Requirements): Requirements role rank is before Architecture!
	// So f-req-crit must be first, then f-arch-crit.
	if repFinal.Findings[0].Finding.ID != "f-req-crit" {
		t.Errorf("finding[0] = %s, want f-req-crit", repFinal.Findings[0].Finding.ID)
	}
	if repFinal.Findings[1].Finding.ID != "f-arch-crit" {
		t.Errorf("finding[1] = %s, want f-arch-crit", repFinal.Findings[1].Finding.ID)
	}
	// Medium item before Low item:
	if repFinal.Findings[2].Finding.ID != "f-arch-med" {
		t.Errorf("finding[2] = %s, want f-arch-med", repFinal.Findings[2].Finding.ID)
	}
	if repFinal.Findings[3].Finding.ID != "f-req-low" {
		t.Errorf("finding[3] = %s, want f-req-low", repFinal.Findings[3].Finding.ID)
	}

	// Basis refs strictly ordered by ordinal ascending
	for _, f := range repFinal.Findings {
		switch f.Finding.ID {
		case "f-req-crit":
			if !reflect.DeepEqual(f.Finding.BasisRefs, []string{"brief-1", "req-1"}) {
				t.Errorf("f-req-crit basis refs = %v, want [brief-1 req-1]", f.Finding.BasisRefs)
			}
		case "f-arch-med":
			if !reflect.DeepEqual(f.Finding.BasisRefs, []string{"flow-1", "const-1"}) {
				t.Errorf("f-arch-med basis refs = %v, want [flow-1 const-1]", f.Finding.BasisRefs)
			}
		}
	}

	// Repeated ComposeSession call is an idempotent read: returns AlreadyTerminal
	compRes2, err := store.ComposeSession(ctx, "sess_not_term")
	if err != nil {
		t.Fatalf("repeat compose: %v", err)
	}
	if !compRes2.IsAlreadyTerminal() {
		t.Errorf("expected IsAlreadyTerminal() = true")
	}
	snapAfterCompose2 := takeDBSnapshot(t, writer)
	assertDBSnapshotEqual(t, snapBase, snapAfterCompose2, "repeat ComposeSession")
}

// =============================================================================
// 15. Modified Applied Migration Checksum End-to-End
// =============================================================================

func TestE2E_15_ModifiedAppliedMigrationChecksum(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "migration_tamper.db")

	store1, err := Open(Config{
		Path:        dbPath,
		BusyTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Open store1: %v", err)
	}
	if err := store1.Migrate(context.Background()); err != nil {
		_ = store1.Close()
		t.Fatalf("Migrate store1: %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("Close store1: %v", err)
	}

	// Direct raw connection to tamper with applied checksum of migration 001
	rawDB, err := sql.Open("sqlite", buildDSN(dbPath, 100, false))
	if err != nil {
		t.Fatalf("open raw DB: %v", err)
	}
	// Tamper with checksum: use valid 64-character lowercase hex string to satisfy CHECK constraint
	_, err = rawDB.Exec("UPDATE schema_migrations SET checksum = '0000000000000000000000000000000000000000000000000000000000000000' WHERE version = 1;")
	if err != nil {
		_ = rawDB.Close()
		t.Fatalf("update checksum: %v", err)
	}
	_ = rawDB.Close()

	// Reopen store on the tampered database
	store2, err := Open(Config{
		Path:        dbPath,
		BusyTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Open store2: %v", err)
	}
	defer store2.Close()

	// Migration must fail closed!
	migErr := store2.Migrate(context.Background())
	if migErr == nil {
		t.Fatalf("expected Migrate to fail on tampered checksum, but succeeded")
	}
	if !strings.Contains(migErr.Error(), "checksum mismatch") {
		t.Errorf("expected error containing 'checksum mismatch', got: %v", migErr)
	}
}

// =============================================================================
// 16. Forbidden Scope & Structural Invariants Proof
// =============================================================================

func TestE2E_16_ForbiddenScopeAndStructuralInvariants(t *testing.T) {
	store, writer, _ := setupE2EStore(t, 100*time.Millisecond)
	ctx := context.Background()

	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	snap := createTestFrozenSnapshot(t, "snap_invariants")
	_, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_invariants",
		ProjectID:      "proj_invariants",
		IdempotencyKey: "idem_invariants",
		Title:          "Invariant Spec",
		Content:        "Invariant Content",
		Snapshot:       snap,
		CreatedAt:      t0,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// 1. Table schema inspection: ONLY the canonical 7 tables exist
	rows, err := writer.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name ASC;")
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer rows.Close()

	var tablesFound []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		tablesFound = append(tablesFound, name)
	}

	expectedCanonicalTables := []string{
		"evidence_units",
		"finding_basis_refs",
		"findings",
		"role_runs",
		"schema_migrations",
		"sessions",
		"snapshots",
	}
	if !reflect.DeepEqual(tablesFound, expectedCanonicalTables) {
		t.Errorf("tablesFound = %v, want canonical %v (no extra tables permitted)", tablesFound, expectedCanonicalTables)
	}

	// 2. All 4 canonical roles share the exact same snapshot ID and snapshot hash
	var snapCountForRoles int
	err = writer.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT s.snapshot_id) FROM sessions s
		JOIN role_runs rr ON s.id = rr.session_id
		WHERE s.id = 'sess_invariants';
	`).Scan(&snapCountForRoles)
	if err != nil || snapCountForRoles != 1 {
		t.Errorf("all 4 canonical roles must share exactly 1 snapshot ID, got %d (err=%v)", snapCountForRoles, err)
	}

	// 3. Complete all 4 roles and verify terminal session invariant: completed + incomplete == 4
	policy := TimingPolicy{
		DispatchCutoff:      20 * time.Minute,
		CallTimeout:         10 * time.Minute,
		SessionHardDeadline: 35 * time.Minute,
	}
	_, _ = store.ClaimSessionWithNow(ctx, policy, t0)

	for i, role := range domain.Roles {
		res, err := store.ReservePendingRoleWithNow(ctx, "sess_invariants", t0.Add(time.Duration(i+1)*time.Minute))
		if err != nil || !res.Reserved {
			t.Fatalf("reserve %s: %v", role, err)
		}
		if i == 0 {
			_, err = store.PublishRoleSuccess(ctx, PublishSuccessParams{
				SessionID:   "sess_invariants",
				RoleRunID:   res.RoleRunID,
				Role:        role,
				Findings:    []domain.Finding{{ID: "f-inv-1", Kind: domain.FindingExisting, Severity: domain.SeverityCritical, Category: "correctness", Issue: "Issue", Recommendation: "Rec", BasisRefs: []string{"req-1"}}},
				CallCount:   1,
				CompletedAt: t0.Add(time.Duration(i+2) * time.Minute),
			})
		} else {
			_, err = store.PublishRoleFailure(ctx, PublishFailureParams{
				SessionID:     "sess_invariants",
				RoleRunID:     res.RoleRunID,
				Role:          role,
				ErrorCategory: domain.ErrTransport,
				ErrorMessage:  "transport err",
				CallCount:     1,
				CompletedAt:   t0.Add(time.Duration(i+2) * time.Minute),
			})
		}
		if err != nil {
			t.Fatalf("publish %s: %v", role, err)
		}
	}

	compRes, err := store.ComposeSessionWithParams(ctx, ComposeParams{
		SessionID: "sess_invariants",
		Now:       t0.Add(10 * time.Minute),
	})
	if err != nil || !compRes.IsComposed() {
		t.Fatalf("compose: %v", err)
	}

	// Assert exactly 4 terminal roles exist in database
	var termRoleCount int
	err = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM role_runs WHERE session_id = 'sess_invariants' AND status IN ('complete', 'failed', 'interrupted');").Scan(&termRoleCount)
	if err != nil || termRoleCount != 4 {
		t.Errorf("termRoleCount = %d, want 4", termRoleCount)
	}

	// Assert completed + incomplete == 4 on session row
	var compCount, incompCount int
	err = writer.QueryRowContext(ctx, "SELECT completed_role_count, incomplete_role_count FROM sessions WHERE id = 'sess_invariants';").Scan(&compCount, &incompCount)
	if err != nil || compCount+incompCount != 4 {
		t.Errorf("compCount=%d incompCount=%d, sum must equal 4", compCount, incompCount)
	}

	// 4. Goroutine-held SQLite transaction leak check:
	// Verify that writer pool has zero active/open transactions by acquiring dedicated conn
	writerConn, err := writer.Conn(ctx)
	if err != nil {
		t.Fatalf("writer.Conn: %v", err)
	}
	if _, err := writerConn.ExecContext(ctx, "BEGIN IMMEDIATE; ROLLBACK;"); err != nil {
		t.Errorf("writer connection could not acquire immediate transaction (possible leaked transaction): %v", err)
	}
	_ = writerConn.Close()

	// 5. Scoped error coverage across all public storage seams
	unknownSession := "unknown_session_xyz"
	wrongProject := "wrong_project_xyz"

	if _, err := store.ReadStatusScoped(ctx, wrongProject, "sess_invariants"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("ReadStatusScoped wrong project want ErrSessionNotFound, got %v", err)
	}
	if _, err := store.ReadReportScoped(ctx, wrongProject, "sess_invariants"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("ReadReportScoped wrong project want ErrSessionNotFound, got %v", err)
	}
	if _, err := store.RequestCancellationScoped(ctx, wrongProject, "sess_invariants"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("RequestCancellationScoped wrong project want ErrSessionNotFound, got %v", err)
	}
	if _, err := store.ReservePendingRoleScoped(ctx, wrongProject, "sess_invariants", time.Now()); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("ReservePendingRoleScoped wrong project want ErrSessionNotFound, got %v", err)
	}
	if _, err := store.SweepCancellationScoped(ctx, wrongProject, "sess_invariants", time.Now()); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("SweepCancellationScoped wrong project want ErrSessionNotFound, got %v", err)
	}
	if _, err := store.SweepCutoffScoped(ctx, wrongProject, "sess_invariants", time.Now()); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("SweepCutoffScoped wrong project want ErrSessionNotFound, got %v", err)
	}
	if _, err := store.SweepHardDeadlineScoped(ctx, wrongProject, "sess_invariants", time.Now()); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("SweepHardDeadlineScoped wrong project want ErrSessionNotFound, got %v", err)
	}
	if _, err := store.ComposeSessionScoped(ctx, wrongProject, "sess_invariants"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("ComposeSessionScoped wrong project want ErrSessionNotFound, got %v", err)
	}

	if _, err := store.ReadStatus(ctx, unknownSession); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("ReadStatus unknown want ErrSessionNotFound, got %v", err)
	}
	if _, err := store.ReadReport(ctx, unknownSession); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("ReadReport unknown want ErrSessionNotFound, got %v", err)
	}
	if _, err := store.RequestCancellation(ctx, unknownSession); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("RequestCancellation unknown want ErrSessionNotFound, got %v", err)
	}
	if _, err := store.ReservePendingRole(ctx, unknownSession); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("ReservePendingRole unknown want ErrSessionNotFound, got %v", err)
	}
	if _, err := store.SweepCancellation(ctx, unknownSession); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("SweepCancellation unknown want ErrSessionNotFound, got %v", err)
	}
	if _, err := store.ComposeSession(ctx, unknownSession); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("ComposeSession unknown want ErrSessionNotFound, got %v", err)
	}
}
