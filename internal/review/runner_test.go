package review

import (
	"context"
	"testing"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/provider"
)

func script(calls ...provider.ScriptedCall) map[domain.Role][]provider.ScriptedCall {
	return map[domain.Role][]provider.ScriptedCall{domain.RoleRequirements: calls}
}

func runOnce(t *testing.T, script map[domain.Role][]provider.ScriptedCall, budget Budget) (RoleOutcome, *provider.FakeProvider) {
	t.Helper()
	fake := provider.NewFakeProvider(script)
	out := RunRole(context.Background(), fake, domain.RoleRequirements, testSnapshot(t), budget, DefaultPolicy())
	return out, fake
}

func TestHappyPathUsesOneCall(t *testing.T) {
	out, fake := runOnce(t, script(provider.ScriptedCall{Body: validBody()}), Budget{})

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
	out, _ := runOnce(t, script(provider.ScriptedCall{Body: `{"findings":[]}`}), Budget{})

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
		provider.ScriptedCall{TransportError: domain.ErrTransport, Message: "429 too many requests"},
		provider.ScriptedCall{Body: validBody()},
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
		provider.ScriptedCall{Body: "not json at all"},
		provider.ScriptedCall{Body: validBody()},
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

// AC-10: a retry was already used, so malformed output on call 2 fails with no
// third call.
func TestRetryThenMalformedFailsWithoutAThirdCall(t *testing.T) {
	out, fake := runOnce(t, script(
		provider.ScriptedCall{TransportError: domain.ErrTransport, Message: "connection reset"},
		provider.ScriptedCall{Body: "still not json"},
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
		provider.ScriptedCall{Body: "not json"},
		provider.ScriptedCall{TransportError: domain.ErrTimeout, Message: "call timed out"},
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
		provider.ScriptedCall{TransportError: domain.ErrProviderRejected, Message: "401 unauthorized"},
		provider.ScriptedCall{Body: validBody()},
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
	out, fake := runOnce(t, script(provider.ScriptedCall{Body: validBody()}),
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
		provider.ScriptedCall{Body: mkResult(mkFinding("F-1", "high", "R-404"))},
		provider.ScriptedCall{Body: mkResult(mkFinding("F-1", "high", "R-404"))},
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
