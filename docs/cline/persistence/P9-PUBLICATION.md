# P9 — Compare-and-Set Role Publication

## Mission

Add only atomic success/failure publication for one reserved role. Start from a
clean tree at P8 acceptance `ecf7879`. No provider execution or composer.

## Scope

Create or modify only:

```text
internal/storage/sqlite/publish.go
internal/storage/sqlite/publish_test.go
docs/ENGINE-CONTRACT.md
```

## Contract

Provide package-internal publication functions using `withImmediate`:

- `PublishRoleSuccess`: requires the role currently be `in_flight`; atomically
  inserts validated findings and basis references, then changes that role to
  `complete` with call metadata and completed timestamp.
- `PublishRoleFailure`: requires the role currently be `in_flight`; atomically
  changes it to `failed` with one canonical error category/message, call metadata,
  and completed timestamp.

Both functions must:

1. Compare-and-set on session ID, role-run ID, and `status = in_flight`.
2. Reject a late, duplicate, or stale publication with a typed conflict and make
   no changes.
3. Never overwrite terminal role state.
4. Keep provider calls outside the transaction; input is already validated.
5. On success, findings and all basis references are inserted in the same
   transaction as the role transition.
6. On failure, insert no findings or references.
7. Preserve the frozen snapshot and session fields.
8. Enforce finding and basis ordering/uniqueness and reject invalid references.
9. Set `completed_at` and preserve exact call count; no third-call behavior.
10. Never compose a session verdict or change session status/counts.

## Tests and gate

Use real temporary databases and P8 reservations. Cover valid success, valid
failure categories, invalid findings rollback, invalid basis rollback, duplicate
publication, stale role publication, concurrent publishers with one winner,
late provider result protection, exact call counts/timestamps, and unchanged
session fields. Prove no provider, worker, HTTP, or composer scope.

```bash
gofmt -w internal/storage/sqlite/publish.go internal/storage/sqlite/publish_test.go
test -z "$(gofmt -l .)"
go test ./internal/storage/sqlite -count=1
go test ./internal/storage/sqlite -count=10
go test ./... -count=1
go vet ./...
git diff --check
```

Commit exactly:

```text
Add compare-and-set role publication
```

Never push from Cline. Any failure means BLOCKED and no commit. Do not begin P10.

## Report

```text
P9 RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests:
- ...
Full gate:
- ...
Acceptance evidence:
- success publication:
- failure publication:
- compare-and-set:
- rollback:
- duplicate/late protection:
- unchanged session:
- forbidden scope:
Remaining limitations:
- ...
Git status:
<exact output>
```
