# SpecCouncil Build Map for Cline

## Purpose

This is the execution map for finishing SpecCouncil without asking one coding
agent to invent or implement the whole system at once. Work is divided into
small, independently verified slices. Only one slice is executed at a time.

The current phase ends when the SQLite persistence foundation is complete and
proven. Worker orchestration, HTTP, authentication, a live provider, and UI are
later phases and must not leak into these slices.

## Authority

Read in this order before every slice:

1. `docs/CANONICAL-FLOW.md` — runtime behavior; highest authority.
2. The current slice packet in this directory.
3. Existing Go code and tests.
4. `docs/ENGINE-CONTRACT.md` — current implementation gaps only.

If code, comments, or old tests conflict with `CANONICAL-FLOW.md`, the canonical
flow wins. Do not silently alter the canonical flow. Record a blocker instead.

## Execution protocol

For every slice, Cline must follow this loop:

```text
UNDERSTAND
-> inspect git status and the named source files
-> restate the slice boundary internally

IMPLEMENT
-> make only the slice's changes
-> preserve unrelated work

TEST
-> run focused tests
-> run gofmt, full tests, vet, and git diff checks

FIX
-> investigate every failure
-> rerun focused checks after each repair

VERIFY
-> inspect the final diff against every acceptance item
-> prove no forbidden scope was changed

COMMIT
-> commit only after every gate passes
-> never push

REPORT
-> exact commit
-> changed files
-> commands and real results
-> acceptance evidence
-> remaining limitations
-> STOP
```

One packet equals one Cline execution. Never execute two packets together. If a
slice fails, repair that slice; do not begin the next one. Hermes independently
verifies each report before the next packet is written or released.

## Global rules

- Repository: `D:\PROJECT\SpecCouncil`
- Language: Go 1.27.1.
- Use `modernc.org/sqlite` for a pure-Go SQLite driver; do not introduce CGO.
- Use `database/sql`; do not add an ORM or query builder.
- Use embedded, numbered SQL migrations. Applied migrations are immutable.
- Provider calls never run inside database transactions.
- Write-critical operations use a dedicated connection with explicit
  `BEGIN IMMEDIATE`; do not assume `sql.Tx` begins an immediate transaction.
- The immediate-transaction helper retries only SQLite busy/locked failures, at
  most the configured `DB_RETRIES`, then returns a typed persistence-unavailable
  error. Validation, conflict, not-found, and invariant errors are never retried.
- Every connection must enforce foreign keys and a bounded busy timeout.
- Use WAL mode for the file-backed database.
- Persistence/concurrency tests use a real file in `t.TempDir()`, not a separate
  `:memory:` database per connection.
- Store UTC timestamps in one documented representation and compare them
  consistently.
- Never store provider credentials, prompts containing secrets, or raw provider
  error bodies.
- No hidden fallback behavior. Return typed errors.
- No API, authentication implementation, live provider, worker pool, web UI,
  cross-review, leases, heartbeats, broker, or automatic provider replay in this
  phase.
- Do not edit `docs/CANONICAL-FLOW.md` from an implementation slice.
- Do not push or configure a Git remote.

## Verification gate after every slice

Run from the repository root:

```bash
gofmt -w <changed-go-files>
test -z "$(gofmt -l .)"
go test ./...
go vet ./...
git diff --check
git status --short
```

The slice packet adds focused commands. A commit is permitted only after the
focused checks and this full gate pass.

## Persistence phase DAG

```text
P0 Canonical domain alignment
 |
 v
P1 SQLite schema + migration runner
 |
 v
P2 Atomic submission + idempotency
 |
 +-------------------+
 |                   |
 v                   v
P3 Snapshot/session  P4 Claim/cancel/guarded dispatch
read models           |
 |                   |
 +---------+---------+
           v
P5 Atomic role publication
           |
           v
P6 Interruption + restart recovery
           |
           v
P7 Transactional composer + report reads
           |
           v
P8 Persistence integration and race gate
```

## Slice index

| Slice | Deliverable | Depends on | Status |
|---|---|---|---|
| P0 | Align domain/composer with the approved state and reason model | current core | READY |
| P1 | Open SQLite store, migrations, schema constraints, transition guards | P0 | BLOCKED |
| P2 | Request hash v1 and atomic create/idempotency behavior | P1 | BLOCKED |
| P3 | Deterministic snapshot, session, role, and finding read models | P2 | BLOCKED |
| P4 | FIFO claim, cancellation flag, and guarded dispatch transaction | P2 | BLOCKED |
| P5 | Compare-and-set success/failure publication with findings | P3, P4 | BLOCKED |
| P6 | Cancel/cutoff/hard-deadline interruption and restart recovery | P5 | BLOCKED |
| P7 | Transactional gate/composer and read-only deterministic report | P6 | BLOCKED |
| P8 | File-backed integration, concurrency, rollback, and race proof | P7 | BLOCKED |

Only P0 has an executable packet now. Later packets are finalized after their
dependencies are independently verified, so they can cite real APIs and file
paths instead of guesses.

## Slice contracts

### P0 — Canonical domain alignment

Goal: remove stale in-memory semantics before encoding them in SQLite.

Required result:

- Terminal reason includes `role_failures`.
- Composer derives the reason from role interruption causes, not a live cancel
  flag.
- Status and reason precedence exactly match the canonical flow.
- `failed_role_count` becomes `incomplete_role_count` everywhere.
- Report JSON uses `incomplete_role_count`.
- All complete/partial/failed and mixed-cause cases have tests.

Forbidden: database code, worker concurrency, API, provider changes.

Verify: focused domain/review tests plus the full gate.

### P1 — SQLite schema and migration runner

Goal: create the durable state model and database invariants without repository
business methods.

Planned package:

```text
internal/storage/sqlite/
  migrations/*.sql
  migrate.go
  store.go
  schema_test.go
  transition_test.go
```

Schema must represent:

- immutable snapshots and ordered evidence units;
- review sessions with project/idempotency/hash/state/timing/reason/count fields;
- exactly the four canonical role identities per created session;
- role state, interruption cause, error category, and call metadata;
- findings and ordered basis references;
- migration version history.

Database constraints/triggers must reject:

- unknown session, role, reason, cause, error, severity, purpose, or unit-kind
  values;
- illegal role and session state transitions;
- mutation/deletion of frozen snapshots or units;
- findings for a role that is not `in_flight`;
- evidence references absent from the session's snapshot;
- mutation of terminal roles;
- terminal-field combinations that contradict the status.

Opening an existing database must be idempotent. Failed migrations roll back.
The store exposes a true read-only connection/pool for report queries. The
immediate-transaction helper implements the bounded busy/locked retry contract
and returns a typed terminal error after exhaustion; worker fail-stop behavior is
a later phase.

Verify: fresh/open-again migration tests, pragma tests, constraint rejection,
transition matrix, and full gate.

### P2 — Atomic submission and idempotency

Goal: implement canonical request hashing and atomic creation.

Required behavior:

- Hash normalization version 1 plus exact `project_id`, `title`, and `content`.
- Strict UTF-8, no trimming, no Unicode normalization, preserved line endings,
  deterministic unambiguous field boundaries, SHA-256.
- One `BEGIN IMMEDIATE` operation creates snapshot, units, queued session, and
  four pending roles.
- Same project/key/hash returns the existing review.
- Same project/key/different hash returns typed idempotency conflict.
- Different projects may reuse a key.
- Concurrent same-key creators have one winner; losers reread and compare.
- Any insert failure leaves none of the creation rows behind.

Verify with a real file-backed database and concurrent goroutines.

### P3 — Deterministic read models

Goal: reconstruct committed snapshots and review state without mutation.

Required behavior:

- Snapshot round-trip preserves unit order and recomputes the stored hash.
- Session and four role rows load deterministically in canonical role order.
- Findings load with ordered, unique basis references.
- Not-found is typed and does not expose another project through a future API
  seam.
- Read methods use the store's read-only connection and do not compose, repair,
  or write.

Verify repeat reads produce deeply equal values and leave database write counters
unchanged.

### P4 — Claim, cancel, and guarded dispatch

Goal: encode the authority transactions that close scheduling races.

Required behavior:

- FIFO claim orders by `created_at, id`, refuses to claim while any session is
  already reviewing, and conditionally changes one session `queued -> reviewing`
  while setting timing metadata in the same transaction.
- Cancellation only flips false to true for queued/reviewing sessions; terminal
  sessions and repeated cancellation are no-ops with explicit effectiveness.
- Guarded dispatch uses database time and conditionally changes one canonical
  pending role to in-flight only while the session is reviewing, cancellation is
  false, cutoff is future, and database in-flight count is below two.
- No third role can be claimed under concurrent dispatch attempts.
- A committed cancel beats a later dispatch; a committed dispatch is allowed to
  drain.

Do not start goroutines or call a provider in this slice.

### P5 — Atomic role publication

Goal: persist trusted role outcomes exactly once.

Success transaction:

- require role `in_flight`;
- insert all validated findings and ordered basis references;
- store call metadata;
- compare-and-set role to `complete`;
- commit all or nothing.

Failure transaction:

- require role `in_flight`;
- store typed error and call metadata;
- compare-and-set role to `failed`;
- no findings.

Late or duplicate terminal writes must fail closed without changing committed
rows. Injected failures must prove rollback. This slice does not implement retry
loops or supervisor behavior.

### P6 — Interruption and restart recovery

Goal: terminalize control-path work without replaying providers.

Required operations:

- Cancel sweep: pending -> interrupted/user_cancelled; in-flight unchanged.
- Cutoff sweep: pending -> interrupted/deadline_cutoff; in-flight unchanged.
- Hard-deadline sweep: remaining pending -> interrupted/deadline_cutoff. The
  provider goroutine later publishes in-flight timeout via P5 compare-and-set.
- Restart sweep over stale reviewing sessions:
  - complete/failed/interrupted unchanged;
  - in-flight -> interrupted/process_restart;
  - pending -> interrupted/user_cancelled when cancel was recorded, otherwise
    interrupted/process_restart;
  - queued sessions untouched;
  - no provider action or replay.

Every sweep is idempotent and transactional.

### P7 — Transactional composer and report reads

Goal: finalize once from committed rows and expose deterministic read data.

Required behavior:

- One `BEGIN IMMEDIATE` operation rereads all four roles, verifies all terminal
  and zero in-flight, derives counts/status/reason, and conditionally changes the
  session from reviewing to terminal.
- Precedence: all roles complete, user_cancelled, process_restart,
  deadline_cutoff, role_failures.
- Cancellation with zero completed roles is partial/user_cancelled.
- Store completed and incomplete counts.
- Concurrent composer calls produce one winner and one typed already-finalized
  result; they never diverge.
- Report reads are read-only and deterministically order roles and findings.

### P8 — Persistence integration and race gate

Goal: prove the persistence phase as one system, not only isolated methods.

Required scenarios:

1. submit -> claim -> dispatch two -> publish -> dispatch remaining -> publish ->
   compose complete;
2. same-key concurrent submission winner/loser behavior;
3. cancel beats pending dispatch;
4. dispatch beats cancel and in-flight publication drains;
5. cutoff interrupts pending roles;
6. restart keeps complete findings and interrupts unfinished roles without replay;
7. late provider publication cannot overwrite restart interruption;
8. finding insert failure rolls back role completion;
9. persistence lock/busy failure is typed and bounded;
10. repeated reads and composer calls do not mutate terminal data.

Use synchronization barriers, not sleeps, for race tests. Run each concurrency
scenario repeatedly and run the full suite with Go's race detector:

```bash
go test -race ./...
```

## Persistence phase definition of done

The phase is done only when:

- P0 through P8 each have a verified commit;
- all acceptance cases are executable tests;
- `go test -race ./...`, normal tests, vet, formatting, and diff checks pass;
- the final tree is clean;
- no forbidden later-phase code was introduced;
- `docs/ENGINE-CONTRACT.md` is updated only to state what is now implemented,
  without copying the canonical flow or adding progress narrative to README;
- Hermes independently verifies the final repository state.

Passing the persistence phase does not mean the product is finished. It unlocks
the next phase: the serialized worker and bounded two-role dispatcher.
