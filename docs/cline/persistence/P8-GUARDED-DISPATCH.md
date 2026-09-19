# P8 — Guarded Dispatch Reservation

## Mission

Add only the transactional guarded reservation of pending roles. Start from clean
P7 acceptance `5bd6388`. This slice does not execute providers or start goroutines.

## Scope

Create or modify only:

```text
internal/storage/sqlite/dispatch.go
internal/storage/sqlite/dispatch_test.go
docs/ENGINE-CONTRACT.md
```

## Contract

Provide a package-internal reservation function using `withImmediate` that:

- operates only on a `reviewing` session;
- refuses reservation when `cancel_requested = 1`;
- refuses reservation when `now >= dispatch_cutoff_at`;
- counts persisted `in_flight` roles inside the same transaction;
- never reserves when the count is already `MAX_IN_FLIGHT = 2`;
- selects a pending role deterministically by canonical role order;
- atomically changes exactly one pending role to `in_flight` and sets its
  `started_at` and initial call metadata;
- returns a typed no-work result for cancellation, cutoff, full capacity, no
  pending role, non-reviewing session, or a guarded-update race;
- rechecks every predicate in the authoritative UPDATE; fast-path reads are not
  authority;
- never changes session status or cancel flag;
- never calls a provider, starts a goroutine, or holds a transaction over work
  outside the database.

Test `MAX_IN_FLIGHT = 2` as a persisted invariant. A concurrent cancellation that
commits before the guarded update must prevent reservation; a reservation that
commits first is legitimately in_flight and may drain later.

## Tests and gate

Use real file databases and barriers/channels, never sleeps. Cover capacity 0/1/2,
canonical role order, cutoff boundary, cancellation race, two concurrent
reservations with one winner per slot, duplicate reservation prevention, rollback,
and untouched session/other-role fields.

```bash
gofmt -w internal/storage/sqlite/dispatch.go internal/storage/sqlite/dispatch_test.go
test -z "$(gofmt -l .)"
go test ./internal/storage/sqlite -count=1
go test ./internal/storage/sqlite -count=10
go test ./... -count=1
go vet ./...
git diff --check
```

Commit exactly:

```text
Add guarded role dispatch reservation
```

Never push from Cline. Any failure means BLOCKED and no commit. Do not begin P9.

## Report

```text
P8 RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests:
- ...
Full gate:
- ...
Acceptance evidence:
- authoritative predicates:
- in-flight capacity:
- role ordering:
- cancellation/cutoff race:
- rollback:
- no provider or goroutine scope:
Remaining limitations:
- ...
Git status:
<exact output>
```
