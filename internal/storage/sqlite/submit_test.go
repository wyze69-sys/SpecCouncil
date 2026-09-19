package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
)

func createTestFrozenSnapshot(t *testing.T, snapID string) evidence.Snapshot {
	t.Helper()
	units := []evidence.Unit{
		{ID: "brief-1", Kind: evidence.UnitBrief, Text: "High level project brief"},
		{ID: "req-1", Kind: evidence.UnitRequirement, Text: "System must persist reviews"},
		{ID: "arch-1", Kind: evidence.UnitComponent, Text: "Storage engine component"},
		{ID: "flow-1", Kind: evidence.UnitFlow, Text: "Submit to review execution flow"},
		{ID: "const-1", Kind: evidence.UnitConstraint, Text: "Maximum memory bound"},
		{ID: "data-1", Kind: evidence.UnitDataRule, Text: "Strict UTF-8 encoding rule"},
	}
	snap, err := evidence.Freeze(snapID, units)
	if err != nil {
		t.Fatalf("evidence.Freeze(%s): %v", snapID, err)
	}
	return snap
}

// 1. Request Hash v1 Exact Vectors
func TestRequestHashV1_ExactVectors(t *testing.T) {
	t.Run("pinned test vector produces exact documented hash", func(t *testing.T) {
		h, err := RequestHashV1("proj-alpha", "Core Design", "First line\nSecond line")
		if err != nil {
			t.Fatalf("RequestHashV1 failed: %v", err)
		}
		const want = "9bed8f44810e1c73a55e94df89b9c6630ecf89ffb6ae662ce556c0e0e5320457"
		if h != want {
			t.Errorf("RequestHashV1 = %q, want %q", h, want)
		}
	})

	t.Run("line endings preservation (CRLF vs LF produce different hashes)", func(t *testing.T) {
		hLF, err := RequestHashV1("proj-1", "Title", "line1\nline2")
		if err != nil {
			t.Fatalf("hLF failed: %v", err)
		}
		hCRLF, err := RequestHashV1("proj-1", "Title", "line1\r\nline2")
		if err != nil {
			t.Fatalf("hCRLF failed: %v", err)
		}
		if hLF == hCRLF {
			t.Errorf("expected different hashes for LF vs CRLF, got both %q", hLF)
		}
	})

	t.Run("no trimming (leading or trailing whitespace changes hash)", func(t *testing.T) {
		base, err := RequestHashV1("proj-1", "Title", "content")
		if err != nil {
			t.Fatalf("base failed: %v", err)
		}
		withLeadSpace, _ := RequestHashV1("proj-1", "Title", " content")
		withTrailSpace, _ := RequestHashV1("proj-1", "Title", "content ")
		withTrailNL, _ := RequestHashV1("proj-1", "Title", "content\n")

		if base == withLeadSpace {
			t.Errorf("leading space should produce different hash")
		}
		if base == withTrailSpace {
			t.Errorf("trailing space should produce different hash")
		}
		if base == withTrailNL {
			t.Errorf("trailing newline should produce different hash")
		}
	})

	t.Run("unicode integrity and no normalization", func(t *testing.T) {
		// Valid multibyte UTF-8 hashes deterministically
		h1, err := RequestHashV1("プロジェクト", "設計書", "仕様内容")
		if err != nil {
			t.Fatalf("unicode hash failed: %v", err)
		}
		h2, _ := RequestHashV1("プロジェクト", "設計書", "仕様内容")
		if h1 != h2 {
			t.Errorf("unicode hash non-deterministic: %q != %q", h1, h2)
		}

		// Unicode normalization must NOT be applied: precomposed 'é' (\u00e9) vs combining 'e' + '\u0301'
		precomposed := "\u00e9cole"
		decomposed := "e\u0301cole"
		hPre, _ := RequestHashV1("p", "t", precomposed)
		hDec, _ := RequestHashV1("p", "t", decomposed)
		if hPre == hDec {
			t.Errorf("expected precomposed and decomposed unicode to produce different hashes (no normalization)")
		}
	})

	t.Run("field boundary shifting immunity", func(t *testing.T) {
		// Moving characters across field boundary must yield different hashes due to decimal length prefix
		h1, _ := RequestHashV1("ab", "c", "content")
		h2, _ := RequestHashV1("a", "bc", "content")
		if h1 == h2 {
			t.Errorf("boundary shifting produced identical hash: %q", h1)
		}

		// Fields with delimiter-like characters
		hCol1, _ := RequestHashV1("a:b", "c", "d")
		hCol2, _ := RequestHashV1("a", "b:c", "d")
		if hCol1 == hCol2 {
			t.Errorf("delimiter-like characters produced identical hash: %q", hCol1)
		}
	})

	t.Run("invalid UTF-8 is rejected before hashing", func(t *testing.T) {
		invalidBytes := string([]byte{0xff, 0xfe, 0xfd})
		_, err := RequestHashV1(invalidBytes, "Title", "Content")
		if !errors.Is(err, ErrInvalidUTF8) {
			t.Errorf("expected ErrInvalidUTF8 for projectID, got %v", err)
		}

		_, err = RequestHashV1("proj-1", invalidBytes, "Content")
		if !errors.Is(err, ErrInvalidUTF8) {
			t.Errorf("expected ErrInvalidUTF8 for title, got %v", err)
		}

		_, err = RequestHashV1("proj-1", "Title", invalidBytes)
		if !errors.Is(err, ErrInvalidUTF8) {
			t.Errorf("expected ErrInvalidUTF8 for content, got %v", err)
		}
	})

	t.Run("empty required fields are rejected", func(t *testing.T) {
		_, err := RequestHashV1("", "Title", "Content")
		if !errors.Is(err, ErrEmptyField) {
			t.Errorf("expected ErrEmptyField for empty projectID, got %v", err)
		}
		_, err = RequestHashV1("proj-1", "", "Content")
		if !errors.Is(err, ErrEmptyField) {
			t.Errorf("expected ErrEmptyField for empty title, got %v", err)
		}
		_, err = RequestHashV1("proj-1", "Title", "")
		if !errors.Is(err, ErrEmptyField) {
			t.Errorf("expected ErrEmptyField for empty content, got %v", err)
		}
	})
}

// 2. Fresh Atomic Creation
func TestSubmit_FreshAtomicCreation(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_fresh_1")

	params := SubmitParams{
		SessionID:      "sess_fresh_1",
		ProjectID:      "proj_fresh",
		IdempotencyKey: "idem_fresh_1",
		Title:          "Architecture Review",
		Content:        "Design document content",
		Snapshot:       snap,
	}

	res, err := store.Submit(ctx, params)
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	if res == nil {
		t.Fatalf("expected non-nil SubmitResult")
	}
	if res.Replay {
		t.Errorf("expected Replay = false for fresh submission")
	}
	if res.SessionID != "sess_fresh_1" {
		t.Errorf("SessionID = %q, want %q", res.SessionID, "sess_fresh_1")
	}
	if res.SnapshotID != snap.ID {
		t.Errorf("SnapshotID = %q, want %q", res.SnapshotID, snap.ID)
	}
	if res.Status != domain.SessionQueued {
		t.Errorf("Status = %q, want %q", res.Status, domain.SessionQueued)
	}

	wantHash, _ := RequestHashV1(params.ProjectID, params.Title, params.Content)
	if res.RequestHash != wantHash {
		t.Errorf("RequestHash = %q, want %q", res.RequestHash, wantHash)
	}

	// Verify database state:
	// 1. Exactly 1 snapshot
	var snapCount int
	var snapHash string
	var normVer int
	err = writer.QueryRowContext(ctx, "SELECT COUNT(*), hash, normalization_version FROM snapshots WHERE id = ?;", snap.ID).
		Scan(&snapCount, &snapHash, &normVer)
	if err != nil || snapCount != 1 {
		t.Fatalf("snapshot query failed: err=%v, count=%d", err, snapCount)
	}
	if snapHash != snap.Hash {
		t.Errorf("snapshot hash = %q, want %q", snapHash, snap.Hash)
	}
	if normVer != 1 {
		t.Errorf("normalization_version = %d, want 1", normVer)
	}

	// 2. Exactly len(snap.Units) evidence units
	var unitCount int
	err = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM evidence_units WHERE snapshot_id = ?;", snap.ID).Scan(&unitCount)
	if err != nil || unitCount != len(snap.Units) {
		t.Fatalf("evidence units count = %d, want %d", unitCount, len(snap.Units))
	}

	// 3. Exactly 1 session with canonical queued initial fields
	var (
		sessCount   int
		status      string
		cancelReq   int
		compCount   int
		incompCount int
		claimedAt   sql.NullString
		cutoffAt    sql.NullString
		deadlineAt  sql.NullString
		termAt      sql.NullString
		termReason  sql.NullString
	)
	err = writer.QueryRowContext(ctx, `SELECT COUNT(*), status, cancel_requested, completed_role_count, incomplete_role_count,
		claimed_at, dispatch_cutoff_at, hard_deadline_at, terminal_at, terminal_reason
		FROM sessions WHERE id = ?;`, "sess_fresh_1").Scan(
		&sessCount, &status, &cancelReq, &compCount, &incompCount,
		&claimedAt, &cutoffAt, &deadlineAt, &termAt, &termReason,
	)
	if err != nil || sessCount != 1 {
		t.Fatalf("session query failed: err=%v, count=%d", err, sessCount)
	}
	if status != "queued" {
		t.Errorf("session status = %q, want queued", status)
	}
	if cancelReq != 0 {
		t.Errorf("cancel_requested = %d, want 0", cancelReq)
	}
	if compCount != 0 {
		t.Errorf("completed_role_count = %d, want 0", compCount)
	}
	if incompCount != 4 {
		t.Errorf("incomplete_role_count = %d, want 4", incompCount)
	}
	if claimedAt.Valid || cutoffAt.Valid || deadlineAt.Valid || termAt.Valid || termReason.Valid {
		t.Errorf("timing/claim/terminal fields must be NULL for queued session")
	}

	// 4. Exactly 4 role runs in pending status
	var roleCount int
	err = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM role_runs WHERE session_id = ? AND status = 'pending';", "sess_fresh_1").Scan(&roleCount)
	if err != nil || roleCount != 4 {
		t.Fatalf("role runs count = %d, want 4", roleCount)
	}
}

// 3. Four Role Order
func TestSubmit_FourRoleOrder(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_role_ord")

	res, err := store.Submit(ctx, SubmitParams{
		ProjectID:      "proj_order",
		IdempotencyKey: "idem_order_1",
		Title:          "Review",
		Content:        "Content",
		Snapshot:       snap,
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	// Read role rows ordered by ROWID (insert order)
	rows, err := writer.QueryContext(ctx, "SELECT role, status, call_count FROM role_runs WHERE session_id = ? ORDER BY rowid ASC;", res.SessionID)
	if err != nil {
		t.Fatalf("query role runs: %v", err)
	}
	defer rows.Close()

	var actualRoles []string
	for rows.Next() {
		var role, st string
		var callCount int
		if err := rows.Scan(&role, &st, &callCount); err != nil {
			t.Fatalf("scan role: %v", err)
		}
		if st != "pending" {
			t.Errorf("role %s status = %q, want pending", role, st)
		}
		if callCount != 0 {
			t.Errorf("role %s call_count = %d, want 0", role, callCount)
		}
		actualRoles = append(actualRoles, role)
	}

	expectedRoles := []string{"requirements", "architecture", "qa", "security"}
	if len(actualRoles) != len(expectedRoles) {
		t.Fatalf("role count = %d, want %d", len(actualRoles), len(expectedRoles))
	}
	for i, want := range expectedRoles {
		if actualRoles[i] != want {
			t.Errorf("role[%d] = %q, want %q (exact domain.Roles order required)", i, actualRoles[i], want)
		}
	}
}

// 4. Snapshot and Evidence Persistence
func TestSubmit_SnapshotAndEvidencePersistence(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_persist")

	res, err := store.Submit(ctx, SubmitParams{
		ProjectID:      "proj_persist",
		IdempotencyKey: "idem_persist",
		Title:          "Persist Test",
		Content:        "Persist Content",
		Snapshot:       snap,
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	// Verify evidence units match snapshot.Units in exact ordinal order
	rows, err := writer.QueryContext(ctx, "SELECT unit_id, ordinal, kind, text FROM evidence_units WHERE snapshot_id = ? ORDER BY ordinal ASC;", res.SnapshotID)
	if err != nil {
		t.Fatalf("query units: %v", err)
	}
	defer rows.Close()

	var i int
	for rows.Next() {
		var unitID, kind, text string
		var ordinal int
		if err := rows.Scan(&unitID, &ordinal, &kind, &text); err != nil {
			t.Fatalf("scan unit: %v", err)
		}
		if i >= len(snap.Units) {
			t.Fatalf("too many units in database: %d", i+1)
		}
		wantUnit := snap.Units[i]
		if unitID != wantUnit.ID {
			t.Errorf("unit[%d].unit_id = %q, want %q", i, unitID, wantUnit.ID)
		}
		if ordinal != i {
			t.Errorf("unit[%d].ordinal = %d, want %d", i, ordinal, i)
		}
		if kind != string(wantUnit.Kind) {
			t.Errorf("unit[%d].kind = %q, want %q", i, kind, wantUnit.Kind)
		}
		if text != wantUnit.Text {
			t.Errorf("unit[%d].text = %q, want %q", i, text, wantUnit.Text)
		}
		i++
	}
	if i != len(snap.Units) {
		t.Errorf("persisted units = %d, want %d", i, len(snap.Units))
	}
}

// 5. Same-Key Same-Hash Replay
func TestSubmit_SameKeySameHashReplay(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_replay")

	params := SubmitParams{
		SessionID:      "sess_replay_orig",
		ProjectID:      "proj_replay",
		IdempotencyKey: "idem_replay_key",
		Title:          "Initial Title",
		Content:        "Initial Content",
		Snapshot:       snap,
	}

	// 1. Initial submission
	res1, err := store.Submit(ctx, params)
	if err != nil {
		t.Fatalf("initial submit: %v", err)
	}
	if res1.Replay {
		t.Fatalf("expected initial submit Replay = false")
	}

	// 2. Replay submission with same (project_id, idempotency_key) and exact same payload
	replayParams := params
	replayParams.SessionID = "sess_should_be_ignored"
	res2, err := store.Submit(ctx, replayParams)
	if err != nil {
		t.Fatalf("replay submit: %v", err)
	}

	if !res2.Replay {
		t.Errorf("expected res2.Replay = true on replay")
	}
	if res2.SessionID != res1.SessionID {
		t.Errorf("res2.SessionID = %q, want original %q", res2.SessionID, res1.SessionID)
	}
	if res2.RequestHash != res1.RequestHash {
		t.Errorf("res2.RequestHash = %q, want %q", res2.RequestHash, res1.RequestHash)
	}
	if res2.Status != domain.SessionQueued {
		t.Errorf("res2.Status = %q, want %q", res2.Status, domain.SessionQueued)
	}

	// Verify no duplicate records created
	var sessionCount int
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions WHERE project_id = ?;", params.ProjectID).Scan(&sessionCount)
	if sessionCount != 1 {
		t.Errorf("total sessions in database = %d, want 1", sessionCount)
	}

	var roleCount int
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM role_runs WHERE session_id = ?;", res1.SessionID).Scan(&roleCount)
	if roleCount != 4 {
		t.Errorf("total role_runs = %d, want 4", roleCount)
	}
}

// 6. Same-Key Different-Hash Conflict
func TestSubmit_SameKeyDifferentHashConflict(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_conflict")

	params1 := SubmitParams{
		ProjectID:      "proj_conflict",
		IdempotencyKey: "idem_conflict_key",
		Title:          "Original Title",
		Content:        "Original Content",
		Snapshot:       snap,
	}

	res1, err := store.Submit(ctx, params1)
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}

	// Second submit with same key but different content (different hash)
	params2 := params1
	params2.Content = "Different Content That Changes Hash"

	res2, err := store.Submit(ctx, params2)
	if err == nil {
		t.Fatalf("expected idempotency conflict error, got success: %+v", res2)
	}

	var conflictErr *IdempotencyConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("expected error to match *IdempotencyConflictError, got: %T (%v)", err, err)
	}

	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Errorf("expected errors.Is(err, ErrIdempotencyConflict) = true")
	}

	if conflictErr.ProjectID != params1.ProjectID {
		t.Errorf("conflict.ProjectID = %q, want %q", conflictErr.ProjectID, params1.ProjectID)
	}
	if conflictErr.IdempotencyKey != params1.IdempotencyKey {
		t.Errorf("conflict.IdempotencyKey = %q, want %q", conflictErr.IdempotencyKey, params1.IdempotencyKey)
	}
	if conflictErr.ExistingHash != res1.RequestHash {
		t.Errorf("conflict.ExistingHash = %q, want %q", conflictErr.ExistingHash, res1.RequestHash)
	}

	wantNewHash, _ := RequestHashV1(params2.ProjectID, params2.Title, params2.Content)
	if conflictErr.IncomingHash != wantNewHash {
		t.Errorf("conflict.IncomingHash = %q, want %q", conflictErr.IncomingHash, wantNewHash)
	}

	// Verify database was untouched by the failed submission
	var sessionCount int
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions WHERE project_id = ?;", params1.ProjectID).Scan(&sessionCount)
	if sessionCount != 1 {
		t.Errorf("sessions count = %d, want 1", sessionCount)
	}
}

// 7. Rollback on Failure (No Partial Rows)
func TestSubmit_RollbackOnFailure(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_rollback")

	// Inject error before commit hook
	submitBeforeCommitHook = func(ctx context.Context, conn *sql.Conn) error {
		return errors.New("injected database error before commit")
	}
	defer func() { submitBeforeCommitHook = nil }()

	_, err := store.Submit(ctx, SubmitParams{
		SessionID:      "sess_rollback_1",
		ProjectID:      "proj_rollback",
		IdempotencyKey: "idem_rollback",
		Title:          "Title",
		Content:        "Content",
		Snapshot:       snap,
	})

	if err == nil {
		t.Fatalf("expected injected error, got nil")
	}
	if !strings.Contains(err.Error(), "injected database error before commit") {
		t.Fatalf("unexpected error message: %v", err)
	}

	// Verify zero partial rows remain in any table
	var count int
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions WHERE id = 'sess_rollback_1';").Scan(&count)
	if count != 0 {
		t.Errorf("session survived failed transaction")
	}
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM snapshots WHERE id = 'snap_rollback';").Scan(&count)
	if count != 0 {
		t.Errorf("snapshot survived failed transaction")
	}
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM evidence_units WHERE snapshot_id = 'snap_rollback';").Scan(&count)
	if count != 0 {
		t.Errorf("evidence_units survived failed transaction")
	}
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM role_runs WHERE session_id = 'sess_rollback_1';").Scan(&count)
	if count != 0 {
		t.Errorf("role_runs survived failed transaction")
	}
}

// 8. Concurrent Same-Key Submissions (Yielding Exactly One Winner)
func TestSubmit_ConcurrentSameKeySubmissions(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_concurrent")

	const concurrency = 10
	var (
		wg          sync.WaitGroup
		startGate   = make(chan struct{})
		results     = make([]*SubmitResult, concurrency)
		errs        = make([]error, concurrency)
		freshCount  int32
		replayCount int32
	)

	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			defer wg.Done()
			<-startGate // Wait for synchronized start

			res, err := store.Submit(ctx, SubmitParams{
				ProjectID:      "proj_concurrent",
				IdempotencyKey: "idem_concurrent_key",
				Title:          "Concurrent Title",
				Content:        "Concurrent Content",
				Snapshot:       snap,
			})
			results[idx] = res
			errs[idx] = err
			if err == nil {
				if res.Replay {
					atomic.AddInt32(&replayCount, 1)
				} else {
					atomic.AddInt32(&freshCount, 1)
				}
			}
		}(i)
	}

	// Release all racers simultaneously
	close(startGate)
	wg.Wait()

	// Verify every racer completed successfully
	var winnerID string
	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d failed: %v", i, err)
		}
		if results[i] == nil {
			t.Fatalf("racer %d result is nil", i)
		}
		if winnerID == "" {
			winnerID = results[i].SessionID
		} else if results[i].SessionID != winnerID {
			t.Errorf("racer %d returned sessionID %q, want winner %q", i, results[i].SessionID, winnerID)
		}
	}

	// Exactly 1 must be fresh, exactly concurrency - 1 must be replay
	if freshCount != 1 {
		t.Errorf("freshCount = %d, want exactly 1 winner", freshCount)
	}
	if replayCount != concurrency-1 {
		t.Errorf("replayCount = %d, want %d replays", replayCount, concurrency-1)
	}

	// Exactly 1 session row and 4 role runs in database
	var sessCount int
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions WHERE project_id = 'proj_concurrent';").Scan(&sessCount)
	if sessCount != 1 {
		t.Errorf("database session count = %d, want 1", sessCount)
	}

	var roleCount int
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM role_runs WHERE session_id = ?;", winnerID).Scan(&roleCount)
	if roleCount != 4 {
		t.Errorf("database role runs = %d, want 4", roleCount)
	}
}

// 9. Concurrent Same-Key Conflicting Submissions
func TestSubmit_ConcurrentSameKeyConflictingSubmissions(t *testing.T) {
	store, writer := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_conc_conflict")

	const concurrency = 6
	var (
		wg        sync.WaitGroup
		startGate = make(chan struct{})
		successes int32
		conflicts int32
	)

	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			defer wg.Done()
			<-startGate

			// Each racer uses a different content string -> different request hash
			_, err := store.Submit(ctx, SubmitParams{
				ProjectID:      "proj_conc_conf",
				IdempotencyKey: "same_key",
				Title:          "Title",
				Content:        fmt.Sprintf("Content variant %d", idx),
				Snapshot:       snap,
			})

			if err == nil {
				atomic.AddInt32(&successes, 1)
			} else if errors.Is(err, ErrIdempotencyConflict) {
				atomic.AddInt32(&conflicts, 1)
			} else {
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}

	close(startGate)
	wg.Wait()

	if successes != 1 {
		t.Errorf("expected exactly 1 winner, got %d", successes)
	}
	if conflicts != concurrency-1 {
		t.Errorf("expected %d conflicts, got %d", concurrency-1, conflicts)
	}

	var sessCount int
	_ = writer.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions WHERE project_id = 'proj_conc_conf';").Scan(&sessCount)
	if sessCount != 1 {
		t.Errorf("database session count = %d, want 1", sessCount)
	}
}

// 10. Validation Errors
func TestSubmit_ValidationErrors(t *testing.T) {
	store, _ := openGuardTestStore(t)
	ctx := context.Background()

	snap := createTestFrozenSnapshot(t, "snap_val")

	t.Run("nil store rejected", func(t *testing.T) {
		var nilStore *Store
		_, err := nilStore.Submit(ctx, SubmitParams{
			ProjectID: "p", IdempotencyKey: "k", Title: "t", Content: "c", Snapshot: snap,
		})
		if err == nil {
			t.Errorf("expected error on nil store")
		}
	})

	t.Run("empty fields rejected", func(t *testing.T) {
		tests := []struct {
			name   string
			params SubmitParams
		}{
			{"empty project", SubmitParams{ProjectID: "", IdempotencyKey: "k", Title: "t", Content: "c", Snapshot: snap}},
			{"empty key", SubmitParams{ProjectID: "p", IdempotencyKey: "", Title: "t", Content: "c", Snapshot: snap}},
			{"empty title", SubmitParams{ProjectID: "p", IdempotencyKey: "k", Title: "", Content: "c", Snapshot: snap}},
			{"empty content", SubmitParams{ProjectID: "p", IdempotencyKey: "k", Title: "t", Content: "", Snapshot: snap}},
			{"empty snapshot", SubmitParams{ProjectID: "p", IdempotencyKey: "k", Title: "t", Content: "c", Snapshot: evidence.Snapshot{}}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				_, err := store.Submit(ctx, tt.params)
				if err == nil {
					t.Errorf("expected validation error for %s", tt.name)
				}
			})
		}
	})

	t.Run("cancelled context rejected", func(t *testing.T) {
		cancelCtx, cancel := context.WithCancel(ctx)
		cancel()
		_, err := store.Submit(cancelCtx, SubmitParams{
			ProjectID: "p", IdempotencyKey: "k", Title: "t", Content: "c", Snapshot: snap,
		})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got: %v", err)
		}
	})
}
