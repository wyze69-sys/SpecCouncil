# W3 — Serialized Guarded Dispatch Loop

## Mission

Add the owner-loop seam that repeatedly reserves pending roles through the
released authoritative SQLite dispatch API. The loop is serialized: timer ticks
and role-completion notifications wake one owner, never competing dispatch
routines. This slice reserves roles and invokes an injected post-commit callback
only; it does not call providers or publish role results.

Start from clean master at W2 commit
`236ad36eae996995e7171a6ebb9c92b1e0ec6292`.

## Allowed files

```text
internal/worker/dispatch.go
internal/worker/dispatch_test.go
docs/ENGINE-CONTRACT.md
docs/cline/worker/00-BUILD-MAP.md
docs/cline/worker/W3-DISPATCH-LOOP.md
```

Do not modify `internal/storage/sqlite`, migrations, `internal/worker/run.go`,
lock files, `internal/review`, `internal/provider`, `cmd/speccouncil`,
`docs/CANONICAL-FLOW.md`, or README.

## Contract

Create a worker-owned serialized dispatch loop with a narrow injected callback.
The exact type names may be chosen sensibly, but the behavior must provide:

1. Accept only a claimed `reviewing` session identity. Do not claim sessions,
   recover sessions, or accept an arbitrary queued session here.
2. One owner goroutine performs every reservation. Public wake/tick methods may
   signal it, but must never call `ReservePendingRole` directly.
3. Before each reservation, evaluate the fast-path state in this order:
   cancellation, dispatch cutoff, local execution capacity, then canonical next
   pending role. The authoritative result remains the SQLite transaction result;
   fast reads are advisory and may race.
4. Call `ReservePendingRole` only after the session is claimed and only from the
   serialized owner loop. A successful reservation is committed before the
   injected `OnReserved` callback runs.
5. Never exceed two persisted in-flight roles. Do not invent a second capacity
   counter that can override SQLite; local capacity may only prevent a start.
6. If reservation returns a typed no-work result, preserve its reason and stop or
   wait according to the loop state; do not turn cancellation, cutoff, capacity,
   or no-pending into generic errors.
7. Role completion notifications wake the same owner loop. The callback must not
   be invoked while a SQLite transaction is open, and callback execution must not
   block the owner from observing context cancellation forever.
8. The loop must stop on context cancellation and release all internal channels,
   goroutines, and timers. No timer-based busy spin and no sleeps in tests.
9. W3 must not invoke providers, compose reports, publish success/failure, or
   mutate session state outside `ReservePendingRole`.
10. Expose enough result/trace information for tests to prove reservation order,
    no-work reason, callback-after-commit, and clean shutdown.

Do not use a second dispatcher goroutine, a process-global singleton, polling
that bypasses wake signals, or test-only SQL to advance role state.

## Required regression proof

Use real temporary SQLite stores, barriers, and deterministic clocks; no sleeps:

1. claimed reviewing session reserves the lowest canonical pending role first;
2. a completion notification wakes the same serialized owner and permits the
   next reservation;
3. two simultaneous wake/tick signals still produce one ordered reservation
   sequence with no concurrent calls to `ReservePendingRole`;
4. persisted capacity 0/1/2 is respected and never exceeds two in-flight roles;
5. committed cancellation prevents later reservation and preserves the typed
   cancelled no-work reason;
6. committed cutoff prevents later reservation and preserves the typed cutoff
   no-work reason;
7. reservation commits before `OnReserved` begins; callback failure cannot roll
   back or rewrite the committed in-flight role;
8. no pending role produces the typed no-pending result without an error;
9. context cancellation stops the loop without leaked goroutines or timers;
10. a persistence error exits the loop cleanly and is returned unchanged;
11. no provider, composer, publication, second claim, or transaction-held
    callback occurs;
12. repeated full-loop execution remains deterministic and race-clean.

## Verification

```bash
gofmt -w internal/worker/dispatch.go internal/worker/dispatch_test.go
test -z "$(gofmt -l .)"
go test -v ./internal/worker -run 'Test(Dispatch|WorkerDispatch)' -count=1
go test -race ./internal/worker -run 'Test(Dispatch|WorkerDispatch)' -count=1
go test ./... -count=1
go vet ./...
go test -race ./...
git diff --check
```

Also verify scope:

```bash
git diff --name-only | grep -v -E '^(internal/worker/dispatch\.go|internal/worker/dispatch_test\.go|docs/ENGINE-CONTRACT\.md|docs/cline/worker/00-BUILD-MAP\.md|docs/cline/worker/W3-DISPATCH-LOOP\.md)$' && echo "FORBIDDEN FILES MODIFIED" || echo "scope clean"
```

Do not push. Do not begin W4. Any unmet requirement means BLOCKED and no commit.

## Commit

```text
Add serialized guarded dispatch loop
```

## Report

```text
W3 RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests:
- ...
Full gate:
- ...
Acceptance evidence:
- canonical order:
- serialized owner:
- capacity:
- cancellation/cutoff:
- commit-before-callback:
- shutdown:
- forbidden provider/publication scope:
- scope:
Git status:
<exact output>
```
