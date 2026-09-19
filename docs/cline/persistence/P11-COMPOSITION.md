# P11 — Transactional Composition and Terminal Reports

## Mission

Add the persistence boundary that composes a terminal session exactly once from
committed role outcomes and makes the existing deterministic report readable
only after terminalization. Start from a clean tree at P10 acceptance
`7945ee6`.

## Scope

Allowed paths only:

```text
internal/storage/sqlite/compose.go
internal/storage/sqlite/compose_test.go
docs/ENGINE-CONTRACT.md
```

Use the existing `review.Compose` and `review.BuildReport`; do not change the
review package or existing read models except through the new SQLite boundary.

## Contract

Implement a transactional operation that, for one reviewing session:

1. Reads all four committed role runs, findings, and basis references inside one
   `BEGIN IMMEDIATE` transaction.
2. Refuses composition unless all four roles are terminal and no role is pending
   or in-flight; return a typed not-ready error and make no writes.
3. Applies the frozen `review.Compose` verdict rules and deterministic role,
   finding, and citation ordering.
4. Updates the session atomically to `complete`, `partial`, or `failed`, with
   exact terminal reason, completed/incomplete counts, and UTC RFC3339Nano
   `terminal_at`.
5. Uses compare-and-set predicates requiring the session to remain `reviewing`;
   duplicate composition is an idempotent read of the committed terminal result,
   while a stale concurrent composer loses without overwriting terminal state.
6. Never changes immutable snapshot/evidence/finding/reference rows, role rows,
   cancellation state, claim/cutoff/deadline timestamps, or persisted role call
   metadata.
7. Performs no provider call and holds no transaction while doing provider work
   (there is no provider work in this slice).
8. Returns the same deterministic terminal report through the read-only path;
   non-terminal report requests return the existing typed `SessionNotTerminalError`.
9. Rolls back entirely on injected commit failure or malformed persisted role
   data; no partial terminal session is left behind.
10. Preserves project scoping and existing not-found semantics.

## Tests and verification

Use real temporary databases populated through P4/P8/P9/P10 operations. Cover
all-role success, partial with cancellation, partial with completed roles,
failed with no completed roles, reason precedence, non-terminal refusal,
concurrent composers with one writer, repeated composition, report-after-
composition, report-before-composition, project mismatch, rollback, exact
unchanged row snapshots, deterministic report bytes, and forbidden scope.
Use barriers and injected clocks; do not use sleeps.

Run:

```bash
gofmt -w internal/storage/sqlite/compose.go internal/storage/sqlite/compose_test.go
test -z "$(gofmt -l .)"
go test -v ./internal/storage/sqlite -run 'Test(Compose|Finalize|TerminalReport)'
go test ./internal/storage/sqlite -count=1
go test ./internal/storage/sqlite -count=10
go test ./...
go test -race ./...
go vet ./...
git diff --check
```

If `go test -race ./...` is unavailable because the Windows host lacks a C
compiler, report that exact host limitation and still run every other gate.

Commit only after every applicable command passes, using exactly:

```text
Add transactional session composition
```

Do not push. Do not begin P12. Any unmet requirement means BLOCKED and no
commit.

## Report

```text
P11 RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests:
- ...
Full gate:
- ...
Acceptance evidence:
- terminal composition:
- verdict/reason precedence:
- compare-and-set/idempotency:
- report gate and determinism:
- rollback:
- unchanged persisted rows:
- forbidden scope:
Remaining limitations:
- ...
Git status:
<exact output>
```
