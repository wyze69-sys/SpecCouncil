package ingest_test

import (
	"reflect"
	"strconv"
	"testing"

	"github.com/wyze69-sys/SpecCouncil/internal/evidence"
	"github.com/wyze69-sys/SpecCouncil/internal/ingest"
)

// kindCase is one table-driven mapping case. want is the exact UnitKind string,
// so a mapping change cannot pass by merely staying "valid".
type kindCase struct {
	name string
	kind ingest.BlockKind
	want evidence.UnitKind
}

func kindCases() []kindCase {
	return []kindCase{
		{"heading maps to brief", ingest.BlockHeading, evidence.UnitBrief},
		{"paragraph maps to requirement", ingest.BlockParagraph, evidence.UnitRequirement},
		{"list item maps to requirement", ingest.BlockListItem, evidence.UnitRequirement},
		{"table row maps to data rule", ingest.BlockTableRow, evidence.UnitDataRule},
		{"code maps to constraint", ingest.BlockCode, evidence.UnitConstraint},
	}
}

// kb is a compact KindedBlock builder for exact-slice assertions.
func kb(id string, kind evidence.UnitKind, block ingest.Block) ingest.KindedBlock {
	return ingest.KindedBlock{ID: id, Kind: kind, Block: block}
}

// TestMapBlockKindKnownKinds asserts the exact UnitKind string for every known
// BlockKind, so the frozen mapping cannot drift silently.
func TestMapBlockKindKnownKinds(t *testing.T) {
	for _, tc := range kindCases() {
		t.Run(tc.name, func(t *testing.T) {
			got := ingest.MapBlockKind(tc.kind)
			if got != tc.want {
				t.Fatalf("MapBlockKind(%q) = %q, want %q", tc.kind, got, tc.want)
			}
		})
	}
}

// TestMapBlockKindKnownKindsAreValid asserts every kind this slice produces is
// one that evidence.Freeze will accept later.
func TestMapBlockKindKnownKindsAreValid(t *testing.T) {
	for _, tc := range kindCases() {
		if got := ingest.MapBlockKind(tc.kind); !evidence.IsValidUnitKind(got) {
			t.Errorf("MapBlockKind(%q) = %q, which IsValidUnitKind rejects", tc.kind, got)
		}
	}
}

// TestMapBlockKindUnknownIsInvalid pins the defensive default: an unrecognized
// block kind maps to the empty UnitKind, which evidence.Freeze must reject.
func TestMapBlockKindUnknownIsInvalid(t *testing.T) {
	for _, kind := range []ingest.BlockKind{"bogus", "", "Heading", "heading ", "code_block"} {
		got := ingest.MapBlockKind(kind)
		if got != "" {
			t.Errorf("MapBlockKind(%q) = %q, want %q", kind, got, "")
		}
		if evidence.IsValidUnitKind(got) {
			t.Errorf("IsValidUnitKind(%q) = true, want false", got)
		}
	}
}

// TestAssignKindsMixedDocument asserts the whole output slice exactly: order,
// kinds, and unchanged IDs and blocks for one block of every kind.
func TestAssignKindsMixedDocument(t *testing.T) {
	input := []ingest.IdentifiedBlock{
		ib("u0", ingest.BlockHeading, "Design Review", 0),
		ib("REQ-12", ingest.BlockParagraph, "REQ-12 the system must reject empty titles", 1),
		ib("u2", ingest.BlockListItem, "first requirement", 2),
		ib("u3", ingest.BlockTableRow, "| title | non-empty |", 3),
		ib("u4", ingest.BlockCode, "func main() {}", 4),
	}

	want := []ingest.KindedBlock{
		kb("u0", evidence.UnitBrief, b(ingest.BlockHeading, "Design Review", 0)),
		kb("REQ-12", evidence.UnitRequirement, b(ingest.BlockParagraph, "REQ-12 the system must reject empty titles", 1)),
		kb("u2", evidence.UnitRequirement, b(ingest.BlockListItem, "first requirement", 2)),
		kb("u3", evidence.UnitDataRule, b(ingest.BlockTableRow, "| title | non-empty |", 3)),
		kb("u4", evidence.UnitConstraint, b(ingest.BlockCode, "func main() {}", 4)),
	}

	got := ingest.AssignKinds(input)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AssignKinds mixed document differs\n got: %#v\nwant: %#v", got, want)
	}

	// The kind sequence must be readable independently of the blocks.
	kinds := make([]evidence.UnitKind, 0, len(got))
	for _, entry := range got {
		kinds = append(kinds, entry.Kind)
	}
	wantKinds := []evidence.UnitKind{
		evidence.UnitBrief,
		evidence.UnitRequirement,
		evidence.UnitRequirement,
		evidence.UnitDataRule,
		evidence.UnitConstraint,
	}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Fatalf("AssignKinds kind sequence = %#v, want %#v", kinds, wantKinds)
	}

	// Order, ID, and Block must be carried through unchanged.
	for i := range input {
		if got[i].ID != input[i].ID {
			t.Errorf("entry %d ID = %q, want %q", i, got[i].ID, input[i].ID)
		}
		if !reflect.DeepEqual(got[i].Block, input[i].Block) {
			t.Errorf("entry %d block = %#v, want %#v", i, got[i].Block, input[i].Block)
		}
		if got[i].Kind != ingest.MapBlockKind(input[i].Block.Kind) {
			t.Errorf("entry %d kind = %q, want MapBlockKind(%q) = %q",
				i, got[i].Kind, input[i].Block.Kind, ingest.MapBlockKind(input[i].Block.Kind))
		}
	}
}

// TestAssignKindsPassesThroughIDsAndBlocks uses realistic distinct IDs from both
// the author-ID and generated-ID families and checks each pair is carried
// through byte-for-byte, including for an unrecognized block kind.
func TestAssignKindsPassesThroughIDsAndBlocks(t *testing.T) {
	input := ingest.AssignIDs([]ingest.Block{
		b(ingest.BlockParagraph, "REQ-12 the system must reject empty titles", 0),
		b(ingest.BlockHeading, "Design Review", 1),
	})

	wantIDs := []string{"REQ-12", "u1"}
	wantKinds := []evidence.UnitKind{evidence.UnitRequirement, evidence.UnitBrief}

	got := ingest.AssignKinds(input)
	if len(got) != len(input) {
		t.Fatalf("AssignKinds returned %d entries, want %d", len(got), len(input))
	}
	for i := range got {
		if got[i].ID != wantIDs[i] {
			t.Errorf("entry %d ID = %q, want %q", i, got[i].ID, wantIDs[i])
		}
		if got[i].Kind != wantKinds[i] {
			t.Errorf("entry %d kind = %q, want %q", i, got[i].Kind, wantKinds[i])
		}
		if !reflect.DeepEqual(got[i].Block, input[i].Block) {
			t.Errorf("entry %d block = %#v, want %#v", i, got[i].Block, input[i].Block)
		}
	}

	// An unrecognized block kind survives with an empty, invalid kind.
	bogus := []ingest.IdentifiedBlock{ib("u0", ingest.BlockKind("bogus"), "mystery", 0)}
	gotBogus := ingest.AssignKinds(bogus)
	if len(gotBogus) != 1 {
		t.Fatalf("AssignKinds(bogus) returned %d entries, want 1", len(gotBogus))
	}
	if gotBogus[0].Kind != "" {
		t.Errorf("AssignKinds(bogus) kind = %q, want %q", gotBogus[0].Kind, "")
	}
	if gotBogus[0].Block.Text != "mystery" {
		t.Errorf("AssignKinds(bogus) mutated text to %q", gotBogus[0].Block.Text)
	}
}

// TestAssignKindsEmptyInputRequiresNonNilSlice pins that nil input and empty
// non-nil input both yield a non-nil empty slice.
func TestAssignKindsEmptyInputRequiresNonNilSlice(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input []ingest.IdentifiedBlock
	}{
		{"nil input", nil},
		{"empty input", []ingest.IdentifiedBlock{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ingest.AssignKinds(tc.input)
			if got == nil {
				t.Fatalf("AssignKinds(%s) = nil, want non-nil []KindedBlock{}", tc.name)
			}
			if len(got) != 0 {
				t.Fatalf("AssignKinds(%s) = %#v, want length 0", tc.name, got)
			}
			if !reflect.DeepEqual(got, []ingest.KindedBlock{}) {
				t.Fatalf("AssignKinds(%s) = %#v, want []KindedBlock{}", tc.name, got)
			}
		})
	}
}

// TestAssignKindsIsDeterministic maps a non-trivial parsed document twice and
// requires identical results, with no mutation of the input.
func TestAssignKindsIsDeterministic(t *testing.T) {
	input := ingest.AssignIDs(ingest.ParseBlocks(multiKindDocument("\n")))
	base := len(input)
	input = append(input, ingest.IdentifiedBlock{
		ID:    "u" + strconv.Itoa(base),
		Block: ingest.Block{Kind: ingest.BlockKind("bogus"), Text: "unknown kind block", Order: base},
	})

	first := ingest.AssignKinds(input)
	second := ingest.AssignKinds(input)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("AssignKinds differs across identical calls\nfirst:  %#v\nsecond: %#v", first, second)
	}
	if len(first) < 9 {
		t.Fatalf("fixture was too trivial: %d entries", len(first))
	}
	// Every kind derived from a real parsed block must be valid; the last entry
	// is the deliberately unrecognized kind and must stay invalid.
	for i, entry := range first[:base] {
		if !evidence.IsValidUnitKind(entry.Kind) {
			t.Errorf("entry %d kind %q is not a valid UnitKind", i, entry.Kind)
		}
	}
	if last := first[len(first)-1]; evidence.IsValidUnitKind(last.Kind) || last.Kind != "" {
		t.Errorf("bogus entry kind = %q, want the invalid empty UnitKind", last.Kind)
	}

	// Input must still be intact after two mapping passes.
	for i := range input {
		if first[i].ID != input[i].ID || !reflect.DeepEqual(first[i].Block, input[i].Block) {
			t.Errorf("entry %d was mutated: %#v, want ID %q block %#v",
				i, first[i], input[i].ID, input[i].Block)
		}
	}
}
