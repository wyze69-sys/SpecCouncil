# W1 — Exclusive Worker Process Ownership

## Mission

Add only the crash-safe process-ownership seam required before a worker can
claim queued sessions. A worker process must acquire an exclusive OS-backed lock;
a second process must exit without touching the database or claiming work. The
lock must become acquirable automatically when the owner exits or crashes.

Start from clean master at the persistence repair acceptance commit. Persistence
is complete through R4 (`f3372dd` snapshot-hash repair and `a6db999` findings
insert guard). Do not modify persistence behavior.

## Allowed files

```text
internal/worker/lock.go
internal/worker/lock_windows.go
internal/worker/lock_unix.go
internal/worker/lock_test.go
docs/ENGINE-CONTRACT.md
docs/cline/worker/00-BUILD-MAP.md
docs/cline/worker/W1-PROCESS-LOCK.md
```

Do not modify `internal/storage/sqlite`, migrations, `internal/review`,
`internal/provider`, `cmd/speccouncil`, `docs/CANONICAL-FLOW.md`, or README.
Do not add a worker loop, session claim, restart sweep, provider call, HTTP,
auth, supervisor, or database lock table in this slice.

## Contract

Create a small package-level API with no global mutable singleton:

```go
type ProcessLock struct { /* owned OS handle */ }

func AcquireProcessLock(ctx context.Context, path string) (*ProcessLock, error)
func (l *ProcessLock) Release() error
```

Requirements:

1. Reject empty paths and unusable parent paths with typed, inspectable errors;
   do not silently choose a default location.
2. Open/create the lock file and acquire an exclusive OS lock that is tied to
   the owning process/handle. The lock must not be represented by a PID file,
   SQLite row, or stale marker requiring cleanup.
3. A second acquisition against the same path fails promptly with a typed
   `ErrAlreadyOwned` result. It must not block indefinitely and must not modify
   application tables.
4. `Release` is idempotent, closes the OS handle, and removes no unrelated file.
   The lock is also released by normal process exit and by OS cleanup after a
   crash/forced termination.
5. Context cancellation is honored while opening/acquiring. No goroutine or
   transaction is leaked on error.
6. Platform code must compile on Windows and Unix-like targets. Keep the public
   API in `lock.go`; isolate OS calls in build-tagged files.
7. Do not expose the lock file contents as an ownership protocol. A zero-length
   lock file is acceptable; tests must prove semantics through acquisition.

## Required regression proof

Use a real temporary directory and deterministic coordination; no sleeps:

1. empty path is rejected;
2. first acquisition succeeds;
3. second acquisition on the same path returns `ErrAlreadyOwned` promptly;
4. releasing the first lock permits a later acquisition;
5. repeated `Release` is safe;
6. an owner handle becoming unreachable/closed releases the OS lock and a new
   acquisition succeeds (use a subprocess helper where necessary; do not claim
   in-process GC proves crash behavior);
7. a subprocess that acquires the lock and is terminated without calling
   `Release` leaves the path acquirable by the parent;
8. two concurrent acquisition attempts have exactly one winner;
9. cancellation before acquisition returns context cancellation without a live
   lock or leaked helper goroutine;
10. lock acquisition does not open the SpecCouncil database or change any table.

The subprocess tests must be bounded and skip only when the target platform
cannot provide the required OS primitive; do not skip the primary Windows proof.

## Verification

```bash
gofmt -w internal/worker/*.go
test -z "$(gofmt -l .)"
go test -v ./internal/worker -count=1
go test -race ./internal/worker -count=1
go test ./... -count=1
go vet ./...
go test -race ./...
git diff --check
```

Also verify scope:

```bash
git diff --name-only | grep -v -E '^(internal/worker/lock\.go|internal/worker/lock_windows\.go|internal/worker/lock_unix\.go|internal/worker/lock_test\.go|docs/ENGINE-CONTRACT\.md|docs/cline/worker/00-BUILD-MAP\.md|docs/cline/worker/W1-PROCESS-LOCK\.md)$' && echo "FORBIDDEN FILES MODIFIED" || echo "scope clean"
```

Do not push. Do not claim W2 readiness until Hermes independently verifies the
commit and the full race gate.

## Commit

```text
Add crash-safe worker process lock
```

## Report

```text
W1 RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests:
- ...
Full gate:
- ...
Acceptance evidence:
- second owner rejected:
- release reacquisition:
- abnormal-exit reacquisition:
- concurrent winner count:
- persistence untouched:
- scope:
Git status:
<exact output>
```
