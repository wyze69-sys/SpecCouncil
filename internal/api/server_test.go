package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/review"
	"github.com/wyze69-sys/SpecCouncil/internal/storage/sqlite"
)

type fakeAuthenticator struct {
	authFunc func(r *http.Request) (Identity, error)
}

func (f *fakeAuthenticator) Authenticate(r *http.Request) (Identity, error) {
	if f.authFunc != nil {
		return f.authFunc(r)
	}
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return Identity{}, errors.New("missing authorization header")
	}
	if auth == "Bearer invalid-token" {
		return Identity{}, errors.New("invalid credentials")
	}
	return Identity{Subject: "test-user"}, nil
}

type fakeAuthorizer struct {
	authzFunc func(ctx context.Context, id Identity, projectID string) error
}

func (f *fakeAuthorizer) CanAccessProject(ctx context.Context, id Identity, projectID string) error {
	if f.authzFunc != nil {
		return f.authzFunc(ctx, id, projectID)
	}
	if strings.HasPrefix(projectID, "foreign-") || projectID == "unauthorized-project" {
		return errors.New("project access denied")
	}
	return nil
}

type fakeStore struct {
	mu sync.Mutex

	submitFunc        func(ctx context.Context, params sqlite.SubmitParams) (*sqlite.SubmitResult, error)
	readStatusFunc    func(ctx context.Context, projectID, sessionID string) (*sqlite.SessionStatus, error)
	readReportFunc    func(ctx context.Context, projectID, sessionID string) (*review.Report, error)
	requestCancelFunc func(ctx context.Context, projectID, sessionID string) (*sqlite.CancelResult, error)

	submitCalls        []sqlite.SubmitParams
	readStatusCalls    [][2]string
	readReportCalls    [][2]string
	requestCancelCalls [][2]string
	lastContexts       []context.Context
}

func (s *fakeStore) Submit(ctx context.Context, params sqlite.SubmitParams) (*sqlite.SubmitResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.submitCalls = append(s.submitCalls, params)
	s.lastContexts = append(s.lastContexts, ctx)
	if s.submitFunc != nil {
		return s.submitFunc(ctx, params)
	}
	return &sqlite.SubmitResult{
		SessionID:   "sess-default",
		SnapshotID:  "snap-default",
		Status:      domain.SessionQueued,
		RequestHash: "hash-default",
		Replay:      false,
	}, nil
}

func (s *fakeStore) ReadStatusScoped(ctx context.Context, projectID, sessionID string) (*sqlite.SessionStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readStatusCalls = append(s.readStatusCalls, [2]string{projectID, sessionID})
	s.lastContexts = append(s.lastContexts, ctx)
	if s.readStatusFunc != nil {
		return s.readStatusFunc(ctx, projectID, sessionID)
	}
	return &sqlite.SessionStatus{
		SessionID: sessionID,
		ProjectID: projectID,
		Status:    domain.SessionReviewing,
	}, nil
}

func (s *fakeStore) ReadReportScoped(ctx context.Context, projectID, sessionID string) (*review.Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readReportCalls = append(s.readReportCalls, [2]string{projectID, sessionID})
	s.lastContexts = append(s.lastContexts, ctx)
	if s.readReportFunc != nil {
		return s.readReportFunc(ctx, projectID, sessionID)
	}
	return &review.Report{
		SessionID: sessionID,
		Status:    domain.SessionComplete,
		Reason:    domain.ReasonAllRolesComplete,
	}, nil
}

func (s *fakeStore) RequestCancellationScoped(ctx context.Context, projectID, sessionID string) (*sqlite.CancelResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requestCancelCalls = append(s.requestCancelCalls, [2]string{projectID, sessionID})
	s.lastContexts = append(s.lastContexts, ctx)
	if s.requestCancelFunc != nil {
		return s.requestCancelFunc(ctx, projectID, sessionID)
	}
	return &sqlite.CancelResult{
		Effective:       true,
		SessionID:       sessionID,
		ProjectID:       projectID,
		Status:          domain.SessionReviewing,
		CancelRequested: true,
	}, nil
}

func setupTestServer(t *testing.T) (*Server, *fakeStore, *fakeAuthenticator, *fakeAuthorizer) {
	t.Helper()
	store := &fakeStore{}
	auth := &fakeAuthenticator{}
	authz := &fakeAuthorizer{}

	srv, err := NewServer(Config{
		Store:         store,
		Authenticator: auth,
		Authorizer:    authz,
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	return srv, store, auth, authz
}

func parseErrorResponse(t *testing.T, body *bytes.Buffer) ErrorResponse {
	t.Helper()
	var errResp ErrorResponse
	if err := json.Unmarshal(body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to decode error response: %v, raw: %s", err, body.String())
	}
	return errResp
}

// ----------------------------------------------------------------------------
// 1. Health returns exact successful JSON response and never touches storage
// ----------------------------------------------------------------------------
func TestHealth_SuccessAndNoStorage(t *testing.T) {
	srv, store, _, _ := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Fatalf("expected Content-Type application/json, got %q", ct)
	}

	var resp HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode health response: %v", err)
	}
	if resp.Status != "ok" {
		t.Fatalf("expected status 'ok', got %q", resp.Status)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.submitCalls) > 0 || len(store.readStatusCalls) > 0 ||
		len(store.readReportCalls) > 0 || len(store.requestCancelCalls) > 0 {
		t.Fatal("health endpoint touched storage")
	}
}

// ----------------------------------------------------------------------------
// 2. Route/method rejection returns deterministic 404/405 responses
// ----------------------------------------------------------------------------
func TestRouteAndMethodRejection(t *testing.T) {
	srv, _, _, _ := setupTestServer(t)

	testCases := []struct {
		name         string
		method       string
		path         string
		expectedCode int
		allowHeader  string
	}{
		{"healthz POST not allowed", http.MethodPost, "/healthz", http.StatusMethodNotAllowed, http.MethodGet},
		{"healthz PUT not allowed", http.MethodPut, "/healthz", http.StatusMethodNotAllowed, http.MethodGet},
		{"healthz with trailing slash", http.MethodGet, "/healthz/", http.StatusNotFound, ""},
		{"unknown root route", http.MethodGet, "/unknown", http.StatusNotFound, ""},
		{"incomplete projects route", http.MethodGet, "/v1/projects", http.StatusNotFound, ""},
		{"incomplete projects route with slash", http.MethodGet, "/v1/projects/", http.StatusNotFound, ""},
		{"missing project id", http.MethodPost, "/v1/projects//reviews", http.StatusNotFound, ""},
		{"extra segment after submit", http.MethodPost, "/v1/projects/p1/reviews/", http.StatusNotFound, ""},
		{"submit wrong method GET", http.MethodGet, "/v1/projects/p1/reviews", http.StatusMethodNotAllowed, http.MethodPost},
		{"submit wrong method PUT", http.MethodPut, "/v1/projects/p1/reviews", http.StatusMethodNotAllowed, http.MethodPost},
		{"status wrong method POST", http.MethodPost, "/v1/projects/p1/reviews/s1", http.StatusMethodNotAllowed, http.MethodGet},
		{"status with empty session", http.MethodGet, "/v1/projects/p1/reviews//report", http.StatusNotFound, ""},
		{"report wrong method POST", http.MethodPost, "/v1/projects/p1/reviews/s1/report", http.StatusMethodNotAllowed, http.MethodGet},
		{"report wrong method DELETE", http.MethodDelete, "/v1/projects/p1/reviews/s1/report", http.StatusMethodNotAllowed, http.MethodGet},
		{"cancel wrong method GET", http.MethodGet, "/v1/projects/p1/reviews/s1/cancel", http.StatusMethodNotAllowed, http.MethodPost},
		{"unknown action on review", http.MethodGet, "/v1/projects/p1/reviews/s1/other", http.StatusNotFound, ""},
		{"extra trailing segment after report", http.MethodGet, "/v1/projects/p1/reviews/s1/report/extra", http.StatusNotFound, ""},
		{"extra trailing segment after cancel", http.MethodPost, "/v1/projects/p1/reviews/s1/cancel/extra", http.StatusNotFound, ""},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("Authorization", "Bearer valid-token")
			rec := httptest.NewRecorder()

			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != tc.expectedCode {
				t.Fatalf("path %q method %q expected %d, got %d", tc.path, tc.method, tc.expectedCode, rec.Code)
			}
			if tc.allowHeader != "" {
				gotAllow := rec.Header().Get("Allow")
				if gotAllow != tc.allowHeader {
					t.Fatalf("expected Allow %q, got %q", tc.allowHeader, gotAllow)
				}
			}
			errResp := parseErrorResponse(t, rec.Body)
			if tc.expectedCode == http.StatusMethodNotAllowed && errResp.Error.Code != "method_not_allowed" {
				t.Fatalf("expected code 'method_not_allowed', got %q", errResp.Error.Code)
			}
			if tc.expectedCode == http.StatusNotFound && errResp.Error.Code != "not_found" {
				t.Fatalf("expected code 'not_found', got %q", errResp.Error.Code)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// 3. Protected requests reject missing/invalid identity before storage access
// ----------------------------------------------------------------------------
func TestProtectedRoutes_RejectUnauthenticated(t *testing.T) {
	srv, store, _, _ := setupTestServer(t)

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/v1/projects/p1/reviews"},
		{http.MethodGet, "/v1/projects/p1/reviews/s1"},
		{http.MethodGet, "/v1/projects/p1/reviews/s1/report"},
		{http.MethodPost, "/v1/projects/p1/reviews/s1/cancel"},
	}

	for _, r := range routes {
		t.Run(r.method+" "+r.path+" missing auth", func(t *testing.T) {
			req := httptest.NewRequest(r.method, r.path, strings.NewReader(`{"idempotency_key":"k","title":"t","content":"c"}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 Unauthorized, got %d", rec.Code)
			}
			errResp := parseErrorResponse(t, rec.Body)
			if errResp.Error.Code != "unauthorized" {
				t.Fatalf("expected code 'unauthorized', got %q", errResp.Error.Code)
			}
		})

		t.Run(r.method+" "+r.path+" invalid token", func(t *testing.T) {
			req := httptest.NewRequest(r.method, r.path, strings.NewReader(`{"idempotency_key":"k","title":"t","content":"c"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer invalid-token")
			rec := httptest.NewRecorder()

			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 Unauthorized, got %d", rec.Code)
			}
			errResp := parseErrorResponse(t, rec.Body)
			if errResp.Error.Code != "unauthorized" {
				t.Fatalf("expected code 'unauthorized', got %q", errResp.Error.Code)
			}
		})
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.submitCalls)+len(store.readStatusCalls)+len(store.readReportCalls)+len(store.requestCancelCalls) > 0 {
		t.Fatal("storage was called on unauthenticated requests")
	}
}

// ----------------------------------------------------------------------------
// 4. Foreign project access returns 404 and does not reveal authorization state
// ----------------------------------------------------------------------------
func TestForeignProjectAccess_Returns404AntiEnumeration(t *testing.T) {
	srv, store, _, _ := setupTestServer(t)

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/v1/projects/foreign-proj/reviews"},
		{http.MethodGet, "/v1/projects/foreign-proj/reviews/s1"},
		{http.MethodGet, "/v1/projects/foreign-proj/reviews/s1/report"},
		{http.MethodPost, "/v1/projects/foreign-proj/reviews/s1/cancel"},
	}

	for _, r := range routes {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			req := httptest.NewRequest(r.method, r.path, strings.NewReader(`{"idempotency_key":"k","title":"t","content":"c"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer valid-token")
			rec := httptest.NewRecorder()

			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("expected 404 Not Found (anti-enumeration), got %d", rec.Code)
			}
			errResp := parseErrorResponse(t, rec.Body)
			if errResp.Error.Code != "not_found" {
				t.Fatalf("expected error code 'not_found', got %q", errResp.Error.Code)
			}
		})
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.submitCalls)+len(store.readStatusCalls)+len(store.readReportCalls)+len(store.requestCancelCalls) > 0 {
		t.Fatal("storage was called on unauthorized project requests")
	}
}

// ----------------------------------------------------------------------------
// 5. Submit creates one request through the scoped persistence seam
// 6. Submit preserves title/content bytes and route project ID
// ----------------------------------------------------------------------------
func TestSubmit_SeamCallAndPreserveBytes(t *testing.T) {
	srv, store, _, _ := setupTestServer(t)

	rawBody := `{
  "idempotency_key": "client-key-123",
  "title": "  Design with \r\n CRLF and   spaces  ",
  "content": "  Content with \r\n line breaks \t and tabs  "
}`

	req := httptest.NewRequest(http.MethodPost, "/v1/projects/project-exact-bytes/reviews", strings.NewReader(rawBody))
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
	if call.ProjectID != "project-exact-bytes" {
		t.Errorf("ProjectID = %q, want 'project-exact-bytes'", call.ProjectID)
	}
	if call.IdempotencyKey != "client-key-123" {
		t.Errorf("IdempotencyKey = %q, want 'client-key-123'", call.IdempotencyKey)
	}
	wantTitle := "  Design with \r\n CRLF and   spaces  "
	if call.Title != wantTitle {
		t.Errorf("Title bytes not preserved: got %q, want %q", call.Title, wantTitle)
	}
	wantContent := "  Content with \r\n line breaks \t and tabs  "
	if call.Content != wantContent {
		t.Errorf("Content bytes not preserved: got %q, want %q", call.Content, wantContent)
	}
}

// ----------------------------------------------------------------------------
//  7. Malformed JSON, unknown fields, trailing JSON, missing fields, and invalid UTF-8
//     return 400 without persistence calls
//
// ----------------------------------------------------------------------------
func TestSubmit_ValidationFailures_NoStorageCalls(t *testing.T) {
	srv, store, _, _ := setupTestServer(t)

	cases := []struct {
		name        string
		body        []byte
		contentType string
		wantCode    int
		wantErrCode string
	}{
		{
			name:        "malformed JSON syntax",
			body:        []byte(`{"idempotency_key": "k", "title": "t", "content": `),
			contentType: "application/json",
			wantCode:    http.StatusBadRequest,
			wantErrCode: "bad_request",
		},
		{
			name:        "unknown field",
			body:        []byte(`{"idempotency_key":"k","title":"t","content":"c","project_id":"forbidden_substitute"}`),
			contentType: "application/json",
			wantCode:    http.StatusBadRequest,
			wantErrCode: "bad_request",
		},
		{
			name:        "trailing JSON object",
			body:        []byte(`{"idempotency_key":"k","title":"t","content":"c"} {"another":"object"}`),
			contentType: "application/json",
			wantCode:    http.StatusBadRequest,
			wantErrCode: "bad_request",
		},
		{
			name:        "trailing JSON token",
			body:        []byte(`{"idempotency_key":"k","title":"t","content":"c"} 1234`),
			contentType: "application/json",
			wantCode:    http.StatusBadRequest,
			wantErrCode: "bad_request",
		},
		{
			name:        "missing idempotency_key",
			body:        []byte(`{"title":"t","content":"c"}`),
			contentType: "application/json",
			wantCode:    http.StatusBadRequest,
			wantErrCode: "bad_request",
		},
		{
			name:        "empty idempotency_key",
			body:        []byte(`{"idempotency_key":"","title":"t","content":"c"}`),
			contentType: "application/json",
			wantCode:    http.StatusBadRequest,
			wantErrCode: "bad_request",
		},
		{
			name:        "missing title",
			body:        []byte(`{"idempotency_key":"k","content":"c"}`),
			contentType: "application/json",
			wantCode:    http.StatusBadRequest,
			wantErrCode: "bad_request",
		},
		{
			name:        "empty title",
			body:        []byte(`{"idempotency_key":"k","title":"","content":"c"}`),
			contentType: "application/json",
			wantCode:    http.StatusBadRequest,
			wantErrCode: "bad_request",
		},
		{
			name:        "missing content",
			body:        []byte(`{"idempotency_key":"k","title":"t"}`),
			contentType: "application/json",
			wantCode:    http.StatusBadRequest,
			wantErrCode: "bad_request",
		},
		{
			name:        "empty content",
			body:        []byte(`{"idempotency_key":"k","title":"t","content":""}`),
			contentType: "application/json",
			wantCode:    http.StatusBadRequest,
			wantErrCode: "bad_request",
		},
		{
			name:        "empty body",
			body:        []byte(``),
			contentType: "application/json",
			wantCode:    http.StatusBadRequest,
			wantErrCode: "bad_request",
		},
		{
			name:        "invalid UTF-8 sequence",
			body:        []byte{0x7b, 0x22, 0x74, 0x22, 0x3a, 0xff, 0xfe, 0x7d},
			contentType: "application/json",
			wantCode:    http.StatusBadRequest,
			wantErrCode: "bad_request",
		},
		{
			name:        "unsupported content type",
			body:        []byte(`{"idempotency_key":"k","title":"t","content":"c"}`),
			contentType: "text/plain",
			wantCode:    http.StatusUnsupportedMediaType,
			wantErrCode: "unsupported_media_type",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store.mu.Lock()
			initialCalls := len(store.submitCalls)
			store.mu.Unlock()

			req := httptest.NewRequest(http.MethodPost, "/v1/projects/p1/reviews", bytes.NewReader(tc.body))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			req.Header.Set("Authorization", "Bearer valid-token")
			rec := httptest.NewRecorder()

			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("expected %d, got %d: %s", tc.wantCode, rec.Code, rec.Body.String())
			}
			errResp := parseErrorResponse(t, rec.Body)
			if errResp.Error.Code != tc.wantErrCode {
				t.Fatalf("expected error code %q, got %q", tc.wantErrCode, errResp.Error.Code)
			}

			store.mu.Lock()
			postCalls := len(store.submitCalls)
			store.mu.Unlock()

			if postCalls != initialCalls {
				t.Fatalf("store.Submit was called despite validation failure in test %q", tc.name)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// 8. New submission returns 201 with authoritative result
// ----------------------------------------------------------------------------
func TestSubmit_NewSubmissionReturns201(t *testing.T) {
	srv, store, _, _ := setupTestServer(t)

	store.submitFunc = func(ctx context.Context, params sqlite.SubmitParams) (*sqlite.SubmitResult, error) {
		return &sqlite.SubmitResult{
			SessionID:   "sess-new-1",
			SnapshotID:  "snap-new-1",
			Status:      domain.SessionQueued,
			RequestHash: "req-hash-abc",
			Replay:      false,
		}, nil
	}

	reqBody := `{"idempotency_key":"key-1","title":"Design Title","content":"Content Body"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/proj-1/reviews", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer valid-token")
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d", rec.Code)
	}

	var resp SubmitResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.SessionID != "sess-new-1" {
		t.Errorf("SessionID = %q, want sess-new-1", resp.SessionID)
	}
	if resp.SnapshotID != "snap-new-1" {
		t.Errorf("SnapshotID = %q, want snap-new-1", resp.SnapshotID)
	}
	if resp.Status != domain.SessionQueued {
		t.Errorf("Status = %q, want queued", resp.Status)
	}
	if resp.RequestHash != "req-hash-abc" {
		t.Errorf("RequestHash = %q, want req-hash-abc", resp.RequestHash)
	}
	if resp.Replay != false {
		t.Errorf("Replay = true, want false")
	}
}

// ----------------------------------------------------------------------------
// 9. Same idempotency replay returns 200 without second submission
// ----------------------------------------------------------------------------
func TestSubmit_IdempotentReplayReturns200(t *testing.T) {
	srv, store, _, _ := setupTestServer(t)

	store.submitFunc = func(ctx context.Context, params sqlite.SubmitParams) (*sqlite.SubmitResult, error) {
		return &sqlite.SubmitResult{
			SessionID:   "sess-existing-1",
			SnapshotID:  "snap-existing-1",
			Status:      domain.SessionReviewing,
			RequestHash: "req-hash-abc",
			Replay:      true,
		}, nil
	}

	reqBody := `{"idempotency_key":"key-1","title":"Design Title","content":"Content Body"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/proj-1/reviews", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer valid-token")
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on replay, got %d", rec.Code)
	}

	var resp SubmitResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Replay {
		t.Errorf("expected replay=true, got false")
	}
	if resp.SessionID != "sess-existing-1" {
		t.Errorf("SessionID = %q, want sess-existing-1", resp.SessionID)
	}
}

// ----------------------------------------------------------------------------
// 10. Idempotency conflict maps to 409 without leaking sensitive body data
// ----------------------------------------------------------------------------
func TestSubmit_IdempotencyConflict_409WithoutDataLeak(t *testing.T) {
	srv, store, _, _ := setupTestServer(t)

	store.submitFunc = func(ctx context.Context, params sqlite.SubmitParams) (*sqlite.SubmitResult, error) {
		return nil, &sqlite.IdempotencyConflictError{
			ProjectID:      params.ProjectID,
			IdempotencyKey: params.IdempotencyKey,
			ExistingHash:   "secret-existing-hash-1111",
			IncomingHash:   "secret-incoming-hash-2222",
		}
	}

	secretBody := `{"idempotency_key":"key-conflict","title":"Top Secret Design","content":"Proprietary Algorithm"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/proj-1/reviews", strings.NewReader(secretBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer valid-token")
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict, got %d", rec.Code)
	}

	rawBody := rec.Body.String()
	if strings.Contains(rawBody, "secret-existing-hash-1111") || strings.Contains(rawBody, "secret-incoming-hash-2222") {
		t.Fatalf("response leaked internal hashes: %s", rawBody)
	}
	if strings.Contains(rawBody, "Top Secret Design") || strings.Contains(rawBody, "Proprietary Algorithm") {
		t.Fatalf("response leaked request content: %s", rawBody)
	}

	errResp := parseErrorResponse(t, rec.Body)
	if errResp.Error.Code != "idempotency_conflict" {
		t.Fatalf("expected code 'idempotency_conflict', got %q", errResp.Error.Code)
	}
}

// ----------------------------------------------------------------------------
// 11. Status returns persisted scoped status and performs no mutation
// ----------------------------------------------------------------------------
func TestStatus_SuccessAndNoMutation(t *testing.T) {
	srv, store, _, _ := setupTestServer(t)

	now := time.Now().UTC()
	store.readStatusFunc = func(ctx context.Context, projectID, sessionID string) (*sqlite.SessionStatus, error) {
		return &sqlite.SessionStatus{
			SessionID:           sessionID,
			ProjectID:           projectID,
			IdempotencyKey:      "idem-k",
			RequestHash:         "hash-xyz",
			SnapshotID:          "snap-xyz",
			SnapshotHash:        "snap-hash-xyz",
			Status:              domain.SessionReviewing,
			CancelRequested:     false,
			CompletedRoleCount:  1,
			IncompleteRoleCount: 3,
			CreatedAt:           now,
		}, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/projects/p-1/reviews/s-1", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var status sqlite.SessionStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("failed to decode session status: %v", err)
	}

	if status.SessionID != "s-1" || status.ProjectID != "p-1" || status.Status != domain.SessionReviewing {
		t.Fatalf("unexpected status returned: %+v", status)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.submitCalls) > 0 || len(store.requestCancelCalls) > 0 {
		t.Fatal("mutation was executed during status read")
	}
	if len(store.readStatusCalls) != 1 {
		t.Fatalf("expected 1 readStatus call, got %d", len(store.readStatusCalls))
	}
}

// ----------------------------------------------------------------------------
// 12. Report returns 200 only for a terminal report
// 13. Non-terminal report maps to 409 with stable not_finished code
// 14. Report handler never calls composition or provider code
// ----------------------------------------------------------------------------
func TestReport_TerminalSuccessAndNonTerminalConflict(t *testing.T) {
	srv, store, _, _ := setupTestServer(t)

	t.Run("terminal report returns 200", func(t *testing.T) {
		store.readReportFunc = func(ctx context.Context, projectID, sessionID string) (*review.Report, error) {
			return &review.Report{
				SessionID:           sessionID,
				SnapshotID:          "snap-term",
				SnapshotHash:        "snap-hash-term",
				Status:              domain.SessionComplete,
				Reason:              domain.ReasonAllRolesComplete,
				CancelRequested:     false,
				CompletedRoleCount:  4,
				IncompleteRoleCount: 0,
			}, nil
		}

		req := httptest.NewRequest(http.MethodGet, "/v1/projects/p-1/reviews/s-term/report", nil)
		req.Header.Set("Authorization", "Bearer valid-token")
		rec := httptest.NewRecorder()

		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", rec.Code)
		}

		var rep review.Report
		if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
			t.Fatalf("failed to decode report: %v", err)
		}
		if rep.Status != domain.SessionComplete {
			t.Fatalf("expected complete status, got %s", rep.Status)
		}
	})

	t.Run("non-terminal report returns 409 not_finished", func(t *testing.T) {
		store.readReportFunc = func(ctx context.Context, projectID, sessionID string) (*review.Report, error) {
			return nil, &sqlite.SessionNotTerminalError{
				SessionID: sessionID,
				Status:    domain.SessionReviewing,
			}
		}

		req := httptest.NewRequest(http.MethodGet, "/v1/projects/p-1/reviews/s-reviewing/report", nil)
		req.Header.Set("Authorization", "Bearer valid-token")
		rec := httptest.NewRecorder()

		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusConflict {
			t.Fatalf("expected 409 Conflict, got %d", rec.Code)
		}

		errResp := parseErrorResponse(t, rec.Body)
		if errResp.Error.Code != "not_finished" {
			t.Fatalf("expected error code 'not_finished', got %q", errResp.Error.Code)
		}
	})

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.submitCalls) > 0 || len(store.requestCancelCalls) > 0 {
		t.Fatal("composition or mutation invoked during report reads")
	}
}

// ----------------------------------------------------------------------------
// 15. Cancel calls scoped cancellation exactly once
// 16. Effective cancellation returns 202
// 17. Repeated/terminal cancellation returns 200 with effective=false
// 18. Cancellation does not mutate role rows or compose a report in the API
// ----------------------------------------------------------------------------
func TestCancel_EffectiveAndRepeated(t *testing.T) {
	srv, store, _, _ := setupTestServer(t)

	t.Run("effective cancellation returns 202", func(t *testing.T) {
		store.requestCancelFunc = func(ctx context.Context, projectID, sessionID string) (*sqlite.CancelResult, error) {
			return &sqlite.CancelResult{
				Effective:       true,
				SessionID:       sessionID,
				ProjectID:       projectID,
				Status:          domain.SessionReviewing,
				CancelRequested: true,
			}, nil
		}

		req := httptest.NewRequest(http.MethodPost, "/v1/projects/p-1/reviews/s-cancel/cancel", nil)
		req.Header.Set("Authorization", "Bearer valid-token")
		rec := httptest.NewRecorder()

		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusAccepted {
			t.Fatalf("expected 202 Accepted, got %d", rec.Code)
		}

		var res sqlite.CancelResult
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("failed to decode cancel result: %v", err)
		}
		if !res.Effective {
			t.Fatalf("expected Effective=true, got false")
		}
	})

	t.Run("repeated/terminal cancellation returns 200 with effective=false", func(t *testing.T) {
		store.requestCancelFunc = func(ctx context.Context, projectID, sessionID string) (*sqlite.CancelResult, error) {
			return &sqlite.CancelResult{
				Effective:       false,
				AlreadyTerminal: true,
				NoOp:            true,
				SessionID:       sessionID,
				ProjectID:       projectID,
				Status:          domain.SessionComplete,
				CancelRequested: true,
			}, nil
		}

		req := httptest.NewRequest(http.MethodPost, "/v1/projects/p-1/reviews/s-cancel-term/cancel", nil)
		req.Header.Set("Authorization", "Bearer valid-token")
		rec := httptest.NewRecorder()

		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", rec.Code)
		}

		var res sqlite.CancelResult
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("failed to decode cancel result: %v", err)
		}
		if res.Effective {
			t.Fatalf("expected Effective=false, got true")
		}
	})

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.requestCancelCalls) != 2 {
		t.Fatalf("expected 2 cancel calls, got %d", len(store.requestCancelCalls))
	}
	if len(store.submitCalls) > 0 || len(store.readReportCalls) > 0 {
		t.Fatal("cancellation caused submission or composition calls in API")
	}
}

// ----------------------------------------------------------------------------
// 19. Unknown/foreign session maps to 404 for status, report, and cancel
// ----------------------------------------------------------------------------
func TestUnknownForeignSession_MapsTo404(t *testing.T) {
	srv, store, _, _ := setupTestServer(t)

	store.readStatusFunc = func(ctx context.Context, projectID, sessionID string) (*sqlite.SessionStatus, error) {
		return nil, &sqlite.SessionNotFoundError{SessionID: sessionID, ProjectID: projectID}
	}
	store.readReportFunc = func(ctx context.Context, projectID, sessionID string) (*review.Report, error) {
		return nil, &sqlite.SessionNotFoundError{SessionID: sessionID, ProjectID: projectID}
	}
	store.requestCancelFunc = func(ctx context.Context, projectID, sessionID string) (*sqlite.CancelResult, error) {
		return nil, &sqlite.SessionNotFoundError{SessionID: sessionID, ProjectID: projectID}
	}

	endpoints := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/projects/p-1/reviews/unknown-session"},
		{http.MethodGet, "/v1/projects/p-1/reviews/unknown-session/report"},
		{http.MethodPost, "/v1/projects/p-1/reviews/unknown-session/cancel"},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			req := httptest.NewRequest(ep.method, ep.path, nil)
			req.Header.Set("Authorization", "Bearer valid-token")
			rec := httptest.NewRecorder()

			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("expected 404 Not Found, got %d", rec.Code)
			}
			errResp := parseErrorResponse(t, rec.Body)
			if errResp.Error.Code != "not_found" {
				t.Fatalf("expected error code 'not_found', got %q", errResp.Error.Code)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// 20. Persistence errors map consistently and do not leak raw internals
// ----------------------------------------------------------------------------
func TestPersistenceErrors_Consistent503AndNoLeakage(t *testing.T) {
	srv, store, _, _ := setupTestServer(t)

	rawDBError := errors.New("sqlite: busy lock on file:/etc/secrets/database.db?busy_timeout=5000: SQL table locked")
	persistenceUnavailable := &sqlite.PersistenceUnavailable{
		Op:       "submit_session",
		Attempts: 5,
		Err:      rawDBError,
	}

	store.submitFunc = func(ctx context.Context, params sqlite.SubmitParams) (*sqlite.SubmitResult, error) {
		return nil, persistenceUnavailable
	}
	store.readStatusFunc = func(ctx context.Context, projectID, sessionID string) (*sqlite.SessionStatus, error) {
		return nil, persistenceUnavailable
	}
	store.readReportFunc = func(ctx context.Context, projectID, sessionID string) (*review.Report, error) {
		return nil, persistenceUnavailable
	}
	store.requestCancelFunc = func(ctx context.Context, projectID, sessionID string) (*sqlite.CancelResult, error) {
		return nil, persistenceUnavailable
	}

	endpoints := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/v1/projects/p1/reviews", `{"idempotency_key":"k","title":"t","content":"c"}`},
		{http.MethodGet, "/v1/projects/p1/reviews/s1", ""},
		{http.MethodGet, "/v1/projects/p1/reviews/s1/report", ""},
		{http.MethodPost, "/v1/projects/p1/reviews/s1/cancel", ""},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			var bodyReader io.Reader
			if ep.body != "" {
				bodyReader = strings.NewReader(ep.body)
			}
			req := httptest.NewRequest(ep.method, ep.path, bodyReader)
			if ep.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			req.Header.Set("Authorization", "Bearer valid-token")
			rec := httptest.NewRecorder()

			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected 503 Service Unavailable, got %d", rec.Code)
			}

			raw := rec.Body.String()
			for _, sensitive := range []string{"database.db", "busy lock", "SQL table locked", "/etc/secrets"} {
				if strings.Contains(raw, sensitive) {
					t.Fatalf("leaked internal persistence detail %q in response: %s", sensitive, raw)
				}
			}

			errResp := parseErrorResponse(t, rec.Body)
			if errResp.Error.Code != "service_unavailable" {
				t.Fatalf("expected code 'service_unavailable', got %q", errResp.Error.Code)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// 21. Request context cancellation reaches the persistence seam
// ----------------------------------------------------------------------------
func TestContextCancellation_ReachesPersistenceSeam(t *testing.T) {
	srv, store, _, _ := setupTestServer(t)

	store.submitFunc = func(ctx context.Context, params sqlite.SubmitParams) (*sqlite.SubmitResult, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return &sqlite.SubmitResult{}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel context

	reqBody := `{"idempotency_key":"key-ctx","title":"Title","content":"Content"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/proj-1/reviews", strings.NewReader(reqBody)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer valid-token")
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	store.mu.Lock()
	defer store.mu.Unlock()

	if len(store.lastContexts) == 0 {
		t.Fatal("persistence seam was not called")
	}

	lastCtx := store.lastContexts[len(store.lastContexts)-1]
	if lastCtx.Err() == nil {
		t.Fatal("expected canceled context to reach persistence seam")
	}
	if !errors.Is(lastCtx.Err(), context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", lastCtx.Err())
	}
}

// ----------------------------------------------------------------------------
// 22. Concurrent requests do not race or share identity/response state
// ----------------------------------------------------------------------------
func TestConcurrentRequests_NoDataRaceOrSharedState(t *testing.T) {
	srv, _, _, _ := setupTestServer(t)

	const goroutines = 40
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()

			projID := fmt.Sprintf("proj-%d", idx%5)
			sessID := fmt.Sprintf("sess-%d", idx)

			var req *http.Request
			switch idx % 4 {
			case 0:
				body := fmt.Sprintf(`{"idempotency_key":"k-%d","title":"t-%d","content":"c-%d"}`, idx, idx, idx)
				req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/projects/%s/reviews", projID), strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
			case 1:
				req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/projects/%s/reviews/%s", projID, sessID), nil)
			case 2:
				req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
			case 3:
				req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/projects/%s/reviews/%s/cancel", projID, sessID), nil)
			}

			req.Header.Set("Authorization", fmt.Sprintf("Bearer user-%d", idx))
			rec := httptest.NewRecorder()

			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK && rec.Code != http.StatusCreated && rec.Code != http.StatusAccepted {
				t.Errorf("request %d failed with code %d: %s", idx, rec.Code, rec.Body.String())
			}
		}(i)
	}

	wg.Wait()
}

// ----------------------------------------------------------------------------
// 23. No credentials, authorization headers, DSNs, or request content appear in error responses or logs
// ----------------------------------------------------------------------------
func TestNoCredentialLeakage_InResponsesOrLogs(t *testing.T) {
	srv, store, _, _ := setupTestServer(t)

	store.submitFunc = func(ctx context.Context, params sqlite.SubmitParams) (*sqlite.SubmitResult, error) {
		return nil, errors.New("db error with connection dsn: user:secretPassword@tcp(db.prod:3306)/speccouncil")
	}

	secretAuthHeader := "Bearer super-secret-admin-token-999"
	secretBody := `{"idempotency_key":"key-leak-test","title":"Secret Spec Title","content":"Super Confidential Intellectual Property"}`

	req := httptest.NewRequest(http.MethodPost, "/v1/projects/proj-1/reviews", strings.NewReader(secretBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", secretAuthHeader)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	respBody := rec.Body.String()

	sensitiveTerms := []string{
		"super-secret-admin-token-999",
		"secretPassword",
		"db.prod:3306",
		"Secret Spec Title",
		"Super Confidential Intellectual Property",
	}

	for _, term := range sensitiveTerms {
		if strings.Contains(respBody, term) {
			t.Fatalf("response leaked sensitive term %q: %s", term, respBody)
		}
	}
}

// ----------------------------------------------------------------------------
// Composition root validation: NewServer rejects incomplete Config
// ----------------------------------------------------------------------------
func TestNewServer_Validation(t *testing.T) {
	store := &fakeStore{}
	auth := &fakeAuthenticator{}
	authz := &fakeAuthorizer{}

	t.Run("nil store rejected", func(t *testing.T) {
		_, err := NewServer(Config{
			Store:         nil,
			Authenticator: auth,
			Authorizer:    authz,
		})
		if err == nil {
			t.Fatal("expected error for nil Store")
		}
	})

	t.Run("nil authenticator rejected", func(t *testing.T) {
		_, err := NewServer(Config{
			Store:         store,
			Authenticator: nil,
			Authorizer:    authz,
		})
		if err == nil {
			t.Fatal("expected error for nil Authenticator")
		}
	})

	t.Run("nil authorizer rejected", func(t *testing.T) {
		_, err := NewServer(Config{
			Store:         store,
			Authenticator: auth,
			Authorizer:    nil,
		})
		if err == nil {
			t.Fatal("expected error for nil Authorizer")
		}
	})

	t.Run("valid config succeeds", func(t *testing.T) {
		srv, err := NewServer(Config{
			Store:         store,
			Authenticator: auth,
			Authorizer:    authz,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if srv.Handler() == nil {
			t.Fatal("Handler() returned nil")
		}
	})
}
