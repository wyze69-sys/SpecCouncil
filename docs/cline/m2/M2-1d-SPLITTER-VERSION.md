# M2-1d — Splitter Version Constant

## Mission

Fourth slice of evidence ingestion: define a single, stable **splitter version**
constant that identifies the ingestion algorithm (the M2-1a parser + M2-1b IDs +
M2-1c kinds) that produced a set of evidence units. Expose it through one
accessor so later slices can record it alongside a snapshot.

This slice defines the version value and its accessor ONLY. It does NOT build
`evidence.Unit` values, does NOT call `evidence.Freeze`, does NOT build a
`Snapshot` (all M2-1e), and does NOT touch HTTP (M2-1f). It does NOT add the
version to the snapshot hash.

## Why this slice exists

Ingestion is deterministic, but the algorithm can change over time. If we ever
change how blocks are parsed, IDed, or kind-mapped, evidence produced by the new
splitter is not comparable to evidence produced by the old one. A recorded
version string makes the producing algorithm identifiable after the fact. This
slice only defines the value; recording it on a snapshot is M2-1e/M2-1f.

## Design decision (fixed — do not re-derive)

- The splitter version is a **string constant**, value exactly `"1"`.
- It is a plain monotonic identifier, NOT semver, NOT a date, NOT a git hash
  (those would be non-deterministic or over-specified). It is bumped by a human
  editing this constant only when the ingestion algorithm's output changes.
- It is NOT part of the snapshot hash. `evidence.Freeze` hashing stays exactly as
  today (units only). The version is metadata recorded beside the snapshot in a
  later slice, never mixed into the content hash.

## Starting point

- Start from a clean tree at current `master` HEAD (M2-1c commit `470759d` or
  later). Confirm `git status --short` is clean before editing.
- Module `github.com/wyze69-sys/SpecCouncil`, Go 1.27.1.

## Authority (read in this order)

1. `docs/CANONICAL-FLOW.md` — runtime authority.
2. `docs/cline/persistence/00-BUILD-MAP.md` — M2 plan; line ~174 states "Record a
   splitter version so future ingestion changes are identifiable." This is slice
   M2-1d.
3. `internal/ingest/parser.go`, `ids.go`, `kinds.go` — the ingestion the version
   identifies (read for context; do not modify).

Canonical flow wins on any conflict. Do not edit `docs/CANONICAL-FLOW.md`.

## Scope

Create only:

```text
internal/ingest/version.go
internal/ingest/version_test.go
```

Modify only:

```text
docs/ENGINE-CONTRACT.md   (append a short "splitter version (M2-1d)" note only)
```

Do NOT modify `parser.go`, `ids.go`, `kinds.go`, `internal/evidence`,
`internal/api`, `internal/storage`, `internal/worker`, `internal/review`,
`internal/provider`, `internal/domain`, or `cmd/`.

Forbidden in this slice:

- building `evidence.Unit` values (M2-1e);
- calling `evidence.Freeze` or building a `Snapshot` (M2-1e);
- adding the version into the snapshot hash (never — it is metadata);
- any HTTP wiring (M2-1f);
- importing `internal/evidence` or any other internal package (stdlib only; this
  slice needs no imports at all);
- network, filesystem, time, or randomness.

## Contract

Define exactly:

```go
// SplitterVersion identifies the evidence ingestion algorithm (block parsing,
// ID assignment, and kind mapping) that produced a set of evidence units.
//
// It is a plain monotonic identifier, not semver or a hash. Bump it by editing
// this constant only when the ingestion algorithm's output changes for the same
// input. It is metadata recorded beside a snapshot; it is NOT part of the
// snapshot content hash.
const SplitterVersion = "1"

// Splitter returns the current splitter version. Prefer this accessor at call
// sites so the constant has a single named entry point.
func Splitter() string
```

### Behavior rules

1. `SplitterVersion` is the untyped string constant `"1"`.
2. `Splitter()` returns exactly `SplitterVersion`; it is pure and takes no args.
3. The value is non-empty (a recorded version must never be blank).
4. No clock, filesystem, network, or randomness. No imports.

## Tests and gate

Tests in `internal/ingest/version_test.go` (package `ingest_test`) covering:

- `SplitterVersion == "1"` (assert the exact literal);
- `Splitter() == SplitterVersion` and `Splitter() == "1"`;
- `SplitterVersion != ""` (non-empty guarantee);
- determinism: two calls to `Splitter()` return the same value.

Run and pass:

```bash
gofmt -w internal/ingest/version.go internal/ingest/version_test.go
test -z "$(gofmt -l .)"
go test ./internal/ingest -count=1
go test ./internal/ingest -count=10
go test ./... -count=1
go vet ./...
git diff --check
git status --short
```

`-count=10` must pass every run. Race gate not required (pure, no state); if run,
WSL Ubuntu only, reported by actual environment, never a blocked run as PASS.

## Commit

Commit exactly (only after every gate passes):

```text
Add evidence splitter version constant
```

Never push. Any failure means BLOCKED, no commit, do not begin M2-1e.

## Report

```text
M2-1d RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests (-count=1 and -count=10):
- ...
Full gate (test/vet/gofmt/diff/status):
- ...
Acceptance evidence:
- SplitterVersion == "1":
- Splitter() == SplitterVersion == "1":
- non-empty guarantee:
- determinism (two calls equal):
- forbidden scope untouched (no Unit/Freeze/Snapshot, no hash change, no HTTP, no imports):
Remaining limitations:
- ...
Git status:
<exact output>
```
