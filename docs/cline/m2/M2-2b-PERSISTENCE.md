# M2-2b — Persist the finding kind and omission anchor

Slice: M2-2b (persistence)
Start commit: current `master` HEAD at release time (`839a6be` when written).
Depends on: M2-2a (`839a6be`, verified).
Module: `github.com/wyze69-sys/SpecCouncil`, Go 1.27.1
Windows toolchain: `export PATH="/c/Users/User/sdk/go/bin:$PATH"` before every Go command.

## Mission

M2-2a added `kind` and `anchor_ref` to the in-memory contract only. Persist them,
enforce the same rules at the database layer, and reconstruct them on read so the
terminal report shows the same finding the provider produced. No schema change to
any table other than the two new `findings` columns, and no change to how basis
refs are stored.

## Verified environment facts (probed against this repo's SQLite driver)

Checked before writing this packet, so the executor does not have to guess:

- `ALTER TABLE ... ADD COLUMN` **with** a `CHECK` constraint is accepted.
- `ALTER TABLE ... ADD COLUMN anchor_unit_id TEXT REFERENCES evidence_units(id) ON DELETE RESTRICT`
  is accepted (nullable column, so the FK default rule is satisfied).
- Two additional `BEFORE INSERT` triggers on `findings` coexist with the existing
  `findings_insert_in_flight_guard` from migration 004.
- A legacy insert that omits `kind` stores `'existing'` (the column default).
- An out-of-enum value is rejected by the column CHECK.

## Migration `007_finding_kind_and_anchor.sql`

New file: `internal/storage/sqlite/migrations/007_finding_kind_and_anchor.sql`.
The filename must match `^[0-9]{3}_[a-z][a-z0-9_]*\.sql$` (discovery walks the
embedded directory; `//go:embed *` picks it up automatically, no manifest edit).
`006_normalize_legacy_timestamps.sql` already exists — do not reuse 006.

Exact contents:

```sql
-- 007_finding_kind_and_anchor.sql
-- Persist the v2 finding contract: the kind of concern a finding raises and, for
-- omissions, the anchor unit the omission is about.

ALTER TABLE findings ADD COLUMN kind TEXT NOT NULL DEFAULT 'existing'
  CHECK (kind IN ('existing', 'conflicting', 'missing'));

ALTER TABLE findings ADD COLUMN anchor_unit_id TEXT REFERENCES evidence_units(id) ON DELETE RESTRICT;

CREATE TRIGGER findings_missing_requires_anchor
BEFORE INSERT ON findings
WHEN NEW.kind = 'missing' AND NEW.anchor_unit_id IS NULL
BEGIN
  SELECT RAISE(ABORT, 'missing finding requires an anchor');
END;

CREATE TRIGGER findings_anchor_only_for_missing
BEFORE INSERT ON findings
WHEN NEW.kind != 'missing' AND NEW.anchor_unit_id IS NOT NULL
BEGIN
  SELECT RAISE(ABORT, 'anchor is only allowed for a missing finding');
END;

CREATE TRIGGER findings_anchor_snapshot_guard
BEFORE INSERT ON findings
WHEN NEW.anchor_unit_id IS NOT NULL
AND (
  SELECT eu.snapshot_id FROM evidence_units eu WHERE eu.id = NEW.anchor_unit_id
) IS NOT NULL
AND (
  SELECT s.snapshot_id FROM role_runs rr JOIN sessions s ON s.id = rr.session_id
  WHERE rr.id = NEW.role_run_id
) IS NOT NULL
AND (
  SELECT eu.snapshot_id FROM evidence_units eu WHERE eu.id = NEW.anchor_unit_id
) != (
  SELECT s.snapshot_id FROM role_runs rr JOIN sessions s ON s.id = rr.session_id
  WHERE rr.id = NEW.role_run_id
)
BEGIN
  SELECT RAISE(ABORT, 'cross-snapshot anchor rejected');
END;
```

## Stale assertions this migration makes obsolete (narrow compatibility repairs)

These are the only existing test expectations that must change; change nothing else:

- `internal/storage/sqlite/schema_test.go:61-63` — the `findings` column list gains
  `kind` and `anchor_unit_id` (the exact list is asserted at line 63).
- `internal/storage/sqlite/migrate_test.go:522`, `:541`, `:569` — the three
  `len(applied) != 6` assertions become `!= 7`.

Do **not** weaken checksum, rollback, manifest, or idempotency assertions, and do
not touch the 006 timestamp expectations.

## `internal/storage/sqlite/publish.go`

Validation (`validatePublishSuccessParams`, `publish.go:115`), inside the existing
per-finding loop, adding kind rules that mirror `internal/review` but keep this
package's error style (`PublicationValidationError` with `Field`/`Message`):

- empty or whitespace `kind` → field `findings[i].kind`, message `must not be empty`
- unknown kind → field `findings[i].kind`, message `invalid kind %q`
- ref count outside `domain.BasisRefsBounds(f.Kind)` → field `findings[i].basis_refs`,
  message `basis_refs count %d must be between %d and %d` (same wording as today,
  with the kind-aware bounds)
- `existing`/`conflicting` with a non-empty `AnchorRef` → field
  `findings[i].anchor_ref`, message `must be empty unless the finding kind is missing`
- `missing` with an empty `AnchorRef` → field `findings[i].anchor_ref`, message
  `must not be empty when the finding kind is missing`
- anchor membership: validated in the in-transaction loop that already resolves
  `unitIDToDBID` for basis refs. An anchor that is not in `unitIDToDBID` → field
  `findings[i].anchor_ref`, message
  `anchor ref %q does not exist in session snapshot %q` (mirrors the basis-ref
  wording at the same place).

Insert: extend the `INSERT INTO findings` statement with `kind` and
`anchor_unit_id` (value `unitIDToDBID[f.AnchorRef]` when `AnchorRef != ""`, else
`nil`). Immediate-mode binding order stays as it is.

## `internal/storage/sqlite/read.go`

- The findings query (`queryFindings`, near line 560) becomes:

```sql
SELECT f.id, rr.role, f.finding_id, f.kind, f.severity, f.category, f.issue,
       f.recommendation, COALESCE(eu.unit_id, '')
FROM findings f
JOIN role_runs rr ON f.role_run_id = rr.id
LEFT JOIN evidence_units eu ON eu.id = f.anchor_unit_id
WHERE rr.session_id = ? AND rr.status = 'complete';
```

- Scan the two new columns; reject an invalid kind with
  `fmt.Errorf("%w: invalid finding kind %q", ErrMalformedData, kindStr)`.
- Reconstruct `Kind` and `AnchorRef` on the returned `domain.Finding`.
- `primaryRef` (near line 755) falls back to `AnchorRef` when `BasisRefs` is empty.
- The existing ordinal bounds check on `finding_basis_refs` stays exactly as is.

## `internal/storage/sqlite/compose.go`

- Same query/scan/reconstruct change as `read.go` (findings query near line 410).
- The local `primaryRef` equivalent used for report ordering must use the same
  anchor fallback.

## `internal/review/composer.go`

- `primaryRef` (line 240) falls back to `AnchorRef` when `BasisRefs` is empty, so
  the composed order and the DB-reconstructed order agree for omission findings.
- The frozen order key is otherwise unchanged: severity rank, role rank, primary
  ref, category, finding id.

## Required tests

`internal/storage/sqlite` (raw SQL, real store, following existing file style):

1. Guard test: inserting `kind='missing'` with `anchor_unit_id IS NULL` aborts with
   `missing finding requires an anchor`.
2. Guard test: inserting `kind='existing'` with a non-null anchor aborts with
   `anchor is only allowed for a missing finding`.
3. Guard test: inserting a `missing` finding whose anchor belongs to a different
   session's snapshot aborts with `cross-snapshot anchor rejected`.
4. Guard test: `kind='omission'` is rejected by the column CHECK.
5. Migration test: exactly 7 applied migrations after `Migrate`, with the three
   stale `6` assertions updated (do not add new assertions beyond the count).
6. Publish round-trip: a `missing` finding with anchor `H-1` and no basis refs
   publishes successfully, and `ReadReportScoped` returns it with `Kind == missing`
   and `AnchorRef == "H-1"`; a `conflicting` finding with two refs returns
   `Kind == conflicting` and an empty anchor.
7. Publish rejection: each new validation error above is asserted by `Field` and
   `Message` exactly.
8. Ordering: a report containing a `missing` finding with no refs and an anchor
   sorts deterministically by the anchor as its primary ref — assert the exact
   order of the returned findings, and assert the composed order
   (`review.BuildReport`) equals the reconstructed order from the store.

`internal/review`: a composer ordering test name for the anchor fallback, pinning
that a zero-ref missing finding sorts by `AnchorRef`.

Existing behaviour that must not change: failed/interrupted roles still contribute
zero findings; basis refs still ordered by ordinal; the 1..5 ordinal CHECK and
`finding_basis_refs` guards are untouched.

## Gates

```bash
export PATH="/c/Users/User/sdk/go/bin:$PATH"
cd /d/PROJECT/SpecCouncil
gofmt -l .                                  # empty
go vet ./...                                # clean
go test ./internal/storage/sqlite -count=3  # storage suite (slow, ~1 min)
go test ./... -count=1                      # every package green
go run ./cmd/speccouncil                    # demo still exits 0 with 4 complete roles
git add <only the allowed paths>
git diff --cached --name-only               # exactly the allowed list
git status --short                          # only those paths staged
```

Race: the verifier runs `go test -race ./internal/review ./internal/storage/sqlite`
in WSL Ubuntu (`export PATH=$HOME/sdk/go/bin:$PATH`, gcc 13.3 is present). Report
`UNKNOWN/BLOCKED` only with the exact command and error you actually saw.

## Allowed paths

- `internal/storage/sqlite/migrations/007_finding_kind_and_anchor.sql` (new)
- `internal/storage/sqlite/publish.go`
- `internal/storage/sqlite/read.go`
- `internal/storage/sqlite/compose.go`
- `internal/storage/sqlite/*_test.go` (the guards, publish, read, compose, schema,
  and migrate tests named above)
- `internal/review/composer.go`
- `internal/review/composer_test.go`

## Forbidden

`internal/domain/**`, `internal/review/validate.go`, `internal/review/prompt.go`,
`internal/review/runner.go`, `internal/worker/**`, `internal/api/**`,
`internal/evidence/**`, `internal/ingest/**`, `cmd/**`, migrations `001`–`006`
(never edit an applied migration), `go.mod`, `go.sum`, `docs/**`, `.gitattributes`.
No new dependencies. No push.

## Commit

One commit, message exactly:

```
Persist finding kind and omission anchor
```

## Completion report format

```
SLICE: M2-2b
START: <sha> / FINAL: <sha>
CHANGED: <git diff --cached --name-only>
MIGRATION: <filename, statements added, applied count after Migrate>
STALE ASSERTIONS UPDATED: <file:line list, and only those>
PUBLISH: <validation errors added + insert columns>
READ/COMPOSE: <query change, reconstruction, primaryRef fallback>
GUARDS: <the four raw-SQL guard results, verbatim error text>
TESTS: <new test names and what each asserts>
GATES:
  gofmt -l .                          -> <empty|output>
  go vet ./...                        -> <clean|output>
  go test storage/sqlite -count=3     -> <ok|fail>
  go test ./... -count=1              -> <per-package>
  go run ./cmd/speccouncil            -> <exit code + status line>
RACE: UNKNOWN/BLOCKED | <output>
PUSHED: no
NOTES: <deviations, or NONE>
```
