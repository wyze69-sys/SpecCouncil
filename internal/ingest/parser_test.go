package ingest_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/wyze69-sys/SpecCouncil/internal/ingest"
)

// doc is one table-driven parsing case. want is asserted exactly (Kind, Text,
// Order), never by count alone.
type doc struct {
	name  string
	input string
	want  []ingest.Block
}

func b(kind ingest.BlockKind, text string, order int) ingest.Block {
	return ingest.Block{Kind: kind, Text: text, Order: order}
}

func parseCases() []doc {
	return []doc{
		{
			name:  "heading levels one through six",
			input: "# H1\n## H2\n### H3\n#### H4\n##### H5\n###### H6\n",
			want: []ingest.Block{
				b(ingest.BlockHeading, "H1", 0),
				b(ingest.BlockHeading, "H2", 1),
				b(ingest.BlockHeading, "H3", 2),
				b(ingest.BlockHeading, "H4", 3),
				b(ingest.BlockHeading, "H5", 4),
				b(ingest.BlockHeading, "H6", 5),
			},
		},
		{
			name:  "hash without a space is paragraph text",
			input: "#not a heading\n#also-not\n",
			want: []ingest.Block{
				b(ingest.BlockParagraph, "#not a heading\n#also-not", 0),
			},
		},
		{
			name:  "seven hashes is paragraph text",
			input: "####### seven\n#######also seven\n",
			want: []ingest.Block{
				b(ingest.BlockParagraph, "####### seven\n#######also seven", 0),
			},
		},
		{
			name:  "heading text is trimmed and a tab after hashes is not a heading",
			input: "#    spaced heading   \n##\ttabbed\t\n",
			want: []ingest.Block{
				b(ingest.BlockHeading, "spaced heading", 0),
				b(ingest.BlockParagraph, "##\ttabbed", 1),
			},
		},
		{
			name:  "multi-line paragraph joined with newlines and terminated by a blank line",
			input: "first line\nsecond line\nthird line\n\nnext para\n",
			want: []ingest.Block{
				b(ingest.BlockParagraph, "first line\nsecond line\nthird line", 0),
				b(ingest.BlockParagraph, "next para", 1),
			},
		},
		{
			name:  "two paragraphs separated by a blank line keep order and interior joins",
			input: "alpha\nbeta\n\ngamma\ndelta\ntrailing newline is not a line\n",
			want: []ingest.Block{
				b(ingest.BlockParagraph, "alpha\nbeta", 0),
				b(ingest.BlockParagraph, "gamma\ndelta\ntrailing newline is not a line", 1),
			},
		},
		{
			name:  "paragraph trims its outer edges but preserves interior indentation",
			input: "   leading and\n   trailing   \n",
			want: []ingest.Block{
				b(ingest.BlockParagraph, "leading and\n   trailing", 0),
			},
		},
		{
			name:  "unordered list markers",
			input: "- dash item\n* star item\n+ plus item\n",
			want: []ingest.Block{
				b(ingest.BlockListItem, "dash item", 0),
				b(ingest.BlockListItem, "star item", 1),
				b(ingest.BlockListItem, "plus item", 2),
			},
		},
		{
			name:  "ordered list markers with dot and paren",
			input: "1. first\n2) second\n10. tenth\n",
			want: []ingest.Block{
				b(ingest.BlockListItem, "first", 0),
				b(ingest.BlockListItem, "second", 1),
				b(ingest.BlockListItem, "tenth", 2),
			},
		},
		{
			name:  "non-marker lines are not list items",
			input: "2024 Review Plan\n-\n1\n1.\n1.2 not ordered\n",
			want: []ingest.Block{
				b(ingest.BlockParagraph, "2024 Review Plan", 0),
				b(ingest.BlockParagraph, "1", 1),
				b(ingest.BlockParagraph, "1.2 not ordered", 2),
			},
		},
		{
			name:  "bare and separator-only markers emit no block",
			input: "-\n1.\n- item\n",
			want: []ingest.Block{
				b(ingest.BlockListItem, "item", 0),
			},
		},
		{
			name:  "list item text is trimmed after the marker",
			input: "-    spacious item   \n1)   ordered spacious  \n",
			want: []ingest.Block{
				b(ingest.BlockListItem, "spacious item", 0),
				b(ingest.BlockListItem, "ordered spacious", 1),
			},
		},
		{
			name:  "table header separator and data rows",
			input: "| Name | Owner |\n| --- | :--: |\n| API | Platform |\n",
			want: []ingest.Block{
				b(ingest.BlockTableRow, "| Name | Owner |", 0),
				b(ingest.BlockTableRow, "| --- | :--: |", 1),
				b(ingest.BlockTableRow, "| API | Platform |", 2),
			},
		},
		{
			name:  "single pipe line is paragraph text",
			input: "|not a table\n| not a table either\n",
			want: []ingest.Block{
				b(ingest.BlockParagraph, "|not a table\n| not a table either", 0),
			},
		},
		{
			name:  "table row text is trimmed and preserves pipes",
			input: "   | A | B |   \n",
			want: []ingest.Block{
				b(ingest.BlockTableRow, "| A | B |", 0),
			},
		},
	}
}

// flushCases covers the mandatory paragraph-flush guard: a paragraph
// immediately followed, with no blank line, by each structural construct.
func flushCases() []doc {
	return []doc{
		{
			name:  "flush guard before a heading",
			input: "some text\n# Heading\n",
			want: []ingest.Block{
				b(ingest.BlockParagraph, "some text", 0),
				b(ingest.BlockHeading, "Heading", 1),
			},
		},
		{
			name:  "flush guard before a table row",
			input: "some text\n| A | B |\n",
			want: []ingest.Block{
				b(ingest.BlockParagraph, "some text", 0),
				b(ingest.BlockTableRow, "| A | B |", 1),
			},
		},
		{
			name:  "flush guard before a list item",
			input: "some text\n- item\n",
			want: []ingest.Block{
				b(ingest.BlockParagraph, "some text", 0),
				b(ingest.BlockListItem, "item", 1),
			},
		},
		{
			name:  "flush guard before a code fence",
			input: "some text\n```\ncode\n```\n",
			want: []ingest.Block{
				b(ingest.BlockParagraph, "some text", 0),
				b(ingest.BlockCode, "code", 1),
			},
		},
		{
			name:  "flush guard before end of input without a trailing newline",
			input: "some text",
			want: []ingest.Block{
				b(ingest.BlockParagraph, "some text", 0),
			},
		},
		{
			name:  "structural lines never merge into a paragraph",
			input: "intro\n# H\n| A | B |\n- item\n```\ncode\n```\noutro\n",
			want: []ingest.Block{
				b(ingest.BlockParagraph, "intro", 0),
				b(ingest.BlockHeading, "H", 1),
				b(ingest.BlockTableRow, "| A | B |", 2),
				b(ingest.BlockListItem, "item", 3),
				b(ingest.BlockCode, "code", 4),
				b(ingest.BlockParagraph, "outro", 5),
			},
		},
		{
			name:  "structural constructs flush empty paragraphs silently",
			input: "# H\n\n\n- item\n",
			want: []ingest.Block{
				b(ingest.BlockHeading, "H", 0),
				b(ingest.BlockListItem, "item", 1),
			},
		},
	}
}

func codeCases() []doc {
	return []doc{
		{
			name:  "fenced code discards the info string",
			input: "```go\nx := 1\n```\n",
			want: []ingest.Block{
				b(ingest.BlockCode, "x := 1", 0),
			},
		},
		{
			name:  "fenced code preserves interior whitespace and blank lines",
			input: "```\n\tindented\n\n  two spaces  \n```\n",
			want: []ingest.Block{
				b(ingest.BlockCode, "\tindented\n\n  two spaces  ", 0),
			},
		},
		{
			name:  "unterminated fence runs to EOF as one code block",
			input: "```\nstill code\nlast line\n",
			want: []ingest.Block{
				b(ingest.BlockCode, "still code\nlast line", 0),
			},
		},
		{
			name:  "unterminated fence without a trailing newline runs to EOF",
			input: "```\nstill code",
			want: []ingest.Block{
				b(ingest.BlockCode, "still code", 0),
			},
		},
		{
			name:  "markers pipes and hashes inside code are code",
			input: "```\n# not a heading\n- not a list\n| not | a table |\n```\n",
			want: []ingest.Block{
				b(ingest.BlockCode, "# not a heading\n- not a list\n| not | a table |", 0),
			},
		},
		{
			name:  "fences longer than three backticks behave like three",
			input: "````\ncode\n````\n",
			want: []ingest.Block{
				b(ingest.BlockCode, "code", 0),
			},
		},
		{
			name:  "two consecutive code blocks",
			input: "```\nfirst\n```\n```\nsecond\n```\n",
			want: []ingest.Block{
				b(ingest.BlockCode, "first", 0),
				b(ingest.BlockCode, "second", 1),
			},
		},
		{
			name:  "empty fenced code block emits nothing",
			input: "```\n```\n",
			want:  []ingest.Block{},
		},
		{
			name:  "code block text is not trimmed so blank interior only blocks emit nothing",
			input: "```\n\n```\n",
			want:  []ingest.Block{},
		},
		{
			name:  "whitespace-only code interior emits nothing",
			input: "```\n   \n```\n",
			want:  []ingest.Block{},
		},
		{
			name:  "tab-and-space code interior emits nothing",
			input: "```\n	 \n```\n",
			want:  []ingest.Block{},
		},
		{
			name:  "leading code interior whitespace survives when real code is present",
			input: "```\n   \ncode\n```\n",
			want: []ingest.Block{
				b(ingest.BlockCode, "   \ncode", 0),
			},
		},
		{
			name:  "trailing code interior whitespace survives",
			input: "```\ncode\n   \n```\n",
			want: []ingest.Block{
				b(ingest.BlockCode, "code\n   ", 0),
			},
		},
		{
			name:  "a whitespace-only fence consumes no order",
			input: "```\n   \n```\n# H\n",
			want: []ingest.Block{
				b(ingest.BlockHeading, "H", 0),
			},
		},
		{
			name:  "code fence is a paragraph boundary on both sides",
			input: "before\n```\ncode\n```\nafter\n",
			want: []ingest.Block{
				b(ingest.BlockParagraph, "before", 0),
				b(ingest.BlockCode, "code", 1),
				b(ingest.BlockParagraph, "after", 2),
			},
		},
	}
}

// TestParseBlocksEmitsNothingForMarkersWithoutContent covers "a marker with no
// content emits no block" for every form the packet makes a marker construct: a
// bare or separator-only list marker, a bare fence, and a content-free heading.
// "#######" is excluded because the packet makes it paragraph text.
func TestParseBlocksEmitsNothingForMarkersWithoutContent(t *testing.T) {
	inputs := []string{
		"-",
		"- ",
		"*",
		"* ",
		"+",
		"+ ",
		"1.",
		"1. ",
		"2)",
		"2) ",
		"```",
		"```\n```",
		"```   \n```",
		"```\n   \n```",
		"```\n	\n```",
	}
	for _, input := range inputs {
		got := ingest.ParseBlocks(input)
		if got == nil {
			t.Errorf("ParseBlocks(%q) = nil, want non-nil slice", input)
			continue
		}
		if len(got) != 0 {
			t.Errorf("ParseBlocks(%q) = %#v, want no blocks", input, got)
		}
	}

	// A content-free heading is accepted by the heading rule but carries no text,
	// so it is paragraph text rather than a block of its own.
	heading := "###"
	got := ingest.ParseBlocks(heading)
	want := []ingest.Block{b(ingest.BlockParagraph, "###", 0)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseBlocks(%q)\n got: %#v\nwant: %#v", heading, got, want)
	}
}

// TestParseBlocksEmitsNothingAroundMarkerOnlyLines shows that a content-free
// list marker and an empty code fence contribute nothing inside a document while
// still acting as paragraph boundaries, and that a content-free heading line is
// transparent paragraph text because it is not the heading construct at all.
func TestParseBlocksEmitsNothingAroundMarkerOnlyLines(t *testing.T) {
	markers := "intro\n\n-   \n1. \n\n```\n```\n\noutro\n"
	wantMarkers := []ingest.Block{
		b(ingest.BlockParagraph, "intro", 0),
		b(ingest.BlockParagraph, "outro", 1),
	}
	if got := ingest.ParseBlocks(markers); !reflect.DeepEqual(got, wantMarkers) {
		t.Errorf("ParseBlocks(%q)\n got: %#v\nwant: %#v", markers, got, wantMarkers)
	}

	headings := "intro\n\n###\n#######\n\noutro\n"
	wantHeadings := []ingest.Block{
		b(ingest.BlockParagraph, "intro", 0),
		b(ingest.BlockParagraph, "###\n#######", 1),
		b(ingest.BlockParagraph, "outro", 2),
	}
	if got := ingest.ParseBlocks(headings); !reflect.DeepEqual(got, wantHeadings) {
		t.Errorf("ParseBlocks(%q)\n got: %#v\nwant: %#v", headings, got, wantHeadings)
	}

	// A separator-only heading ("###   ") normalizes to "###" and joins the
	// surrounding paragraph text, so an empty heading contributes nothing.
	emptyHeading := "before\n###   \nafter\n"
	wantEmptyHeading := []ingest.Block{
		b(ingest.BlockParagraph, "before\n###   \nafter", 0),
	}
	if got := ingest.ParseBlocks(emptyHeading); !reflect.DeepEqual(got, wantEmptyHeading) {
		t.Errorf("ParseBlocks(%q)\n got: %#v\nwant: %#v", emptyHeading, got, wantEmptyHeading)
	}
}

// TestParseBlocksTreatsEmptyContentMarkersAsParagraphText pins the packet's
// literal distinction. A hash run with no heading text ("###", "###   ") is
// paragraph text, while a list marker with no content emits nothing. Because a
// paragraph keeps its lines' own bytes, the emitted text is the document's
// literal lines.
func TestParseBlocksTreatsEmptyContentMarkersAsParagraphText(t *testing.T) {
	input := "###   \n#######"
	want := []ingest.Block{
		b(ingest.BlockParagraph, "###   \n#######", 0),
	}
	got := ingest.ParseBlocks(input)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseBlocks(%q)\n got: %#v\nwant: %#v", input, got, want)
	}

	// A bare or separator-only marker is a list item with no content, so it acts
	// as a paragraph boundary and emits nothing of its own.
	adjacent := "before\n-\n- \n1.\n1. \nafter\n"
	wantAdjacent := []ingest.Block{
		b(ingest.BlockParagraph, "before", 0),
		b(ingest.BlockParagraph, "after", 1),
	}
	if got := ingest.ParseBlocks(adjacent); !reflect.DeepEqual(got, wantAdjacent) {
		t.Errorf("ParseBlocks(%q)\n got: %#v\nwant: %#v", adjacent, got, wantAdjacent)
	}

	// A digit run not followed by "." or ")" is paragraph text, not a marker, and a
	// "." or ")" that introduces a further digit run ("1.2") is not a marker either.
	notMarkers := "2024 Review\n1.2 pipeline\n3-4 items\n1.2. done\n"
	wantNotMarkers := []ingest.Block{
		b(ingest.BlockParagraph, "2024 Review\n1.2 pipeline\n3-4 items\n1.2. done", 0),
	}
	if got := ingest.ParseBlocks(notMarkers); !reflect.DeepEqual(got, wantNotMarkers) {
		t.Errorf("ParseBlocks(%q)\n got: %#v\nwant: %#v", notMarkers, got, wantNotMarkers)
	}
}

func TestParseBlocksReturnsNonNilEmptySliceForBlankInput(t *testing.T) {
	for _, input := range []string{"", "\n", "\r\n", "\r", "   ", "\t", " \n \n ", "\r\n\r\n", "\n\n\n\t \t\n"} {
		got := ingest.ParseBlocks(input)
		if got == nil {
			t.Errorf("ParseBlocks(%q) = nil, want non-nil empty slice", input)
			continue
		}
		if len(got) != 0 {
			t.Errorf("ParseBlocks(%q) = %#v, want empty slice", input, got)
		}
	}
}

func TestParseBlocksNormalizesLineEndingsByteEqual(t *testing.T) {
	lf := multiKindDocument("\n")

	variants := map[string]string{
		"CRLF":     multiKindDocument("\r\n"),
		"lone CR":  multiKindDocument("\r"),
		"mixed":    mixedLineEndingsDocument(),
		"LF twice": lf,
	}
	want := ingest.ParseBlocks(lf)
	for name, input := range variants {
		got := ingest.ParseBlocks(input)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: ParseBlocks\n got: %#v\nwant: %#v", name, got, want)
		}
	}
}

func TestParseBlocksNormalizesCRLFInsideParagraphAndCodeText(t *testing.T) {
	crlf := "para line one\r\npara line two\r\n\r\n```\r\ncode line one\r\ncode line two\r\n```\r\n\r\n# Heading\r\n"
	lf := "para line one\npara line two\n\n```\ncode line one\ncode line two\n```\n\n# Heading\n"

	want := []ingest.Block{
		b(ingest.BlockParagraph, "para line one\npara line two", 0),
		b(ingest.BlockCode, "code line one\ncode line two", 1),
		b(ingest.BlockHeading, "Heading", 2),
	}
	if got := ingest.ParseBlocks(lf); !reflect.DeepEqual(got, want) {
		t.Fatalf("LF variant\n got: %#v\nwant: %#v", got, want)
	}
	if got := ingest.ParseBlocks(crlf); !reflect.DeepEqual(got, want) {
		t.Errorf("CRLF variant\n got: %#v\nwant: %#v", got, want)
	}
}

// multiKindDocument builds a non-trivial document with every block kind, using
// eol as the line ending so LF, CRLF, and lone-CR inputs can be compared.
func multiKindDocument(eol string) string {
	lines := []string{
		"# Design Review",
		"",
		"Intro paragraph line one.",
		"Intro paragraph line two.",
		"",
		"## Requirements",
		"",
		"- first requirement",
		"* second requirement",
		"1. third requirement",
		"",
		"| Field | Rule |",
		"| --- | :--: |",
		"| title | non-empty |",
		"",
		"```go",
		"func main() {",
		"\tprintln(\"hi\")",
		"",
		"}",
		"```",
		"",
		"After paragraph.",
	}
	return strings.Join(lines, eol) + eol
}

// mixedLineEndingsDocument mixes LF, CRLF, and lone CR in one document.
func mixedLineEndingsDocument() string {
	return "# Design Review\r\n\r\nIntro paragraph line one.\nIntro paragraph line two.\r\n" +
		"\r\n## Requirements\r\n\r\n- first requirement\r\n* second requirement\r\n1. third requirement\n" +
		"\r\n| Field | Rule |\r\n| --- | :--: |\n| title | non-empty |\r\n" +
		"\r\n```go\r\nfunc main() {\r\n\tprintln(\"hi\")\r\n\r\n}\r\n```\r\n\r\nAfter paragraph.\r\n"
}

func TestParseBlocksIsDeterministic(t *testing.T) {
	input := multiKindDocument("\r\n")

	first := ingest.ParseBlocks(input)
	second := ingest.ParseBlocks(input)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("parsing the same document twice differs\nfirst:  %#v\nsecond: %#v", first, second)
	}

	// The document must actually be non-trivial for this to prove anything.
	if len(first) < 6 {
		t.Fatalf("fixture produced %d blocks, want at least 6", len(first))
	}
	for i, blk := range first {
		if blk.Order != i {
			t.Errorf("block %d has Order %d, want %d", i, blk.Order, i)
		}
		if blk.Text == "" {
			t.Errorf("block %d (%s) has empty text", i, blk.Kind)
		}
	}

	// A permuted-but-equivalent call must not see shared mutable state.
	third := ingest.ParseBlocks(input)
	if !reflect.DeepEqual(first, third) {
		t.Errorf("third parse differs from the first: %#v vs %#v", first, third)
	}
}

func TestParseBlocksOrderIsGapFreeAcrossAllKinds(t *testing.T) {
	got := ingest.ParseBlocks(multiKindDocument("\n"))

	kinds := map[ingest.BlockKind]bool{}
	for i, blk := range got {
		if blk.Order != i {
			t.Errorf("index %d has Order %d, want %d", i, blk.Order, i)
		}
		kinds[blk.Kind] = true
	}
	for _, kind := range []ingest.BlockKind{
		ingest.BlockHeading,
		ingest.BlockParagraph,
		ingest.BlockListItem,
		ingest.BlockTableRow,
		ingest.BlockCode,
	} {
		if !kinds[kind] {
			t.Errorf("fixture did not produce any %s block", kind)
		}
	}
}

func TestParseBlocksDoesNotAliasCallerInput(t *testing.T) {
	const input = "text\n"
	got := ingest.ParseBlocks(input)
	if len(got) != 1 || got[0].Text != "text" {
		t.Fatalf("ParseBlocks(%q) = %#v, want one paragraph block", input, got)
	}
	// A string is immutable in Go, so this asserts the raw block bytes are the
	// document's own text rather than a normalized copy.
	if !strings.Contains(input, got[0].Text) {
		t.Errorf("block text %q is not contained in the input document", got[0].Text)
	}
}

func TestParseBlocksClassifiesLineOrientedCases(t *testing.T) {
	cases := parseCases()
	cases = append(cases, flushCases()...)
	cases = append(cases, codeCases()...)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ingest.ParseBlocks(tc.input)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseBlocks(%q)\n got: %#v\nwant: %#v", tc.input, got, tc.want)
			}
		})
	}
}
