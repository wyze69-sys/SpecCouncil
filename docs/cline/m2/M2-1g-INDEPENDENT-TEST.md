# M2-1g Independent Test Task — ingestion blank-code repair

You are testing, not building. Do not modify repository files. Work read-only and
report raw command output.

- Repository: `D:\PROJECT\SpecCouncil` (module `github.com/wyze69-sys/SpecCouncil`)
- Tree under test: `master` HEAD — `23fb266` (repair commit `8c7165e`, acceptance
  `516a16e`). Verify `git log -1 --oneline` before you start and report it.
- Windows toolchain: `export PATH="/c/Users/User/sdk/go/bin:$PATH"` before every
  Go command. Shell is bash (git-bash), not PowerShell.
- Report: raw output only. Every claim needs the command that produced it.
  A missing or skipped check is `UNKNOWN`, never `PASS`.

## What was claimed

An independent audit reported that content containing a fenced code block whose
interior is blank but not empty (for example three backticks, a line with three
spaces, three backticks) was answered with **HTTP 503 service unavailable** — a
client-input error reported as a server outage. The repair claims:

1. **D1** — `ParseBlocks` (`internal/ingest/parser.go`) emits no block when a
   fenced code block's text is blank under `strings.TrimSpace`; a document that
   also contains real content still submits normally.
2. **D3** — `SplitterVersion` is `"2"` (it was `"1"`), because parser output
   changed for the same input.
3. **D2** — `internal/api/ingest_e2e_test.go` proves HTTP submit against a **real**
   `*sqlite.Store`, replacing the earlier proof that used only the in-memory
   `fakeStore` test double.

Your job is to try to falsify these, not to re-run the happy path only.

## Step 0 — reproduce the original defect

Do not take the defect on trust. In a scratch worktree, check out the pre-repair
tree and observe the old behavior:

```bash
export PATH="/c/Users/User/sdk/go/bin:$PATH"
cd /d/PROJECT/SpecCouncil
git worktree add ../sc-prerepair-verify e605999
cd ../sc-prerepair-verify
# add a temporary throwaway test in internal/api that POSTs content "```\n   \n```"
# through a Server built with NewIngestSnapshotProvider(), print the status code,
# run it, then delete the file.
cd /d/PROJECT/SpecCouncil
git worktree remove ../sc-prerepair-verify --force
```

Expected on `e605999`: **503**. If you cannot reproduce 503 on the old tree, the
test you are using cannot detect the bug — report that as a BLOCKER and stop.

## Step 1 — source inspection (read the real code, do not trust this document)

```bash
cd /d/PROJECT/SpecCouncil
git diff --name-only e605999..HEAD          # expect docs + the 6 repair files
git diff e605999..8c7165e -- internal/ingest/parser.go
git diff e605999..8c7165e -- internal/ingest/version.go
git status --short                          # expect empty
```

Confirm:

- the only `parser.go` change is the code-block emission guard now testing
  `strings.TrimSpace(text) != ""` plus its comment;
- `internal/api/server.go` and `internal/api/snapshot_provider.go` are **not**
  touched by `8c7165e` (the 400 mapping already existed and must not be the fix);
- `internal/evidence/**` and `internal/storage/sqlite/**` are untouched.

Report each diff and state whether the claim holds.

## Step 2 — parser behavior, including the edge cases that could break it

Put this throwaway test in `internal/ingest/` (delete it afterwards), run it, and
paste the output:

```go
package ingest_test

import "testing"
import "github.com/wyze69-sys/SpecCouncil/internal/ingest"

func TestProbeBlankFences(t *testing.T) {
	cases := []string{
		"```\n   \n```\n",          // spaces only
		"```\n\t\n```\n",          // tab only
		"```\n \t \n```\n",        // mixed
		"```\r\n   \r\n```\r\n",   // CRLF
		"```\n   \n",              // unterminated, blank
		"```\n   \ncode\n```\n",   // blank line plus real code
		"```\ncode\n   \n```\n",   // real code plus trailing blank line
	}
	for _, c := range cases {
		t.Logf("%q -> %d blocks %+v", c, len(ingest.ParseBlocks(c)), ingest.ParseBlocks(c))
	}
}
```

Expect: the first five produce **0 blocks**; case six produces one code block
whose text is exactly `"   \ncode"`; case seven produces one code block whose text
is exactly `"code\n   "` (interior whitespace must survive byte-for-byte when real
code is present).

Then assert the strongest invariant in the report: **a blank fence is inert.**
The snapshot for `"# H\n\n```\n   \n```"` must have exactly the same hash as the
snapshot for `"# H"`. Print both hashes. Different hashes = FAIL.

## Step 3 — HTTP behavior against the real store

```bash
go test ./internal/api -run 'TestIngestE2E|TestSubmitHandler_ValidSubmitWithProvider|TestSubmitHandler_BlankContentWithProvider' -v -count=1
```

Then confirm the end-to-end tests are not secretly using the fake:

```bash
grep -n 'fakeStore\|sqlite.Open\|newRealStoreServer' internal/api/ingest_e2e_test.go
```

Expect `ingest_e2e_test.go` to use `sqlite.Open` and never `fakeStore`. If any
`TestIngestE2E_*` test constructs `fakeStore`, the D2 claim is FALSE — report it.

Attack cases to run yourself (temporary test, then delete): submit through the
real store and report the status code for each

- content `"```\n   \n```"` → expect **400**, `bad_request`, and **no** snapshot
  row (check with `store.ReadSnapshot` on the derived id; the error is a
  not-found error, not `sqlite.ErrNotFound`, which is the session sentinel);
- content `"```\n   \n```\n\n# Heading"` → expect **201**;
- the document in `docs/cline/m2/M2-1g-TEST-DOC.md` → expect **201**, splitter
  version `2`, **11** units, snapshot hash starting `b52e67a76269`, and unit IDs
  `u0, REQ-12, u2, u3, u4, u5, u6, u7, u8, u9, REQ-13`;
- content `"   "` → expect **400**;
- the same request sent twice with one `idempotency_key` → expect **201** then
  **200** with `replay: true` and the same `session_id`.

Also try one case the packet did not anticipate: a fence whose only content is an
invisible character such as U+200B. Report the observed status and whether Go's
`strings.TrimSpace` strips it — if it does not, that is a finding, not a failure
of the repair.

## Step 4 — full gates and race

```bash
cd /d/PROJECT/SpecCouncil
gofmt -l .
go vet ./...
go test ./internal/ingest -count=10
go test ./internal/api -count=10
go test ./... -count=1
git status --short
```

Race gate (native Windows `-race` is blocked: the toolchain has no cgo compiler).
Run it in WSL Ubuntu, which already has Go at `~/sdk/go/bin`:

```bash
wsl -e bash -lc 'export PATH=$HOME/sdk/go/bin:$PATH; rm -rf ~/race-verify; mkdir -p ~/race-verify; cd /mnt/d/PROJECT/SpecCouncil && git archive HEAD | tar -x -C ~/race-verify; cd ~/race-verify && go test -race ./internal/ingest ./internal/api -count=1'
```

If the race gate cannot be run, report it as `UNKNOWN/BLOCKED` with the exact
error. Never rewrite a blocked gate as PASS.

## Step 5 — scope honesty

Confirm from `git show --stat 8c7165e` and `git show --stat 516a16e` that the
repair touched exactly these six files and the acceptance commit touched docs
only:

```
internal/api/ingest_e2e_test.go
internal/ingest/parser.go
internal/ingest/parser_test.go
internal/ingest/snapshot_test.go
internal/ingest/version.go
internal/ingest/version_test.go
```

Then state plainly which claims are still **deferred by design** and must not be
counted as passing: the splitter version is not persisted to SQLite, and the
provider is not wired into any `cmd/` HTTP binary.

## Report format

```
TREE: <git log -1 --oneline output>
STEP 0 old-tree 503 reproduction: <observed | BLOCKED + error>
STEP 1 source inspection: <claim holds | defect + evidence>
STEP 2 parser cases: <raw block dump>
STEP 2 inert-fence hashes: <both hashes; equal? yes/no>
STEP 3 http: <status code per case>
STEP 3 e2e uses real store: <yes/no + grep output>
STEP 3 replay: <201 then 200 replay=true?>
STEP 4 gates: <raw output per command>
STEP 4 race: <ok | UNKNOWN/BLOCKED + exact error + environment>
STEP 5 scope: <file list + verdict>
DEFERRED (not failures): <list>
VERDICT: PASS | FAIL | CONDITIONAL PASS, with the single strongest reason
```

Only report PASS if every step produced raw output that matches the expectation.
Anything not run is UNKNOWN.
