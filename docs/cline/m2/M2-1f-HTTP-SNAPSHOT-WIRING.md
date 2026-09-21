# M2-1f — Wire Ingestion Snapshot into HTTP Submit

## Mission

Final ingestion slice. Provide a concrete `api.SnapshotProvider` built from
`ingest.BuildSnapshot`, so the HTTP submit path produces a real frozen snapshot
instead of an empty one. This closes the W7 nil-snapshot blocker: with the
provider installed, `sqlite.Submit` receives a valid snapshot and no longer
rejects with `ErrNilSnapshot`.

This slice does the wiring ONLY. It does NOT change ingestion logic, the snapshot
hash, `sqlite.Submit`, the worker, the provider, or review. It touches
`internal/api` at exactly one existing seam plus one new file.

## Why this slice exists

`ErrNilSnapshot` (submit.go): `sqlite.Submit` requires
`Snapshot.ID`, `Snapshot.Hash`, and `Snapshot.Units` all non-empty. The HTTP
handler only sets `params.Snapshot` when `s.snapshotProvider != nil`
(server.go). Until now no provider existed, so real HTTP submission could not
build the snapshot. This slice supplies that provider and maps its one client
error to a 400.

## Starting point

- Start from a clean tree at current `master` HEAD (M2-1e commit `71d78f2` or
  later). Confirm `git status --short` is clean before editing.
- Module `github.com/wyze69-sys/SpecCouncil`, Go 1.27.1.

## Authority (read in this order)

1. `docs/CANONICAL-FLOW.md` — runtime authority.
2. `docs/cline/persistence/00-BUILD-MAP.md` — M2 plan; this is slice M2-1f, the
   HTTP snapshot hook that closes the nil-snapshot blocker.
3. `internal/api/server.go` — READ these exact regions:
   - lines 44-53: `SnapshotProvider` type and `Config.SnapshotProvider`.
   - lines 331-345: submit handler builds `SubmitParams` and calls the provider.
   - lines 138-145: `handlePersistenceError` (ErrNotFound→404, else→503).
   - lines 129-136: `writeError` helper.
4. `internal/ingest/snapshot.go` — `BuildSnapshot`, `IngestResult`,
   `ErrNoEvidenceUnits` (read; do not modify).
5. `internal/storage/sqlite/submit.go` — lines 119-172: `SubmitParams`, the
   `ErrNilSnapshot` guard, and the snapshot hash re-check (read; do not modify).

Canonical flow wins on any conflict. Do not edit `docs/CANONICAL-FLOW.md`.

## Fixed facts from the code (do not re-derive)

- `api.SnapshotProvider` is `func(ctx context.Context, projectID, title, content string) (evidence.Snapshot, error)` (server.go:45).
- The handler already does (server.go:338-345):
  ```go
  if s.snapshotProvider != nil {
      snap, err := s.snapshotProvider(r.Context(), projectID, *req.Title, *req.Content)
      if err != nil {
          s.handlePersistenceError(w, err)
          return
      }
      params.Snapshot = snap
  }
  ```
- `sqlite.Submit` requires `Snapshot.ID`, `Snapshot.Hash`, `Snapshot.Units` all
  non-empty, then re-runs `evidence.Freeze` and checks the hash matches
  (submit.go:161-172). So the provider MUST return a snapshot whose `Hash` was
  produced by `evidence.Freeze` over its own `Units` — `ingest.BuildSnapshot`
  already does exactly this.
- `api` importing `ingest` is safe: `ingest` imports only `internal/evidence`,
  never `internal/api`. No import cycle.

## Scope

Create only:

```text
internal/api/snapshot_provider.go
internal/api/snapshot_provider_test.go
```

Modify only:

```text
internal/api/server.go   (see the ONE surgical change below)
docs/ENGINE-CONTRACT.md  (append a short "HTTP snapshot wiring (M2-1f)" note;
                          update the nil-snapshot blocker note to CLOSED)
```

Do NOT modify `internal/ingest`, `internal/evidence`, `internal/storage`,
`internal/worker`, `internal/review`, `internal/provider`, `internal/domain`,
`cmd/`, or any `server.go` code outside the single change specified below.

Forbidden in this slice:

- changing ingestion logic, the snapshot hash, `sqlite.Submit`, or `SubmitParams`;
- changing any existing `server.go` behavior other than the one provider-error
  branch specified below (do not touch auth, routing, other handlers, or the
  existing 404/503 mapping for non-ingest errors);
- persisting the splitter version (no schema change; `submit.go` already writes
  `normalization_version = 1`; the version is dropped by the adapter for now);
- network, filesystem, time, or randomness in the adapter.

## Contract

### New file `internal/api/snapshot_provider.go`

```go
package api

import (
    "context"
    "crypto/sha256"
    "encoding/hex"

    "github.com/wyze69-sys/SpecCouncil/internal/evidence"
    "github.com/wyze69-sys/SpecCouncil/internal/ingest"
)

// NewIngestSnapshotProvider returns a SnapshotProvider that builds a frozen
// evidence snapshot from the submitted content using the deterministic
// ingestion pipeline (ingest.BuildSnapshot).
//
// The snapshot ID is derived deterministically from (projectID, title, content)
// so identical resubmissions reuse the same snapshot row, and distinct projects
// or titles never collide on one ID. The ingestion splitter version is not
// persisted in this slice (submit writes normalization_version = 1); it is
// intentionally dropped here.
//
// It returns ingest.ErrNoEvidenceUnits unchanged when the content yields no
// evidence, so the caller can map it to a client error.
func NewIngestSnapshotProvider() SnapshotProvider {
    return func(ctx context.Context, projectID, title, content string) (evidence.Snapshot, error) {
        id := snapshotID(projectID, title, content)
        res, err := ingest.BuildSnapshot(id, content)
        if err != nil {
            return evidence.Snapshot{}, err
        }
        return res.Snapshot, nil
    }
}

// snapshotID derives a stable, project-scoped snapshot identifier.
func snapshotID(projectID, title, content string) string {
    h := sha256.New()
    // NUL separators keep the fields unambiguous across concatenation.
    h.Write([]byte(projectID))
    h.Write([]byte{0})
    h.Write([]byte(title))
    h.Write([]byte{0})
    h.Write([]byte(content))
    return "snap-" + hex.EncodeToString(h.Sum(nil))
}
```

Pin exactly:

- `NewIngestSnapshotProvider` takes no arguments and returns `SnapshotProvider`.
- The ID formula is `"snap-" + hex(sha256(projectID || 0x00 || title || 0x00 || content))`.
  Do NOT include anything else (no clock, no counter, no randomness).
- The adapter returns the provider error unchanged (so `errors.Is(err,
  ingest.ErrNoEvidenceUnits)` still works at the call site).

### The ONE surgical change in `server.go`

In the submit handler (the block at lines 338-345), add a single branch so the
`ingest.ErrNoEvidenceUnits` sentinel maps to HTTP 400 instead of 503. Replace:

```go
        if err != nil {
            s.handlePersistenceError(w, err)
            return
        }
```

with:

```go
        if err != nil {
            if errors.Is(err, ingest.ErrNoEvidenceUnits) {
                writeError(w, http.StatusBadRequest, "bad_request", "content produced no reviewable evidence")
                return
            }
            s.handlePersistenceError(w, err)
            return
        }
```

Add the `github.com/wyze69-sys/SpecCouncil/internal/ingest` import to
`server.go`. `errors`, `net/http`, and `writeError` are already present. Change
NOTHING else in `server.go`.

## Tests and gate

Tests in `internal/api/snapshot_provider_test.go` (package `api` internal test,
so it can use unexported `snapshotID`; if the existing `server_test.go` uses
`package api`, match it — otherwise use `package api` for this file). Cover:

- **provider builds a valid snapshot**: call the provider from
  `NewIngestSnapshotProvider()` with a multi-kind content string; assert no
  error, `Snapshot.ID` has prefix `snap-`, `Snapshot.Hash` non-empty,
  `len(Snapshot.Units) > 0`, and the snapshot satisfies `sqlite.Submit`'s
  non-empty guard (ID, Hash, Units all non-empty). Assert an author ID in the
  content (e.g. `REQ-12 ...`) is addressable via `Snapshot.HasRef("REQ-12")`.
- **determinism**: same `(projectID, title, content)` → identical `Snapshot.ID`
  and `Snapshot.Hash` on two calls.
- **project/title scoping**: different `projectID` (same title+content) → different
  `Snapshot.ID`; different `title` (same projectID+content) → different
  `Snapshot.ID`; identical inputs → identical ID (assert exact equality, not just
  difference).
- **no-evidence content → sentinel**: content `"   "` (or `""`) makes the
  provider return `errors.Is(err, ingest.ErrNoEvidenceUnits)` true and a zero
  `evidence.Snapshot`.
- **handler mapping (end-to-end through the HTTP server)**: using the existing
  `server_test.go` harness pattern, construct a server with
  `SnapshotProvider: NewIngestSnapshotProvider()` and a store test double that
  records the `SubmitParams` it receives (or the existing fake store). Then:
  - POST a valid submit (non-empty title + real multi-line content) → assert the
    store's `Submit` received a `SubmitParams` whose `Snapshot.ID/Hash/Units` are
    all non-empty (i.e. the nil-snapshot blocker is closed), and the HTTP status
    is the harness's success status.
  - POST a submit whose content is structure-only/blank (e.g. `"   "`) → assert
    HTTP status `400` and error code `bad_request`, and that the store's `Submit`
    was NOT called.

The existing `server_test.go` (package `api`) already has a `fakeStore` that
records every `SubmitParams` in `submitCalls` and exposes a `submitFunc` hook and
a `setupTestServer` helper. REUSE it: build the server with
`Config{Store: store, Authenticator: ..., Authorizer: ..., SnapshotProvider:
NewIngestSnapshotProvider()}` (mirror `setupTestServer`, just adding the
provider) and read `store.submitCalls` to inspect the captured `Snapshot`. Assert
"store.Submit NOT called" via `len(store.submitCalls) == 0`. Do NOT add a new
store double and do NOT change production code for the test.

Run and pass:

```bash
gofmt -w internal/api/snapshot_provider.go internal/api/snapshot_provider_test.go internal/api/server.go
test -z "$(gofmt -l .)"
go test ./internal/api -count=1
go test ./internal/api -count=10
go test ./... -count=1
go vet ./...
git diff --check
git status --short
```

`-count=10` must pass every run. Race gate: this slice adds no concurrency, but
`internal/api` may already have concurrent tests. Do NOT report a native Windows
`-race` run as PASS (`-race requires cgo`); if a race run is done it is WSL
Ubuntu only, reported by the actual environment.

## Commit

Commit exactly (only after every gate passes):

```text
Wire deterministic ingestion snapshot into HTTP submit
```

Never push. Any failure means BLOCKED, no commit.

## Report

```text
M2-1f RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests (-count=1 and -count=10):
- ...
Full gate (test/vet/gofmt/diff/status):
- ...
Acceptance evidence:
- provider builds valid snapshot (snap- prefix, hash, units, HasRef author ID):
- determinism (same inputs -> same ID + hash):
- scoping (project/title change -> different ID; identical -> identical ID):
- no-evidence content -> ErrNoEvidenceUnits sentinel + zero snapshot:
- handler success: store.Submit received non-empty Snapshot (nil-snapshot blocker CLOSED):
- handler blank content -> 400 bad_request, store.Submit NOT called:
- server.go change is exactly the one provider-error branch + ingest import (no other diff):
- forbidden scope untouched (ingest/evidence/sqlite/worker/review/provider/domain/cmd unmodified):
Remaining limitations:
- splitter version not persisted (submit writes normalization_version = 1); dropped by adapter:
- Freeze errors other than the no-units sentinel (e.g. whitespace-only code unit) still map to 503, not 400:
- provider not yet wired in cmd/ (no HTTP server composition in main.go); seam proven via api tests:
Git status:
<exact output>
```
