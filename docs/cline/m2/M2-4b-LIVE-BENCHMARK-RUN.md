# M2-4b — Live four-arm benchmark run (real Cline provider, hard cost cap)

Slice: M2-4b (validation run + minimal wiring)
Start commit: current `master` HEAD at release time (`895b719` when written).
Depends on: M2-4a (`87960d2`) — harness, arms, scoring, cost guard all built and
verified fake-only; M2-3 (`0771c10`) — `internal/provider/cline` adapter.
Module: `github.com/wyze69-sys/SpecCouncil`, Go 1.27.1
Windows toolchain: `export PATH="/c/Users/User/sdk/go/bin:$PATH"` before every Go command.

## Mission

Turn on the one thing M2-4a deliberately left off: run the four arms against the
REAL model and record the results. This is the first slice that spends real money.
It has two parts:

1. A small, safe wiring change: `cmd/benchmark -provider cline` actually constructs
   the real `cline.Provider` and runs, instead of erroring with the M2-4a
   placeholder. Guarded by a HARD cost cap (below) on top of the existing
   `-i-accept-cost` flag.
2. A recorded, reproducible live run: execute the benchmark, persist raw results
   and the rendered comparison to disk under `docs/cline/m2/results/`, and write a
   short findings note. The scoring code is FROZEN (built in M2-4a) — this slice
   does not change how anything is measured, only runs it for real.

## Hard money rules (non-negotiable)

- **Cost cap: US$3.00.** Before any live call, compute the estimate
  (`totalCalls * perCallEstimate`). If the estimate exceeds the cap, refuse and
  exit non-zero without making a single call. The cap is a constant AND overridable
  DOWN only via `-max-cost` (a value above $3.00 is clamped to $3.00). It can never
  be raised above $3.00 in this slice.
- The existing `-i-accept-cost` flag stays required. Both the flag AND the
  under-cap check must pass. No key, no flag, or over-cap → zero calls.
- Default `-repeats 3`, 2 fixture cases, 4 arms = 60 calls. At the real
  deepseek-flash rate this is well under $3 (the smoke test showed ~$0.0000369 for
  a few tokens; even at a generous $0.01/call that is $0.60). Keep the coarse
  `0.03/call` estimate from M2-4a as the conservative pre-run number so the guard
  errs high, not low.
- The run reads the key exactly like `cmd/speccouncil`: from
  `SPECCOUNCIL_CLINE_KEY_FILE` (default `.secrets/cline_api_key`), TrimSpace,
  refuse on empty/placeholder. Never log the key. The key file stays git-ignored.
- `go test ./...` and `go run ./cmd/benchmark` (no `-provider cline`) MUST still be
  $0 and network-free. Only `-provider cline -i-accept-cost` makes paid calls.

## Real-model reality the run MUST tolerate (proven in M2-3)

The model is intermittently flaky: an identical request can return HTTP 500
`empty response content` then succeed on retry. The `cline` adapter already maps
that to `domain.ErrTransport`. But the benchmark arms call the provider directly
(not through the worker's retry engine), so:

- `ExecuteArm` must treat a transport/timeout error from the provider as a
  **retryable** failure and retry that single call up to 2 times (3 attempts total)
  with a short backoff, before recording the run as failed. Use the existing
  `provider.CategoryOf` + `domain.ErrorCategory.IsRetryableTransport()` — do NOT
  invent new classification.
- A run that still fails after retries is recorded with `RunResult.Err` set and
  contributes zero findings (the scoring already handles empty findings). One flaky
  arm must not abort the whole benchmark.
- `provider_rejected` (e.g. wrong model, auth) is NOT retried — fail fast, surface
  the error, stop the run so we don't burn calls against a misconfiguration.

This retry lives in the benchmark harness only. Do NOT touch the product worker or
the cline adapter.

## Real cost capture (replace the M2-4a stub)

In M2-4a `countingProvider.cost` is a stub (`// Compute cost if known`). The Cline
success body carries a real cost in `provider_metadata.gateway.cost`. Two acceptable
options — pick the simpler that keeps the adapter frozen:

- Preferred: the benchmark's `countingProvider` estimates cost from
  `Response.TokensIn/TokensOut` (already populated by the adapter from `usage`) times
  a per-token constant, recorded as **non-baseline telemetry only**. Exact billing is
  not required — this is telemetry, explicitly not a baseline metric.
- Do NOT change `provider.Response` or the cline adapter to surface the cost field;
  that would touch frozen code. Token-based estimate is sufficient.

Cost/latency remain clearly separated as non-baseline in the report (M2-4a already
does this).

## Wiring change (`cmd/benchmark/main.go` only)

Replace the M2-4a placeholder block (the `// M2-4b: wire live Cline provider here`
error) with:

1. Read key (as above). 2. Build `cline.New(cline.Config{BaseURL:
"https://api.cline.bot/api/v1", APIKey: key, Model: "deepseek/deepseek-v4.1-flash",
Timeout: 60s, MaxTokens: 1024})`. 3. Enforce the cost cap. 4. Run the benchmark with
the real provider. 5. On completion, write outputs to disk (below). The `fake`
default path stays byte-for-byte unchanged.

Add flags: `-out <dir>` (default `docs/cline/m2/results`), `-max-cost <float>`
(clamped to <=3.00).

## Outputs the run must persist (committed evidence)

Under `docs/cline/m2/results/` (create dir):

- `run-<UTCstamp>.json` — every `RunResult` (case, arm, repeat, findings, err,
  token telemetry). Deterministic field order. This is the raw evidence.
- `comparison-<UTCstamp>.md` — the `RenderComparison` output (baseline metrics +
  non-baseline telemetry table).
- `M2-4b-FINDINGS.md` — a short written readout: the recall/false-finding numbers
  per arm, whether the four-role arm beat the single structured call BY MORE THAN
  run-to-run variance (the pre-registered question from CANONICAL-FLOW §4), total
  real cost and latency, and any flaky-retry events observed. State plainly if the
  sample (2 cases) is too small to conclude — it is a pilot, not the final verdict.
  Do NOT claim four-role superiority as proven; report what the numbers show and
  label it pilot-scale.

The timestamp makes runs non-clobbering; commit the specific run this slice produces.

## Who runs the paid call

The worker executes this packet but **must STOP before the paid run and hand back**
if it cannot confirm the key file is present and non-placeholder — it should build
the wiring, run all fake-mode gates, then report READY-FOR-LIVE rather than guessing.
Hermes (not the worker) performs the actual `-provider cline -i-accept-cost` run
after confirming the key, so the money is spent under direct supervision. The worker
delivers: wiring + retry + cost capture + all fake gates green + a documented exact
command to run live.

## Gates (all $0, run by the worker)

```bash
export PATH="/c/Users/User/sdk/go/bin:$PATH"
cd /d/PROJECT/SpecCouncil
gofmt -l .                                   # empty
go vet ./...                                 # clean
go test ./internal/benchmark/... -count=1    # harness+retry tests pass (fake)
go test ./cmd/benchmark/... -count=1          # cost-cap + wiring tests pass (fake)
go test ./... -count=1                        # every package green, $0
go build ./...
go run ./cmd/benchmark                        # fake, $0, exits 0
go run ./cmd/benchmark -provider cline         # NO -i-accept-cost -> refuses, $0
# over-cap refusal (fake-forced high estimate or -max-cost 0): refuses, $0
```

New retry/cost-cap logic MUST have fake-provider unit tests:
- an arm whose scripted fake returns a transport error then a good body → retried,
  succeeds, recorded with findings;
- an arm that returns transport errors every attempt → recorded failed, zero
  findings, benchmark continues;
- a `provider_rejected` scripted error → NOT retried, run stops;
- estimate over cap → zero calls, non-zero exit;
- `-max-cost 5` clamps to 3.00.

## Allowed paths

- `cmd/benchmark/main.go` and `cmd/benchmark/main_test.go`
- `internal/benchmark/arms.go` and `internal/benchmark/arms_test.go` (retry + cost
  capture ONLY; do NOT change scoring in score.go or the arm call budgets)
- `docs/cline/m2/results/**` (new, run outputs — only after the live run)
- `docs/cline/m2/M2-4b-FINDINGS.md`

## Forbidden

`internal/benchmark/score.go`, `report.go`, `types.go`, `fixtures.go` (scoring and
data model are FROZEN from M2-4a — measurement must not change for a live run),
`internal/provider/**` (adapter frozen), all other product code, migrations,
`go.mod`/`go.sum` (no new deps). Do NOT raise the cost cap above $3.00. Do NOT commit
the key. Do NOT change how any metric is computed. If a real result looks bad for the
four-role arm, report it honestly — changing the harness to flatter an arm is the one
explicitly forbidden act in the design (BUILD-MAP M2-4).

## Commit

Two commits allowed in this slice:
1. Wiring + retry + cost capture + tests (no live run yet), message exactly:
   ```
   Wire live Cline benchmark run behind a hard cost cap with transport retry
   ```
2. (Made by Hermes after the supervised live run) the committed results +
   findings note, message exactly:
   ```
   Record M2-4b live four-arm benchmark pilot results
   ```
No push.

## Completion report format (worker, part 1)

```
SLICE: M2-4b (wiring only; live run deferred to Hermes)
START: <sha> / FINAL: <sha>
CHANGED: <git diff --cached --name-only>
WIRING: <how -provider cline builds cline.New; key read path; fake default unchanged>
COSTCAP: <the $3.00 constant, -max-cost clamp behavior, and that over-cap makes zero calls>
RETRY: <transport/timeout retried up to 3 attempts; provider_rejected not retried; failed run = zero findings, benchmark continues — all via provider.CategoryOf/IsRetryableTransport>
COSTCAPTURE: <token-based telemetry estimate; confirm provider.Response and cline adapter NOT modified>
FROZEN: <confirm score.go/report.go/types.go/fixtures.go unchanged; call budgets 1/1/4/4 unchanged>
TESTS: <the 5 new fake-provider retry/cap tests and what each asserts>
GATES:
  gofmt -l .                       -> <empty|output>
  go vet ./...                     -> <clean|output>
  go test ./internal/benchmark/... -> <PASS|FAIL>
  go test ./cmd/benchmark/...      -> <PASS|FAIL>
  go test ./... -count=1           -> <per-package>
  go build ./...                   -> <ok|err>
  go run ./cmd/benchmark           -> <fake $0 exit 0>
  go run ./cmd/benchmark -provider cline (no accept) -> <refuses, $0>
  over-cap                         -> <refuses, $0>
READY-FOR-LIVE: <the EXACT command Hermes should run, and confirm key presence was NOT assumed>
RACE: <WSL output | UNKNOWN/BLOCKED reason>
PUSHED: no
NOTES: <deviations, or NONE>
```
