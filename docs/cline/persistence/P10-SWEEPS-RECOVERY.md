# P10 — Control Sweeps and Restart Recovery

## Mission

Implement only the transactional, idempotent control sweeps required by the
canonical flow. Start from a clean tree at P9 acceptance `b4b3dfe`.

## Scope

Allowed paths only:

```text
internal/storage/sqlite/sweep.go
internal/storage/sqlite/sweep_test.go
docs/ENGINE-CONTRACT.md
```

## Contract

Use the existing `withImmediate` transaction boundary and typed read models.
No provider call, goroutine, worker loop, or context cancellation of remote work
belongs in this slice.

Implement package-internal operations for:

1. **Cancellation sweep**
   - scoped to one reviewing session;
   - pending roles become `interrupted` with cause `user_cancelled`;
   - in-flight roles remain unchanged;
   - completed/failed/interrupted roles remain unchanged;
   - idempotent and safe under concurrent invocation.

2. **Cutoff sweep**
   - scoped to one reviewing session and an injected `now`;
   - pending roles become `interrupted` with cause `deadline_cutoff` when the
     dispatch cutoff has been reached;
   - in-flight roles remain unchanged;
   - before cutoff it is a no-op; terminal roles remain unchanged;
   - idempotent and safe under concurrent invocation.

3. **Hard-deadline sweep**
   - scoped to one reviewing session and an injected `now`;
   - pending roles become `interrupted` with cause `deadline_cutoff`;
   - in-flight roles are not rewritten by this sweep; expose the exact set of
     in-flight role IDs for the caller to cancel locally and publish timeout
     through P9;
   - no provider context cancellation occurs here.

4. **Restart recovery sweep**
   - operates on stale reviewing sessions selected by an explicit cutoff;
   - terminal sessions are unchanged;
   - in-flight roles become `interrupted/process_restart`;
   - pending roles become `interrupted/user_cancelled` when the session has
     `cancel_requested = 1`, otherwise `interrupted/process_restart`;
   - queued sessions are untouched;
   - never replays provider work.

All mutations must use authoritative predicates, preserve immutable snapshot and
session identity fields, preserve completed roles, and leave role call metadata
unchanged unless the contract explicitly requires interruption fields. Unknown
or project-mismatched IDs return the existing typed not-found error. Invalid
states and injected commit failures roll back completely.

## Tests and verification

Use real temporary databases, P8 reservations, and P9 publication fixtures.
Use barriers, injected timestamps, and repeated invocations; do not use sleeps.
Cover cancellation, cutoff boundary, hard-deadline pending/in-flight split,
restart recovery across multiple sessions, queued/terminal no-ops, idempotency,
concurrency, scoped errors, rollback, unchanged completed roles, and exact
returned in-flight IDs. Prove no provider, worker, goroutine, HTTP, auth, or
composer scope.

Run:

```bash
gofmt -w internal/storage/sqlite/sweep.go internal/storage/sqlite/sweep_test.go
test -z "$(gofmt -l .)"
go test -v ./internal/storage/sqlite -run 'Test(Sweep|CancelSweep|CutoffSweep|DeadlineSweep|RestartSweep)'
go test ./internal/storage/sqlite -count=1
go test ./internal/storage/sqlite -count=10
go test ./... -count=1
go test -race ./...
go vet ./...
git diff --check
```

Commit only after every command passes, using exactly:

```text
Add control sweeps and restart recovery
```

Do not push. Do not begin P11. Any unmet requirement means BLOCKED and no
commit.

## Report

```text
P10 RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests:
- ...
Full gate:
- ...
Acceptance evidence:
- cancellation sweep:
- cutoff sweep:
- hard-deadline sweep:
- restart recovery:
- idempotency/concurrency:
- rollback:
- unchanged fields:
- forbidden scope:
Remaining limitations:
- ...
Git status:
<exact output>
```
