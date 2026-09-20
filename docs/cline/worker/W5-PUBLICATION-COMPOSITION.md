# W5 — Publication, Composition, and Supervisor Boundary

## Mission

Connect the released worker slices into one bounded execution path for one
claimed reviewing session.

The W5 integration must:

1. receive one already-claimed `reviewing` session;
2. use the released serialized dispatcher to receive committed role reservations;
3. execute each reservation exactly once through the released W4 execution seam;
4. publish each terminal role outcome through the released SQLite compare-and-set
   publication API;
5. notify the same dispatcher only after publication has returned;
6. stop new work on cancellation, cutoff, hard deadline, or persistence failure;
7. compose the session exactly once after all four role rows are terminal and the
   persisted in-flight count is zero; and
8. return a non-nil error at the supervisor boundary when persistence failure
   requires process restart.

This packet integrates existing seams. It does not redesign SQLite, dispatch,
role execution, provider behavior, publication, composition, HTTP, or the
supervisor daemon.

Start from the latest clean master:

```text
34cb587 Prepare W5 publication and composition packet
```

The W4 implementation commit already included in that tree is:

```text
09e4e061abceb6713c56c1523a568ddc2a80a8de
```

## Authority to read first

Read these before editing:

```text
docs/CANONICAL-FLOW.md
docs/cline/worker/00-BUILD-MAP.md
docs/cline/worker/W1-PROCESS-LOCK.md
docs/cline/worker/W2-RECOVERY-CLAIM.md
docs/cline/worker/W3-DISPATCH-LOOP.md
docs/cline/worker/W4-ROLE-EXECUTION.md
docs/cline/worker/W5-PUBLICATION-COMPOSITION.md
docs/ENGINE-CONTRACT.md
internal/worker/run.go
internal/worker/dispatch.go
internal/worker/execute.go
internal/storage/sqlite/publish.go
internal/storage/sqlite/compose.go
```

The canonical flow and released persistence APIs are authoritative. Do not infer
behavior from this packet when the source contract says otherwise.

## Allowed files

Only these paths may be modified:

```text
internal/worker/run.go
internal/worker/run_test.go
internal/worker/supervise.go
internal/worker/supervise_test.go
docs/ENGINE-CONTRACT.md
docs/cline/worker/00-BUILD-MAP.md
docs/cline/worker/W5-PUBLICATION-COMPOSITION.md
```

`internal/worker/supervise.go` and its test file are new W5 files. Keep the
integration seam narrow. If implementation requires changing any file outside
this list, stop and report `BLOCKED`; do not widen scope silently.

The worker may define narrow interfaces satisfied by the released store. Do not
import `internal/storage/sqlite` into worker code merely to use concrete store,
publication, or composition types. Do not use reflection or ad-hoc SQL to bypass
that boundary.

## Forbidden scope

Do not modify or add:

```text
internal/storage/sqlite
internal/storage/sqlite/migrations
internal/worker/lock.go
internal/worker/dispatch.go
internal/worker/dispatch_test.go
internal/worker/execute.go
internal/worker/execute_test.go
internal/review/runner.go
internal/review/runner_test.go
internal/provider
cmd/speccouncil
docs/CANONICAL-FLOW.md
README*
```

Do not add or change:

```text
HTTP
authentication
UI
live-provider configuration
supervisor daemon or restart loop
broker
lease
heartbeat
cross-review behavior
new persistence APIs
new migrations
new publication or composition implementation
```

The phrase “supervisor boundary” means that W5 returns a truthful non-nil error
when the outer process supervisor must restart the worker. W5 must not implement
a daemon, automatic restart loop, broker, lease, or heartbeat.

## Required integration contract

### 1. Claimed-session boundary

- Accept only a session already claimed as `reviewing` by W2.
- Do not claim another session.
- Do not call recovery or acquire a second process lock.
- Do not reconstruct a snapshot from mutable project/session input.
- Use only the immutable snapshot and reservation metadata returned by the
  released claim and dispatch seams.
- A no-work result remains a successful no-work result and must release the
  process lock through the existing W1/W2 path.

### 2. Reservation to W4 execution

For every committed reservation:

- invoke W4 exactly once;
- pass the reserved session ID, role identity, role-run identity, immutable
  snapshot, provider, timing policy, call timeout, and hard deadline unchanged;
- do not invoke a provider before the reservation transaction has committed;
- do not execute a second role from the same reservation;
- do not make a second provider attempt outside W4;
- preserve W4’s strict prompt-budget, two-call, retry/repair-XOR, timeout,
  cancellation, and evidence-reference behavior.

### 3. Successful publication

When W4 returns a valid successful role outcome:

- call the released success publication seam exactly once;
- pass the exact session ID and role-run ID from the reservation;
- pass the validated findings and basis references without remapping them;
- pass the truthful provider call count and available call metadata;
- require the persistence layer to enforce `in_flight -> complete` compare-and-set;
- treat findings, basis references, role completion, and metadata as one atomic
  persistence operation;
- never insert findings or references directly from worker code.

### 4. Failure publication

When W4 returns a failed or timed-out terminal outcome:

- call the released failure publication seam exactly once;
- pass the canonical error category and message from W4;
- pass the truthful provider call count and available call metadata;
- require the persistence layer to enforce `in_flight -> failed` compare-and-set;
- ensure failed roles contribute no findings.

Cancellation follows the canonical flow:

- pending roles are interrupted by the released cancellation sweep;
- roles already `in_flight` are drained rather than rewritten by the sweep;
- if W4 returns a canonical timeout/cancellation failure for an in-flight role,
  publish that returned outcome through the failure CAS seam;
- never invent a cancellation status or rewrite a role directly from worker code.

### 5. Publication conflicts

A typed publication conflict means the reservation is stale or a late result lost
the compare-and-set race.

On publication conflict:

- do not retry provider work;
- do not call publication a second time;
- do not overwrite the terminal role;
- do not insert findings or references elsewhere;
- surface the typed conflict according to the worker error contract;
- still release worker-owned resources and stop/continue only as the surrounding
  contract explicitly permits.

### 6. Transaction boundary

- Provider execution must finish before publication begins.
- No provider callback, W4 call, dispatcher callback, or composition call may
  execute while a SQLite transaction is open.
- W5 must call persistence only through released interfaces.
- W5 must not open transactions, issue SQL, or retry persistence independently.
- SQLite retry and busy/locked handling remain owned by the persistence package.

### 7. Dispatcher ordering

- Notify the same serialized dispatcher only after publication returns.
- A completion notification must represent a committed terminal role result.
- A notification or callback error must not roll back committed publication.
- Completion notifications must not invoke reservation directly or create a
  second owner loop.
- Do not modify `dispatch.go`; use its released public seam.

### 8. Cancellation, cutoff, and hard deadline

- Cancellation prevents new reservations and new provider calls.
- Dispatch cutoff prevents new reservations and new provider calls.
- Hard deadline prevents new reservations and new provider calls.
- A provider call already in progress is bounded by W4’s cancellable per-attempt
  deadline and must be drained or terminalized according to the canonical flow.
- No goroutine may remain blocked after the worker context is cancelled.
- Do not convert a typed no-work reason into a generic error.

### 9. Composition

After the released store/dispatcher state shows all four roles terminal and zero
persisted in-flight roles:

- call the released transactional composition seam exactly once for the run;
- do not compose before the readiness condition;
- do not compose while a provider call is running;
- do not construct a report in worker code;
- do not derive or rewrite session status, terminal reason, completed count, or
  incomplete count in worker code;
- return the persisted deterministic report and terminal session result unchanged;
- treat a non-terminal composition result or `SessionNotReady` error as not
  complete, not as a successful report;
- preserve duplicate composition idempotency: no row or timestamp changes on a
  second composition attempt.

### 10. Persistence failure and supervisor boundary

If publication, readiness reads, or composition returns a persistence error after
its own bounded retry policy is exhausted:

- stop new dispatch;
- do not start another provider call;
- do not hide the error or continue as if the role/session were complete;
- return a non-nil worker error to the outer supervisor;
- release the process lock and all worker-owned resources;
- leave restart recovery to W2/the outer supervisor.

W5 must not implement automatic restart, heartbeat, lease, or broker behavior.

### 11. Cleanup

The integration must release/stop all resources on:

- successful completion;
- no-work;
- context cancellation;
- dispatch cutoff;
- hard deadline;
- publication conflict;
- publication error;
- composition refusal;
- composition error; and
- abnormal worker return.

Prove no leaked process lock, dispatcher owner, ticker, provider call,
completion callback, or untracked goroutine remains.

## Required regression tests

Use real temporary SQLite stores where the released APIs are exercised, plus
barriers and deterministic fake providers. Do not use sleeps, arbitrary polling,
test-side SQL to advance production state, or substitute persistence logic.

Tests must prove all of the following:

1. A claimed reviewing session with one reservation executes that role once and
   publishes a valid success atomically.
2. A failed role publishes canonical failure metadata with no findings.
3. A timed-out role publishes canonical timeout/failure metadata with its real
   call count.
4. Pending cancellation interrupts pending roles; already in-flight work drains.
5. Cancellation, cutoff, and hard deadline prevent later reservations and
   provider calls.
6. Exactly one publication call occurs per reservation.
7. Publication happens after W4/provider execution and before dispatcher
   completion notification.
8. Notification failure cannot roll back a committed publication.
9. A stale/late publication conflict makes no second provider call and no extra
   writes.
10. Two concurrently reserved roles never exceed W4’s call budget or the
    persisted `MAX_IN_FLIGHT = 2` limit.
11. Four terminal role rows and zero persisted in-flight roles trigger one
    deterministic composition.
12. Composition is refused while any role is pending or in-flight.
13. Duplicate composition returns the persisted terminal result without changing
    rows or timestamps.
14. Persistence failure stops dispatch, returns a non-nil error, and starts no
    hidden provider, timer, callback, or goroutine work.
15. Process lock cleanup works on success, no-work, cancellation, publication
    conflict, publication failure, composition refusal, composition failure, and
    abnormal worker return.
16. No second session claim, direct SQL, direct findings insertion, publication
    bypass, fabricated report, or provider call inside a transaction occurs.
17. Concurrent completion notifications remain serialized and race-clean.
18. Existing W1, W2, W3, and W4 tests remain green.

## Verification ladder

Run exactly:

```bash
gofmt -w internal/worker/run.go internal/worker/run_test.go internal/worker/supervise.go internal/worker/supervise_test.go
test -z "$(gofmt -l .)"
go test -v ./internal/worker -run 'Test(Supervise|Worker|RunOnce|Publication|Composition)' -count=1
go test -v ./internal/worker -run 'Test(Supervise|Worker|RunOnce|Publication|Composition)' -count=10
go test -race ./internal/worker -run 'Test(Supervise|Worker|RunOnce|Publication|Composition)' -count=1
go test ./... -count=1
go vet ./...
go test -race ./...
git diff --check
```

On this Windows host, native race testing requires CGO and a C compiler. If a
native race command reports:

```text
go: -race requires cgo; enable cgo by setting CGO_ENABLED=1
```

run the same focused and full race commands from WSL Ubuntu:

```bash
cd /mnt/d/PROJECT/SpecCouncil
export CGO_ENABLED=1
go test -race ./internal/worker -run 'Test(Supervise|Worker|RunOnce|Publication|Composition)' -count=1
go test -race ./...
```

If neither environment can run race tests, report `BLOCKED`. Never report a
blocked race command as `PASS`.

## Scope audit

Before committing, run:

```bash
git diff --name-only
```

The final changed-path set must be a subset of:

```text
internal/worker/run.go
internal/worker/run_test.go
internal/worker/supervise.go
internal/worker/supervise_test.go
docs/ENGINE-CONTRACT.md
docs/cline/worker/00-BUILD-MAP.md
docs/cline/worker/W5-PUBLICATION-COMPOSITION.md
```

Also verify:

```bash
git grep -n 'internal/storage/sqlite' -- internal/worker/run.go internal/worker/run_test.go internal/worker/supervise.go internal/worker/supervise_test.go || true
git diff --check
git status --short
```

Any forbidden path, direct SQLite import, direct SQL, off-scope dirty file,
failed test, failed vet, failed formatting check, or unavailable required race
gate means `BLOCKED` and **no commit**.

## Commit and push policy

Commit exactly:

```text
Integrate worker publication and composition
```

Do not push.

Do not begin any later phase.

## Required report

Return exactly this structure:

```text
W5 RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests:
- ...
Full gate:
- ...
Acceptance evidence:
- claimed-session boundary:
- reserved-role execution:
- success publication:
- failure/timeout publication:
- cancellation/cutoff/deadline:
- publication ordering and CAS conflict:
- dispatcher serialization:
- composition exactly once:
- idempotent duplicate composition:
- persistence failure/supervisor boundary:
- lock and goroutine cleanup:
- forbidden provider/transaction/direct-SQL scope:
- scope:
Git status:
<exact output>
```

Do not claim race PASS unless the command actually returned exit code 0 with no
race report. Do not claim W5 complete when any required acceptance item is only
inferred, untested, or blocked.
