# R1 — Normalize SQLite Timestamp Ordering

## Mission

Repair only the timestamp defect found in the P0–P12 audit. The implementation
base is clean commit `a930f36`; execute from latest master at packet-release
commit `34744c9`, which adds this packet only. Do not fix cancellation, deadline
guards, findings triggers, or snapshot hash validation in this slice.

## Defect

SQLite stores timestamps as `TEXT` and compares them lexicographically. Go's
`time.RFC3339Nano` removes trailing fractional zeroes, so values such as
`...00.1Z` and `...00.15Z` do not sort chronologically. This can reject valid
CHECK constraints and break FIFO, cutoff, restart, publication, and composition.

## Allowed files

```text
internal/storage/sqlite/claim.go
internal/storage/sqlite/migrate.go
internal/storage/sqlite/submit.go
internal/storage/sqlite/compose.go
internal/storage/sqlite/timestamp_regression_test.go
docs/ENGINE-CONTRACT.md
```

Do not modify migrations in this slice. Preserve the public timestamp contract:
UTC, RFC3339-compatible, `Z` suffix. Use one fixed-width formatter with exactly
nine zero-padded fractional digits, for example:

```text
2006-01-02T15:04:05.000000000Z
```

Update every production timestamp writer in the allowed files, including:
claim timestamps, submission `created_at`, composition `terminal_at`, and
migration `applied_at`. Existing parsing may continue to use RFC3339Nano.
Do not leave production uses of variable-width `Format(time.RFC3339Nano)` in
these paths.

## Required regression proof

Add deterministic tests using subsecond values with different fractional widths:

1. `00.100000000Z` and `00.150000000Z` sort chronologically in SQLite text
   comparisons.
2. Claim succeeds when `claimed_at` is 50ms after `created_at`.
3. Role publication succeeds when `completed_at` is 50ms after `started_at`.
4. Composition succeeds when `terminal_at` is 50ms after `claimed_at`.
5. Cutoff and restart temporal predicates still work with subsecond values.
6. Migration timestamps are fixed-width and validate on reopen.
7. Existing whole-second and nanosecond timestamps remain parseable.

Use real temporary stores and existing production methods. Do not weaken schema
checks or replace temporal comparisons with sleeps. Do not modify existing test
helpers merely to hide failures.

## Verification

```bash
gofmt -w internal/storage/sqlite/claim.go internal/storage/sqlite/migrate.go internal/storage/sqlite/submit.go internal/storage/sqlite/compose.go internal/storage/sqlite/timestamp_regression_test.go
test -z "$(gofmt -l .)"
go test -v ./internal/storage/sqlite -run TestTimestamp
go test ./internal/storage/sqlite -count=1
go test ./internal/storage/sqlite -count=10
go test ./...
go vet ./...
git diff --check
```

Inspect production code to prove no variable-width timestamp writer remains in
the allowed paths. Do not run or claim `go test -race` unless available.

Commit exactly:

```text
Fix SQLite timestamp ordering
```

Do not push. Do not fix any other audit finding. Any unrelated changed file or
unmet regression means BLOCKED and no commit.

## Report

```text
R1 RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests:
- ...
Full gate:
- ...
Acceptance evidence:
- fixed-width representation:
- SQLite chronological ordering:
- claim/publication/composition:
- cutoff/restart:
- migration metadata:
- forbidden scope:
Git status:
<exact output>
```
