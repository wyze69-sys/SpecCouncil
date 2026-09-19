# P12 — End-to-End Persistence and Concurrency Proof

## Mission

Prove the complete SQLite persistence phase through the public storage seams,
without adding worker/provider/API/UI code. Start from a clean tree at P11
acceptance `3066996`.

## Scope

Allowed paths only:

```text
internal/storage/sqlite/persistence_e2e_test.go
docs/ENGINE-CONTRACT.md
```

Tests may call existing storage methods and the existing deterministic review
composer. Do not modify production behavior, migrations, domain/review code, or
any earlier tests.

## Required end-to-end scenarios

Build real temporary databases and prove, using barriers rather than sleeps:

1. Fresh submit creates one frozen snapshot, ordered evidence, one queued
   session, and four pending role runs.
2. FIFO claim creates one reviewing session with exact timing fields.
3. Guarded dispatch reserves at most two roles and respects canonical order.
4. Successful P9 publication persists findings and citations atomically.
5. Failed publication persists no findings and preserves the failure metadata.
6. Cancellation interrupts pending roles, drains in-flight publication, and
   composes `partial/user_cancelled`.
7. Cancellation before dispatch prevents reservation and yields the canonical
   zero-completed partial result.
8. Cutoff interrupts pending roles while an in-flight role may publish.
9. Hard deadline interrupts pending roles and routes in-flight roles through
   P9 timeout failure; late success cannot overwrite terminal state.
10. Restart recovery preserves completed findings, interrupts unfinished roles,
    and never replays provider work.
11. Same-key concurrent submissions produce exactly one fresh session and
    replays or typed conflicts as appropriate.
12. Concurrent claimers, dispatchers, publishers, sweepers, and composers have
    exactly one valid winner where the contract requires it and no lost update.
13. Busy/locked retry exhaustion returns the typed persistence-unavailable error
    at the configured bound.
14. Reports before terminalization return typed not-finished; terminal reports
    are deterministic and repeated reads/composition do not mutate persisted
    data.
15. A modified applied migration checksum prevents the store from opening.

## Proof requirements

Snapshot all relevant tables before and after each read-only operation. Verify
immutable data, session identity, cancellation, timing, role call counts,
findings, basis references, and terminal fields exactly. Assert every final
session has four terminal roles and valid counts. Verify all four canonical
roles share one snapshot ID/hash. Include project scoping and unknown-ID errors.

The test file must include a forbidden-scope inspection proving no provider
calls, network calls, worker loops, HTTP/auth routes, UI code, or goroutine-held
SQLite transactions were added by this slice.

## Verification gate

```bash
gofmt -w internal/storage/sqlite/persistence_e2e_test.go
test -z "$(gofmt -l .)"
go test ./internal/storage/sqlite -count=1
go test ./internal/storage/sqlite -count=10
go test ./... -count=1
go test -race ./...
go vet ./...
git diff --check
```

If race testing is unavailable because the Windows host lacks a C compiler,
record the exact error and run the remaining gates; do not fabricate a race
result. The final report must include the exact command output and git status.

Commit only after every applicable requirement passes, using exactly:

```text
Prove end-to-end persistence invariants
```

Do not push. Do not add later-phase implementation. Any unmet scenario means
BLOCKED and no commit.

## Report

```text
P12 RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests:
- ...
Full gate:
- ...
Acceptance evidence:
- submit/claim:
- dispatch/publication:
- cancellation/cutoff/deadline:
- restart recovery:
- concurrency/idempotency:
- retry/migration integrity:
- report determinism:
- forbidden scope:
Remaining limitations:
- ...
Git status:
<exact output>
```
