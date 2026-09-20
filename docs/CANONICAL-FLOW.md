# SpecCouncil v1 Canonical Flow

Status: approved current-review runtime baseline on 2026-09-19; M2 validation direction approved on 2026-09-20.

This document is the authority for the implemented SpecCouncil v1 current-review
runtime flow. The numeric timing values remain measurement-driven configuration;
the state transitions and control behavior below are frozen unless changed
explicitly.

## Authority boundary

The implemented canonical flow remains one frozen snapshot reviewed by the four
canonical roles. M2 validates the evidence-ingestion and real-provider behavior
needed to decide whether that product is useful in practice. M2 does not add an
approved-baseline lifecycle or change-review runtime.

The candidate post-design workflow is:

```text
current design review
-> human-approved design baseline
-> proposed complete revision
-> cited change-impact review
-> human Accept / Revise / Reject decision
-> optional later approval of a complete revised baseline
```

That workflow is an experiment subject, not an approved runtime contract. Until
the M2 validation gate passes, this document does not authorize baseline tables,
snapshot lineage, cross-snapshot citations, change-review sessions, finding
lifecycle inference, or baseline-advancement APIs.

## Fixed state model

Role states:

```text
pending -> in_flight -> complete | failed | interrupted
pending -------------> interrupted
```

Terminal role states never change. Retrying is not a role state.

Interruption causes:

```text
user_cancelled | deadline_cutoff | process_restart
```

Session states:

```text
queued -> reviewing -> complete | partial | failed
```

There is no `cancelled` session state. Every terminal session stores a
`terminal_reason`.

Canonical role order:

```text
1. Requirements
2. Architecture
3. QA
4. Security
```

## 1. Submit

```text
Submit design + Idempotency-Key
-> authenticate
-> authorize project
     inaccessible or foreign project -> 404
-> validate size, schema, required fields, and strict UTF-8
-> normalize request
-> compute request_body_hash
-> BEGIN IMMEDIATE
     lookup UNIQUE(project_id, idempotency_key)

     key exists:
       same hash      -> return original review, 200
       different hash -> idempotency_conflict, 409

     key absent:
       atomically create:
         immutable snapshot and snapshot units
         snapshot_hash
         session = queued
         Requirements = pending
         Architecture = pending
         QA = pending
         Security = pending
-> COMMIT
-> return 201 {review_id, status: queued}
```

The database unique constraint is authoritative. If a concurrent request wins an
insert race, the loser rereads the winning row and compares hashes:

```text
same hash      -> 200 existing review
different hash -> 409 idempotency_conflict
```

There is no admission cap in v1.

### Request hash v1

The hash input is exactly:

```text
normalization_version = 1
project_id
title
content
```

Rules:

- Strictly decode UTF-8; reject invalid input.
- Do not trim text.
- Do not apply Unicode normalization.
- Preserve submitted line endings for the idempotency hash.
- Serialize the version and fields with deterministic, unambiguous boundaries.
- Compute SHA-256 over those serialized bytes.

The snapshot splitter may normalize line endings separately. That does not alter
the request hash.

### Evidence-ingestion boundary

The current runtime accepts an already constructed frozen `evidence.Snapshot`.
It does not yet define how raw proposal `content` becomes evidence units. Until
M2 freezes that contract, snapshot construction must not invent unit IDs, kinds,
or segmentation rules.

M2 will evaluate a deterministic splitter v1 with these candidate boundaries:

```text
headings | paragraphs | list items | tables | code blocks
```

The candidate must preserve explicit author identifiers such as `REQ-12`, assign
deterministic IDs where none exist, record its splitter version, and produce the
same units for the same bytes and version. The exact normalization, ID, table,
and collision rules remain unfrozen until the M2 specification is approved.

## 2. Worker claim

```text
worker starts
-> acquire exclusive worker-process lock
     unavailable -> second worker exits
-> before claiming new work, run RESTART recovery
-> select oldest queued session deterministically:
     ORDER BY created_at, id
-> atomically claim one session:
     queued -> reviewing
     set started_at
     set dispatch_cutoff_at = now + DISPATCH_CUTOFF
     set hard_deadline_at   = now + SESSION_HARD_DEADLINE
-> affected rows != 1 -> re-poll
```

One worker process handles one review session at a time. The process lock must be
released automatically when the process exits or crashes.

## 3. Serialized dispatch loop

One owner loop controls dispatch. Timer ticks and role-completion events wake
that loop; they do not run competing dispatch routines.

Run the loop on every `TICK_INTERVAL` and after every role reaches a terminal
state:

```text
1. cancel_requested?
     yes -> CANCEL path

2. now >= dispatch_cutoff_at?
     yes -> CUTOFF path

3. local execution slot available?
     no -> wait for completion or next tick

4. choose the next pending role in canonical role order

5. authoritative guarded dispatch:
     BEGIN IMMEDIATE
     UPDATE selected role:
       pending -> in_flight
     WHERE:
       role is pending
       session is reviewing
       cancel_requested = false
       database current time < dispatch_cutoff_at
       database count(in_flight) < MAX_IN_FLIGHT
     COMMIT

     affected rows = 1 -> start role goroutine after commit
     affected rows = 0 -> rerun dispatch loop
```

`MAX_IN_FLIGHT = 2` for v1. The fast reads improve responsiveness; the guarded
transaction is authoritative. Therefore:

- If cancellation commits first, a pending role cannot start.
- If dispatch commits first, that role is legitimately in flight and may drain.
- The database guard and serialized owner loop prevent more than two roles from
  becoming in flight.

No provider call runs while a database transaction is open.

## 4. Role execution

Every role receives the same immutable frozen snapshot plus its role-specific
instructions.

```text
assemble prompt
-> count tokens with the configured model's tokenizer
-> prompt exceeds MAX_IN_TOKENS?
     yes -> failed / budget_exhausted / 0 provider calls
-> ensure time remains before hard_deadline_at
     no -> failed / timeout / 0 provider calls
-> provider call 1, purpose = initial
```

Every attempt uses a cancellable local context with deadline:

```text
min(now + CALL_TIMEOUT, hard_deadline_at)
```

### Call 1 classification

```text
A. Retryable transport failure
   Examples: 429, retryable 5xx, reset, network fault, call timeout.

   -> wait for jittered backoff in BACKOFF_MIN..BACKOFF_MAX only while the
      session hard deadline still permits another attempt to start
   -> call 2, purpose = transport_retry

      transport failure -> failed / transport or timeout
      transport success -> validate response
        valid   -> success
        invalid -> failed / exact validation category
                   no format repair because call budget is spent

B. Fatal provider failure
   Examples: non-retryable 4xx, authentication failure, provider rejection,
   confirmed context overflow.

   -> failed / provider_rejected or budget_exhausted
   -> no second call

C. Transport success

   -> strict validation
      valid   -> success
      invalid -> call 2, purpose = format_repair, if time remains

         transport failure -> failed / transport or timeout
                              no transport retry after repair
         invalid output    -> failed / exact validation category
         valid output      -> success
```

Absolute call rule:

```text
MAX_PROVIDER_CALLS_PER_ROLE = 2

allowed:
  initial
  initial -> transport_retry
  initial -> format_repair

forbidden:
  initial -> transport_retry -> format_repair
  initial -> format_repair -> transport_retry
  any third call
```

### Strict output validation

```text
strict JSON parse; reject unknown fields
-> schema and required-field validation
-> field-size and finding-count limits
-> basis_refs count and uniqueness
-> every basis_ref must exist in the frozen snapshot
-> trusted role result
```

An empty findings list is a valid successful result. Invalid output never becomes
a finding, and missing or invalid evidence references are never invented or
silently remapped.

Validation proves that a cited unit exists in the frozen snapshot. It does not
prove that the unit semantically supports the finding, that the finding is true,
or that the review found every important issue. The current mandatory
`basis_refs` shape also has no explicit representation for an omission claim.
M2 must measure citation support and define the experimental finding/citation
schema before a real provider result is treated as product evidence. Verbatim
excerpts may improve traceability, but substring matching alone is not semantic
verification.

## 5. Persist role result

Provider work is complete before opening the transaction.

Success:

```text
BEGIN IMMEDIATE
-> read role and require status = in_flight
-> insert all validated findings
-> store provider-call metadata
-> UPDATE role: in_flight -> complete
     WHERE status = in_flight
-> affected rows must equal 1
     otherwise ROLLBACK and discard the late/stale result
-> COMMIT
```

Findings and `complete` are atomic.

Execution failure:

```text
BEGIN IMMEDIATE
-> UPDATE role:
     store typed error_category and provider-call metadata
     in_flight -> failed
   WHERE status = in_flight
-> affected rows must equal 1
     otherwise ROLLBACK and discard the late/stale result
-> COMMIT
```

Failed and interrupted roles contribute no findings.

### Persistence failure policy

```text
write transaction fails
-> retry up to DB_RETRIES with bounded backoff
-> retries exhausted:
     stop all new dispatch
     exit worker with a nonzero status
-> supervisor restarts worker
-> RESTART recovery terminalizes stale work
```

A supervised restart policy is required. The worker must not continue and leave
a silently stuck `in_flight` role.

## 6. Gate

After every terminal role persist or interruption transaction:

```text
all 4 role rows terminal AND database count(in_flight) = 0?
  no  -> return to dispatch loop
  yes -> composer
```

Terminal role states are `complete`, `failed`, and `interrupted`.

## 7. Deterministic composer

The composer does not use an LLM. It runs once in a short transaction:

```text
BEGIN IMMEDIATE
-> reread all four committed role rows
-> verify all four are terminal
-> verify count(in_flight) = 0
-> derive counts, session status, and terminal_reason
-> UPDATE session
     WHERE status = reviewing
-> affected rows must equal 1
-> COMMIT
```

Session status:

```text
4 complete                                    -> complete
otherwise, any user_cancelled interruption   -> partial
otherwise, at least 1 complete               -> partial
otherwise                                    -> failed
```

A user-cancelled session is `partial` even when zero roles completed. The
first-class terminal reason makes the absence of results explicit without
mislabeling an intentional cancellation as a system failure.

Terminal reason is derived from committed role outcomes, never from the live
`cancel_requested` flag. Precedence:

```text
1. 4 roles complete                         -> all_roles_complete
2. any interrupted / user_cancelled        -> user_cancelled
3. any interrupted / process_restart       -> process_restart
4. any interrupted / deadline_cutoff       -> deadline_cutoff
5. otherwise                               -> role_failures
```

Store:

```text
completed_role_count
incomplete_role_count
terminal_reason
```

Do not label interrupted roles as failed in stored counters.

## 8. Read-only status and report

```text
GET status or report
-> authenticate
-> authorize project
     inaccessible or foreign project -> 404
-> report requested while nonterminal -> 409 not_finished
-> read committed rows through a read-only connection
-> render deterministic output
```

The report shows together at the top:

```text
session status
terminal_reason
cancel_requested
completed_role_count
incomplete_role_count
```

It also shows each role's status, interruption cause or failure category,
provider-call metadata, and validated findings in deterministic order.

A read path never:

```text
runs the composer
repairs state
restarts roles
calls a provider
writes database rows
```

## Control path: cancellation

```text
POST cancel
-> authenticate and authorize
-> session already terminal?
     yes -> 200 {effective:false}; no mutation
-> cancel_requested already true?
     yes -> 200 {effective:false}
-> queued or reviewing:
     atomically set cancel_requested false -> true
     return 202 {effective:true}
```

At the next dispatch cycle:

```text
BEGIN IMMEDIATE
-> pending roles -> interrupted / user_cancelled
-> COMMIT
-> in_flight roles continue normally
-> complete and failed roles remain unchanged
-> findings are never deleted
-> gate
```

If cancellation commits after all four roles were dispatched, no role gains a
`user_cancelled` cause. The in-flight roles drain, and the composer derives the
terminal reason from their actual outcomes. The report still shows
`cancel_requested = true`.

### Cancellation while queued

```text
queued session receives cancel request
-> remains queued with cancel_requested = true
-> worker claims it normally
-> first dispatch cycle interrupts all four pending roles / user_cancelled
-> 0 provider calls
-> composer -> partial / user_cancelled
```

Only the worker finalizes sessions.

## Control path: dispatch cutoff

```text
now >= dispatch_cutoff_at
-> BEGIN IMMEDIATE
-> remaining pending roles -> interrupted / deadline_cutoff
-> COMMIT
-> no new roles start
-> existing in_flight roles continue
-> an optional call 2 may start only if hard_deadline_at still permits it
-> gate after each role drains
```

The dispatch cutoff controls starts. It does not kill in-flight work.

## Control path: session hard deadline

Every provider attempt is constrained by the local context deadline. The owner
loop also schedules a wake-up at the exact hard deadline; it does not rely only
on the periodic tick.

At `hard_deadline_at`:

```text
BEGIN IMMEDIATE
-> remaining pending roles -> interrupted / deadline_cutoff
-> COMMIT
-> cancel and close local in-flight provider requests
-> no new dispatch, retry, or repair may start
-> each affected in-flight role goroutine persists:
     failed / timeout
-> every terminal write uses compare-and-set from in_flight
     a late provider result cannot overwrite a terminal role
-> gate
```

The guarantee applies to SpecCouncil's local request. A remote provider may
continue computation after the client disconnects; SpecCouncil cannot prove or
control that external behavior.

Configuration must reject startup unless:

```text
SESSION_HARD_DEADLINE >= DISPATCH_CUTOFF + CALL_TIMEOUT
```

This guarantees a full initial call for a role dispatched just before cutoff. It
does not guarantee enough time for an optional second call.

The exact positive timing values come from measurements and configuration. They
are not invented by this flow.

## Control path: process restart

After acquiring the exclusive worker lock and before claiming queued work:

```text
for each stale reviewing session:
  complete roles    -> unchanged; findings kept
  failed roles      -> unchanged
  interrupted roles -> unchanged
  in_flight roles   -> interrupted / process_restart
  pending roles:
    cancel_requested = true  -> interrupted / user_cancelled
    otherwise                -> interrupted / process_restart

-> no provider work is replayed
-> run gate and composer for the recovered session
```

Queued sessions remain queued, including queued sessions with cancellation
requested. They are handled normally when claimed.

## Control path: second worker

```text
exclusive process lock unavailable
-> second worker exits
```

## M2 validation gate

M2 validates the current product before any approved-baseline or change-review
runtime is frozen. It contains five workstreams:

1. Specify deterministic raw-content-to-evidence-unit ingestion.
2. Freeze the experimental finding and citation schema, including an explicit
   omission representation, without claiming that structural validation proves
   semantic support.
3. Exercise one safely configured real provider while retaining the deterministic
   fake provider for automated tests.
4. Run a controlled four-arm benchmark:
   - free-form single call;
   - one structured call using the same output contract;
   - four canonical specialist roles;
   - four generic calls composed under the same presentation format.
5. Run a concierge change-review experiment using real historical changes and,
   when suitable participants exist, a prospective 4-6 week live trial.

Each benchmark arm must receive equivalent source evidence, use comparable model
quality and declared token/cost budgets, render into a normalized blinded format,
and run at least three times per case. The pilot measures seeded-defect recall,
precision, citation support, important misses, duplicate or conflicting findings,
run-to-run variance, cost, latency, and evidence-preparation time. Pilot results
set the distribution used to pre-register thresholds for any larger comparison;
the threshold must not be moved after seeing that larger result.

M2 passes only if direct real-provider evidence shows useful review quality and
users can prepare evidence with acceptable effort. Four-role superiority is not
assumed. If a structured single call matches the four-role path within measured
variance, the role architecture and positioning must be reconsidered before M3.
Historical examples cannot prove voluntary return behavior; only prospective use
can test whether users bring back another meaningful change.

M2 does not authorize these M3 candidates:

```text
approved baseline records
baseline version lineage or active-baseline pointers
cross-snapshot or typed baseline/proposed/diff citations
change-review sessions or request hash v2
human change-decision records
finding-to-finding continuity
ADR export, agent hooks, CI gates, or drift detection
```

Any later M3 specification must preserve these boundaries:

- Review completion is not design approval.
- Accepting a change is not approving a new baseline.
- Provider findings remain nondeterministic.
- Findings remain immutable occurrences; fixed, reopened, regressed, or renamed
  relationships are not inferred automatically.
- SpecCouncil does not claim design correctness, implementation conformance,
  security, release readiness, complete risk discovery, or reproducible findings.
- A human owns every Accept, Revise, Reject, and baseline-approval decision.

## Compressed flow

```text
SUBMIT
-> AUTHENTICATE + AUTHORIZE + VALIDATE
-> NORMALIZE + HASH
-> IDEMPOTENCY
-> ATOMIC SNAPSHOT + QUEUED SESSION + 4 PENDING ROLES
-> WORKER CLAIM
-> REVIEWING
-> SERIALIZED DISPATCH LOOP, MAX 2 IN_FLIGHT
-> SAME FROZEN SNAPSHOT FOR EVERY ROLE
-> TOKEN BUDGET
-> INITIAL PROVIDER CALL
-> OPTIONAL RETRY XOR REPAIR
-> MAX 2 CALLS
-> STRICT VALIDATION
-> ATOMIC ROLE PERSIST
-> COMPLETE / FAILED / INTERRUPTED
-> ALL 4 TERMINAL AND 0 IN_FLIGHT
-> DETERMINISTIC TRANSACTIONAL COMPOSER
-> COMPLETE / PARTIAL / FAILED + TERMINAL_REASON
-> READ-ONLY STATUS / REPORT
```

Side paths:

```text
CANCEL -----------> terminal role -> GATE
DISPATCH CUTOFF --> terminal role -> GATE
HARD DEADLINE ----> terminal role -> GATE
PROVIDER FAILURE -> terminal role -> GATE
PROCESS RESTART --> terminal role -> GATE
PERSIST FAILURE --> fail-stop -> supervised restart -> recovery -> GATE
```
