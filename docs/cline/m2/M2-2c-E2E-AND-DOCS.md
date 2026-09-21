# M2-2c — End-to-end proof of the v2 finding contract + contract docs

Slice: M2-2c (integration proof + documentation)
Start commit: current `master` HEAD at release time (`56fb2e3` when written).
Depends on: M2-2a (`839a6be`), M2-2b (`aa73f9c`) — both verified.
Module: `github.com/wyze69-sys/SpecCouncil`, Go 1.27.1
Windows toolchain: `export PATH="/c/Users/User/sdk/go/bin:$PATH"` before every Go command.

## Mission

M2-2a added the `kind`/`anchor_ref` contract and validation; M2-2b persisted and
reconstructed them. Nothing yet proves the whole chain end to end, and the frozen
contract doc still describes the v1 rule. This slice does exactly two things:

1. Add ONE worker-level integration test that drives the real chain
   `fake provider → SuperviseSession → SQLite publish → ReadReportScoped` and
   asserts that a `missing` finding's `kind` and `anchor_ref` survive into the
   reconstructed terminal report, alongside an `existing` and a `conflicting`
   finding.
2. Update `docs/ENGINE-CONTRACT.md` so it states the v2 contract truthfully.

No production code changes. No new dependencies. This is the acceptance gate for
M2-2 as a whole.

## Verified reuse points (probed against the tree at draft time)

- `internal/worker/supervise_test.go` already runs the full chain in
  `TestSupervise_ClaimedReviewingSessionExecutesAndPublishesAtomically` (line 114):
  it builds a `fake.NewFakeProvider` per role, calls `worker.SuperviseSession`, then
  reads back with `store.ReadStatus` and `res.TerminalReport()`.
- `successScript(findings ...string) fake.ScriptedCall` (line 77) returns a default
  `existing` body citing `unit_1`, or wraps a caller-supplied JSON body when one is
  passed. Pass full JSON bodies to script the three kinds.
- `standardTestSnapshot(t, snapID)` (line 21) freezes two units: `unit_1`
  (requirement) and `unit_2` (component). Use `unit_1`/`unit_2` as refs and the
  anchor; do not invent unit IDs.
- `submitAndClaimSession(t, store, sessionID, projectID, policy)` returns
  `(claimRes, snap)` with the session already in reviewing state; reuse it verbatim.
- The config struct is the generic `worker.SuperviseSessionConfig[...]` shown at
  line 130; copy the existing instantiation and only change the provider scripts.

Do not build a new store double, a new provider, or a new config builder. Reuse
`standardPublishSuccessParamsBuilder` and `standardPublishFailureParamsBuilder`.

## The integration test (new, in `internal/worker/supervise_test.go`)

Name: `TestSupervise_V2FindingContractSurvivesToReport`.

Script the four roles through the fake provider so the published report contains
one of each kind plus a plain one, all citing only `unit_1`/`unit_2`:

- Requirements: one `existing` finding, `basis_refs:["unit_1"]`, no anchor.
- Architecture: one `conflicting` finding, `basis_refs:["unit_1","unit_2"]`, no anchor.
- QA: one `missing` finding, `basis_refs:[]`, `anchor_ref:"unit_2"`.
- Security: one `missing` finding, `basis_refs:["unit_1"]`, `anchor_ref:"unit_2"`
  (proves the 0..5 + mandatory-anchor rule with a supporting ref present).

Build each body inline (or via `successScript(body)`), for example the QA body:

```json
{"findings":[{"id":"Q-1","kind":"missing","severity":"medium","category":"coverage","issue":"No acceptance test for the cancel path.","recommendation":"Add an acceptance test that cancels an in-flight booking.","basis_refs":[],"anchor_ref":"unit_2"}]}
```

Assertions, all against the reconstructed report from a real store read — do **not**
assert on the in-memory outcome only:

1. `worker.SuperviseSession` returns a composed result; `store.ReadStatus` shows
   `SessionComplete` and `CompletedRoleCount == 4`.
2. Re-read the terminal report through the store, not just `res.TerminalReport()`:
   call `store.ReadReportScoped(ctx, projectID, sessionID)` and use that report for
   every field assertion, so the proof exercises the SQLite reconstruction path.
3. The report has exactly 4 findings.
4. Find each finding by its role and assert its `Kind` and `AnchorRef`:
   - requirements finding: `Kind == domain.FindingExisting`, `AnchorRef == ""`.
   - architecture finding: `Kind == domain.FindingConflicting`, `AnchorRef == ""`,
     `len(BasisRefs) == 2`.
   - QA finding: `Kind == domain.FindingMissing`, `AnchorRef == "unit_2"`,
     `len(BasisRefs) == 0`.
   - security finding: `Kind == domain.FindingMissing`, `AnchorRef == "unit_2"`,
     `BasisRefs == ["unit_1"]`.
5. Determinism: read the report a second time and assert
   `reflect.DeepEqual` of the two reads (the reconstruction is stable).

Confirmed against the tree: `func (s *Store) ReadReportScoped(ctx, projectID,
sessionID string) (*review.Report, error)` exists at `internal/storage/sqlite/read.go:158`.
The two existing supervise tests only assert on `res.TerminalReport()` (lines 180,
268) and never read the report back from the store, so this test is the first to
exercise the SQLite report reconstruction for the v2 fields — that is the whole
point, so the store readback is mandatory, not optional.

## `docs/ENGINE-CONTRACT.md` updates (documentation only)

Make the doc describe the v2 contract. The stale lines were located at draft time;
verify against the live file before editing and change only what is stale:

- The finding schema / bounds paragraph (line 81 today: "Enforced bounds: at most
  15 findings; 1–5 unique basis refs per finding; ...", and line 88 which warns that
  "requiring 1–5 references can pressure a provider to cite nearby text"). Replace
  the flat 1–5 rule with the kind-aware rule:
  - every finding has a `kind` of `existing`, `conflicting`, or `missing`;
  - `existing`: 1–5 basis refs; `conflicting`: 2–5 basis refs; `missing`: 0–5 basis
    refs and exactly one `anchor_ref`;
  - `anchor_ref` is allowed only for `missing` and names the unit the omission is
    about;
  - keep the existing sentence that citation validation proves a referenced unit
    exists, not that it supports the finding, and extend it to the anchor;
  - the line 88 caveat ("requiring 1–5 references can pressure a provider to cite
    nearby text") should now say the `missing` kind removes that pressure: an
    omission cites zero basis refs and instead names the section it is about via
    `anchor_ref`.
- The publish/validation description (around line 343, "1..5 basis refs per
  finding") gains the kind-aware bounds and the anchor rules, and notes the three
  new insert triggers from migration 007 (`findings_missing_requires_anchor`,
  `findings_anchor_only_for_missing`, `findings_anchor_snapshot_guard`).
- The persisted-schema section (around line 256) notes the two new `findings`
  columns: `kind` (CHECK in `existing`/`conflicting`/`missing`) and nullable
  `anchor_unit_id` (FK to `evidence_units`, `ON DELETE RESTRICT`).
- The deterministic finding order section (around line 121/288) notes that
  `primary basis_ref` falls back to the `anchor_ref` when a finding has no basis
  refs, so omission findings still sort deterministically.

Do not restate build/milestone status in this doc, and do not touch the public
README. Keep the wording literal: validation is structural, not semantic.

## Gates

```bash
export PATH="/c/Users/User/sdk/go/bin:$PATH"
cd /d/PROJECT/SpecCouncil
gofmt -l .                                  # empty
go vet ./...                                # clean
go test ./internal/worker -run V2FindingContract -v -count=1
go test ./internal/worker -count=3          # whole worker suite, no flakes
go test ./... -count=1                      # every package green
git add <only the allowed paths>
git diff --cached --name-only               # exactly the allowed list
git status --short                          # only those paths staged
```

Race: the verifier runs `go test -race ./internal/worker` in WSL Ubuntu
(`export PATH=$HOME/sdk/go/bin:$PATH`; gcc 13.3 present). Report `UNKNOWN/BLOCKED`
only with the exact command and error you actually saw — do not assume it is
blocked.

## Allowed paths

- `internal/worker/supervise_test.go` (the one new test only; do not alter existing tests)
- `docs/ENGINE-CONTRACT.md`

## Forbidden

Every non-test `.go` file (this slice adds no production code), all of
`internal/domain/**`, `internal/review/**`, `internal/storage/**`, `internal/api/**`,
`internal/ingest/**`, `internal/evidence/**`, `cmd/**`, migrations, `go.mod`,
`go.sum`, the README, and any doc other than `docs/ENGINE-CONTRACT.md`. No new
dependencies. No push. If the integration test cannot pass without a production
change, STOP and report the exact reason instead of editing production code — that
would mean M2-2a/M2-2b left a real gap.

## Commit

One commit, message exactly:

```
Prove v2 finding contract end to end and update engine contract
```

## Completion report format

```
SLICE: M2-2c
START: <sha> / FINAL: <sha>
CHANGED: <git diff --cached --name-only>   (must be exactly the 2 allowed files)
TEST: <what the new test scripts per role, and every Kind/AnchorRef/BasisRefs assertion>
READBACK: <confirm assertions use ReadReportScoped from the store, not only res.TerminalReport()>
DOC: <the ENGINE-CONTRACT sections changed, with before/after of the bounds line>
GATES:
  gofmt -l .                       -> <empty|output>
  go vet ./...                     -> <clean|output>
  go test worker -run V2 -v        -> <PASS|FAIL>
  go test ./internal/worker -count=3 -> <ok|fail>
  go test ./... -count=1           -> <per-package>
RACE: UNKNOWN/BLOCKED | <output>
PUSHED: no
NOTES: <deviations, or NONE>
```
