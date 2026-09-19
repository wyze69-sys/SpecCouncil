# P5 — Deterministic Read Models

## Mission

Add read-only SQLite status and terminal report models. Reads must never compose,
repair, dispatch, call providers, or write state. Start from clean latest P4
acceptance `e5cf297`.

## Scope

Create or modify only:

```text
internal/storage/sqlite/read.go
internal/storage/sqlite/read_test.go
docs/ENGINE-CONTRACT.md
```

Read the canonical flow, build map, domain report/status types, and existing
schema before coding. Do not modify migrations, triggers, submission, or P1-P4
implementation.

## Contract

Provide package-internal read functions using only the P1A read-only pool:

- status by session ID;
- terminal report by session ID.

The functions must:

1. Execute only SELECT statements through the read-only pool. No INSERT, UPDATE,
   DELETE, transaction write, migration, trigger, composer, provider, or worker
   call is allowed.
2. Return a typed not-found error for an unknown session.
3. Return a typed not-ready error for a report requested before the session is
   terminal. Status reads remain valid for queued and reviewing sessions.
4. Reconstruct deterministic role order from `domain.Roles`, not SQL row order.
5. Reconstruct findings in deterministic order and basis references by ordinal.
6. Preserve `cancel_requested`, status, terminal reason, completed count, and
   incomplete count exactly as persisted.
7. Return no report findings from failed or interrupted roles.
8. Never derive a terminal reason from a live cancellation flag or compose a new
   verdict. Read the committed session/report state only.
9. Use stable UTC timestamp decoding and return malformed persisted data as an
   error, never silently repair it.
10. Avoid leaking credentials, DSNs, raw provider errors, or raw SQL in errors.

Use existing domain types where they are compatible. If a narrow read model is
needed, keep it package-internal and document its mapping.

## Tests

Use a real temporary database and P4 submission fixtures. Cover:

- queued and reviewing status reads;
- terminal complete, partial, and failed status reads;
- unknown session and nonterminal report errors;
- deterministic role order, finding order, and basis ordinal order despite
  insertion order;
- persisted cancellation and count fields are preserved;
- failed/interrupted roles contribute no findings;
- malformed persisted timestamps/enums return errors;
- all reads work through the read-only pool;
- prove reads do not mutate any table, timestamp, or row count;
- prove read functions do not invoke composer/provider/worker paths;
- repeated reads return identical models;
- no write SQL, migration, trigger, API, or worker scope.

No sleeps or SQLite CLI.

## Verify and commit

```bash
gofmt -w internal/storage/sqlite/read.go internal/storage/sqlite/read_test.go
test -z "$(gofmt -l .)"
go test ./internal/storage/sqlite -count=1
go test ./internal/storage/sqlite -count=10
go test ./... -count=1
go vet ./...
git diff --check
```

Commit exactly:

```text
Add deterministic SQLite read models
```

Never push from Cline. Any failure means BLOCKED and no commit. Do not begin P6.

## Report

```text
P5 RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests:
- ...
Full gate:
- ...
Acceptance evidence:
- read-only access:
- status models:
- terminal report gate:
- deterministic ordering:
- persisted field preservation:
- no mutation:
- forbidden-scope inspection:
Remaining limitations:
- ...
Git status:
<exact output>
```
