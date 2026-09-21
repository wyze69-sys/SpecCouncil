package domain

import "testing"

func TestIsValidFindingKind(t *testing.T) {
	cases := []struct {
		kind FindingKind
		want bool
	}{
		{FindingExisting, true},
		{FindingConflicting, true},
		{FindingMissing, true},
		{FindingKind(""), false},
		{FindingKind("Existing"), false},
		{FindingKind(" omission"), false},
	}
	for _, tc := range cases {
		if got := IsValidFindingKind(tc.kind); got != tc.want {
			t.Errorf("IsValidFindingKind(%q) = %v, want %v", tc.kind, got, tc.want)
		}
	}
}

func TestBasisRefsBounds(t *testing.T) {
	cases := []struct {
		kind    FindingKind
		wantMin int
		wantMax int
		wantOK  bool
	}{
		{FindingExisting, 1, 5, true},
		{FindingConflicting, 2, 5, true},
		{FindingMissing, 0, 5, true},
		{FindingKind(""), 0, 0, false},
		{FindingKind("unknown"), 0, 0, false},
		{FindingKind("omission"), 0, 0, false},
	}
	for _, tc := range cases {
		min, max, ok := BasisRefsBounds(tc.kind)
		if min != tc.wantMin || max != tc.wantMax || ok != tc.wantOK {
			t.Errorf("BasisRefsBounds(%q) = (%d, %d, %v), want (%d, %d, %v)",
				tc.kind, min, max, ok, tc.wantMin, tc.wantMax, tc.wantOK)
		}
	}
}

func TestMaxAnchorRefsPerFindingConstant(t *testing.T) {
	if MaxAnchorRefsPerFinding != 1 {
		t.Errorf("MaxAnchorRefsPerFinding = %d, want 1", MaxAnchorRefsPerFinding)
	}
}
