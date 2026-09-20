// Package fake provides a deterministic provider implementation for tests and
// the local review-core demonstration.
package fake

import (
	"context"
	"sync"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/provider"
)

// ScriptedCall is one canned provider result used by FakeProvider.
type ScriptedCall struct {
	// Body is returned when TransportError is empty.
	Body string
	// TransportError, when set, is returned instead of a body.
	TransportError domain.ErrorCategory
	// Message is optional detail for a transport error.
	Message string
	Model   string
	// DelayMs simulates provider latency.
	DelayMs int
}

// FakeProvider is a deterministic provider driven by a script.
//
// It is the primary test double: every retry, repair and failure path in the
// engine is exercised through it without spending a real provider call.
type FakeProvider struct {
	mu     sync.Mutex
	script map[domain.Role][]ScriptedCall
	calls  []provider.Request
	next   map[domain.Role]int
}

// NewFakeProvider builds a fake provider from a per-role call script.
func NewFakeProvider(script map[domain.Role][]ScriptedCall) *FakeProvider {
	return &FakeProvider{
		script: script,
		next:   make(map[domain.Role]int),
	}
}

// Call returns the next scripted result for the requested role.
func (f *FakeProvider) Call(ctx context.Context, req provider.Request) (provider.Response, error) {
	if err := ctx.Err(); err != nil {
		return provider.Response{}, provider.NewError(domain.ErrTimeout, "context ended before call: %v", err)
	}

	f.mu.Lock()
	calls := f.script[req.Role]
	idx := f.next[req.Role]
	f.calls = append(f.calls, req)
	if idx >= len(calls) {
		f.mu.Unlock()
		return provider.Response{}, provider.NewError(domain.ErrProviderRejected,
			"fake provider has no scripted call %d for role %s", idx+1, req.Role)
	}
	call := calls[idx]
	f.next[req.Role] = idx + 1
	f.mu.Unlock()

	if call.TransportError != "" {
		return provider.Response{}, provider.NewError(call.TransportError, "%s", call.Message)
	}

	model := call.Model
	if model == "" {
		model = "fake-model"
	}
	return provider.Response{
		Body:      []byte(call.Body),
		Model:     model,
		TokensIn:  len(req.Prompt) / 4,
		TokensOut: len(call.Body) / 4,
	}, nil
}

// Calls returns a copy of every request the fake provider received, in order.
func (f *FakeProvider) Calls() []provider.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]provider.Request, len(f.calls))
	copy(out, f.calls)
	return out
}

// CallCount reports how many calls one role made.
func (f *FakeProvider) CallCount(role domain.Role) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.next[role]
}

var _ provider.Provider = (*FakeProvider)(nil)
