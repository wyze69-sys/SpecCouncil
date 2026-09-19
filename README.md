# SpecCouncil

**Evidence-grounded software design review before implementation begins.**

SpecCouncil takes one documented design and examines it from four specialist
perspectives. Every reviewer works from the same frozen source, every accepted
finding must cite that source, and the final report is assembled by deterministic
code rather than another model call.

The result is a review that shows what is wrong, why it matters, where the concern
comes from, and what to change before committing engineering time.

## What SpecCouncil reviews

A submission can describe:

- Product goals and requirements
- Components and system boundaries
- User and data flows
- Security and privacy constraints
- Failure behavior and operational limits
- Acceptance criteria and test plans

SpecCouncil freezes the submitted material into one immutable evidence snapshot.
That snapshot is the only evidence the reviewers may cite.

## The review council

### Requirements

Finds ambiguity, missing behavior, conflicting requirements, undefined actors,
and acceptance criteria that cannot be verified.

### Architecture

Checks boundaries, dependencies, data ownership, failure handling, scalability,
and whether the proposed components can satisfy the stated requirements.

### QA

Looks for untestable claims, missing edge cases, weak acceptance criteria,
state-transition gaps, and failure paths that have no verification plan.

### Security

Examines trust boundaries, authorization, sensitive data, abuse paths, unsafe
defaults, and missing security controls.

These are independent review perspectives, not four attempts to produce the same
answer.

## How it works

```text
Submit design
→ authenticate, authorize, and validate
→ freeze one immutable evidence snapshot
→ run Requirements, Architecture, QA, and Security reviews
→ allow at most two reviewers in flight at once
→ validate every reviewer response
→ reject findings with invalid or invented evidence references
→ persist each reviewer result atomically
→ compose one deterministic report
→ present the report without changing stored results
```

Each reviewer may make at most two provider calls:

```text
initial call
+
one transport retry OR one format repair
```

A reviewer never receives both a retry and a repair, and there is no third call.

## What the report contains

- Overall outcome: `complete`, `partial`, or `failed`
- A terminal reason explaining why processing ended
- Status for each specialist reviewer
- Typed failure or interruption details when a reviewer did not complete
- Validated findings in deterministic order
- Severity, issue, recommendation, and evidence references for every finding
- Completed and incomplete reviewer counts

Zero findings is a valid successful review. A failed or interrupted reviewer does
not generate placeholder findings.

## Design guarantees

- **One source of truth:** every reviewer receives the same frozen snapshot.
- **Evidence required:** accepted findings must cite evidence IDs that exist in
  that snapshot.
- **Bounded model use:** every role has a hard two-call limit.
- **No model-written verdict:** session status and report ordering are computed by
  deterministic code.
- **Partial results survive:** one failed reviewer does not erase successful work
  from the others.
- **No silent replay:** ambiguous provider work is not repeated after a process
  restart.
- **Read-only reporting:** reading a report cannot run reviewers, repair state, or
  change persisted results.

## What SpecCouncil does not do

SpecCouncil does not execute the proposed application, inspect a running system,
or prove that a design is correct or secure. It does not replace engineering
judgment. It gives the owner a cited, structured review of the design that was
actually submitted.

## Run the review-core demo

Requires Go 1.27 or newer.

```bash
go run ./cmd/speccouncil
```

The command runs a local example through the complete review core using a
scripted provider. It exercises the same prompt, call-budget, validation, and
report-composition boundaries without sending data to an external model.

Run with your own frozen snapshot and scripted responses:

```bash
go run ./cmd/speccouncil \
  -snapshot snapshot.json \
  -script script.json \
  -session review-1
```

Example snapshot:

```json
{
  "id": "snapshot-1",
  "units": [
    {
      "id": "REQ-1",
      "kind": "requirement",
      "text": "Only a project owner may approve a production release."
    },
    {
      "id": "FLOW-1",
      "kind": "flow",
      "text": "The release service records the approver before deployment starts."
    }
  ]
}
```

## Test

```bash
go test ./...
```

## Design reference

The complete runtime and control-path contract is documented in
[`docs/CANONICAL-FLOW.md`](docs/CANONICAL-FLOW.md).
