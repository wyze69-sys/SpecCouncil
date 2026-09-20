# W4 — Deadline-Bounded Role Execution

## Mission

Connect one reserved role to the existing review engine/provider boundary with
hard local timing and the canonical two-call budget. This slice executes and
returns a terminal `review.RoleOutcome`; it does not persist the result, publish
findings, compose a report, or start another role.

Start from clean master at W3 commit `150303f`.

## Allowed files

```text
internal/worker/execute.go
internal/worker/execute_test.go
internal/review/runner.go
internal/review/runner_test.go
docs/ENGINE-CONTRACT.md
docs/cline/worker/00-BUILD-MAP.md
docs/cline/worker/W4-ROLE-EXECUTION.md
```

Do not modify `internal/storage/sqlite`, migrations, `internal/worker/run.go`,
`internal/worker/dispatch.go`, lock files, `internal/provider/provider.go`,
`cmd/speccouncil`, `docs/CANONICAL-FLOW.md`, or README.

## Contract

Add a narrow worker execution seam around the existing `review.RunRole` and
`provider.Provider` APIs. Keep persistence and publication outside this slice.

1. Accept only a reserved role identity, immutable frozen snapshot, provider,
   timing policy, and hard-deadline timestamp. Reject missing/invalid inputs.
2. Before the first provider call, assemble and budget-check the role prompt. If
   it exceeds the configured maximum, return failed/budget_exhausted with zero
   provider calls.
3. Each provider attempt gets a cancellable context whose deadline is the
   earlier of `now + CALL_TIMEOUT` and the session hard deadline. Never start an
   attempt after the hard deadline.
4. Enforce the absolute role budget of two provider calls:
   - initial → transport_retry, or
   - initial → format_repair;
   - never a third call or a retry after the alternate second-call path.
5. Retry only typed retryable transport failures. Fatal provider errors make the
   role failed with no second call. Invalid successful output gets one targeted
   format-repair call when time remains.
6. Backoff, when used between attempts, must be bounded by configured minimum and
   maximum values and must stop on context cancellation or hard deadline. Tests
   must inject deterministic timing/backoff; do not sleep in tests.
7. Preserve strict output validation and evidence-reference checks. Never invent,
   remap, or persist findings in this slice.
8. Provider call metadata in the returned outcome must truthfully report count,
   final purpose, model/tokens where available, and the exact failure category.
9. If the provider blocks, cancellation or the per-attempt deadline must return
   a terminal timeout outcome. No goroutine may be leaked to escape a blocked
   call; use the provider context as the cancellation boundary.
10. No SQLite transaction, publication, composer, dispatch reservation, or
    second role is touched by execution.

If the existing review runner cannot satisfy the per-attempt deadline or
retry-budget contract, make the smallest compatible repair within the allowed
review files and preserve existing tests/API behavior.

## Required regression proof

Use deterministic fake providers and barriers; no sleeps:

1. valid output succeeds with one call and correct metadata;
2. prompt budget exhaustion makes zero provider calls;
3. retryable transport failure permits exactly one transport retry;
4. fatal provider failure makes no second call;
5. invalid output permits exactly one format repair;
6. transport-retry path cannot later format-repair, and format-repair path cannot
   later transport-retry;
7. no execution path makes a third provider call;
8. per-attempt timeout cancels a blocked provider and returns timeout;
9. hard deadline prevents a call from starting and returns timeout;
10. cancellation during backoff prevents the second call;
11. malformed provider output never becomes a finding;
12. worker execution performs no SQLite, publication, composer, dispatch, or
    unrelated goroutine work;
13. concurrent independent roles use independent contexts and do not share
    mutable attempt state;
14. full existing review runner tests remain green.

## Verification

```bash
gofmt -w internal/worker/execute.go internal/worker/execute_test.go internal/review/runner.go internal/review/runner_test.go
test -z "$(gofmt -l .)"
go test -v ./internal/worker -run 'Test(Execute|RoleExecution)' -count=1
go test -v ./internal/review -run 'Test(RunRole|Retry|FormatRepair)' -count=1
go test -race ./internal/worker -run 'Test(Execute|RoleExecution)' -count=1
go test ./... -count=1
go vet ./...
go test -race ./...
git diff --check
```

Also verify scope:

```bash
git diff --name-only | grep -v -E '^(internal/worker/execute\.go|internal/worker/execute_test\.go|internal/review/runner\.go|internal/review/runner_test\.go|docs/ENGINE-CONTRACT\.md|docs/cline/worker/00-BUILD-MAP\.md|docs/cline/worker/W4-ROLE-EXECUTION\.md)$' && echo "FORBIDDEN FILES MODIFIED" || echo "scope clean"
```

Do not push. Do not begin W5. Any unmet requirement means BLOCKED and no commit.

## Commit

```text
Add deadline-bounded role execution
```

## Report

```text
W4 RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests:
- ...
Full gate:
- ...
Acceptance evidence:
- budget:
- retry/repair XOR:
- max calls:
- per-attempt timeout:
- hard deadline:
- cancellation:
- strict validation:
- forbidden persistence/publication scope:
- scope:
Git status:
<exact output>
```
