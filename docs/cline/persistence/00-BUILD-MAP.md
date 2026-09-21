# SpecCouncil M2 Build Map for Cline

## Purpose

The persistence, worker, and HTTP-composition foundation is complete and
independently verified. This map now drives **M2**, the validation milestone
that decides whether SpecCouncil should grow beyond one-time proposal review.

M2 has five items. The first three are real product capabilities. The last two
are validation systems that test whether the product is worth expanding before
any M3 architecture is built.

Only one packet is executed per Cline invocation. Hermes independently verifies
each result before releasing the next packet. No M3 capability is implemented in
this milestone.

## Authority

Read in this order for every slice:

1. `docs/CANONICAL-FLOW.md` — runtime authority for implemented v1 behavior.
2. The current packet in this directory.
3. Existing Go code and tests.
4. `docs/ENGINE-CONTRACT.md` — implementation facts, gaps, and blockers.

If code or old tests conflict with the canonical flow, the canonical flow wins.
Workers may not edit it or invent a replacement rule. M2 documentation changes
must not silently alter proven v1 runtime behavior.

## Execution protocol

```text
UNDERSTAND
→ inspect clean Git state and named files
IMPLEMENT
→ change only the current slice
TEST
→ focused tests, then the full gate
FIX
→ repair every failure and rerun
VERIFY
→ inspect final diff against every acceptance item
COMMIT
→ only after all gates pass; never push
REPORT
→ real commands, results, files, limitations, clean status
STOP
```

One packet equals one invocation. A failed slice is repaired; the next slice
does not start.

## Global rules

- Repository: `D:\PROJECT\SpecCouncil`; Go 1.27.1; module
  `github.com/wyze69-sys/SpecCouncil`.
- Use `database/sql` with pure-Go `modernc.org/sqlite`; no CGO or ORM.
- Applied migrations are **never edited**. Corrections and new tables are new
  numbered migrations with recorded SHA-256 checksums; changed applied SQL fails
  closed.
- Preserve every proven guarantee unchanged: immutable snapshots and hashes,
  strict `basis_refs` validation, at most two provider calls per role, retry XOR
  format repair, cancellation/deadline behavior, at most two persisted roles in
  flight, guarded dispatch, compare-and-set publication, late-result protection,
  restart recovery, idempotent submission, deterministic composition,
  COMPLETE/PARTIAL/FAILED terminal semantics, read-only report behavior, process
  lock, and one-attempt worker supervisor behavior.
- Do not weaken database constraints or move authoritative concurrency guarantees
  from SQLite into application memory.
- Never persist credentials, secret-bearing prompts, or raw provider errors.
  Real provider work uses server-side credentials only; no secret ever enters
  source, packets, reports, prompts, logs, or chat.
- The fake provider is retained for all automated tests. A real provider is
  called only under the M2-3 safety envelope.
- Do not add remotes or push.

## Gate after every slice

```bash
test -z "$(gofmt -l .)"
go test ./... -count=1
go vet ./...
git diff --check
git status --short
```

Race-sensitive slices additionally run `go test -race ./...`. Native Windows
`-race` is blocked by CGO (`go: -race requires cgo; enable cgo by setting
CGO_ENABLED=1`); run it in WSL Ubuntu with Go+GCC and report the actual
environment. Never rewrite a blocked race gate as PASS.

## Verified foundation (do not reopen without a proven defect)

| Phase | Deliverable | Commit |
|---|---|---|
| P0 | Canonical domain alignment | `171b9d2` |
| P1A | SQLite store open + read-only pool | `c21c240` |
| P1B | Checksummed migration runner | `0975ad0` |
| P1C | Immediate transaction + bounded busy retry | `71da0fe` |
| P2 | Core schema | `9db3dab` |
| P3 | Transition/citation/immutability guards | `975c123` |
| P4 | Atomic submission + idempotency | `6906d85` |
| P5 | Deterministic read models | `7c8016c` |
| P6 | FIFO claim + timing policy | `32c6ec1` |
| P7 | Cancellation request | `c43fdc0` |
| P8 | Guarded dispatch | `b2218d7` |
| P9 | Atomic role publication | `61241f4` |
| P10 | Control sweeps + restart recovery | `7945ee6` |
| P11 | Transactional composer + report reads | `3066996` |
| P12 | End-to-end persistence + race gate | `7e57a05` |
| R1–R4 | Timestamp/sweep/findings-guard/snapshot-hash repairs | released |
| W1–W6 | Worker execution, dispatch, lock, one-attempt supervisor | through `daec080` |
| W7 | HTTP/API composition boundary | `ec33dcb` |

**Blocker carried into M2 — CLOSED by M2-1f (`66267bd`, verified):** HTTP submit
could reach real `sqlite.Submit` without a constructed frozen snapshot;
`SnapshotProvider` was optional, so when nil `SubmitParams.Snapshot` was empty and
SQLite returned `ErrNilSnapshot`. `api.NewIngestSnapshotProvider()` now builds a
deterministic frozen snapshot from submitted content, so the provider supplies a
non-empty snapshot and the blocker no longer occurs (proven via the api handler
tests). Remaining: wire the provider in `cmd/` service composition (deferred).

## M2 DAG

```text
M2-1 Deterministic evidence ingestion
 |        (closes the HTTP nil-snapshot blocker)
 v
M2-2 Stronger finding + citation contract
 |
 v
M2-3 One safe real-provider adapter
 |
 +----------------------+
 v                      v
M2-4 Four-arm         M2-5 Concierge change-review
     benchmark              experiment
```

M2-1→M2-3 are ordered product capabilities. M2-4 and M2-5 are validation
systems; both depend on M2-3 being able to produce real review output but are
otherwise independent of each other.

## M2 slice index

| Slice | Deliverable | Depends on | Type | Status |
|---|---|---|---|---|
| M2-1 | Deterministic evidence ingestion | W7, P4 | product | DONE (`66267bd`, verified) — sub-slices 1a `3a8ca42` / 1b `4cfc40a` / 1c `470759d` / 1d `8b0c5cf` / 1e `71d78f2` / 1f `66267bd` |
| M2-2 | Stronger finding/citation contract | M2-1 | product | DONE (`84affd4`, verified) — M2-2a `839a6be` / M2-2b `aa73f9c` / M2-2c `84affd4`; optional M2-2d (repair-prompt) deferred to before M2-4 |
| M2-3 | One safe real-provider adapter | M2-2 | product | NOT STARTED |
| M2-4 | Four-arm review benchmark | M2-3 | validation | NOT STARTED |
| M2-5 | Concierge change-review experiment | M2-3 | validation | NOT STARTED |

Only M2-1 gets an executable packet first. Later packets are written after the
predecessor is independently verified, so they cite real APIs and paths.

## M2 slice contracts

### M2-1 — Deterministic evidence ingestion

Convert a submitted design into stable evidence units so HTTP submission can
build the frozen snapshot SQLite requires.

Required behavior:

- Split submitted `title`/`content` into ordered `evidence.Unit` values covering:
  - headings;
  - paragraphs;
  - list items;
  - tables;
  - code blocks.
- Preserve explicit author IDs such as `REQ-12` as the unit ID when present;
  otherwise assign deterministic IDs.
- Assign each unit a canonical `evidence.UnitKind`.
- Produce byte-identical units and ordering for identical input (deterministic:
  no wall-clock, no randomness, no map iteration order).
- Record a **splitter version** so future ingestion changes are identifiable.
- Feed the produced units into the existing `evidence.Freeze` path so the
  snapshot hash is computed exactly as today; do not change hashing.
- Wire ingestion as the HTTP `SnapshotProvider` so submit no longer reaches
  SQLite with an empty snapshot. Construction fails closed when no ingestion
  source is supplied.

Rationale: closes the W7 nil-snapshot blocker; gives every later M2 slice a real
snapshot to review.

Forbidden: baseline tables, snapshot lineage/parentage, diffing, or any change to
the frozen snapshot hash algorithm.

Tests: deterministic unit output per input class; ID preservation vs assignment;
splitter-version identity; byte-identical repeat runs; HTTP submit against a real
`*sqlite.Store` now succeeds end to end (the current failing path).

### M2-2 — Stronger finding and citation contract

Make findings express what kind of concern they are and anchor omissions
explicitly.

Required behavior:

- Each finding separates: issue, recommendation, severity, cited evidence, and a
  **finding kind**:
  - existing text (a concern about content that is present);
  - conflicting text (a contradiction between cited units);
  - missing information (an omission).
- Omission findings carry an explicit anchor (the scope/section the omission is
  about) instead of citing unrelated evidence to satisfy the current
  1–5 `basis_refs` rule.
- Preserve current structural validation (ID existence, uniqueness, count,
  allowed-snapshot membership). Add kind/anchor validation on top; do not weaken
  existing checks.
- Document plainly that citation validation proves a referenced unit exists, not
  that it semantically supports the finding.

Rationale: current validation proves a cited ID exists; it does not prove the
citation supports the finding, and it forces omission findings to cite tangential
text.

Forbidden: cross-review finding identity, `fixed`/`reopened`/`regressed`
lifecycle, or baseline/proposed citation namespaces (all M3).

Tests: each finding kind validated and rejected correctly; omission anchor
required and validated; existing basis-ref regression tests stay green.

### M2-3 — One safe real-provider adapter

Connect exactly one real AI provider behind the existing provider boundary.

Required behavior:

- Configure server-side only: base URL, model identifier, and credential from a
  provider-specific environment variable. No secret in source, packets, reports,
  prompts, logs, or chat.
- Enforce a spending limit, a per-request timeout, and the existing bounded
  two-calls-per-role budget.
- Redact secrets and raw provider errors from all persisted output and logs.
- Keep the fake provider as the automated-test provider; the real adapter is
  exercised only by an explicit, opt-in safe smoke path, never by the normal
  suite.
- Before any live call, run a same-working-directory boolean presence probe for
  provider, model, and credential; never print the key or `.env`.
- Treat "OpenAI-compatible" as the request surface only; probe the actual
  response envelope before accepting a gateway.

Rationale: current tests prove engine mechanics, not real review quality.

Forbidden: making validation permissive to avoid real-provider failures;
inventing or remapping evidence references; storing credentials.

Tests: config validation; redaction; budget/timeout enforcement with a stub
transport; the normal suite still uses the fake provider only.

### M2-4 — Four-arm review benchmark (validation system)

Compare four approaches on the same designs to prove whether four roles beat one
strong call.

Arms:

1. free-form AI review;
2. one structured AI call (same schema, caps, validator);
3. four specialist reviewers (current design);
4. four generic calls merged together.

Conditions:

- same source evidence for every arm;
- normalized report rendering so blinding is fair;
- comparable token budgets;
- each arm repeated at least three times;
- seeded defects in real approved designs (contradiction, dropped requirement,
  weakened auth check, missing failure path) for objective ground truth.

Measure:

- defect recall;
- false findings;
- citation support (not just existence);
- important misses;
- repeated findings;
- run-to-run variation;
- cost;
- latency.

Baseline artifacts must be deterministic and reproducible; capture latency and
cost separately as explicitly non-baseline telemetry. Pre-register thresholds
for "meaningful advantage" before running.

Rationale: we must prove whether four roles are better than one strong call.

Forbidden: shipping the benchmark harness as product behavior; changing the v1
runtime to make an arm look better.

#### Optional second-layer checker (Jev / typed-decision model) — experimental

Candidate add-on, NOT core engine behavior. After Layer 1 (the generative roles)
writes findings, an optional Layer 2 could take each finding plus its cited
evidence and return a typed decision such as "cited evidence supports this
finding: yes/no" with a calibrated confidence score. TypeSafe AI's Jev is the
current candidate for this layer (typed output, cheap, fast; it does not generate
prose, so it cannot write findings itself).

Rules if this is tried:

- It lives behind the provider boundary as an optional checker; the engine and
  every automated test MUST still run and pass with the checker absent.
- Its output is a model judgment, not proof. No trust language ("verified",
  "proven") may attach to a finding because Layer 2 scored it. It may only
  annotate or down-rank, never certify.
- It is a validation experiment first: evaluate it as an extra condition inside
  the M2-4 benchmark (does adding Layer 2 reduce false findings / unsupported
  citations without cutting real defect recall?) before any product wiring.
- Do NOT make it a hard dependency, and do NOT let it replace the M2-2 citation
  contract, which is the primary hallucination control.

Timing decision (owned by Hermes): defer wiring until M2-1 and M2-2 are complete;
then test as an M2-4 arm/condition. Only promote to product if the benchmark
shows it meaningfully cuts unsupported findings.

### M2-5 — Concierge change-review experiment (validation system)

Manually test the future workflow with real users:

```text
Approved design
+ proposed revision
→ cited change review
→ human decision
```

Method:

- historical examples first;
- then a 4–6 week live concierge trial if suitable users are available.

Measure:

- preparation time;
- decision usefulness;
- whether findings change decisions;
- whether users voluntarily return with another meaningful change.

Rationale: this tests real demand before we build baseline tables and
change-review infrastructure.

Forbidden: building any M3 persistence, API, or lifecycle to run the experiment;
the concierge work is manual and instrumented, not new product code.

## Not planned for implementation yet (M3 candidates)

These are held out of M2 entirely and built only if M2 shows real users value the
workflow:

1. approved design baselines;
2. baseline version history;
3. automated design diffs;
4. change-review sessions;
5. human Accept / Revise / Reject records;
6. ADR Markdown export;
7. active-baseline concurrency protection.

## M2 definition of done

- M2-1 through M2-3 each have an independently verified commit with executable
  test evidence, and every proven v1 guarantee is unchanged.
- The HTTP nil-snapshot blocker is closed by real end-to-end submission against a
  real `*sqlite.Store`.
- M2-4 and M2-5 produce recorded, reproducible evidence with pre-registered
  thresholds; results are reported as measured facts, not as proof the product is
  "verified" or "independent".
- Normal tests, race tests (WSL where native is CGO-blocked), vet, formatting,
  and diff checks pass; final tree is clean and contains no M3 code.
- `docs/ENGINE-CONTRACT.md` reflects only verified implementation facts.
- A separate, explicitly approved M3 contract is required before any M3 candidate
  is implemented.
