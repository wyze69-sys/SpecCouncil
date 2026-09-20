package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
)

func TestTimestampMigrationNormalizesLegacyWholeSecondRows(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStore(t, 100*time.Millisecond)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("initial migrate: %v", err)
	}
	writer, err := store.writerDB()
	if err != nil {
		t.Fatalf("writerDB: %v", err)
	}

	if _, err := writer.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version = 6;"); err != nil {
		t.Fatalf("remove migration 6 receipt: %v", err)
	}

	if _, err := writer.ExecContext(ctx, `
		INSERT INTO snapshots (id, hash, project_id, title, content, normalization_version, created_at)
		VALUES ('legacy_snap', '012345678901234567890123456789012345678901234567890123456789abcd', 'legacy_proj', 'Legacy', 'Legacy content', 1, '2026-09-19T12:00:00Z');
		INSERT INTO sessions (id, project_id, idempotency_key, request_hash, snapshot_id, status, created_at)
		VALUES ('legacy_session', 'legacy_proj', 'legacy_key', '012345678901234567890123456789012345678901234567890123456789abcd', 'legacy_snap', 'queued', '2026-09-19T12:00:00.1Z');
		INSERT INTO role_runs (id, session_id, role, status, created_at)
		VALUES
		 ('legacy_rr_req', 'legacy_session', 'requirements', 'pending', '2026-09-19T12:00:00Z'),
		 ('legacy_rr_arch', 'legacy_session', 'architecture', 'pending', '2026-09-19T12:00:00Z'),
		 ('legacy_rr_qa', 'legacy_session', 'qa', 'pending', '2026-09-19T12:00:00Z'),
		 ('legacy_rr_sec', 'legacy_session', 'security', 'pending', '2026-09-19T12:00:00Z');
	`); err != nil {
		t.Fatalf("seed legacy rows: %v", err)
	}

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("reapply timestamp migration: %v", err)
	}

	var createdAt string
	if err := writer.QueryRowContext(ctx, "SELECT created_at FROM sessions WHERE id = 'legacy_session';").Scan(&createdAt); err != nil {
		t.Fatalf("read normalized session: %v", err)
	}
	if createdAt != "2026-09-19T12:00:00.100000000Z" {
		t.Fatalf("normalized session created_at = %q", createdAt)
	}

	policy := TimingPolicy{DispatchCutoff: time.Minute, CallTimeout: time.Minute, SessionHardDeadline: 2 * time.Minute}
	claim, err := store.ClaimSessionWithNow(ctx, policy, time.Date(2026, 9, 19, 12, 0, 0, 100_000_000, time.UTC))
	if err != nil {
		t.Fatalf("claim normalized legacy session: %v", err)
	}
	if !claim.Claimed || claim.SessionID != "legacy_session" {
		t.Fatalf("claim result = %+v", claim)
	}
}

func TestTimestampFixedWidthOrderingAndParsing(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStore(t, 100*time.Millisecond)
	writer, err := store.writerDB()
	if err != nil {
		t.Fatalf("writerDB: %v", err)
	}

	earlier := time.Date(2026, 9, 19, 12, 0, 0, 100_000_000, time.UTC)
	later := earlier.Add(50 * time.Millisecond)
	earlierText := formatUTCTimestamp(earlier)
	laterText := formatUTCTimestamp(later)
	if earlierText != "2026-09-19T12:00:00.100000000Z" {
		t.Fatalf("earlier timestamp = %q, want fixed-width nanoseconds", earlierText)
	}
	if laterText != "2026-09-19T12:00:00.150000000Z" {
		t.Fatalf("later timestamp = %q, want fixed-width nanoseconds", laterText)
	}

	var ordered int
	if err := writer.QueryRowContext(ctx, "SELECT ? < ?;", earlierText, laterText).Scan(&ordered); err != nil {
		t.Fatalf("compare timestamps in SQLite: %v", err)
	}
	if ordered != 1 {
		t.Fatalf("SQLite did not order %q before %q", earlierText, laterText)
	}

	for _, timestamp := range []string{
		"2026-09-19T12:00:00Z",
		"2026-09-19T12:00:00.123456789Z",
	} {
		if _, err := parseUTCTimestamp(timestamp); err != nil {
			t.Errorf("parseUTCTimestamp(%q): %v", timestamp, err)
		}
	}
}

func TestTimestampClaimFiftyMillisecondsAfterCreation(t *testing.T) {
	ctx := context.Background()
	store, _ := openGuardTestStore(t)
	createdAt := time.Date(2026, 9, 19, 12, 0, 0, 100_000_000, time.UTC)
	claimedAt := createdAt.Add(50 * time.Millisecond)

	_, err := store.Submit(ctx, SubmitParams{
		SessionID:      "timestamp_claim",
		ProjectID:      "timestamp_claim_project",
		IdempotencyKey: "timestamp_claim_key",
		Title:          "Timestamp claim",
		Content:        "Timestamp ordering regression",
		Snapshot:       createTestFrozenSnapshot(t, "timestamp_claim_snapshot"),
		CreatedAt:      createdAt,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	result, err := store.ClaimSessionWithNow(ctx, timestampTestPolicy(), claimedAt)
	if err != nil {
		t.Fatalf("ClaimSessionWithNow: %v", err)
	}
	if !result.Claimed || !result.ClaimedAt.Equal(claimedAt) {
		t.Fatalf("claim result = %+v, want claim at %v", result, claimedAt)
	}
}

func TestTimestampPublicationFiftyMillisecondsAfterStart(t *testing.T) {
	ctx := context.Background()
	store, _ := openGuardTestStore(t)
	base := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	startedAt := base.Add(100 * time.Millisecond)
	completedAt := startedAt.Add(50 * time.Millisecond)
	prepareTimestampSession(t, store, "timestamp_publish", base.Add(-time.Second), base.Add(-500*time.Millisecond))

	reservation, err := store.ReservePendingRoleWithNow(ctx, "timestamp_publish", startedAt)
	if err != nil || !reservation.Reserved {
		t.Fatalf("ReservePendingRoleWithNow: result=%+v err=%v", reservation, err)
	}
	publication, err := store.PublishRoleSuccess(ctx, PublishSuccessParams{
		SessionID:   reservation.SessionID,
		RoleRunID:   reservation.RoleRunID,
		Role:        reservation.Role,
		CallCount:   1,
		CompletedAt: completedAt,
	})
	if err != nil {
		t.Fatalf("PublishRoleSuccess: %v", err)
	}
	if !publication.CompletedAt.Equal(completedAt) {
		t.Fatalf("completed_at = %v, want %v", publication.CompletedAt, completedAt)
	}
}

func TestTimestampCompositionFiftyMillisecondsAfterClaim(t *testing.T) {
	ctx := context.Background()
	store, _ := openGuardTestStore(t)
	base := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	claimedAt := base.Add(100 * time.Millisecond)
	terminalAt := claimedAt.Add(50 * time.Millisecond)
	prepareTimestampSession(t, store, "timestamp_compose", base.Add(-time.Second), claimedAt)

	for _, role := range domain.Roles {
		reservation, err := store.ReservePendingRoleWithNow(ctx, "timestamp_compose", claimedAt)
		if err != nil || !reservation.Reserved {
			t.Fatalf("reserve %s: result=%+v err=%v", role, reservation, err)
		}
		if _, err := store.PublishRoleSuccess(ctx, PublishSuccessParams{
			SessionID:   reservation.SessionID,
			RoleRunID:   reservation.RoleRunID,
			Role:        role,
			CallCount:   1,
			CompletedAt: terminalAt,
		}); err != nil {
			t.Fatalf("publish %s: %v", role, err)
		}
	}

	result, err := store.ComposeSessionWithParams(ctx, ComposeParams{
		SessionID: "timestamp_compose",
		Now:       terminalAt,
	})
	if err != nil {
		t.Fatalf("ComposeSessionWithParams: %v", err)
	}
	if !result.Composed || !result.TerminalAt.Equal(terminalAt) {
		t.Fatalf("composition result = %+v, want terminal_at %v", result, terminalAt)
	}
}

func TestTimestampCutoffAndRestartPredicates(t *testing.T) {
	base := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	claimedAt := base.Add(100 * time.Millisecond)
	boundary := claimedAt.Add(50 * time.Millisecond)

	t.Run("cutoff", func(t *testing.T) {
		ctx := context.Background()
		store, _ := openGuardTestStore(t)
		_, err := store.Submit(ctx, SubmitParams{
			SessionID:      "timestamp_cutoff",
			ProjectID:      "timestamp_cutoff_project",
			IdempotencyKey: "timestamp_cutoff_key",
			Title:          "Timestamp cutoff",
			Content:        "Timestamp ordering regression",
			Snapshot:       createTestFrozenSnapshot(t, "timestamp_cutoff_snapshot"),
			CreatedAt:      base.Add(-time.Second),
		})
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		policy := TimingPolicy{
			DispatchCutoff:      50 * time.Millisecond,
			CallTimeout:         25 * time.Millisecond,
			SessionHardDeadline: 75 * time.Millisecond,
		}
		if _, err := store.ClaimSessionWithNow(ctx, policy, claimedAt); err != nil {
			t.Fatalf("ClaimSessionWithNow: %v", err)
		}

		before, err := store.SweepCutoff(ctx, "timestamp_cutoff", boundary.Add(-time.Nanosecond))
		if err != nil {
			t.Fatalf("SweepCutoff before boundary: %v", err)
		}
		if before.CutoffReached {
			t.Fatalf("cutoff reached before %v", boundary)
		}
		at, err := store.SweepCutoff(ctx, "timestamp_cutoff", boundary)
		if err != nil {
			t.Fatalf("SweepCutoff at boundary: %v", err)
		}
		if !at.CutoffReached || at.InterruptedCount != domain.RoleCount {
			t.Fatalf("cutoff result = %+v, want all pending roles interrupted", at)
		}
	})

	t.Run("restart", func(t *testing.T) {
		ctx := context.Background()
		store, _ := openGuardTestStore(t)
		prepareTimestampSession(t, store, "timestamp_restart", base.Add(-time.Second), claimedAt)
		result, err := store.SweepRestartRecoveryWithNow(ctx, boundary, boundary.Add(50*time.Millisecond))
		if err != nil {
			t.Fatalf("SweepRestartRecoveryWithNow: %v", err)
		}
		if result.SessionsRecovered != 1 || result.TotalInterrupted != domain.RoleCount {
			t.Fatalf("restart result = %+v, want one recovered session", result)
		}
	})
}

func TestTimestampMigrationMetadataFixedWidthOnReopen(t *testing.T) {
	ctx := context.Background()
	store, dbPath := setupTestStore(t, 100*time.Millisecond)
	appliedAt := time.Date(2026, 9, 19, 12, 0, 0, 100_000_000, time.UTC)
	originalClock := clock
	clock = func() time.Time { return appliedAt }
	t.Cleanup(func() { clock = originalClock })

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	writer, err := store.writerDB()
	if err != nil {
		t.Fatalf("writerDB: %v", err)
	}
	rows := queryAppliedMigrations(t, writer)
	want := "2026-09-19T12:00:00.100000000Z"
	for _, row := range rows {
		if row.AppliedAt != want {
			t.Errorf("migration %d applied_at = %q, want %q", row.Version, row.AppliedAt, want)
		}
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close before reopen: %v", err)
	}
	reopened, err := Open(Config{Path: filepath.Clean(dbPath), BusyTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("Open after migration: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Migrate(ctx); err != nil {
		t.Fatalf("Migrate after reopen: %v", err)
	}
}

func prepareTimestampSession(t *testing.T, store *Store, sessionID string, createdAt, claimedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	_, err := store.Submit(ctx, SubmitParams{
		SessionID:      sessionID,
		ProjectID:      sessionID + "_project",
		IdempotencyKey: sessionID + "_key",
		Title:          "Timestamp regression",
		Content:        "Timestamp ordering regression",
		Snapshot:       createTestFrozenSnapshot(t, sessionID+"_snapshot"),
		CreatedAt:      createdAt,
	})
	if err != nil {
		t.Fatalf("Submit %s: %v", sessionID, err)
	}
	result, err := store.ClaimSessionWithNow(ctx, timestampTestPolicy(), claimedAt)
	if err != nil {
		t.Fatalf("ClaimSessionWithNow %s: %v", sessionID, err)
	}
	if !result.Claimed {
		t.Fatalf("session %s was not claimed: %+v", sessionID, result)
	}
}

func timestampTestPolicy() TimingPolicy {
	return TimingPolicy{
		DispatchCutoff:      10 * time.Second,
		CallTimeout:         5 * time.Second,
		SessionHardDeadline: 15 * time.Second,
	}
}
