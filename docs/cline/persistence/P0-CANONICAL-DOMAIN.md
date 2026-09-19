# P0 — Canonical Domain Alignment

## Mission

Align the in-memory domain, composer, report JSON, and tests with
`docs/CANONICAL-FLOW.md` before SQLite. Execute only P0, commit only after every
gate passes, report, then STOP.

Repository: `D:\PROJECT\SpecCouncil` (Go 1.27.1).

## Start and read

Run `git status --short`, `git branch --show-current`, and
`git log -1 --oneline`. The tree must be clean. If not, preserve it and report
`BLOCKED`; never reset, clean, or stash.

Read:

1. `docs/CANONICAL-FLOW.md` — Fixed state model and Deterministic composer.
2. `docs/cline/persistence/00-BUILD-MAP.md` — protocol and global rules.
3. `internal/domain/status.go`, `status_test.go`, and `errors.go`.
4. `internal/review/composer.go`, its tests, engine, and engine tests.
5. Every Go reference to `ComposerInput`, `FailedRoleCount`,
   `failed_role_count`, `TerminalReason`, and `InterruptCause`.

## Implement

1. Add terminal reason `role_failures` and these exact validators:

```go
func IsValidTerminalReason(TerminalReason) bool
func IsValidInterruptCause(InterruptCause) bool
```

Test every canonical value, plus empty and unknown values.

2. Remove `CancelRequested` from `ComposerInput` only. The composer uses committed
role outcomes, never the live flag.
3. Add `InterruptCause` to `RoleRow`. Enforce:

```text
interrupted → required canonical cause
complete/failed → empty cause
```

4. Continue requiring exactly one terminal row for every canonical role.
5. Implement this exact first-match logic:

```text
4 complete
  → complete / all_roles_complete
else any interrupted/user_cancelled
  → partial / user_cancelled
else 1..3 complete
  → partial / first remaining reason
else
  → failed / first remaining reason

remaining reason precedence:
process_restart → deadline_cutoff → role_failures
```

Required consequences:

```text
0 complete + user_cancelled       → partial/user_cancelled
0 complete + process_restart      → failed/process_restart
0 complete + deadline_cutoff      → failed/deadline_cutoff
0 complete + only failed roles    → failed/role_failures
1..3 complete + failed remainder  → partial/role_failures
```

6. Rename the outcome count everywhere in Go and report JSON:

```text
FailedRoleCount / failed_role_count
→ IncompleteRoleCount / incomplete_role_count
```

Its value is always `4 - completed_role_count`.

7. Preserve cancellation visibility independently from composition:

```go
Report.CancelRequested bool `json:"cancel_requested"`
```

Pass that value into report construction without putting it back into
`ComposerInput`. The JSON field must always be present, including `false`.

8. Preserve deterministic role/finding order, rejection of missing/duplicate/
unknown/nonterminal rows, zero-findings success, and findings only from complete
roles. Do not change provider behavior.

## Tests

Add table-driven coverage for:

- four complete;
- three complete plus failed;
- one complete plus process restart;
- one complete plus deadline cutoff;
- zero complete: all failed, user cancel, process restart, deadline cutoff;
- mixed user/process/deadline proves user precedence;
- mixed process/deadline proves process precedence;
- interrupted with empty/unknown cause is rejected;
- complete/failed with a cause is rejected;
- every canonical/empty/unknown terminal reason and interruption cause;
- JSON has `incomplete_role_count`, never `failed_role_count`, and always has
  `cancel_requested` with the supplied boolean value;
- existing missing/duplicate/nonterminal/unknown-role rejection remains green.

Assert status, reason, completed count, and incomplete count. No sleeps or map-order
assumptions.

## Scope

After tests pass, update only the relevant composer gap lines in
`docs/ENGINE-CONTRACT.md`. Do not edit README, `docs/CANONICAL-FLOW.md`, this
map/packet, provider/prompt/token behavior, SQLite, migrations, worker, API,
auth, remotes, or UI.

## Verify

```bash
go test ./internal/domain ./internal/review -count=1
gofmt -w internal/domain/status.go internal/domain/status_test.go \
  internal/review/composer.go internal/review/composer_test.go \
  internal/review/engine.go internal/review/engine_test.go
test -z "$(gofmt -l .)"
go test ./... -count=1
go vet ./...
git diff --check
! git grep -n -E 'FailedRoleCount|failed_role_count' -- . \
  ':(exclude)docs/cline/**'
git grep -n 'CancelRequested' -- '*.go'
```

The last grep is an inspection: `CancelRequested` is allowed on the report/read
path, but forbidden in `ComposerInput` and composer decision logic. Inspect the
final diff and prove every changed file belongs to P0.

If all gates pass:

```bash
git add internal/domain/status.go internal/domain/status_test.go \
  internal/review/composer.go internal/review/composer_test.go \
  internal/review/engine.go internal/review/engine_test.go \
  docs/ENGINE-CONTRACT.md
git diff --cached --check
git commit -m "Align composer with canonical terminal outcomes"
```

If an expected listed file is unchanged, `git add` remains safe. Never push.

## Report and stop

```text
P0 RESULT: COMPLETE or BLOCKED
Commit: <hash or none>
Changed files:
- ...
Focused tests:
- <command>: <real result>
Full gate:
- <command>: <real result>
Acceptance evidence:
- reason precedence:
- zero-result cancellation:
- count rename:
- cancel_requested report field:
- invalid enum/cause rejection:
Remaining limitations:
- ...
Git status:
<exact git status --short output>
```

Any failed requirement or check means `BLOCKED` and no commit. Do not begin P1.
