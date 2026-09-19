# P1C — Immediate Transactions and Bounded Busy Retry

## Mission

Add only the reusable SQLite write boundary required by later persistence slices:
a dedicated connection, explicit `BEGIN IMMEDIATE`, structured busy/locked
classification, bounded retry, cleanup, and typed exhaustion errors. Report, then
STOP. Do not add product schema or use this boundary around provider calls.

Repository: `D:\PROJECT\SpecCouncil` (Go 1.27.1).

## Start and read

Run:

```bash
git status --short --branch
git log -1 --oneline
```

The tree must be clean and include P1B acceptance commit `94d7d66`. Otherwise
preserve everything and report `BLOCKED`.

Read:

1. `docs/CANONICAL-FLOW.md`.
2. `docs/cline/persistence/00-BUILD-MAP.md`.
3. `docs/cline/persistence/P1A-SQLITE-STORE.md`.
4. `docs/cline/persistence/P1B-MIGRATION-RUNNER.md`.
5. `internal/storage/sqlite/store.go` and `migrate.go`.
6. `docs/ENGINE-CONTRACT.md`.

## Scope

Use the existing pure-Go `modernc.org/sqlite` driver. Create or modify only:

```text
internal/storage/sqlite/immediate.go
internal/storage/sqlite/immediate_test.go
internal/storage/sqlite/errors.go
internal/storage/sqlite/errors_test.go
docs/ENGINE-CONTRACT.md   (one factual P1C update after verification)
```

Do not modify P1A/P1B behavior except for a compile-safe package-internal seam
strictly required by this boundary and covered by a focused regression test.

## Configuration and error contract

Define a small package-internal configuration for:

```text
DB_RETRIES >= 0 and bounded so 1 + DB_RETRIES cannot overflow
backoff policy with an enforceable upper bound
```

Use the existing P1A busy timeout. Do not introduce arbitrary unbounded retry or
sleep behavior. Context cancellation always stops retrying.

Expose a typed `PersistenceUnavailable` error for retry exhaustion. It must
record the operation, total attempts, and final driver error, support
`errors.Is`/`errors.As` for the final driver error, and never include DSNs,
credentials, or raw SQL. Domain/callback errors must remain distinguishable from
persistence-unavailable exhaustion.

## Immediate transaction contract

Provide a package-internal function equivalent to:

```text
withImmediate(ctx, db, policy, callback)
```

It must:

1. Check `ctx` before each attempt.
2. Acquire a dedicated `*sql.Conn`; never use a pooled `db.BeginTx` for this
   boundary.
3. Execute literal `BEGIN IMMEDIATE` on that connection. Do not assume a normal
   `sql.Tx` is immediate.
4. Run the callback using the transaction/connection owned by that attempt.
5. Commit only after callback success and context validation.
6. Roll back after every post-BEGIN failure: callback error or panic, context
   cancellation, and commit failure. Use a bounded cleanup context that is not
   the canceled operation context. Preserve the primary error. On panic, clean
   up and repanic with the original panic value.
7. If rollback cannot be confirmed, poison/discard the physical connection before
   returning it to the pool (for example through the driver's `driver.ErrBadConn`
   path). Do not reuse an uncertain connection.
8. Close the dedicated connection on every path.
9. Retry only structured SQLite contention errors from `BEGIN IMMEDIATE`, callback
   SQL, or `COMMIT`: primary result code `SQLITE_BUSY` or `SQLITE_LOCKED`, including
   extended codes such as `SQLITE_BUSY_SNAPSHOT`. Use `errors.As` to the driver's
   structured SQLite error and classify the primary code with the driver's
   documented mask; never use substring matching.
10. Do not retry callback/domain/validation/conflict/stale errors, arbitrary
    driver errors, context cancellation, panic, or cleanup failure.
11. Apply bounded backoff only between retryable attempts, honoring context. A
    retryable callback error must first complete transaction cleanup.
12. Return success only after a confirmed commit. A successful commit is
    authoritative even if cancellation arrives immediately afterward. A failed
    commit is failure, never success.
13. Never call a provider or hold this transaction open across provider work. The
    callback is database-only and must be documented as such.

Use the driver's typed error API and constants available in the pinned module
version. Do not guess result-code values if the module exposes constants.

## Tests

Use real file-backed SQLite databases and barriers/channels, never sleeps. Cover:

- dedicated connection and literal `BEGIN IMMEDIATE` behavior;
- two writers contending on one file, with a controlled busy/locked retry;
- extended busy/locked result codes classify as retryable by primary code;
- retryable busy/locked errors from begin, callback SQL, and commit;
- callback/domain/validation/conflict errors are not retried;
- context cancellation before begin and during backoff stops attempts;
- exact attempt count is `1 + DB_RETRIES` and never overflows;
- exhaustion returns `PersistenceUnavailable` with operation, attempt count, and
  final driver error preserved for `errors.Is`/`errors.As`;
- rollback after callback error, panic, cancellation, and commit failure;
- panic value is repanicked after cleanup;
- uncertain rollback does not return a poisoned connection to the pool;
- dedicated connections close on success, failure, retry, and panic;
- no provider or non-database callback work is introduced;
- no sleeps, unbounded loops, SQLite CLI, product tables, or migration changes.

## Forbidden scope

Do not add:

- product tables, migrations, triggers, schema changes, or read models;
- submission, idempotency, claims, cancellation mutation, dispatch, composer,
  worker, API, auth, provider, UI, leases, heartbeats, or process locks;
- retries for anything except structured SQLite busy/locked contention;
- provider calls inside the transaction;
- changes to `docs/CANONICAL-FLOW.md`, README, build packets, P1A, or P1B.

## Verify

```bash
gofmt -w internal/storage/sqlite/immediate.go \
  internal/storage/sqlite/immediate_test.go \
  internal/storage/sqlite/errors.go \
  internal/storage/sqlite/errors_test.go
test -z "$(gofmt -l .)"
go test ./internal/storage/sqlite -count=1
go test ./internal/storage/sqlite -count=10
go test ./... -count=1
go vet ./...
git diff --check
```

Inspect the final diff for scope and verify no provider call, product schema, or
migration change was added.

If all requirements pass, commit exactly:

```text
Add immediate SQLite transaction retry boundary
```

Never push from Cline.

## Report and stop

```text
P1C RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests:
- <command>: <real result>
Full gate:
- <command>: <real result>
Acceptance evidence:
- dedicated connection:
- BEGIN IMMEDIATE:
- busy/locked classification:
- retry attempt bound:
- non-retryable errors:
- rollback and panic cleanup:
- context cancellation:
- typed exhaustion:
- no provider/database-scope violations:
Remaining limitations:
- ...
Git status:
<exact git status --short output>
```

Any unmet requirement means `BLOCKED` and no commit. Do not begin P2.
