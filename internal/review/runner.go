package review

import (
	"context"

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
func RunRole(
	ctx context.Context,
	p provider.Provider,
	role domain.Role,
	snap evidence.Snapshot,
	budget Budget,
	policy Policy,
) RoleOutcome {
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

	out.Status = domain.RoleInFlight

	// ---- Call 1: initial --------------------------------------------------
	out.CallCount = 1
	out.LastPurpose = domain.PurposeInitial

	first, callErr := p.Call(ctx, provider.Request{
		Role: role, Purpose: domain.PurposeInitial, Prompt: prompt, Attempt: 1,
	})
	if callErr != nil {
		category := provider.CategoryOf(callErr)
		if !category.IsRetryableTransport() {
			// Fatal provider rejection: no second call is permitted.
			return fail(out, category)
		}
		return transportRetry(ctx, p, out, prompt, role, snap)
	}

	result, verr := DecodeAndValidate(first.Body, snap)
	if verr == nil {
		return succeed(out, result)
	}
	return formatRepair(ctx, p, out, prompt, role, snap, verr)
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
) RoleOutcome {
	out.CallCount = 2
	out.LastPurpose = domain.PurposeTransportRetry

	second, err := p.Call(ctx, provider.Request{
		Role: role, Purpose: domain.PurposeTransportRetry, Prompt: prompt, Attempt: 2,
	})
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
) RoleOutcome {
	// Guard the XOR: a repair is only possible while the budget allows a call.
	if out.CallCount >= domain.MaxProviderCallsPerRole {
		return failValidation(out, firstErr)
	}

	out.CallCount = 2
	out.LastPurpose = domain.PurposeFormatRepair

	second, err := p.Call(ctx, provider.Request{
		Role: role, Purpose: domain.PurposeFormatRepair, Prompt: prompt, Attempt: 2,
	})
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
