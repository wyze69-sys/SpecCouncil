# P3 — State, Citation, and Immutability Guards

## Mission

Add only database triggers that protect canonical state transitions, cancellation
monotonicity, immutable records, and evidence citations. Build on P2 commit
`57dc440`. Do not add repository methods or application workflow.

## Start

Require a clean tree and latest P2 acceptance commit `57dc440`. Read
`docs/CANONICAL-FLOW.md`, the build map, P2 packet, domain status definitions,
and the existing schema migration.

## Scope

Create or modify only:

```text
internal/storage/sqlite/migrations/003_state_guards.sql
internal/storage/sqlite/state_guard_test.go
docs/ENGINE-CONTRACT.md
```

## Required triggers

Add migration 003 with triggers that enforce:

### Role transitions

Allowed only:

```text
pending   -> in_flight | interrupted
in_flight -> complete | failed | interrupted
```

Terminal role states cannot change. Role transition updates must preserve the
canonical cause/error field rules already defined by P2.

### Session transitions

Allowed only:

```text
queued   -> reviewing
reviewing -> complete | partial | failed
```

Terminal session states cannot change. The trigger must reject composition that
violates the canonical terminal fields and counts. P11 still owns composition;
P3 only rejects invalid stored transitions.

### Cancellation

`cancel_requested` may change only from `0` to `1`. It may never reset to `0`.
No trigger may compose a verdict or dispatch a role.

### Immutability

After insert, reject updates and deletes for:

- snapshots;
- evidence units;
- findings;
- finding basis references;
- immutable identity/hash/content fields of sessions and role runs.

Allow only fields explicitly needed by the canonical runtime to change: session
status/timing/cancel/terminal fields and role-run status/cause/error/call/timing
fields, subject to the transition guards.

### Citation integrity

On finding-basis-reference insert or update, require the referenced evidence unit
to belong to the same snapshot as the finding's role run's session. Reject
cross-snapshot citations. Reject references to missing findings or evidence units
through existing foreign keys.

Do not use triggers to enforce exactly four role rows; P4 owns atomic creation.
Do not add triggers that call providers, compose reports, or dispatch work.

## Tests

Use a real temporary database and the migration runner. Test:

- every legal role transition succeeds;
- illegal role transitions and terminal rewrites fail;
- every legal session transition succeeds;
- illegal session transitions and terminal rewrites fail;
- cancellation is monotonic;
- immutable snapshot/evidence/finding/reference data rejects update/delete;
- allowed mutable session and role fields remain usable;
- cross-snapshot evidence references fail;
- same-snapshot references succeed;
- trigger failures leave the attempted transaction unchanged;
- migration 003 is idempotent;
- no repository, worker, API, provider, or composition code is added.

No sleeps and no SQLite CLI.

## Forbidden

Do not add P4 submission/idempotency, P5 reads, claims, dispatch, workers, API,
auth, provider, UI, or repository methods. Do not modify P1/P2 migrations,
canonical flow, README, or prior tests except compile-safe fixture updates.

## Verify and commit

```bash
gofmt -w internal/storage/sqlite/state_guard_test.go
test -z "$(gofmt -l .)"
go test ./internal/storage/sqlite -count=1
go test ./internal/storage/sqlite -count=10
go test ./... -count=1
go vet ./...
git diff --check
```

Commit exactly:

```text
Add SQLite state and citation guards
```

Never push from Cline.

## Report

```text
P3 RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests:
- ...
Full gate:
- ...
Acceptance evidence:
- role transitions:
- session transitions:
- cancellation monotonicity:
- immutability:
- citation integrity:
- idempotent migration:
- forbidden-scope inspection:
Remaining limitations:
- ...
Git status:
<exact output>
```

Any failure means BLOCKED and no commit. Do not begin P4.
