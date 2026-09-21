package domain

// Severity ranks how important a finding is.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"
)

// SeverityRank returns the sort rank of a severity (lowest value sorts first).
func SeverityRank(s Severity) int {
	switch s {
	case SeverityCritical:
		return 0
	case SeverityHigh:
		return 1
	case SeverityMedium:
		return 2
	case SeverityLow:
		return 3
	}
	return 4
}

// IsValidSeverity reports whether s is a canonical severity.
func IsValidSeverity(s Severity) bool {
	return SeverityRank(s) < 4
}

// FindingKind is the kind of concern a finding raises.
type FindingKind string

const (
	FindingExisting    FindingKind = "existing"
	FindingConflicting FindingKind = "conflicting"
	FindingMissing     FindingKind = "missing"
)

// IsValidFindingKind reports whether k is a canonical finding kind.
func IsValidFindingKind(k FindingKind) bool {
	switch k {
	case FindingExisting, FindingConflicting, FindingMissing:
		return true
	}
	return false
}

// BasisRefsBounds returns the inclusive basis_refs count allowed for a finding
// kind, and whether the kind is known.
func BasisRefsBounds(k FindingKind) (min, max int, ok bool) {
	switch k {
	case FindingExisting:
		return 1, 5, true
	case FindingConflicting:
		return 2, 5, true
	case FindingMissing:
		return 0, 5, true
	}
	return 0, 0, false
}

// Finding is one validated finding produced by one reviewer role.
type Finding struct {
	ID             string      `json:"id"`
	Kind           FindingKind `json:"kind"`
	Severity       Severity    `json:"severity"`
	Category       string      `json:"category"`
	Issue          string      `json:"issue"`
	Recommendation string      `json:"recommendation"`
	BasisRefs      []string    `json:"basis_refs"`
	AnchorRef      string      `json:"anchor_ref,omitempty"`
}

// Output limits from the frozen contract.
const (
	// MaxFindingsPerRole is the maximum number of findings one role may return.
	MaxFindingsPerRole = 15
	// MinBasisRefsPerFinding / MaxBasisRefsPerFinding bound each finding's citations.
	MinBasisRefsPerFinding  = 1
	MaxBasisRefsPerFinding  = 5
	MaxAnchorRefsPerFinding = 1
	// MaxIssueChars and MaxRecommendationChars bound the prose fields.
	MaxIssueChars          = 1000
	MaxRecommendationChars = 1000
)

// ReviewerResult is the normalized, validated output of one reviewer role.
//
// An empty Findings slice is a valid, successful result. There is no LLM
// quality gate: valid-but-weak output is accepted.
type ReviewerResult struct {
	Findings []Finding `json:"findings"`
}
