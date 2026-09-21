package cline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/provider"
)

const (
	defaultTimeout   = 60 * time.Second
	defaultMaxTokens = 1024
)

// Config is the frozen, per-review configuration of the Cline adapter.
type Config struct {
	BaseURL    string        // e.g. "https://api.cline.bot/api/v1"
	APIKey     string        // bearer token; never logged
	Model      string        // e.g. "deepseek/deepseek-v4.1-flash"
	HTTPClient *http.Client  // optional; if nil, a client with Timeout is built
	Timeout    time.Duration // per-call ceiling; if zero, default 60s
	MaxTokens  int           // output cap; if zero, a sane default (e.g. 1024)
}

// Provider implements provider.Provider for the Cline OpenAI-compatible HTTP API.
type Provider struct {
	cfg    Config
	client *http.Client
}

var _ provider.Provider = (*Provider)(nil)

// New builds a Cline provider adapter.
//
// New validates that BaseURL, APIKey, and Model are non-empty. It trims any
// trailing slash from BaseURL. It never reads keys from disk or environment.
func New(cfg Config) (*Provider, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("baseURL is required")
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("apiKey is required")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("model is required")
	}

	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")

	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = defaultMaxTokens
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: cfg.Timeout,
		}
	}

	return &Provider{
		cfg:    cfg,
		client: client,
	}, nil
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature int           `json:"temperature"`
	Stream      bool          `json:"stream"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type clineResponse struct {
	Data struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	} `json:"data"`
}

// Call executes one provider attempt against the Cline endpoint.
func (p *Provider) Call(ctx context.Context, req provider.Request) (provider.Response, error) {
	if ctx.Err() != nil {
		return provider.Response{}, provider.NewError(domain.ErrTimeout, "context canceled: %v", ctx.Err())
	}

	callCtx := ctx
	if p.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, p.cfg.Timeout)
		defer cancel()
	}

	reqPayload := chatRequest{
		Model: p.cfg.Model,
		Messages: []chatMessage{
			{Role: "user", Content: req.Prompt},
		},
		MaxTokens:   p.cfg.MaxTokens,
		Temperature: 0,
		Stream:      false,
	}

	bodyBytes, err := json.Marshal(reqPayload)
	if err != nil {
		return provider.Response{}, provider.NewError(domain.ErrTransport, "marshal request: %v", err)
	}

	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, p.cfg.BaseURL+"/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return provider.Response{}, provider.NewError(domain.ErrTransport, "create request: %v", p.sanitize(err.Error()))
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := p.client.Do(httpReq)
	if err != nil {
		if callCtx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return provider.Response{}, provider.NewError(domain.ErrTimeout, "request timed out: %v", p.sanitize(err.Error()))
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return provider.Response{}, provider.NewError(domain.ErrTimeout, "request timed out: %v", p.sanitize(err.Error()))
		}
		return provider.Response{}, provider.NewError(domain.ErrTransport, "transport error: %v", p.sanitize(err.Error()))
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		if callCtx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return provider.Response{}, provider.NewError(domain.ErrTimeout, "read response timed out: %v", p.sanitize(err.Error()))
		}
		return provider.Response{}, provider.NewError(domain.ErrTransport, "read response: %v", p.sanitize(err.Error()))
	}

	if httpResp.StatusCode == http.StatusOK {
		var cResp clineResponse
		if err := json.Unmarshal(respBody, &cResp); err != nil {
			return provider.Response{}, provider.NewError(domain.ErrTransport, "malformed response: %v", p.sanitize(err.Error()))
		}
		if len(cResp.Data.Choices) == 0 {
			return provider.Response{}, provider.NewError(domain.ErrTransport, "empty response content: missing choices")
		}
		content := cResp.Data.Choices[0].Message.Content
		if strings.TrimSpace(content) == "" {
			return provider.Response{}, provider.NewError(domain.ErrTransport, "empty response content")
		}
		return provider.Response{
			Body:      []byte(content),
			Model:     p.cfg.Model,
			TokensIn:  cResp.Data.Usage.PromptTokens,
			TokensOut: cResp.Data.Usage.CompletionTokens,
		}, nil
	}

	bodySnippet := p.sanitize(string(respBody))

	switch {
	case httpResp.StatusCode == http.StatusTooManyRequests:
		return provider.Response{}, provider.NewError(domain.ErrTransport, "http 429 rate limited: %s", bodySnippet)
	case httpResp.StatusCode == http.StatusInternalServerError:
		if strings.Contains(string(respBody), "model_not_found") {
			return provider.Response{}, provider.NewError(domain.ErrProviderRejected, "http 500 model not found: %s", bodySnippet)
		}
		return provider.Response{}, provider.NewError(domain.ErrTransport, "http 500 server error: %s", bodySnippet)
	case httpResp.StatusCode >= 500:
		return provider.Response{}, provider.NewError(domain.ErrTransport, "http %d server error: %s", httpResp.StatusCode, bodySnippet)
	case httpResp.StatusCode >= 400:
		return provider.Response{}, provider.NewError(domain.ErrProviderRejected, "http %d client error: %s", httpResp.StatusCode, bodySnippet)
	default:
		return provider.Response{}, provider.NewError(domain.ErrTransport, "http %d unexpected status: %s", httpResp.StatusCode, bodySnippet)
	}
}

func (p *Provider) sanitize(msg string) string {
	if p.cfg.APIKey != "" {
		msg = strings.ReplaceAll(msg, p.cfg.APIKey, "[REDACTED]")
	}
	msg = strings.TrimSpace(msg)
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}
