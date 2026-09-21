//go:build clinelive

package cline

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/provider"
)

func TestClineLiveSmoke(t *testing.T) {
	apiKey := strings.TrimSpace(os.Getenv("SPECCOUNCIL_CLINE_KEY"))
	if apiKey == "" {
		t.Skip("SPECCOUNCIL_CLINE_KEY is empty; skipping live smoke test")
	}

	cfg := Config{
		BaseURL: "https://api.cline.bot/api/v1",
		APIKey:  apiKey,
		Model:   "deepseek/deepseek-v4.1-flash",
		Timeout: 60 * time.Second,
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := provider.Request{
		Role:    domain.RoleArchitecture,
		Purpose: domain.PurposeInitial,
		Prompt:  "Reply with the single word: OK",
		Attempt: 1,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, err := p.Call(ctx, req)
	if err != nil {
		if provider.CategoryOf(err) == domain.ErrTransport {
			t.Logf("first attempt had transport error (%v), retrying once...", err)
			time.Sleep(1 * time.Second)
			resp, err = p.Call(ctx, req)
		}
	}
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if len(resp.Body) == 0 {
		t.Fatal("expected non-empty response body")
	}
	t.Logf("live smoke response: %s (tokens in: %d, out: %d)", string(resp.Body), resp.TokensIn, resp.TokensOut)
}
