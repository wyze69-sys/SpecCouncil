# M2-1b — Evidence Unit ID Assignment

## Mission

Add the second slice of evidence ingestion: take the ordered `[]ingest.Block`
produced by M2-1a and give each block a **stable, deterministic unit ID**. The
result is a new ordered list of `(id, block)` pairs.

This slice assigns IDs only. It does NOT map blocks to `evidence.UnitKind`
(M2-1c), does NOT build a snapshot (M2-1e), and does NOT touch HTTP, SQLite,
worker, or provider code.

## Why this slice exists

Findings cite evidence by unit ID. IDs must be stable and collision-free for the
same input so citations are reproducible. This slice defines how IDs are derived,
including preserving an author-supplied ID like `REQ-12`.

## Starting point

- Start from a clean tree at current `master` HEAD (M2-1a commit `3a8ca42` or
  later). Confirm `git status --short` is clean before editing.
- Module `github.com/wyze69-sys/SpecCouncil`, Go 1.27.1.

## Authority (read in this order)

1. `docs/CANONICAL-FLOW.md` — runtime authority.
2. `docs/cline/persistence/00-BUILD-MAP.md` — M2 plan; this is slice M2-1b.
3. `internal/ingest/parser.go` — the `Block`/`BlockKind`/`ParseBlocks` types this
   slice consumes (read; do not modify).
4. `internal/evidence/snapshot.go` — read for context only; `Unit.ID` rules there
   (non-empty, unique per snapshot) are what these IDs must satisfy later. Do NOT
   modify it in this slice.

Canonical flow wins on any conflict. Do not edit `docs/CANONICAL-FLOW.md`.

## Scope

Create only:

```text
internal/ingest/ids.go
internal/ingest/ids_test.go
```

Modify only:

```text
docs/ENGINE-CONTRACT.md   (append a short "evidence unit IDs (M2-1b)" note only)
```

Do NOT modify `parser.go`, `internal/evidence`, `internal/api`,
`internal/storage`, `internal/worker`, `internal/review`, `internal/provider`,
`internal/domain`, or `cmd/`.

Forbidden in this slice:

- mapping to `evidence.UnitKind` (M2-1c);
- a splitter version constant (M2-1d);
- calling `evidence.Freeze` or building a `Snapshot` (M2-1e);
- any HTTP wiring (M2-1f);
- importing `internal/evidence` or any other internal package (stdlib only);
- network, filesystem, time, or randomness.

## Contract

### Type

Define exactly:

```go
// IdentifiedBlock is one parsed block paired with its assigned stable unit ID.
type IdentifiedBlock struct {
    ID    string
    Block Block
}
```

### Function

```go
// AssignIDs assigns a stable, deterministic unit ID to each block, preserving
// input order. It is pure: identical input yields identical output, and it uses
// no clock, filesystem, network, or randomness.
func AssignIDs(blocks []Block) []IdentifiedBlock
```

### ID rules (deterministic)

For each block, in input order:

1. **Author ID preservation.** If the block's text begins with an explicit author
   ID token, use that token as the base ID. An author ID token is defined
   precisely as: at the very start of `Block.Text` (after no leading whitespace,
   since block text is already trimmed), a match of
   `^[A-Za-z][A-Za-z0-9]*-[0-9]+` — one ASCII letter, then zero or more ASCII
   letters/digits, then a hyphen, then one or more digits — that is immediately
   followed by end-of-text, a space, a tab, a colon, a period, or a `)`.
   Examples that match: `REQ-12`, `FR-034`, `NFR-6`, `AC-17`. Examples that do
   NOT match: `REQ12` (no hyphen), `-12` (no leading letter), `REQ-` (no digits),
   `REQ-12abc` (digits not followed by an allowed boundary — the char after the
   digits is a letter), `REQ-12-2` (the char after the digits is a hyphen, which
   is NOT an allowed boundary). The captured token (e.g. `REQ-12`) is the base ID
   exactly as written, preserving its original case.
   - Author-ID detection applies to every block kind EXCEPT `BlockCode`. Code
     block text is significant/verbatim and must never be scanned for an author
     ID; code blocks always use a generated base ID.
   - ID assignment never mutates `Block.Text`. The captured token is COPIED into
     the ID; the block's text is returned byte-for-byte unchanged (the author ID
     also remains at the start of the text).

2. **Generated base ID.** When no author ID applies, the base ID is
   `u` + the block's 0-based `Block.Order`, e.g. `u0`, `u1`, `u2`. (Use
   `Block.Order`, not the slice index, though for M2-1a output they are equal;
   using `Order` keeps the rule robust.)

3. **Uniqueness (collision suffix).** IDs must be unique across the returned
   slice. Track assigned IDs in order. If a base ID has already been used, append
   `-2`, `-3`, ... (the smallest integer ≥ 2 that is not yet taken) to the base
   ID until it is unique. The first occurrence of a base ID is used bare (no
   suffix). Collision resolution is deterministic and depends only on input
   order.
   - Example: two blocks both yielding base `REQ-12` become `REQ-12` then
     `REQ-12-2`. If a third block also yields base `REQ-12`, the suffix search
     skips any already-taken value: `REQ-12`, `REQ-12-2`, `REQ-12-3`.
   - Note: a block whose text literally starts `REQ-12-2` is NOT an author ID
     (the char after the digits is a hyphen, not an allowed boundary per rule 1),
     so it takes a generated `u<order>` base, not `REQ-12-2`. The collision
     suffix `-2`/`-3` is only ever produced by this rule, never read from input.

4. **Non-empty guarantee.** Every assigned ID is non-empty (guaranteed by the
   `u<order>` fallback). Empty input returns a non-nil empty slice
   `[]IdentifiedBlock{}`.

Order is preserved: `out[i].Block == in[i]` for all i.

## Tests and gate

Table-driven tests in `internal/ingest/ids_test.go` (package `ingest_test`)
covering at least:

- a paragraph starting `REQ-12 the system must ...` → ID `REQ-12`;
- author-ID boundary matches: `FR-034:`, `NFR-6 `, `AC-17)`, and `REQ-12` at
  end-of-text all extract the token;
- non-matches stay generated: `REQ12 ...`, `-12 ...`, `REQ- ...`,
  `REQ-12abc ...` → ID `u<order>`;
- a `BlockCode` whose text begins with `REQ-12` is NOT treated as author ID → it
  gets `u<order>`;
- blocks with no author ID get `u0`, `u1`, ... matching their `Order`;
- two blocks with the same author base `REQ-12` → `REQ-12`, `REQ-12-2`;
- three-way collision → base, `-2`, `-3`;
- an input block literally starting `REQ-12-2` is NOT an author ID (hyphen after
  digits is not a boundary) and takes a generated `u<order>` ID;
- smallest-free-integer suffix: three blocks yielding base `REQ-1` produce
  `REQ-1`, `REQ-1-2`, `REQ-1-3` (the search skips the taken `-2`);
- generated-ID collision is impossible for distinct Orders but assert `u0`.. are
  unique on a mixed document;
- order preservation: `out[i].Block` deep-equals `in[i]` for a multi-kind input;
- empty input → non-nil `[]IdentifiedBlock{}`;
- determinism: `AssignIDs` on the same non-trivial input twice is
  `reflect.DeepEqual`, and all returned IDs are unique.

Assert exact ID strings, not just uniqueness counts.

Run and pass:

```bash
gofmt -w internal/ingest/ids.go internal/ingest/ids_test.go
test -z "$(gofmt -l .)"
go test ./internal/ingest -count=1
go test ./internal/ingest -count=10
go test ./... -count=1
go vet ./...
git diff --check
git status --short
```

`-count=10` must pass every run. Race gate not required (pure, single-threaded);
if run, WSL Ubuntu only, reported by actual environment, never a blocked run as
PASS.

## Commit

Commit exactly (only after every gate passes):

```text
Add deterministic evidence unit ID assignment
```

Never push. Any failure means BLOCKED, no commit, do not begin M2-1c.

## Report

```text
M2-1b RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests (-count=1 and -count=10):
- ...
Full gate (test/vet/gofmt/diff/status):
- ...
Acceptance evidence:
- author ID preserved (REQ-12):
- boundary matches (colon/space/paren/EOT):
- non-matches -> generated:
- code block never author-ID:
- generated u<order> sequence:
- two-way collision REQ-12 / REQ-12-2:
- three-way collision base/-2/-3:
- literal -2 collision -> smallest free integer:
- order preservation (Block deep-equal):
- empty -> []IdentifiedBlock{}:
- determinism (DeepEqual + unique IDs):
- forbidden scope untouched:
Remaining limitations:
- ...
Git status:
<exact output>
```
