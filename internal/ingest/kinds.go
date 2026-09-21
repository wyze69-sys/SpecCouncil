package ingest

import "github.com/wyze69-sys/SpecCouncil/internal/evidence"

// KindedBlock is one identified block paired with its mapped evidence kind.
type KindedBlock struct {
	ID    string
	Kind  evidence.UnitKind
	Block Block
}

// MapBlockKind maps a structural BlockKind to its evidence UnitKind.
// It keys only on the block kind and is a pure total function.
// For the five known BlockKind values it returns a valid UnitKind (a kind for
// which evidence.IsValidUnitKind is true). For any other value it returns the
// empty UnitKind "" (which IsValidUnitKind rejects); ParseBlocks never emits
// such a value, so this default is defensive only.
//
// The mapping is fixed and need not be injective: BlockParagraph and
// BlockListItem both map to evidence.UnitRequirement. evidence.UnitComponent and
// evidence.UnitFlow are intentionally never produced, because separating a
// component or a flow from prose needs semantic classification that this
// structural slice does not have.
func MapBlockKind(k BlockKind) evidence.UnitKind {
	switch k {
	case BlockHeading:
		return evidence.UnitBrief
	case BlockParagraph, BlockListItem:
		return evidence.UnitRequirement
	case BlockTableRow:
		return evidence.UnitDataRule
	case BlockCode:
		return evidence.UnitConstraint
	}
	return ""
}

// AssignKinds maps every identified block to its evidence kind, preserving
// input order. It is pure: identical input yields identical output, and it uses
// no clock, filesystem, network, or randomness. The returned slice is always
// non-nil, so empty input yields []KindedBlock{}.
//
// Input is never mutated: each ID and Block is copied through unchanged.
func AssignKinds(blocks []IdentifiedBlock) []KindedBlock {
	out := make([]KindedBlock, 0, len(blocks))
	for _, blk := range blocks {
		out = append(out, KindedBlock{
			ID:    blk.ID,
			Kind:  MapBlockKind(blk.Block.Kind),
			Block: blk.Block,
		})
	}
	return out
}
