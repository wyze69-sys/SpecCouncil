# R4 — Verify Snapshot Hash in Submit

## Mission

Repair only the missing P4 snapshot-hash integrity check. The implementation
base is clean commit `2d95904` (R2 complete; R3 concurrent); execute from
latest master at the packet-release commit that adds this packet only. Do not
fix findings triggers in this slice.

## Defect

`internal/storage/sqlite/submit.go` accepts any caller-supplied `Snapshot.Hash`
after only checking it is non-empty. It never recomputes `evidence.Freeze(id,
units)` to verify the hash matches the actual units. A tampered hash such as
`0000...0000` is persisted and only fails later during read, violating the
build-map P4 requirement: “The store recomputes `evidence.Freeze(snapshot.ID,
snapshot.Units)` and rejects a missing/mismatched snapshot hash.”

## Allowed files

```text
internal/storage/sqlite/submit.go
internal/storage/sqlite/snapshot_hash_test.go
docs/ENGINE-CONTRACT.md
```

Do not modify migrations, `sweep.go`, `publish.go`, `claim.go`, `compose.go`,
or any migration file in this slice. The fixed-width timestamp formatting
from R1 must remain (no `Format(time.RFC3339Nano)` reintroduction).

## Contract

In `internal/storage/sqlite/submit.go`:

1. Before computing `RequestHashV1` or entering the immediate transaction, verify
   the snapshot hash:

```go
frozen, err := evidence.Freeze(params.Snapshot.ID, params.Snapshot.Units)
if err != nil {
    return nil, fmt.Errorf("invalid snapshot: %w", err) // wrap evidence error
}
if frozen.Hash != params.Snapshot.Hash {
    return nil, fmt.Errorf("snapshot hash mismatch for %q: supplied %s != recomputed %s",
        params.Snapshot.ID, params.Snapshot.Hash, frozen.Hash)
}
```

- Use the already-imported `github.com/wyze69-sys/SpecCouncil/internal/evidence` (already present).
- Validation must run before duplicate-key / idempotency checks so no corrupted row is written.
- Error must not include sensitive body content beyond IDs/hashes.
- Existing snapshot unit validation (empty ID, duplicate IDs, invalid kind) is already performed by `evidence.Freeze`; no additional per-unit checks needed here.

2. When the snapshot already exists in `snapshots` with a different hash than the
   recomputed hash, the earlier branch that returns
   `snapshot %q exists with mismatched hash` remains; do not alter that
   behavior. The new check merely rejects the incoming tampered hash before any
   write, so the existing-row mismatch path covers only legitimate concurrent
   races with a different but internally-consistent snapshot ID.

3. Preserve existing retry policy, context cancellation, and rollback semantics.
   No provider calls or schema changes.

## Required regression proof

Add `internal/storage/sqlite/snapshot_hash_test.go` with deterministic tests using real temporary stores (no sleeps):

1. Submit with correct `evidence.Freeze` hash succeeds and the session/snapshot are readable with matching hash.
2. Submit with tampered hash `0000...0000` (64 hex) is rejected (no session/snapshot row created; error contains “hash mismatch”).
3. Submit with randomly wrong 64-char hash is rejected (no partial writes).
4. Submit with empty hash is rejected via existing `ErrNilSnapshot` path (no regression).
5. Submit with duplicated unit IDs is rejected via `evidence.Freeze` (error contains “duplicate unit id”).
6. Submit with invalid unit kind is rejected via `evidence.Freeze`.
7. Idempotency path: same `project_id+idempotency_key` with identical content but tampered second hash is rejected before the replay check.
8. Concurrent submissions with tampered hashes produce zero persisted sessions for tampered callers.

Use `createTestFrozenSnapshot` helper for the valid baseline, then clone and overwrite `Hash` before the second `Submit`. Assert counts via `SELECT COUNT(*) FROM sessions/snapshots/evidence_units` before and after.

## Verification

```bash
gofmt -w internal/storage/sqlite/submit.go internal/storage/sqlite/snapshot_hash_test.go
test -z "$(gofmt -l .)"
go test -v ./internal/storage/sqlite -run TestSnapshotHash
go test -v ./internal/storage/sqlite -run TestTimestamp
go test ./internal/storage/sqlite -count=1
go test ./internal/storage/sqlite -count=10
go test ./... -count=1
go vet ./...
git diff --check
```

Also verify scope:

```bash
git diff --name-only | grep -v -E '^(internal/storage/sqlite/submit\.go|internal/storage/sqlite/snapshot_hash_test\.go|docs/ENGINE-CONTRACT\.md|docs/cline/persistence/R4-SNAPSHOT-HASH\.md)$' && echo "FORBIDDEN FILES MODIFIED" || echo "scope clean"
git grep -n 'Format(time.RFC3339Nano)' -- internal/storage/sqlite/*.go && echo "VARIABLE WIDTH TIMESTAMP FOUND" || echo "timestamp width clean"
```

Do not run or claim `go test -race` unless available.

Commit exactly:

```text
Verify snapshot hash in submit
```

Do not push. Do not fix findings triggers. Any unrelated changed file or unmet regression means BLOCKED and no commit.

## Report

```text
R4 RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests:
- ...
Full gate:
- ...
Acceptance evidence:
- valid hash acceptance:
- tampered hash rejection:
- duplicate/invalid unit rejection:
- idempotency rejection:
- no partial writes:
- forbidden scope:
Git status:
<exact output>
```
