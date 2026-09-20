# R3 — Findings Insertion Guard

## Mission

Repair only the missing P3 database guard that allows findings to be inserted
while a role is not `in_flight`. The implementation base is clean commit
`2d95904` (R2 complete); execute from latest master at the packet-release
commit that adds this packet only. Do not fix snapshot hash validation in this
slice.

## Defect

`internal/storage/sqlite/migrations/003_state_guards.sql` creates `findings_no_update`
and `findings_no_delete` but no `BEFORE INSERT` guard. A direct SQL insert
into `findings` for a role in `pending`, `complete`, `failed`, or
`interrupted` is permitted by the database, violating the build-map P3
requirement: “findings may be inserted only while their role is in-flight”.

Existing tests only assert UPDATE/DELETE rejection and therefore pass
false-green.

## Allowed files

```text
internal/storage/sqlite/migrations/004_findings_insert_guard.sql
internal/storage/sqlite/findings_guard_test.go
docs/ENGINE-CONTRACT.md
```

Do not modify `003_state_guards.sql`, `sweep.go`, `submit.go`, `publish.go`,
`claim.go`, `migrate.go`, `compose.go`, or any other migration in this slice.
The prior fixed-width timestamp formatting from R1 must remain (no `Format(time.RFC3339Nano)`
reintroduction). A new versioned migration (`004`) is required because existing
checksums are immutable.

## Contract

1. Create `internal/storage/sqlite/migrations/004_findings_insert_guard.sql` containing exactly one trigger:

```sql
CREATE TRIGGER findings_insert_in_flight_guard
BEFORE INSERT ON findings
WHEN (SELECT status FROM role_runs WHERE id = NEW.role_run_id) IS NOT 'in_flight'
BEGIN
  SELECT RAISE(ABORT, 'findings may only be inserted while role is in_flight');
END;
```

Notes:
- Use `IS NOT` (NULL-safe) so the guard also fires when the role does not exist; the foreign-key constraint remains the primary enforcement for nonexistent roles.
- Trigger name must be exactly `findings_insert_in_flight_guard`.
- File must be non-empty, match `^[0-9]{3}_[a-z][a-z0-9_]*\.sql$`, and be discovered by `discoverManifest`.

2. The migration must apply cleanly on both fresh and existing databases (where 001–003 already applied) via `Migrate()`, and be recorded in `schema_migrations` with SHA-256 checksum and fixed-width `applied_at` (UTC, nine fractional digits, `Z`).

3. After migration, a direct SQL insert:

```sql
INSERT INTO findings (...) VALUES (...)
```

must be rejected with the above ABORT message whenever the referenced `role_runs.status` is not `in_flight` (including `pending` and terminal states). Inserts when the role is `in_flight` must succeed.

4. Existing application flows (`PublishRoleSuccess` inside `in_flight`) must continue to succeed; `PublishRoleFailure`/terminal/in-flight sweeps are unaffected.

## Required regression proof

Add `internal/storage/sqlite/findings_guard_test.go` with deterministic tests using real temporary stores (no sleeps):

1. Fresh store migrates includes `004_findings_insert_guard.sql` (verify `schema_migrations` count becomes 4 after `Migrate()`).
2. Direct insert of a finding into a `pending` role is rejected (error contains “findings may only be inserted while role is in_flight”).
3. Direct insert into `complete`, `failed`, and `interrupted` roles is rejected with the same message.
4. Direct insert into `in_flight` role succeeds and the row is readable (count = 1).
5. `PublishRoleSuccess` for an `in_flight` role succeeds end-to-end and persists findings.
6. `Migrate()` idempotency: second run is a no-op and does not change `applied_at` checksums.
7. Verification that the guard coexists with existing P3 guards (existing `state_guard_test.go` UPDATE/DELETE checks still pass).

Use `openGuardTestStore` / `Submit` + `ClaimSessionWithNow` + `ReservePendingRoleWithNow` helpers as in prior sweep tests. Do not edit test helpers to hide failures beyond the guard itself.

## Verification

```bash
gofmt -w internal/storage/sqlite/migrations/004_findings_insert_guard.sql internal/storage/sqlite/findings_guard_test.go
test -z "$(gofmt -l .)"
go test -v ./internal/storage/sqlite -run TestFindingsInsertGuard
go test -v ./internal/storage/sqlite -run TestStateGuard
go test ./internal/storage/sqlite -count=1
go test ./internal/storage/sqlite -count=10
go test ./... -count=1
go vet ./...
git diff --check
```

Also verify scope:

```bash
git diff --name-only | grep -v -E '^(internal/storage/sqlite/migrations/004_findings_insert_guard\.sql|internal/storage/sqlite/findings_guard_test\.go|docs/ENGINE-CONTRACT\.md|docs/cline/persistence/R3-FINDINGS-GUARD\.md)$' && echo "FORBIDDEN FILES MODIFIED" || echo "scope clean"
git grep -n 'Format(time.RFC3339Nano)' -- internal/storage/sqlite/*.go && echo "VARIABLE WIDTH TIMESTAMP FOUND" || echo "timestamp width clean"
```

Do not run or claim `go test -race` unless available.

Commit exactly:

```text
Add findings in-flight insert guard
```

Do not push. Do not fix snapshot hash. Any unrelated changed file or unmet regression means BLOCKED and no commit.

## Report

```text
R3 RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests:
- ...
Full gate:
- ...
Acceptance evidence:
- trigger exists:
- pending rejection:
- terminal rejection:
- in_flight success:
- PublishRoleSuccess:
- migration idempotency:
- forbidden scope:
Git status:
<exact output>
```
