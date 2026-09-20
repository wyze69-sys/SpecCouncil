package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	testValidHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func openGuardTestStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	store, _ := setupTestStore(t, 100*time.Millisecond)
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}
	writer, err := store.writerDB()
	if err != nil {
		t.Fatalf("writerDB: %v", err)
	}
	return store, writer
}

func insertSnapshotAndUnit(t *testing.T, writer *sql.DB, snapID, unitID string) {
	t.Helper()
	ctx := context.Background()
	_, err := writer.ExecContext(ctx, `INSERT INTO snapshots (id, hash, project_id, title, content, normalization_version, created_at)
		VALUES (?, ?, 'proj1', 'Title', 'Content', 1, '2026-09-19T12:00:00Z');`, snapID, testValidHash)
	if err != nil {
		t.Fatalf("insert snapshot %s: %v", snapID, err)
	}

	_, err = writer.ExecContext(ctx, `INSERT INTO evidence_units (id, snapshot_id, unit_id, ordinal, kind, text)
		VALUES (?, ?, ?, 0, 'brief', 'Brief text');`, unitID, snapID, unitID)
	if err != nil {
		t.Fatalf("insert evidence unit %s: %v", unitID, err)
	}
}

func insertSession(t *testing.T, writer *sql.DB, sessID, snapID, status string) {
	t.Helper()
	ctx := context.Background()
	var q string
	switch status {
	case "queued":
		q = fmt.Sprintf(`INSERT INTO sessions (id, project_id, idempotency_key, request_hash, snapshot_id, status, created_at)
			VALUES ('%s', 'proj1', 'idem-%s', '%s', '%s', 'queued', '2026-09-19T12:00:00Z');`, sessID, sessID, testValidHash, snapID)
	case "reviewing":
		q = fmt.Sprintf(`INSERT INTO sessions (id, project_id, idempotency_key, request_hash, snapshot_id, status, created_at, claimed_at, dispatch_cutoff_at, hard_deadline_at)
			VALUES ('%s', 'proj1', 'idem-%s', '%s', '%s', 'reviewing', '2026-09-19T12:00:00Z', '2026-09-19T12:01:00Z', '2026-09-19T12:02:00Z', '2026-09-19T12:05:00Z');`, sessID, sessID, testValidHash, snapID)
	case "complete":
		q = fmt.Sprintf(`INSERT INTO sessions (id, project_id, idempotency_key, request_hash, snapshot_id, status, created_at, claimed_at, dispatch_cutoff_at, hard_deadline_at, terminal_at, terminal_reason, completed_role_count, incomplete_role_count)
			VALUES ('%s', 'proj1', 'idem-%s', '%s', '%s', 'complete', '2026-09-19T12:00:00Z', '2026-09-19T12:01:00Z', '2026-09-19T12:02:00Z', '2026-09-19T12:05:00Z', '2026-09-19T12:03:00Z', 'all_roles_complete', 4, 0);`, sessID, sessID, testValidHash, snapID)
	case "partial":
		q = fmt.Sprintf(`INSERT INTO sessions (id, project_id, idempotency_key, request_hash, snapshot_id, status, created_at, claimed_at, dispatch_cutoff_at, hard_deadline_at, terminal_at, terminal_reason, completed_role_count, incomplete_role_count)
			VALUES ('%s', 'proj1', 'idem-%s', '%s', '%s', 'partial', '2026-09-19T12:00:00Z', '2026-09-19T12:01:00Z', '2026-09-19T12:02:00Z', '2026-09-19T12:05:00Z', '2026-09-19T12:03:00Z', 'user_cancelled', 0, 4);`, sessID, sessID, testValidHash, snapID)
	case "failed":
		q = fmt.Sprintf(`INSERT INTO sessions (id, project_id, idempotency_key, request_hash, snapshot_id, status, created_at, claimed_at, dispatch_cutoff_at, hard_deadline_at, terminal_at, terminal_reason, completed_role_count, incomplete_role_count)
			VALUES ('%s', 'proj1', 'idem-%s', '%s', '%s', 'failed', '2026-09-19T12:00:00Z', '2026-09-19T12:01:00Z', '2026-09-19T12:02:00Z', '2026-09-19T12:05:00Z', '2026-09-19T12:03:00Z', 'role_failures', 0, 4);`, sessID, sessID, testValidHash, snapID)
	default:
		t.Fatalf("unsupported session status for fixture: %s", status)
	}
	if _, err := writer.ExecContext(ctx, q); err != nil {
		t.Fatalf("insert session %s (%s): %v", sessID, status, err)
	}
}

func insertRoleRun(t *testing.T, writer *sql.DB, roleID, sessID, role, status string) {
	t.Helper()
	ctx := context.Background()
	var q string
	switch status {
	case "pending":
		q = fmt.Sprintf(`INSERT INTO role_runs (id, session_id, role, status, created_at)
			VALUES ('%s', '%s', '%s', 'pending', '2026-09-19T12:00:00Z');`, roleID, sessID, role)
	case "in_flight":
		q = fmt.Sprintf(`INSERT INTO role_runs (id, session_id, role, status, started_at, created_at)
			VALUES ('%s', '%s', '%s', 'in_flight', '2026-09-19T12:01:00Z', '2026-09-19T12:00:00Z');`, roleID, sessID, role)
	case "complete":
		q = fmt.Sprintf(`INSERT INTO role_runs (id, session_id, role, status, started_at, completed_at, call_count, created_at)
			VALUES ('%s', '%s', '%s', 'complete', '2026-09-19T12:01:00Z', '2026-09-19T12:02:00Z', 1, '2026-09-19T12:00:00Z');`, roleID, sessID, role)
	case "failed":
		q = fmt.Sprintf(`INSERT INTO role_runs (id, session_id, role, status, completed_at, error_category, created_at)
			VALUES ('%s', '%s', '%s', 'failed', '2026-09-19T12:02:00Z', 'transport', '2026-09-19T12:00:00Z');`, roleID, sessID, role)
	case "interrupted":
		q = fmt.Sprintf(`INSERT INTO role_runs (id, session_id, role, status, completed_at, cause, created_at)
			VALUES ('%s', '%s', '%s', 'interrupted', '2026-09-19T12:02:00Z', 'user_cancelled', '2026-09-19T12:00:00Z');`, roleID, sessID, role)
	default:
		t.Fatalf("unsupported role status for fixture: %s", status)
	}
	if _, err := writer.ExecContext(ctx, q); err != nil {
		t.Fatalf("insert role %s (%s): %v", roleID, status, err)
	}
}

// 1. Role State Transitions: Legal Transitions
func TestStateGuards_RoleTransitions_Legal(t *testing.T) {
	_, writer := openGuardTestStore(t)
	ctx := context.Background()

	insertSnapshotAndUnit(t, writer, "snap_role_legal", "eu_role_legal")
	insertSession(t, writer, "sess_role_legal", "snap_role_legal", "reviewing")

	t.Run("pending to in_flight", func(t *testing.T) {
		insertRoleRun(t, writer, "rr_p_inf", "sess_role_legal", "requirements", "pending")
		_, err := writer.ExecContext(ctx, `UPDATE role_runs SET status = 'in_flight', started_at = '2026-09-19T12:01:00Z' WHERE id = 'rr_p_inf';`)
		if err != nil {
			t.Fatalf("legal transition pending -> in_flight failed: %v", err)
		}
	})

	t.Run("pending to interrupted (user_cancelled)", func(t *testing.T) {
		insertRoleRun(t, writer, "rr_p_int_uc", "sess_role_legal", "architecture", "pending")
		_, err := writer.ExecContext(ctx, `UPDATE role_runs SET status = 'interrupted', completed_at = '2026-09-19T12:01:00Z', cause = 'user_cancelled' WHERE id = 'rr_p_int_uc';`)
		if err != nil {
			t.Fatalf("legal transition pending -> interrupted (user_cancelled) failed: %v", err)
		}
	})

	t.Run("pending to interrupted (deadline_cutoff)", func(t *testing.T) {
		insertRoleRun(t, writer, "rr_p_int_dc", "sess_role_legal", "qa", "pending")
		_, err := writer.ExecContext(ctx, `UPDATE role_runs SET status = 'interrupted', completed_at = '2026-09-19T12:01:00Z', cause = 'deadline_cutoff' WHERE id = 'rr_p_int_dc';`)
		if err != nil {
			t.Fatalf("legal transition pending -> interrupted (deadline_cutoff) failed: %v", err)
		}
	})

	t.Run("pending to interrupted (process_restart)", func(t *testing.T) {
		insertRoleRun(t, writer, "rr_p_int_pr", "sess_role_legal", "security", "pending")
		_, err := writer.ExecContext(ctx, `UPDATE role_runs SET status = 'interrupted', completed_at = '2026-09-19T12:01:00Z', cause = 'process_restart' WHERE id = 'rr_p_int_pr';`)
		if err != nil {
			t.Fatalf("legal transition pending -> interrupted (process_restart) failed: %v", err)
		}
	})

	// New session for in_flight transitions
	insertSession(t, writer, "sess_inf_legal", "snap_role_legal", "reviewing")

	t.Run("in_flight to complete", func(t *testing.T) {
		insertRoleRun(t, writer, "rr_inf_comp", "sess_inf_legal", "requirements", "in_flight")
		_, err := writer.ExecContext(ctx, `UPDATE role_runs SET status = 'complete', completed_at = '2026-09-19T12:02:00Z', call_count = 1 WHERE id = 'rr_inf_comp';`)
		if err != nil {
			t.Fatalf("legal transition in_flight -> complete failed: %v", err)
		}
	})

	t.Run("in_flight to failed", func(t *testing.T) {
		insertRoleRun(t, writer, "rr_inf_fail", "sess_inf_legal", "architecture", "in_flight")
		_, err := writer.ExecContext(ctx, `UPDATE role_runs SET status = 'failed', completed_at = '2026-09-19T12:02:00Z', error_category = 'timeout' WHERE id = 'rr_inf_fail';`)
		if err != nil {
			t.Fatalf("legal transition in_flight -> failed failed: %v", err)
		}
	})

	t.Run("in_flight to interrupted (process_restart)", func(t *testing.T) {
		insertRoleRun(t, writer, "rr_inf_int", "sess_inf_legal", "qa", "in_flight")
		_, err := writer.ExecContext(ctx, `UPDATE role_runs SET status = 'interrupted', completed_at = '2026-09-19T12:02:00Z', cause = 'process_restart' WHERE id = 'rr_inf_int';`)
		if err != nil {
			t.Fatalf("legal transition in_flight -> interrupted failed: %v", err)
		}
	})
}

// 2. Role State Transitions: Illegal Transitions and Terminal Rewrites
func TestStateGuards_RoleTransitions_IllegalAndTerminalRewrites(t *testing.T) {
	_, writer := openGuardTestStore(t)
	ctx := context.Background()

	insertSnapshotAndUnit(t, writer, "snap_role_illegal", "eu_role_illegal")
	insertSession(t, writer, "sess_role_illegal", "snap_role_illegal", "reviewing")

	t.Run("pending to complete directly is rejected", func(t *testing.T) {
		insertRoleRun(t, writer, "rr_bad_p_comp", "sess_role_illegal", "requirements", "pending")
		_, err := writer.ExecContext(ctx, `UPDATE role_runs SET status = 'complete', started_at = '2026-09-19T12:01:00Z', completed_at = '2026-09-19T12:02:00Z', call_count = 1 WHERE id = 'rr_bad_p_comp';`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "illegal role run status transition") {
			t.Fatalf("expected illegal role status transition error, got: %v", err)
		}
	})

	t.Run("pending to failed directly is rejected", func(t *testing.T) {
		insertRoleRun(t, writer, "rr_bad_p_fail", "sess_role_illegal", "architecture", "pending")
		_, err := writer.ExecContext(ctx, `UPDATE role_runs SET status = 'failed', completed_at = '2026-09-19T12:02:00Z', error_category = 'timeout' WHERE id = 'rr_bad_p_fail';`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "illegal role run status transition") {
			t.Fatalf("expected illegal role status transition error, got: %v", err)
		}
	})

	t.Run("in_flight to pending rewind is rejected", func(t *testing.T) {
		insertRoleRun(t, writer, "rr_bad_inf_p", "sess_role_illegal", "qa", "in_flight")
		_, err := writer.ExecContext(ctx, `UPDATE role_runs SET status = 'pending', started_at = NULL WHERE id = 'rr_bad_inf_p';`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "illegal role run status transition") {
			t.Fatalf("expected illegal role status transition error, got: %v", err)
		}
	})

	// Terminal rewrites
	insertSession(t, writer, "sess_term_roles", "snap_role_illegal", "reviewing")
	insertRoleRun(t, writer, "rr_term_comp", "sess_term_roles", "requirements", "complete")
	insertRoleRun(t, writer, "rr_term_fail", "sess_term_roles", "architecture", "failed")
	insertRoleRun(t, writer, "rr_term_int", "sess_term_roles", "qa", "interrupted")

	t.Run("complete role cannot change status", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `UPDATE role_runs SET status = 'in_flight' WHERE id = 'rr_term_comp';`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "terminal role run cannot be modified") {
			t.Fatalf("expected terminal role run cannot be modified error, got: %v", err)
		}
	})

	t.Run("complete role cannot be rewritten with same status", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `UPDATE role_runs SET call_count = 2 WHERE id = 'rr_term_comp';`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "terminal role run cannot be modified") {
			t.Fatalf("expected terminal role run cannot be modified error, got: %v", err)
		}
	})

	t.Run("failed role cannot change status", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `UPDATE role_runs SET status = 'in_flight', started_at = '2026-09-19T12:01:00Z' WHERE id = 'rr_term_fail';`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "terminal role run cannot be modified") {
			t.Fatalf("expected terminal role run cannot be modified error, got: %v", err)
		}
	})

	t.Run("failed role cannot rewrite error message", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `UPDATE role_runs SET error_message = 'New error' WHERE id = 'rr_term_fail';`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "terminal role run cannot be modified") {
			t.Fatalf("expected terminal role run cannot be modified error, got: %v", err)
		}
	})

	t.Run("interrupted role cannot change status", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `UPDATE role_runs SET status = 'in_flight', started_at = '2026-09-19T12:01:00Z' WHERE id = 'rr_term_int';`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "terminal role run cannot be modified") {
			t.Fatalf("expected terminal role run cannot be modified error, got: %v", err)
		}
	})

	t.Run("interrupted role cannot rewrite cause", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `UPDATE role_runs SET cause = 'deadline_cutoff' WHERE id = 'rr_term_int';`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "terminal role run cannot be modified") {
			t.Fatalf("expected terminal role run cannot be modified error, got: %v", err)
		}
	})

	// Invalid transition fields
	insertSession(t, writer, "sess_fields_guard", "snap_role_illegal", "reviewing")

	t.Run("in_flight to complete without call_count >= 1 rejected", func(t *testing.T) {
		insertRoleRun(t, writer, "rr_bad_f_comp", "sess_fields_guard", "requirements", "in_flight")
		_, err := writer.ExecContext(ctx, `UPDATE role_runs SET status = 'complete', completed_at = '2026-09-19T12:02:00Z', call_count = 0 WHERE id = 'rr_bad_f_comp';`)
		if err == nil {
			t.Fatalf("expected error transitioning to complete with call_count = 0")
		}
	})

	t.Run("in_flight to failed with cause rejected", func(t *testing.T) {
		insertRoleRun(t, writer, "rr_bad_f_fail", "sess_fields_guard", "architecture", "in_flight")
		_, err := writer.ExecContext(ctx, `UPDATE role_runs SET status = 'failed', completed_at = '2026-09-19T12:02:00Z', cause = 'user_cancelled', error_category = 'transport' WHERE id = 'rr_bad_f_fail';`)
		if err == nil {
			t.Fatalf("expected error transitioning to failed with cause set")
		}
	})

	t.Run("in_flight to interrupted with error_category rejected", func(t *testing.T) {
		insertRoleRun(t, writer, "rr_bad_f_int", "sess_fields_guard", "qa", "in_flight")
		_, err := writer.ExecContext(ctx, `UPDATE role_runs SET status = 'interrupted', completed_at = '2026-09-19T12:02:00Z', cause = 'user_cancelled', error_category = 'transport' WHERE id = 'rr_bad_f_int';`)
		if err == nil {
			t.Fatalf("expected error transitioning to interrupted with error_category set")
		}
	})
}

// 3. Session State Transitions: Legal Transitions
func TestStateGuards_SessionTransitions_Legal(t *testing.T) {
	_, writer := openGuardTestStore(t)
	ctx := context.Background()

	insertSnapshotAndUnit(t, writer, "snap_sess_legal", "eu_sess_legal")

	t.Run("queued to reviewing", func(t *testing.T) {
		insertSession(t, writer, "s_q_rev", "snap_sess_legal", "queued")
		_, err := writer.ExecContext(ctx, `UPDATE sessions SET status = 'reviewing',
			claimed_at = '2026-09-19T12:01:00Z',
			dispatch_cutoff_at = '2026-09-19T12:02:00Z',
			hard_deadline_at = '2026-09-19T12:05:00Z'
			WHERE id = 's_q_rev';`)
		if err != nil {
			t.Fatalf("legal transition queued -> reviewing failed: %v", err)
		}
	})

	t.Run("reviewing to complete", func(t *testing.T) {
		insertSession(t, writer, "s_rev_comp", "snap_sess_legal", "reviewing")
		_, err := writer.ExecContext(ctx, `UPDATE sessions SET status = 'complete',
			terminal_at = '2026-09-19T12:03:00Z',
			terminal_reason = 'all_roles_complete',
			completed_role_count = 4,
			incomplete_role_count = 0
			WHERE id = 's_rev_comp';`)
		if err != nil {
			t.Fatalf("legal transition reviewing -> complete failed: %v", err)
		}
	})

	t.Run("reviewing to partial (user_cancelled)", func(t *testing.T) {
		insertSession(t, writer, "s_rev_part_uc", "snap_sess_legal", "reviewing")
		_, err := writer.ExecContext(ctx, `UPDATE sessions SET status = 'partial',
			terminal_at = '2026-09-19T12:03:00Z',
			terminal_reason = 'user_cancelled',
			completed_role_count = 0,
			incomplete_role_count = 4
			WHERE id = 's_rev_part_uc';`)
		if err != nil {
			t.Fatalf("legal transition reviewing -> partial (user_cancelled) failed: %v", err)
		}
	})

	t.Run("reviewing to partial (deadline_cutoff with completed roles)", func(t *testing.T) {
		insertSession(t, writer, "s_rev_part_dc", "snap_sess_legal", "reviewing")
		_, err := writer.ExecContext(ctx, `UPDATE sessions SET status = 'partial',
			terminal_at = '2026-09-19T12:03:00Z',
			terminal_reason = 'deadline_cutoff',
			completed_role_count = 2,
			incomplete_role_count = 2
			WHERE id = 's_rev_part_dc';`)
		if err != nil {
			t.Fatalf("legal transition reviewing -> partial (deadline_cutoff) failed: %v", err)
		}
	})

	t.Run("reviewing to failed", func(t *testing.T) {
		insertSession(t, writer, "s_rev_fail", "snap_sess_legal", "reviewing")
		_, err := writer.ExecContext(ctx, `UPDATE sessions SET status = 'failed',
			terminal_at = '2026-09-19T12:03:00Z',
			terminal_reason = 'role_failures',
			completed_role_count = 0,
			incomplete_role_count = 4
			WHERE id = 's_rev_fail';`)
		if err != nil {
			t.Fatalf("legal transition reviewing -> failed failed: %v", err)
		}
	})
}

// 4. Session State Transitions: Illegal Transitions and Terminal Rewrites
func TestStateGuards_SessionTransitions_IllegalAndTerminalRewrites(t *testing.T) {
	_, writer := openGuardTestStore(t)
	ctx := context.Background()

	insertSnapshotAndUnit(t, writer, "snap_sess_illegal", "eu_sess_illegal")

	t.Run("queued directly to complete rejected", func(t *testing.T) {
		insertSession(t, writer, "s_bad_q_comp", "snap_sess_illegal", "queued")
		_, err := writer.ExecContext(ctx, `UPDATE sessions SET status = 'complete',
			claimed_at = '2026-09-19T12:01:00Z',
			dispatch_cutoff_at = '2026-09-19T12:02:00Z',
			hard_deadline_at = '2026-09-19T12:05:00Z',
			terminal_at = '2026-09-19T12:03:00Z',
			terminal_reason = 'all_roles_complete',
			completed_role_count = 4,
			incomplete_role_count = 0
			WHERE id = 's_bad_q_comp';`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "illegal session status transition") {
			t.Fatalf("expected illegal session status transition error, got: %v", err)
		}
	})

	t.Run("queued directly to partial rejected", func(t *testing.T) {
		insertSession(t, writer, "s_bad_q_part", "snap_sess_illegal", "queued")
		_, err := writer.ExecContext(ctx, `UPDATE sessions SET status = 'partial',
			claimed_at = '2026-09-19T12:01:00Z',
			dispatch_cutoff_at = '2026-09-19T12:02:00Z',
			hard_deadline_at = '2026-09-19T12:05:00Z',
			terminal_at = '2026-09-19T12:03:00Z',
			terminal_reason = 'user_cancelled',
			completed_role_count = 0,
			incomplete_role_count = 4
			WHERE id = 's_bad_q_part';`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "illegal session status transition") {
			t.Fatalf("expected illegal session status transition error, got: %v", err)
		}
	})

	t.Run("queued directly to failed rejected", func(t *testing.T) {
		insertSession(t, writer, "s_bad_q_fail", "snap_sess_illegal", "queued")
		_, err := writer.ExecContext(ctx, `UPDATE sessions SET status = 'failed',
			claimed_at = '2026-09-19T12:01:00Z',
			dispatch_cutoff_at = '2026-09-19T12:02:00Z',
			hard_deadline_at = '2026-09-19T12:05:00Z',
			terminal_at = '2026-09-19T12:03:00Z',
			terminal_reason = 'role_failures',
			completed_role_count = 0,
			incomplete_role_count = 4
			WHERE id = 's_bad_q_fail';`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "illegal session status transition") {
			t.Fatalf("expected illegal session status transition error, got: %v", err)
		}
	})

	t.Run("reviewing rewind to queued rejected", func(t *testing.T) {
		insertSession(t, writer, "s_bad_rev_q", "snap_sess_illegal", "reviewing")
		_, err := writer.ExecContext(ctx, `UPDATE sessions SET status = 'queued',
			claimed_at = NULL,
			dispatch_cutoff_at = NULL,
			hard_deadline_at = NULL
			WHERE id = 's_bad_rev_q';`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "illegal session status transition") {
			t.Fatalf("expected illegal session status transition error, got: %v", err)
		}
	})

	// Terminal rewrites
	insertSession(t, writer, "s_term_comp", "snap_sess_illegal", "complete")
	insertSession(t, writer, "s_term_part", "snap_sess_illegal", "partial")
	insertSession(t, writer, "s_term_fail", "snap_sess_illegal", "failed")

	t.Run("complete session cannot change status", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `UPDATE sessions SET status = 'reviewing' WHERE id = 's_term_comp';`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "terminal session cannot be modified") {
			t.Fatalf("expected terminal session cannot be modified error, got: %v", err)
		}
	})

	t.Run("complete session cannot modify terminal counts", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `UPDATE sessions SET completed_role_count = 3, incomplete_role_count = 1 WHERE id = 's_term_comp';`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "terminal session cannot be modified") {
			t.Fatalf("expected terminal session cannot be modified error, got: %v", err)
		}
	})

	t.Run("partial session cannot change status", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `UPDATE sessions SET status = 'complete' WHERE id = 's_term_part';`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "terminal session cannot be modified") {
			t.Fatalf("expected terminal session cannot be modified error, got: %v", err)
		}
	})

	t.Run("failed session cannot change status", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `UPDATE sessions SET status = 'reviewing' WHERE id = 's_term_fail';`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "terminal session cannot be modified") {
			t.Fatalf("expected terminal session cannot be modified error, got: %v", err)
		}
	})

	// Terminal composition validation on transition
	t.Run("reviewing to complete with completed_role_count < 4 rejected", func(t *testing.T) {
		insertSession(t, writer, "s_comp_cnt_bad", "snap_sess_illegal", "reviewing")
		_, err := writer.ExecContext(ctx, `UPDATE sessions SET status = 'complete',
			terminal_at = '2026-09-19T12:03:00Z',
			terminal_reason = 'all_roles_complete',
			completed_role_count = 3,
			incomplete_role_count = 1
			WHERE id = 's_comp_cnt_bad';`)
		if err == nil {
			t.Fatalf("expected error on transition to complete with completed_role_count < 4")
		}
	})

	t.Run("reviewing to complete with non-matching terminal_reason rejected", func(t *testing.T) {
		insertSession(t, writer, "s_comp_reas_bad", "snap_sess_illegal", "reviewing")
		_, err := writer.ExecContext(ctx, `UPDATE sessions SET status = 'complete',
			terminal_at = '2026-09-19T12:03:00Z',
			terminal_reason = 'user_cancelled',
			completed_role_count = 4,
			incomplete_role_count = 0
			WHERE id = 's_comp_reas_bad';`)
		if err == nil {
			t.Fatalf("expected error on transition to complete with terminal_reason != all_roles_complete")
		}
	})

	t.Run("reviewing to failed with completed_role_count > 0 rejected", func(t *testing.T) {
		insertSession(t, writer, "s_fail_cnt_bad", "snap_sess_illegal", "reviewing")
		_, err := writer.ExecContext(ctx, `UPDATE sessions SET status = 'failed',
			terminal_at = '2026-09-19T12:03:00Z',
			terminal_reason = 'role_failures',
			completed_role_count = 1,
			incomplete_role_count = 3
			WHERE id = 's_fail_cnt_bad';`)
		if err == nil {
			t.Fatalf("expected error on transition to failed with completed_role_count > 0")
		}
	})
}

// 5. Cancellation Monotonicity
func TestStateGuards_CancellationMonotonicity(t *testing.T) {
	_, writer := openGuardTestStore(t)
	ctx := context.Background()

	insertSnapshotAndUnit(t, writer, "snap_cancel", "eu_cancel")

	t.Run("queued cancel_requested 0 to 1 succeeds", func(t *testing.T) {
		insertSession(t, writer, "s_c_q", "snap_cancel", "queued")
		_, err := writer.ExecContext(ctx, `UPDATE sessions SET cancel_requested = 1 WHERE id = 's_c_q';`)
		if err != nil {
			t.Fatalf("0 -> 1 cancel_requested in queued failed: %v", err)
		}
	})

	t.Run("queued cancel_requested 1 to 1 succeeds (idempotent)", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `UPDATE sessions SET cancel_requested = 1 WHERE id = 's_c_q';`)
		if err != nil {
			t.Fatalf("1 -> 1 cancel_requested failed: %v", err)
		}
	})

	t.Run("queued cancel_requested 1 to 0 fails", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `UPDATE sessions SET cancel_requested = 0 WHERE id = 's_c_q';`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "cancel_requested is monotonic") {
			t.Fatalf("expected cancel_requested is monotonic error, got: %v", err)
		}
	})

	t.Run("reviewing cancel_requested 0 to 1 succeeds", func(t *testing.T) {
		insertSession(t, writer, "s_c_rev", "snap_cancel", "reviewing")
		_, err := writer.ExecContext(ctx, `UPDATE sessions SET cancel_requested = 1 WHERE id = 's_c_rev';`)
		if err != nil {
			t.Fatalf("0 -> 1 cancel_requested in reviewing failed: %v", err)
		}
	})

	t.Run("reviewing cancel_requested 1 to 0 fails", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `UPDATE sessions SET cancel_requested = 0 WHERE id = 's_c_rev';`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "cancel_requested is monotonic") {
			t.Fatalf("expected cancel_requested is monotonic error, got: %v", err)
		}
	})
}

// 6. Immutability Guards: Snapshots, Evidence Units, Findings, Basis Refs, Immutable Entity Fields
func TestStateGuards_Immutability(t *testing.T) {
	_, writer := openGuardTestStore(t)
	ctx := context.Background()

	insertSnapshotAndUnit(t, writer, "snap_imm", "eu_imm")
	insertSession(t, writer, "sess_imm", "snap_imm", "reviewing")
	insertRoleRun(t, writer, "rr_imm", "sess_imm", "requirements", "in_flight")

	_, err := writer.ExecContext(ctx, `INSERT INTO findings (id, role_run_id, finding_id, severity, category, issue, recommendation, created_at)
		VALUES ('f_imm', 'rr_imm', 'find-1', 'high', 'security', 'issue text', 'rec text', '2026-09-19T12:00:00Z');`)
	if err != nil {
		t.Fatalf("insert finding: %v", err)
	}

	_, err = writer.ExecContext(ctx, `INSERT INTO finding_basis_refs (id, finding_id, evidence_unit_id, ordinal)
		VALUES ('fbr_imm', 'f_imm', 'eu_imm', 1);`)
	if err != nil {
		t.Fatalf("insert basis ref: %v", err)
	}

	t.Run("snapshots update rejected", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `UPDATE snapshots SET title = 'Updated' WHERE id = 'snap_imm';`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "snapshots are immutable") {
			t.Fatalf("expected snapshots are immutable, got: %v", err)
		}
	})

	t.Run("snapshots delete rejected", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `DELETE FROM snapshots WHERE id = 'snap_imm';`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "snapshots are immutable") {
			t.Fatalf("expected snapshots are immutable, got: %v", err)
		}
	})

	t.Run("evidence units update rejected", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `UPDATE evidence_units SET text = 'Updated text' WHERE id = 'eu_imm';`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "evidence units are immutable") {
			t.Fatalf("expected evidence units are immutable, got: %v", err)
		}
	})

	t.Run("evidence units delete rejected", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `DELETE FROM evidence_units WHERE id = 'eu_imm';`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "evidence units are immutable") {
			t.Fatalf("expected evidence units are immutable, got: %v", err)
		}
	})

	t.Run("findings update rejected", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `UPDATE findings SET issue = 'Updated issue' WHERE id = 'f_imm';`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "findings are immutable") {
			t.Fatalf("expected findings are immutable, got: %v", err)
		}
	})

	t.Run("findings delete rejected", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `DELETE FROM findings WHERE id = 'f_imm';`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "findings are immutable") {
			t.Fatalf("expected findings are immutable, got: %v", err)
		}
	})

	t.Run("finding basis refs update rejected", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `UPDATE finding_basis_refs SET ordinal = 2 WHERE id = 'fbr_imm';`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "finding basis references are immutable") {
			t.Fatalf("expected finding basis references are immutable, got: %v", err)
		}
	})

	t.Run("finding basis refs delete rejected", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `DELETE FROM finding_basis_refs WHERE id = 'fbr_imm';`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "finding basis references are immutable") {
			t.Fatalf("expected finding basis references are immutable, got: %v", err)
		}
	})

	t.Run("sessions immutable identity/hash fields rejected", func(t *testing.T) {
		illegalSessionUpdates := []struct {
			name string
			sql  string
		}{
			{"id", "UPDATE sessions SET id = 'new_id' WHERE id = 'sess_imm';"},
			{"project_id", "UPDATE sessions SET project_id = 'new_p' WHERE id = 'sess_imm';"},
			{"idempotency_key", "UPDATE sessions SET idempotency_key = 'new_k' WHERE id = 'sess_imm';"},
			{"request_hash", "UPDATE sessions SET request_hash = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' WHERE id = 'sess_imm';"},
			{"snapshot_id", "UPDATE sessions SET snapshot_id = 'new_snap' WHERE id = 'sess_imm';"},
			{"created_at", "UPDATE sessions SET created_at = '2026-09-19T11:00:00Z' WHERE id = 'sess_imm';"},
		}
		for _, u := range illegalSessionUpdates {
			t.Run(u.name, func(t *testing.T) {
				_, err := writer.ExecContext(ctx, u.sql)
				if err == nil || !strings.Contains(strings.ToLower(err.Error()), "immutable") {
					t.Fatalf("expected immutable field error modifying %s, got: %v", u.name, err)
				}
			})
		}
	})

	t.Run("role_runs immutable identity fields rejected", func(t *testing.T) {
		illegalRoleUpdates := []struct {
			name string
			sql  string
		}{
			{"id", "UPDATE role_runs SET id = 'new_rr' WHERE id = 'rr_imm';"},
			{"session_id", "UPDATE role_runs SET session_id = 'new_sess' WHERE id = 'rr_imm';"},
			{"role", "UPDATE role_runs SET role = 'architecture' WHERE id = 'rr_imm';"},
			{"created_at", "UPDATE role_runs SET created_at = '2026-09-19T11:00:00Z' WHERE id = 'rr_imm';"},
		}
		for _, u := range illegalRoleUpdates {
			t.Run(u.name, func(t *testing.T) {
				_, err := writer.ExecContext(ctx, u.sql)
				if err == nil || !strings.Contains(strings.ToLower(err.Error()), "immutable") {
					t.Fatalf("expected immutable field error modifying %s, got: %v", u.name, err)
				}
			})
		}
	})
}

// 7. Allowed Mutable Fields
func TestStateGuards_AllowedMutableFields(t *testing.T) {
	_, writer := openGuardTestStore(t)
	ctx := context.Background()

	insertSnapshotAndUnit(t, writer, "snap_mutable", "eu_mutable")

	t.Run("session cancel_requested can mutate in queued", func(t *testing.T) {
		insertSession(t, writer, "s_mut_q", "snap_mutable", "queued")
		_, err := writer.ExecContext(ctx, `UPDATE sessions SET cancel_requested = 1 WHERE id = 's_mut_q';`)
		if err != nil {
			t.Fatalf("allowed mutation cancel_requested failed: %v", err)
		}
	})

	t.Run("session cancel_requested can mutate in reviewing", func(t *testing.T) {
		insertSession(t, writer, "s_mut_rev", "snap_mutable", "reviewing")
		_, err := writer.ExecContext(ctx, `UPDATE sessions SET cancel_requested = 1 WHERE id = 's_mut_rev';`)
		if err != nil {
			t.Fatalf("allowed mutation cancel_requested in reviewing failed: %v", err)
		}
	})

	t.Run("role_run call_count and started_at can mutate while in_flight", func(t *testing.T) {
		insertSession(t, writer, "s_mut_rr", "snap_mutable", "reviewing")
		insertRoleRun(t, writer, "rr_mut", "s_mut_rr", "requirements", "in_flight")

		_, err := writer.ExecContext(ctx, `UPDATE role_runs SET call_count = 1 WHERE id = 'rr_mut';`)
		if err != nil {
			t.Fatalf("allowed mutation call_count = 1 failed: %v", err)
		}

		_, err = writer.ExecContext(ctx, `UPDATE role_runs SET call_count = 2 WHERE id = 'rr_mut';`)
		if err != nil {
			t.Fatalf("allowed mutation call_count = 2 failed: %v", err)
		}
	})
}

// 8. Citation Integrity: Cross-Snapshot Citations Rejected, Same-Snapshot Citations Accepted
func TestStateGuards_CitationIntegrity(t *testing.T) {
	_, writer := openGuardTestStore(t)
	ctx := context.Background()

	// Snapshot A
	insertSnapshotAndUnit(t, writer, "snap_cite_A", "eu_cite_A1")
	insertSession(t, writer, "sess_cite_A", "snap_cite_A", "reviewing")
	insertRoleRun(t, writer, "rr_cite_A", "sess_cite_A", "requirements", "in_flight")

	_, err := writer.ExecContext(ctx, `INSERT INTO findings (id, role_run_id, finding_id, severity, category, issue, recommendation, created_at)
		VALUES ('f_cite_A', 'rr_cite_A', 'find-A', 'high', 'qa', 'issue A', 'rec A', '2026-09-19T12:00:00Z');`)
	if err != nil {
		t.Fatalf("insert finding A: %v", err)
	}

	// Snapshot B
	insertSnapshotAndUnit(t, writer, "snap_cite_B", "eu_cite_B1")
	insertSession(t, writer, "sess_cite_B", "snap_cite_B", "reviewing")
	insertRoleRun(t, writer, "rr_cite_B", "sess_cite_B", "security", "in_flight")

	_, err = writer.ExecContext(ctx, `INSERT INTO findings (id, role_run_id, finding_id, severity, category, issue, recommendation, created_at)
		VALUES ('f_cite_B', 'rr_cite_B', 'find-B', 'medium', 'security', 'issue B', 'rec B', '2026-09-19T12:00:00Z');`)
	if err != nil {
		t.Fatalf("insert finding B: %v", err)
	}

	t.Run("same-snapshot citation succeeds", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `INSERT INTO finding_basis_refs (id, finding_id, evidence_unit_id, ordinal)
			VALUES ('fbr_valid_A', 'f_cite_A', 'eu_cite_A1', 1);`)
		if err != nil {
			t.Fatalf("same-snapshot citation failed: %v", err)
		}

		_, err = writer.ExecContext(ctx, `INSERT INTO finding_basis_refs (id, finding_id, evidence_unit_id, ordinal)
			VALUES ('fbr_valid_B', 'f_cite_B', 'eu_cite_B1', 1);`)
		if err != nil {
			t.Fatalf("same-snapshot citation B failed: %v", err)
		}
	})

	t.Run("cross-snapshot citation from finding A to unit B1 rejected", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `INSERT INTO finding_basis_refs (id, finding_id, evidence_unit_id, ordinal)
			VALUES ('fbr_cross_1', 'f_cite_A', 'eu_cite_B1', 2);`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "cross-snapshot evidence citation rejected") {
			t.Fatalf("expected cross-snapshot citation rejected error, got: %v", err)
		}
	})

	t.Run("cross-snapshot citation from finding B to unit A1 rejected", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `INSERT INTO finding_basis_refs (id, finding_id, evidence_unit_id, ordinal)
			VALUES ('fbr_cross_2', 'f_cite_B', 'eu_cite_A1', 2);`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "cross-snapshot evidence citation rejected") {
			t.Fatalf("expected cross-snapshot citation rejected error, got: %v", err)
		}
	})

	t.Run("missing finding rejected via foreign key", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `INSERT INTO finding_basis_refs (id, finding_id, evidence_unit_id, ordinal)
			VALUES ('fbr_missing_f', 'nonexistent_finding', 'eu_cite_A1', 1);`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
			t.Fatalf("expected foreign key error on missing finding, got: %v", err)
		}
	})

	t.Run("missing evidence unit rejected via foreign key", func(t *testing.T) {
		_, err := writer.ExecContext(ctx, `INSERT INTO finding_basis_refs (id, finding_id, evidence_unit_id, ordinal)
			VALUES ('fbr_missing_u', 'f_cite_A', 'nonexistent_unit', 2);`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
			t.Fatalf("expected foreign key error on missing evidence unit, got: %v", err)
		}
	})
}

// 9. Transaction Atomicity: Trigger Failures Leave the Transaction Unchanged
func TestStateGuards_TransactionAtomicityOnFailure(t *testing.T) {
	_, writer := openGuardTestStore(t)
	ctx := context.Background()

	insertSnapshotAndUnit(t, writer, "snap_tx", "eu_tx")
	insertSession(t, writer, "sess_tx", "snap_tx", "queued")
	insertRoleRun(t, writer, "rr_tx", "sess_tx", "requirements", "pending")

	tx, err := writer.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}

	// Legal action in tx
	_, err = tx.ExecContext(ctx, `UPDATE sessions SET cancel_requested = 1 WHERE id = 'sess_tx';`)
	if err != nil {
		t.Fatalf("legal update in tx: %v", err)
	}

	// Illegal action in tx: attempt to delete immutable snapshot
	_, err = tx.ExecContext(ctx, `DELETE FROM snapshots WHERE id = 'snap_tx';`)
	if err == nil {
		t.Fatalf("expected trigger failure on delete snapshot")
	}

	// Roll back tx
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// Verify cancel_requested was rolled back to 0
	var cancelReq int
	err = writer.QueryRowContext(ctx, `SELECT cancel_requested FROM sessions WHERE id = 'sess_tx';`).Scan(&cancelReq)
	if err != nil {
		t.Fatalf("query session: %v", err)
	}
	if cancelReq != 0 {
		t.Fatalf("expected cancel_requested to remain 0 after rollback, got %d", cancelReq)
	}
}

// 10. Migration 003 Idempotency
func TestStateGuards_Migration003Idempotency(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	var countBefore int
	err := writer.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations;`).Scan(&countBefore)
	if err != nil {
		t.Fatalf("query count before: %v", err)
	}
	if countBefore != 6 {
		t.Fatalf("expected 6 migrations applied, got %d", countBefore)
	}

	// Run migration again
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second store.Migrate failed: %v", err)
	}

	var countAfter int
	err = writer.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations;`).Scan(&countAfter)
	if err != nil {
		t.Fatalf("query count after: %v", err)
	}
	if countAfter != 6 {
		t.Fatalf("expected count after to remain 6, got %d", countAfter)
	}
}

// 11. Forbidden Scope Inspection
func TestStateGuards_ForbiddenScopeInspection(t *testing.T) {
	_, writer := openGuardTestStore(t)
	ctx := context.Background()

	// Verify triggers list matches expected state guards
	rows, err := writer.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'trigger' ORDER BY name ASC;`)
	if err != nil {
		t.Fatalf("query triggers: %v", err)
	}
	defer rows.Close()

	var triggerNames []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan trigger name: %v", err)
		}
		triggerNames = append(triggerNames, name)
	}

	expectedTriggers := []string{
		"evidence_units_no_delete",
		"evidence_units_no_update",
		"finding_basis_refs_citation_insert",
		"finding_basis_refs_citation_update",
		"finding_basis_refs_insert_in_flight_guard",
		"finding_basis_refs_no_delete",
		"finding_basis_refs_no_update",
		"findings_insert_in_flight_guard",
		"findings_no_delete",
		"findings_no_update",
		"role_runs_immutable_fields",
		"role_runs_status_transition",
		"role_runs_terminal_immutable",
		"role_runs_transition_fields_guard",
		"sessions_cancel_monotonic",
		"sessions_immutable_fields",
		"sessions_status_transition",
		"sessions_terminal_composition_guard",
		"sessions_terminal_immutable",
		"snapshots_no_delete",
		"snapshots_no_update",
	}

	if len(triggerNames) != len(expectedTriggers) {
		t.Fatalf("expected %d triggers, found %d: %v", len(expectedTriggers), len(triggerNames), triggerNames)
	}

	for i, expected := range expectedTriggers {
		if triggerNames[i] != expected {
			t.Errorf("trigger[%d] = %q, want %q", i, triggerNames[i], expected)
		}
	}

	// Verify no trigger body attempts to compose a verdict, dispatch roles, or call external APIs
	var forbiddenTriggers int
	err = writer.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND (
		sql LIKE '%INSERT INTO findings%'
		OR sql LIKE '%UPDATE role_runs SET status = ''in_flight''%'
		OR sql LIKE '%UPDATE sessions SET status = ''complete''%'
	);`).Scan(&forbiddenTriggers)
	if err != nil {
		t.Fatalf("check forbidden trigger behavior: %v", err)
	}
	if forbiddenTriggers != 0 {
		t.Errorf("found %d forbidden autonomous triggers composing verdicts or dispatching roles", forbiddenTriggers)
	}
}
