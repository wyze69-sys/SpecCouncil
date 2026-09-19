# Engine Contract — milestone 1

This file records what the engine implements, and marks every place where the
specification has not frozen a decision. Unfrozen choices are isolated in one
place in the code so that freezing the specification is a one-line change.

## Canonical runtime flow

The approved v1 runtime authority is [`CANONICAL-FLOW.md`](CANONICAL-FLOW.md).
It resolves the previously open cancellation, persistence-failure, request-hash,
hard-deadline, and terminal-reason rules. This milestone contract records only
which parts of that flow the current code implements.

## Confirmed contract implemented

### Canonical roles

Requirements, Architecture, QA, Security. Dispatch order is the order above.
`domain.Roles` is the single source of that order, and `RoleOrder` rejects any
other identifier.

### Role states

`pending | in_flight | complete | failed | interrupted`

Terminal means `complete | failed | interrupted`. Legal transitions:

```
pending   -> in_flight | interrupted
in_flight -> complete  | failed | interrupted
```

Retrying is not a role state. Both provider attempts of a role happen inside one
`in_flight` execution.

Interrupts carry a cause: `user_cancelled`, `deadline_cutoff`, `process_restart`.

### Session states

`queued | reviewing | complete | partial | failed`

There is no persistent `cancelled` session state. Cancellation is a request flag
plus a terminal reason.

### Snapshot

`evidence.Freeze` copies the caller's units, rejects empty ids, empty text,
unknown kinds and duplicate ids, and hashes the units in sorted order so the
hash does not depend on input order. The hash is what makes a snapshot
addressable.

All four roles receive the same snapshot. No per-role selection.

### Provider output pipeline

```
raw body
  -> strict JSON parse            (unknown fields rejected)   -> invalid_json
  -> structural schema validation                            -> schema_invalid
  -> semantic validation (basis_refs must exist)             -> invalid_basis_ref
  -> trusted ReviewerResult
```

Enforced bounds: at most 15 findings; 1–5 unique basis refs per finding; issue
and recommendation non-empty and at most 1000 characters; unique finding ids.
An empty findings list is valid.

### Two-call provider budget

| First call result | Second call | Purpose | Outcome if the second call also fails |
|---|---|---|---|
| Transport error (network, 429, retryable 5xx, timeout) | yes | `transport_retry` | role failed, transport category |
| Transport OK but invalid JSON / schema / basis refs | yes | `format_repair` | role failed, validation category |
| Fatal 4xx, auth failure, provider rejection | no | — | role failed, `provider_rejected` |
| Prompt exceeds the model input budget | no call at all | — | role failed, `budget_exhausted` |
| Retry already used, then malformed output | no third call | — | role failed, validation category |

A role can never exceed two provider calls.

### Composer

The current milestone composer correctly refuses to run until all four role
outcomes are terminal and produces deterministic ordering. Its in-memory verdict
logic aligns with the approved canonical flow:

- derives `terminal_reason` from committed interruption causes, not the live
  cancellation flag;
- includes `process_restart`, `deadline_cutoff`, and `role_failures` reasons;
- uses `incomplete_role_count` (`4 - completed_role_count`);
- executing the gate and terminal session update transactionally will be added
  once persistence exists.

Canonical composer behavior is defined only by `CANONICAL-FLOW.md`.

Current deterministic finding order is:

```text
severity rank, role rank, primary basis_ref, category, finding id
```

A failed or interrupted role contributes zero findings.

### SQLite connection foundation

Package `internal/storage/sqlite` provides the verified pure-Go SQLite connection foundation:

- Validated configuration requiring an absolute filesystem path with an existing parent directory (rejecting empty, relative, directory, and memory-mode paths) and an exact whole-millisecond busy timeout within `[1ms, 2147483647ms]`.
- Single-connection writer pool with live verification of `foreign_keys = ON`, `journal_mode = WAL`, and configured `busy_timeout`.
- Distinct read-only reader pool (`mode=ro`) with connection-local foreign-key and busy-timeout pragmas inherited by pooled connections.
- Cross-pool visibility verified and SQL writes through the read-only pool rejected.
- Concurrency-safe and idempotent store cleanup closing both pools and joining errors.

### SQLite schema migrations

Package `internal/storage/sqlite` provides the embedded, ordered, checksummed SQLite migration runner:

- Manifest validation enforcing `^[0-9]{3}_[a-z][a-z0-9_]*\.sql$` filenames, decimal versions 001–999, non-empty SQL bytes, and rejecting malformed names, version 000, duplicate versions, and non-empty subdirectories.
- SHA-256 checksums computed over exact embedded SQL bytes.
- Atomic execution of each migration's SQL and metadata recording (`schema_migrations` tracking table with numeric version, exact name, checksum, and UTC applied time in RFC3339Nano format).
- Fail-closed verification comparing existing metadata rows against the manifest on every run; rejecting missing applied versions, name mismatches, or checksum mismatches.
- Idempotent reopen without modifying applied timestamps or re-running applied migrations.
- Context cancellation respected before and during migration execution.

### SQLite immediate transaction retry boundary

Package `internal/storage/sqlite` provides the verified immediate transaction write boundary and bounded busy retry:

- Dedicated `*sql.Conn` per attempt with literal `BEGIN IMMEDIATE` execution (acquiring the RESERVED lock immediately, never assuming a normal `sql.Tx` is immediate).
- Structured `SQLITE_BUSY` and `SQLITE_LOCKED` contention classification (including extended result codes such as `SQLITE_BUSY_SNAPSHOT`) using the 8-bit primary result code mask and `errors.As` without substring matching.
- Bounded retry loop executing at most `1 + DB_RETRIES` attempts with enforceable upper-bounded backoff honoring context cancellation.
- Typed `PersistenceUnavailable` error for retry exhaustion preserving operation name, total attempts, and the underlying driver error for `errors.Is`/`errors.As` without exposing DSNs, credentials, or raw SQL.
- Strict database-only callback boundary; domain, validation, conflict, or non-busy driver errors fail immediately without retry.
- Cleanup guarantees: rollback using a bounded independent cleanup context on callback error, panic, context cancellation, and commit failure; re-panic of original panic value; and connection poisoning via `driver.ErrBadConn` if rollback cannot be confirmed.
- Authoritative commit: a confirmed commit returns success even if cancellation arrives immediately afterward.

### SQLite core schema

Package `internal/storage/sqlite` provides the verified core production schema via migration `002_core_schema.sql`:

- `snapshots`: immutable snapshot identity, lowercase SHA-256 hash, project ID, title, content, normalization version 1, and UTC RFC3339Nano timestamp with 'Z' suffix.
- `evidence_units`: addressable design segments with foreign key to snapshot (`ON DELETE CASCADE`), stable unit ID, non-negative ordinal, canonical kind (`brief`, `requirement`, `component`, `flow`, `constraint`, `data_rule`), non-empty text, and unique `(snapshot_id, unit_id)` and `(snapshot_id, ordinal)`.
- `sessions`: review session entity with project ID, idempotency key, request hash, foreign key to snapshot (`ON DELETE RESTRICT`), canonical status (`queued`, `reviewing`, `complete`, `partial`, `failed`), `cancel_requested` boolean (0/1), timing deadlines (`dispatch_cutoff_at`, `hard_deadline_at`), terminal reason, completed/incomplete role counts (`completed_role_count + incomplete_role_count = 4`), timestamps, unique `(project_id, idempotency_key)`, and valid state-dependent null/non-null column checks.
- `role_runs`: per-role execution tracking with foreign key to session (`ON DELETE CASCADE`), canonical role (`requirements`, `architecture`, `qa`, `security`), canonical status (`pending`, `in_flight`, `complete`, `failed`, `interrupted`), interruption cause, error category, call count (0..2), timestamps, unique `(session_id, role)`, and valid status/cause/error checks.
- `findings`: reviewer findings with foreign key to role run (`ON DELETE CASCADE`), stable finding ID, canonical severity (`critical`, `high`, `medium`, `low`), category, length-bounded issue (<= 1000 chars) and recommendation (<= 1000 chars), created timestamp, and unique `(role_run_id, finding_id)`.
- `finding_basis_refs`: citation links from findings to evidence units with foreign key to finding (`ON DELETE CASCADE`), foreign key to evidence unit (`ON DELETE RESTRICT`), positive ordinal (1..5), and unique `(finding_id, evidence_unit_id)` and `(finding_id, ordinal)`.

### SQLite state, citation, and immutability guards

Package `internal/storage/sqlite` provides verified database triggers via migration `003_state_guards.sql`:

- `Role transitions`: guarded to allow only `pending -> in_flight | interrupted` and `in_flight -> complete | failed | interrupted`. Terminal role states (`complete`, `failed`, `interrupted`) cannot be rewritten or transitioned. Transition field combinations (started_at, completed_at, cause, error_category, call_count) are strictly verified.
- `Session transitions`: guarded to allow only `queued -> reviewing` and `reviewing -> complete | partial | failed`. Terminal sessions (`complete`, `partial`, `failed`) cannot be rewritten or transitioned. Terminal composition validation rejects invalid counts or terminal reasons upon transitioning to terminal states.
- `Cancellation monotonicity`: `cancel_requested` may only transition from `0` to `1` and can never be reset to `0`. Triggers do not autonomously compose verdicts or dispatch roles.
- `Record immutability`: `snapshots`, `evidence_units`, `findings`, and `finding_basis_refs` reject all `UPDATE` and `DELETE` operations after initial insertion. Core identity, project, idempotency, hash, and creation timestamp fields of `sessions` and `role_runs` are immutable after insert.
- `Citation integrity`: `finding_basis_refs` requires referenced evidence units to belong to the exact same snapshot as the finding's session on both insert and update. Cross-snapshot citations are aborted. References to non-existent findings or evidence units are rejected by existing foreign key constraints.

### SQLite atomic submission and idempotency

Package `internal/storage/sqlite` provides verified atomic submission and idempotency handling via `submit.go`:

- `Request hash v1`: deterministic SHA-256 over unambiguous length-prefixed bytes:
  `normalization_version=1\n<byte_len(project_id)>:<project_id>\n<byte_len(title)>:<title>\n<byte_len(content)>:<content>`.
  Strict UTF-8 validation rejecting invalid sequences before hashing; no trimming; no Unicode normalization; exact submitted line endings preserved.
- `Atomic submission`: executed entirely within one `withImmediate` transaction. Checks `(project_id, idempotency_key)`. Absent key atomically creates the immutable snapshot, ordered evidence units, queued session (`cancel_requested = 0`, `completed_role_count = 0`, `incomplete_role_count = 4`, null timing/claim/terminal fields), and exactly the four canonical role runs in `domain.Roles` order (`requirements`, `architecture`, `qa`, `security`) in `pending` status.
- `Idempotency replay`: matching `(project_id, idempotency_key)` with identical request hash returns the original session identity (`Replay: true`) without creating new rows.
- `Idempotency conflict`: matching `(project_id, idempotency_key)` with mismatched request hash returns a typed `IdempotencyConflictError` matching sentinel `ErrIdempotencyConflict`.
- `Concurrency safety`: insert races on `(project_id, idempotency_key)` are resolved authoritatively by rereading the winning committed row and verifying request hash equivalence.

### SQLite deterministic read models

Package `internal/storage/sqlite` provides verified read-only status and terminal report models via `read.go`:

- `Read-only pool execution`: status, report, and snapshot queries execute exclusively through the dedicated `mode=ro` pool via `SELECT` statements; no write transaction, mutation, provider, composer, or worker invocation is allowed.
- `Scoped typed errors`: missing sessions return a typed `SessionNotFoundError` matching `ErrNotFound` and `ErrSessionNotFound`, preserving `ProjectID` scoping for 404 mapping; report requests on non-terminal sessions (`queued`, `reviewing`) fail closed with `SessionNotTerminalError` matching `ErrNotTerminal` and `ErrNotFinished`.
- `Deterministic reconstruction`: role ordering is reconstructed strictly from canonical `domain.Roles` (`requirements`, `architecture`, `qa`, `security`) rather than database row order; findings are sorted by deterministic total order (`severity rank`, `role rank`, `primary basis_ref`, `category`, `finding id`); finding basis references are ordered strictly by positive ordinal `1..5`.
- `Persisted field preservation`: `cancel_requested`, `status`, `terminal_reason`, `completed_role_count`, and `incomplete_role_count` are returned exactly as committed in storage without verdict composition or re-derivation from live flags.
- `Filtered role findings`: failed and interrupted roles contribute zero findings to terminal reports.
- `Snapshot hash integrity`: `ReadSnapshot` reconstructs ordered evidence units, recomputes canonical hash via `evidence.Freeze`, and fails closed with `ErrSnapshotCorrupted` on mismatch.
- `Strict UTC timestamp and enum decoding`: all persisted timestamps require UTC RFC3339Nano representation ending in 'Z' with length >= 20; malformed timestamps, enums, or count combinations return typed errors without silent fallback.

## Not built yet

API endpoints, authentication and authorization, the bounded
two-at-a-time scheduler, `dispatch_cutoff_at` / `call_timeout` /
`hard_deadline_at` enforcement, cancellation at the dispatch boundary under
concurrency, startup recovery sweep, the real
provider adapter, and the supervised worker restart policy.

## Known implementation gaps against the canonical flow

| Gap | Current code | Required change |
|---|---|---|
| Format-repair call fails in transport | Reports the transport failure | Keep this behavior; persist `transport` or `timeout` |
| Prompt token count | Rough four-characters-per-token estimate | Use the configured model's tokenizer |
| Finding field set | Requires `severity` and `category` | Freeze the complete output schema before the real adapter |

## Evidence status

Everything in this milestone is proven by the Go test suite through the fake
provider. No real provider has been called, so nothing here is evidence about
live model behaviour.