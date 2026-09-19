# P4 — Atomic Submission and Idempotency

## Mission

Add the persistence submission boundary: deterministic request hash v1, atomic
snapshot/evidence/session/four-role creation, and idempotency replay/conflict.
No HTTP or worker behavior.

Start only from a clean tree at P3 acceptance `18b3621`. Read canonical flow,
build map, P1C, P2, P3, domain roles, and evidence snapshot types.

## Scope

Create or modify only:

```text
internal/storage/sqlite/submit.go
internal/storage/sqlite/submit_test.go
docs/ENGINE-CONTRACT.md
```

## Contract

Provide a package-internal submission function receiving project ID,
idempotency key, title, original content, and an already frozen immutable
`evidence.Snapshot`. It must use `withImmediate` for one database transaction.

Request hash v1 is SHA-256 over deterministic serialization of:

```text
normalization_version = 1
project_id
title
content
```

Use strict UTF-8, no trimming, no Unicode normalization, preserve submitted line
endings, and make field boundaries unambiguous. Hash the exact bytes and document
the encoding. Reject invalid UTF-8 before hashing.

Inside one immediate transaction:

1. Check `(project_id, idempotency_key)`.
2. Existing row with same hash returns the original session identity without
   creating anything.
3. Existing row with different hash returns a typed idempotency conflict.
4. Otherwise insert one immutable snapshot, all ordered evidence units from the
   supplied snapshot, one queued session, and exactly the four canonical role
   runs in `domain.Roles` order.
5. No partial rows may remain on any failure.
6. No provider call, network call, or transaction retry logic may be added.

Do not invent an input splitter. Persistence receives the already frozen snapshot.
Do not add an admission cap. Store `cancel_requested = 0`, queued status, zero
completed roles, incomplete count four, and null claim/timing/terminal fields.

## Tests and gate

Use real temporary databases and barriers where needed, never sleeps. Test exact
hash vectors for line endings, Unicode, invalid UTF-8, field boundaries, and no
trimming; fresh atomic creation; four role order; snapshot/evidence persistence;
same-key same-hash replay; same-key different-hash conflict; rollback on injected
failure; concurrent same-key submissions yielding one winner; and no provider/API
scope.

```bash
gofmt -w internal/storage/sqlite/submit.go internal/storage/sqlite/submit_test.go
test -z "$(gofmt -l .)"
go test ./internal/storage/sqlite -count=1
go test ./internal/storage/sqlite -count=10
go test ./... -count=1
go vet ./...
git diff --check
```

Commit exactly:

```text
Add atomic submission and idempotency
```

Never push from Cline. Any failure means BLOCKED and no commit. Do not begin P5.

## Report

```text
P4 RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests:
- ...
Full gate:
- ...
Acceptance evidence:
- hash v1:
- atomic creation:
- role order:
- replay:
- conflict:
- concurrency:
- rollback:
- forbidden scope:
Remaining limitations:
- ...
Git status:
<exact output>
```
