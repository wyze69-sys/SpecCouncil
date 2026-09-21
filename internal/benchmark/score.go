package benchmark

import (
	"sort"
	"strings"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
)

// Matches reports whether a finding cites/anchors the defect's unit(s).
func Matches(f domain.Finding, d SeededDefect) bool {
	if len(d.UnitIDs) == 0 {
		return false
	}
	unitMap := make(map[string]struct{}, len(d.UnitIDs))
	for _, u := range d.UnitIDs {
		if u != "" {
			unitMap[u] = struct{}{}
		}
	}
	if f.AnchorRef != "" {
		if _, ok := unitMap[f.AnchorRef]; ok {
			return true
		}
	}
	for _, ref := range f.BasisRefs {
		if _, ok := unitMap[ref]; ok {
			return true
		}
	}
	return false
}

// RunScore computes per-run metrics for one RunResult against a case's defects.
type RunScore struct {
	DefectsFound      int `json:"defects_found"`
	DefectsTotal      int `json:"defects_total"`
	FalseFindings     int `json:"false_findings"`
	SupportedCites    int `json:"supported_cites"`
	TotalFindings     int `json:"total_findings"`
	DuplicateFindings int `json:"duplicate_findings"`
}

// ScoreRun computes per-run metrics for one RunResult against a case's defects.
func ScoreRun(r RunResult, c Case) RunScore {
	totalFindings := len(r.Findings)
	defectsTotal := len(c.Defects)

	// DefectsFound: distinct seeded defects matched by >= 1 finding
	defectsFound := 0
	for _, d := range c.Defects {
		matched := false
		for _, f := range r.Findings {
			if Matches(f, d) {
				matched = true
				break
			}
		}
		if matched {
			defectsFound++
		}
	}

	// FalseFindings: findings matching no seeded defect
	// SupportedCites: findings whose refs land on any seeded-defect unit
	falseFindings := 0
	supportedCites := 0
	for _, f := range r.Findings {
		matchedAny := false
		for _, d := range c.Defects {
			if Matches(f, d) {
				matchedAny = true
				break
			}
		}
		if matchedAny {
			supportedCites++
		} else {
			falseFindings++
		}
	}

	// DuplicateFindings: findings with identical (kind, sorted basis_refs, anchor)
	duplicateFindings := 0
	seen := make(map[string]struct{}, totalFindings)
	for _, f := range r.Findings {
		key := findingDuplicateKey(f)
		if _, ok := seen[key]; ok {
			duplicateFindings++
		} else {
			seen[key] = struct{}{}
		}
	}

	return RunScore{
		DefectsFound:      defectsFound,
		DefectsTotal:      defectsTotal,
		FalseFindings:     falseFindings,
		SupportedCites:    supportedCites,
		TotalFindings:     totalFindings,
		DuplicateFindings: duplicateFindings,
	}
}

func findingDuplicateKey(f domain.Finding) string {
	sortedRefs := make([]string, len(f.BasisRefs))
	copy(sortedRefs, f.BasisRefs)
	sort.Strings(sortedRefs)
	return string(f.Kind) + "\x00" + strings.Join(sortedRefs, ",") + "\x00" + f.AnchorRef
}

// ArmScore aggregates every repeat of one arm over one case (>=3 repeats).
type ArmScore struct {
	Arm             Arm      `json:"arm"`
	CaseID          string   `json:"case_id"`
	Repeats         int      `json:"repeats"`
	MeanRecall      float64  `json:"mean_recall"`
	RecallVariance  float64  `json:"recall_variance"`
	MeanFalse       float64  `json:"mean_false"`
	ImportantMisses []string `json:"important_misses"`
	MeanCost        float64  `json:"mean_cost"`
	MeanLatencyMS   float64  `json:"mean_latency_ms"`
}

// AggregateArm aggregates every repeat of one arm over one case (>=3 repeats).
func AggregateArm(arm Arm, c Case, runs []RunResult) ArmScore {
	score := ArmScore{
		Arm:             arm,
		CaseID:          c.ID,
		Repeats:         len(runs),
		ImportantMisses: []string{},
	}
	if len(runs) == 0 {
		return score
	}

	recalls := make([]float64, len(runs))
	var sumRecall float64
	var sumFalse float64
	var sumCost float64
	var sumLatencyMS float64

	// Track which defect IDs were found by at least one run
	defectsEverFound := make(map[string]bool, len(c.Defects))
	for _, d := range c.Defects {
		defectsEverFound[d.ID] = false
	}

	for i, r := range runs {
		rs := ScoreRun(r, c)
		var recall float64
		if rs.DefectsTotal > 0 {
			recall = float64(rs.DefectsFound) / float64(rs.DefectsTotal)
		}
		recalls[i] = recall
		sumRecall += recall
		sumFalse += float64(rs.FalseFindings)
		sumCost += r.Cost
		sumLatencyMS += float64(r.Latency.Nanoseconds()) / 1e6

		for _, d := range c.Defects {
			if defectsEverFound[d.ID] {
				continue
			}
			for _, f := range r.Findings {
				if Matches(f, d) {
					defectsEverFound[d.ID] = true
					break
				}
			}
		}
	}

	n := float64(len(runs))
	meanRecall := sumRecall / n
	score.MeanRecall = meanRecall
	score.MeanFalse = sumFalse / n
	score.MeanCost = sumCost / n
	score.MeanLatencyMS = sumLatencyMS / n

	// Population variance of recall across repeats: sum((recall - mean)^2) / N
	var sumSqDiff float64
	for _, rec := range recalls {
		diff := rec - meanRecall
		sumSqDiff += diff * diff
	}
	score.RecallVariance = sumSqDiff / n

	// Important misses: defect IDs no repeat found, deterministically sorted
	for _, d := range c.Defects {
		if !defectsEverFound[d.ID] {
			score.ImportantMisses = append(score.ImportantMisses, d.ID)
		}
	}
	sort.Strings(score.ImportantMisses)

	return score
}
