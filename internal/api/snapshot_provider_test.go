package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
	"github.com/wyze69-sys/SpecCouncil/internal/ingest"
)

func setupTestServerWithProvider(t *testing.T, provider SnapshotProvider) (*Server, *fakeStore, *fakeAuthenticator, *fakeAuthorizer) {
	t.Helper()
	store := &fakeStore{}
	auth := &fakeAuthenticator{}
	authz := &fakeAuthorizer{}

	srv, err := NewServer(Config{
		Store:            store,
		Authenticator:    auth,
		Authorizer:       authz,
		SnapshotProvider: provider,
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	return srv, store, auth, authz
}

func TestSnapshotProvider_BuildsValidSnapshot(t *testing.T) {
	provider := NewIngestSnapshotProvider()
	content := `# Architectural Specification

REQ-12 System shall process requests deterministically.

- List item A
- List item B

| Col1 | Col2 |
| val1 | val2 |

` + "```go\nfunc main() {}\n```"

	snap, err := provider(context.Background(), "proj-alpha", "Architecture Doc", content)
	if err != nil {
		t.Fatalf("unexpected error from provider: %v", err)
	}

	if !strings.HasPrefix(snap.ID, "snap-") {
		t.Fatalf("Snapshot.ID expected prefix 'snap-', got %q", snap.ID)
	}
	if snap.Hash == "" {
		t.Fatal("Snapshot.Hash must not be empty")
	}
	if len(snap.Units) == 0 {
		t.Fatal("Snapshot.Units must not be empty")
	}

	// Satisfies sqlite.Submit non-empty guard:
	// len(params.Snapshot.ID) == 0 || len(params.Snapshot.Hash) == 0 || len(params.Snapshot.Units) == 0
	if len(snap.ID) == 0 || len(snap.Hash) == 0 || len(snap.Units) == 0 {
		t.Fatal("Snapshot violates sqlite.Submit non-empty guard")
	}

	// Re-verify hash matches evidence.Freeze as sqlite.Submit requires
	frozen, err := evidence.Freeze(snap.ID, snap.Units)
	if err != nil {
		t.Fatalf("evidence.Freeze failed on produced units: %v", err)
	}
	if frozen.Hash != snap.Hash {
		t.Fatalf("Snapshot.Hash mismatch: frozen %q != snap %q", frozen.Hash, snap.Hash)
	}

	// Addressable author ID
	if !snap.HasRef("REQ-12") {
		t.Fatal("expected author ID 'REQ-12' to be addressable via Snapshot.HasRef")
	}
}

func TestSnapshotProvider_Determinism(t *testing.T) {
	provider := NewIngestSnapshotProvider()
	content := "# Deterministic Document\n\nREQ-01 First requirement.\n\nParagraph text."

	snap1, err1 := provider(context.Background(), "proj-det", "Spec Title", content)
	if err1 != nil {
		t.Fatalf("provider call 1 failed: %v", err1)
	}

	snap2, err2 := provider(context.Background(), "proj-det", "Spec Title", content)
	if err2 != nil {
		t.Fatalf("provider call 2 failed: %v", err2)
	}

	if snap1.ID != snap2.ID {
		t.Fatalf("deterministic ID failure: %q != %q", snap1.ID, snap2.ID)
	}
	if snap1.Hash != snap2.Hash {
		t.Fatalf("deterministic Hash failure: %q != %q", snap1.Hash, snap2.Hash)
	}
	if len(snap1.Units) != len(snap2.Units) {
		t.Fatalf("deterministic Units count mismatch: %d != %d", len(snap1.Units), len(snap2.Units))
	}
	for i := range snap1.Units {
		if snap1.Units[i] != snap2.Units[i] {
			t.Fatalf("unit %d mismatch: %+v != %+v", i, snap1.Units[i], snap2.Units[i])
		}
	}
}

func TestSnapshotProvider_ProjectAndTitleScoping(t *testing.T) {
	provider := NewIngestSnapshotProvider()
	content := "# Scope Test\n\nREQ-99 Testing scopes."

	snapA, err := provider(context.Background(), "proj-A", "Title 1", content)
	if err != nil {
		t.Fatalf("provider snapA failed: %v", err)
	}
	snapB, err := provider(context.Background(), "proj-B", "Title 1", content)
	if err != nil {
		t.Fatalf("provider snapB failed: %v", err)
	}
	snapC, err := provider(context.Background(), "proj-A", "Title 2", content)
	if err != nil {
		t.Fatalf("provider snapC failed: %v", err)
	}
	snapA2, err := provider(context.Background(), "proj-A", "Title 1", content)
	if err != nil {
		t.Fatalf("provider snapA2 failed: %v", err)
	}

	if snapA.ID == snapB.ID {
		t.Fatalf("expected different snapshot IDs for different projectID: %q == %q", snapA.ID, snapB.ID)
	}
	if snapA.ID == snapC.ID {
		t.Fatalf("expected different snapshot IDs for different title: %q == %q", snapA.ID, snapC.ID)
	}
	if snapA.ID != snapA2.ID {
		t.Fatalf("expected exact equality for identical inputs: %q != %q", snapA.ID, snapA2.ID)
	}

	// Direct test of snapshotID unexported function
	id1 := snapshotID("p1", "t1", "c1")
	id2 := snapshotID("p1", "t1", "c1")
	idP := snapshotID("p2", "t1", "c1")
	idT := snapshotID("p1", "t2", "c1")
	idC := snapshotID("p1", "t1", "c2")

	if id1 != id2 {
		t.Fatalf("snapshotID must be deterministic: %q != %q", id1, id2)
	}
	if id1 == idP {
		t.Fatalf("snapshotID failed to scope by projectID")
	}
	if id1 == idT {
		t.Fatalf("snapshotID failed to scope by title")
	}
	if id1 == idC {
		t.Fatalf("snapshotID failed to scope by content")
	}
}

func TestSnapshotProvider_NoEvidenceContent_Sentinel(t *testing.T) {
	provider := NewIngestSnapshotProvider()

	cases := []struct {
		name    string
		content string
	}{
		{name: "empty string", content: ""},
		{name: "spaces only", content: "   "},
		{name: "tabs and newlines", content: " \t \r\n \n "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap, err := provider(context.Background(), "proj-1", "Title", tc.content)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
			if !errors.Is(err, ingest.ErrNoEvidenceUnits) {
				t.Fatalf("expected errors.Is(err, ingest.ErrNoEvidenceUnits) == true, got %v", err)
			}
			if snap.ID != "" || snap.Hash != "" || len(snap.Units) != 0 {
				t.Fatalf("expected zero Snapshot, got ID=%q Hash=%q Units=%d", snap.ID, snap.Hash, len(snap.Units))
			}
		})
	}
}

func TestSubmitHandler_ValidSubmitWithProvider_ClosesNilSnapshotBlocker(t *testing.T) {
	srv, store, _, _ := setupTestServerWithProvider(t, NewIngestSnapshotProvider())

	rawBody := `{
  "idempotency_key": "key-submit-valid-1",
  "title": "Valid System Spec",
  "content": "# System Specification\n\nREQ-12 System shall process requests deterministically.\n\nMore detail here."
}`

	req := httptest.NewRequest(http.MethodPost, "/v1/projects/proj-valid/reviews", strings.NewReader(rawBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer valid-token")
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", rec.Code, rec.Body.String())
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if len(store.submitCalls) != 1 {
		t.Fatalf("expected exactly 1 Submit call, got %d", len(store.submitCalls))
	}

	call := store.submitCalls[0]
	if call.ProjectID != "proj-valid" {
		t.Errorf("ProjectID = %q, want 'proj-valid'", call.ProjectID)
	}
	if call.IdempotencyKey != "key-submit-valid-1" {
		t.Errorf("IdempotencyKey = %q, want 'key-submit-valid-1'", call.IdempotencyKey)
	}

	// Assert the nil-snapshot blocker is closed:
	// Snapshot.ID, Snapshot.Hash, Snapshot.Units are all non-empty
	if call.Snapshot.ID == "" {
		t.Fatal("call.Snapshot.ID is empty; nil-snapshot blocker not closed")
	}
	if !strings.HasPrefix(call.Snapshot.ID, "snap-") {
		t.Fatalf("call.Snapshot.ID expected prefix 'snap-', got %q", call.Snapshot.ID)
	}
	if call.Snapshot.Hash == "" {
		t.Fatal("call.Snapshot.Hash is empty; nil-snapshot blocker not closed")
	}
	if len(call.Snapshot.Units) == 0 {
		t.Fatal("call.Snapshot.Units is empty; nil-snapshot blocker not closed")
	}
	if !call.Snapshot.HasRef("REQ-12") {
		t.Fatal("expected call.Snapshot.HasRef('REQ-12') to be true")
	}

	// Verify snapshot re-freeze consistency
	frozen, err := evidence.Freeze(call.Snapshot.ID, call.Snapshot.Units)
	if err != nil {
		t.Fatalf("evidence.Freeze failed on captured snapshot: %v", err)
	}
	if frozen.Hash != call.Snapshot.Hash {
		t.Fatalf("captured snapshot hash mismatch: %q != %q", frozen.Hash, call.Snapshot.Hash)
	}
}

func TestSubmitHandler_BlankContentWithProvider_Returns400_NoStoreCall(t *testing.T) {
	srv, store, _, _ := setupTestServerWithProvider(t, NewIngestSnapshotProvider())

	blankCases := []struct {
		name    string
		content string
	}{
		{name: "spaces only", content: "   "},
		{name: "newlines only", content: "\n\n\r\n\t "},
	}

	for _, tc := range blankCases {
		t.Run(tc.name, func(t *testing.T) {
			store.mu.Lock()
			initialCalls := len(store.submitCalls)
			store.mu.Unlock()

			bodyObj := map[string]string{
				"idempotency_key": "key-blank-test",
				"title":           "Spec Title",
				"content":         tc.content,
			}
			raw, err := json.Marshal(bodyObj)
			if err != nil {
				t.Fatalf("json.Marshal failed: %v", err)
			}

			req := httptest.NewRequest(http.MethodPost, "/v1/projects/proj-blank/reviews", bytes.NewReader(raw))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer valid-token")
			rec := httptest.NewRecorder()

			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 Bad Request, got %d: %s", rec.Code, rec.Body.String())
			}

			errResp := parseErrorResponse(t, rec.Body)
			if errResp.Error.Code != "bad_request" {
				t.Errorf("error code = %q, want 'bad_request'", errResp.Error.Code)
			}
			if errResp.Error.Message != "content produced no reviewable evidence" {
				t.Errorf("error message = %q, want 'content produced no reviewable evidence'", errResp.Error.Message)
			}

			store.mu.Lock()
			postCalls := len(store.submitCalls)
			store.mu.Unlock()

			if postCalls != initialCalls {
				t.Fatalf("store.Submit was called despite blank content: initial=%d, post=%d", initialCalls, postCalls)
			}
		})
	}
}

func TestSubmitHandler_UnexpectedProviderError_MapsTo503(t *testing.T) {
	unexpectedErr := errors.New("synthetic provider database failure")
	brokenProvider := func(ctx context.Context, projectID, title, content string) (evidence.Snapshot, error) {
		return evidence.Snapshot{}, unexpectedErr
	}

	srv, store, _, _ := setupTestServerWithProvider(t, brokenProvider)

	reqBody := `{"idempotency_key":"key-err-1","title":"Title","content":"Valid content"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/proj-err/reviews", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer valid-token")
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable, got %d: %s", rec.Code, rec.Body.String())
	}

	errResp := parseErrorResponse(t, rec.Body)
	if errResp.Error.Code != "service_unavailable" {
		t.Errorf("error code = %q, want 'service_unavailable'", errResp.Error.Code)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.submitCalls) != 0 {
		t.Fatalf("store.Submit was called despite provider failure: %d calls", len(store.submitCalls))
	}
}
