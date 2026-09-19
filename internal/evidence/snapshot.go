package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// UnitKind classifies one piece of frozen design evidence.
type UnitKind string

const (
	UnitBrief       UnitKind = "brief"
	UnitRequirement UnitKind = "requirement"
	UnitComponent   UnitKind = "component"
	UnitFlow        UnitKind = "flow"
	UnitConstraint  UnitKind = "constraint"
	UnitDataRule    UnitKind = "data_rule"
)

// IsValidUnitKind reports whether k is a recognized evidence kind.
func IsValidUnitKind(k UnitKind) bool {
	switch k {
	case UnitBrief, UnitRequirement, UnitComponent, UnitFlow, UnitConstraint, UnitDataRule:
		return true
	}
	return false
}

// Unit is one addressable piece of the frozen snapshot.
//
// A finding cites units by ID through basis_refs, so unit IDs are the
// citation vocabulary of the whole product.
type Unit struct {
	ID   string   `json:"id"`
	Kind UnitKind `json:"kind"`
	Text string   `json:"text"`
}

// Snapshot is the immutable evidence a review session is executed against.
//
// All four reviewer roles receive the same frozen snapshot. v1 has no
// per-role section selection.
type Snapshot struct {
	ID    string `json:"id"`
	Hash  string `json:"hash"`
	Units []Unit `json:"units"`

	refs map[string]struct{}
}

// Freeze builds an immutable snapshot and computes its deterministic hash.
//
// The returned snapshot is a value type: later mutations of the input slice
// cannot change it, and every role reads exactly these units.
func Freeze(id string, units []Unit) (Snapshot, error) {
	if strings.TrimSpace(id) == "" {
		return Snapshot{}, errors.New("evidence: snapshot id must not be empty")
	}
	if len(units) == 0 {
		return Snapshot{}, errors.New("evidence: snapshot needs at least one unit")
	}

	// Copy and verify the caller's units before anything else observes them.
	frozen := make([]Unit, len(units))
	copy(frozen, units)

	refs := make(map[string]struct{}, len(frozen))
	for i, u := range frozen {
		if strings.TrimSpace(u.ID) == "" {
			return Snapshot{}, fmt.Errorf("evidence: unit %d has an empty id", i)
		}
		if strings.TrimSpace(u.Text) == "" {
			return Snapshot{}, fmt.Errorf("evidence: unit %q has empty text", u.ID)
		}
		if !IsValidUnitKind(u.Kind) {
			return Snapshot{}, fmt.Errorf("evidence: unit %q has unknown kind %q", u.ID, u.Kind)
		}
		if _, dup := refs[u.ID]; dup {
			return Snapshot{}, fmt.Errorf("evidence: duplicate unit id %q", u.ID)
		}
		refs[u.ID] = struct{}{}
	}

	hash, err := canonicalHash(frozen)
	if err != nil {
		return Snapshot{}, err
	}

	return Snapshot{ID: id, Hash: hash, Units: frozen, refs: refs}, nil
}

// HasRef reports whether ref is addressable inside this snapshot.
func (s Snapshot) HasRef(ref string) bool {
	if s.refs != nil {
		_, ok := s.refs[ref]
		return ok
	}
	for _, u := range s.Units {
		if u.ID == ref {
			return true
		}
	}
	return false
}

// Refs returns every addressable unit ID, sorted.
func (s Snapshot) Refs() []string {
	out := make([]string, 0, len(s.Units))
	for _, u := range s.Units {
		out = append(out, u.ID)
	}
	sort.Strings(out)
	return out
}

// canonicalHash hashes units in a stable order, independent of input order.
func canonicalHash(units []Unit) (string, error) {
	ordered := make([]Unit, len(units))
	copy(ordered, units)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })

	payload, err := json.Marshal(ordered)
	if err != nil {
		return "", fmt.Errorf("evidence: canonical encoding failed: %w", err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}
