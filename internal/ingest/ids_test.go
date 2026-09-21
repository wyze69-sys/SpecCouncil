package ingest_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/wyze69-sys/SpecCouncil/internal/ingest"
)

// idCase is one table-driven ID assignment case. want is asserted as exact ID
// strings, never by count or uniqueness alone.
type idCase struct {
	name  string
	input []ingest.Block
	want  []string
}

func ids(entries []ingest.IdentifiedBlock) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.ID)
	}
	return out
}

// ib is a compact IdentifiedBlock builder for order-preservation assertions.
func ib(id string, kind ingest.BlockKind, text string, order int) ingest.IdentifiedBlock {
	return ingest.IdentifiedBlock{ID: id, Block: ingest.Block{Kind: kind, Text: text, Order: order}}
}

func idCases() []idCase {
	return []idCase{
		{
			name: "author id preserved",
			input: []ingest.Block{
				b(ingest.BlockParagraph, "REQ-12 the system must reject empty titles", 0),
			},
			want: []string{"REQ-12"},
		},
		{
			name: "author id followed by colon",
			input: []ingest.Block{
				b(ingest.BlockParagraph, "FR-034: the system must log every decision", 0),
			},
			want: []string{"FR-034"},
		},
		{
			name: "author id followed by space",
			input: []ingest.Block{
				b(ingest.BlockParagraph, "NFR-6 latency budget is 200ms", 0),
			},
			want: []string{"NFR-6"},
		},
		{
			name: "author id followed by close paren",
			input: []ingest.Block{
				b(ingest.BlockParagraph, "AC-17) review completes within the deadline", 0),
			},
			want: []string{"AC-17"},
		},
		{
			name: "author id followed by period",
			input: []ingest.Block{
				b(ingest.BlockParagraph, "REQ-7. Audit records are immutable", 0),
			},
			want: []string{"REQ-7"},
		},
		{
			name: "author id at end of text",
			input: []ingest.Block{
				b(ingest.BlockParagraph, "REQ-12", 0),
			},
			want: []string{"REQ-12"},
		},
		{
			name: "heading text, list item, and table row author id placement",
			input: []ingest.Block{
				b(ingest.BlockHeading, "Requirements", 0),
				b(ingest.BlockListItem, "FR-2 the session queue is FIFO", 1),
				b(ingest.BlockTableRow, "| NFR-3 | 500ms budget |", 2),
			},
			want: []string{"u0", "FR-2", "u2"},
		},
		{
			name: "hyphenless, digitless, and leading-hyphen tokens do not match",
			input: []ingest.Block{
				b(ingest.BlockParagraph, "REQ12 no hyphen", 0),
				b(ingest.BlockParagraph, "-12 no leading letter", 1),
				b(ingest.BlockParagraph, "REQ- no digits", 2),
			},
			want: []string{"u0", "u1", "u2"},
		},
		{
			name: "digits followed by a letter do not match",
			input: []ingest.Block{
				b(ingest.BlockParagraph, "REQ-12abc the suffix makes this prose", 0),
			},
			want: []string{"u0"},
		},
		{
			name: "digits followed by a hyphen are not an author id",
			input: []ingest.Block{
				b(ingest.BlockParagraph, "REQ-12-2 is not an author id boundary", 0),
			},
			want: []string{"u0"},
		},
		{
			name: "code blocks are never scanned for an author id",
			input: []ingest.Block{
				b(ingest.BlockCode, "REQ-12 the system must reject empty titles", 0),
			},
			want: []string{"u0"},
		},
		{
			name: "code block alongside matching prose blocks",
			input: []ingest.Block{
				b(ingest.BlockParagraph, "REQ-12 prose keeps its id", 0),
				b(ingest.BlockCode, "REQ-12 code does not", 1),
				b(ingest.BlockParagraph, "no author id here", 2),
			},
			want: []string{"REQ-12", "u1", "u2"},
		},
		{
			name: "generated ids follow block order",
			input: []ingest.Block{
				b(ingest.BlockHeading, "Design", 0),
				b(ingest.BlockParagraph, "Intro prose.", 1),
				b(ingest.BlockListItem, "a bullet", 2),
				b(ingest.BlockTableRow, "| a | b |", 3),
				b(ingest.BlockCode, "println(1)", 4),
			},
			want: []string{"u0", "u1", "u2", "u3", "u4"},
		},
		{
			name: "generated ids use block order not slice index",
			input: []ingest.Block{
				b(ingest.BlockParagraph, "first", 7),
				b(ingest.BlockParagraph, "second", 9),
			},
			want: []string{"u7", "u9"},
		},
		{
			name: "two-way author id collision",
			input: []ingest.Block{
				b(ingest.BlockParagraph, "REQ-12 the system must reject empty titles", 0),
				b(ingest.BlockParagraph, "REQ-12 the system must reject blank bodies", 1),
			},
			want: []string{"REQ-12", "REQ-12-2"},
		},
		{
			name: "three-way author id collision",
			input: []ingest.Block{
				b(ingest.BlockListItem, "REQ-12 first", 0),
				b(ingest.BlockListItem, "REQ-12 second", 1),
				b(ingest.BlockListItem, "REQ-12 third", 2),
			},
			want: []string{"REQ-12", "REQ-12-2", "REQ-12-3"},
		},
		{
			name: "literal -2 is not an author id so it takes a generated id",
			input: []ingest.Block{
				b(ingest.BlockParagraph, "REQ-12 first", 0),
				b(ingest.BlockParagraph, "REQ-12 second", 1),
				b(ingest.BlockParagraph, "REQ-12-2 third", 2),
			},
			// "REQ-12-2 third" does not end in an allowed boundary, so it gets
			// no author ID at all and falls through to the generated id.
			want: []string{"REQ-12", "REQ-12-2", "u2"},
		},
		{
			name: "a suffixed literal is avoided by the whole-base suffix search",
			input: []ingest.Block{
				b(ingest.BlockParagraph, "REQ-1 first", 0),
				b(ingest.BlockParagraph, "REQ-1 second", 1),
				b(ingest.BlockParagraph, "REQ-1-2 quoted elsewhere", 2),
				b(ingest.BlockParagraph, "REQ-1 fourth", 3),
			},
			// The bare base literal reserves "REQ-1-2" for base "REQ-1", so the
			// suffix search skips it and takes the smallest free integer, "-3".
			want: []string{"REQ-1", "REQ-1-2", "u2", "REQ-1-3"},
		},
		{
			name: "author id case is preserved exactly",
			input: []ingest.Block{
				b(ingest.BlockParagraph, "req-12 lowercase stays lowercase", 0),
				b(ingest.BlockParagraph, "FR-034 mixed case stays", 1),
			},
			want: []string{"req-12", "FR-034"},
		},
		{
			name: "non-ascii leading letter is not an author id",
			input: []ingest.Block{
				b(ingest.BlockParagraph, "\u00dcnicode-12 not ascii", 0),
			},
			want: []string{"u0"},
		},
		{
			name:  "empty input",
			input: nil,
			want:  []string{},
		},
	}
}

func TestAssignIDsTable(t *testing.T) {
	for _, tc := range idCases() {
		t.Run(tc.name, func(t *testing.T) {
			got := ingest.AssignIDs(tc.input)
			if len(got) != len(tc.want) {
				t.Fatalf("AssignIDs(%#v) returned %d entries, want %d", tc.input, len(got), len(tc.want))
			}
			if gotIDs := ids(got); !reflect.DeepEqual(gotIDs, tc.want) {
				t.Errorf("AssignIDs(%#v)\n got ids: %#v\nwant ids: %#v", tc.input, gotIDs, tc.want)
			}
			assertUniqueIDs(t, got)
			for i := range tc.input {
				if !reflect.DeepEqual(got[i].Block, tc.input[i]) {
					t.Errorf("entry %d block changed: %#v, want %#v", i, got[i].Block, tc.input[i])
				}
			}
		})
	}
}

func TestAssignIDsEmptyInputReturnsNonNilEmptySlice(t *testing.T) {
	got := ingest.AssignIDs(nil)
	if got == nil {
		t.Fatalf("AssignIDs(nil) = nil, want a non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("AssignIDs(nil) = %#v, want an empty slice", got)
	}

	empty := ingest.AssignIDs([]ingest.Block{})
	if empty == nil {
		t.Fatalf("AssignIDs([]Block{}) = nil, want a non-nil empty slice")
	}
	if len(empty) != 0 {
		t.Fatalf("AssignIDs([]Block{}) = %#v, want an empty slice", empty)
	}
}

func TestAssignIDsPreservesBlockAndOrder(t *testing.T) {
	input := ingest.ParseBlocks(multiKindDocument("\n"))
	if len(input) < 6 {
		t.Fatalf("fixture produced %d blocks, want at least 6", len(input))
	}

	got := ingest.AssignIDs(input)
	if len(got) != len(input) {
		t.Fatalf("AssignIDs returned %d entries, want %d", len(got), len(input))
	}
	for i := range input {
		if !reflect.DeepEqual(got[i].Block, input[i]) {
			t.Errorf("entry %d block changed\n got: %#v\nwant: %#v", i, got[i].Block, input[i])
		}
	}
}

func TestAssignIDsDeepEqualOrderPreservationForMultiKindInput(t *testing.T) {
	input := []ingest.Block{
		b(ingest.BlockHeading, "Design", 0),
		b(ingest.BlockParagraph, "prose without an id", 1),
		b(ingest.BlockListItem, "FR-2 a bullet requirement", 2),
		b(ingest.BlockTableRow, "| NFR-3 | budget |", 3),
		b(ingest.BlockCode, "REQ-1 := literal()", 4),
	}
	want := []ingest.IdentifiedBlock{
		ib("u0", ingest.BlockHeading, "Design", 0),
		ib("u1", ingest.BlockParagraph, "prose without an id", 1),
		ib("FR-2", ingest.BlockListItem, "FR-2 a bullet requirement", 2),
		ib("u3", ingest.BlockTableRow, "| NFR-3 | budget |", 3),
		ib("u4", ingest.BlockCode, "REQ-1 := literal()", 4),
	}

	got := ingest.AssignIDs(input)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AssignIDs(multi-kind)\n got: %#v\nwant: %#v", got, want)
	}
}

// assertUniqueIDs fails when any assigned ID repeats or is empty.
func assertUniqueIDs(t *testing.T, got []ingest.IdentifiedBlock) {
	t.Helper()
	seen := map[string]int{}
	for i, entry := range got {
		if entry.ID == "" {
			t.Errorf("entry %d has an empty id", i)
		}
		if prev, dup := seen[entry.ID]; dup {
			t.Errorf("id %q appears at entries %d and %d", entry.ID, prev, i)
		}
		seen[entry.ID] = i
	}
}

func TestAssignIDsMixedDocumentUsesAuthorIdsAndGeneratedIds(t *testing.T) {
	input := ingest.ParseBlocks(strings.Join([]string{
		"# Review Design",
		"",
		"REQ-12 the system must reject empty titles",
		"",
		"- REQ-12 the system must reject blank bodies",
		"- plain bullet with no author id",
		"",
		"FR-034: the system must log every decision",
		"",
		"```go",
		"req12 := build()",
		"```",
		"",
		"FR-2 first colliding requirement",
		"",
		"FR-2-2 a bare-base literal that reserves the first suffix",
		"",
		"FR-2 second colliding requirement",
		"",
	}, "\n"))

	got := ingest.AssignIDs(input)
	// Block order from ParseBlocks: 0 heading, 1 REQ-12 paragraph, 2 REQ-12
	// list item, 3 plain list item, 4 FR-034 paragraph, 5 code, 6 FR-2
	// paragraph, 7 bare-base literal paragraph, 8 FR-2 paragraph.
	// "FR-2-2 ..." is not an author ID (digits followed by a hyphen), so block 7
	// is generated; because block 7's id is generated nothing consumes "FR-2-2",
	// so block 8 still takes the smallest free integer "-2".
	wantIDs := []string{"u0", "REQ-12", "REQ-12-2", "u3", "FR-034", "u5", "FR-2", "u7", "FR-2-2"}
	gotIDs := ids(got)
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("AssignIDs(mixed document)\n got ids: %#v\nwant ids: %#v", gotIDs, wantIDs)
	}
	assertUniqueIDs(t, got)

	kinds := map[ingest.BlockKind]bool{}
	for _, entry := range got {
		kinds[entry.Block.Kind] = true
	}
	for _, kind := range []ingest.BlockKind{
		ingest.BlockHeading,
		ingest.BlockParagraph,
		ingest.BlockListItem,
		ingest.BlockCode,
	} {
		if !kinds[kind] {
			t.Errorf("fixture did not produce any %s block", kind)
		}
	}
}

func TestAssignIDsGeneratedIDsAreUniqueOnMixedDocument(t *testing.T) {
	input := ingest.ParseBlocks(strings.Join([]string{
		"# Design",
		"",
		"Intro prose.",
		"",
		"- bullet one",
		"- bullet two",
		"",
		"| a | b |",
		"| - | - |",
		"",
		"```go",
		"println(1)",
		"```",
		"",
		"Tail prose.",
	}, "\n"))

	got := ingest.AssignIDs(input)
	generated := 0
	for _, entry := range got {
		if strings.HasPrefix(entry.ID, "u") {
			generated++
		}
	}
	if generated != len(got) {
		t.Fatalf("expected every id to be generated, got ids %#v", ids(got))
	}
	assertUniqueIDs(t, got)
}

func TestAssignIDsIsDeterministic(t *testing.T) {
	input := ingest.ParseBlocks(multiKindDocument("\r\n"))
	base := len(input)
	input = append(input,
		ingest.Block{Kind: ingest.BlockParagraph, Text: "REQ-12 first duplicate", Order: base},
		ingest.Block{Kind: ingest.BlockParagraph, Text: "REQ-12 second duplicate", Order: base + 1},
		ingest.Block{Kind: ingest.BlockParagraph, Text: "REQ-12 third duplicate", Order: base + 2},
	)

	first := ingest.AssignIDs(input)
	second := ingest.AssignIDs(input)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("AssignIDs differs across identical calls\nfirst:  %#v\nsecond: %#v", first, second)
	}
	if len(first) < 9 {
		t.Fatalf("fixture was too trivial: %d entries", len(first))
	}
	assertUniqueIDs(t, first)

	// AssignIDs must not mutate or alias its input.
	third := ingest.AssignIDs(input)
	if !reflect.DeepEqual(first, third) {
		t.Errorf("third call differs from the first:\nfirst: %#v\nthird: %#v", first, third)
	}
	for i := range input {
		if !reflect.DeepEqual(first[i].Block, input[i]) {
			t.Errorf("entry %d block was mutated: %#v, want %#v", i, first[i].Block, input[i])
		}
	}
}

// TestAssignIDsContractExamples pins the exact examples written in the contract
// so the documented rule and the code cannot drift apart.
func TestAssignIDsContractExamples(t *testing.T) {
	matching := []struct {
		text string
		want string
	}{
		{"REQ-12 the system must x", "REQ-12"},
		{"REQ-12", "REQ-12"},
		{"FR-034: x", "FR-034"},
		{"NFR-6 x", "NFR-6"},
		{"AC-17) x", "AC-17"},
		{"REQ-12\tx", "REQ-12"},
		{"REQ-12.x", "REQ-12"},
	}
	for _, tc := range matching {
		got := ingest.AssignIDs([]ingest.Block{b(ingest.BlockParagraph, tc.text, 0)})
		if len(got) != 1 || got[0].ID != tc.want {
			t.Errorf("AssignIDs(%q) ids = %#v, want [%q]", tc.text, ids(got), tc.want)
			continue
		}
		if got[0].Block.Text != tc.text {
			t.Errorf("AssignIDs(%q) mutated block text to %q", tc.text, got[0].Block.Text)
		}
	}

	nonMatching := []string{"REQ12 x", "-12 x", "REQ- x", "REQ-12abc x", "REQ-12-2 x"}
	for _, text := range nonMatching {
		got := ingest.AssignIDs([]ingest.Block{b(ingest.BlockParagraph, text, 0)})
		if len(got) != 1 || got[0].ID != "u0" {
			t.Errorf("AssignIDs(%q) ids = %#v, want [u0]", text, ids(got))
		}
	}
}
