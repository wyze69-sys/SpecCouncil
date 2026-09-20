# W2 — Restart Recovery and Single-Session Claim

## Mission

Add the first worker orchestration seam after the crash-safe process lock. A
worker run must acquire ownership, perform restart recovery before claiming new
work, then claim at most one queued session through the released SQLite APIs.
This slice ends after the claim; it does not dispatch roles or call providers.

Start from clean master at W1 commit `87980539c5c810d9bc088ea13eb5e28a8e6b4d31`.

## Allowed files

```text
internal/worker/run.go
internal/worker/run_test.go
docs/ENGINE-CONTRACT.md
docs/cline/worker/00-BUILD-MAP.md
docs/cline/worker/W2-RECOVERY-CLAIM.md
```

Do not modify `internal/storage/sqlite`, migrations, `internal/review`,
`internal/provider`, lock implementation files, `cmd/speccouncil`,
`docs/CANONICAL-FLOW.md`, or README.

## Contract

Add a small worker orchestration API using the existing `worker.ProcessLock` and
existing `sqlite.Store` methods. Keep the exact public names and configuration
shape reasonable, but expose enough result data for callers/tests to distinguish
ownership, recovery, claim, no-work, and persistence errors.

A single `RunOnce(ctx, config)` operation must:

1. Validate required configuration before acquiring the lock. The lock path,
   store, timing policy, and explicit restart cutoff must be present and valid.
2. Acquire the process lock. If another owner holds it, return the typed
   `ErrAlreadyOwned` outcome promptly. Do not open or mutate application state
   after ownership failure.
3. Defer lock release on every path. Preserve the original operation error if
   release also fails, and surface release failure when the operation otherwise
   succeeds.
4. Run exactly one `SweepRestartRecovery` operation before any claim. Use the
   explicit cutoff supplied by the caller; do not infer a stale threshold from
   wall-clock time inside the worker.
5. Call `ClaimSession` exactly once after recovery. Use the caller's validated
   timing policy. Do not claim a second session, poll, sleep, or loop in W2.
6. Return the authoritative recovery result and claim result without fabricating
   status. A no-work claim is a successful worker run, not an error.
7. Never start a goroutine, provider call, dispatch operation, composer, HTTP
   route, or transaction held across lock/recovery/claim calls.
8. If recovery fails, do not claim. If claim fails, release the lock and return
   the persistence error. Database mutations remain governed by the released
   `withImmediate` boundaries.
9. The worker must not accept a caller-supplied session ID for claiming. FIFO
   selection remains solely in `sqlite.ClaimSession`.

Do not use a process-global singleton, PID marker, SQLite lock row, or test-only
replacement store. Tests may use the real temporary SQLite store and the existing
persistence fixtures, but must not issue ad-hoc SQL to create or advance work.

## Required regression proof

Use real temporary stores and deterministic timestamps/barriers; no sleeps:

1. valid run acquires ownership, runs recovery first, claims exactly one FIFO
   session, and returns its claim result;
2. stale reviewing sessions are recovered before the queued session is claimed;
3. queued cancellation and terminal rows are preserved according to the released
   restart-sweep contract;
4. no queued session returns a successful no-work result;
5. a second concurrent process/run returns `ErrAlreadyOwned` promptly and leaves
   sessions unchanged;
6. two concurrent runs against the same lock produce exactly one owner and no
   double claim;
7. recovery failure prevents claim;
8. claim failure still releases the process lock;
9. context cancellation before lock, during setup, and before claim returns
   cancellation without leaking ownership;
10. release failure is surfaced without hiding a successful claim result, while
    an operation error remains the primary error;
11. no dispatch, provider, composer, goroutine-held transaction, or second
    session claim occurs;
12. a subprocess run terminated after acquisition leaves the lock acquirable and
    does not corrupt or partially alter the SQLite state.

For ordering proof, use an injected test seam or recorded calls that observes
recovery completion before claim begins; do not rely on timestamps or sleeps.

## Verification

```bash
gofmt -w internal/worker/run.go internal/worker/run_test.go
test -z "$(gofmt -l .)"
go test -v ./internal/worker -run 'Test(RunOnce|Worker)' -count=1
go test -race ./internal/worker -run 'Test(RunOnce|Worker)' -count=1
go test ./... -count=1
go vet ./...
go test -race ./...
git diff --check
```

Also verify scope:

```bash
git diff --name-only | grep -v -E '^(internal/worker/run\.go|internal/worker/run_test\.go|docs/ENGINE-CONTRACT\.md|docs/cline/worker/00-BUILD-MAP\.md|docs/cline/worker/W2-RECOVERY-CLAIM\.md)$' && echo "FORBIDDEN FILES MODIFIED" || echo "scope clean"
```

Do not push. Do not begin W3. Any unmet requirement means BLOCKED and no commit.

## Commit

```text
Add worker restart recovery and single-session claim
```

## Report

```text
W2 RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests:
- ...
Full gate:
- ...
Acceptance evidence:
- recovery-before-claim:
- single-session FIFO claim:
- no-work:
- second-owner rejection:
- failure cleanup:
- cancellation:
- persistence/provider scope:
- scope:
Git status:
<exact output>
```
