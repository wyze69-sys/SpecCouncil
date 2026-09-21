package benchmark

import (
	"strings"
	"testing"
)

// Test 5: RenderComparison: golden-string test — deterministic output, telemetry under
// its own heading, stable ordering, no clock.
func TestRenderComparisonGolden(t *testing.T) {
	cases := DefaultCases()

	// Provide unsorted scores to verify stable sorting
	scores := []ArmScore{
		{
			Arm:             ArmGeneric,
			CaseID:          "case_auth",
			Repeats:         3,
			MeanRecall:      0.67,
			RecallVariance:  0.0000,
			MeanFalse:       1.00,
			ImportantMisses: []string{"D2"},
			MeanCost:        0.0000,
			MeanLatencyMS:   15.2,
		},
		{
			Arm:             ArmFreeform,
			CaseID:          "case_auth",
			Repeats:         3,
			MeanRecall:      0.67,
			RecallVariance:  0.0000,
			MeanFalse:       1.00,
			ImportantMisses: []string{"D2"},
			MeanCost:        0.0000,
			MeanLatencyMS:   10.5,
		},
		{
			Arm:             ArmRoles,
			CaseID:          "case_auth",
			Repeats:         3,
			MeanRecall:      1.00,
			RecallVariance:  0.0000,
			MeanFalse:       0.00,
			ImportantMisses: []string{},
			MeanCost:        0.0000,
			MeanLatencyMS:   42.0,
		},
		{
			Arm:             ArmStructured,
			CaseID:          "case_auth",
			Repeats:         3,
			MeanRecall:      0.56,
			RecallVariance:  0.0247,
			MeanFalse:       0.67,
			ImportantMisses: []string{"D2"},
			MeanCost:        0.0000,
			MeanLatencyMS:   12.1,
		},
	}

	got := RenderComparison(cases, scores)

	want := `# Benchmark Comparison

| Case | Arm | Repeats | Mean Recall | Recall Var | Mean False | Important Misses |
|---|---|---|---|---|---|---|
| case_auth | freeform | 3 | 0.67 | 0.0000 | 1.00 | D2 |
| case_auth | structured | 3 | 0.56 | 0.0247 | 0.67 | D2 |
| case_auth | roles | 3 | 1.00 | 0.0000 | 0.00 | none |
| case_auth | generic | 3 | 0.67 | 0.0000 | 1.00 | D2 |

## Telemetry (non-baseline)

| Case | Arm | Mean Cost ($) | Mean Latency (ms) |
|---|---|---|---|
| case_auth | freeform | 0.0000 | 10.5 |
| case_auth | structured | 0.0000 | 12.1 |
| case_auth | roles | 0.0000 | 42.0 |
| case_auth | generic | 0.0000 | 15.2 |
`

	if got != want {
		t.Errorf("RenderComparison output mismatch.\nGot:\n%s\nWant:\n%s", got, want)
	}

	// Verify telemetry header exists
	if !strings.Contains(got, "## Telemetry (non-baseline)") {
		t.Error("rendered report missing ## Telemetry (non-baseline) heading")
	}

	// Verify no timestamps in baseline section (split by telemetry)
	parts := strings.Split(got, "## Telemetry (non-baseline)")
	baselineSection := parts[0]
	for _, forbidden := range []string{"2026-", "UTC", "time", "duration", "ms"} {
		if strings.Contains(strings.ToLower(baselineSection), forbidden) {
			t.Errorf("baseline section contains forbidden temporal reference %q", forbidden)
		}
	}
}
