# P1A — SQLite Store Opening and Read-Only Pool

## Mission

Add only the pure-Go SQLite connection foundation: validated file opening, one
configured writer connection, a distinct read-only pool, and safe cleanup. No
migrator, transaction helper, retry loop, or product table. Report, then STOP.

Repository: `D:\PROJECT\SpecCouncil` (Go 1.27.1).

## Start and read

Run `git status --short`, `git branch --show-current`, and
`git log -1 --oneline`. The tree must be clean and include verified P0 commit
`171b9d2`. Otherwise preserve everything and report `BLOCKED`.

Read:

1. `docs/CANONICAL-FLOW.md` — persistence boundaries.
2. `docs/cline/persistence/00-BUILD-MAP.md` — P1A and global rules.
3. `go.mod`, `.gitignore`, and current Go package conventions.

## Scope

Use `database/sql` with pure-Go `modernc.org/sqlite`. Add it through Go modules;
do not hand-edit checksums. No CGO, ORM, query builder, global store, CLI, API, or
background goroutine.

Create only:

```text
internal/storage/sqlite/store.go
internal/storage/sqlite/store_test.go
go.mod
go.sum
docs/ENGINE-CONTRACT.md   (one factual P1A update after verification)
```

## Contract

Define a small validated configuration containing:

```text
database file path
busy timeout > 0
```

Requirements:

1. Require an absolute file path. Clean it, reject empty, relative, directory,
   `:memory:`, and SQLite memory-mode paths, and require its parent directory to
   already exist. The database file itself may be created. Never create directories
   or depend on the current working directory.
2. Open a writer `*sql.DB` with exactly one open and one idle connection.
3. Configure the writer connection to enforce:

```text
PRAGMA foreign_keys = ON
PRAGMA journal_mode = WAL
PRAGMA busy_timeout = configured milliseconds
```

4. Query the live writer connection and fail opening if any required pragma did
   not take effect. Do not treat DSN text as proof.
5. After writer initialization, open a distinct pool using SQLite `mode=ro` for
   the same canonical absolute file path. A SQL write through it must fail.
6. Reader connections must enforce foreign keys and the same bounded busy timeout.
   WAL is verified from the writer and visible to the reader; never attempt to set
   journal mode through the read-only pool. Verify pragmas on two simultaneously
   held reader connections so pooled-connection setup is proven.
7. Prove the reader sees data committed by the writer. P1A tests may create a
   temporary probe table; production code creates no tables.
8. Keep raw pools private. Provide only narrow unexported access that later files
   in package `sqlite` can use.
9. If configure, ping, or reader-open fails, close every resource already opened.
   A narrow unexported opener hook is allowed for deterministic cleanup tests; do
   not add a production test mode or exported reset hook.
10. `Close` is idempotent, closes reader and writer once, returns the real close
    error, and leaves future operations unusable.
11. Wrap errors with operation context but never include DSNs, credentials, or
    raw file content.

Use URL-safe/canonical path construction that works for Windows drive paths,
spaces, and `#`. Do not construct a SQLite URI by raw string concatenation.

## Tests

Use separate real files under `t.TempDir()`. Cover:

- invalid empty, relative, directory, missing-parent, memory, and malformed paths;
- absolute Windows-relevant filenames containing spaces and `#`;
- fresh open creates/opens the file successfully;
- writer has one connection and live foreign-key/WAL/busy-timeout values;
- reader has live foreign-key/busy-timeout values and observes WAL mode;
- reader sees committed writer data;
- write through reader fails and leaves writer data unchanged;
- failure after writer open does not leave a usable/leaked partial store;
- close behavior matches its documented contract;
- two independent test databases never share state.

Do not use sleeps. Tests must not depend on a globally installed SQLite CLI.

## Forbidden scope

Do not add:

- migrations or migration metadata;
- `BEGIN IMMEDIATE` helper or retry behavior;
- snapshot/session/role/finding tables;
- domain/review changes;
- request hashing, idempotency, claims, cancellation, dispatch, composition;
- worker, API, auth, provider, process lock, UI, or remote.

Do not edit README, `docs/CANONICAL-FLOW.md`, build packets, or P0 files.
After all checks pass, update `docs/ENGINE-CONTRACT.md` only to state that the
verified SQLite connection foundation exists; do not claim product persistence.

## Verify

```bash
gofmt -w internal/storage/sqlite/store.go \
  internal/storage/sqlite/store_test.go
test -z "$(gofmt -l .)"
go test ./internal/storage/sqlite -count=1
go test ./internal/storage/sqlite -count=10
go test ./... -count=1
go vet ./...
git diff --check
```

Inspect the diff and verify no migration or product table exists.

If all gates pass, stage only actual P1A paths, run
`git diff --cached --check`, and commit:

```text
Add SQLite connection foundation
```

Never push.

## Report and stop

```text
P1A RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests:
- <command>: <real result>
Full gate:
- <command>: <real result>
Acceptance evidence:
- writer connection limit:
- live writer pragmas:
- live reader pragmas:
- read-only write rejection:
- cross-pool visibility:
- path handling:
- cleanup/close behavior:
Remaining limitations:
- ...
Git status:
<exact git status --short output>
```

Any unmet requirement means `BLOCKED` and no commit. Do not begin P1B.
