# SpecCouncil

SpecCouncil reviews a documented software design **before implementation**. Four
AI reviewers examine one frozen evidence snapshot from four perspectives, and a
deterministic composer turns the committed role results into a report.

The product does not execute the proposed application, does not prove a design
correct, and does not implement recommendations.

## Status

**Milestone 1 — review core.** This is a clean-room Go rebuild. It contains no
HTTP server, no database, no concurrency and no real provider.

What works today:

- Frozen evidence snapshot with a deterministic hash
- Four canonical roles sharing one snapshot
- One role executed end to end with a hard two-call provider budget
- Strict parsing and validation of provider output
- Deterministic composer and report

What does not exist yet: the API, SQLite persistence, the bounded two-at-a-time
scheduler, cancellation and deadline handling, startup recovery, and the real
provider adapter.

## Requirements

Go 1.27 or newer.

## Run

```bash
go run ./cmd/speccouncil
```

That runs the built-in demo: a small design snapshot reviewed by all four roles
through the fake provider. The QA role is scripted to fail its first answer so
the demonstration also exercises the repair path.

Point it at real files:

```bash
go run ./cmd/speccouncil -snapshot snapshot.json -script script.json -session rev-1
```

`snapshot.json`:

```json
{
  "id": "snap-1",
  "units": [
    { "id": "REQ-1", "kind": "requirement", "text": "A student may cancel their own booking." }
  ]
}
```

`script.json` maps each role to its ordered canned provider answers:

```json
{
  "requirements": [{ "body": "{\"findings\":[]}" }],
  "qa": [
    { "transport_error": "transport", "message": "429 too many requests" },
    { "body": "{\"findings\":[]}" }
  ]
}
```

## Test

```bash
go test ./...
```

## Package layout

```
internal/domain     roles, role and session states, error categories, findings
internal/evidence   frozen snapshot and its deterministic hash
internal/provider   provider boundary and the scripted fake provider
internal/review     prompt, validation, role runner, engine, composer
cmd/speccouncil     command-line entry point
```

## Design rules that are enforced in code

- The four roles are exactly Requirements, Architecture, QA, Security. Placeholder
  role names are rejected.
- Role states are exactly `pending | in_flight | complete | failed | interrupted`.
  Retrying is not a role state; both provider attempts happen inside one `in_flight`.
- There is no persistent session `cancelled` state. Cancellation is represented by
  a request flag plus a terminal reason.
- Aggregation refuses to run unless all four roles are terminal.
- All four roles receive the same frozen snapshot. There is no per-role section
  selection in v1.
- A role makes at most two provider calls. Call two is a transport retry **or** a
  format repair, never both.
- A failed or interrupted role contributes zero findings.
- An empty findings list is a valid success.
- Report ordering is a total order, so completion order cannot change the output.

See `docs/ENGINE-CONTRACT.md` for the full contract and the list of decisions the
specification has not frozen yet.
