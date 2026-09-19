# SpecCouncil Persistence Build Map for Cline

## Purpose

Build the SQLite persistence foundation in small, independently verified slices.
Only one packet is executed per Cline invocation. This phase ends with durable,
race-tested persistence; worker orchestration, HTTP/auth, live providers, and UI
remain separate later phases.

## Authority

Read in this order for every slice:

1. `docs/CANONICAL-FLOW.md` — runtime authority.
2. The current packet in this directory.
3. Existing Go code and tests.
4. `docs/ENGINE-CONTRACT.md` — implementation gaps only.

If code or old tests conflict with the canonical flow, the canonical flow wins.
Workers may not edit it or silently invent a replacement rule.

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

One packet equals one invocation. A failed slice is repaired; the next slice does
not start. Hermes verifies every result before releasing its successor packet.

## Global rules

- Repository: `D:\PROJECT\SpecCouncil`; Go 1.27.1.
- Use `database/sql` with pure-Go `modernc.org/sqlite`; no CGO or ORM.
- Use embedded numbered migrations. Record and verify an SHA-256 checksum for
  every applied migration; changed applied SQL fails closed.
- Applied migrations are never edited. Corrections are new migrations.
- Write-critical operations use a dedicated connection and explicit
  `BEGIN IMMEDIATE`; do not assume `sql.Tx` is immediate.
- The reusable immediate-transaction boundary retries only SQLite busy/locked
  failures, at most configured `DB_RETRIES`, then returns a typed
  persistence-unavailable error. Never retry conflicts, validation, not-found,
  invariant, or stale-write errors.
- Every connection enforces foreign keys and bounded busy timeout. The file DB
  uses WAL. Report queries use a real read-only connection/pool.
- Concurrency tests use a real file under `t.TempDir()`, not independent
  `:memory:` connections. Synchronize races with barriers, never sleeps.
- Use one documented UTC timestamp representation.
- Never persist credentials, secret-bearing prompts, or raw provider errors.
- No admission cap in v1.
- No splitter may be invented in this phase. Persistence receives an already
  frozen `evidence.Snapshot` plus the original title/content used for request
  hashing. Input-to-evidence splitting belongs to a later ingestion contract.
- No HTTP, auth implementation, live provider, worker goroutines, process lock,
  UI, cross-review, leases, heartbeats, broker, or provider replay.
- Do not edit `docs/CANONICAL-FLOW.md` or add progress narrative to README.
- Do not configure a remote or push.

## Gate after every slice

```bash
test -z "$(gofmt -l .)"
go test ./... -count=1
go vet ./...
git diff --check
git status --short
```

A packet adds focused checks. Commit only when both focused and full gates pass.

## DAG

```text
P0 Canonical domain alignment
 |
 v
P1 Store opening + migration runner + busy retry boundary
 |
 v
P2 Core schema
 |
 v
P3 Transition, citation, and immutability guards
 |
 v
P4 Atomic submission + idempotency
 |\
 | +--------------------+
 v                      v
P5 Read models       P6 FIFO claim + timing policy
 |                      |
 |                   P7 Cancel request
 |                      |
 +-------------------P8 Guarded dispatch
             \          /
              v        v
              P9 Atomic role publication
                       |
                       v
              P10 Control sweeps + restart recovery
                       |
                       v
              P11 Transactional composer + report reads
                       |
                       v
              P12 Integration and race gate
```

## Slice index

| Slice | Deliverable | Depends on | Status |
|---|---|---|---|
| P0 | Canonical composer/reason/count/report contract | current core | READY |
| P1 | SQLite open/close, checksummed migrator, read-only pool, bounded busy retry | P0 | BLOCKED |
| P2 | Core tables, enum/check/FK/unique constraints | P1 | BLOCKED |
| P3 | State-transition, citation, and immutability triggers | P2 | BLOCKED |
| P4 | Request hash v1 and atomic create/idempotency | P3 | BLOCKED |
| P5 | Deterministic read models | P4 | BLOCKED |
| P6 | Validated timing policy and single-session FIFO claim | P4 | BLOCKED |
| P7 | Idempotent cancellation request mutation | P6 | BLOCKED |
| P8 | Deterministic guarded dispatch and cancel race | P7 | BLOCKED |
| P9 | Compare-and-set success/failure publication | P5, P8 | BLOCKED |
| P10 | Cancel/cutoff/deadline sweeps and restart recovery | P9 | BLOCKED |
| P11 | Transactional composition and terminal-only report reads | P10 | BLOCKED |
| P12 | End-to-end persistence and concurrency proof | P11 | BLOCKED |

Only P0 has an executable packet now. Later packets are written after predecessor
verification so they cite real APIs and paths rather than guesses.

## Slice contracts

### P0 — Canonical domain alignment

Remove stale semantics before they enter the schema:

- add `role_failures` and canonical reason/cause validators;
- composer reasons come from role interruption causes, not cancel flag;
- keep `cancel_requested` independently on the report/read model;
- implement canonical status/reason precedence;
- rename failed count to incomplete count and JSON field;
- test all status, reason, cause, count, and JSON cases.

Forbidden: persistence, worker, API, or provider behavior.

### P1 — Store, migrator, and retry boundary

Planned package:

```text
internal/storage/sqlite/
  migrations/
  store.go
  migrate.go
  immediate.go
  errors.go
  *_test.go
```

Required behavior:

- open/close file database with WAL, foreign keys, busy timeout, and documented
  UTC timestamp handling;
- expose a separate read-only pool that cannot execute writes;
- apply ordered embedded migrations atomically;
- store version, filename, SHA-256 checksum, and applied timestamp;
- opening again is idempotent; checksum mismatch and failed migration fail closed;
- immediate transaction helper pins one connection, uses `BEGIN IMMEDIATE`,
  commits/rolls back safely, retries only busy/locked errors up to configured
  `DB_RETRIES`, and returns typed exhaustion;
- validation/conflict/stale errors are never retried.

No product tables are required yet; migrator tests use controlled migration
fixtures. Supervisor exit is a later worker responsibility; this layer must expose
retry exhaustion distinctly.

### P2 — Core schema

Add the first production migration. Tables represent:

- immutable snapshots and ordered evidence units;
- sessions with project/idempotency/request hash/snapshot/state/cancel/timing/
  terminal reason/count fields;
- role runs with canonical role, status, cause, error, and call metadata;
- findings and ordered basis references.

Enforce with CHECK/FK/UNIQUE constraints:

- all canonical enum values only;
- `UNIQUE(project_id, idempotency_key)`;
- `UNIQUE(session_id, role)`;
- per-role finding-ID uniqueness;
- per-finding basis-reference uniqueness and stable ordinal uniqueness;
- unique snapshot unit IDs and ordinals;
- valid null/non-null field combinations for each stored state.

Do not try to enforce “exactly four children” with exotic triggers. P4 atomically
inserts the four canonical roles; the schema enforces identity/uniqueness, and the
composer later requires exact completeness.

### P3 — Transition, citation, and immutability guards

Add a new migration and direct database rejection tests for:

```text
role: pending → in_flight | interrupted
      in_flight → complete | failed | interrupted
session: queued → reviewing
         reviewing → complete | partial | failed
```

Terminal states have no outgoing transitions. Also enforce:

- snapshots and units cannot be updated or deleted;
- findings may be inserted only while their role is in-flight;
- every basis ref exists in that session's snapshot;
- findings and basis refs cannot be updated or deleted after insertion;
- terminal role status, cause/error, and call metadata are immutable;
- illegal cause/error/status combinations fail closed.

### P4 — Atomic submission and idempotency

Persistence input is explicit:

```text
project_id
idempotency_key
title
content
already-frozen evidence.Snapshot
```

The store recomputes `evidence.Freeze(snapshot.ID, snapshot.Units)` and rejects a
missing/mismatched snapshot hash. It does not split content.

Required behavior:

- request hash v1 covers version 1 + exact project_id/title/content;
- strict UTF-8, no trim, no Unicode normalization, submitted line endings
  preserved, deterministic length-delimited encoding, SHA-256;
- one immediate transaction creates snapshot, units, queued session, and the
  four pending roles in canonical order;
- same project/key/hash returns existing; different hash returns typed conflict;
- different projects may reuse a key;
- concurrent same-key loser rereads and compares;
- any failure rolls back every created row;
- no queue/admission cap is added.

### P5 — Deterministic read models

Required behavior:

- reconstruct snapshot and verify stored hash; preserve unit ordinal order;
- load session and exactly four roles in canonical order;
- load findings and ordered unique refs deterministically;
- return typed not-found scoped by project/session identifiers for future 404
  mapping;
- status reads work for every session state;
- reads use the read-only pool and never compose, repair, or write;
- repeated reads return deeply equal values.

### P6 — FIFO claim and timing policy

Define a validated timing policy:

```text
DISPATCH_CUTOFF > 0
CALL_TIMEOUT > 0
SESSION_HARD_DEADLINE >= DISPATCH_CUTOFF + CALL_TIMEOUT
```

Claim uses database time and one immediate transaction:

- refuse to claim while any session is reviewing;
- select oldest queued session by `created_at, id` even when it already has
  `cancel_requested = true`;
- change exactly one `queued → reviewing`;
- set started, dispatch-cutoff, and hard-deadline timestamps atomically;
- zero-row race returns typed no-work/retry outcome.

No process lock, goroutine, or provider call in this slice.

### P7 — Cancellation request

Implement only the request mutation:

- queued/reviewing false → true returns effective true;
- already true returns effective false;
- terminal session returns effective false without mutation;
- unknown/project-mismatched session returns typed not-found;
- concurrent cancels have one effective winner;
- no role state changes and no composer execution here.

### P8 — Deterministic guarded dispatch

One authoritative immediate transaction must:

1. select the lowest canonical pending role (Requirements, Architecture, QA,
   Security);
2. recheck session reviewing, cancel false, database time before cutoff, and
   database in-flight count below two;
3. change only that selected role `pending → in_flight`;
4. return a typed no-dispatch reason when a guard fails.

Prove with barrier-based races:

- never more than two in-flight roles;
- no later role dispatches while an earlier canonical role remains pending;
- committed cancellation beats later dispatch;
- committed dispatch remains legitimately in-flight when cancellation follows.

### P9 — Atomic role publication

Success transaction:

- require and compare-and-set from in-flight;
- insert all validated findings and ordered refs;
- store call metadata;
- set complete;
- all or nothing.

Failure transaction:

- require and compare-and-set from in-flight;
- store typed error and call metadata;
- set failed;
- no findings.

Late/duplicate terminal writes fail closed. Finding/ref failure rolls back role
completion. This uses P1's bounded immediate-transaction retry boundary; it does
not implement worker exit.

### P10 — Control sweeps and restart recovery

Transactional, idempotent operations:

- cancel sweep: pending → interrupted/user_cancelled; in-flight unchanged;
- cutoff sweep: pending → interrupted/deadline_cutoff; in-flight unchanged;
- hard-deadline sweep: pending → interrupted/deadline_cutoff; worker later
  cancels provider contexts and publishes in-flight timeout through P9;
- restart sweep over stale reviewing sessions:
  - complete/failed/interrupted unchanged;
  - in-flight → interrupted/process_restart;
  - pending → interrupted/user_cancelled if cancellation was recorded, otherwise
    interrupted/process_restart;
  - queued sessions untouched;
  - never replay provider work.

### P11 — Transactional composer and reports

Composer in one immediate transaction:

- reread exactly four roles;
- require all terminal and zero in-flight;
- derive canonical counts/status/reason;
- conditional reviewing → terminal update with one winner;
- concurrent second call returns typed already-finalized without divergence.

Report and status reads use the read-only pool. Report method:

- rejects every nonterminal session with typed `not_finished` for future HTTP 409;
- includes session status, terminal reason, cancel flag, completed/incomplete
  counts, per-role status/cause/error/call metadata, and findings;
- orders roles/findings deterministically;
- never composes, repairs, calls providers, or writes.

### P12 — Integration and race gate

Prove the phase as one system:

1. submit → claim → dispatch 2 → publish → dispatch remaining → publish →
   compose complete;
2. concurrent same-key same-hash winner/loser;
3. same key/different hash conflict;
4. queued cancellation → claim → cancel sweep → zero calls represented as
   partial/user_cancelled;
5. cancel beats pending dispatch;
6. dispatch beats cancel and in-flight publication drains;
7. cutoff interrupts pending while in-flight can publish;
8. hard deadline interrupts pending; in-flight timeout publishes failed; late
   success cannot overwrite terminal state;
9. restart preserves complete findings, interrupts unfinished, and replays none;
10. finding/ref insertion failure rolls back completion;
11. busy/locked writes retry only to configured bound and surface typed exhaustion;
12. report before terminal returns typed not_finished;
13. repeated report/composer calls do not mutate terminal data;
14. modified applied migration checksum fails open.

Use barriers, not sleeps. Run repeatedly and with the race detector:

```bash
go test -race ./...
```

## Persistence phase definition of done

- P0–P12 each have an independently verified commit.
- Every acceptance case is executable test evidence.
- Normal tests, race tests, vet, formatting, and diff checks pass.
- Final tree is clean and contains no later-phase code.
- `docs/ENGINE-CONTRACT.md` reflects only verified implementation facts.
- README remains product-focused.
- Hermes independently verifies the final repository.

Completing persistence unlocks the next phase: exclusive worker ownership,
serialized dispatch, two-role concurrency, provider attempt deadlines, and
fail-stop process behavior.
