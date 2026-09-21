package ingest

import (
	"errors"
	"fmt"

	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
)

// IngestResult is a frozen evidence snapshot plus the splitter version that
// produced it. The version rides beside the snapshot because evidence.Snapshot
// is intentionally not modified by ingestion.
type IngestResult struct {
	Snapshot        evidence.Snapshot
	SplitterVersion string
}

// ErrNoEvidenceUnits is returned when the source produces zero evidence units
// (empty or structure-only input). Callers can map this to a client error.
var ErrNoEvidenceUnits = errors.New("ingest: source produced no evidence units")

// BuildSnapshot ingests raw design source into a frozen evidence snapshot.
// It runs ParseBlocks -> AssignIDs -> AssignKinds, builds units in block order,
// and calls evidence.Freeze. It is pure and deterministic: identical (id, source)
// yields an identical result, without clock, filesystem, network, or randomness.
//
// Zero parsed blocks return ErrNoEvidenceUnits before Freeze is called. Otherwise
// Freeze errors are wrapped with %w. Every error returns a zero IngestResult.
func BuildSnapshot(id string, source string) (IngestResult, error) {
	blocks := ParseBlocks(source)
	identified := AssignIDs(blocks)
	kinded := AssignKinds(identified)
	if len(blocks) == 0 {
		return IngestResult{}, ErrNoEvidenceUnits
	}

	units := make([]evidence.Unit, 0, len(kinded))
	for _, kb := range kinded {
		units = append(units, evidence.Unit{ID: kb.ID, Kind: kb.Kind, Text: kb.Block.Text})
	}
	snap, err := evidence.Freeze(id, units)
	if err != nil {
		return IngestResult{}, fmt.Errorf("ingest: build snapshot: %w", err)
	}
	return IngestResult{Snapshot: snap, SplitterVersion: Splitter()}, nil
}
