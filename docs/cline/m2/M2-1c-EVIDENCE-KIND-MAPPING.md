# M2-1c — Evidence Kind Mapping

## Mission

Third slice of evidence ingestion: map each parsed block's structural
`ingest.BlockKind` to a frozen-evidence `evidence.UnitKind`. The result pairs
every identified block with a valid `evidence.UnitKind`, preserving input order.

This slice maps kinds only. It does NOT build `evidence.Unit` values, does NOT
call `evidence.Freeze`, does NOT build a `Snapshot` (all M2-1e), does NOT add a
splitter version (M2-1d), and does NOT touch HTTP (M2-1f).

## Why this slice exists

`evidence.Freeze` rejects any unit whose `Kind` is not one of the six recognized
`UnitKind` values. Before ingestion can build units, every block must carry a
valid kind. This slice defines that mapping once, deterministically, so the same
document always produces the same kinds.

## Starting point

- Start from a clean tree at current `master` HEAD (M2-1b commit `4cfc40a` or
  later; the packet-wording fix `623a2e9` is docs-only and does not affect code).
  Confirm `git status --short` is clean before editing.
- Module `github.com/wyze69-sys/SpecCouncil`, Go 1.27.1.

## Authority (read in this order)

1. `docs/CANONICAL-FLOW.md` — runtime authority.
2. `docs/cline/persistence/00-BUILD-MAP.md` — M2 plan; this is slice M2-1c.
3. `internal/ingest/parser.go` — the `Block`/`BlockKind` types and the five
   `BlockKind` constants (read; do not modify).
4. `internal/ingest/ids.go` — the `IdentifiedBlock` type this slice consumes
   (read; do not modify).
5. `internal/evidence/snapshot.go` — the `UnitKind` type, its six constants, and
   `IsValidUnitKind`. Read only; do NOT modify it in this slice.

Canonical flow wins on any conflict. Do not edit `docs/CANONICAL-FLOW.md`.

## Fixed facts from the code (do not re-derive)

The five `BlockKind` constants in `parser.go`:

```text
BlockHeading   = "heading"
BlockParagraph = "paragraph"
BlockListItem  = "list_item"
BlockTableRow  = "table_row"
BlockCode      = "code"
```

The six `UnitKind` constants in `snapshot.go`:

```text
UnitBrief       = "brief"
UnitRequirement = "requirement"
UnitComponent   = "component"
UnitFlow        = "flow"
UnitConstraint  = "constraint"
UnitDataRule    = "data_rule"
```

## Scope

Create only:

```text
internal/ingest/kinds.go
internal/ingest/kinds_test.go
```

Modify only:

```text
docs/ENGINE-CONTRACT.md   (append a short "evidence kind mapping (M2-1c)" note only)
```

Do NOT modify `parser.go`, `ids.go`, `internal/evidence`, `internal/api`,
`internal/storage`, `internal/worker`, `internal/review`, `internal/provider`,
`internal/domain`, or `cmd/`.

Forbidden in this slice:

- building `evidence.Unit` values (M2-1e);
- calling `evidence.Freeze` or building a `Snapshot` (M2-1e);
- a splitter version constant (M2-1d);
- any HTTP wiring (M2-1f);
- inspecting `Block.Text` to infer a kind — the mapping keys ONLY on
  `Block.Kind`. No text parsing, regex, keyword matching, or author-ID reading;
- network, filesystem, time, or randomness.

This slice MAY import `internal/evidence` (only for the `UnitKind` type and its
constants). It MUST NOT import `internal/api`, `internal/storage`,
`internal/worker`, `internal/review`, or `internal/provider`.

## Contract

### Type

Define exactly:

```go
// KindedBlock is one identified block paired with its mapped evidence kind.
type KindedBlock struct {
    ID    string
    Kind  evidence.UnitKind
    Block Block
}
```

### Functions

```go
// MapBlockKind maps a structural BlockKind to its evidence UnitKind.
// It keys only on the block kind and is a pure total function.
// For the five known BlockKind values it returns a valid UnitKind (a kind for
// which evidence.IsValidUnitKind is true). For any other value it returns the
// empty UnitKind "" (which IsValidUnitKind rejects); ParseBlocks never emits
// such a value, so this default is defensive only.
func MapBlockKind(k BlockKind) evidence.UnitKind

// AssignKinds maps every identified block to its evidence kind, preserving
// input order. It is pure: identical input yields identical output.
func AssignKinds(blocks []IdentifiedBlock) []KindedBlock
```

### Mapping table (fixed, deterministic)

Key on `Block.Kind` ONLY. This exact table:

```text
BlockHeading   -> UnitBrief
BlockParagraph -> UnitRequirement
BlockListItem  -> UnitRequirement
BlockTableRow  -> UnitDataRule
BlockCode      -> UnitConstraint
(any other)    -> ""   (empty UnitKind; invalid; unreachable from ParseBlocks)
```

Rationale (informational, not a rule to re-derive): headings frame sections
(brief); prose and list statements carry design requirements; table rows express
structured data (data_rule); fenced code expresses hard constraints. The mapping
need not be injective — two block kinds map to `UnitRequirement`, and that is
correct.

`UnitComponent` and `UnitFlow` are intentionally NOT produced by this structural
mapping. Distinguishing a component or flow needs semantic classification, which
is out of scope for v1 ingestion and belongs to later work or the provider.
Every kind this slice DOES produce is a valid `UnitKind`. Do not add heuristics
to produce `UnitComponent`/`UnitFlow` in this slice.

### Behavior rules

1. `AssignKinds` preserves order: `out[i].ID == in[i].ID` and
   `out[i].Block == in[i].Block` for all i, with
   `out[i].Kind == MapBlockKind(in[i].Block.Kind)`.
2. It never mutates input: `ID` and `Block` are copied through unchanged.
3. Empty input returns a non-nil empty slice `[]KindedBlock{}` (nil must fail).
4. Pure: no clock, filesystem, network, or randomness.

## Tests and gate

Table-driven tests in `internal/ingest/kinds_test.go` (package `ingest_test`)
covering at least:

- each of the five known `BlockKind` values maps to its exact expected
  `UnitKind`: heading→brief, paragraph→requirement, list_item→requirement,
  table_row→data_rule, code→constraint (assert the exact `UnitKind` string);
- every mapped output of the five known kinds satisfies
  `evidence.IsValidUnitKind`;
- an unrecognized `BlockKind("bogus")` maps to `""` and
  `evidence.IsValidUnitKind("")` is false;
- `AssignKinds` over a mixed 5-block input (one of each kind, Orders 0..4)
  returns kinds `[brief, requirement, requirement, data_rule, constraint]` in
  order, with each `ID` and `Block` carried through unchanged
  (`reflect.DeepEqual` the whole `[]KindedBlock` against an exact expected slice);
- ID/Block pass-through: build `IdentifiedBlock`s with distinct IDs (e.g.
  `REQ-12`, `u1`) and assert `out[i].ID`/`out[i].Block` equal the inputs;
- empty input (`nil` and `[]IdentifiedBlock{}`) → non-nil length-0 `[]KindedBlock{}`;
- determinism: `AssignKinds` on the same non-trivial input twice is
  `reflect.DeepEqual`.

Assert exact `UnitKind` strings, not just validity.

Run and pass:

```bash
gofmt -w internal/ingest/kinds.go internal/ingest/kinds_test.go
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
Add deterministic evidence kind mapping
```

Never push. Any failure means BLOCKED, no commit, do not begin M2-1d.

## Report

```text
M2-1c RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests (-count=1 and -count=10):
- ...
Full gate (test/vet/gofmt/diff/status):
- ...
Acceptance evidence:
- five known kinds map exactly (heading/paragraph/list_item/table_row/code):
- all five mapped outputs pass IsValidUnitKind:
- unknown BlockKind -> "" (invalid):
- mixed 5-block AssignKinds order + kinds:
- ID/Block pass-through unchanged:
- empty -> []KindedBlock{}:
- determinism (DeepEqual):
- forbidden scope untouched (no Unit/Freeze/Snapshot, no splitter version, no HTTP, no Text inspection):
- imports (evidence only, no api/storage/worker/review/provider):
Remaining limitations:
- ...
Git status:
<exact output>
```
