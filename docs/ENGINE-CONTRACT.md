# Engine Contract — milestone 1

This file records what the engine implements, and marks every place where the
specification has not frozen a decision. Unfrozen choices are isolated in one
place in the code so that freezing the specification is a one-line change.

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

Runs only when all four roles are terminal. Rules are first-match-wins:

1. all four complete → `complete` / `all_roles_complete`
2. else cancel requested and all roles terminal → `partial` / `user_cancelled`
3. else at least one complete → `partial`
4. else → `failed`

Counts:

```
completed_role_count = roles with status complete
failed_role_count    = 4 - completed_role_count
```

Report ordering is a total order, so execution order cannot change the output:

```
severity rank, role rank, primary basis_ref, category, finding id
```

A failed or interrupted role contributes zero findings.

## Not built yet

API endpoints, authentication and authorization, SQLite persistence and its
guards, the bounded two-at-a-time scheduler, `dispatch_deadline` / `call_timeout`
/ `session_hard_deadline` enforcement, cancellation at the dispatch boundary
under concurrency, startup recovery sweep, idempotency and request hashing, and
the real provider adapter.

## Unfrozen decisions, and where they live

| Decision | Where | Current behaviour |
|---|---|---|
| Final category when a format repair fails at the transport layer | `review.Policy.RepairTransportFailureCategory` | Reports the transport failure |
| `failed_role_count` also counts interrupted roles | `review.Compose` | Kept as specified, flagged in a comment |
| Terminal reason for composer rules 3 and 4 | `review.Compose` | Left empty rather than invented |
| Prompt token budget rule | `review.EstimatePromptTokens` | Rough four-characters-per-token estimate; must be replaced by real tokenizer accounting |
| Finding field set | `domain.Finding` | `severity` and `category` are required fields; the frozen finding schema must still confirm them |

## Evidence status

Everything in this milestone is proven by the Go test suite through the fake
provider. No real provider has been called, so nothing here is evidence about
live model behaviour.