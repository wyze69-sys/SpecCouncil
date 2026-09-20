# M2-1a — Deterministic Evidence Block Parser

## Mission

Add the first slice of deterministic evidence ingestion: a pure, deterministic
parser that splits a submitted design document into an ordered list of raw
structural blocks. This slice produces **blocks only**. It does not assign unit
IDs, does not map to `evidence.UnitKind`, does not build an `evidence.Snapshot`,
and does not touch HTTP, SQLite, worker, or provider code.

This is the foundation for M2-1b (ID handling), M2-1c (kind mapping), M2-1d
(determinism + splitter version), M2-1e (freeze wiring), and M2-1f (HTTP hook).
Keep this slice small and correct; later slices build on its output type.

## Why this slice exists

HTTP submission currently cannot build the frozen `evidence.Snapshot` that
`sqlite.Submit` requires, so `SnapshotProvider` is nil and submit fails with
`ErrNilSnapshot`. Deterministic ingestion is the approved fix. Parsing raw
structure into ordered blocks is the first, independently testable step.

## Starting point

- Start from a clean tree at the current `master` HEAD (the build-map rewrite
  commit `1558353` or later). Confirm `git status --short` is clean before
  editing.
- Module: `github.com/wyze69-sys/SpecCouncil`, Go 1.27.1.

## Authority (read in this order)

1. `docs/CANONICAL-FLOW.md` — runtime authority.
2. `docs/cline/persistence/00-BUILD-MAP.md` — M2 plan; this packet is slice M2-1a.
3. `internal/evidence/snapshot.go` — the existing `Unit`/`Snapshot`/`Freeze`
   types this ingestion pipeline will eventually feed (read for context only; do
   NOT modify it in this slice).
4. `docs/ENGINE-CONTRACT.md` — implementation facts and gaps.

If code or old tests conflict with the canonical flow, the canonical flow wins.
Do not edit `docs/CANONICAL-FLOW.md`.

## Scope

Create only:

```text
internal/ingest/parser.go
internal/ingest/parser_test.go
```

You may modify only:

```text
docs/ENGINE-CONTRACT.md   (add a short "evidence ingestion (M2-1a)" note only)
```

Create the new `internal/ingest` package. Do NOT modify `internal/evidence`,
`internal/api`, `internal/storage`, `internal/worker`, `internal/review`,
`internal/provider`, `internal/domain`, or `cmd/`.

Forbidden in this slice:

- assigning unit IDs (M2-1b);
- mapping blocks to `evidence.UnitKind` (M2-1c);
- a splitter version constant (M2-1d);
- calling `evidence.Freeze` or constructing a `Snapshot` (M2-1e);
- any HTTP `SnapshotProvider` wiring (M2-1f);
- any dependency on `internal/evidence` or any other internal package;
- regexp-based Markdown parsing that is order- or map-dependent;
- network, filesystem, time, or randomness of any kind.

## Contract

### Block type

Define exactly:

```go
// BlockKind classifies one raw structural block of a submitted design.
type BlockKind string

const (
    BlockHeading   BlockKind = "heading"
    BlockParagraph BlockKind = "paragraph"
    BlockListItem  BlockKind = "list_item"
    BlockTableRow  BlockKind = "table_row"
    BlockCode      BlockKind = "code"
)

// Block is one ordered structural piece of a submitted design document.
// It carries no ID and no evidence kind; those are assigned by later
// ingestion slices.
type Block struct {
    Kind  BlockKind
    Text  string // normalized block text (see rules)
    Order int    // 0-based position in document order
}
```

### Parse function

Provide exactly one exported entry point:

```go
// ParseBlocks splits a submitted design document into ordered structural
// blocks. It is pure and deterministic: identical input always yields an
// identical slice, independent of environment, and it never consults the
// clock, filesystem, network, or any random source.
func ParseBlocks(content string) []Block
```

### Parsing rules (deterministic, line-oriented)

Process `content` as UTF-8 text, splitting on line boundaries. Normalize line
endings for classification by treating `\r\n` and `\r` as `\n`. Classify in a
single forward pass over the lines. The classification precedence, checked in
this exact order, is:

1. **Fenced code block.** A line whose trimmed text starts with ` ``` `
   (three backticks) opens a fenced code block; the next line starting with a
   matching ` ``` ` fence closes it. Every line strictly between the fences is
   part of one `BlockCode` block. The fence lines themselves are not included in
   the block text. The code block text is the inner lines joined by `\n`, with
   NO trimming of inner whitespace (code is significant). An unterminated fence
   (EOF before a closing fence) still produces one `BlockCode` block from the
   opening fence to EOF.

2. **Heading.** Outside a code block, a line whose trimmed text starts with one
   to six `#` characters followed by at least one space is a `BlockHeading`. The
   block text is the heading text after the `#` run and its following spaces,
   trimmed of surrounding whitespace. One heading line = one block.

3. **Table row.** Outside a code block, a line whose trimmed text starts with
   `|` and contains at least one more `|` is a `BlockTableRow`. The block text is
   the trimmed line, verbatim (pipes preserved). A GitHub-style separator row
   (cells containing only `-`, `:`, and spaces, e.g. `| --- | :--: |`) is STILL a
   `BlockTableRow` in this slice; later slices decide semantics. One row = one
   block.

4. **List item.** Outside a code block, a line whose trimmed text starts with
   `- `, `* `, `+ ` (unordered) or an ordered marker matching digits followed by
   `.` or `)` and a space (e.g. `1. `, `2) `) is a `BlockListItem`. The block
   text is the content after the marker, trimmed. One list line = one block
   (no nested-list flattening in this slice).

5. **Paragraph.** Any other non-blank line contributes to the current
   `BlockParagraph`. Consecutive non-blank, otherwise-unclassified lines are
   joined into one paragraph block with single `\n` separators. Leading and
   trailing whitespace of the joined paragraph text is trimmed; interior single
   `\n` line joins are preserved.

### Paragraph flushing (mandatory)

A pending paragraph is flushed (emitted as one block, if it has non-empty text)
BEFORE any of these events, with no blank line required between them:

- a blank line;
- a heading line (rule 2);
- a table row (rule 3);
- a list item (rule 4);
- an opening code fence (rule 1);
- end of input.

Concretely, this input produces a paragraph block THEN a heading block, in that
order, even though no blank line separates them:

```text
some text
# Heading
```

A structural line never merges into a paragraph, and paragraph text never
absorbs a following structural line.

### Whitespace, edge, and boundary rules (pin these exactly)

- "Trimmed" and "blank" always mean Go `strings.TrimSpace` semantics (spaces,
  tabs, and other Unicode whitespace), applied consistently for both
  classification and emitted text.
- Heading: a run of exactly 1–6 leading `#` followed by at least one space is a
  heading. A run of 7 or more `#` is NOT a heading; treat the line as paragraph
  text. `#` with no following space is NOT a heading (paragraph text).
- Code fence: an opening fence is a trimmed line whose first three characters are
  ` ``` `. Any trailing text on the opening fence line (an info string such as
  ` ```go `) is discarded and is not part of the block text. A closing fence is
  the next trimmed line whose first three characters are ` ``` `. Fences longer
  than three backticks are treated the same as a three-backtick fence in this
  slice (first-three-chars test); nested fences are not supported.
- Blank lines never produce a block. `Order` is a strict gap-free 0-based counter
  in document order across all emitted blocks of every kind.
- Empty input (`""`) or input that is entirely blank/whitespace returns a
  non-nil empty slice `[]Block{}`.
- Every emitted block must have non-empty `Text` after its normalization rule. If
  a rule would yield empty text (a heading `###` with no text, a list marker with
  no content, a fenced code block whose interior is empty), emit nothing for that
  construct rather than an empty block.

## Tests and gate

Write table-driven tests in `internal/ingest/parser_test.go` (package
`ingest_test`) covering at least:

- headings level 1 through 6, and `#` without a space is NOT a heading (it is
  paragraph text);
- a paragraph spanning multiple lines joined with `\n`, terminated by a blank
  line;
- two paragraphs separated by a blank line produce two blocks with correct
  `Order`;
- unordered list markers `-`, `*`, `+` and ordered markers `1.`, `2)`;
- a table with a header row, a `| --- |` separator row, and a data row = three
  `BlockTableRow` blocks in order;
- a paragraph immediately followed (no blank line) by a heading, table row, list
  item, and code fence each flushes the paragraph first, then emits the
  structural block (paragraph-flush guard);
- a `#######` (seven-hash) line and a `#`-with-no-space line are paragraph text,
  not headings;
- an opening fence with an info string (` ```go `) discards the info string and
  is not part of the code block text;
- a fenced code block preserves interior whitespace and blank lines and excludes
  the fences;
- an unterminated fenced code block runs to EOF as one `BlockCode`;
- `#` / list markers / pipes appearing INSIDE a fenced code block are code, not
  headings/lists/tables;
- CRLF and lone-CR input classifies identically to `\n` input (byte-equal block
  slices);
- empty and blank-only input returns `[]Block{}` (non-nil, length 0);
- a line that is only a marker with no content emits no block;
- **determinism:** parsing the same non-trivial multi-kind document twice yields
  a `reflect.DeepEqual` slice, and `Order` is a gap-free 0..n-1 sequence.

Assert exact expected `Block` values (Kind, Text, Order), not just counts.

Run and pass the full gate:

```bash
gofmt -w internal/ingest/parser.go internal/ingest/parser_test.go
test -z "$(gofmt -l .)"
go test ./internal/ingest -count=1
go test ./internal/ingest -count=10
go test ./... -count=1
go vet ./...
git diff --check
git status --short
```

`go test ./internal/ingest -count=10` must pass every run (determinism guard).
Native Windows `-race` is CGO-blocked; this slice is pure and single-threaded, so
a race gate is not required, but if run it must be in WSL Ubuntu and reported by
actual environment. Do not report a blocked race gate as PASS.

## Commit

Commit exactly (only after every gate passes):

```text
Add deterministic evidence block parser
```

Never push. Any failure means BLOCKED, no commit, do not begin M2-1b.

## Report

```text
M2-1a RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests (-count=1 and -count=10):
- ...
Full gate (test/vet/gofmt/diff/status):
- ...
Acceptance evidence:
- headings 1-6 + non-heading '#':
- multi-line paragraph + blank-line split:
- list markers (unordered + ordered):
- table header/separator/data rows:
- fenced code preserves interior + excludes fences:
- unterminated fence to EOF:
- markers inside code are code:
- CRLF/CR == LF byte-equal:
- empty/blank -> []Block{}:
- marker-only line emits nothing:
- determinism (DeepEqual + gap-free Order):
- forbidden scope untouched (no evidence/api/sqlite/worker/provider edits):
Remaining limitations:
- ...
Git status:
<exact output>
```
