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
- Atomic execution of each migration's SQL and metadata recording (`schema_migrations` tracking table with numeric version, exact name, checksum, and UTC applied time in fixed-width RFC3339-compatible format with exactly nine fractional digits).
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
- `Snapshot hash verification`: recomputes canonical snapshot hash via `evidence.Freeze(snapshot.ID, snapshot.Units)` and rejects any missing, mismatched, or corrupted snapshot hash before entering transactions or evaluating idempotency replay.
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
- `Strict UTC timestamp and enum decoding`: persisted timestamps are written in fixed-width RFC3339-compatible UTC representation with exactly nine fractional digits and a `Z` suffix; existing whole-second and nanosecond RFC3339 timestamps remain readable; malformed timestamps, enums, or count combinations return typed errors without silent fallback.

### SQLite FIFO session claim and timing policy

Package `internal/storage/sqlite` provides verified atomic single-session FIFO claim and timing policy via `claim.go`:

- `Validated timing policy`: requires positive, bounded durations for `DispatchCutoff`, `CallTimeout`, and `SessionHardDeadline` satisfying `SessionHardDeadline >= DispatchCutoff + CallTimeout`; rejects zero, negative, duration overflow, and impossible relationships; strictly configuration-driven without hardcoded production defaults.
- `Single active session constraint`: enforces at most one session in `reviewing` status across the database; returning a typed no-work result (`Claimed: false`, `NoWorkReason: NoWorkActiveReviewing`) without error when another session is reviewing.
- `Authoritative FIFO ordering`: selects the oldest queued session by `created_at ASC, id ASC`; includes sessions with `cancel_requested = 1`.
- `Atomic timing mutation`: executes within a dedicated single `withImmediate` transaction transitioning exactly one session from `queued` to `reviewing`, setting `claimed_at = now`, `dispatch_cutoff_at = now + DispatchCutoff`, and `hard_deadline_at = now + SessionHardDeadline` formatted in UTC RFC3339Nano with 'Z' suffix.
- `Guarded update and no-work handling`: returns a typed no-work result when no queued session exists (`NoWorkNoQueuedSession`) or when the guarded update affects zero rows (`NoWorkGuardConflict`), leaving database state untouched.
- `Non-claimable states`: rejects and never transitions terminal (`complete`, `partial`, `failed`), already reviewing, or malformed sessions.
- `Role run preservation`: all four canonical role runs remain in `pending` status; no roles set to in-flight; no composer, provider, worker, or API invocation.
- `Concurrency protection`: concurrent claim attempts serialize through SQLite immediate transaction locks, guaranteeing exactly one winning claimer.

### SQLite idempotent cancellation request

Package `internal/storage/sqlite` provides verified atomic, idempotent cancellation request mutation via `cancel.go`:

- `Immediate transaction execution`: executes within a dedicated single `withImmediate` transaction acquiring `BEGIN IMMEDIATE`.
- `Atomic mutation on queued and reviewing sessions`: transitions `cancel_requested` from 0 to 1 atomically for sessions in `queued` or `reviewing` status, returning `Effective: true` and the committed session read model.
- `Strict idempotency`: repeating cancellation on an already requested session (1 -> 1) succeeds, returning `Effective: false` without updating fields, timestamps, or role rows.
- `Terminal session no-op`: terminal sessions (`complete`, `partial`, `failed`) are never mutated, reopened, or updated; returning `Effective: false`, `AlreadyTerminal: true`, `NoOp: true`, and the committed session read model.
- `Typed session not-found`: unknown session IDs and project ID mismatches fail closed with typed `SessionNotFoundError` matching `ErrNotFound` / `ErrSessionNotFound`.
- `Scope preservation`: leaves status, role runs, terminal reason, role counts, deadlines, timestamps, and findings completely unchanged; no worker, dispatch, composer, or provider invocation.
- `Concurrency safety`: concurrent cancellation attempts serialize through SQLite immediate transaction locks, ensuring exactly one winning attempt reports `Effective: true` while all concurrent callers receive consistent committed state.

### SQLite guarded role dispatch reservation

Package `internal/storage/sqlite` provides verified atomic, guarded pending-role reservation via `dispatch.go`:

- `Reviewing-only execution`: operates only on sessions in `reviewing` status; non-reviewing sessions (queued or terminal) return a typed no-work result (`DispatchNoWorkNotReviewing`) without modifying database state.
- `Cancellation guard`: refuses reservation when `cancel_requested = 1`, returning typed no-work (`DispatchNoWorkCancelled`).
- `Cutoff boundary enforcement`: refuses reservation when current time `now >= dispatch_cutoff_at`, returning typed no-work (`DispatchNoWorkCutoff`).
- `Persisted in-flight capacity bound`: counts persisted `in_flight` roles inside the same immediate transaction; never reserves when the count is already `MAX_IN_FLIGHT = 2`, returning typed no-work (`DispatchNoWorkCapacityFull`).
- `Canonical role ordering`: selects the next pending role deterministically by canonical role order (`requirements`, `architecture`, `qa`, `security`) regardless of physical row order in storage. Returns typed no-work (`DispatchNoWorkNoPendingRole`) when no pending roles remain.
- `Atomic transition and call metadata`: changes exactly one pending role to `in_flight`, setting `started_at` in UTC RFC3339Nano with 'Z' suffix and initial call metadata (`call_count = 0`).
- `Authoritative UPDATE predicate rechecking`: rechecks session reviewing status, un-cancelled flag, cutoff boundary, and in-flight capacity `< 2` within the authoritative `UPDATE` statement's `WHERE` clause; fast-path reads are not treated as authority. If concurrent mutation causes 0 rows affected, returns typed no-work (`DispatchNoWorkGuardConflict`).
- `Cancellation and cutoff race safety`: a concurrent cancellation committing before the guarded update prevents reservation; a reservation committing first remains legitimately in-flight while cancellation follows.
- `Strict persistence scope`: leaves session status, cancel flag, and other role rows completely untouched; never calls providers, starts goroutines, or holds transactions over external work.

### SQLite compare-and-set role publication

Package `internal/storage/sqlite` provides verified atomic, compare-and-set role publication via `publish.go`:

- `Immediate transaction execution`: operates within a dedicated immediate transaction using `withImmediate`, serializing concurrent publishers on the writer pool.
- `Compare-and-set guard`: requires the target role run to currently be in `in_flight` status, verifying session ID and role-run ID inside the transaction; rechecking in the authoritative `UPDATE` statement's `WHERE` clause.
- `Atomic success publication`: atomically inserts validated findings and basis references, and transitions the role run from `in_flight` to `complete`, recording call count (1..2) and `completed_at` in UTC RFC3339Nano with 'Z' suffix.
- `Atomic failure publication`: atomically transitions the role run from `in_flight` to `failed`, recording canonical error category, error message, call count (0..2), and `completed_at`; inserts zero findings or basis references.
- `Late, duplicate, and stale rejection`: returns a typed `PublicationConflictError` matching `ErrPublicationConflict` when encountering non-in-flight roles (stale `pending`, duplicate or late `complete`/`failed`/`interrupted`); makes zero modifications.
- `Terminal role state immutability`: terminal role states (`complete`, `failed`, `interrupted`) are never overwritten or transitioned.
- `Input validation and ordering enforcement`: strictly validates findings and basis references before and during execution; enforces at most 15 findings, non-empty and unique finding IDs, canonical severities, non-empty categories, bounded issue/recommendation (1..1000 characters), 1..5 basis refs per finding, unique basis refs within a finding, and 1-indexed basis ordinals.
- `Snapshot citation integrity`: verifies all cited basis references exist in the session's frozen snapshot as evidence units, rejecting unknown citations and cross-snapshot citations.
- `All-or-nothing transaction rollback`: validation failures, constraint violations, or commit errors abort and roll back the transaction completely, preserving the role in `in_flight` and inserting no findings or references.
- `Strict persistence scope and field preservation`: preserves snapshot rows and all session fields (`status`, `cancel_requested`, `completed_role_count`, `incomplete_role_count`, `terminal_reason`, timestamps) without modification; never composes session verdicts or invokes providers, workers, HTTP, or composer logic.

### SQLite control sweeps and restart recovery

Package `internal/storage/sqlite` provides verified atomic, idempotent control sweeps and restart recovery via `sweep.go`:

- `Immediate transaction execution`: all sweeps execute within dedicated immediate transactions using `withImmediate`, serializing mutations and honoring bounded busy retries.
- `Authoritative UPDATE predicates`: all updates re-verify session status and timing boundaries directly in SQL `WHERE` clauses with `EXISTS` subqueries, ensuring state guards are respected under concurrent modifications.
- `Cancellation sweep`: scoped to a reviewing session; pending roles atomically transition to `interrupted` with cause `user_cancelled` and record `completed_at` only when `cancel_requested = 1` (non-cancelled sessions with `cancel_requested = 0` are safe no-ops); in-flight and terminal roles remain untouched; queued and terminal sessions are safe no-ops; rechecks `status='reviewing' AND cancel_requested = 1` in SQL UPDATE EXISTS subquery; fully idempotent and concurrency-safe.
- `Cutoff sweep`: scoped to a reviewing session and injected `now`; pending roles atomically transition to `interrupted` with cause `deadline_cutoff` when `now >= dispatch_cutoff_at`; before cutoff is a no-op; in-flight and terminal roles remain untouched; queued and terminal sessions are safe no-ops.
- `Hard-deadline sweep`: scoped to a reviewing session and injected `now`; pending roles atomically transition to `interrupted` with cause `deadline_cutoff` only when `now >= hard_deadline_at` (before hard deadline is a safe no-op leaving pending roles unchanged); rechecks `status='reviewing' AND hard_deadline_at IS NOT NULL AND ? >= hard_deadline_at` in SQL UPDATE EXISTS subquery; in-flight roles are not rewritten in storage; exposes the exact set of in-flight role IDs and canonical role metadata for caller local cancellation and P9 timeout publication.
- `Restart recovery sweep`: operates on stale reviewing sessions selected by an explicit cutoff timestamp (`claimed_at <= cutoff`); in-flight roles transition to `interrupted` with cause `process_restart`; pending roles transition to `interrupted/user_cancelled` when session `cancel_requested = 1` or `interrupted/process_restart` otherwise; completed, failed, and already interrupted roles remain unchanged; queued sessions are untouched; supports both database-wide multi-session sweeps and single-session recovery.
- `Scoped error handling`: unknown session IDs and project mismatches fail closed with typed `SessionNotFoundError` matching `ErrNotFound` / `ErrSessionNotFound`.
- `Transaction rollback`: validation failures, constraint violations, and injected commit errors roll back completely, preserving pending and in-flight states.
- `Strict persistence scope`: preserves snapshots, evidence units, findings, basis citations, session status, counts, deadlines, and role call counts without modification; never invokes providers, starts worker loops or goroutines, or executes composer verdict logic.

### SQLite transactional session composition and terminal reports

Package `internal/storage/sqlite` provides verified atomic, idempotent transactional session composition via `compose.go`:

- `Immediate transaction execution`: composes a terminal session within a single dedicated immediate transaction using `withImmediate`, serializing mutations on the writer pool and honoring bounded busy retries.
- `Non-terminal refusal`: reads all four committed role runs, findings, and citations inside the transaction; refuses composition unless all four roles are terminal and zero in-flight or pending roles remain; returns typed `SessionNotReadyError` matching `ErrSessionNotReady` and makes zero writes. Queued sessions are refused without modification.
- `Canonical verdict rules and reason precedence`: applies frozen `review.Compose` rules deriving status (`complete`, `partial`, `failed`) and terminal reason (`all_roles_complete`, `user_cancelled`, `process_restart`, `deadline_cutoff`, `role_failures`) strictly from committed role runs.
- `Deterministic total ordering`: report findings and citations follow the deterministic order (`severity rank, role rank, primary basis_ref, category, finding id`) and ordinal ascending links. Failed and interrupted roles contribute zero findings.
- `Compare-and-set terminal transition`: atomically updates `sessions` requiring status to remain `reviewing`, recording status, terminal reason, completed/incomplete counts, and UTC RFC3339Nano `terminal_at`.
- `Idempotency and concurrency safety`: duplicate composition is an idempotent read of the committed terminal state returning `AlreadyTerminal: true` without mutating data or timestamps. Stale concurrent composers lose compare-and-set without overwriting terminal state.
- `All-or-nothing transaction rollback`: injected commit errors or malformed persisted role data abort and roll back completely; no partial terminal state is persisted.
- `Read-only terminal report access`: `ReadReport` and `ReadTerminalReport` execute via the read-only pool, returning the exact deterministic terminal report bytes once terminal, and failing closed with typed `SessionNotTerminalError` matching `ErrNotTerminal` when non-terminal.
- `Strict persistence scope and row preservation`: snapshots, evidence units, findings, basis references, role rows, cancellation flag, claim/cutoff/deadline timestamps, and role call counts are strictly immutable and left untouched. No provider calls or worker loops are invoked.

### SQLite end-to-end persistence and concurrency proof

Package `internal/storage/sqlite` provides verified end-to-end integration and concurrency proofs across the complete persistence lifecycle via `persistence_e2e_test.go`:

- `Full lifecycle integration`: submit creates immutable snapshots, ordered evidence units, queued sessions, and four pending roles; FIFO claim assigns reviewing status with exact timing fields; guarded dispatch reserves at most two in-flight roles respecting canonical order; compare-and-set publication persists findings and citations atomically or records failure metadata; cancellation, cutoff, deadline, and restart sweeps transition pending and in-flight roles deterministically; transactional composition finalizes sessions and persists deterministic reports.
- `Strict immutability and table snapshots`: snapshots, evidence units, findings, and citation links are strictly immutable and reject update/delete; read-only pool queries (`ReadStatus`, `ReadReport`, `ReadSnapshot`) leave all database tables completely unchanged as verified by before/after database state snapshots.
- `Barrier-synchronized concurrency`: concurrent claimers, dispatchers, publishers, sweepers, and composers resolve through SQLite immediate transaction serialization and compare-and-set guards, ensuring exactly one winning actor where required, no lost updates, and idempotent secondary operations without data divergence.
- `Failure and boundary handling`: busy/locked retry exhaustion returns typed `PersistenceUnavailable` at the configured bound; non-terminal report queries fail closed with `SessionNotTerminalError`; tampered migration checksums prevent the store from opening.
- `Forbidden scope verification`: guarantees zero worker loops, goroutine-held SQLite transactions, provider invocations, network calls, HTTP/auth routes, or UI code in the persistence layer.

### Crash-safe worker process lock

Package `internal/worker` provides the verified OS-backed exclusive process lock foundation via `AcquireProcessLock` and `ProcessLock.Release`:

- `Validated path semantics`: requires a non-empty, clean filesystem path with an existing directory parent; rejects empty, whitespace, and unusable parent paths with inspectable typed `PathError`, sentinel `ErrEmptyPath`, and sentinel `ErrInvalidParentDir` without silently defaulting path locations.
- `OS-backed handle ownership`: ownership is directly tied to the process-level OS file handle (Windows `CreateFile` with `dwShareMode = 0` and `LockFileEx`, Unix `flock` with `LOCK_EX | LOCK_NB`); never represented by a PID file, database row, or stale marker requiring manual cleanup.
- `Prompt contention rejection`: a second acquisition attempt against an already owned path fails promptly with typed sentinel `ErrAlreadyOwned` without blocking indefinitely, waiting, or touching storage.
- `Automatic OS reclamation and idempotent release`: `Release` is safe and idempotent, closing the underlying OS handle and removing no unrelated files; normal process termination and abnormal termination/crashes automatically release the lock at the kernel level, leaving the path immediately acquirable.
- `Context cancellation`: cancellation before or during acquisition returns context cancellation promptly without leaking live locks or helper goroutines.
- `Strict persistence isolation`: lock acquisition and release never open, query, or mutate SpecCouncil SQLite database files or schema migrations, and package `internal/worker` maintains zero imports of storage packages.

### Worker restart recovery and single-session claim

Package `internal/worker` provides the verified worker execution seam via `RunOnce`:

- `Validated configuration`: validates non-empty clean lock path, non-nil store, non-zero explicit restart cutoff, and valid timing policy (`SessionHardDeadline >= DispatchCutoff + CallTimeout`) before attempting lock acquisition or touching storage.
- `Exclusive process ownership`: acquires OS-backed `ProcessLock` and returns typed sentinel `ErrAlreadyOwned` promptly upon contention without opening or altering database rows.
- `Deferred release and error preservation`: defers lock release across all paths, preserving primary operation errors over release errors via `errors.Join`, and surfacing release errors when operations otherwise succeed without hiding successful claim outcomes.
- `Recovery before claim ordering`: executes exactly one `SweepRestartRecovery` using the caller's explicit cutoff timestamp before attempting any claim; fails closed and never claims if recovery encounters persistence errors.
- `Single FIFO session claim`: invokes `ClaimSession` exactly once with caller-provided validated timing policy, claiming at most one queued session via deterministic FIFO order (`created_at ASC, id ASC`); returns authoritative claim or typed no-work result without error.
- `No-work success semantics`: returns non-nil `RunResult` and nil error when no queued session exists, treating empty queue as a successful run.
- `Zero forbidden side-effects`: strictly runs synchronously with zero background goroutines, provider invocations, role dispatch loops, composer executions, HTTP/auth routes, or transactions held across lock/recovery/claim boundaries.
- `Persistence package isolation`: orchestrates operations via generic interfaces `SessionStore` and `TimingPolicyValidator` satisfied directly by `*sqlite.Store` without `internal/worker` importing persistence packages.

### Worker serialized guarded dispatch loop

Package `internal/worker` provides the verified serialized guarded dispatch loop via `Dispatcher` (`NewDispatcher`, `Dispatcher.Run`, `Dispatcher.Step`, `Dispatcher.Wake`, `Dispatcher.Tick`, `Dispatcher.NotifyRoleCompleted`, and `Dispatcher.Stop`):

- `Reviewing session scoping`: validates non-empty session ID, non-nil store, and positive tick interval; operates exclusively on the claimed reviewing session without claiming, recovering, or accepting queued sessions.
- `Single serialized owner`: exactly one owner loop executes reservations and evaluations; concurrent wake and tick signals are coalesced via non-blocking buffered signaling, guaranteeing that wake/tick signals never invoke `ReservePendingRole` directly and concurrent callers never execute overlapping reservation attempts.
- `Fast-path evaluation ordering`: checks cancellation (`cancel_requested = 1`), dispatch cutoff (`now >= dispatch_cutoff_at`), local execution capacity (`< MAX_IN_FLIGHT = 2`), and canonical next pending role in strict order before reservation, falling back to authoritative SQLite transaction results without turning no-work reasons into generic errors.
- `Persisted capacity authority`: enforces maximum 2 in-flight roles based on authoritative persisted state; local capacity tracking guards scheduling but never overrides or bypasses SQLite authority.
- `Post-commit callback execution`: executes injected `OnReserved` callback only after `ReservePendingRole` commits successfully inside SQLite; callback errors or failures cannot roll back or corrupt committed in-flight reservations; callback execution never holds database locks or transactions.
- `Completion notification wake`: `NotifyRoleCompleted` decrements local in-flight tracking and wakes the owner loop to immediately evaluate and reserve the next pending canonical role.
- `Clean shutdown and resource release`: stops promptly on context cancellation or explicit `Stop()` without leaking timers, channels, or goroutines; returns typed persistence errors unchanged.
- `Persistence package isolation`: communicates with storage via generic interfaces `RoleReservationStore` and `SessionStatusReader` satisfied by `*sqlite.Store` without `internal/worker` importing persistence packages.
- `Strict worker phase scope`: strictly reserves pending roles without invoking providers, composing reports, publishing success or failure, or mutating session records outside authoritative dispatch reservation.

### Worker deadline-bounded role execution

Package `internal/worker` and `internal/review` provide the verified deadline-bounded role execution seam via `worker.Execute` and `review.RunRole`:

- `Validated input configuration`: validates canonical role identity, non-empty frozen snapshot, non-nil provider, positive per-attempt `CallTimeout`, and non-zero `HardDeadlineAt`. Rejects empty snapshots, invalid roles, and missing inputs fail-closed.
- `Prompt token budget`: evaluates token budget before the first provider attempt. If budget is exceeded, returns `failed` with category `budget_exhausted` making zero provider calls.
- `Hard deadline guard`: checks whether the hard deadline has already elapsed before initiating any provider attempt. If expired, returns `failed` with category `timeout` making zero provider calls.
- `Per-attempt deadline context`: bounds each provider attempt context to `min(now + CallTimeout, HardDeadlineAt)`. Context cancellation cleans up blocked provider calls promptly without leaking goroutines.
- `Canonical two-call budget`: enforces a maximum of two provider calls per role execution with strict XOR semantics between transport retry and format repair (`initial -> transport_retry` XOR `initial -> format_repair`). No execution path permits a third call.
- `Retry and backoff semantics`: only typed retryable transport failures permit a transport retry attempt; fatal provider errors fail immediately with zero retry. Backoff between attempts is bounded by configured minimum and maximum limits, and aborts immediately upon context cancellation or hard deadline expiry.
- `Strict validation preservation`: validates provider output strictly against schema and evidence references in the immutable frozen snapshot; malformed provider output never becomes a finding.
- `Strict worker phase isolation`: strictly executes role provider attempts and returns terminal `review.RoleOutcome` without touching SQLite transactions, publication, report composition, dispatch loops, or secondary roles.

### Worker publication, composition, and supervisor boundary

Package `internal/worker` provides the verified publication, composition, and supervisor boundary integration via `SuperviseSession` and `Supervise`:

- `Claimed-session boundary`: operates strictly on a single already-claimed `reviewing` session; never claims another session, executes restart recovery, or acquires redundant process locks inside the session supervisor.
- `Reservation to execution`: each committed role reservation from the serialized dispatcher is executed exactly once via `worker.Execute` with the session's immutable frozen snapshot, timing policy, call timeout, and hard deadline; no provider call is made before reservation commit or while SQLite transactions are open.
- `Compare-and-set publication`: publishes terminal role outcomes atomically via narrow persistence interfaces (`PublishRoleSuccess`, `PublishRoleFailure`); enforces `in_flight -> complete` (with validated findings and citations) or `in_flight -> failed` (with canonical error metadata); failed roles contribute zero findings.
- `Publication ordering and CAS conflict handling`: notifies the serialized dispatcher (`NotifyRoleCompleted`) strictly after publication returns; typed publication conflicts (`PublicationConflictError`) make no second provider call, perform no secondary writes, and surface cleanly without rolling back committed state.
- `Cancellation and deadline bounds`: respects context cancellation, dispatch cutoff, and session hard deadlines; stops new reservations promptly, drains in-flight roles within their deadline contexts, and publishes canonical timeout/failure outcomes without inventing non-canonical statuses.
- `Transactional session composition`: triggers composition exactly once after all four roles are terminal and persisted `in_flight == 0`; retrieves deterministic report and terminal session models via `ComposeSession` without modifying reports or deriving verdicts in worker code; preserves idempotency on duplicate composition.
- `Supervisor boundary and persistence failure`: surfaces persistence errors after bounded SQLite retry exhaustion directly to the outer supervisor caller as a truthful non-nil error requiring process restart; does not implement a daemon, automatic restart loop, broker, lease, or heartbeat.
- `Resource and lock cleanup`: guarantees deferred release of OS-backed process locks, stop/drain of dispatchers, and termination of all worker goroutines across success, no-work, cancellation, publication conflicts, and persistence failures.
- `Persistence package isolation`: communicates with persistence strictly through narrow interfaces (`SessionPublisher`, `SessionComposer`, `RoleReservationStore`, `SessionStatusReader`, `SnapshotReader`) satisfied by `*sqlite.Store` without importing `internal/storage/sqlite` in worker production code.

### Worker process supervisor boundary

Package `internal/worker` provides the verified one-attempt process supervisor boundary via `RunAttempt`, `ProcessSupervisor`, `RunOnceAttempt`, and `SuperviseAttempt`:

- `One-attempt process boundary`: executes exactly one worker attempt under a caller-owned context, returning its truthful result and error without hiding or rewriting typed errors.
- `Context ownership`: the caller owns the context; pre-canceled contexts reject execution promptly with the context error before starting work or invoking attempts.
- `Cooperative shutdown`: cancellation during execution is observable by the attempt via `ctx.Done()` or `ctx.Err()`; the supervisor waits synchronously for the attempt to return before returning itself.
- `No automatic restart`: does not implement a daemon loop, retry loop, or automatic process restart; restart decisions belong exclusively to the outer deployment or service manager.
- `Outcome and error preservation`: preserves successful results, no-work results, typed sentinels (`ErrAlreadyOwned`, `context.Canceled`, `context.DeadlineExceeded`), and persistence errors without alteration.
- `Underlying layer ownership`: process locks, restart recovery sweeps, FIFO claims, role dispatching, provider execution, publication, and composition remain exclusively owned by the released W1–W5 worker layers; W6 does not acquire secondary locks or duplicate worker operations.
- `Deferred scope`: HTTP endpoints, authentication and authorization, real provider networking, and deployment restart policies remain later work.

### HTTP API composition boundary

Package `internal/api` provides the verified real `net/http` composition root and routing boundary via `NewServer` and `Server.Handler`:

- `Real net/http composition root and handler`: validates required dependencies (`Store`, `Authenticator`, `Authorizer`) at construction; exposes an `http.Handler` testable via `httptest` without starting a network listener.
- `Protected route authentication and project authorization boundaries`: protected endpoints (`/v1/projects/{project_id}/reviews...`) authenticate before accessing persistence; pass authenticated `Identity` to project authorization; reject unauthenticated requests with 401.
- `404 anti-enumeration behavior`: inaccessible or foreign project requests return HTTP 404 rather than 403, preventing enumeration of project existence.
- `Scoped submit/status/report/cancel persistence calls`: delegates review operations exclusively to scoped persistence methods (`Submit`, `ReadStatusScoped`, `ReadReportScoped`, `RequestCancellationScoped`); preserves exact submitted title/content bytes and route project ID.
- `Non-terminal report -> 409 not_finished`: report reads on non-terminal sessions fail closed with HTTP 409 Conflict and error code `not_finished`.
- `Cancellation request semantics`: calling cancel transitions `cancel_requested` from 0 to 1 returning HTTP 202 Accepted (`effective: true`); repeated or terminal cancellations return HTTP 200 OK (`effective: false`); cancellation never mutates role rows or composes reports in the API layer.
- `Health endpoint`: unauthenticated `GET /healthz` returns `{"status":"ok"}` without touching storage.
- `Strict request validation and error mapping`: enforces valid UTF-8, strict JSON decoding (disallowing unknown fields and trailing data), required field presence, and unsupported Content-Type rejection; maps persistence unavailable errors to 503 without leaking DSNs, SQL, or internal details.
- `No worker or provider invocation from HTTP`: the API layer never claims work, dispatches roles, calls providers, or composes reports directly.
- `No concrete auth/account system yet`: authentication and authorization interfaces accept injectable adapters; user accounts, passwords, JWTs, OAuth, and authorization storage are deferred.

## Not built yet

The real provider adapter, concrete auth/account persistence, and deployment
restart policy.

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