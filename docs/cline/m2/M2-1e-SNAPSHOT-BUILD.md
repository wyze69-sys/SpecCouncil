# M2-1e — Build Frozen Snapshot from Source

## Mission

Fifth ingestion slice: compose the whole ingestion pipeline into one function
that turns raw submitted design text into a frozen `evidence.Snapshot`, and
returns the splitter version beside it. This is the slice that actually calls
`evidence.Freeze`.

Pipeline: `source string` → `ParseBlocks` (M2-1a) → `AssignIDs` (M2-1b) →
`AssignKinds` (M2-1c) → build `[]evidence.Unit` → `evidence.Freeze` → return
snapshot + `SplitterVersion` (M2-1d).

This slice does NOT touch HTTP, SQLite, worker, provider, or `internal/api`
(that is M2-1f). It does NOT modify `internal/evidence`. It does NOT change the
snapshot hash — it only feeds units into the existing `Freeze`.

## Why this slice exists

The W7/HTTP submission path is blocked because it has no way to build the frozen
snapshot `sqlite.Submit` requires (the nil-snapshot blocker in
`docs/ENGINE-CONTRACT.md`). This slice builds that snapshot deterministically
from source text. M2-1f then wires it into the HTTP submit path.

## Starting point

- Start from a clean tree at current `master` HEAD (M2-1d commit `8b0c5cf` or
  later). Confirm `git status --short` is clean before editing.
- Module `github.com/wyze69-sys/SpecCouncil`, Go 1.27.1.

## Authority (read in this order)

1. `docs/CANONICAL-FLOW.md` — runtime authority.
2. `docs/cline/persistence/00-BUILD-MAP.md` — M2 plan; lines ~172-176 (byte-
   identical deterministic units; record splitter version; feed units into the
   existing `evidence.Freeze` path; do not change hashing). This is slice M2-1e.
3. `internal/ingest/parser.go`, `ids.go`, `kinds.go`, `version.go` — the pipeline
   stages this slice composes (read; do not modify).
4. `internal/evidence/snapshot.go` — the `Unit`, `UnitKind`, `Snapshot` types and
   `Freeze`. READ CAREFULLY. Do NOT modify it.

Canonical flow wins on any conflict. Do not edit `docs/CANONICAL-FLOW.md`.

## Fixed facts from the code (do not re-derive)

`evidence.Freeze(id string, units []Unit) (Snapshot, error)` validates, in this
order, and returns an error (empty `Snapshot`) on the first failure:

1. `id` must not be blank (`strings.TrimSpace(id) == ""` → error).
2. `len(units) == 0` → error `"evidence: snapshot needs at least one unit"`.
3. For each unit: non-blank `ID`, non-blank `Text`, `IsValidUnitKind(Kind)` true,
   and no duplicate `ID`.

`evidence.Unit` is `{ ID string; Kind UnitKind; Text string }`.

The M2-1a parser only emits blocks with non-empty `Text` for headings,
paragraphs, list items, and table rows; a fenced code block's `Text` is its
interior and could in principle be whitespace-only, which `Freeze` would reject
as empty text. This slice does NOT pre-filter or repair such units — it passes
them to `Freeze` and returns whatever error `Freeze` produces. (Repair/filtering
is not in scope; the parser rarely produces this and the behavior stays honest.)

## Scope

Create only:

```text
internal/ingest/snapshot.go
internal/ingest/snapshot_test.go
```

Modify only:

```text
docs/ENGINE-CONTRACT.md   (append a short "snapshot build (M2-1e)" note only)
```

Do NOT modify `parser.go`, `ids.go`, `kinds.go`, `version.go`,
`internal/evidence`, `internal/api`, `internal/storage`, `internal/worker`,
`internal/review`, `internal/provider`, `internal/domain`, or `cmd/`.

Forbidden in this slice:

- any HTTP wiring or `internal/api` change (M2-1f);
- modifying `internal/evidence` or the snapshot hash in any way;
- importing `internal/api`, `internal/storage`, `internal/worker`,
  `internal/review`, or `internal/provider`;
- network, filesystem, time, or randomness.

Allowed imports: `internal/evidence`, `errors`, and other stdlib as needed. No
other internal packages.

## Contract

### Types

```go
// IngestResult is a frozen evidence snapshot plus the splitter version that
// produced it. The version rides beside the snapshot because evidence.Snapshot
// is intentionally not modified by ingestion.
type IngestResult struct {
    Snapshot        evidence.Snapshot
    SplitterVersion string
}
```

### Sentinel error

```go
// ErrNoEvidenceUnits is returned when the source produces zero evidence units
// (empty or structure-only input). Callers (e.g. the HTTP submit path) map this
// to a client error rather than a server fault.
var ErrNoEvidenceUnits = errors.New("ingest: source produced no evidence units")
```

### Function

```go
// BuildSnapshot ingests raw design source into a frozen evidence snapshot.
//
// It runs the deterministic pipeline (ParseBlocks -> AssignIDs -> AssignKinds),
// builds one evidence.Unit per block (ID from AssignIDs, Kind from AssignKinds,
// Text from the block), and calls evidence.Freeze(id, units).
//
// It is pure and deterministic: identical (id, source) yields an identical
// IngestResult, using no clock, filesystem, network, or randomness.
//
// Errors:
//   - ErrNoEvidenceUnits if the source produces zero blocks (checked before
//     Freeze so the caller gets a stable sentinel, not Freeze's message).
//   - otherwise, any error from evidence.Freeze is returned unwrapped-in-meaning
//     (wrapped with %w so errors.Is still works) — e.g. blank id, empty unit
//     text, duplicate id.
//
// On any error the returned IngestResult is the zero value.
func BuildSnapshot(id string, source string) (IngestResult, error)
```

### Behavior rules (pin exactly)

1. Run `ParseBlocks(source)` → `AssignIDs(blocks)` → `AssignKinds(identified)`.
2. If the parsed block count is 0, return `IngestResult{}, ErrNoEvidenceUnits`
   BEFORE calling `Freeze`.
3. Build `units []evidence.Unit` in pipeline order: for each kinded block,
   `Unit{ID: kb.ID, Kind: kb.Kind, Text: kb.Block.Text}`. Order of `units`
   matches block order (Freeze hashes order-independently, but building in order
   keeps behavior obvious and deterministic).
4. Call `evidence.Freeze(id, units)`. On error return
   `IngestResult{}, fmt.Errorf("ingest: build snapshot: %w", err)`.
5. On success return `IngestResult{Snapshot: snap, SplitterVersion: Splitter()}`,
   `nil`.
6. Never mutate inputs; no shared mutable state.

## Tests and gate

Table-driven / focused tests in `internal/ingest/snapshot_test.go` (package
`ingest_test`) covering at least:

- **happy path**: a multi-kind source (heading + paragraph + list item + table
  row) with `id = "snap-1"` returns no error; `IngestResult.SplitterVersion ==
  "1"`; `Snapshot.ID == "snap-1"`; `Snapshot.Hash` is non-empty; every parsed
  block's ID is addressable via `Snapshot.HasRef(id)`; `len(Snapshot.Units)`
  equals the parsed block count and each unit's `Kind` passes
  `evidence.IsValidUnitKind`.
- **author ID preserved end-to-end**: source `"REQ-12 the system must ..."`
  yields a unit whose ID is `REQ-12` and `Snapshot.HasRef("REQ-12")` is true.
- **units built from all kinds**: assert the unit for a table row has kind
  `data_rule` and for a code block has kind `constraint` (end-to-end through the
  real mapping).
- **empty/blank source → ErrNoEvidenceUnits**: `""`, `"   "`, `"\n\n"` each
  return `errors.Is(err, ErrNoEvidenceUnits)` and a zero `IngestResult`; assert
  `Freeze` is effectively not reached by also checking the error is the sentinel,
  not Freeze's "at least one unit" text.
- **blank snapshot id → error**: a valid source with `id = ""` (or `"  "`)
  returns a non-nil error and a zero `IngestResult`; the error is NOT
  `ErrNoEvidenceUnits`.
- **determinism**: `BuildSnapshot("snap-1", src)` called twice on a non-trivial
  source is `reflect.DeepEqual` on the result (including `Snapshot.Hash` and
  `SplitterVersion`).
- **hash stability against evidence**: the `Snapshot.Hash` from `BuildSnapshot`
  equals the hash from calling `evidence.Freeze("snap-1", units)` directly with
  the same units built by hand — proving this slice did not change hashing.
- **no forbidden imports**: (assert by inspection in the report, not a test).

Assert exact strings where the value carries the decision (`"1"`, `"snap-1"`,
`REQ-12`, `data_rule`, `constraint`).

Run and pass:

```bash
gofmt -w internal/ingest/snapshot.go internal/ingest/snapshot_test.go
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
Add deterministic snapshot build from source
```

Never push. Any failure means BLOCKED, no commit, do not begin M2-1f.

## Report

```text
M2-1e RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests (-count=1 and -count=10):
- ...
Full gate (test/vet/gofmt/diff/status):
- ...
Acceptance evidence:
- happy path (snapshot id/hash/refs/unit count/kinds valid):
- author ID preserved end-to-end (REQ-12 addressable):
- table row -> data_rule, code -> constraint end-to-end:
- empty/blank source -> ErrNoEvidenceUnits (sentinel, zero result):
- blank snapshot id -> error (not the sentinel):
- determinism (DeepEqual incl hash + version):
- hash matches direct evidence.Freeze on same units (hashing unchanged):
- SplitterVersion == "1" on success:
- forbidden scope untouched (no HTTP/api/storage/worker/review/provider, evidence unmodified):
- imports (evidence + stdlib only):
Remaining limitations:
- ...
Git status:
<exact output>
```
