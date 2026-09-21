package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
	"github.com/wyze69-sys/SpecCouncil/internal/storage/sqlite"
)

// newRealStoreServer wires the HTTP server to a real *sqlite.Store on a fresh
// temp database, so an end-to-end submit exercises production SQLite rather than
// the in-memory fakeStore used by the handler tests.
func newRealStoreServer(t *testing.T) (*Server, *sqlite.Store) {
	t.Helper()

	store, err := sqlite.Open(sqlite.Config{
		Path:        filepath.Join(t.TempDir(), "api_e2e.db"),
		BusyTimeout: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}

	srv, err := NewServer(Config{
		Store:            store,
		Authenticator:    &fakeAuthenticator{},
		Authorizer:       &fakeAuthorizer{},
		SnapshotProvider: NewIngestSnapshotProvider(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv, store
}

// submitReview posts one review body and returns the raw recorder.
func submitReview(t *testing.T, srv *Server, projectID, idempotencyKey, title, content string) *httptest.ResponseRecorder {
	t.Helper()

	body, err := json.Marshal(map[string]string{
		"idempotency_key": idempotencyKey,
		"title":           title,
		"content":         content,
	})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/projects/"+projectID+"/reviews", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer valid-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeSubmitResponse(t *testing.T, rec *httptest.ResponseRecorder) SubmitResponse {
	t.Helper()

	var resp SubmitResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode SubmitResponse from %q: %v", rec.Body.String(), err)
	}
	return resp
}

const e2eContent = "# System Spec\n\nREQ-12 The system shall process requests deterministically.\n\n```go\nfunc main() {}\n```"

// TestIngestE2E_RealSQLiteSubmitPersistsFrozenSnapshot proves the nil-snapshot
// blocker is closed against production SQLite: an HTTP submit must persist a
// frozen snapshot and its units, readable back through public store APIs.
func TestIngestE2E_RealSQLiteSubmitPersistsFrozenSnapshot(t *testing.T) {
	ctx := context.Background()
	srv, store := newRealStoreServer(t)

	rec := submitReview(t, srv, "proj-e2e", "e2e-key-1", "E2E Spec", e2eContent)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	resp := decodeSubmitResponse(t, rec)
	if resp.Status != domain.SessionQueued {
		t.Errorf("status = %q, want %q", resp.Status, domain.SessionQueued)
	}
	if resp.SessionID == "" {
		t.Fatal("SessionID is empty")
	}
	if !strings.HasPrefix(resp.SnapshotID, "snap-") {
		t.Fatalf("SnapshotID = %q, want a snap- prefix", resp.SnapshotID)
	}
	if resp.RequestHash == "" {
		t.Error("RequestHash is empty")
	}
	if resp.Replay {
		t.Error("Replay = true on a first submit")
	}

	status, err := store.ReadStatusScoped(ctx, "proj-e2e", resp.SessionID)
	if err != nil {
		t.Fatalf("ReadStatusScoped: %v", err)
	}
	if status.Status != domain.SessionQueued {
		t.Errorf("persisted status = %q, want %q", status.Status, domain.SessionQueued)
	}
	if status.SnapshotID != resp.SnapshotID {
		t.Errorf("persisted SnapshotID = %q, want %q", status.SnapshotID, resp.SnapshotID)
	}
	if status.SnapshotHash == "" {
		t.Error("persisted SnapshotHash is empty")
	}

	snap, err := store.ReadSnapshot(ctx, resp.SnapshotID)
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if snap.ID != resp.SnapshotID {
		t.Errorf("snapshot ID = %q, want %q", snap.ID, resp.SnapshotID)
	}
	if len(snap.Units) != 3 {
		t.Fatalf("units = %d, want 3 (%+v)", len(snap.Units), snap.Units)
	}
	if wantIDs := []string{"u0", "REQ-12", "u2"}; snap.Units[0].ID != wantIDs[0] || snap.Units[1].ID != wantIDs[1] || snap.Units[2].ID != wantIDs[2] {
		t.Errorf("unit IDs = %q/%q/%q, want %q/%q/%q",
			snap.Units[0].ID, snap.Units[1].ID, snap.Units[2].ID, wantIDs[0], wantIDs[1], wantIDs[2])
	}
	if !snap.HasRef("REQ-12") {
		t.Error("persisted snapshot does not address author ID REQ-12")
	}

	refrozen, err := evidence.Freeze(snap.ID, snap.Units)
	if err != nil {
		t.Fatalf("evidence.Freeze on persisted units: %v", err)
	}
	if refrozen.Hash != snap.Hash {
		t.Errorf("persisted hash = %q, recomputed = %q", snap.Hash, refrozen.Hash)
	}
}

// TestIngestE2E_IdempotentReplayReturnsStoredSession proves the second identical
// submit replays the stored session instead of writing a new snapshot row.
func TestIngestE2E_IdempotentReplayReturnsStoredSession(t *testing.T) {
	ctx := context.Background()
	srv, store := newRealStoreServer(t)

	first := submitReview(t, srv, "proj-replay", "e2e-key-replay", "Replay Spec", e2eContent)
	if first.Code != http.StatusCreated {
		t.Fatalf("first submit status = %d, want 201: %s", first.Code, first.Body.String())
	}
	firstResp := decodeSubmitResponse(t, first)
	if firstResp.Replay {
		t.Error("first submit Replay = true, want false")
	}

	second := submitReview(t, srv, "proj-replay", "e2e-key-replay", "Replay Spec", e2eContent)
	if second.Code != http.StatusOK {
		t.Fatalf("second submit status = %d, want 200: %s", second.Code, second.Body.String())
	}
	secondResp := decodeSubmitResponse(t, second)
	if !secondResp.Replay {
		t.Error("second submit Replay = false, want true")
	}
	if secondResp.SessionID != firstResp.SessionID {
		t.Errorf("replay SessionID = %q, want %q", secondResp.SessionID, firstResp.SessionID)
	}
	if secondResp.SnapshotID != firstResp.SnapshotID {
		t.Errorf("replay SnapshotID = %q, want %q", secondResp.SnapshotID, firstResp.SnapshotID)
	}

	status, err := store.ReadStatusScoped(ctx, "proj-replay", firstResp.SessionID)
	if err != nil {
		t.Fatalf("ReadStatusScoped: %v", err)
	}
	if status.IdempotencyKey != "e2e-key-replay" {
		t.Errorf("persisted IdempotencyKey = %q, want e2e-key-replay", status.IdempotencyKey)
	}
}

// TestIngestE2E_WhitespaceOnlyFenceIsClientError pins the D1 repair end to end: a
// blank fenced code block is client input, so it must answer 400 and persist
// nothing (it previously answered 503 service unavailable).
func TestIngestE2E_WhitespaceOnlyFenceIsClientError(t *testing.T) {
	ctx := context.Background()
	srv, store := newRealStoreServer(t)

	const content = "```\n   \n```"
	rec := submitReview(t, srv, "proj-blank", "e2e-key-blank", "Blank", content)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}

	errResp := parseErrorResponse(t, rec.Body)
	if errResp.Error.Code != "bad_request" {
		t.Errorf("error code = %q, want bad_request", errResp.Error.Code)
	}
	if errResp.Error.Message != "content produced no reviewable evidence" {
		t.Errorf("error message = %q, want the no-evidence message", errResp.Error.Message)
	}

	snap, err := store.ReadSnapshot(ctx, snapshotID("proj-blank", "Blank", content))
	if err == nil {
		t.Fatalf("snapshot %+v was persisted for blank-code content", snap)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("ReadSnapshot error = %v, want a not-found error", err)
	}
}

// TestIngestE2E_WhitespaceFenceWithRealContentStillSubmits proves the other half
// of D1: a blank code fence does not poison a document that holds real evidence.
func TestIngestE2E_WhitespaceFenceWithRealContentStillSubmits(t *testing.T) {
	ctx := context.Background()
	srv, store := newRealStoreServer(t)

	rec := submitReview(t, srv, "proj-mixed", "e2e-key-mixed", "Mixed", "```\n   \n```\n\n# Heading")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	resp := decodeSubmitResponse(t, rec)
	if resp.SnapshotID == "" {
		t.Fatal("SnapshotID is empty for a document with real content")
	}

	snap, err := store.ReadSnapshot(ctx, resp.SnapshotID)
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if len(snap.Units) != 1 {
		t.Fatalf("units = %d, want 1 (%+v)", len(snap.Units), snap.Units)
	}
	if snap.Units[0].Text != "Heading" || snap.Units[0].Kind != evidence.UnitBrief {
		t.Errorf("unit = %+v, want the brief heading 'Heading'", snap.Units[0])
	}
}
