package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
	"github.com/wyze69-sys/SpecCouncil/internal/review"
	"github.com/wyze69-sys/SpecCouncil/internal/storage/sqlite"
)

// Identity represents an authenticated subject.
type Identity struct {
	Subject string
}

// Authenticator authenticates incoming requests.
type Authenticator interface {
	Authenticate(*http.Request) (Identity, error)
}

// Authorizer checks whether an authenticated identity is authorized to access a project.
type Authorizer interface {
	CanAccessProject(context.Context, Identity, string) error
}

// Store defines the scoped persistence contract required by the HTTP API.
// It is satisfied directly by *sqlite.Store or test adapters.
type Store interface {
	Submit(ctx context.Context, params sqlite.SubmitParams) (*sqlite.SubmitResult, error)
	ReadStatusScoped(ctx context.Context, projectID, sessionID string) (*sqlite.SessionStatus, error)
	ReadReportScoped(ctx context.Context, projectID, sessionID string) (*review.Report, error)
	RequestCancellationScoped(ctx context.Context, projectID, sessionID string) (*sqlite.CancelResult, error)
}

// SnapshotProvider optionally supplies or resolves an evidence.Snapshot for submission.
type SnapshotProvider func(ctx context.Context, projectID, title, content string) (evidence.Snapshot, error)

// Config specifies dependencies required to construct a Server.
type Config struct {
	Store            Store
	Authenticator    Authenticator
	Authorizer       Authorizer
	SnapshotProvider SnapshotProvider // optional
}

// Server is the HTTP API composition root.
type Server struct {
	store            Store
	authenticator    Authenticator
	authorizer       Authorizer
	snapshotProvider SnapshotProvider
	mux              http.Handler
}

// NewServer validates required dependencies and constructs a new Server.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Store == nil {
		return nil, errors.New("api: store is required")
	}
	if cfg.Authenticator == nil {
		return nil, errors.New("api: authenticator is required")
	}
	if cfg.Authorizer == nil {
		return nil, errors.New("api: authorizer is required")
	}

	s := &Server{
		store:            cfg.Store,
		authenticator:    cfg.Authenticator,
		authorizer:       cfg.Authorizer,
		snapshotProvider: cfg.SnapshotProvider,
	}
	s.mux = s.buildRoutes()
	return s, nil
}

// Handler returns the HTTP handler for the server.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// ErrorDetail describes a machine-readable error code and human-readable message.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ErrorResponse represents the stable JSON error envelope.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// HealthResponse represents the successful health response.
type HealthResponse struct {
	Status string `json:"status"`
}

// SubmitResponse represents the successful submission response.
type SubmitResponse struct {
	SessionID   string               `json:"session_id"`
	ReviewID    string               `json:"review_id,omitempty"`
	SnapshotID  string               `json:"snapshot_id,omitempty"`
	Status      domain.SessionStatus `json:"status"`
	RequestHash string               `json:"request_hash,omitempty"`
	Replay      bool                 `json:"replay"`
}

type submitRequestBody struct {
	IdempotencyKey *string `json:"idempotency_key"`
	Title          *string `json:"title"`
	Content        *string `json:"content"`
}

func writeJSON(w http.ResponseWriter, status int, val any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(val)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorResponse{
		Error: ErrorDetail{
			Code:    code,
			Message: message,
		},
	})
}

func (s *Server) handlePersistenceError(w http.ResponseWriter, err error) {
	if errors.Is(err, sqlite.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	// Never leak DSNs, SQL queries, credentials, or internal details
	writeError(w, http.StatusServiceUnavailable, "service_unavailable", "service unavailable")
}

func (s *Server) checkAuth(w http.ResponseWriter, r *http.Request, projectID string) (Identity, bool) {
	identity, err := s.authenticator.Authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return Identity{}, false
	}

	if err := s.authorizer.CanAccessProject(r.Context(), identity, projectID); err != nil {
		// 404 anti-enumeration: inaccessible or foreign project returns 404, not 403
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return Identity{}, false
	}

	return identity, true
}

func (s *Server) buildRoutes() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Health endpoint: GET /healthz
		if path == "/healthz" {
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", http.MethodGet)
				writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
				return
			}
			writeJSON(w, http.StatusOK, HealthResponse{Status: "ok"})
			return
		}

		// Protected endpoints must match /v1/projects/{project_id}/reviews...
		if !strings.HasPrefix(path, "/v1/projects/") {
			writeError(w, http.StatusNotFound, "not_found", "not found")
			return
		}

		// Split path into segments
		// Valid paths start with "/", so segments[0] == ""
		segments := strings.Split(path, "/")
		if len(segments) < 5 {
			writeError(w, http.StatusNotFound, "not_found", "not found")
			return
		}

		projectID := segments[3]
		if projectID == "" {
			writeError(w, http.StatusNotFound, "not_found", "not found")
			return
		}

		if segments[4] != "reviews" {
			writeError(w, http.StatusNotFound, "not_found", "not found")
			return
		}

		switch len(segments) {
		case 5:
			// POST /v1/projects/{project_id}/reviews
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", http.MethodPost)
				writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
				return
			}
			s.handleSubmit(w, r, projectID)

		case 6:
			// GET /v1/projects/{project_id}/reviews/{session_id}
			sessionID := segments[5]
			if sessionID == "" {
				writeError(w, http.StatusNotFound, "not_found", "not found")
				return
			}
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", http.MethodGet)
				writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
				return
			}
			s.handleStatus(w, r, projectID, sessionID)

		case 7:
			// /v1/projects/{project_id}/reviews/{session_id}/{action}
			sessionID := segments[5]
			action := segments[6]
			if sessionID == "" || action == "" {
				writeError(w, http.StatusNotFound, "not_found", "not found")
				return
			}

			switch action {
			case "report":
				// GET /v1/projects/{project_id}/reviews/{session_id}/report
				if r.Method != http.MethodGet {
					w.Header().Set("Allow", http.MethodGet)
					writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
					return
				}
				s.handleReport(w, r, projectID, sessionID)

			case "cancel":
				// POST /v1/projects/{project_id}/reviews/{session_id}/cancel
				if r.Method != http.MethodPost {
					w.Header().Set("Allow", http.MethodPost)
					writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
					return
				}
				s.handleCancel(w, r, projectID, sessionID)

			default:
				writeError(w, http.StatusNotFound, "not_found", "not found")
			}

		default:
			// Extra path segments are rejected
			writeError(w, http.StatusNotFound, "not_found", "not found")
		}
	})
}

func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request, projectID string) {
	_, ok := s.checkAuth(w, r, projectID)
	if !ok {
		return
	}

	// Content-Type validation when header is provided
	ct := r.Header.Get("Content-Type")
	if ct != "" {
		mediaType, _, _ := mime.ParseMediaType(ct)
		if mediaType != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
			return
		}
	}

	if r.Body == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "request body must not be empty")
		return
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 10<<20)) // 10MB bound
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "failed to read request body")
		return
	}
	if len(bodyBytes) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "request body must not be empty")
		return
	}

	if !utf8.Valid(bodyBytes) {
		writeError(w, http.StatusBadRequest, "bad_request", "request body contains invalid UTF-8")
		return
	}

	decoder := json.NewDecoder(bytes.NewReader(bodyBytes))
	decoder.DisallowUnknownFields()

	var req submitRequestBody
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "malformed JSON request body")
		return
	}

	// Check for trailing JSON values
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, "bad_request", "unexpected trailing data after JSON body")
		return
	}

	if req.IdempotencyKey == nil || *req.IdempotencyKey == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "idempotency_key is required")
		return
	}
	if req.Title == nil || *req.Title == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "title is required")
		return
	}
	if req.Content == nil || *req.Content == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "content is required")
		return
	}

	params := sqlite.SubmitParams{
		ProjectID:      projectID,
		IdempotencyKey: *req.IdempotencyKey,
		Title:          *req.Title,
		Content:        *req.Content,
	}

	if s.snapshotProvider != nil {
		snap, err := s.snapshotProvider(r.Context(), projectID, *req.Title, *req.Content)
		if err != nil {
			s.handlePersistenceError(w, err)
			return
		}
		params.Snapshot = snap
	}

	res, err := s.store.Submit(r.Context(), params)
	if err != nil {
		if errors.Is(err, sqlite.ErrIdempotencyConflict) {
			writeError(w, http.StatusConflict, "idempotency_conflict", "idempotency conflict for project and key")
			return
		}
		s.handlePersistenceError(w, err)
		return
	}

	statusCode := http.StatusCreated
	if res.Replay {
		statusCode = http.StatusOK
	}

	writeJSON(w, statusCode, SubmitResponse{
		SessionID:   res.SessionID,
		ReviewID:    res.SessionID,
		SnapshotID:  res.SnapshotID,
		Status:      res.Status,
		RequestHash: res.RequestHash,
		Replay:      res.Replay,
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request, projectID, sessionID string) {
	_, ok := s.checkAuth(w, r, projectID)
	if !ok {
		return
	}

	status, err := s.store.ReadStatusScoped(r.Context(), projectID, sessionID)
	if err != nil {
		if errors.Is(err, sqlite.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "not found")
			return
		}
		s.handlePersistenceError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request, projectID, sessionID string) {
	_, ok := s.checkAuth(w, r, projectID)
	if !ok {
		return
	}

	report, err := s.store.ReadReportScoped(r.Context(), projectID, sessionID)
	if err != nil {
		if errors.Is(err, sqlite.ErrNotFinished) || errors.Is(err, sqlite.ErrNotTerminal) {
			writeError(w, http.StatusConflict, "not_finished", "review report is not finished")
			return
		}
		if errors.Is(err, sqlite.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "not found")
			return
		}
		s.handlePersistenceError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, report)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request, projectID, sessionID string) {
	_, ok := s.checkAuth(w, r, projectID)
	if !ok {
		return
	}

	if r.Body != nil {
		bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err == nil && len(bodyBytes) > 0 {
			if !utf8.Valid(bodyBytes) {
				writeError(w, http.StatusBadRequest, "bad_request", "request body contains invalid UTF-8")
				return
			}
			var dummy any
			dec := json.NewDecoder(bytes.NewReader(bodyBytes))
			if err := dec.Decode(&dummy); err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "malformed JSON request body")
				return
			}
		}
	}

	res, err := s.store.RequestCancellationScoped(r.Context(), projectID, sessionID)
	if err != nil {
		if errors.Is(err, sqlite.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "not found")
			return
		}
		s.handlePersistenceError(w, err)
		return
	}

	statusCode := http.StatusOK
	if res.Effective {
		statusCode = http.StatusAccepted
	}

	writeJSON(w, statusCode, res)
}
