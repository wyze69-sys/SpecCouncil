package benchmark

import (
	"time"

	"github.com/wyze69-sys/SpecCouncil/internal/domain"
	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
)

// Arm is one of the four benchmark comparison arms.
type Arm string

const (
	ArmFreeform   Arm = "freeform"
	ArmStructured Arm = "structured"
	ArmRoles      Arm = "roles"
	ArmGeneric    Arm = "generic"
)

// SeededDefect is one known-planted flaw with the evidence unit it lives in.
type DefectKind string

const (
	DefectContradiction      DefectKind = "contradiction"
	DefectDroppedRequirement DefectKind = "dropped_requirement"
	DefectWeakenedAuth       DefectKind = "weakened_auth"
	DefectMissingFailurePath DefectKind = "missing_failure_path"
)

type SeededDefect struct {
	ID        string     `json:"id"`
	Kind      DefectKind `json:"kind"`
	UnitIDs   []string   `json:"unit_ids"`
	Rationale string     `json:"rationale"`
}

// Case is one approved design plus its planted defects and frozen snapshot.
type Case struct {
	ID       string            `json:"id"`
	Snapshot evidence.Snapshot `json:"snapshot"`
	Defects  []SeededDefect    `json:"defects"`
}

// RunResult is one arm executed once over one case.
type RunResult struct {
	CaseID   string           `json:"case_id"`
	Arm      Arm              `json:"arm"`
	Repeat   int              `json:"repeat"`
	Findings []domain.Finding `json:"findings"`
	// Telemetry — non-baseline, recorded separately.
	Cost    float64       `json:"cost"`
	Latency time.Duration `json:"latency"`
	Err     string        `json:"err,omitempty"`
}
