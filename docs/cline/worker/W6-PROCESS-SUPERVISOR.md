# W6 — Worker Process Attempt Supervisor

## Status

COMPLETE.

## Mission

Add one small, production-safe process-lifetime boundary around the released
W1–W5 worker APIs.

W6 owns exactly one concern: running one worker attempt under a caller-owned
context and returning a truthful result or error. The caller may connect that
context to OS signals or a service manager later. W6 must not become a daemon,
automatic restart loop, HTTP server, provider adapter, or second orchestration
layer.

The W6 implementation must make these boundaries explicit:

```text
outer process / service manager
        |
        | creates context and decides whether to restart
        v
W6 process-attempt supervisor
        |
        | invokes exactly one attempt, waits for it, returns its result/error
        v
released W1–W5 worker attempt
        |
        v
lock -> recovery -> claim -> dispatch -> execute -> publish -> compose
```

W6 must not duplicate any operation below its boundary.

## Starting point

Start from the latest clean tree containing the released W5 implementation and
the W6 packet commit:

```text
4d80095 Prepare W6 worker process supervisor packet
```

Before editing, verify the live tree and read the current files. Do not assume
historical reports are current.

## Authority and required reading

Read all of these before editing:

```text
docs/CANONICAL-FLOW.md
docs/ENGINE-CONTRACT.md
docs/cline/worker/00-BUILD-MAP.md
docs/cline/worker/W1-PROCESS-LOCK.md
docs/cline/worker/W2-RECOVERY-CLAIM.md
docs/cline/worker/W3-DISPATCH-LOOP.md
docs/cline/worker/W4-ROLE-EXECUTION.md
docs/cline/worker/W5-PUBLICATION-COMPOSITION.md
docs/cline/worker/W6-PROCESS-SUPERVISOR.md
internal/worker/lock.go
internal/worker/run.go
internal/worker/dispatch.go
internal/worker/execute.go
internal/worker/supervise.go
internal/worker/*_test.go
```

The canonical flow and live released source are authoritative. If this packet
conflicts with the source contract, stop and report the exact conflict instead
of guessing.

## Exact W6 deliverable

Implement a narrow worker package API in a new file, preferably:

```text
internal/worker/process.go
internal/worker/process_test.go
```

Use another filename only if the live package proves that a different name is
required. Do not split the worker package into subpackages.

The API must represent one process attempt. A suitable shape is an injected
attempt function or narrow interface, for example:

```go
type AttemptFunc func(context.Context) (TResult, error)

func RunAttempt[T any](ctx context.Context, attempt AttemptFunc[T]) (TResult, error)
```

The exact names and generic shape are implementation choices, but the following
observable contract is mandatory:

- the caller owns the context;
- W6 invokes one attempt at most once;
- the attempt runs to return after cancellation is requested;
- W6 waits for the attempt to return before returning itself;
- W6 returns the attempt result and error without hiding typed errors;
- W6 does not acquire a lock, recover a session, claim work, dispatch roles,
  invoke a provider, publish, or compose directly;
- W6 does not decide whether the outer process should restart.

If the existing worker APIs require an adapter, keep that adapter narrow and
inside W6. Do not import `internal/storage/sqlite` into production worker code.
Do not add reflection, direct SQL, or a second store interface merely to wire the
attempt boundary.

## Context and shutdown contract

### Caller-owned cancellation

W6 receives a context from its caller. An OS signal handler, HTTP server, CLI,
or service manager is outside this slice. Do not call `signal.Notify` inside the
worker package unless the live architecture proves there is no cleaner boundary;
the preferred design is context-only and directly testable.

When the context is already canceled before the attempt starts:

- return the context error;
- do not invoke the attempt;
- do not start a goroutine;
- do not acquire a process lock;
- do not claim a session;
- do not convert cancellation into a no-work result.

When the context is canceled while the attempt is running:

- cancellation must be observable by the attempt through `ctx.Done()` or
  `ctx.Err()`;
- W6 must wait until the attempt function returns;
- W6 must not return early while the attempt is still running;
- W6 must not start a replacement attempt;
- W6 must return the attempt's returned error if the attempt returns one;
- if the attempt returns nil after observing cancellation, preserve that result
  and do not fabricate a worker failure in W6.

W6 cannot forcibly terminate a function that ignores its context. The test must
make that distinction explicit: a cooperative attempt returns after observing
cancellation; a deliberately non-cooperative attempt must not be hidden behind
an early supervisor return. Do not add unsafe goroutine killing or arbitrary
sleep-based timeouts.

### Error precedence

The attempt is the source of worker outcome. W6 must not replace a typed error
with a generic `context.Canceled` merely because cancellation was requested.
Use normal Go error identity rules:

- preserve `errors.Is` for `context.Canceled`, `context.DeadlineExceeded`,
  `worker.ErrAlreadyOwned`, persistence errors, and typed worker errors;
- return the attempt error when the attempt returned an error;
- return the context error only when the attempt was not started or the contract
  explicitly requires a context error after a clean cancellation with no attempt
  result;
- never return nil when the attempt reported an error;
- never retry or wrap repeatedly in a loop.

If a wrapper error is useful, it must preserve the underlying error with `%w`.
Tests must assert error identity, not only error strings.

## Attempt serialization and lifecycle

The W6 API represents one attempt, so concurrent callers must not accidentally
create overlapping worker attempts if a reusable supervisor object is used.
Choose one of these designs and document it in code:

1. a one-shot function with no reusable mutable state; or
2. a supervisor object whose `Run` method rejects concurrent calls with a typed
   error and never starts the second attempt.

The preferred design is the one-shot function because it makes overlap
impossible by construction. If a reusable object is required by existing code,
then it must prove:

- at most one attempt is active;
- the second caller receives a deterministic error before its attempt starts;
- the first attempt is not canceled or altered by the rejected caller;
- after the first attempt returns, a later call is either explicitly allowed by
  the documented API or rejected as closed; do not infer reuse semantics;
- no lock, channel, timer, or goroutine leaks across calls.

Do not implement an implicit loop such as:

```text
for {
    run attempt
    sleep
    run another attempt
}
```

Automatic restart belongs to the outer deployment/process layer and is not part
of W6.

## No-work and failure boundary

W1/W2 already distinguish a successful no-work claim from an error. W6 must
preserve that distinction without inspecting concrete SQLite types or inventing
new statuses.

Required behavior:

- a successful no-work attempt returns success and its no-work result;
- lock contention returns the existing typed ownership error;
- invalid configuration returns the existing validation error;
- recovery failure returns its original error;
- claim failure returns its original error;
- dispatch, execution, publication, composition, or persistence failure returns
  its original typed error through W6;
- W6 does not retry any of these errors;
- W6 does not turn no-work into failure;
- W6 does not turn failure into no-work;
- W6 does not create a report or alter persisted status.

The outer process caller decides whether a returned error should terminate the
process and whether a later process restart is appropriate.

## Process-lock boundary

W6 must not acquire a second process lock. The released W1/W2 attempt remains
the owner of process-lock acquisition and release.

Tests must prove the following boundary:

- W6 invokes the attempt once;
- the attempt's existing `RunOnce` path acquires and releases the lock;
- W6 does not call `AcquireProcessLock` directly;
- after a successful return, no-work return, cancellation, or attempt failure,
  the lock is released according to the existing W1/W2 contract;
- lock contention remains `errors.Is(err, worker.ErrAlreadyOwned)` where the
  released API provides that identity.

Do not modify `internal/worker/lock.go` or the W1/W2 lock semantics in this
slice. If a lock test fails because the current released behavior is broken,
report `BLOCKED` and do not repair it inside W6.

## Forbidden behavior

Do not add or modify any of the following:

```text
internal/storage/sqlite/**
internal/storage/sqlite/migrations/**
internal/worker/lock.go
internal/worker/dispatch.go
internal/worker/execute.go
internal/worker/supervise.go
internal/review/**
internal/provider/**
cmd/speccouncil/**
```

Do not add:

- HTTP endpoints or middleware;
- authentication or authorization;
- UI or browser code;
- a real provider or network client;
- provider credentials, environment loading, or model selection;
- a daemon main loop;
- automatic restart or exponential retry;
- broker, lease, heartbeat, PID database, or cross-process coordination;
- a second worker lock;
- a second session claim or recovery sweep;
- new SQLite methods or migrations;
- direct SQL or test-side replacement persistence;
- empty future packages;
- unrelated formatting or cleanup.

Do not edit `docs/CANONICAL-FLOW.md` or the public README.

## Allowed files

The final implementation change must be limited to:

```text
internal/worker/process.go
internal/worker/process_test.go
docs/ENGINE-CONTRACT.md
docs/cline/worker/00-BUILD-MAP.md
docs/cline/worker/W6-PROCESS-SUPERVISOR.md
```

The packet and build-map changes are coordinator/documentation changes, not a
reason to modify unrelated files. If another production or test file must change,
stop first and report `BLOCKED` with the exact path and reason.

## Required tests

Use channels, barriers, atomic counters, and deterministic fake attempts. Do not
use arbitrary sleeps, wall-clock polling, or timing luck as race proof.

At minimum, add tests that prove:

1. **One attempt** — a normal attempt is invoked exactly once and its result is
   returned.
2. **Nil/invalid input** — nil attempt or invalid supervisor configuration is
   rejected before any work starts, using a stable typed error if the API needs
   one.
3. **Already canceled** — a canceled context invokes no attempt and returns a
   context error.
4. **Cancellation delivery** — a blocked cooperative attempt observes context
   cancellation and returns; W6 waits for that return.
5. **Wait-before-return** — a barrier proves W6 does not return until the
   attempt has recorded that it finished.
6. **No replacement** — cancellation causes zero additional attempts.
7. **No-work preservation** — a fake attempt returns a successful no-work result;
   W6 returns success without rewriting it.
8. **Error identity** — a sentinel or typed worker error returned by the attempt
   is preserved with `errors.Is`.
9. **Deadline identity** — a deadline error is not rewritten as generic failure.
10. **Concurrent-call behavior** — if the API is reusable, two callers cannot
    overlap attempts; if it is one-shot, the API shape makes overlap impossible
    and the test documents that property.
11. **Lock boundary** — integration with the released attempt path proves W6
    does not acquire a second lock and existing lock contention remains typed.
12. **Repeated lifecycle** — repeated successful, no-work, canceled, and failed
    calls do not leak supervisor-owned goroutines or channels.
13. **Race safety** — the focused package tests pass under `go test -race` in a
    CGO-capable environment.
14. **Existing behavior** — all current W1–W5 tests remain green.

Tests must not make direct SQL writes, bypass the released store, invoke a
provider from W6, or use a fake implementation that replaces the worker attempt
contract instead of testing it.

## Documentation requirements

Update `docs/ENGINE-CONTRACT.md` only after the implementation is verified. Add
one concise W6 section that states:

- W6 is a one-attempt process boundary;
- context ownership belongs to the caller;
- cancellation is cooperative and the supervisor waits for attempt return;
- no automatic restart is implemented;
- worker errors and no-work results are preserved;
- lock/recovery/claim/dispatch/execution/publication/composition remain owned by
  the released worker layers;
- HTTP, auth, live provider, and deployment restart policy remain later work.

Update `docs/cline/worker/00-BUILD-MAP.md` from `READY` to `COMPLETE` only if
all acceptance gates pass. Do not mark W6 complete from focused tests alone.

## Verification ladder

Run the exact checks below from `D:\PROJECT\SpecCouncil`:

```bash
git status --short --branch
git diff --check

test -z "$(gofmt -l .)"
go test -v ./internal/worker -run 'Test(Process|Attempt|Supervisor|RunOnce|Worker)' -count=1
go test -v ./internal/worker -run 'Test(Process|Attempt|Supervisor|RunOnce|Worker)' -count=10
go test ./... -count=1
go vet ./...
git diff --check
```

Then run the race gate:

```bash
go test -race ./internal/worker -run 'Test(Process|Attempt|Supervisor|RunOnce|Worker)' -count=1
go test -race ./...
```

On native Windows, if Go returns exactly:

```text
go: -race requires cgo; enable cgo by setting CGO_ENABLED=1
```

that native result is `BLOCKED`, not `PASS`. Run the same commands in WSL Ubuntu:

```bash
/c/Windows/System32/wsl.exe -d Ubuntu -- bash -lc \
  'cd /mnt/d/PROJECT/SpecCouncil && export CGO_ENABLED=1 && \
   go test -race ./internal/worker -run "Test(Process|Attempt|Supervisor|RunOnce|Worker)" -count=1 && \
   go test -race ./...'
```

Report native Windows and WSL results separately. A race command that exits
nonzero, times out, or reports a race is not a pass.

## Scope audit before commit

Run:

```bash
git diff --name-only

git diff --check
grep -R -n 'internal/storage/sqlite\|AcquireProcessLock\|signal.Notify\|for {' \
  internal/worker/process.go internal/worker/process_test.go || true
git status --short
```

Interpret the scope probe carefully:

- a direct SQLite import in W6 production code is forbidden;
- a direct `AcquireProcessLock` call in W6 production code is forbidden;
- `signal.Notify` is forbidden unless explicitly justified and approved by the
  live architecture;
- an unbounded `for` loop is forbidden;
- test-only references must still prove they do not bypass the boundary.

The final changed-path set must be a subset of the allowed list. Any off-scope
file, dirty unrelated work, failed gate, unverified race requirement, or
unresolved source/packet conflict means `BLOCKED` and no implementation commit.

## Exact commit rule

After all gates pass, commit exactly:

```text
Add worker process supervisor boundary
```

Do not push.

Do not begin HTTP, auth, live-provider, UI, or later worker work in this slice.

## Required completion report

Return exactly this structure:

```text
W6 RESULT: COMPLETE | BLOCKED | FAILED
Commit: <sha or none>
Changed files:
- <exact path>

Focused tests:
- <command>: PASS/FAIL/BLOCKED

Full gate:
- <command>: PASS/FAIL/BLOCKED

Race gate:
- native Windows: PASS/FAIL/BLOCKED
- WSL Ubuntu: PASS/FAIL/BLOCKED

Acceptance evidence:
- one-attempt boundary:
- already-canceled context:
- cooperative shutdown cancellation:
- wait-before-return:
- no replacement attempt:
- no-work preservation:
- typed error preservation:
- attempt serialization:
- process-lock boundary:
- cleanup/no leaked goroutines:
- forbidden scope audit:

Git status:
<exact output>

Not implemented by W6:
- HTTP/API
- authentication/authorization
- UI
- real provider/network calls
- automatic restart policy
- broker/lease/heartbeat
```

Do not claim `COMPLETE` when a required item is inferred rather than directly
tested. Do not claim race `PASS` for a blocked command. Do not push.
