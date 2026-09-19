# P7 — Cancellation Request Mutation

## Mission

Add only the idempotent cancellation-request mutation. Start from clean P6
acceptance `a642e58`. Do not dispatch, interrupt roles, compose, or run a worker.

## Scope

Create or modify only:

```text
internal/storage/sqlite/cancel.go
internal/storage/sqlite/cancel_test.go
docs/ENGINE-CONTRACT.md
```

## Contract

Provide a package-internal `RequestCancellation(ctx, sessionID)` using
`withImmediate`.

- Unknown session returns the typed not-found error.
- `queued` and `reviewing` sessions atomically change `cancel_requested` from 0
  to 1 and return the committed session state.
- Repeating the request is idempotent: 1 -> 1 succeeds without changing other
  fields or timestamps.
- Terminal sessions are not reopened, have no role states changed, and return a
  typed already-terminal/no-op result according to the existing read model
  contract.
- The mutation never changes status, role runs, terminal reason, counts, timing,
  or findings. Pending-role interruption is P10, not P7.
- No provider, worker, dispatch, composer, HTTP, auth, or UI behavior.

## Tests and gate

Use real temporary databases. Cover queued/reviewing mutation, repeated requests,
terminal no-op, unknown ID, concurrent requests, rollback, and proof that all
non-cancellation fields and role rows remain unchanged.

```bash
gofmt -w internal/storage/sqlite/cancel.go internal/storage/sqlite/cancel_test.go
test -z "$(gofmt -l .)"
go test ./internal/storage/sqlite -count=1
go test ./internal/storage/sqlite -count=10
go test ./... -count=1
go vet ./...
git diff --check
```

Commit exactly:

```text
Add idempotent cancellation request
```

Never push from Cline. Any failure means BLOCKED and no commit. Do not begin P8.

## Report

```text
P7 RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests:
- ...
Full gate:
- ...
Acceptance evidence:
- queued/reviewing:
- idempotency:
- terminal behavior:
- concurrency:
- rollback:
- unchanged role/session fields:
- forbidden scope:
Remaining limitations:
- ...
Git status:
<exact output>
```
