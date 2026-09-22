package benchmark

import (
	"context"
	"strings"
	"testing"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/provider"
	"github.com/wyze69-sys/SpecCouncil/internal/provider/fake"
)

// Test 4: Arm execution: each arm, driven by a scripted fake, makes exactly its budgeted
// number of provider calls (1/1/4/4) and returns the expected findings; a malformed
// arm response is rejected by the shared validator.
func TestArmExecutionBudgetsAndFindings(t *testing.T) {
	cases := DefaultCases()
	caseAuth := cases[0]

	ctx := context.Background()

	arms := []struct {
		arm            Arm
		expectedCalls  int
		expectFindings int
	}{
		{ArmFreeform, 1, 3},
		{ArmStructured, 1, 4}, // Repeat 1 returns 4 findings
		{ArmRoles, 4, 4},      // 1 finding per role * 4 roles = 4
		{ArmGeneric, 4, 4},    // 1 finding per call * 4 calls = 4
	}

	for _, tc := range arms {
		t.Run(string(tc.arm), func(t *testing.T) {
			fakeP := ScriptedFakeProviderForRun(caseAuth.ID, tc.arm, 1)

			res, err := ExecuteArm(ctx, fakeP, tc.arm, caseAuth, 1)
			if err != nil {
				t.Fatalf("ExecuteArm(%s) failed: %v", tc.arm, err)
			}

			if res.Err != "" {
				t.Fatalf("res.Err is non-empty: %s", res.Err)
			}

			if len(fakeP.Calls()) != tc.expectedCalls {
				t.Errorf("arm %s made %d calls; want %d", tc.arm, len(fakeP.Calls()), tc.expectedCalls)
			}

			if len(res.Findings) != tc.expectFindings {
				t.Errorf("arm %s returned %d findings; want %d", tc.arm, len(res.Findings), tc.expectFindings)
			}
		})
	}
}

func TestArmRejectsMalformedResponses(t *testing.T) {
	cases := DefaultCases()
	caseAuth := cases[0]
	ctx := context.Background()

	t.Run("structured malformed JSON rejected", func(t *testing.T) {
		script := map[domain.Role][]fake.ScriptedCall{
			domain.Role("structured"): {
				{Body: `{"findings": [not valid json`},
			},
		}
		p := fake.NewFakeProvider(script)
		res, err := ExecuteArm(ctx, p, ArmStructured, caseAuth, 1)
		if err == nil {
			t.Fatal("ExecuteArm succeeded on malformed JSON; want error")
		}
		if res.Err == "" {
			t.Error("res.Err is empty; want recorded error")
		}
	})

	t.Run("roles invalid basis ref rejected", func(t *testing.T) {
		// Architecture role cites a non-existent unit R-999
		script := map[domain.Role][]fake.ScriptedCall{
			domain.RoleRequirements: {
				{Body: `{"findings":[]}`},
			},
			domain.RoleArchitecture: {
				{Body: `{"findings":[{"id":"A1","kind":"existing","severity":"high","category":"arch","issue":"test","recommendation":"test","basis_refs":["R-999"]}]}`},
			},
			domain.RoleQA:       {{Body: `{"findings":[]}`}},
			domain.RoleSecurity: {{Body: `{"findings":[]}`}},
		}
		p := fake.NewFakeProvider(script)
		res, err := ExecuteArm(ctx, p, ArmRoles, caseAuth, 1)
		if err == nil {
			t.Fatal("ExecuteArm succeeded on invalid basis ref; want error")
		}
		if !strings.Contains(err.Error(), "invalid_basis_ref") && !strings.Contains(res.Err, "invalid_basis_ref") {
			t.Errorf("error %q should mention invalid_basis_ref", err.Error())
		}
	})

	t.Run("generic unknown severity rejected", func(t *testing.T) {
		script := map[domain.Role][]fake.ScriptedCall{
			domain.Role("generic"): {
				{Body: `{"findings":[{"id":"G1","kind":"existing","severity":"catastrophic","category":"g","issue":"test","recommendation":"test","basis_refs":["REQ-1"]}]}`},
			},
		}
		p := fake.NewFakeProvider(script)
		res, err := ExecuteArm(ctx, p, ArmGeneric, caseAuth, 1)
		if err == nil {
			t.Fatal("ExecuteArm succeeded on unknown severity; want error")
		}
		if !strings.Contains(err.Error(), "schema_invalid") && !strings.Contains(res.Err, "schema_invalid") {
			t.Errorf("error %q should mention schema_invalid", err.Error())
		}
	})
}

func TestArmRetryOnTransportErrorSucceeds(t *testing.T) {
	cases := DefaultCases()
	caseAuth := cases[0]
	ctx := context.Background()

	validBody := `{"findings":[{"id":"S1","kind":"existing","severity":"high","category":"auth","issue":"insecure token","recommendation":"use hmac","basis_refs":["REQ-1"]}]}`

	script := map[domain.Role][]fake.ScriptedCall{
		domain.Role("structured"): {
			{TransportError: domain.ErrTransport, Message: "flaky network 500"},
			{Body: validBody},
		},
	}
	p := fake.NewFakeProvider(script)

	res, err := ExecuteArm(ctx, p, ArmStructured, caseAuth, 1)
	if err != nil {
		t.Fatalf("expected ExecuteArm to succeed after retry; failed with: %v", err)
	}
	if res.Err != "" {
		t.Fatalf("expected res.Err to be empty; got: %s", res.Err)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("expected 1 finding; got %d", len(res.Findings))
	}
	if len(p.Calls()) != 2 {
		t.Fatalf("expected 2 provider calls (1 failed + 1 retry); got %d", len(p.Calls()))
	}
}

func TestArmTransportErrorExhaustsRetriesContinues(t *testing.T) {
	cases := DefaultCases()
	caseAuth := cases[0]
	ctx := context.Background()

	script := map[domain.Role][]fake.ScriptedCall{
		domain.Role("structured"): {
			{TransportError: domain.ErrTransport, Message: "timeout 1"},
			{TransportError: domain.ErrTransport, Message: "timeout 2"},
			{TransportError: domain.ErrTransport, Message: "timeout 3"},
		},
	}
	p := fake.NewFakeProvider(script)

	// ExecuteArm directly: fails after 3 attempts, returns res with res.Err set, 0 findings, and nil error
	res, err := ExecuteArm(ctx, p, ArmStructured, caseAuth, 1)
	if err != nil {
		t.Fatalf("expected ExecuteArm to return nil err on exhausted retries so harness continues; got: %v", err)
	}
	if res.Err == "" {
		t.Fatal("expected res.Err to be non-empty")
	}
	if !strings.Contains(res.Err, "timeout") {
		t.Errorf("res.Err %q should mention timeout", res.Err)
	}
	if len(res.Findings) != 0 {
		t.Errorf("expected 0 findings on failed run; got %d", len(res.Findings))
	}
	if len(p.Calls()) != 3 {
		t.Fatalf("expected 3 provider attempts; got %d", len(p.Calls()))
	}

	// Runner.Run: a flaky arm does NOT abort the benchmark
	runner := NewRunner(RunnerOptions{
		Repeats: 1,
		Arms:    []Arm{ArmStructured},
		ProviderFactory: func(caseID string, arm Arm, repeat int) (provider.Provider, error) {
			return fake.NewFakeProvider(script), nil
		},
	})
	scores, err := runner.Run(ctx, []Case{caseAuth})
	if err != nil {
		t.Fatalf("expected runner.Run to continue and complete; got error: %v", err)
	}
	if len(scores) != 1 {
		t.Fatalf("expected 1 score; got %d", len(scores))
	}
	if scores[0].MeanRecall != 0.0 {
		t.Errorf("expected 0.0 mean recall for failed arm; got %f", scores[0].MeanRecall)
	}
}

func TestArmProviderRejectedNotRetriedRunStops(t *testing.T) {
	cases := DefaultCases()
	caseAuth := cases[0]
	ctx := context.Background()

	script := map[domain.Role][]fake.ScriptedCall{
		domain.Role("structured"): {
			{TransportError: domain.ErrProviderRejected, Message: "unauthorized 401"},
			{Body: `{"findings":[]}`}, // second call should never be reached
		},
	}
	p := fake.NewFakeProvider(script)

	res, err := ExecuteArm(ctx, p, ArmStructured, caseAuth, 1)
	if err == nil {
		t.Fatal("expected ExecuteArm to fail immediately on provider_rejected; got nil error")
	}
	if res.Err == "" {
		t.Fatal("expected res.Err to be non-empty")
	}
	if len(p.Calls()) != 1 {
		t.Fatalf("expected exactly 1 provider call (no retries); got %d", len(p.Calls()))
	}

	// Runner.Run must abort on provider_rejected
	runner := NewRunner(RunnerOptions{
		Repeats: 1,
		Arms:    []Arm{ArmStructured},
		ProviderFactory: func(caseID string, arm Arm, repeat int) (provider.Provider, error) {
			return fake.NewFakeProvider(script), nil
		},
	})
	_, rErr := runner.Run(ctx, []Case{caseAuth})
	if rErr == nil {
		t.Fatal("expected runner.Run to abort on provider_rejected; got nil error")
	}
}

func TestTokenBasedEstimateUnderDefaultCap(t *testing.T) {
	cases := DefaultCases()
	// 32000 = the benchmark's DefaultMaxTokens (a reasoning model needs headroom
	// to reason AND emit findings; small caps yield empty-content HTTP 500s). The
	// estimate is NOT ceiling-based: it uses the realistic per-call output
	// (EstimatedRealisticOutputTokensPerCall=8000), so a generous ceiling does not
	// inflate the estimate.
	estCost, totalCalls := EstimateLiveCost(cases, 3, 32000)

	// 2 cases * 3 repeats * 10 calls/repeat = 60 calls
	if totalCalls != 60 {
		t.Errorf("expected 60 calls; got %d", totalCalls)
	}
	if estCost <= 0.0 {
		t.Errorf("expected estimate > 0.0; got $%.4f", estCost)
	}
	// Padded estimate (8000 output tokens/call) must stay under the $1.50 default
	// cap so the live run is not falsely refused; real spend is far lower.
	if estCost >= 1.50 {
		t.Errorf("expected estimate < $1.50 default cap; got $%.4f", estCost)
	}
	t.Logf("60-call token-based estimate: $%.4f for %d calls", estCost, totalCalls)
}

type mockRealProvider struct {
	tokensIn  int
	tokensOut int
}

func (m *mockRealProvider) Call(ctx context.Context, req provider.Request) (provider.Response, error) {
	return provider.Response{
		Body:      []byte(`{"findings":[]}`),
		Model:     "deepseek/deepseek-v4.1-flash",
		TokensIn:  m.tokensIn,
		TokensOut: m.tokensOut,
	}, nil
}

func TestCountingProviderTelemetryCost(t *testing.T) {
	ctx := context.Background()

	// Fake provider: cost is always $0.0
	fakeP := fake.NewFakeProvider(map[domain.Role][]fake.ScriptedCall{
		domain.Role("test"): {{Body: `{"findings":[]}`}},
	})
	cpFake := newCountingProvider(fakeP, 1)
	_, err := cpFake.Call(ctx, provider.Request{Role: "test"})
	if err != nil {
		t.Fatalf("fake call failed: %v", err)
	}
	if cpFake.Cost() != 0.0 {
		t.Errorf("expected fake provider cost $0.0; got $%.6f", cpFake.Cost())
	}

	// Real provider mock: cost estimated from tokens
	mockP := &mockRealProvider{tokensIn: 1000, tokensOut: 500}
	cpReal := newCountingProvider(mockP, 1)
	_, err = cpReal.Call(ctx, provider.Request{Role: "test"})
	if err != nil {
		t.Fatalf("real call failed: %v", err)
	}
	expectedCost := float64(1000)*ClinePricePerInputTokenUSD + float64(500)*ClinePricePerOutputTokenUSD
	if cpReal.Cost() != expectedCost {
		t.Errorf("expected real provider cost $%.6f; got $%.6f", expectedCost, cpReal.Cost())
	}
}
