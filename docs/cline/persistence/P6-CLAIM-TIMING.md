# P6 — FIFO Claim and Timing Policy

## Mission

Add the atomic single-session FIFO claim and validated timing policy. Start from
clean latest P5 acceptance `8890be5`. No worker loop or dispatch.

## Scope

Create or modify only:

```text
internal/storage/sqlite/claim.go
internal/storage/sqlite/claim_test.go
docs/ENGINE-CONTRACT.md
```

Read the canonical flow, build map, P1C, P2-P5 packets, domain statuses, and
existing schema before coding.

## Contract

Define a validated timing policy with positive bounded durations:

```text
DispatchCutoff
CallTimeout
SessionHardDeadline
```

Require:

```text
SessionHardDeadline >= DispatchCutoff + CallTimeout
```

Reject zero, negative, overflow, and impossible relationships. Keep numeric
values configuration-driven; do not invent a production default in this slice.

Provide a package-internal atomic claim function using `withImmediate`:

1. At most one queued session may be claimed when another session is already in
   `reviewing`; return a typed no-work result, not an error.
2. Select FIFO by `created_at ASC, id ASC`.
3. Update exactly one `queued` session to `reviewing`.
4. In the same transaction set `claimed_at = now`,
   `dispatch_cutoff_at = now + DispatchCutoff`, and
   `hard_deadline_at = now + SessionHardDeadline`.
5. Set no role to in_flight and perform no provider work.
6. If the guarded update affects zero rows, return no-work and leave state alone.
7. Use one documented UTC RFC3339Nano `Z` representation and an injectable clock
   for deterministic tests.
8. A second claim while a session is reviewing must not claim another session.
9. Never claim terminal, malformed, or already reviewing sessions.

## Tests and gate

Use real temporary databases and deterministic clocks/barriers, never sleeps.
Test policy validation and overflow; FIFO tie-breaking; one-row claim; exact
cutoff/deadline values; no-work with no queued session; no-work with active
reviewing session; concurrent claimers yielding one winner; and rollback on
injected failure. Prove roles remain pending and no provider/worker/API code is
introduced.

```bash
gofmt -w internal/storage/sqlite/claim.go internal/storage/sqlite/claim_test.go
test -z "$(gofmt -l .)"
go test ./internal/storage/sqlite -count=1
go test ./internal/storage/sqlite -count=10
go test ./... -count=1
go vet ./...
git diff --check
```

Commit exactly:

```text
Add FIFO session claim and timing policy
```

Never push from Cline. Any failure means BLOCKED and no commit. Do not begin P7.

## Report

```text
P6 RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests:
- ...
Full gate:
- ...
Acceptance evidence:
- policy validation:
- FIFO ordering:
- single active session:
- atomic timing fields:
- concurrency:
- rollback:
- forbidden scope:
Remaining limitations:
- ...
Git status:
<exact output>
```
