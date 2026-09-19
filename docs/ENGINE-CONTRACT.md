# Engine Contract — milestone 1

This file records what the engine implements, and marks every place where the
specification has not frozen a decision. Unfrozen choices are isolated in one
place in the code so that freezing the specification is a one-line change.

## Canonical runtime flow

The approved v1 runtime authority is [`CANONICAL-FLOW.md`](CANONICAL-FLOW.md).
It resolves the previously open cancellation, persistence-failure, request-hash,
hard-deadline, and terminal-reason rules. This milestone contract records only
which parts of that flow the current code implements.

## Confirmed contract implemented

### Canonical roles

Requirements, Architecture, QA, Security. Dispatch order is the order above.
`domain.Roles` is the single source of that order, and `RoleOrder` rejects any
other identifier.

### Role states

`pending | in_flight | complete | failed | interrupted`

Terminal means `complete | failed | interrupted`. Legal transitions:

```
pending   -> in_flight | interrupted
in_flight -> complete  | failed | interrupted
```

Retrying is not a role state. Both provider attempts of a role happen inside one
`in_flight` execution.

Interrupts carry a cause: `user_cancelled`, `deadline_cutoff`, `process_restart`.

### Session states

`queued | reviewing | complete | partial | failed`

There is no persistent `cancelled` session state. Cancellation is a request flag
plus a terminal reason.

### Snapshot

`evidence.Freeze` copies the caller's units, rejects empty ids, empty text,
unknown kinds and duplicate ids, and hashes the units in sorted order so the
hash does not depend on input order. The hash is what makes a snapshot
addressable.

All four roles receive the same snapshot. No per-role selection.

### Provider output pipeline

```
raw body
  -> strict JSON parse            (unknown fields rejected)   -> invalid_json
  -> structural schema validation                            -> schema_invalid
  -> semantic validation (basis_refs must exist)             -> invalid_basis_ref
  -> trusted ReviewerResult
```

Enforced bounds: at most 15 findings; 1–5 unique basis refs per finding; issue
and recommendation non-empty and at most 1000 characters; unique finding ids.
An empty findings list is valid.

### Two-call provider budget

| First call result | Second call | Purpose | Outcome if the second call also fails |
|---|---|---|---|
| Transport error (network, 429, retryable 5xx, timeout) | yes | `transport_retry` | role failed, transport category |
| Transport OK but invalid JSON / schema / basis refs | yes | `format_repair` | role failed, validation category |
| Fatal 4xx, auth failure, provider rejection | no | — | role failed, `provider_rejected` |
| Prompt exceeds the model input budget | no call at all | — | role failed, `budget_exhausted` |
| Retry already used, then malformed output | no third call | — | role failed, validation category |

A role can never exceed two provider calls.

### Composer

The current milestone composer correctly refuses to run until all four role
outcomes are terminal and produces deterministic ordering. Its in-memory verdict
logic predates the approved canonical flow and still needs these implementation
changes:

- derive `terminal_reason` from committed interruption causes, not the live
  cancellation flag;
- add `process_restart`, `deadline_cutoff`, and `role_failures` reasons;
- replace the legacy `failed_role_count` with `incomplete_role_count`;
- execute the gate and terminal session update transactionally once persistence
  exists.

Canonical composer behavior is defined only by `CANONICAL-FLOW.md`.

Current deterministic finding order is:

```text
severity rank, role rank, primary basis_ref, category, finding id
```

A failed or interrupted role contributes zero findings.

## Not built yet

API endpoints, authentication and authorization, SQLite persistence and its
guards, the bounded two-at-a-time scheduler, `dispatch_cutoff_at` / `call_timeout`
/ `hard_deadline_at` enforcement, cancellation at the dispatch boundary under
concurrency, startup recovery sweep, idempotency and request hashing, the real
provider adapter, and the supervised worker restart policy.

## Known implementation gaps against the canonical flow

| Gap | Current code | Required change |
|---|---|---|
| Format-repair call fails in transport | Reports the transport failure | Keep this behavior; persist `transport` or `timeout` |
| Outcome counter | `failed_role_count` counts every non-complete role | Rename to `incomplete_role_count` |
| Terminal reason | Missing for most partial/failed outcomes | Derive it from committed role causes using canonical precedence |
| Prompt token count | Rough four-characters-per-token estimate | Use the configured model's tokenizer |
| Finding field set | Requires `severity` and `category` | Freeze the complete output schema before the real adapter |

## Evidence status

Everything in this milestone is proven by the Go test suite through the fake
provider. No real provider has been called, so nothing here is evidence about
live model behaviour.