# M2-1 Verification Report — Deterministic Evidence Ingestion

Status: **PASS**. All six slices complete, independently verified against the
working tree on this machine. Nothing pushed.

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
| 4 | Splitter version recorded | `internal/ingest/version.go` | `TestSplitterVersionIsExactLiteral`, `TestSplitterReturnsSplitterVersion`, `TestSplitterVersionIsNonEmpty` |
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
- `Freeze` errors other than the no-units sentinel (e.g. a whitespace-only code
  unit) still map to HTTP 503, not 400. Only the empty-content case is a 400.
- The ingestion `SnapshotProvider` is proven through `internal/api` handler
  tests, but is not yet wired into `cmd/` service composition (no HTTP server in
  `main.go` yet). This is service wiring, deferred.
- Native Windows `-race` was not run (`-race requires cgo`); the WSL Ubuntu race
  environment was not exercised in this run. These slices add no concurrency, so
  no race result is claimed as PASS.

## Conclusion

M2-1 meets every stated requirement (1–7) with live, passing test evidence on the
current tree, and preserves all M1 guarantees. The HTTP nil-snapshot blocker is
closed. Ready to proceed to M2-2 (finding/citation contract).
