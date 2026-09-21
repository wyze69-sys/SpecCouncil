package benchmark

import (
	"context"
	"strings"
	"testing"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
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
