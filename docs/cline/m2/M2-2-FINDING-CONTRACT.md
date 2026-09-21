# M2-2 — Stronger finding and citation contract (DRAFT)

Status: **DRAFT for wyze review.** Not released for execution. M2-2a below becomes
the executable packet once the three decisions at the end are confirmed.

Authority: `docs/cline/persistence/00-BUILD-MAP.md` § M2-2, `docs/ENGINE-CONTRACT.md`
(finding schema, validation staging, report ordering), `docs/CANONICAL-FLOW.md`
(strict validation precedes atomic findings + COMPLETE persistence). M2-2 supersedes
nothing in M1; it extends the finding contract and every extension is additive.

## Why

Current validation proves a cited unit ID *exists* in the frozen snapshot. It does
not prove the citation supports the finding, and the 1–5 `basis_refs` rule forces an
omission finding to cite tangential text just to pass validation. M2-2 makes the
kind of concern explicit and gives omissions an anchor of their own.

## The v2 finding contract

```go
type Finding struct {
	ID             string      `json:"id"`
	Kind           FindingKind `json:"kind"`                  // NEW, required
	Severity       Severity    `json:"severity"`
	Category       string      `json:"category"`
	Issue          string      `json:"issue"`
	Recommendation string      `json:"recommendation"`
	BasisRefs      []string    `json:"basis_refs"`
	AnchorRef      string      `json:"anchor_ref,omitempty"`   // NEW, missing kind only
}
```

`FindingKind` is one of exactly three values:

| Kind | Meaning | `basis_refs` | `anchor_ref` |
|---|---|---|---|
| `existing` | a concern about content that is present | 1..5 | must be empty |
| `conflicting` | a contradiction between two or more cited units | 2..5 | must be empty |
| `missing` | an omission | 0..5 | required, exactly 1 |

The anchor is the unit the omission is *about* — normally a section heading — so an
omission never has to cite unrelated text. It must resolve in the same frozen
snapshot, exactly like a basis ref.

Unchanged and not weakened: at most 15 findings; non-empty unique finding IDs;
canonical severities; non-empty category; issue and recommendation 1..1000
characters; unique refs within a finding; unknown JSON fields rejected; strict
staging (`invalid_json` → `schema_invalid` → `invalid_basis_ref`).

Documentation requirement: state plainly, wherever the contract is described, that
structural citation validation proves a referenced unit exists and is unique — not
that it supports the finding. That sentence exists today in
`docs/ENGINE-CONTRACT.md`; M2-2 keeps it and extends it to the anchor.

Explicit non-goals (M3): cross-review finding identity; `fixed`/`reopened`/
`regressed` lifecycle; baseline versus proposed citation namespaces; severity decay;
anything that ranks findings by model confidence.

## Slice plan

| Slice | Scope | Depends on |
|---|---|---|
| **M2-2a** | `internal/domain` contract + `internal/review` validation and prompt + every provider-response fixture that must now carry `kind` | M2-1 (done) |
| **M2-2b** | Persistence: migration `006` (`findings.kind`, `findings.anchor_unit_id`), publish validation + insert, read/compose reconstruction, report ordering fallback to the anchor | M2-2a |
| **M2-2c** | End-to-end proof (fake provider → worker → SQLite → report), contract docs, independent test round | M2-2b |

M2-2a deliberately does **not** persist `kind`; it only introduces the contract and
its validation. That keeps the schema change isolated and reviewable in M2-2b, where
the migration, the publish path, and the read path change together.

## DRAFT DECISIONS (wyze picks; recommendation is the default the packet assumes)

1. **Is `kind` required on the wire?**
   Recommended: **yes, required.** An empty or unknown kind is rejected as
   `schema_invalid`. Cost: every provider-response fixture in tests and the `cmd/`
   demo scripts must add `"kind"`. Benefit: no silent defaulting of an omission into
   "existing".
2. **May a `missing` finding still carry basis refs?**
   Recommended: **0..5, anchor mandatory.** Zero refs is the normal omission case;
   allowing supporting context refs keeps the field useful. The alternative
   (hard `0`) would reject a legitimate citation of nearby context.
3. **Must a `conflicting` finding cite at least two units?**
   Recommended: **yes, 2..5.** A contradiction needs two sides; one ref cannot
   describe a conflict.

## M2-2a — executable packet (pending decision 1–3)

Slice: M2-2a (contract + validation)
Start commit: current `master` HEAD at release time; verify with `git log -1 --oneline`.
Module: `github.com/wyze69-sys/SpecCouncil`, Go 1.27.1
Windows toolchain: `export PATH="/c/Users/User/sdk/go/bin:$PATH"` before every Go command.

### Mission

Introduce the v2 finding contract in `internal/domain`, enforce it in
`internal/review.DecodeAndValidate`, describe it in the provider prompt, and update
every provider-response fixture so the suite stays green. Do not touch persistence,
the schema, the composer, the worker engine, or the HTTP layer.

### Exact additions to `internal/domain/finding.go`

```go
// FindingKind is the kind of concern a finding raises.
type FindingKind string

const (
	FindingExisting    FindingKind = "existing"
	FindingConflicting FindingKind = "conflicting"
	FindingMissing     FindingKind = "missing"
)

// IsValidFindingKind reports whether k is a canonical finding kind.
func IsValidFindingKind(k FindingKind) bool {
	switch k {
	case FindingExisting, FindingConflicting, FindingMissing:
		return true
	}
	return false
}

// BasisRefsBounds returns the inclusive basis_refs count allowed for a finding
// kind, and whether the kind is known.
func BasisRefsBounds(k FindingKind) (min, max int, ok bool) {
	switch k {
	case FindingExisting:
		return 1, 5, true
	case FindingConflicting:
		return 2, 5, true
	case FindingMissing:
		return 0, 5, true
	}
	return 0, 0, false
}
```

`Finding` gains `Kind FindingKind \`json:"kind"\`` immediately after `ID`, and
`AnchorRef string \`json:"anchor_ref,omitempty"\`` after `BasisRefs`. Keep
`MinBasisRefsPerFinding`/`MaxBasisRefsPerFinding` unchanged for compatibility, and
add `const MaxAnchorRefsPerFinding = 1`.

### Exact validation rules in `internal/review/validate.go`

Structural stage (`validateStructure`, category `schema_invalid`). New checks run
per finding `i`, after the existing recommendation-length check and before the
per-ref checks, in this exact order:

1. empty kind → `findings[i]: empty kind`
2. unknown kind → `findings[i]: unknown kind "x"`
3. basis ref bounds from `domain.BasisRefsBounds(kind)`, using
   `MaxAnchorRefsPerFinding` for the anchor check in step 5 →
   `findings[i]: kind "existing" allows 1..5 basis_refs, got 0`
   (format `findings[%d]: kind %q allows %d..%d basis_refs, got %d`); this
   replaces the current fixed `1..5` check
4. `existing` or `conflicting` with a non-empty `anchor_ref` →
   `findings[i]: anchor_ref is only allowed for a missing finding`
5. `missing` with an empty `anchor_ref` →
   `findings[i]: missing finding requires exactly one anchor_ref`
6. `missing` whose `anchor_ref` duplicates a value in its own `basis_refs` is
   allowed (the anchor may also be cited as context); no rule is added for it

Then the existing per-ref checks (non-empty, unique) run unchanged for basis refs,
and the anchor (when present) must be non-empty — which step 5 already guarantees.

Semantic stage (`validateSemantics`, category `invalid_basis_ref`) runs the existing
snapshot membership check for every basis ref and adds, for a non-empty anchor:
`findings[i] anchors "X", which is not in snapshot <id>`.

### Exact prompt contract text (`internal/review/prompt.go`)

Replace `outputContract` with the v2 description, keeping the same voice and rules:

```text
Return one JSON object and nothing else.

{
  "findings": [
    {
      "id": "string, unique within this response",
      "kind": "existing | conflicting | missing",
      "severity": "critical | high | medium | low",
      "category": "short label",
      "issue": "string, 1..1000 characters",
      "recommendation": "string, 1..1000 characters",
      "basis_refs": ["unit ids taken from the evidence above"],
      "anchor_ref": "one unit id, only for kind missing"
    }
  ]
}

Rules:
- At most 15 findings.
- kind existing: 1 to 5 basis_refs about content that is present.
- kind conflicting: 2 to 5 basis_refs, citing the units that contradict each other.
- kind missing: anchor_ref is required and names the unit the omission is about
  (usually a section heading); basis_refs may be empty or hold supporting context.
- anchor_ref is only allowed for kind missing.
- Every basis_ref and anchor_ref must be a unit id that appears in the evidence above.
- An empty findings list is a valid answer when the design raises no concern.
- Unknown fields are rejected. Return no prose outside the JSON object.
```

### Fixture sweep (required, mechanical)

Run `grep -rn 'basis_refs' --include=*_test.go .` and
`grep -rn 'basis_refs' cmd/` and add `"kind"` to every provider-response JSON body
that goes through `DecodeAndValidate` or the runner. Known sites to start from:

- `internal/review/validate_test.go` (`mkResult` helper plus its inline bodies)
- `internal/review/runner_test.go`, `internal/review/engine_test.go`
- `internal/worker/execute_test.go`, `internal/worker/supervise_test.go`
- `cmd/speccouncil/main.go` (built-in demo scripts)
- any api or sqlite test that feeds a provider body rather than a struct literal

Struct literals (`domain.Finding{...}`) do **not** need `Kind` in this slice,
because only the decode path enforces it here. Do not add kind checks to
`internal/storage/sqlite/publish.go` in this slice.

### Required tests in this slice

`internal/domain`: `IsValidFindingKind` truth table over the three canonical values
plus `""`, `"Existing"`, `" omission"`; `BasisRefsBounds` returns
`(1,5,true)`, `(2,5,true)`, `(0,5,true)`, `(0,0,false)` for an unknown kind.

`internal/review` (`validate_test.go`) — exact cases, each asserting the exact
message and category:

| Case | Input shape | Expected |
|---|---|---|
| valid existing | kind existing, 1 ref | accepted |
| valid conflicting | kind conflicting, 2 refs | accepted |
| valid missing with anchor | kind missing, 0 refs, anchor `H-1` | accepted |
| valid missing with context | kind missing, 2 refs, anchor `H-1` | accepted |
| empty kind | `"kind":""` | `schema_invalid`, `findings[0]: empty kind` |
| unknown kind | `"kind":"omission"` | `schema_invalid`, `findings[0]: unknown kind "omission"` |
| existing with 0 refs | kind existing, no refs | `schema_invalid`, `findings[0]: kind "existing" allows 1..5 basis_refs, got 0` |
| conflicting with 1 ref | kind conflicting, 1 ref | `schema_invalid`, `findings[0]: kind "conflicting" allows 2..5 basis_refs, got 1` |
| existing with anchor | kind existing, anchor set | `schema_invalid`, `findings[0]: anchor_ref is only allowed for a missing finding` |
| missing without anchor | kind missing, no anchor | `schema_invalid`, `findings[0]: missing finding requires exactly one anchor_ref` |
| missing with unknown anchor | kind missing, anchor `NOPE` | `invalid_basis_ref`, `findings[0] anchors "NOPE", which is not in snapshot snap-1` |
| unknown field still rejected | adds `"confidence":0.9` | `invalid_json` (unchanged) |

`internal/review` (`runner_test.go`): the existing repair-path tests must still pass
with v2 bodies; add one case where a v2 schema error (bad kind) triggers exactly one
format repair, and the repaired body with a valid kind succeeds.

### Gates

```bash
export PATH="/c/Users/User/sdk/go/bin:$PATH"
cd /d/PROJECT/SpecCouncil
gofmt -l .                        # empty
go vet ./...                      # clean
go test ./internal/domain ./internal/review -count=10
go test ./... -count=1            # every package green
git add <only the allowed paths>
git diff --cached --name-only     # exactly the allowed list
git status --short                # only those paths staged
```

Race: native Windows `-race` is blocked (no cgo compiler). The verifier runs
`go test -race ./internal/domain ./internal/review` in WSL Ubuntu
(`~/sdk/go/bin/go`). Report the race gate as UNKNOWN/BLOCKED unless you ran it.

### Allowed paths

- `internal/domain/finding.go`
- `internal/review/validate.go`
- `internal/review/prompt.go`
- `internal/domain/*_test.go`, `internal/review/*_test.go`
- `internal/worker/*_test.go`
- `internal/api/*_test.go` (only if such a fixture exists and carries a provider body)
- `cmd/speccouncil/main.go` (demo script JSON only)

### Forbidden

`internal/storage/sqlite/**` (production and migrations — slice M2-2b),
`internal/evidence/**`, `internal/provider/**`, `internal/api/**`,
`internal/worker/*.go` non-test files, `go.mod`, `go.sum`, `docs/**`,
`.gitattributes`. No new dependencies. No schema or persistence change. No push.

### Commit

One commit, message exactly:

```
Add finding kind and omission anchor contract
```

### Completion report format

```
SLICE: M2-2a
START: <sha> / FINAL: <sha>
CHANGED: <git diff --cached --name-only>
CONTRACT: <kind values, ref bounds per kind, anchor rule>
VALIDATION: <exact new messages and the order they are evaluated in>
FIXTURES: <how many provider-response bodies updated, in which files, plus `grep -c` numbers>
TESTS: <new test names + what each pins>
GATES:
  gofmt -l .                     -> <empty|output>
  go vet ./...                   -> <clean|output>
  go test domain+review -count=10 -> <ok|fail>
  go test ./... -count=1          -> <per-package result>
  grep -rn 'basis_refs' --include=*_test.go . -> <confirm every JSON body has kind>
RACE: UNKNOWN/BLOCKED | <output>
PUSHED: no
NOTES: <deviations, or NONE>
```
