package provider

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
)

// Request is one provider attempt.
type Request struct {
	Role    domain.Role
	Purpose domain.CallPurpose
	Prompt  string
	// Attempt is 1 for the first call and 2 for the second.
	Attempt int
}

// Response is one raw provider attempt that reached the model and returned.
//
// Body is intentionally unstructured: parsing and validation are separate,
// testable steps and raw output never becomes trusted state.
type Response struct {
	Body      []byte
	Model     string
	TokensIn  int
	TokensOut int
}

// Error is a typed provider failure carrying its domain error category.
type Error struct {
	Category domain.ErrorCategory
	Message  string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return string(e.Category)
	}
	return fmt.Sprintf("%s: %s", e.Category, e.Message)
}

// NewError builds a typed provider error.
func NewError(category domain.ErrorCategory, format string, args ...any) *Error {
	return &Error{Category: category, Message: fmt.Sprintf(format, args...)}
}

// CategoryOf extracts the domain category from a provider error.
//
// Any error that is not explicitly typed is treated as a transport fault,
// because an unclassified network failure is the safe default for retry logic.
func CategoryOf(err error) domain.ErrorCategory {
	var perr *Error
	if errors.As(err, &perr) {
		return perr.Category
	}
	return domain.ErrTransport
}

// Provider is the single adapter boundary of v1.
//
// v1 has exactly one provider and one model per review, frozen per review.
// There is no vendor fallback.
type Provider interface {
	Call(ctx context.Context, req Request) (Response, error)
}

// ScriptedCall is one canned provider result used by the fake provider.
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
	calls  []Request
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
func (f *FakeProvider) Call(ctx context.Context, req Request) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, NewError(domain.ErrTimeout, "context ended before call: %v", err)
	}

	f.mu.Lock()
	calls := f.script[req.Role]
	idx := f.next[req.Role]
	f.calls = append(f.calls, req)
	if idx >= len(calls) {
		f.mu.Unlock()
		return Response{}, NewError(domain.ErrProviderRejected,
			"fake provider has no scripted call %d for role %s", idx+1, req.Role)
	}
	call := calls[idx]
	f.next[req.Role] = idx + 1
	f.mu.Unlock()

	if call.TransportError != "" {
		return Response{}, NewError(call.TransportError, "%s", call.Message)
	}

	model := call.Model
	if model == "" {
		model = "fake-model"
	}
	return Response{
		Body:      []byte(call.Body),
		Model:     model,
		TokensIn:  len(req.Prompt) / 4,
		TokensOut: len(call.Body) / 4,
	}, nil
}

// Calls returns a copy of every request the fake provider received, in order.
func (f *FakeProvider) Calls() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Request, len(f.calls))
	copy(out, f.calls)
	return out
}

// CallCount reports how many calls one role made.
func (f *FakeProvider) CallCount(role domain.Role) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.next[role]
}
