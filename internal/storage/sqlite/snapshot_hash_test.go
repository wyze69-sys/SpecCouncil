package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
)

func countTableRows(t *testing.T, db *sql.DB) (sessions, snapshots, units int) {
	t.Helper()
	ctx := context.Background()
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions;").Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM snapshots;").Scan(&snapshots); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM evidence_units;").Scan(&units); err != nil {
		t.Fatalf("count evidence_units: %v", err)
	}
	return
}

func TestSnapshotHash_1_ValidHashAccepted(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_valid_1")
	params := SubmitParams{
		ProjectID:      "proj_valid",
		IdempotencyKey: "idem_valid",
		Title:          "Valid Title",
		Content:        "Valid Content",
		Snapshot:       snap,
	}

	res, err := store.Submit(ctx, params)
	if err != nil {
		t.Fatalf("Submit with valid snapshot failed: %v", err)
	}
	if res.Replay {
		t.Fatalf("expected fresh submit, got replay")
	}

	sess, err := store.ReadStatus(ctx, res.SessionID)
	if err != nil {
		t.Fatalf("ReadStatus failed: %v", err)
	}
	if sess.SnapshotID != snap.ID {
		t.Errorf("ReadStatus snapshot ID = %q, want %q", sess.SnapshotID, snap.ID)
	}

	readSnap, err := store.ReadSnapshot(ctx, snap.ID)
	if err != nil {
		t.Fatalf("ReadSnapshot failed: %v", err)
	}
	if readSnap.Hash != snap.Hash {
		t.Errorf("ReadSnapshot hash = %q, want %q", readSnap.Hash, snap.Hash)
	}

	sCount, snapCount, uCount := countTableRows(t, writer)
	if sCount != 1 || snapCount != 1 || uCount != len(snap.Units) {
		t.Errorf("unexpected counts: sessions=%d, snapshots=%d, units=%d", sCount, snapCount, uCount)
	}
}

func TestSnapshotHash_2_TamperedZerosHashRejected(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	s0, snap0, u0 := countTableRows(t, writer)

	snap := createTestFrozenSnapshot(t, "snap_zeros")
	snap.Hash = strings.Repeat("0", 64)

	params := SubmitParams{
		ProjectID:      "proj_tampered",
		IdempotencyKey: "idem_zeros",
		Title:          "Tampered Zeros",
		Content:        "Tampered Content",
		Snapshot:       snap,
	}

	_, err := store.Submit(ctx, params)
	if err == nil {
		t.Fatalf("expected error submitting tampered snapshot with zeros, got nil")
	}
	if !strings.Contains(err.Error(), "snapshot hash mismatch") {
		t.Fatalf("expected error to contain %q, got %q", "snapshot hash mismatch", err.Error())
	}

	s1, snap1, u1 := countTableRows(t, writer)
	if s1 != s0 || snap1 != snap0 || u1 != u0 {
		t.Fatalf("partial writes occurred: before=(%d,%d,%d), after=(%d,%d,%d)", s0, snap0, u0, s1, snap1, u1)
	}
}

func TestSnapshotHash_3_RandomWrongHashRejectedNoPartialWrites(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	s0, snap0, u0 := countTableRows(t, writer)

	snap := createTestFrozenSnapshot(t, "snap_random")
	snap.Hash = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

	params := SubmitParams{
		ProjectID:      "proj_random",
		IdempotencyKey: "idem_random",
		Title:          "Random Hash",
		Content:        "Random Content",
		Snapshot:       snap,
	}

	_, err := store.Submit(ctx, params)
	if err == nil {
		t.Fatalf("expected error submitting tampered snapshot, got nil")
	}
	if !strings.Contains(err.Error(), "snapshot hash mismatch") {
		t.Fatalf("expected error to contain %q, got %q", "snapshot hash mismatch", err.Error())
	}

	s1, snap1, u1 := countTableRows(t, writer)
	if s1 != s0 || snap1 != snap0 || u1 != u0 {
		t.Fatalf("partial writes occurred: before=(%d,%d,%d), after=(%d,%d,%d)", s0, snap0, u0, s1, snap1, u1)
	}
}

func TestSnapshotHash_4_EmptyHashRejectedViaErrNilSnapshot(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	s0, snap0, u0 := countTableRows(t, writer)

	snap := createTestFrozenSnapshot(t, "snap_empty")
	snap.Hash = ""

	params := SubmitParams{
		ProjectID:      "proj_empty",
		IdempotencyKey: "idem_empty",
		Title:          "Empty Hash",
		Content:        "Empty Content",
		Snapshot:       snap,
	}

	_, err := store.Submit(ctx, params)
	if err == nil {
		t.Fatalf("expected error submitting snapshot with empty hash, got nil")
	}
	if !errors.Is(err, ErrNilSnapshot) {
		t.Fatalf("expected ErrNilSnapshot, got %v", err)
	}

	s1, snap1, u1 := countTableRows(t, writer)
	if s1 != s0 || snap1 != snap0 || u1 != u0 {
		t.Fatalf("partial writes occurred: before=(%d,%d,%d), after=(%d,%d,%d)", s0, snap0, u0, s1, snap1, u1)
	}
}

func TestSnapshotHash_5_DuplicateUnitIDsRejected(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	s0, snap0, u0 := countTableRows(t, writer)

	snap := createTestFrozenSnapshot(t, "snap_dup")
	// Introduce duplicate unit ID.
	snap.Units = append(snap.Units, snap.Units[0])

	params := SubmitParams{
		ProjectID:      "proj_dup",
		IdempotencyKey: "idem_dup",
		Title:          "Dup Unit IDs",
		Content:        "Dup Content",
		Snapshot:       snap,
	}

	_, err := store.Submit(ctx, params)
	if err == nil {
		t.Fatalf("expected error submitting snapshot with duplicate unit IDs, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate unit id") {
		t.Fatalf("expected error to contain %q, got %q", "duplicate unit id", err.Error())
	}

	s1, snap1, u1 := countTableRows(t, writer)
	if s1 != s0 || snap1 != snap0 || u1 != u0 {
		t.Fatalf("partial writes occurred: before=(%d,%d,%d), after=(%d,%d,%d)", s0, snap0, u0, s1, snap1, u1)
	}
}

func TestSnapshotHash_6_InvalidUnitKindRejected(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	s0, snap0, u0 := countTableRows(t, writer)

	snap := createTestFrozenSnapshot(t, "snap_bad_kind")
	snap.Units = []evidence.Unit{
		{ID: "unit-invalid", Kind: evidence.UnitKind("invalid_kind"), Text: "Some text"},
	}

	params := SubmitParams{
		ProjectID:      "proj_bad_kind",
		IdempotencyKey: "idem_bad_kind",
		Title:          "Bad Kind",
		Content:        "Bad Kind Content",
		Snapshot:       snap,
	}

	_, err := store.Submit(ctx, params)
	if err == nil {
		t.Fatalf("expected error submitting snapshot with invalid unit kind, got nil")
	}
	if !strings.Contains(err.Error(), "unknown unit kind") && !strings.Contains(err.Error(), "invalid snapshot") {
		t.Fatalf("expected error to mention unit kind / invalid snapshot, got %q", err.Error())
	}

	s1, snap1, u1 := countTableRows(t, writer)
	if s1 != s0 || snap1 != snap0 || u1 != u0 {
		t.Fatalf("partial writes occurred: before=(%d,%d,%d), after=(%d,%d,%d)", s0, snap0, u0, s1, snap1, u1)
	}
}

func TestSnapshotHash_7_IdempotencyReplayRejectedOnTamperedHash(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_idem")
	params := SubmitParams{
		ProjectID:      "proj_idem",
		IdempotencyKey: "idem_key_1",
		Title:          "Idem Title",
		Content:        "Idem Content",
		Snapshot:       snap,
	}

	res1, err := store.Submit(ctx, params)
	if err != nil {
		t.Fatalf("first Submit failed: %v", err)
	}
	if res1.Replay {
		t.Fatalf("expected first submit to not be replay")
	}

	sCount1, snapCount1, uCount1 := countTableRows(t, writer)

	// Second submit with identical project, idempotency key, title, content,
	// but tampered snapshot hash.
	tamperedSnap := snap
	tamperedSnap.Hash = strings.Repeat("a", 64)

	paramsTampered := params
	paramsTampered.Snapshot = tamperedSnap

	_, err = store.Submit(ctx, paramsTampered)
	if err == nil {
		t.Fatalf("expected error on tampered second submit, got nil")
	}
	if !strings.Contains(err.Error(), "snapshot hash mismatch") {
		t.Fatalf("expected error to contain %q, got %q", "snapshot hash mismatch", err.Error())
	}

	// Verify database state is untouched by the tampered attempt.
	sCount2, snapCount2, uCount2 := countTableRows(t, writer)
	if sCount2 != sCount1 || snapCount2 != snapCount1 || uCount2 != uCount1 {
		t.Fatalf("tampered submit modified counts: before=(%d,%d,%d), after=(%d,%d,%d)",
			sCount1, snapCount1, uCount1, sCount2, snapCount2, uCount2)
	}
}

func TestSnapshotHash_8_ConcurrentSubmissionsTamperedProduceZeroPersisted(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	const concurrency = 8
	var wg sync.WaitGroup
	startBarrier := make(chan struct{})
	errs := make([]error, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-startBarrier

			snap := createTestFrozenSnapshot(t, fmt.Sprintf("snap_conc_%d", idx))
			snap.Hash = fmt.Sprintf("%064x", idx) // tampered hash

			params := SubmitParams{
				ProjectID:      "proj_conc_tamper",
				IdempotencyKey: fmt.Sprintf("idem_conc_%d", idx),
				Title:          fmt.Sprintf("Title %d", idx),
				Content:        fmt.Sprintf("Content %d", idx),
				Snapshot:       snap,
			}

			_, errs[idx] = store.Submit(ctx, params)
		}(i)
	}

	close(startBarrier)
	wg.Wait()

	for i, err := range errs {
		if err == nil {
			t.Errorf("caller %d expected error, got nil", i)
		} else if !strings.Contains(err.Error(), "snapshot hash mismatch") {
			t.Errorf("caller %d expected hash mismatch, got %q", i, err.Error())
		}
	}

	sCount, snapCount, uCount := countTableRows(t, writer)
	if sCount != 0 || snapCount != 0 || uCount != 0 {
		t.Fatalf("tampered callers produced rows: sessions=%d, snapshots=%d, units=%d (expected 0)",
			sCount, snapCount, uCount)
	}
}
