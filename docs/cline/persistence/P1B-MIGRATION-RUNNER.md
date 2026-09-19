# P1B — Checksummed SQLite Migration Runner

## Mission

Add only the embedded, ordered, checksummed migration runner on top of the
verified P1A SQLite store. Report, then STOP. Do not implement P1C, product
schema, transactions for application writes, or worker behavior.

Repository: `D:\PROJECT\SpecCouncil` (Go 1.27.1).

## Start and read

Run:

```bash
git status --short --branch
git log -1 --oneline
```

The tree must be clean and include the P1A implementation commit `c21c240` plus
coordinator acceptance commit `2f55d30`. Otherwise preserve everything and report
`BLOCKED`.

Read:

1. `docs/CANONICAL-FLOW.md`.
2. `docs/cline/persistence/00-BUILD-MAP.md`.
3. `docs/cline/persistence/P1A-SQLITE-STORE.md`.
4. `internal/storage/sqlite/store.go` and its tests.
5. `docs/ENGINE-CONTRACT.md`.

## Scope

Use the existing pure-Go `modernc.org/sqlite` dependency and P1A store. Do not
add another driver, ORM, query builder, CGO dependency, or migration library.

Create or modify only:

```text
internal/storage/sqlite/migrate.go
internal/storage/sqlite/migrate_test.go
internal/storage/sqlite/migrations/embed.go
internal/storage/sqlite/migrations/001_migration_metadata.sql
docs/ENGINE-CONTRACT.md   (one factual P1B update after verification)
```

If the package layout requires a different migration file location, keep the
same owned scope under `internal/storage/sqlite/` and report the exact paths.
Do not modify P1A store behavior or its tests except where a compile-safe seam is
strictly required and covered by a focused regression test.

## Exact migration manifest rules

The embedded migration filesystem includes the complete top-level migration tree.
Production must embed the whole `migrations` directory (not only `*.sql`) and
walk it recursively so a non-empty nested directory cannot be hidden by an
include pattern. Test filesystems use the same complete-tree traversal.

```text
^[0-9]{3}_[a-z][a-z0-9_]*\.sql$
```

Rules:

- versions are decimal `001` through `999`; `000` is invalid;
- the name after the underscore is lowercase ASCII letters, digits, and `_`, and
  starts with a lowercase letter;
- matching files are sorted by numeric version;
- duplicate numeric versions are rejected, even if names differ;
- top-level `.sql` files not matching the pattern are rejected;
- non-SQL files such as `README.md` are ignored;
- subdirectories are rejected if they contain any file; do not silently discover
  nested migrations;
- an accepted SQL file must be non-empty;
- checksum is SHA-256 over the exact embedded SQL bytes, without trimming,
  newline conversion, or Unicode normalization.

The metadata migration is `001_migration_metadata.sql`. It creates only the
following exact runner-owned table, not SpecCouncil product tables:

```sql
CREATE TABLE schema_migrations (
  version INTEGER PRIMARY KEY CHECK (version BETWEEN 1 AND 999),
  name TEXT NOT NULL UNIQUE,
  checksum TEXT NOT NULL CHECK (length(checksum) = 64),
  applied_at TEXT NOT NULL
);
```

The runner must validate this exact column order, declared types, NOT NULL flags,
primary-key flag, and uniqueness before trusting an existing metadata table.
Checksum is lowercase hexadecimal SHA-256. `applied_at` is UTC RFC3339Nano text
with a `Z` suffix. Extra columns, missing columns, wrong types, nullable required
columns, or wrong constraints fail closed. The metadata migration itself is
applied as the first migration and is recorded in this table.

## Runner contract

Provide a narrow package-internal runner callable by later SQLite packets. It must:

1. Discover and validate the complete manifest before executing any new SQL.
2. Open/validate the metadata table before comparing later migrations. A fresh
   database may apply the metadata migration first.
3. Compare every metadata row with the current manifest before new migrations
   run. Any applied version missing from the manifest fails closed. Any applied
   version whose name or checksum differs fails closed. Do not delete metadata.
4. Apply pending migrations in ascending version order. Each migration's SQL and
   its metadata row are committed atomically in one transaction.
5. Be idempotent: reopening an unchanged database performs no migration work and
   does not alter applied timestamps.
6. Roll back both SQL effects and metadata when migration SQL or metadata insert
   fails. A failed migration must not partially change the database.
7. Reject duplicate versions, malformed names, empty SQL, invalid metadata, and
   checksum/name/version mismatches with errors that identify the operation but
   never expose credentials, DSNs, or secret-bearing SQL.
8. Respect context cancellation at the defined operation boundary: check before
   opening a transaction, before each SQL statement, and immediately before
   calling `Commit`. If cancellation is observed before `Commit` starts, roll back
   and return `context.Canceled`/`context.DeadlineExceeded`. `Commit` has no
   context parameter, so once `Commit` starts its result is authoritative: a
   successful commit is reported as success even if cancellation arrives during
   or immediately after the commit; a failed commit is reported as failure and
   must not be reported as success. Never claim rollback can undo a committed
   migration.
9. Keep migration input injectable through `fs.FS` for tests. Production uses
   `go:embed` of the complete migration directory. Do not add an exported reset
   hook or production test mode.
10. Use ordinary `database/sql` transactions only for migration atomicity. This
   is an explicit P1B exception to the global write-critical transaction rule:
   P1C owns the reusable dedicated-connection `BEGIN IMMEDIATE` boundary. P1B
   must not claim to implement it.
11. Keep raw migration SQL out of errors; errors may identify version/name and
   operation but never expose credentials, DSNs, or secret-bearing SQL.

Migration SQL must be executed as SQL bytes, not reconstructed from parsed text.
Do not use `CREATE TABLE IF NOT EXISTS` to hide checksum or metadata mismatches.

## Tests

Use real temporary database files opened through P1A. Use controlled `fstest.MapFS`
or equivalent injected filesystems for manifest tests. Cover:

- exact filename acceptance and rejection, including `000`, malformed top-level
  SQL, duplicate versions, nested files, ignored README, and empty SQL;
- numeric ordering independent of filesystem enumeration order;
- exact SHA-256 bytes, including a file whose final newline matters;
- fresh database creates metadata and applies all pending migrations;
- migration SQL and metadata row appear atomically;
- unchanged rerun is idempotent and preserves applied timestamps;
- changed SQL for an applied version fails closed;
- changed name for an applied version fails closed;
- applied metadata whose version is missing from the current manifest fails closed;
- a pending migration failure leaves neither its schema effect nor metadata row;
- metadata insert failure rolls back the migration effect;
- context cancellation before work and before commit returns cancellation without
  claiming success; cancellation after commit starts follows the defined commit
  boundary and a successful commit remains successful;
- no product table, session table, role table, snapshot table, or finding table is
  created by P1B;
- two separate databases do not share migration state.

Do not use sleeps or a globally installed SQLite CLI. Do not make tests depend on
wall-clock ordering; compare timestamps for equality on rerun or use a controlled
clock seam only if needed.

## Forbidden scope

Do not add:

- `BEGIN IMMEDIATE`, dedicated write connections, retry loops, busy classification,
  or persistence-unavailable errors (P1C);
- SpecCouncil product tables or schema (P2);
- state transitions, triggers, idempotency, claims, cancellation, dispatch,
  composer persistence, workers, API, auth, providers, UI, or remote changes;
- migration editing after application; corrections are future migration files;
- README, `docs/CANONICAL-FLOW.md`, build-map edits, or P0/P1A source changes
  unrelated to a compile-safe seam.

## Verify

```bash
gofmt -w internal/storage/sqlite/migrate.go \
  internal/storage/sqlite/migrate_test.go \
  internal/storage/sqlite/migrations/embed.go
test -z "$(gofmt -l .)"
go test ./internal/storage/sqlite -count=1
go test ./internal/storage/sqlite -count=10
go test ./... -count=1
go vet ./...
git diff --check
```

Inspect the final diff. Confirm only migration metadata is created and no P1C
behavior was added.

If every gate passes, stage only P1B paths, run `git diff --cached --check`, and
commit exactly:

```text
Add checksummed SQLite migrations
```

Never push from Cline.

## Report and stop

```text
P1B RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests:
- <command>: <real result>
Full gate:
- <command>: <real result>
Acceptance evidence:
- manifest rules:
- exact checksums:
- fresh apply and ordering:
- idempotent reopen:
- changed applied migration rejection:
- missing applied migration rejection:
- atomic rollback:
- cancellation:
- no product schema:
Remaining limitations:
- ...
Git status:
<exact git status --short output>
```

Any unmet requirement means `BLOCKED` and no commit. Do not begin P1C.
