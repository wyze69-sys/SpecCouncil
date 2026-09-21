package benchmark

import (
	"fmt"
	"sort"
	"strings"
)

// armSortOrder defines the fixed canonical presentation order for benchmark arms.
func armSortOrder(a Arm) int {
	switch a {
	case ArmFreeform:
		return 0
	case ArmStructured:
		return 1
	case ArmRoles:
		return 2
	case ArmGeneric:
		return 3
	default:
		return 4
	}
}

// RenderComparison produces a deterministic, blinded markdown comparison report.
// Baseline metrics are presented first; cost and latency appear under a separate
// "Telemetry (non-baseline)" heading. Ordering is sorted strictly by Case ID, then
// by the fixed canonical arm order. No timestamps appear in the baseline section.
func RenderComparison(cases []Case, scores []ArmScore) string {
	ordered := make([]ArmScore, len(scores))
	copy(ordered, scores)

	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].CaseID != ordered[j].CaseID {
			return ordered[i].CaseID < ordered[j].CaseID
		}
		return armSortOrder(ordered[i].Arm) < armSortOrder(ordered[j].Arm)
	})

	var b strings.Builder

	b.WriteString("# Benchmark Comparison\n\n")
	b.WriteString("| Case | Arm | Repeats | Mean Recall | Recall Var | Mean False | Important Misses |\n")
	b.WriteString("|---|---|---|---|---|---|---|\n")

	for _, s := range ordered {
		misses := "none"
		if len(s.ImportantMisses) > 0 {
			misses = strings.Join(s.ImportantMisses, ", ")
		}

		fmt.Fprintf(&b, "| %s | %s | %d | %.2f | %.4f | %.2f | %s |\n",
			s.CaseID,
			s.Arm,
			s.Repeats,
			s.MeanRecall,
			s.RecallVariance,
			s.MeanFalse,
			misses,
		)
	}

	b.WriteString("\n## Telemetry (non-baseline)\n\n")
	b.WriteString("| Case | Arm | Mean Cost ($) | Mean Latency (ms) |\n")
	b.WriteString("|---|---|---|---|\n")

	for _, s := range ordered {
		fmt.Fprintf(&b, "| %s | %s | %.4f | %.1f |\n",
			s.CaseID,
			s.Arm,
			s.MeanCost,
			s.MeanLatencyMS,
		)
	}

	return b.String()
}
