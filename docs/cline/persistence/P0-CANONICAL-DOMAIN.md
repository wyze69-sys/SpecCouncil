# P0 — Canonical Domain Alignment

## Mission

Align the in-memory domain, composer, report JSON, and tests with
`docs/CANONICAL-FLOW.md` before SQLite is introduced. Execute only P0, commit it
only after every gate passes, report, then STOP.

Repository: `D:\PROJECT\SpecCouncil` (Go 1.27.1).

## Start and read

Run `git status --short`, `git branch --show-current`, and
`git log -1 --oneline`. The tree must be clean. If not, preserve it and report
`BLOCKED`; never reset, clean, or stash.

Read:

1. `docs/CANONICAL-FLOW.md` — Fixed state model and Deterministic composer.
2. `docs/cline/persistence/00-BUILD-MAP.md` — protocol and global rules.
3. `internal/domain/status.go` and `errors.go`.
4. `internal/review/composer.go`, its tests, engine, and engine tests.
5. Every repository reference to `ComposerInput`, `FailedRoleCount`,
   `failed_role_count`, `TerminalReason`, and `InterruptCause`.

## Implement

1. Add terminal reason `role_failures`. Add validators for terminal reasons and
   interruption causes; unknown values must fail tests.
2. Remove `CancelRequested` from `ComposerInput`. The composer must use committed
   role outcomes only.
3. Extend `RoleRow` with `InterruptCause`. Enforce:

```text
interrupted → required canonical cause
complete/failed → empty cause
```

4. Keep the requirement for exactly one terminal row for each canonical role.
5. Implement this exact first-match logic:

```text
4 complete
  → complete / all_roles_complete
else any interrupted/user_cancelled
  → partial / user_cancelled
else 1..3 complete
  → partial / first matching reason below
else
  → failed / first matching reason below

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

6. Rename every tracked-code and JSON-contract occurrence:

```text
FailedRoleCount / failed_role_count
→ IncompleteRoleCount / incomplete_role_count
```

Its value is always `4 - completed_role_count`. Update Verdict, Report,
constructors, callers, and tests.
7. Preserve deterministic role/finding order, rejection of missing/duplicate/
unknown/nonterminal rows, zero-findings success, and the rule that only complete
roles contribute findings.

## Tests

Add table-driven tests for:

- four complete;
- three complete plus failed;
- one complete plus process restart;
- one complete plus deadline cutoff;
- zero complete: all failed, user cancel, process restart, deadline cutoff;
- mixed user/process/deadline proves user precedence;
- mixed process/deadline proves process precedence;
- interrupted with empty or unknown cause is rejected;
- complete/failed with a cause is rejected;
- JSON contains `incomplete_role_count` and not `failed_role_count`;
- existing missing/duplicate/nonterminal/unknown-role rejection remains green.

Assert status, reason, completed count, and incomplete count. No sleeps or map-order
assumptions.

## Scope

After tests pass, update only the relevant composer gap lines in
`docs/ENGINE-CONTRACT.md`: mark cause-based reasons and incomplete counts as
implemented. Leave unrelated gaps unchanged.

Do not edit README, `docs/CANONICAL-FLOW.md`, this map/packet, provider behavior,
prompts, token counting, role/session/error names, or call purposes. Add no
SQLite, migration, worker, API, auth, live service, remote, or UI code.

## Verify

```bash
go test ./internal/domain ./internal/review -count=1
gofmt -w <changed-go-files>
test -z "$(gofmt -l .)"
go test ./... -count=1
go vet ./...
git diff --check
git grep -n -E 'FailedRoleCount|failed_role_count|CancelRequested'
```

The final grep must return no matches. Inspect the diff and prove every changed
file belongs to P0.

If all gates pass:

```bash
git add <only P0 files>
git diff --cached --check
git commit -m "Align composer with canonical terminal outcomes"
```

Never push.

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
- invalid cause rejection:
Remaining limitations:
- ...
Git status:
<exact git status --short output>
```

Any failed requirement or check means `BLOCKED` and no commit. Do not begin P1.
