package cline

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/provider"
)

func TestNewValidation(t *testing.T) {
	// Empty BaseURL
	if _, err := New(Config{APIKey: "key", Model: "model"}); err == nil {
		t.Error("expected error for empty BaseURL, got nil")
	}

	// Empty APIKey
	if _, err := New(Config{BaseURL: "http://example.com", Model: "model"}); err == nil {
		t.Error("expected error for empty APIKey, got nil")
	}

	// Empty Model
	if _, err := New(Config{BaseURL: "http://example.com", APIKey: "key"}); err == nil {
		t.Error("expected error for empty Model, got nil")
	}

	// Good config with trailing slash
	p, err := New(Config{
		BaseURL: "http://example.com/api/v1/",
		APIKey:  "secret-key",
		Model:   "deepseek/deepseek-v4.1-flash",
	})
	if err != nil {
		t.Fatalf("unexpected error for good config: %v", err)
	}
	if p.cfg.BaseURL != "http://example.com/api/v1" {
		t.Errorf("expected trimmed BaseURL, got %q", p.cfg.BaseURL)
	}
	if p.cfg.Timeout != defaultTimeout {
		t.Errorf("expected default timeout %v, got %v", defaultTimeout, p.cfg.Timeout)
	}
	if p.cfg.MaxTokens != defaultMaxTokens {
		t.Errorf("expected default max tokens %d, got %d", defaultMaxTokens, p.cfg.MaxTokens)
	}
}

func TestHappyPath(t *testing.T) {
	respPayload := `{
		"data": {
			"choices": [
				{
					"finish_reason": "stop",
					"index": 0,
					"message": {
						"content": "the content",
						"provider_metadata": {"cost": 0.001}
					}
				}
			],
			"usage": {
				"prompt_tokens": 42,
				"completion_tokens": 17
			}
		}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respPayload))
	}))
	defer srv.Close()

	p, err := New(Config{
		BaseURL:    srv.URL,
		APIKey:     "test-key",
		Model:      "deepseek/deepseek-v4.1-flash",
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := p.Call(context.Background(), provider.Request{
		Role:    domain.RoleArchitecture,
		Purpose: domain.PurposeInitial,
		Prompt:  "hello",
		Attempt: 1,
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if string(res.Body) != "the content" {
		t.Errorf("expected body %q, got %q", "the content", string(res.Body))
	}
	if res.Model != "deepseek/deepseek-v4.1-flash" {
		t.Errorf("expected model %q, got %q", "deepseek/deepseek-v4.1-flash", res.Model)
	}
	if res.TokensIn != 42 {
		t.Errorf("expected tokens in 42, got %d", res.TokensIn)
	}
	if res.TokensOut != 17 {
		t.Errorf("expected tokens out 17, got %d", res.TokensOut)
	}
}

func TestRequestShape(t *testing.T) {
	var (
		gotAuth        string
		gotContentType string
		gotMethod      string
		gotPath        string
		gotReqBody     chatRequest
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		gotMethod = r.Method
		gotPath = r.URL.Path

		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotReqBody)

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"choices":[{"message":{"content":"ok"}}]}}`))
	}))
	defer srv.Close()

	p, err := New(Config{
		BaseURL:    srv.URL,
		APIKey:     "secret-token-123",
		Model:      "deepseek/deepseek-v4.1-flash",
		MaxTokens:  512,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = p.Call(context.Background(), provider.Request{
		Role:    domain.RoleSecurity,
		Purpose: domain.PurposeInitial,
		Prompt:  "analyze this design",
		Attempt: 1,
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("expected method POST, got %q", gotMethod)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("expected path /chat/completions, got %q", gotPath)
	}
	if gotAuth != "Bearer secret-token-123" {
		t.Errorf("expected Authorization Bearer secret-token-123, got %q", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", gotContentType)
	}
	if gotReqBody.Model != "deepseek/deepseek-v4.1-flash" {
		t.Errorf("expected model deepseek/deepseek-v4.1-flash, got %q", gotReqBody.Model)
	}
	if gotReqBody.MaxTokens != 512 {
		t.Errorf("expected max_tokens 512, got %d", gotReqBody.MaxTokens)
	}
	if gotReqBody.Temperature != 0 {
		t.Errorf("expected temperature 0, got %d", gotReqBody.Temperature)
	}
	if gotReqBody.Stream != false {
		t.Errorf("expected stream false, got %v", gotReqBody.Stream)
	}
	if len(gotReqBody.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(gotReqBody.Messages))
	}
	if gotReqBody.Messages[0].Role != "user" || gotReqBody.Messages[0].Content != "analyze this design" {
		t.Errorf("unexpected message: %+v", gotReqBody.Messages[0])
	}
}

func TestFlaky500EmptyContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"empty response content","success":false}`))
	}))
	defer srv.Close()

	p, err := New(Config{
		BaseURL:    srv.URL,
		APIKey:     "test-key",
		Model:      "deepseek/deepseek-v4.1-flash",
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = p.Call(context.Background(), provider.Request{
		Role:   domain.RoleQA,
		Prompt: "test",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	cat := provider.CategoryOf(err)
	if cat != domain.ErrTransport {
		t.Errorf("expected domain.ErrTransport, got %v", cat)
	}
}

func Test200EmptyAndMalformed(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty choices", `{"data":{"choices":[]}}`},
		{"missing choices", `{"data":{}}`},
		{"empty content", `{"data":{"choices":[{"message":{"content":""}}]}}`},
		{"whitespace content", `{"data":{"choices":[{"message":{"content":"   \n\t"}}]}}`},
		{"missing data envelope", `{"choices":[{"message":{"content":"ok"}}]}`},
		{"malformed json", `{not-json`},
		{"empty body", ``},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			p, err := New(Config{
				BaseURL:    srv.URL,
				APIKey:     "test-key",
				Model:      "deepseek/deepseek-v4.1-flash",
				HTTPClient: srv.Client(),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			_, err = p.Call(context.Background(), provider.Request{
				Role:   domain.RoleQA,
				Prompt: "test",
			})
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			cat := provider.CategoryOf(err)
			if cat != domain.ErrTransport {
				t.Errorf("expected domain.ErrTransport for %s, got %v", tc.name, cat)
			}
		})
	}
}

func TestModelNotFound500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"Model deepseek/unknown not found","type":"model_not_found"}}`))
	}))
	defer srv.Close()

	p, err := New(Config{
		BaseURL:    srv.URL,
		APIKey:     "test-key",
		Model:      "deepseek/unknown",
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = p.Call(context.Background(), provider.Request{
		Role:   domain.RoleArchitecture,
		Prompt: "test",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	cat := provider.CategoryOf(err)
	if cat != domain.ErrProviderRejected {
		t.Errorf("expected domain.ErrProviderRejected, got %v", cat)
	}
}

func Test4xxClientErrors(t *testing.T) {
	codes := []struct {
		code int
		body string
	}{
		{http.StatusBadRequest, `{"error":"bad request"}`},
		{http.StatusUnauthorized, `{"error":"invalid api key"}`},
		{http.StatusForbidden, `{"error":"forbidden"}`},
		{http.StatusNotFound, `{"error":"endpoint not found"}`},
	}

	for _, tc := range codes {
		t.Run(http.StatusText(tc.code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			p, err := New(Config{
				BaseURL:    srv.URL,
				APIKey:     "test-key",
				Model:      "deepseek/deepseek-v4.1-flash",
				HTTPClient: srv.Client(),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			_, err = p.Call(context.Background(), provider.Request{
				Role:   domain.RoleArchitecture,
				Prompt: "test",
			})
			if err == nil {
				t.Fatal("expected error, got nil")
			}

			cat := provider.CategoryOf(err)
			if cat != domain.ErrProviderRejected {
				t.Errorf("status %d: expected domain.ErrProviderRejected, got %v", tc.code, cat)
			}
		})
	}
}

func TestRetryableStatuses(t *testing.T) {
	statuses := []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout}
	for _, status := range statuses {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`transient error`))
			}))
			defer srv.Close()

			p, err := New(Config{
				BaseURL:    srv.URL,
				APIKey:     "test-key",
				Model:      "deepseek/deepseek-v4.1-flash",
				HTTPClient: srv.Client(),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			_, err = p.Call(context.Background(), provider.Request{Prompt: "test"})
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if cat := provider.CategoryOf(err); cat != domain.ErrTransport {
				t.Errorf("status %d: expected domain.ErrTransport, got %v", status, cat)
			}
		})
	}
}

func TestTimeout(t *testing.T) {
	// Case A: pre-cancelled context
	p, err := New(Config{
		BaseURL: "http://example.com",
		APIKey:  "test-key",
		Model:   "deepseek/deepseek-v4.1-flash",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = p.Call(ctx, provider.Request{Prompt: "test"})
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
	if cat := provider.CategoryOf(err); cat != domain.ErrTimeout {
		t.Errorf("expected domain.ErrTimeout for cancelled context, got %v", cat)
	}

	// Case B: slow server exceeded adapter timeout
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"choices":[{"message":{"content":"late"}}]}}`))
	}))
	defer srv.Close()

	slowClientProvider, err := New(Config{
		BaseURL:    srv.URL,
		APIKey:     "test-key",
		Model:      "deepseek/deepseek-v4.1-flash",
		Timeout:    10 * time.Millisecond,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = slowClientProvider.Call(context.Background(), provider.Request{Prompt: "test"})
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if cat := provider.CategoryOf(err); cat != domain.ErrTimeout {
		t.Errorf("expected domain.ErrTimeout for slow server, got %v (err: %v)", cat, err)
	}
}

func TestKeyHygiene(t *testing.T) {
	secretKey := "super-secret-key-cline-xyz-987"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Even if the server echoes the authorization header or token
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized with token ` + secretKey + `"}`))
	}))
	defer srv.Close()

	p, err := New(Config{
		BaseURL:    srv.URL,
		APIKey:     secretKey,
		Model:      "deepseek/deepseek-v4.1-flash",
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = p.Call(context.Background(), provider.Request{Prompt: "test"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	errStr := err.Error()
	if strings.Contains(errStr, secretKey) {
		t.Errorf("error string leaked secret key: %s", errStr)
	}
}
