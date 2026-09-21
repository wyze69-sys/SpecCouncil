# M2-1g — Ingestion repair: blank code blocks + real-SQLite submit proof

Slice: M2-1g (repair, single commit)
Start commit: `e605999` ("Add M2-1 verification report")
Module: `github.com/wyze69-sys/SpecCouncil`, Go 1.27.1
Windows toolchain: `export PATH="/c/Users/User/sdk/go/bin:$PATH"` before every Go command.

## Mission

The M2-1 audit found one real HTTP defect and one missing proof. Fix both in one
narrow slice, and bump the splitter version because the parser output changes:

- **D1** — a fenced code block whose interior is blank-but-not-empty (for example
  "```\n   \n```") currently produces an evidence unit with blank text, so
  `evidence.Freeze` rejects it and the HTTP handler answers **503 service
  unavailable**. A client-supplied document must never be reported as a server
  outage; this input must produce **400 bad_request**, and a document that also
  contains real content must still submit.
- **D2** — every M2-1f test wires `api.NewServer` to the in-memory `fakeStore`.
  Nothing proves that a real `*sqlite.Store` no longer returns `ErrNilSnapshot`
  through the HTTP submit path. Add that end-to-end proof using public store APIs
  only.
- **D3** — `internal/ingest/version.go` says the splitter version must be bumped
  "when the ingestion algorithm's output changes for the same input". D1 changes
  that output, so the constant becomes `"2"`.

Do not widen scope. Do not touch `internal/evidence`, `internal/storage/sqlite`,
`internal/api/server.go` or `internal/api/snapshot_provider.go`: the existing
`ingest.ErrNoEvidenceUnits -> 400` branch in `server.go` is already correct and
needs no change.

## Evidence for D1 (already reproduced at the start commit)

Probe: `api.Server` built with `NewIngestSnapshotProvider()` and `fakeStore`,
then `POST /v1/projects/proj-probe/reviews`:

| `content` | observed status |
|---|---|
| "```\n   \n```" | **503** `service_unavailable` |
| "```\n\t\n```" | **503** `service_unavailable` |
| "```\n   \n```\n\n# Heading" | **503** `service_unavailable` |
| "```\n```" | 400 `bad_request` (no block emitted, no evidence units) |

Cause: `ParseBlocks` (`internal/ingest/parser.go`) emits `BlockCode` with
`Text = "   "`; `evidence.Freeze` rejects it with `evidence: unit %q has empty
text`; that error is neither `ingest.ErrNoEvidenceUnits` nor a persistence error,
so `server.handlePersistenceError` maps it to 503.

## D1 — exact required rule

In the fenced-code branch of `ParseBlocks`, the emission guard changes from

```go
if text := strings.Join(inner, "\n"); text != "" {
```

to

```go
if text := strings.Join(inner, "\n"); strings.TrimSpace(text) != "" {
```

Nothing else in `parser.go` changes: fence detection, unterminated-fence
behavior, `Order` numbering, paragraph handling, and byte-for-byte preservation
of emitted code text all stay exactly as they are. This is the amendment of the
M2-1a rule "a fenced block emits nothing only when it is byte-empty"; the new
rule is "a fenced block emits nothing when its text is blank under
`strings.TrimSpace`", which matches the blank/trimmed definition the package doc
already states for every other block kind (paragraphs, headings, and list items
already emit nothing for blank text).

The dropped block consumes no `Order`, so downstream `AssignIDs` / `AssignKinds`
/ `BuildSnapshot` behavior is unchanged for every non-blank input.

### Required parser cases (exact expectations)

| input | want |
|---|---|
| `"```\n   \n```\n"` | `[]ingest.Block{}` |
| `"```\n\t \n```\n"` | `[]ingest.Block{}` |
| `"```\n\n```\n"` | `[]ingest.Block{}` (already true today) |
| `"```\n```\n"` | `[]ingest.Block{}` (already true today) |
| `"```\n   \ncode\n```\n"` | `[]ingest.Block{b(ingest.BlockCode, "   \ncode", 0)}` |
| `"```\ncode\n   \n```\n"` | `[]ingest.Block{b(ingest.BlockCode, "code\n   ", 0)}` |
| `"```\n   \n```\n# H\n"` | `[]ingest.Block{b(ingest.BlockHeading, "H", 0)}` |

Case 5 and 6 prove the guard trims only for the emptiness test: emitted code
text still keeps its own bytes. Case 7 proves the dropped block does not consume
an `Order`. Keep the existing "empty fenced code block emits nothing" and "code
block text is not trimmed so blank interior only blocks emit nothing" cases
passing — they must not be deleted or weakened.

### Required ingest-level case (`internal/ingest/snapshot_test.go`)

- `BuildSnapshot("snap-blank", "```\n   \n```")` returns the zero
  `IngestResult{}` and an error with `errors.Is(err, ErrNoEvidenceUnits) == true`.
- `BuildSnapshot("snap-mixed", "# Title\n\n```\n   \n```")` returns no error and
  a snapshot with exactly 1 unit: `{ID: "u0", Kind: evidence.UnitBrief, Text: "Title"}`.

## D3 — splitter version bump

`internal/ingest/version.go`: `const SplitterVersion = "2"`.
Update `TestSplitterVersionIsExactLiteral` in `internal/ingest/version_test.go` to
expect `"2"`. Before finishing, run `grep -rn '"1"' internal/ingest` and confirm
no other assertion still pins the old literal. Do not change the `Splitter()`
accessor or its doc comment beyond the literal.

## D2 — real-SQLite end-to-end proof (new file `internal/api/ingest_e2e_test.go`)

Package `api`. Reuse the existing `fakeAuthenticator` and `fakeAuthorizer` from
`server_test.go` — do not modify that file and do not create a substitute store.
Use only public store APIs (`sqlite.Open`, `(*Store).Migrate`,
`(*Store).ReadStatusScoped`, `(*Store).ReadSnapshot`) and the HTTP handler. No raw
SQL and no direct access to unexported store fields.

Shared setup helper in the new file (name it `newRealStoreServer(t)` or similar):

- `st, err := sqlite.Open(sqlite.Config{Path: filepath.Join(t.TempDir(), "api_e2e.db"), BusyTimeout: 500 * time.Millisecond})`
- `t.Cleanup(func() { _ = st.Close() })`
- `st.Migrate(context.Background())`
- `NewServer(Config{Store: st, Authenticator: &fakeAuthenticator{}, Authorizer: &fakeAuthorizer{}, SnapshotProvider: NewIngestSnapshotProvider()})`
- return both the server and the store handle.

### Test 1 — `TestIngestE2E_RealSQLiteSubmitPersistsFrozenSnapshot`

Request: `POST /v1/projects/proj-e2e/reviews`, headers
`Content-Type: application/json` and `Authorization: Bearer valid-token`, body

```json
{"idempotency_key":"e2e-key-1","title":"E2E Spec","content":"# System Spec\n\nREQ-12 The system shall process requests deterministically.\n\n```go\nfunc main() {}\n```"}
```

Assertions:

1. status `201`; the response decodes as `SubmitResponse` with
   `Status == domain.SessionQueued`, non-empty `SessionID`, `SnapshotID` prefixed
   `"snap-"`, non-empty `RequestHash`, and `Replay == false`.
2. `st.ReadStatusScoped(ctx, "proj-e2e", resp.SessionID)` returns no error with
   `Status == domain.SessionQueued`, `SnapshotID == resp.SnapshotID`, and a
   non-empty `SnapshotHash`.
3. `st.ReadSnapshot(ctx, resp.SnapshotID)` returns no error, the same ID,
   exactly 3 units, `HasRef("REQ-12") == true`, and
   `evidence.Freeze(s.ID, s.Units)` succeeds with a hash equal to `s.Hash`.
   (Expected unit IDs in order: `u0`, `REQ-12`, `u2`.)

### Test 2 — `TestIngestE2E_IdempotentReplayReturnsStoredSession`

Replay the exact same request twice inside this test, against the same server and
store (a fresh temp DB for this test, not Test 1's). Assert that the first call
returns `201` with `Replay == false`, that the second call returns `200` with
`Replay == true`, and that both responses carry the same `SessionID` and
`SnapshotID`; then confirm `ReadStatusScoped` returns that session.

### Test 3 — `TestIngestE2E_WhitespaceOnlyFenceIsClientError`

Body: `{"idempotency_key":"e2e-key-blank","title":"Blank","content":"```\n   \n```"}`.

Assert: status `400`; error envelope code `"bad_request"` and message
`"content produced no reviewable evidence"`; and
`st.ReadSnapshot(ctx, snapshotID("proj-blank", "Blank", "```\n   \n```"))` returns
a nil snapshot with a non-nil error whose message contains `"not found"` — nothing
was persisted. (`sqlite.ErrNotFound` is the *session* sentinel and is not what
`ReadSnapshot` returns for a missing snapshot, so do not assert on it.)

### Test 4 — `TestIngestE2E_WhitespaceFenceWithRealContentStillSubmits`

Body content: `"```\n   \n```\n\n# Heading"`. Assert status `201` with a non-empty
`SnapshotID`, and that `ReadSnapshot` for that ID returns exactly 1 unit.

## Allowed paths (the complete change set)

- `internal/ingest/parser.go` — the guard above, plus one added sentence in the
  `ParseBlocks` doc comment stating that a fenced code block whose text is blank
  under `strings.TrimSpace` emits no block, while an emitted code block still
  keeps its own bytes (no other prose or code change)
- `internal/ingest/parser_test.go` (new cases; existing cases preserved)
- `internal/ingest/snapshot_test.go` (the two ingest-level cases)
- `internal/ingest/version.go` (literal `"2"`)
- `internal/ingest/version_test.go` (literal `"2"`)
- `internal/api/ingest_e2e_test.go` (new file)

## Forbidden

`internal/evidence/**`, `internal/storage/sqlite/**` (production **and** test
files), `internal/api/server.go`, `internal/api/snapshot_provider.go`,
`internal/api/server_test.go`, `internal/api/snapshot_provider_test.go`,
`cmd/**`, `go.mod`, `go.sum`, `docs/**`, `.gitattributes`, and every path not
listed above. No new dependencies. No renames. No formatting-only sweeps of
unrelated files. No push.

## Gates (run all, paste real output)

```bash
export PATH="/c/Users/User/sdk/go/bin:$PATH"
cd /d/PROJECT/SpecCouncil
gofmt -l .                     # must be empty
go vet ./...                   # must be clean
go test ./internal/ingest -count=10
go test ./internal/api -count=10
go test ./... -count=1
git add <only the allowed paths>
git diff --cached --name-only  # must equal the allowed path list exactly
git status --short             # must list only those staged paths before the commit
```

If a gate fails, stop and report the failure. Never make a gate pass by deleting,
skipping, or weakening an existing assertion.

## Race gate

Native Windows `-race` is blocked on this machine (requires cgo, no gcc in the Go
toolchain). Do **not** claim a race pass. The verifier runs
`go test -race ./internal/ingest ./internal/api` in WSL Ubuntu and records the
result separately. Report the race gate as UNKNOWN/BLOCKED unless you actually ran
it in a cgo-capable environment and paste its output.

## Commit

One commit only, message exactly:

```
Repair ingest blank code blocks and prove real-SQLite submit
```

No amend, no extra commits, no push.

## Completion report format (exact)

```
SLICE: M2-1g
START: e605999
FINAL: <commit sha>
CHANGED: <git diff --name-only output, one path per line>
D1: <one line: what changed in parser.go>
D3: <splitter version now "2"; test updated>
D2: <names of the new e2e tests and what each asserted>
GATES:
  gofmt -l .            -> <empty|output>
  go vet ./...          -> <clean|output>
  go test ingest -count=10 -> <ok|fail>
  go test api -count=10    -> <ok|fail>
  go test ./... -count=1   -> <ok|fail, per-package>
  git diff --name-only HEAD -> <list>
  git status --short       -> <empty|list>
RACE: UNKNOWN/BLOCKED (native Windows -race unavailable) | <output if run in WSL>
PUSHED: no
NOTES: <only real deviations, or NONE>
```
