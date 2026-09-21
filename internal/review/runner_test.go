package review

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
	"github.com/wyze69-sys/SpecCouncil/internal/provider"
	"github.com/wyze69-sys/SpecCouncil/internal/provider/fake"
)

func script(calls ...fake.ScriptedCall) map[domain.Role][]fake.ScriptedCall {
	return map[domain.Role][]fake.ScriptedCall{domain.RoleRequirements: calls}
}

func runOnce(t *testing.T, script map[domain.Role][]fake.ScriptedCall, budget Budget) (RoleOutcome, *fake.FakeProvider) {
	t.Helper()
	fake := fake.NewFakeProvider(script)
	out := RunRole(context.Background(), fake, domain.RoleRequirements, testSnapshot(t), budget, DefaultPolicy())
	return out, fake
}

func TestHappyPathUsesOneCall(t *testing.T) {
	out, fake := runOnce(t, script(fake.ScriptedCall{Body: validBody()}), Budget{})

	if out.Status != domain.RoleComplete {
		t.Fatalf("status = %s, want complete (category %s)", out.Status, out.ErrorCategory)
	}
	if out.CallCount != 1 || fake.CallCount(domain.RoleRequirements) != 1 {
		t.Errorf("calls = %d/%d, want 1", out.CallCount, fake.CallCount(domain.RoleRequirements))
	}
	if out.LastPurpose != domain.PurposeInitial {
		t.Errorf("purpose = %s, want initial", out.LastPurpose)
	}
	if len(out.Findings) != 1 {
		t.Errorf("findings = %d, want 1", len(out.Findings))
	}
}

func TestZeroFindingsIsACompleteRole(t *testing.T) {
	out, _ := runOnce(t, script(fake.ScriptedCall{Body: `{"findings":[]}`}), Budget{})

	if out.Status != domain.RoleComplete {
		t.Fatalf("status = %s, want complete", out.Status)
	}
	if len(out.Findings) != 0 {
		t.Errorf("findings = %d, want 0", len(out.Findings))
	}
}

// AC-08: 429 then a valid answer completes with two calls, purpose transport_retry.
func TestRateLimitThenValidUsesTransportRetry(t *testing.T) {
	out, fake := runOnce(t, script(
		fake.ScriptedCall{TransportError: domain.ErrTransport, Message: "429 too many requests"},
		fake.ScriptedCall{Body: validBody()},
	), Budget{})

	if out.Status != domain.RoleComplete {
		t.Fatalf("status = %s, want complete (category %s)", out.Status, out.ErrorCategory)
	}
	if out.CallCount != 2 || fake.CallCount(domain.RoleRequirements) != 2 {
		t.Errorf("calls = %d, want 2", out.CallCount)
	}
	if out.LastPurpose != domain.PurposeTransportRetry {
		t.Errorf("purpose = %s, want transport_retry", out.LastPurpose)
	}
}

// AC-09: bad JSON then a valid repair completes with two calls, purpose format_repair.
func TestBadJSONThenValidRepairUsesFormatRepair(t *testing.T) {
	out, fake := runOnce(t, script(
		fake.ScriptedCall{Body: "not json at all"},
		fake.ScriptedCall{Body: validBody()},
	), Budget{})

	if out.Status != domain.RoleComplete {
		t.Fatalf("status = %s, want complete (category %s)", out.Status, out.ErrorCategory)
	}
	if out.CallCount != 2 || fake.CallCount(domain.RoleRequirements) != 2 {
		t.Errorf("calls = %d, want 2", out.CallCount)
	}
	if out.LastPurpose != domain.PurposeFormatRepair {
		t.Errorf("purpose = %s, want format_repair", out.LastPurpose)
	}
}

// V2 schema error (bad kind) triggers format repair; repaired body succeeds.
func TestBadKindThenValidRepairUsesFormatRepair(t *testing.T) {
	badKindBody := mkResult(`{"id":"F-1","kind":"omission","severity":"high","category":"c","issue":"i","recommendation":"r","basis_refs":["R-1"]}`)
	out, fake := runOnce(t, script(
		fake.ScriptedCall{Body: badKindBody},
		fake.ScriptedCall{Body: validBody()},
	), Budget{})

	if out.Status != domain.RoleComplete {
		t.Fatalf("status = %s, want complete (category %s)", out.Status, out.ErrorCategory)
	}
	if out.CallCount != 2 || fake.CallCount(domain.RoleRequirements) != 2 {
		t.Errorf("calls = %d, want 2", out.CallCount)
	}
	if out.LastPurpose != domain.PurposeFormatRepair {
		t.Errorf("purpose = %s, want format_repair", out.LastPurpose)
	}
	if len(out.Findings) != 1 {
		t.Errorf("findings = %d, want 1", len(out.Findings))
	}
}

// AC-10: a retry was already used, so malformed output on call 2 fails with no
// third call.
func TestRetryThenMalformedFailsWithoutAThirdCall(t *testing.T) {
	out, fake := runOnce(t, script(
		fake.ScriptedCall{TransportError: domain.ErrTransport, Message: "connection reset"},
		fake.ScriptedCall{Body: "still not json"},
	), Budget{})

	if out.Status != domain.RoleFailed {
		t.Fatalf("status = %s, want failed", out.Status)
	}
	if out.ErrorCategory != domain.ErrInvalidJSON {
		t.Errorf("category = %s, want %s", out.ErrorCategory, domain.ErrInvalidJSON)
	}
	if out.CallCount != 2 || fake.CallCount(domain.RoleRequirements) != 2 {
		t.Errorf("calls = %d, want exactly 2: the budget is hard", out.CallCount)
	}
	if len(out.Findings) != 0 {
		t.Errorf("failed role produced %d findings, want 0", len(out.Findings))
	}
}

// The XOR rule: a repair that then fails at transport is still the last call.
func TestRepairThenTransportFailureStopsAtTwoCalls(t *testing.T) {
	out, fake := runOnce(t, script(
		fake.ScriptedCall{Body: "not json"},
		fake.ScriptedCall{TransportError: domain.ErrTimeout, Message: "call timed out"},
	), Budget{})

	if out.Status != domain.RoleFailed {
		t.Fatalf("status = %s, want failed", out.Status)
	}
	if fake.CallCount(domain.RoleRequirements) != 2 {
		t.Errorf("calls = %d, want 2", fake.CallCount(domain.RoleRequirements))
	}
	if out.LastPurpose != domain.PurposeFormatRepair {
		t.Errorf("purpose = %s, want format_repair", out.LastPurpose)
	}
	if !out.ErrorCategory.IsRetryableTransport() {
		t.Errorf("category = %s, want a transport category per DefaultPolicy", out.ErrorCategory)
	}
}

func TestFatalProviderRejectionNeverRetries(t *testing.T) {
	out, fake := runOnce(t, script(
		fake.ScriptedCall{TransportError: domain.ErrProviderRejected, Message: "401 unauthorized"},
		fake.ScriptedCall{Body: validBody()},
	), Budget{})

	if out.Status != domain.RoleFailed {
		t.Fatalf("status = %s, want failed", out.Status)
	}
	if out.ErrorCategory != domain.ErrProviderRejected {
		t.Errorf("category = %s, want provider_rejected", out.ErrorCategory)
	}
	if fake.CallCount(domain.RoleRequirements) != 1 {
		t.Errorf("calls = %d, want 1: a fatal rejection is not retryable", fake.CallCount(domain.RoleRequirements))
	}
}

func TestPromptBudgetBlocksEveryCall(t *testing.T) {
	out, fake := runOnce(t, script(fake.ScriptedCall{Body: validBody()}),
		Budget{MaxPromptTokens: 1})

	if out.Status != domain.RoleFailed {
		t.Fatalf("status = %s, want failed", out.Status)
	}
	if out.ErrorCategory != domain.ErrBudgetExhausted {
		t.Errorf("category = %s, want budget_exhausted", out.ErrorCategory)
	}
	if fake.CallCount(domain.RoleRequirements) != 0 {
		t.Errorf("calls = %d, want 0: an oversized prompt must not reach the provider",
			fake.CallCount(domain.RoleRequirements))
	}
}

func TestTwoInvalidResponsesFailWithTheValidationCategory(t *testing.T) {
	out, _ := runOnce(t, script(
		fake.ScriptedCall{Body: mkResult(mkFinding("F-1", "high", "R-404"))},
		fake.ScriptedCall{Body: mkResult(mkFinding("F-1", "high", "R-404"))},
	), Budget{})

	if out.Status != domain.RoleFailed {
		t.Fatalf("status = %s, want failed", out.Status)
	}
	if out.ErrorCategory != domain.ErrInvalidBasisRef {
		t.Errorf("category = %s, want %s", out.ErrorCategory, domain.ErrInvalidBasisRef)
	}
}

func TestAllFourRolesShareOneFrozenSnapshot(t *testing.T) {
	snap := testSnapshot(t)
	prompts := make(map[domain.Role]string, domain.RoleCount)

	for _, role := range domain.Roles {
		prompt, err := BuildPrompt(role, snap)
		if err != nil {
			t.Fatalf("BuildPrompt(%s): %v", role, err)
		}
		for _, u := range snap.Units {
			if !contains(prompt, u.ID) {
				t.Errorf("role %s prompt is missing unit %s: roles must share the whole snapshot", role, u.ID)
			}
		}
		prompts[role] = prompt
	}

	// Every role sees the same evidence block, byte for byte.
	evidenceBlock := func(p string) string {
		start := indexOf(p, "<<<EVIDENCE")
		end := indexOf(p, "EVIDENCE\n\n")
		return p[start:end]
	}
	first := evidenceBlock(prompts[domain.RoleRequirements])
	for role, p := range prompts {
		if evidenceBlock(p) != first {
			t.Errorf("role %s saw different evidence than requirements", role)
		}
	}
}

func contains(haystack, needle string) bool { return indexOf(haystack, needle) >= 0 }

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// Timing / deadline tests added for W4.
// ---------------------------------------------------------------------------

// makeTestSnap is a convenience builder for timing tests.
func makeTestSnap(t *testing.T) evidence.Snapshot {
	t.Helper()
	return testSnapshot(t)
}

// runWithTiming is a helper that calls RunRole with a CallTiming value.
func runWithTiming(
	t *testing.T,
	s map[domain.Role][]fake.ScriptedCall,
	budget Budget,
	timing CallTiming,
) (RoleOutcome, *fake.FakeProvider) {
	t.Helper()
	fake := fake.NewFakeProvider(s)
	out := RunRole(context.Background(), fake, domain.RoleRequirements,
		testSnapshot(t), budget, DefaultPolicy(), timing)
	return out, fake
}

// TestRunRoleHardDeadlinePreventsCall verifies that a hard deadline already in
// the past blocks every provider call and returns failed/timeout.
func TestRunRoleHardDeadlinePreventsCall(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	timing := CallTiming{
		HardDeadlineAt: past,
		Now:            time.Now,
	}
	out, fake := runWithTiming(t,
		script(fake.ScriptedCall{Body: validBody()}),
		Budget{}, timing)

	if out.Status != domain.RoleFailed {
		t.Fatalf("status = %s, want failed", out.Status)
	}
	if out.ErrorCategory != domain.ErrTimeout {
		t.Errorf("category = %s, want timeout", out.ErrorCategory)
	}
	if fake.CallCount(domain.RoleRequirements) != 0 {
		t.Errorf("calls = %d, want 0: hard deadline must prevent all calls",
			fake.CallCount(domain.RoleRequirements))
	}
}

// TestRunRolePerAttemptTimeoutCancelsProvider verifies that a provider that
// blocks is cancelled by the per-attempt deadline context and RunRole returns
// without leaking the blocked goroutine.
func TestRunRolePerAttemptTimeoutCancelsProvider(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{}, 1)
	blocking := &blockingProvider{
		started:   started,
		cancelled: cancelled,
	}

	const callTimeout = 30 * time.Millisecond
	timing := CallTiming{
		CallTimeout: callTimeout,
		Now:         time.Now,
	}

	snap := makeTestSnap(t)
	done := make(chan RoleOutcome, 1)
	go func() {
		out := RunRole(context.Background(), blocking,
			domain.RoleRequirements, snap, Budget{}, DefaultPolicy(), timing)
		done <- out
	}()

	// Wait for provider to start.
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider call did not start within timeout")
	}

	// Per-attempt deadline should cancel the call.
	select {
	case out := <-done:
		if out.Status != domain.RoleFailed {
			t.Fatalf("status = %s, want failed", out.Status)
		}
		if !out.ErrorCategory.IsRetryableTransport() {
			t.Errorf("category = %s, want retryable transport (timeout)", out.ErrorCategory)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunRole did not return after per-attempt deadline")
	}

	// Provider must have seen its context cancelled (no goroutine leak).
	select {
	case <-cancelled:
	case <-time.After(500 * time.Millisecond):
		t.Error("blocking provider did not observe context cancellation")
	}
}

// TestRunRoleBackoffCancelledPreventsSecondCall verifies that cancelling
// during backoff stops the second provider call.
func TestRunRoleBackoffCancelledPreventsSecondCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// Sleep that immediately cancels the context and signals cancellation.
	cancelOnSleep := func(c context.Context, d time.Duration) error {
		cancel()
		return c.Err()
	}

	timing := CallTiming{
		BackoffMin: 1 * time.Millisecond,
		BackoffMax: 10 * time.Millisecond,
		Now:        time.Now,
		Sleep:      cancelOnSleep,
	}

	fake := fake.NewFakeProvider(script(
		fake.ScriptedCall{TransportError: domain.ErrTransport, Message: "network reset"},
		fake.ScriptedCall{Body: validBody()},
	))

	snap := makeTestSnap(t)
	out := RunRole(ctx, fake, domain.RoleRequirements, snap, Budget{}, DefaultPolicy(), timing)

	if out.Status != domain.RoleFailed {
		t.Fatalf("status = %s, want failed", out.Status)
	}
	if out.ErrorCategory != domain.ErrTimeout {
		t.Errorf("category = %s, want timeout (backoff cancelled)", out.ErrorCategory)
	}
	if fake.CallCount(domain.RoleRequirements) != 1 {
		t.Errorf("calls = %d, want 1: second call must not start when backoff is cancelled",
			fake.CallCount(domain.RoleRequirements))
	}
}

// TestRunRoleHardDeadlinePreventsSecondCallAfterRetry verifies that the hard
// deadline crossed during backoff prevents the transport retry second call.
func TestRunRoleHardDeadlinePreventsSecondCallAfterRetry(t *testing.T) {
	var mu sync.Mutex
	baseNow := time.Now()
	futureDeadline := baseNow.Add(50 * time.Millisecond)
	now := baseNow

	// Sleep that advances the fake clock past the deadline.
	advancePastDeadline := func(c context.Context, d time.Duration) error {
		mu.Lock()
		now = futureDeadline.Add(time.Millisecond)
		mu.Unlock()
		return nil // sleep "succeeds" but deadline was crossed
	}

	timing := CallTiming{
		HardDeadlineAt: futureDeadline,
		BackoffMin:     1 * time.Millisecond,
		BackoffMax:     10 * time.Millisecond,
		Now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		},
		Sleep: advancePastDeadline,
	}

	fake := fake.NewFakeProvider(script(
		fake.ScriptedCall{TransportError: domain.ErrTransport, Message: "transient"},
		fake.ScriptedCall{Body: validBody()},
	))

	snap := makeTestSnap(t)
	out := RunRole(context.Background(), fake, domain.RoleRequirements, snap, Budget{}, DefaultPolicy(), timing)

	if out.Status != domain.RoleFailed {
		t.Fatalf("status = %s, want failed", out.Status)
	}
	if fake.CallCount(domain.RoleRequirements) != 1 {
		t.Errorf("calls = %d, want 1: hard deadline past means no second call",
			fake.CallCount(domain.RoleRequirements))
	}
}

// TestRunRoleHardDeadlinePreventsFormatRepairSecondCall verifies the hard
// deadline also prevents a format repair second call from starting.
func TestRunRoleHardDeadlinePreventsFormatRepairSecondCall(t *testing.T) {
	var mu sync.Mutex
	baseNow := time.Now()
	deadline := baseNow.Add(50 * time.Millisecond)
	now := baseNow

	badThenGood := &interceptProvider{
		inner: fake.NewFakeProvider(script(
			fake.ScriptedCall{Body: "not json"},
			fake.ScriptedCall{Body: validBody()},
		)),
		afterCall: func() {
			// Advance clock past deadline after call 1.
			mu.Lock()
			now = deadline.Add(time.Millisecond)
			mu.Unlock()
		},
	}

	timing := CallTiming{
		HardDeadlineAt: deadline,
		Now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		},
	}

	snap := makeTestSnap(t)
	out := RunRole(context.Background(), badThenGood, domain.RoleRequirements, snap, Budget{}, DefaultPolicy(), timing)

	if out.Status != domain.RoleFailed {
		t.Fatalf("status = %s, want failed", out.Status)
	}
	if out.ErrorCategory != domain.ErrTimeout {
		t.Errorf("category = %s, want timeout", out.ErrorCategory)
	}
	if badThenGood.inner.CallCount(domain.RoleRequirements) != 1 {
		t.Errorf("calls = %d, want 1: hard deadline prevents format repair",
			badThenGood.inner.CallCount(domain.RoleRequirements))
	}
}

// ---------------------------------------------------------------------------
// Test doubles for timing tests.
// ---------------------------------------------------------------------------

// blockingProvider blocks until its context is cancelled.
type blockingProvider struct {
	started   chan struct{}
	cancelled chan struct{}
	once      sync.Once
}

func (b *blockingProvider) Call(ctx context.Context, req provider.Request) (provider.Response, error) {
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	select {
	case b.cancelled <- struct{}{}:
	default:
	}
	return provider.Response{}, provider.NewError(domain.ErrTimeout, "context cancelled: %v", ctx.Err())
}

// interceptProvider wraps an inner FakeProvider and calls afterCall after each Call.
type interceptProvider struct {
	inner     *fake.FakeProvider
	afterCall func()
}

func (ip *interceptProvider) Call(ctx context.Context, req provider.Request) (provider.Response, error) {
	resp, err := ip.inner.Call(ctx, req)
	if ip.afterCall != nil {
		ip.afterCall()
	}
	return resp, err
}
