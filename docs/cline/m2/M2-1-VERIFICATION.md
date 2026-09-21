# M2-1 Verification Report — Deterministic Evidence Ingestion

Status: **PASS for the six implementation slices; M2-1 NOT yet accepted as a
whole** — an independent audit found one client-input defect and one missing
end-to-end proof, both scoped to the M2-1g repair packet. Nothing pushed.

- Verified at HEAD: `f22b2b1`
- Module: `github.com/wyze69-sys/SpecCouncil`, Go 1.27.1
- Date of this run: 2026-09-21
- Method: live `go test` / `go vet` / `gofmt` on the current tree, not a report replay.

## What M2-1 had to deliver (requirements)

From the M2 spec and build map, deterministic evidence ingestion must:

1. Convert a submitted design into stable evidence units: headings, paragraphs,
   list items, tables, code blocks.
2. Preserve author IDs such as `REQ-12`.
3. Map each unit to a valid `evidence.UnitKind`.
4. Record a splitter version.
5. Produce byte-identical units and ordering for identical input (deterministic:
   no clock, randomness, or map-iteration order).
6. Feed units into the existing `evidence.Freeze` path without changing hashing.
7. Close the HTTP nil-snapshot blocker: HTTP submit must build a real frozen
   snapshot so `sqlite.Submit` no longer returns `ErrNilSnapshot`.

## Slices and commits

| Slice | Purpose | Commit |
|---|---|---|
| M2-1a | Block parser (heading/paragraph/list/table/code) | `3a8ca42` |
| M2-1b | Deterministic unit ID assignment (author + generated) | `4cfc40a` |
| M2-1c | Block kind → `evidence.UnitKind` mapping | `470759d` |
| M2-1d | Splitter version constant (`"1"`) | `8b0c5cf` |
| M2-1e | Build frozen snapshot from source (`BuildSnapshot`) | `71d78f2` |
| M2-1f | Wire ingestion snapshot into HTTP submit | `66267bd` |

## Requirement → evidence mapping

| # | Requirement | Where met | Verified by |
|---|---|---|---|
| 1 | Five block kinds parsed | `internal/ingest/parser.go` | `TestParseBlocksClassifiesLineOrientedCases`, `TestParseBlocksOrderIsGapFreeAcrossAllKinds` |
| 2 | Author IDs preserved (`REQ-12`) | `internal/ingest/ids.go` | `TestAssignIDsContractExamples`, `TestAssignIDsMixedDocumentUsesAuthorIdsAndGeneratedIds`; end-to-end `TestBuildSnapshotHappyPath`, `TestSnapshotProvider_BuildsValidSnapshot` (`HasRef("REQ-12")`) |
| 3 | Valid `UnitKind` per unit | `internal/ingest/kinds.go` | `TestMapBlockKindKnownKinds`, `TestMapBlockKindKnownKindsAreValid`, `TestMapBlockKindUnknownIsInvalid` |
| 4 | Splitter version defined (`"1"` at M2-1; `"2"` after M2-1g) | `internal/ingest/version.go` | `TestSplitterVersionIsExactLiteral`, `TestSplitterReturnsSplitterVersion`, `TestSplitterVersionIsNonEmpty`. **Not persisted** — see limitations. |
| 5 | Deterministic output | all slices | `TestParseBlocksIsDeterministic`, `TestAssignIDsIsDeterministic`, `TestAssignKindsIsDeterministic`, `TestBuildSnapshotDeterminism`, `TestSnapshotProvider_Determinism`; `-count=10` clean |
| 6 | Hashing unchanged | `internal/ingest/snapshot.go` | `TestBuildSnapshotMatchesDirectFreeze` (hash equals direct `evidence.Freeze`); `internal/evidence` unmodified |
| 7 | Nil-snapshot blocker closed | `internal/api/snapshot_provider.go` + one `server.go` branch | `TestSubmitHandler_ValidSubmitWithProvider_ClosesNilSnapshotBlocker`, `TestSubmitHandler_BlankContentWithProvider_Returns400_NoStoreCall` |

## Live gate results (this run, HEAD `f22b2b1`)

```text
go test ./... -count=1   -> ALL PASS
  api 1.461s, domain 0.615s, evidence 0.679s, ingest 0.681s,
  review 0.766s, storage/sqlite 14.882s, worker 5.372s
  (cmd/speccouncil, provider, provider/fake, migrations: no test files)
go test ./internal/ingest -count=10 -> ok 0.564s   (determinism holds)
go test ./internal/api    -count=10 -> ok 1.127s   (determinism holds)
go vet ./...             -> clean
gofmt -l .               -> clean (empty)
git status --short       -> clean (empty)
```

## Test inventory

- `internal/ingest`: 34 test functions across parser/ids/kinds/version/snapshot.
- `internal/api` (M2-1f): 7 test functions (4 provider + 3 handler, incl. the
  201-success blocker-closed case, the 400 no-evidence case, and the 503
  unexpected-error case).

## Guarantees preserved (not weakened)

- `internal/evidence` was not modified in any M2-1 slice; the snapshot content
  hash is byte-identical to before (proven by `TestBuildSnapshotMatchesDirectFreeze`).
- `sqlite.Submit`, `SubmitParams`, worker, review, and provider code were not
  modified.
- The only change to proven M1 code is one added import and one added
  `ErrNoEvidenceUnits → 400` branch in `internal/api/server.go`; all existing
  auth, routing, and error-mapping behavior is unchanged.

## Known limitations (honest, in-scope)

- The splitter version is defined and available (`ingest.Splitter()`), but not
  persisted: `sqlite.Submit` still writes `normalization_version = 1`, and the
  HTTP adapter drops the version for now. No schema change was made (deferred).
- **DEFECT (fixed by M2-1g D1):** `Freeze` errors other than the no-units sentinel
  — specifically a fenced code block whose interior is blank but not empty, e.g.
  `"```\n   \n```"` — map to HTTP 503, not 400. Only the empty-content case is a
  400.
- **GAP (closed by M2-1g D2):** the ingestion `SnapshotProvider` is proven through
  `internal/api` handler tests, but every one of those tests wires `NewServer` to
  the in-memory `fakeStore`. No test drives an HTTP submit into a real
  `*sqlite.Store`, so "the nil-snapshot blocker is closed" was proven at the
  provider/parameter level, not end-to-end against SQLite.
- The ingestion `SnapshotProvider` is proven through `internal/api` handler
  tests, but is not yet wired into `cmd/` service composition (no HTTP server in
  `main.go` yet). This is service wiring, deferred.
- Native Windows `-race` was not run (`-race requires cgo`). **This item is now
  closed by an independent verifier run**: `go test -race ./internal/ingest
  ./internal/api -count=1` in WSL Ubuntu 24.04 with go1.27.1 linux/amd64 and gcc
  13.3.0 at tree `e605999` → `ok internal/ingest 1.053s`, `ok internal/api
  1.067s`, both exit 0. The race gate is NOT claimed for `internal/worker` or
  `internal/storage/sqlite`, which M2-1 did not touch.

## Independent audit follow-up (2026-09-21)

An independent audit of this report returned **CONDITIONAL PASS / PARTIAL
ACCEPTANCE** with five gaps. Every claim was re-checked against the working tree
by the coordinator:

| Audit gap | Verdict | Evidence |
|---|---|---|
| GAP-2 whitespace-only code block → 503 | **CONFIRMED, worse than reported** | Live probe at `e605999`: `"```\n   \n```"` → 503; `"```\n\t\n```"` → 503; and a document that also contains a valid heading (`"```\n   \n```\n\n# Heading"`) → **503**, so valid content is rejected as a server outage. `"```\n```"` correctly → 400. |
| GAP-1 blocker proven only with `fakeStore` | **CONFIRMED** | `grep -rn 'sqlite\.' internal/api/*_test.go` shows `sqlite` used for types only; no test opens a real store. |
| GAP-3 splitter version not persisted | **CONFIRMED, but already disclosed** | Requirement row 4 said "recorded"; the limitations section already stated it is not persisted. Row 4 wording corrected above. |
| GAP-4 native `-race` not run | **CLOSED by verifier** | WSL Ubuntu 24.04 + go1.27.1 + gcc 13.3.0 race run above, both packages `ok`. |
| GAP-5 not wired into `cmd/` binary | **CONFIRMED and expected** | `cmd/speccouncil/main.go` is still the M1 CLI demo; no HTTP entry point exists yet by design. |

Repair scope released as `docs/cline/m2/M2-1g-INGEST-REPAIR.md` (packet commit
`6236a31`): D1 blank-code guard + splitter bump to `"2"`, D2 real-SQLite
end-to-end submit proof. **M2-1 is not accepted as complete until M2-1g passes
independent verification.**

## Conclusion

M2-1's ingestion engine parses, IDs, kind-maps, and freezes deterministically,
preserves M1 hashing, and closes the nil-snapshot blocker at the provider and
handler level. It is **not** accepted as a whole: one client-input defect (503
instead of 400) and one missing end-to-end proof (real `*sqlite.Store`) are
tracked by M2-1g. Do not start M2-2 (finding/citation contract) before M2-1g is
verified.

