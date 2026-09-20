# R2 — Fix Sweep Predicates (Cancellation + Hard Deadline)

## Mission

Repair only the two control-sweep predicate defects found in the P0–P12 audit.
The implementation base is clean commit `ccdecaa` (R1 complete); execute from
latest master at the packet-release commit that adds this packet only. Do not
fix findings triggers or snapshot hash validation in this slice.

## Defects

1. `SweepCancellation` does not require `cancel_requested = 1`. It unconditionally
   interrupts pending roles on any reviewing session, even when the session was
   never cancelled.
2. `SweepHardDeadline` never verifies `now >= hard_deadline_at`. It interrupts
   pending roles immediately even when the hard deadline is far in the future.

Both defects allow premature, irreversible role interruption that violates the
canonical flow control paths.

## Allowed files

```text
internal/storage/sqlite/sweep.go
internal/storage/sqlite/sweep_predicate_test.go
docs/ENGINE-CONTRACT.md
```

Do not modify migrations, submit.go, publish.go, compose.go, or any other file
in this slice. Do not modify `dispatch.go`/`claim.go`/`migrate.go` timestamp
logic already fixed in R1.

## Contract

In `internal/storage/sqlite/sweep.go`:

- `SweepCancellationScoped`:
  - SELECT must fetch `cancel_requested` alongside `project_id, status`.
  - If `cancel_requested != 1`, return a NoOp result (InterruptedCount=0, NoOp=true, InFlightCount preserved) without touching `role_runs`.
  - The authoritative UPDATE must recheck cancellation in SQL: add `AND cancel_requested = 1` to the EXISTS subquery that guards `status='reviewing'`.
  - Queued/terminal sessions remain NoOp as before.
  - Existing hook behavior (`sweepBeforeUpdateHook` / `sweepBeforeCommitHook`) preserved.

- `SweepHardDeadline*` (all entry points: SweepHardDeadline, SweepHardDeadlineWithNow, SweepHardDeadlineScoped):
  - SELECT must fetch `hard_deadline_at` alongside `project_id, status`.
  - If `hard_deadline_at IS NULL` or `now < hard_deadline_at`, return NoOp without modifying `role_runs`. InFlight IDs/roles may still be exposed as before when NoOp due to time.
  - The authoritative UPDATE must recheck the deadline in SQL: add `AND hard_deadline_at IS NOT NULL AND ? >= hard_deadline_at` (binding `nowStr`) to the EXISTS subquery.
  - Use the existing fixed-width `formatUTCTimestamp` for `nowStr`; do not reintroduce variable-width formatting.

Preserve project scoping, context cancellation, retry policy, and rollback semantics.
No provider calls, goroutines, or schema changes.

## Required regression proof

Add `internal/storage/sqlite/sweep_predicate_test.go` with deterministic tests using real temporary stores and injected clocks (no sleeps, use barriers where concurrency is exercised):

1. SweepCancellation on a reviewing session with `cancel_requested=0` leaves all 4 roles pending (NoOp, 0 interrupted).
2. SweepCancellation on a reviewing session with `cancel_requested=1` interrupts pending roles with `cause=user_cancelled` and `completed_at = nowStr` (fixed-width, UTC, Z), in-flight/terminal roles unchanged.
3. SweepCancellation idempotency: second call on already-swept session is NoOp.
4. SweepCancellation with concurrent cancel commit: barrier test proves sweep rechecks authoritative predicate (exactly one winner behavior).
5. SweepHardDeadline before `hard_deadline_at` is NoOp (0 interrupted, pending roles remain pending), even with pending roles present.
6. SweepHardDeadline at exactly `hard_deadline_at` interrupts pending roles with `cause=deadline_cutoff`.
7. SweepHardDeadline after `hard_deadline_at` interrupts pending roles and exposes in-flight IDs in canonical order.
8. SweepHardDeadline does not rewrite in-flight roles; they remain `in_flight` and publishable via P9 thereafter.
9. Both sweeps remain NoOp on queued and terminal sessions.
10. Existing whole-second and subsecond timestamps remain correctly ordered (50ms regression still passes).

Do not weaken schema checks or modify existing test helpers to hide failures.

## Verification

```bash
gofmt -w internal/storage/sqlite/sweep.go internal/storage/sqlite/sweep_predicate_test.go
test -z "$(gofmt -l .)"
go test -v ./internal/storage/sqlite -run TestSweep
go test -v ./internal/storage/sqlite -run TestTimestamp
go test ./internal/storage/sqlite -count=1
go test ./internal/storage/sqlite -count=10
go test ./... -count=1
go vet ./...
git diff --check
```

Also verify no forbidden file was touched:

```bash
git diff --name-only | grep -v -E '^(internal/storage/sqlite/sweep\.go|internal/storage/sqlite/sweep_predicate_test\.go|docs/ENGINE-CONTRACT\.md|docs/cline/persistence/R2-SWEEP-PREDICATES\.md)$' && echo "FORBIDDEN FILES MODIFIED" || echo "scope clean"
```

Do not run or claim `go test -race` unless available.

Commit exactly:

```text
Fix sweep cancellation and hard-deadline predicates
```

Do not push. Do not fix findings triggers or snapshot hash. Any unrelated changed file or unmet regression means BLOCKED and no commit.

## Report

```text
R2 RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests:
- ...
Full gate:
- ...
Acceptance evidence:
- cancel predicate:
- hard-deadline predicate:
- SQL authoritative recheck:
- idempotency/concurrency:
- queued/terminal NoOp:
- in-flight preservation:
- forbidden scope:
Git status:
<exact output>
```
