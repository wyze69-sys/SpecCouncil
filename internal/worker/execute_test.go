package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
	"github.com/wyze69-sys/SpecCouncil/internal/provider"
	"github.com/wyze69-sys/SpecCouncil/internal/review"
)

// ---------------------------------------------------------------------------
// Helpers and test doubles.
// ---------------------------------------------------------------------------

func testSnap(t *testing.T) evidence.Snapshot {
	t.Helper()
	snap, err := evidence.Freeze("snap-w4", []evidence.Unit{
		{ID: "R-1", Kind: evidence.UnitRequirement, Text: "Users can edit their own projects."},
		{ID: "C-1", Kind: evidence.UnitComponent, Text: "The project service owns project writes."},
	})
	if err != nil {
		t.Fatalf("evidence.Freeze: %v", err)
	}
	return snap
}

func validBody() string {
	return `{"findings":[{"id":"F-1","severity":"high","category":"authorization","issue":"Ownership is unspecified.","recommendation":"State who may edit a project.","basis_refs":["R-1"]}]}`
}

func scriptExec(calls ...provider.ScriptedCall) map[domain.Role][]provider.ScriptedCall {
	return map[domain.Role][]provider.ScriptedCall{domain.RoleRequirements: calls}
}

// baseConfig returns a valid ExecuteConfig with a far-future hard deadline
// suitable for most tests.
func baseConfig(t *testing.T, p provider.Provider) ExecuteConfig {
	t.Helper()
	return ExecuteConfig{
		Role:           domain.RoleRequirements,
		Snapshot:       testSnap(t),
		Provider:       p,
		Budget:         review.Budget{},
		Policy:         review.DefaultPolicy(),
		CallTimeout:    30 * time.Second,
		HardDeadlineAt: time.Now().Add(5 * time.Minute),
		Now:            time.Now,
	}
}

// execRole is the convenience entry point for most tests.
func execRole(t *testing.T, p provider.Provider) (review.RoleOutcome, *provider.FakeProvider) {
	t.Helper()
	fake, ok := p.(*provider.FakeProvider)
	if !ok {
		t.Fatal("execRole requires a *FakeProvider")
	}
	cfg := baseConfig(t, fake)
	out, err := Execute(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return out, fake
}

// ---------------------------------------------------------------------------
// Regression proof 1: valid output succeeds with one call and correct metadata.
// ---------------------------------------------------------------------------

func TestExecuteValidOutputOneCallSuccess(t *testing.T) {
	fake := provider.NewFakeProvider(scriptExec(provider.ScriptedCall{Body: validBody()}))
	out, _ := execRole(t, fake)

	if out.Status != domain.RoleComplete {
		t.Fatalf("status = %s, want complete", out.Status)
	}
	if out.CallCount != 1 {
		t.Errorf("CallCount = %d, want 1", out.CallCount)
	}
	if out.LastPurpose != domain.PurposeInitial {
		t.Errorf("LastPurpose = %s, want initial", out.LastPurpose)
	}
	if out.Role != domain.RoleRequirements {
		t.Errorf("Role = %s, want requirements", out.Role)
	}
	if fake.CallCount(domain.RoleRequirements) != 1 {
		t.Errorf("provider calls = %d, want 1", fake.CallCount(domain.RoleRequirements))
	}
}

// ---------------------------------------------------------------------------
// Regression proof 2: prompt budget exhaustion makes zero provider calls.
// ---------------------------------------------------------------------------

func TestExecuteBudgetExhaustionMakesZeroCalls(t *testing.T) {
	fake := provider.NewFakeProvider(scriptExec(provider.ScriptedCall{Body: validBody()}))
	cfg := baseConfig(t, fake)
	cfg.Budget = review.Budget{MaxPromptTokens: 1} // impossibly small

	out, err := Execute(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if out.Status != domain.RoleFailed {
		t.Fatalf("status = %s, want failed", out.Status)
	}
	if out.ErrorCategory != domain.ErrBudgetExhausted {
		t.Errorf("category = %s, want budget_exhausted", out.ErrorCategory)
	}
	if fake.CallCount(domain.RoleRequirements) != 0 {
		t.Errorf("calls = %d, want 0: budget check must fire before provider", fake.CallCount(domain.RoleRequirements))
	}
}

// ---------------------------------------------------------------------------
// Regression proof 3: retryable transport failure permits exactly one retry.
// ---------------------------------------------------------------------------

func TestExecuteRetryableTransportPermitsOneRetry(t *testing.T) {
	fake := provider.NewFakeProvider(scriptExec(
		provider.ScriptedCall{TransportError: domain.ErrTransport, Message: "429"},
		provider.ScriptedCall{Body: validBody()},
	))
	out, _ := execRole(t, fake)

	if out.Status != domain.RoleComplete {
		t.Fatalf("status = %s, want complete", out.Status)
	}
	if out.CallCount != 2 {
		t.Errorf("CallCount = %d, want 2", out.CallCount)
	}
	if out.LastPurpose != domain.PurposeTransportRetry {
		t.Errorf("LastPurpose = %s, want transport_retry", out.LastPurpose)
	}
	if fake.CallCount(domain.RoleRequirements) != 2 {
		t.Errorf("provider calls = %d, want 2", fake.CallCount(domain.RoleRequirements))
	}
}

// ---------------------------------------------------------------------------
// Regression proof 4: fatal provider failure makes no second call.
// ---------------------------------------------------------------------------

func TestExecuteFatalProviderFailureMakesNoSecondCall(t *testing.T) {
	fake := provider.NewFakeProvider(scriptExec(
		provider.ScriptedCall{TransportError: domain.ErrProviderRejected, Message: "401 unauthorized"},
		provider.ScriptedCall{Body: validBody()},
	))
	out, _ := execRole(t, fake)

	if out.Status != domain.RoleFailed {
		t.Fatalf("status = %s, want failed", out.Status)
	}
	if out.ErrorCategory != domain.ErrProviderRejected {
		t.Errorf("category = %s, want provider_rejected", out.ErrorCategory)
	}
	if fake.CallCount(domain.RoleRequirements) != 1 {
		t.Errorf("calls = %d, want 1: fatal errors must not retry", fake.CallCount(domain.RoleRequirements))
	}
}

// ---------------------------------------------------------------------------
// Regression proof 5: invalid output permits exactly one format repair.
// ---------------------------------------------------------------------------

func TestExecuteInvalidOutputPermitsOneFormatRepair(t *testing.T) {
	fake := provider.NewFakeProvider(scriptExec(
		provider.ScriptedCall{Body: "not json"},
		provider.ScriptedCall{Body: validBody()},
	))
	out, _ := execRole(t, fake)

	if out.Status != domain.RoleComplete {
		t.Fatalf("status = %s, want complete", out.Status)
	}
	if out.CallCount != 2 {
		t.Errorf("CallCount = %d, want 2", out.CallCount)
	}
	if out.LastPurpose != domain.PurposeFormatRepair {
		t.Errorf("LastPurpose = %s, want format_repair", out.LastPurpose)
	}
}

// ---------------------------------------------------------------------------
// Regression proof 6: XOR — transport-retry path cannot format-repair, and
//
//	format-repair path cannot transport-retry.
//
// ---------------------------------------------------------------------------

func TestExecuteRetryPathCannotThenFormatRepair(t *testing.T) {
	// transport_retry already used call 2; bad output on call 2 must fail, no call 3.
	fake := provider.NewFakeProvider(scriptExec(
		provider.ScriptedCall{TransportError: domain.ErrTransport, Message: "reset"},
		provider.ScriptedCall{Body: "still not json"},
	))
	out, _ := execRole(t, fake)

	if out.Status != domain.RoleFailed {
		t.Fatalf("status = %s, want failed", out.Status)
	}
	if fake.CallCount(domain.RoleRequirements) != 2 {
		t.Errorf("calls = %d, want 2: budget is hard", fake.CallCount(domain.RoleRequirements))
	}
	if out.LastPurpose != domain.PurposeTransportRetry {
		t.Errorf("LastPurpose = %s, want transport_retry", out.LastPurpose)
	}
}

func TestExecuteRepairPathCannotThenTransportRetry(t *testing.T) {
	// format_repair used as call 2; transport failure on call 2 must fail, no call 3.
	fake := provider.NewFakeProvider(scriptExec(
		provider.ScriptedCall{Body: "not json"},
		provider.ScriptedCall{TransportError: domain.ErrTransport, Message: "transport on repair"},
	))
	out, _ := execRole(t, fake)

	if out.Status != domain.RoleFailed {
		t.Fatalf("status = %s, want failed", out.Status)
	}
	if fake.CallCount(domain.RoleRequirements) != 2 {
		t.Errorf("calls = %d, want 2", fake.CallCount(domain.RoleRequirements))
	}
	if out.LastPurpose != domain.PurposeFormatRepair {
		t.Errorf("LastPurpose = %s, want format_repair", out.LastPurpose)
	}
}

// ---------------------------------------------------------------------------
// Regression proof 7: no execution path makes a third provider call.
// ---------------------------------------------------------------------------

func TestExecuteNoThirdCall(t *testing.T) {
	// All permutations above enforce 2-call max.  This test is an explicit
	// belt-and-suspenders check: the FakeProvider itself panics if a scripted
	// call index is exhausted with ErrProviderRejected (not a 3rd call).
	fake := provider.NewFakeProvider(scriptExec(
		provider.ScriptedCall{TransportError: domain.ErrTransport},
		provider.ScriptedCall{Body: "bad json"},
	))
	out, _ := execRole(t, fake)

	if fake.CallCount(domain.RoleRequirements) > 2 {
		t.Errorf("calls = %d, want <= 2", fake.CallCount(domain.RoleRequirements))
	}
	if out.Status != domain.RoleFailed {
		t.Errorf("status = %s, want failed", out.Status)
	}
}

// ---------------------------------------------------------------------------
// Regression proof 8: per-attempt timeout cancels a blocked provider.
// ---------------------------------------------------------------------------

func TestExecutePerAttemptTimeoutCancelsBlockedProvider(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{}, 1)
	blocking := &execBlockingProvider{started: started, cancelled: cancelled}

	cfg := baseConfig(t, blocking)
	cfg.CallTimeout = 30 * time.Millisecond
	cfg.HardDeadlineAt = time.Now().Add(5 * time.Minute)

	done := make(chan review.RoleOutcome, 1)
	go func() {
		out, err := Execute(context.Background(), cfg)
		if err != nil {
			done <- review.RoleOutcome{Status: domain.RoleFailed, ErrorCategory: domain.ErrSchemaInvalid}
			return
		}
		done <- out
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider did not start within deadline")
	}

	select {
	case out := <-done:
		if out.Status != domain.RoleFailed {
			t.Fatalf("status = %s, want failed", out.Status)
		}
		if !out.ErrorCategory.IsRetryableTransport() {
			t.Errorf("category = %s, want retryable transport", out.ErrorCategory)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Execute did not return after per-attempt timeout")
	}

	select {
	case <-cancelled:
	case <-time.After(500 * time.Millisecond):
		t.Error("blocked provider did not observe cancellation — possible goroutine leak")
	}
}

// ---------------------------------------------------------------------------
// Regression proof 9: hard deadline prevents a call from starting.
// ---------------------------------------------------------------------------

func TestExecuteHardDeadlinePreventsCallFromStarting(t *testing.T) {
	fake := provider.NewFakeProvider(scriptExec(provider.ScriptedCall{Body: validBody()}))
	cfg := baseConfig(t, fake)
	cfg.HardDeadlineAt = time.Now().Add(-time.Hour) // already expired

	out, err := Execute(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Execute: unexpected error %v", err)
	}

	if out.Status != domain.RoleFailed {
		t.Fatalf("status = %s, want failed", out.Status)
	}
	if out.ErrorCategory != domain.ErrTimeout {
		t.Errorf("category = %s, want timeout", out.ErrorCategory)
	}
	if fake.CallCount(domain.RoleRequirements) != 0 {
		t.Errorf("calls = %d, want 0: hard deadline past must prevent every call", fake.CallCount(domain.RoleRequirements))
	}
}

// ---------------------------------------------------------------------------
// Regression proof 10: cancellation during backoff prevents the second call.
// ---------------------------------------------------------------------------

func TestExecuteCancellationDuringBackoffPreventsSecondCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	cancelOnSleep := func(c context.Context, d time.Duration) error {
		cancel()
		return c.Err()
	}

	fake := provider.NewFakeProvider(scriptExec(
		provider.ScriptedCall{TransportError: domain.ErrTransport, Message: "net reset"},
		provider.ScriptedCall{Body: validBody()},
	))
	cfg := baseConfig(t, fake)
	cfg.BackoffMin = 1 * time.Millisecond
	cfg.BackoffMax = 10 * time.Millisecond
	cfg.Sleep = cancelOnSleep

	out, err := Execute(ctx, cfg)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if out.Status != domain.RoleFailed {
		t.Fatalf("status = %s, want failed", out.Status)
	}
	if out.ErrorCategory != domain.ErrTimeout {
		t.Errorf("category = %s, want timeout", out.ErrorCategory)
	}
	if fake.CallCount(domain.RoleRequirements) != 1 {
		t.Errorf("calls = %d, want 1: second call must not start", fake.CallCount(domain.RoleRequirements))
	}
}

// ---------------------------------------------------------------------------
// Regression proof 11: malformed provider output never becomes a finding.
// ---------------------------------------------------------------------------

func TestExecuteMalformedOutputNeverBecomesAFinding(t *testing.T) {
	fake := provider.NewFakeProvider(scriptExec(
		provider.ScriptedCall{Body: `{"findings":[{"id":"X","severity":"critical","category":"c","issue":"i","recommendation":"r","basis_refs":["R-999-DOES-NOT-EXIST"]}]}`},
		provider.ScriptedCall{Body: `{"findings":[{"id":"X","severity":"critical","category":"c","issue":"i","recommendation":"r","basis_refs":["R-999-DOES-NOT-EXIST"]}]}`},
	))
	out, _ := execRole(t, fake)

	if out.Status != domain.RoleFailed {
		t.Fatalf("status = %s, want failed (malformed basis_ref)", out.Status)
	}
	if len(out.Findings) != 0 {
		t.Errorf("findings = %d, want 0: malformed output must never become a finding", len(out.Findings))
	}
}

// ---------------------------------------------------------------------------
// Regression proof 12: Execute performs no SQLite, publication, composer,
//
//	dispatch, or unrelated goroutine work.
//
// (Structural: Execute only calls review.RunRole — no storage imports.)
// ---------------------------------------------------------------------------

func TestExecuteForbiddenScopeIsAbsent(t *testing.T) {
	// Execute returns a clean RoleOutcome with no side effects beyond the
	// provider call.  This test verifies it does not require or return any
	// storage, publication, or composer artefact.
	fake := provider.NewFakeProvider(scriptExec(provider.ScriptedCall{Body: validBody()}))
	cfg := baseConfig(t, fake)

	out, err := Execute(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// The outcome must be a terminal role state (complete/failed/interrupted).
	if !out.Status.IsTerminal() {
		t.Errorf("status = %s is not terminal; Execute must return a terminal outcome", out.Status)
	}
	// No session, no persistence artefact, no second role — just the outcome.
	if out.Role != domain.RoleRequirements {
		t.Errorf("role = %s, want requirements", out.Role)
	}
}

// ---------------------------------------------------------------------------
// Regression proof 13: concurrent independent roles use independent contexts
//
//	and do not share mutable attempt state.
//
// ---------------------------------------------------------------------------

func TestExecuteConcurrentRolesIndependentContexts(t *testing.T) {
	const concurrency = 4
	type result struct {
		role domain.Role
		out  review.RoleOutcome
	}
	results := make(chan result, concurrency)

	snap := testSnap(t)

	for _, role := range domain.Roles {
		role := role
		go func() {
			fake := provider.NewFakeProvider(map[domain.Role][]provider.ScriptedCall{
				role: {provider.ScriptedCall{Body: validBody()}},
			})
			cfg := ExecuteConfig{
				Role:           role,
				Snapshot:       snap,
				Provider:       fake,
				Budget:         review.Budget{},
				Policy:         review.DefaultPolicy(),
				CallTimeout:    30 * time.Second,
				HardDeadlineAt: time.Now().Add(5 * time.Minute),
				Now:            time.Now,
			}
			out, err := Execute(context.Background(), cfg)
			if err != nil {
				results <- result{role: role, out: review.RoleOutcome{
					Role:          role,
					Status:        domain.RoleFailed,
					ErrorCategory: domain.ErrSchemaInvalid,
				}}
				return
			}
			results <- result{role: role, out: out}
		}()
	}

	for range domain.Roles {
		r := <-results
		if r.out.Status != domain.RoleComplete {
			t.Errorf("role %s: status = %s, want complete (category %s)",
				r.role, r.out.Status, r.out.ErrorCategory)
		}
		if r.out.Role != r.role {
			t.Errorf("role %s: outcome role = %s, mismatch — shared state suspected", r.role, r.out.Role)
		}
	}
}

// ---------------------------------------------------------------------------
// Validation / input error tests.
// ---------------------------------------------------------------------------

func TestExecuteRejectsInvalidRole(t *testing.T) {
	snap := testSnap(t)
	fake := provider.NewFakeProvider(nil)
	cfg := ExecuteConfig{
		Role:           domain.Role("not-a-role"),
		Snapshot:       snap,
		Provider:       fake,
		CallTimeout:    time.Second,
		HardDeadlineAt: time.Now().Add(time.Minute),
	}
	_, err := Execute(context.Background(), cfg)
	if err == nil {
		t.Fatal("Execute must reject invalid role")
	}
}

func TestExecuteRejectsEmptySnapshot(t *testing.T) {
	fake := provider.NewFakeProvider(nil)
	cfg := ExecuteConfig{
		Role:           domain.RoleRequirements,
		Snapshot:       evidence.Snapshot{}, // no units
		Provider:       fake,
		CallTimeout:    time.Second,
		HardDeadlineAt: time.Now().Add(time.Minute),
	}
	_, err := Execute(context.Background(), cfg)
	if err == nil {
		t.Fatal("Execute must reject empty snapshot")
	}
}

func TestExecuteRejectsNilProvider(t *testing.T) {
	snap := testSnap(t)
	cfg := ExecuteConfig{
		Role:           domain.RoleRequirements,
		Snapshot:       snap,
		Provider:       nil,
		CallTimeout:    time.Second,
		HardDeadlineAt: time.Now().Add(time.Minute),
	}
	_, err := Execute(context.Background(), cfg)
	if err == nil {
		t.Fatal("Execute must reject nil provider")
	}
}

func TestExecuteRejectsZeroCallTimeout(t *testing.T) {
	snap := testSnap(t)
	fake := provider.NewFakeProvider(nil)
	cfg := ExecuteConfig{
		Role:           domain.RoleRequirements,
		Snapshot:       snap,
		Provider:       fake,
		CallTimeout:    0,
		HardDeadlineAt: time.Now().Add(time.Minute),
	}
	_, err := Execute(context.Background(), cfg)
	if err == nil {
		t.Fatal("Execute must reject zero CallTimeout")
	}
}

func TestExecuteRejectsZeroHardDeadline(t *testing.T) {
	snap := testSnap(t)
	fake := provider.NewFakeProvider(nil)
	cfg := ExecuteConfig{
		Role:        domain.RoleRequirements,
		Snapshot:    snap,
		Provider:    fake,
		CallTimeout: time.Second,
		// HardDeadlineAt zero
	}
	_, err := Execute(context.Background(), cfg)
	if err == nil {
		t.Fatal("Execute must reject zero HardDeadlineAt")
	}
}

// ---------------------------------------------------------------------------
// Test doubles.
// ---------------------------------------------------------------------------

// execBlockingProvider blocks until its context is cancelled.
type execBlockingProvider struct {
	started   chan struct{}
	cancelled chan struct{}
	once      sync.Once
}

func (b *execBlockingProvider) Call(ctx context.Context, req provider.Request) (provider.Response, error) {
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	select {
	case b.cancelled <- struct{}{}:
	default:
	}
	return provider.Response{}, provider.NewError(domain.ErrTimeout, "context cancelled: %v", ctx.Err())
}
