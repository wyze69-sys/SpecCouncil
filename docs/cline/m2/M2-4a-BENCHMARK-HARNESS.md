# M2-4a — Four-arm benchmark harness + scoring (deterministic, zero-cost)

Slice: M2-4a (validation tooling)
Start commit: current `master` HEAD at release time (`11d574f` when written).
Depends on: M2-3 (`0771c10`) — real provider exists; M2-2 finding contract.
Module: `github.com/wyze69-sys/SpecCouncil`, Go 1.27.1
Windows toolchain: `export PATH="/c/Users/User/sdk/go/bin:$PATH"` before every Go command.

## Why this is split from the live run

M2-4 is the first validation slice that spends real money on many model calls.
Everything except the paid calls can be built and fully proven for $0 against the
deterministic fake provider. This slice (M2-4a) builds and unit-tests the entire
harness — arms, case fixtures, seeded defects, scoring, aggregation, report
rendering — driven by the fake provider only. The live paid runs are a separate
packet (M2-4b) gated behind an explicit cost cap and wyze's go-ahead. **M2-4a makes
zero network calls and needs no API key.**

## Frozen definitions (from `docs/CANONICAL-FLOW.md` §4 and BUILD-MAP M2-4)

The four arms and the metrics are already fixed by the design; do NOT invent
different ones.

Arms:
1. `freeform` — one free-form AI call (no schema constraint on the prompt).
2. `structured` — one structured call using the same output contract, caps, and
   validator as the product path.
3. `roles` — four canonical specialist roles (the current product design:
   requirements, architecture, qa, security).
4. `generic` — four generic calls merged under the same presentation format.

Conditions (must hold in the harness):
- same source evidence (same frozen snapshot) for every arm;
- normalized/blinded report rendering so arms are comparable;
- comparable token budgets across arms;
- each arm repeated at least 3 times per case;
- seeded defects in approved designs give objective ground truth:
  contradiction, dropped requirement, weakened auth check, missing failure path.

Metrics (recorded per run and aggregated per arm):
- defect recall (seeded defects found / seeded defects);
- false findings (findings not matching any seeded defect);
- citation support (fraction of findings whose basis_refs/anchor point at the
  unit(s) the seeded defect lives in);
- important misses (seeded defects no run of the arm found);
- repeated findings (duplicate findings within a run);
- run-to-run variation (variance of recall across the ≥3 repeats);
- cost and latency — recorded as **non-baseline telemetry**, kept separate.

Forbidden by the design: shipping the harness as product behavior, or changing the
v1 runtime to make an arm look better. The harness lives OUTSIDE the product path.

## Where it lives

New package: `internal/benchmark/` (harness, scoring, aggregation) plus a command
`cmd/benchmark/` (the runner). Nothing under `internal/benchmark` or `cmd/benchmark`
is imported by the product path (`internal/api`, `internal/worker`, the main
`cmd/speccouncil` binary). Verify with a grep gate (below).

## Data model (in `internal/benchmark/types.go`)

```go
type Arm string
const (
    ArmFreeform   Arm = "freeform"
    ArmStructured Arm = "structured"
    ArmRoles      Arm = "roles"
    ArmGeneric    Arm = "generic"
)

// SeededDefect is one known-planted flaw with the evidence unit it lives in.
type DefectKind string
const (
    DefectContradiction    DefectKind = "contradiction"
    DefectDroppedRequirement DefectKind = "dropped_requirement"
    DefectWeakenedAuth     DefectKind = "weakened_auth"
    DefectMissingFailurePath DefectKind = "missing_failure_path"
)

type SeededDefect struct {
    ID        string       // stable id, e.g. "D1"
    Kind      DefectKind
    UnitIDs   []string     // evidence unit(s) a correct finding must cite/anchor
    Rationale string       // what a reviewer should catch (for the report only)
}

// Case is one approved design plus its planted defects and frozen snapshot.
type Case struct {
    ID       string
    Snapshot evidence.Snapshot   // the frozen evidence the arms review
    Defects  []SeededDefect
}

// RunResult is one arm executed once over one case.
type RunResult struct {
    CaseID   string
    Arm      Arm
    Repeat   int
    Findings []domain.Finding
    // Telemetry — non-baseline, recorded separately.
    Cost     float64
    Latency  time.Duration
    Err      string   // non-empty if the run failed
}
```

## Scoring (`internal/benchmark/score.go`) — pure functions, the heart of the slice

A finding **matches** a seeded defect when the finding's `BasisRefs` or `AnchorRef`
intersect the defect's `UnitIDs`. (This is structural citation matching, matching
the project's rule that validation is structural, not semantic — do NOT try to
judge prose meaning.) Define exactly:

```go
// Matches reports whether a finding cites/anchors the defect's unit(s).
func Matches(f domain.Finding, d SeededDefect) bool

// ScoreRun computes per-run metrics for one RunResult against a case's defects.
type RunScore struct {
    DefectsFound   int      // distinct seeded defects matched by >=1 finding
    DefectsTotal   int
    FalseFindings  int      // findings matching no seeded defect
    SupportedCites int      // findings whose refs land on any seeded-defect unit
    TotalFindings  int
    DuplicateFindings int   // findings with identical (kind, sorted basis_refs, anchor)
}
func ScoreRun(r RunResult, c Case) RunScore

// ArmScore aggregates every repeat of one arm over one case (>=3 repeats).
type ArmScore struct {
    Arm            Arm
    CaseID         string
    Repeats        int
    MeanRecall     float64  // mean DefectsFound/DefectsTotal across repeats
    RecallVariance float64  // run-to-run variance of recall
    MeanFalse      float64
    ImportantMisses []string // seeded-defect IDs no repeat found
    MeanCost       float64
    MeanLatencyMS  float64
}
func AggregateArm(arm Arm, c Case, runs []RunResult) ArmScore
```

Every function is deterministic and pure (no clock, no map-order dependence — sort
before reducing). This is what the unit tests hammer.

## Arm execution (`internal/benchmark/arms.go`)

Each arm turns a `Case` + a `provider.Provider` into `[]domain.Finding` for one
repeat. Reuse existing engine pieces; do NOT reimplement validation or parsing:
- `structured`, `roles`, `generic`: build prompts, call the provider, parse+validate
  responses through the SAME `internal/review` validation the product uses, so a
  malformed arm response is rejected identically.
- `freeform`: the call is unconstrained, but its output is still parsed leniently
  into findings for scoring; document that freeform findings may have weaker refs.

The arm functions take a `provider.Provider` argument. In M2-4a every test passes a
`fake.FakeProvider` scripted per arm, so the whole harness is exercised at $0. In
M2-4b the same functions get the real `cline.Provider`. **No arm function may hard-
code which provider it uses.**

Provider-call budget per arm per repeat, asserted by the harness:
`freeform`=1, `structured`=1, `roles`=4, `generic`=4.

## Fixtures (`internal/benchmark/testdata/`)

Provide at least 2 cases as committed fixtures: a small frozen snapshot (reuse the
`standardTestSnapshot` style — a handful of evidence units) with 2–4 seeded defects
each, and per-arm scripted fake responses that produce a KNOWN mix of hits, misses,
false findings, and duplicates — so the scoring tests assert exact numbers. These
fixtures are the ground truth the scoring tests check against.

## Report rendering (`internal/benchmark/report.go`)

`RenderComparison(cases []Case, scores []ArmScore) string` produces a deterministic,
blinded text/markdown table: one row per (arm, case) with recall, variance, false
findings, important misses, and — clearly separated under a "Telemetry (non-baseline)"
heading — cost and latency. Deterministic ordering (sort by case, then a fixed arm
order). No timestamps in the baseline section.

## The runner command (`cmd/benchmark/main.go`)

A CLI that: loads the fixture cases, runs each arm N times (default 3, `-repeats`
flag), and prints the comparison. Provider selection mirrors M2-3:
- default / `-provider fake`: uses a built-in scripted fake — **zero cost, the
  default, what CI and this slice run**;
- `-provider cline`: reserved for M2-4b; in THIS slice it may exist but the packet
  does not run it. If wiring it is more than trivial, leave a `// M2-4b:` TODO and
  a clear error if selected, rather than doing paid calls now.

The runner must print a one-line **cost estimate and refuse to run live without an
explicit `-i-accept-cost` flag** — a guardrail M2-4b will rely on. In M2-4a this
path is not exercised; just build the guard.

## Tests — `internal/benchmark/*_test.go` (all fake, all $0)

1. `Matches`: hit via basis_ref, hit via anchor_ref, miss, empty-refs finding.
2. `ScoreRun`: on a fixture with a known finding mix, assert every field of
   `RunScore` exactly (found, false, duplicates, supported cites).
3. `AggregateArm`: ≥3 scripted repeats with differing results → assert MeanRecall,
   RecallVariance, and ImportantMisses (a defect missed by all repeats) exactly.
4. Arm execution: each arm, driven by a scripted fake, makes exactly its budgeted
   number of provider calls (1/1/4/4) and returns the expected findings; a malformed
   arm response is rejected by the shared validator.
5. `RenderComparison`: golden-string test — deterministic output, telemetry under
   its own heading, stable ordering, no clock.
6. Determinism: running the whole fake benchmark twice yields identical scores and
   identical rendered report (`reflect.DeepEqual` / string equality).
7. Isolation gate (can be a test or the grep gate below): the product path does not
   import `internal/benchmark`.

## Gates

```bash
export PATH="/c/Users/User/sdk/go/bin:$PATH"
cd /d/PROJECT/SpecCouncil
gofmt -l .                                   # empty
go vet ./...                                 # clean
go test ./internal/benchmark/... -v -count=1 # harness+scoring pass
go test ./... -count=1                       # every package green, still $0
go build ./...                               # both binaries build
go run ./cmd/benchmark                       # prints the fake comparison, exits 0, no network
# isolation: product path must NOT import the benchmark package
grep -rn "internal/benchmark" internal/api internal/worker internal/review internal/storage cmd/speccouncil && echo "!!! LEAK" || echo "isolated OK"
# WSL race: go test -race ./internal/benchmark/...
```

`go test ./...` and `go run ./cmd/benchmark` MUST NOT touch the network or read a
key. No live provider call happens in this slice.

## Allowed paths

- `internal/benchmark/**` (new package: types.go, arms.go, score.go, report.go + tests + testdata)
- `cmd/benchmark/**` (new runner)

## Forbidden

Every existing product path: `internal/api/**`, `internal/worker/**`,
`internal/review/**`, `internal/domain/**`, `internal/storage/**`,
`internal/ingest/**`, `internal/evidence/**`, `internal/provider/**` (reuse via
import only — do NOT modify the provider, fake, or cline packages),
`cmd/speccouncil/**`, migrations, `go.mod`/`go.sum` (stdlib + existing internal
packages only; no new dependency). No live provider calls. No push. If scoring
cannot be computed without changing a product package, STOP and report — that is a
real gap, not something to paper over by editing frozen code.

## Commit

One commit, message exactly:

```
Add deterministic four-arm benchmark harness and scoring (fake provider only)
```

## Completion report format

```
SLICE: M2-4a
START: <sha> / FINAL: <sha>
CHANGED: <git diff --cached --name-only>   (only internal/benchmark/** and cmd/benchmark/**)
ISOLATION: <grep result proving product path does not import internal/benchmark>
ARMS: <confirm the 4 arms and their per-repeat call budgets 1/1/4/4, enforced how>
SCORING: <the exact RunScore/ArmScore fields asserted, with the fixture's known numbers>
FIXTURES: <cases, seeded defects per case, and that scripted fake responses give a known hit/miss/false/dup mix>
DETERMINISM: <confirm two full runs give identical scores + identical rendered report>
COSTGUARD: <confirm live path refuses without -i-accept-cost and no paid call runs in this slice>
GATES:
  gofmt -l .                       -> <empty|output>
  go vet ./...                     -> <clean|output>
  go test ./internal/benchmark/... -> <PASS|FAIL>
  go test ./... -count=1           -> <per-package>
  go build ./...                   -> <ok|err>
  go run ./cmd/benchmark           -> <exit 0, sample output head, no network>
RACE: <WSL output | UNKNOWN/BLOCKED with reason>
PUSHED: no
NOTES: <deviations, or NONE>
```
