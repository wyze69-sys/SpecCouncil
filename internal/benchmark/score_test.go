package benchmark

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
)

// Test 1: Matches - hit via basis_ref, hit via anchor_ref, miss, empty-refs finding.
func TestMatches(t *testing.T) {
	defect := SeededDefect{
		ID:      "D1",
		Kind:    DefectWeakenedAuth,
		UnitIDs: []string{"REQ-1", "FLOW-2"},
	}

	tests := []struct {
		name    string
		finding domain.Finding
		want    bool
	}{
		{
			name: "hit via basis_ref",
			finding: domain.Finding{
				ID:        "F1",
				Kind:      domain.FindingExisting,
				BasisRefs: []string{"REQ-1"},
			},
			want: true,
		},
		{
			name: "hit via anchor_ref",
			finding: domain.Finding{
				ID:        "F2",
				Kind:      domain.FindingMissing,
				AnchorRef: "FLOW-2",
			},
			want: true,
		},
		{
			name: "miss",
			finding: domain.Finding{
				ID:        "F3",
				Kind:      domain.FindingExisting,
				BasisRefs: []string{"COMP-1"},
			},
			want: false,
		},
		{
			name: "empty-refs finding",
			finding: domain.Finding{
				ID:   "F4",
				Kind: domain.FindingExisting,
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Matches(tc.finding, defect)
			if got != tc.want {
				t.Errorf("Matches(%+v, %+v) = %v; want %v", tc.finding, defect, got, tc.want)
			}
		})
	}
}

// Test 2: ScoreRun - assert every field of RunScore exactly.
func TestScoreRun(t *testing.T) {
	cases := DefaultCases()
	caseAuth := cases[0] // case_auth has 3 defects: D1, D2, D3

	// Fixture run result with known finding mix:
	// F1: cites REQ-1 (matches D1)
	// F2: anchors FLOW-1 (matches D3)
	// F3: cites COMP-1 (matches no defect -> false finding)
	// F4: duplicate of F1 (same kind and sorted basis_refs)
	r := RunResult{
		CaseID: caseAuth.ID,
		Arm:    ArmStructured,
		Repeat: 1,
		Findings: []domain.Finding{
			{
				ID:        "F1",
				Kind:      domain.FindingExisting,
				BasisRefs: []string{"REQ-1"},
			},
			{
				ID:        "F2",
				Kind:      domain.FindingMissing,
				AnchorRef: "FLOW-1",
			},
			{
				ID:        "F3",
				Kind:      domain.FindingExisting,
				BasisRefs: []string{"COMP-1"},
			},
			{
				ID:        "F4",
				Kind:      domain.FindingExisting,
				BasisRefs: []string{"REQ-1"},
			},
		},
	}

	score := ScoreRun(r, caseAuth)

	if score.DefectsFound != 2 {
		t.Errorf("DefectsFound = %d; want 2 (D1 and D3)", score.DefectsFound)
	}
	if score.DefectsTotal != 3 {
		t.Errorf("DefectsTotal = %d; want 3", score.DefectsTotal)
	}
	if score.FalseFindings != 1 {
		t.Errorf("FalseFindings = %d; want 1 (F3)", score.FalseFindings)
	}
	if score.SupportedCites != 3 {
		t.Errorf("SupportedCites = %d; want 3 (F1, F2, F4)", score.SupportedCites)
	}
	if score.TotalFindings != 4 {
		t.Errorf("TotalFindings = %d; want 4", score.TotalFindings)
	}
	if score.DuplicateFindings != 1 {
		t.Errorf("DuplicateFindings = %d; want 1 (F4 duplicate of F1)", score.DuplicateFindings)
	}
}

// Test 3: AggregateArm - >=3 repeats with differing results -> assert MeanRecall, RecallVariance, ImportantMisses exactly.
func TestAggregateArm(t *testing.T) {
	cases := DefaultCases()
	caseAuth := cases[0] // defects: D1, D2, D3

	runs := []RunResult{
		// Repeat 1: recall = 2/3 (finds D1, D3), 1 false finding
		{
			CaseID: caseAuth.ID,
			Arm:    ArmStructured,
			Repeat: 1,
			Findings: []domain.Finding{
				{ID: "F1", Kind: domain.FindingExisting, BasisRefs: []string{"REQ-1"}},  // D1
				{ID: "F2", Kind: domain.FindingMissing, AnchorRef: "FLOW-1"},            // D3
				{ID: "F3", Kind: domain.FindingExisting, BasisRefs: []string{"COMP-1"}}, // false
			},
			Latency: 100 * time.Millisecond,
			Cost:    0.0,
		},
		// Repeat 2: recall = 1/3 (finds D1), 1 false finding
		{
			CaseID: caseAuth.ID,
			Arm:    ArmStructured,
			Repeat: 2,
			Findings: []domain.Finding{
				{ID: "F1", Kind: domain.FindingExisting, BasisRefs: []string{"REQ-1"}},  // D1
				{ID: "F2", Kind: domain.FindingExisting, BasisRefs: []string{"COMP-1"}}, // false
			},
			Latency: 200 * time.Millisecond,
			Cost:    0.0,
		},
		// Repeat 3: recall = 2/3 (finds D1, D3), 0 false findings
		{
			CaseID: caseAuth.ID,
			Arm:    ArmStructured,
			Repeat: 3,
			Findings: []domain.Finding{
				{ID: "F1", Kind: domain.FindingExisting, BasisRefs: []string{"REQ-1"}}, // D1
				{ID: "F2", Kind: domain.FindingMissing, AnchorRef: "FLOW-1"},           // D3
			},
			Latency: 150 * time.Millisecond,
			Cost:    0.0,
		},
	}

	score := AggregateArm(ArmStructured, caseAuth, runs)

	if score.Repeats != 3 {
		t.Errorf("Repeats = %d; want 3", score.Repeats)
	}

	// Mean recall = (2/3 + 1/3 + 2/3) / 3 = 5/9
	wantMeanRecall := 5.0 / 9.0
	if math.Abs(score.MeanRecall-wantMeanRecall) > 1e-9 {
		t.Errorf("MeanRecall = %f; want %f", score.MeanRecall, wantMeanRecall)
	}

	// Recall variance: ((2/3 - 5/9)^2 + (1/3 - 5/9)^2 + (2/3 - 5/9)^2) / 3 = (1/81 + 4/81 + 1/81) / 3 = 2/81
	wantVariance := 2.0 / 81.0
	if math.Abs(score.RecallVariance-wantVariance) > 1e-9 {
		t.Errorf("RecallVariance = %f; want %f", score.RecallVariance, wantVariance)
	}

	// Mean false: (1 + 1 + 0) / 3 = 2/3
	wantMeanFalse := 2.0 / 3.0
	if math.Abs(score.MeanFalse-wantMeanFalse) > 1e-9 {
		t.Errorf("MeanFalse = %f; want %f", score.MeanFalse, wantMeanFalse)
	}

	// Important misses: D2 was missed by all repeats
	wantMisses := []string{"D2"}
	if !reflect.DeepEqual(score.ImportantMisses, wantMisses) {
		t.Errorf("ImportantMisses = %v; want %v", score.ImportantMisses, wantMisses)
	}

	// Mean latency: (100 + 200 + 150) / 3 = 150 ms
	wantLatencyMS := 150.0
	if math.Abs(score.MeanLatencyMS-wantLatencyMS) > 1e-3 {
		t.Errorf("MeanLatencyMS = %f; want %f", score.MeanLatencyMS, wantLatencyMS)
	}
}
