# P2 — Core SQLite Schema

## Mission

Add only the first SpecCouncil production schema migration and direct database
constraint tests. Build on the verified P1A/P1B/P1C foundation. Report, then
STOP. Do not add repository methods, state transitions, triggers, submission,
worker, API, or provider behavior.

Repository: `D:\PROJECT\SpecCouncil` (Go 1.27.1).

## Start and read

Run:

```bash
git status --short --branch
git log -1 --oneline
```

The tree must be clean and include P1C acceptance commit `93498cd`. Otherwise
preserve everything and report `BLOCKED`.

Read:

1. `docs/CANONICAL-FLOW.md`.
2. `docs/cline/persistence/00-BUILD-MAP.md`.
3. P1A, P1B, and P1C packets.
4. `internal/domain/*.go` and `internal/evidence/*.go`.
5. `docs/ENGINE-CONTRACT.md`.

## Scope

Create or modify only:

```text
internal/storage/sqlite/migrations/002_core_schema.sql
internal/storage/sqlite/schema_test.go
internal/storage/sqlite/migrate_test.go  (only stale P1B production-embedding
                              expectations affected by adding migration 002)
docs/ENGINE-CONTRACT.md   (one factual P2 update after verification)
```

Use the existing migration runner and `withImmediate` only where tests need an
atomic fixture. Do not modify P1A/P1B/P1C code or tests.

## Required schema

Migration `002_core_schema.sql` must create only these production tables:

1. `snapshots`
   - immutable snapshot identity and hash;
   - project identifier, title, content, normalization version, created time;
   - non-empty IDs/text where applicable and unique snapshot identity.
2. `evidence_units`
   - snapshot foreign key, stable unit ID, ordinal, kind, text;
   - unique `(snapshot_id, unit_id)` and `(snapshot_id, ordinal)`;
   - cascading delete only with its parent snapshot.
3. `sessions`
   - ID, project ID, idempotency key, request hash, snapshot ID;
   - canonical session status;
   - `cancel_requested` default false;
   - dispatch cutoff, hard deadline, terminal reason, completed count,
     incomplete count, created/claimed/terminal timestamps;
   - unique `(project_id, idempotency_key)`;
   - foreign key to snapshot;
   - checks for canonical enum values and valid null/non-null combinations.
4. `role_runs`
   - ID, session ID, canonical role, canonical role status, interruption cause;
   - error category/message fields, call count, timestamps;
   - unique `(session_id, role)`;
   - foreign key to session;
   - checks for canonical role/status/cause combinations and valid terminal data.
5. `findings`
   - ID, role run ID, stable finding ID, severity, category, issue,
     recommendation, created time;
   - unique `(role_run_id, finding_id)`;
   - foreign key to a role run.
6. `finding_basis_refs`
   - finding ID, evidence unit ID, ordinal;
   - foreign keys to finding and evidence unit;
   - unique `(finding_id, evidence_unit_id)` and `(finding_id, ordinal)`;
   - valid positive ordinal.

Use the exact canonical values from `internal/domain`, not guessed variants.
Store enum values as stable text. Store booleans as integer `0/1` with checks.
Store timestamps as UTC RFC3339Nano text with a `Z` suffix, consistently with
P1B. Store hashes/checksums as lowercase hexadecimal text with exact length
checks where the source contract defines one.

Use `ON DELETE` behavior deliberately and document it in SQL comments or the
contract update. A finding must not survive its role run; a role run must not
survive its session; evidence references must not point outside the session's
snapshot. Do not add exotic triggers to enforce exactly four roles; P4 owns
atomic creation of the four canonical role rows.

## Required checks

The schema must enforce at the database boundary:

- canonical session, role, terminal-reason, interruption-cause, and role values;
- required uniqueness constraints listed above;
- all foreign keys, with `PRAGMA foreign_keys = ON` already supplied by P1A;
- nonnegative counts and `completed_role_count + incomplete_role_count = 4`;
- completed count cannot exceed four;
- valid null/non-null combinations for queued, reviewing, and terminal records;
- `cancel_requested` is always persisted and never used by a schema trigger to
  compose a verdict;
- no credentials, prompts, raw provider output, or secrets are schema fields.

Do not implement transition enforcement in P2. P3 owns transition,
immutability, and citation guards.

## Tests

Use a real temporary database, run the embedded migration runner, and test direct
SQL rejection/acceptance. Cover:

- all tables and expected columns exist after migration 002;
- foreign keys reject orphan snapshots, units, sessions, roles, findings, and
  basis references;
- every uniqueness constraint rejects duplicates;
- every canonical enum/check accepts valid values and rejects invalid values;
- invalid count combinations reject;
- invalid timestamp/hash/boolean values reject;
- valid null/non-null combinations are accepted;
- delete behavior matches the documented foreign-key policy;
- migration 002 is idempotent through P1B;
- no trigger, API, worker, or repository method is added in P2;
- no credentials or raw provider material appears in schema or tests.

Do not use sleeps or a globally installed SQLite CLI.

## Forbidden scope

Do not add:

- transition or immutability triggers (P3);
- repository/store methods, submission, request hashing, idempotency, claims,
  cancellation mutation, dispatch, composer, workers, API, auth, providers, UI;
- changes to P1A/P1B/P1C implementation code, P1A/P1C tests, canonical flow,
  README, or build packets;
- changes to P1B tests are allowed only to replace stale assertions about the
  production embedded migration count and the pre-P2 no-product-schema state.
  Do not weaken P1B checksum, rollback, manifest, idempotency, or cancellation
  tests; P2 must leave those semantics intact.
- an exactly-four-role trigger or any hidden application workflow.

## Verify

```bash
gofmt -w internal/storage/sqlite/schema_test.go
test -z "$(gofmt -l .)"
go test ./internal/storage/sqlite -count=1
go test ./internal/storage/sqlite -count=10
go test ./... -count=1
go vet ./...
git diff --check
```

Inspect the final diff and verify only `002_core_schema.sql`, its direct tests,
and the factual contract update changed.

If all requirements pass, commit exactly:

```text
Add core SQLite schema
```

Never push from Cline.

## Report and stop

```text
P2 RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests:
- <command>: <real result>
Full gate:
- <command>: <real result>
Acceptance evidence:
- tables and columns:
- canonical enum checks:
- uniqueness:
- foreign keys:
- count and nullability checks:
- delete behavior:
- migration idempotency:
- forbidden-scope inspection:
Remaining limitations:
- ...
Git status:
<exact git status --short output>
```

Any unmet requirement means `BLOCKED` and no commit. Do not begin P3.
