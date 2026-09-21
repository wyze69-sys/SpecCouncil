# M2-1 Verification Report — Deterministic Evidence Ingestion

Status: **PASS / ACCEPTED** (2026-09-21) — all six slices plus the M2-1g repair
are verified against the working tree at `8c7165e`. Pushed to `origin/master` at
`a2ef8cc` on 2026-09-21.

- Verified at HEAD: `8c7165e` (implementation slices `3a8ca42`…`66267bd`, repair `8c7165e`)
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

## Live gate results (implementation run at `f22b2b1`, repair run at `8c7165e`)

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

Repair run at `8c7165e` (same commands, current tree):

```text
gofmt -l .                          -> clean (empty)
go vet ./...                        -> clean
go test ./internal/ingest -count=10 -> ok 0.656s
go test ./internal/api    -count=10 -> ok 3.421s
go test ./... -count=1              -> ALL PASS
  api 2.250s, domain 0.797s, evidence 0.983s, ingest 0.981s,
  review 0.969s, storage/sqlite 18.310s, worker 6.986s
go test -race (WSL, ingest+api)     -> ok / ok, exit 0
git status --short                  -> clean (empty)
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
- **DEFECT — FIXED at `8c7165e` (M2-1g D1):** `Freeze` errors other than the
  no-units sentinel — specifically a fenced code block whose interior is blank but
  not empty, e.g. `"```\n   \n```"` — mapped to HTTP 503, not 400. `ParseBlocks`
  now drops a code block whose text is blank under `strings.TrimSpace`, so a blank
  fence yields no block, the document falls back to `ErrNoEvidenceUnits`, and the
  response is 400 `bad_request` with no row written.
- **GAP — CLOSED at `8c7165e` (M2-1g D2):** the ingestion `SnapshotProvider` was
  proven only against the in-memory `fakeStore`. `internal/api/ingest_e2e_test.go`
  now drives HTTP submit into a real `*sqlite.Store` (temp DB + `Migrate`) and
  reads the persisted snapshot back through public store APIs.
- The ingestion `SnapshotProvider` is proven through `internal/api` handler
  tests, but is not yet wired into `cmd/` service composition (no HTTP server in
  `main.go` yet). This is service wiring, deferred.
- Native Windows `-race` was not run (`-race requires cgo`). **This item is
  closed by independent verifier runs**: `go test -race ./internal/ingest
  ./internal/api -count=1` in WSL Ubuntu 24.04 with go1.27.1 linux/amd64 and gcc
  13.3.0 → `ok internal/ingest` + `ok internal/api`, exit 0, at both tree
  `e605999` (pre-repair) and tree `8c7165e` (post-repair). The race gate is NOT
  claimed for `internal/worker` or `internal/storage/sqlite`, which M2-1 did not
  touch.

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

The repair was released as `docs/cline/m2/M2-1g-INGEST-REPAIR.md` (packet commit
`6236a31`) and executed by the coordinator as commit `8c7165e`:

- `internal/ingest/parser.go` — the code-block emission guard now tests
  `strings.TrimSpace(text) != ""`; emitted code text keeps its own bytes.
- `internal/ingest/version.go` — `SplitterVersion` bumped to `"2"` because the
  parser's output changed for the same input (`version_test.go`, and the
  `snapshot_test.go` updated with it).
- `internal/ingest/parser_test.go`, `internal/ingest/snapshot_test.go` — new
  cases for blank fences, byte-preserving interior whitespace, order gap-freeness,
  and the ingest-level sentinel. The obsolete "blank code text is a Freeze error"
  case was replaced by the sentinel expectation, because that input is no longer
  reachable through the parser (it remains covered by `internal/evidence`).
- `internal/api/ingest_e2e_test.go` — four end-to-end tests against a real
  `*sqlite.Store`: persisted frozen snapshot (3 units, `REQ-12` addressable,
  re-freeze hash matches), idempotent replay, blank fence → 400 with nothing
  persisted, blank fence beside real content → 201.

Verification at `8c7165e`: `gofmt -l .` empty; `go vet ./...` clean;
`go test ./internal/ingest -count=10` ok; `go test ./internal/api -count=10` ok;
`go test ./... -count=1` all packages ok; WSL `-race` ok for `ingest` and `api`;
working tree clean; changed paths exactly the six allowed files.

## Independent test round (2026-09-21)

Task doc: `docs/cline/m2/M2-1g-INDEPENDENT-TEST.md` (`28775bd`). An independent
tester returned **PASS** on all six steps at `28775bd`, including reproducing the
old failure on a scratch worktree of `e605999` (**observed 503** on the pre-repair
tree, so the test can detect the defect), blank-fence parser probes (0 blocks for
spaces/tab/mixed/CRLF/unterminated), inert-fence hash equality, HTTP cases against
the real store (blank fence → 400 with nothing persisted, blank fence + heading →
201, test doc → 201 / 11 units / hash `b52e67a76269…`), replay 201→200, full gates,
and the WSL race gate.

Coordinator spot-checks of that report (self-reports are not acceptance evidence):

- Tree verified clean at `28775bd`; no stray probe files, no leftover git
  worktrees; working tree still pristine after the test round.
- The reported edge case is real and was reproduced: a fence whose only content is
  U+200B (zero-width space) is treated as non-blank, because
  `strings.TrimSpace` does not strip U+200B. `BuildSnapshot("snap-zwsp",
  "```\n\u200b\n```")` succeeds with one `constraint` unit whose text is the
  three bytes `e2 80 8b`. A bare U+200B line also becomes a paragraph block.
  Status: **known behavior, deferred** — it is not an error-class defect (the
  request correctly answers 201), and a stricter blank test would change parser
  output again and force a splitter bump to `"3"`. Decide in M2-2, which defines
  what counts as citable evidence text.

## Conclusion

M2-1 is **verified and accepted** at `8c7165e`. The ingestion engine parses, IDs,
kind-maps, and freezes deterministically, preserves M1 hashing, closes the
nil-snapshot blocker end-to-end against real SQLite, and now answers malformed
client content with 400 instead of 503. Remaining non-blocking deferrals: the
splitter version is still not persisted, and the provider is not yet wired into a
`cmd/` HTTP binary. Next: M2-2 (finding/citation contract).

