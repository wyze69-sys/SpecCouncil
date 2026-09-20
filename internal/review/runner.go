package review

import (
	"context"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
	"github.com/wyze69-sys/SpecCouncil/internal/provider"
)

// Budget bounds the assembled prompt size for the configured model.
//
// Character count is not a guarantee of token fit; the estimate below is a
// deliberate placeholder until provider/timing measurement freezes the real
// rule.
type Budget struct {
	// MaxPromptTokens of 0 disables the check.
	MaxPromptTokens int
}

// EstimatePromptTokens is a coarse, documented estimate (about four
// characters per token). It exists so the budget gate is testable now; it is
// not a tokenizer and must be replaced before freeze.
func EstimatePromptTokens(prompt string) int {
	return (len(prompt) + 3) / 4
}

// Policy holds the engine choices the specification has not frozen yet.
//
// Every unresolved decision lives here, in one greppable place, so freezing
// the specification is a one-line change instead of a rewrite.
type Policy struct {
	// RepairTransportFailureCategory decides the final category when call 1
	// triggers a format repair and call 2 then fails at the transport layer.
	//
	// UNRESOLVED BEFORE FREEZE: the frozen table does not state whether this
	// must be the transport failure or the original validation category.
	RepairTransportFailureCategory domain.ErrorCategory
}

// DefaultPolicy returns the engine policy used until the specification freezes
// these choices.
func DefaultPolicy() Policy {
	return Policy{
		// Report the transport failure as-is: the role did fail for a
		// transport reason, and that is the most truthful observation.
		RepairTransportFailureCategory: domain.ErrTransport,
	}
}

// CallTiming controls per-attempt deadline enforcement and bounded backoff
// between the two provider calls of a role.
//
// A zero-value CallTiming disables deadline enforcement and backoff, which
// preserves the original RunRole behaviour for callers that do not need timing.
type CallTiming struct {
	// CallTimeout is the maximum duration for one provider attempt.
	// Zero means no per-attempt deadline is applied.
	CallTimeout time.Duration

	// HardDeadlineAt, when non-zero, is the absolute instant after which no
	// new provider call may start.  The in-flight attempt context also expires
	// at this instant (whichever is earlier: now+CallTimeout or HardDeadlineAt).
	HardDeadlineAt time.Time

	// BackoffMin is the lower bound of the sleep window between retry attempts.
	// Zero means no backoff is attempted.
	BackoffMin time.Duration

	// BackoffMax is the upper bound of the sleep window between retry attempts.
	BackoffMax time.Duration

	// Now returns the current wall-clock time.  Nil means time.Now.
	Now func() time.Time

	// Sleep pauses for d while respecting ctx cancellation.  Nil falls back to
	// a timer-based sleep.  Must return ctx.Err() when cancelled.
	Sleep func(ctx context.Context, d time.Duration) error

	// Jitter selects a duration in [min, max].  Nil uses the midpoint, which
	// is fully deterministic for tests.
	Jitter func(min, max time.Duration) time.Duration
}

// RoleOutcome is the result of executing one role to a terminal state.
type RoleOutcome struct {
	Role     domain.Role
	Status   domain.RoleStatus
	Findings []domain.Finding

	// ErrorCategory is set only when Status is failed.
	ErrorCategory domain.ErrorCategory
	// InterruptCause is set only when Status is interrupted.
	InterruptCause domain.InterruptCause
	// CallCount is how many provider calls the role actually made (0..2).
	CallCount int
	// LastPurpose is the purpose of the final provider call.
	LastPurpose domain.CallPurpose
	// PromptTokens is the estimated size of the assembled prompt.
	PromptTokens int
}

// RunRole executes one reviewer role to a terminal state.
//
// The provider-call budget is hard: at most two calls, and call 2 is either a
// transport retry or a format repair, never both.
//
// An optional CallTiming value enables per-attempt deadline contexts, hard
// deadline enforcement, and bounded backoff.  Omitting it preserves the
// original behaviour (no deadline, no backoff).
func RunRole(
	ctx context.Context,
	p provider.Provider,
	role domain.Role,
	snap evidence.Snapshot,
	budget Budget,
	policy Policy,
	timing ...CallTiming,
) RoleOutcome {
	var t CallTiming
	if len(timing) > 0 {
		t = timing[0]
	}
	if t.Now == nil {
		t.Now = time.Now
	}

	out := RoleOutcome{Role: role, Status: domain.RolePending}

	prompt, err := BuildPrompt(role, snap)
	if err != nil {
		out.Status = domain.RoleFailed
		out.ErrorCategory = domain.ErrSchemaInvalid
		return out
	}
	out.PromptTokens = EstimatePromptTokens(prompt)

	// The prompt must fit before any call is made. If it does not, no call
	// happens at all and the role fails with budget_exhausted.
	if budget.MaxPromptTokens > 0 && out.PromptTokens > budget.MaxPromptTokens {
		out.Status = domain.RoleFailed
		out.ErrorCategory = domain.ErrBudgetExhausted
		return out
	}

	// Hard deadline: do not start any provider call if the deadline has passed.
	if !t.HardDeadlineAt.IsZero() && !t.Now().Before(t.HardDeadlineAt) {
		out.Status = domain.RoleFailed
		out.ErrorCategory = domain.ErrTimeout
		return out
	}

	out.Status = domain.RoleInFlight

	// ---- Call 1: initial --------------------------------------------------
	out.CallCount = 1
	out.LastPurpose = domain.PurposeInitial

	call1Ctx, call1Cancel := t.attemptContext(ctx)
	first, callErr := p.Call(call1Ctx, provider.Request{
		Role: role, Purpose: domain.PurposeInitial, Prompt: prompt, Attempt: 1,
	})
	call1Cancel()

	if callErr != nil {
		category := provider.CategoryOf(callErr)
		if !category.IsRetryableTransport() {
			// Fatal provider rejection: no second call is permitted.
			return fail(out, category)
		}
		// Retryable transport failure: apply bounded backoff, then retry.
		if sleepErr := t.backoff(ctx); sleepErr != nil {
			// Backoff cancelled (context done or hard deadline exceeded).
			return fail(out, domain.ErrTimeout)
		}
		return transportRetry(ctx, p, out, prompt, role, snap, t)
	}

	result, verr := DecodeAndValidate(first.Body, snap)
	if verr == nil {
		return succeed(out, result)
	}
	return formatRepair(ctx, p, out, prompt, role, snap, verr, t)
}

// transportRetry makes the second call after a retryable transport failure.
//
// If this second call returns a transport-valid but invalid body, there is no
// third call: the role fails with that validation category.
func transportRetry(
	ctx context.Context,
	p provider.Provider,
	out RoleOutcome,
	prompt string,
	role domain.Role,
	snap evidence.Snapshot,
	t CallTiming,
) RoleOutcome {
	out.CallCount = 2
	out.LastPurpose = domain.PurposeTransportRetry

	// Hard deadline: refuse to start the second call.
	if !t.HardDeadlineAt.IsZero() && !t.Now().Before(t.HardDeadlineAt) {
		return fail(out, domain.ErrTimeout)
	}

	call2Ctx, call2Cancel := t.attemptContext(ctx)
	second, err := p.Call(call2Ctx, provider.Request{
		Role: role, Purpose: domain.PurposeTransportRetry, Prompt: prompt, Attempt: 2,
	})
	call2Cancel()
	if err != nil {
		return fail(out, provider.CategoryOf(err))
	}

	result, verr := DecodeAndValidate(second.Body, snap)
	if verr != nil {
		return failValidation(out, verr)
	}
	return succeed(out, result)
}

// formatRepair makes the second call after a transport-valid but invalid body.
func formatRepair(
	ctx context.Context,
	p provider.Provider,
	out RoleOutcome,
	prompt string,
	role domain.Role,
	snap evidence.Snapshot,
	firstErr error,
	t CallTiming,
) RoleOutcome {
	// Guard the XOR: a repair is only possible while the budget allows a call.
	if out.CallCount >= domain.MaxProviderCallsPerRole {
		return failValidation(out, firstErr)
	}

	out.CallCount = 2
	out.LastPurpose = domain.PurposeFormatRepair

	// Hard deadline: refuse to start the second call.
	if !t.HardDeadlineAt.IsZero() && !t.Now().Before(t.HardDeadlineAt) {
		return fail(out, domain.ErrTimeout)
	}

	call2Ctx, call2Cancel := t.attemptContext(ctx)
	second, err := p.Call(call2Ctx, provider.Request{
		Role: role, Purpose: domain.PurposeFormatRepair, Prompt: prompt, Attempt: 2,
	})
	call2Cancel()
	if err != nil {
		// UNRESOLVED BEFORE FREEZE: see Policy.RepairTransportFailureCategory.
		return fail(out, provider.CategoryOf(err))
	}

	result, verr := DecodeAndValidate(second.Body, snap)
	if verr != nil {
		return failValidation(out, verr)
	}
	return succeed(out, result)
}

// attemptContext builds a child context whose deadline is the earlier of
// now+CallTimeout and HardDeadlineAt.  When neither is set the parent context
// is returned with a no-op cancel.
func (t CallTiming) attemptContext(parent context.Context) (context.Context, context.CancelFunc) {
	now := t.Now()

	var deadline time.Time
	if t.CallTimeout > 0 {
		deadline = now.Add(t.CallTimeout)
	}
	if !t.HardDeadlineAt.IsZero() {
		if deadline.IsZero() || t.HardDeadlineAt.Before(deadline) {
			deadline = t.HardDeadlineAt
		}
	}
	if deadline.IsZero() {
		return context.WithCancel(parent)
	}
	return context.WithDeadline(parent, deadline)
}

// backoff sleeps for a jittered duration in [BackoffMin, BackoffMax].
//
// Returns ctx.Err() if the context is cancelled before the sleep completes,
// or context.DeadlineExceeded if the hard deadline is crossed after waking.
// If BackoffMin is zero, backoff is skipped and ctx.Err() is returned
// immediately (nil when the context is still live).
func (t CallTiming) backoff(ctx context.Context) error {
	if t.BackoffMin <= 0 {
		return ctx.Err()
	}
	dur := t.jitter()

	if t.Sleep != nil {
		return t.Sleep(ctx, dur)
	}

	timer := time.NewTimer(dur)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		// After waking, check the hard deadline before proceeding.
		if !t.HardDeadlineAt.IsZero() && !t.Now().Before(t.HardDeadlineAt) {
			return context.DeadlineExceeded
		}
		return nil
	}
}

// jitter returns a duration in [BackoffMin, BackoffMax].
// Falls back to the midpoint (deterministic, test-safe) when Jitter is nil.
func (t CallTiming) jitter() time.Duration {
	if t.Jitter != nil {
		return t.Jitter(t.BackoffMin, t.BackoffMax)
	}
	if t.BackoffMax <= t.BackoffMin {
		return t.BackoffMin
	}
	return t.BackoffMin + (t.BackoffMax-t.BackoffMin)/2
}

func succeed(out RoleOutcome, result domain.ReviewerResult) RoleOutcome {
	out.Status = domain.RoleComplete
	out.Findings = result.Findings
	out.ErrorCategory = ""
	return out
}

func fail(out RoleOutcome, category domain.ErrorCategory) RoleOutcome {
	out.Status = domain.RoleFailed
	out.Findings = nil
	out.ErrorCategory = category
	return out
}

func failValidation(out RoleOutcome, err error) RoleOutcome {
	out.Status = domain.RoleFailed
	out.Findings = nil
	if ve, ok := err.(*ValidationError); ok {
		out.ErrorCategory = ve.Category
		return out
	}
	out.ErrorCategory = domain.ErrSchemaInvalid
	return out
}
