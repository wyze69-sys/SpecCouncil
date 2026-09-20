# W7 — HTTP/API Composition Boundary

## Status

READY.

## Mission

Expose the released SpecCouncil submission, status, report, and cancellation
contracts through a real `net/http` API without changing worker or SQLite
behavior.

W7 is an HTTP composition slice. It wires request decoding, identity
resolution, project authorization, scoped persistence calls, deterministic
response mapping, and server lifecycle. It does not implement authentication
storage, a live provider, a worker daemon, or a browser UI.

The target boundary is:

```text
HTTP request
    -> request validation
    -> authentication boundary
    -> project authorization boundary
    -> scoped SQLite contract
    -> explicit HTTP response mapping
```

The API must never call provider code directly. The API must never claim work,
dispatch roles, publish findings, compose reports, or run recovery. The worker
remains the only layer that executes provider work.

## Starting point

Start from the latest clean tree containing verified W1–W6 and verify it before
editing:

```text
daec080 Add worker process supervisor boundary
```

Do not trust a pasted status report. Run `git status --short --branch`, inspect
the live commit, and confirm the tree is clean before implementation.

## Required reading

Read these files before editing:

```text
docs/CANONICAL-FLOW.md
docs/ENGINE-CONTRACT.md
docs/cline/worker/00-BUILD-MAP.md
docs/cline/worker/W6-PROCESS-SUPERVISOR.md
go.mod
internal/domain/*.go
internal/evidence/*.go
internal/storage/sqlite/submit.go
internal/storage/sqlite/read.go
internal/storage/sqlite/cancel.go
internal/storage/sqlite/errors.go
internal/storage/sqlite/store.go
internal/worker/run.go
internal/worker/process.go
cmd/speccouncil/main.go
```

The canonical flow is authoritative. If the existing persistence contract does
not provide a required HTTP operation, stop and report `BLOCKED`; do not add a
new migration or silently issue ad-hoc SQL from the API.

## Exact deliverable

Create a real internal HTTP package, preferably:

```text
internal/api/server.go
internal/api/server_test.go
```

Use `net/http` from the standard library. Do not add a web framework for this
slice.

The package must expose a testable composition root, such as:

```go
func NewServer(Config) (*Server, error)
func (s *Server) Handler() http.Handler
```

The exact names may differ, but the live API must be constructible in tests
without starting a network listener. If a `Run` or `ListenAndServe` helper is
added, it must accept a caller-owned context or server and must shut down
cleanly; tests must use `httptest` against the real handler.

## HTTP routes

Implement exactly these route families unless the live source proves a naming
conflict. Keep the route shape consistent and reject unknown methods:

```text
POST /v1/projects/{project_id}/reviews
GET  /v1/projects/{project_id}/reviews/{session_id}
GET  /v1/projects/{project_id}/reviews/{session_id}/report
POST /v1/projects/{project_id}/reviews/{session_id}/cancel
GET  /healthz
```

The route parser must reject empty, malformed, or extra path segments with a
truthful 404 or 405 response. Do not accept a project ID from the JSON body as
a substitute for the path project ID.

If the repository already has a different frozen route spelling, preserve the
existing spelling and document the exact mapping in the implementation. Do not
invent duplicate aliases.

## Authentication and authorization boundary

Authentication and authorization are not implemented as a user/account system
in W7, but the HTTP layer must preserve the canonical boundary.

Define narrow injectable interfaces or functions for:

```go
type Identity struct {
    Subject string
}

type Authenticator interface {
    Authenticate(*http.Request) (Identity, error)
}

type Authorizer interface {
    CanAccessProject(context.Context, Identity, string) error
}
```

The exact types are implementation choices. The contract is mandatory:

- protected review routes authenticate before persistence access;
- the authenticated identity is passed to authorization;
- authorization checks the project ID from the route;
- a foreign or inaccessible project is exposed as HTTP 404, not 403, to avoid
  project existence leakage;
- authentication failure is mapped consistently to HTTP 401;
- no protected handler may bypass the authenticator or authorizer;
- tests use deterministic fake implementations;
- W7 must not add password storage, sessions, JWT validation, OAuth, accounts,
  roles, or credential configuration;
- a default production server must not silently become publicly writable. If no
  authenticator/authorizer is configured, `NewServer` must reject the config or
  expose only `/healthz`.

`/healthz` may be unauthenticated and must not read or mutate review state.

## Persistence boundary

Define narrow API-facing interfaces satisfied by the released `*sqlite.Store`
or adapters around it. Prefer the existing scoped methods:

```text
Submit
ReadStatusScoped
ReadReportScoped
RequestCancellationScoped
```

The API package must not import database/sql, issue SQL, open transactions, or
call unscoped methods when a project-scoped method exists.

Required persistence behavior:

- submission uses the existing atomic `Submit` contract;
- snapshot freezing and request hashing remain owned by persistence/evidence;
- idempotency behavior remains unchanged;
- status reads use the read-only persistence path;
- report reads use the read-only terminal-report path;
- cancellation uses the existing atomic cancellation mutation;
- the API never calls `ComposeSession` or any worker method for a GET request;
- the API never runs a cancellation sweep; the worker owns role interruption;
- context cancellation is passed to persistence unchanged;
- persistence errors are mapped explicitly and do not leak SQL, DSNs, file paths,
  credentials, or raw database details.

If a request needs an evidence snapshot, use an existing snapshot reader or an
explicit adapter that calls the released read API. Do not reconstruct or mutate
snapshots from untrusted request fields in the HTTP package.

## Request and response contracts

Use explicit DTOs. Do not expose database/sql types or internal transaction
objects. JSON decoding must be strict enough to reject malformed input and
unknown fields where the contract requires it.

### Submit request

The request body must contain the fields required by the released submission
contract:

```json
{
  "idempotency_key": "client-key",
  "title": "Design title",
  "content": "Frozen design content"
}
```

The project ID comes from the route. Preserve title/content bytes exactly for
the persistence hash: do not trim whitespace, normalize Unicode, or change line
endings. Reject an empty body, missing required fields, invalid JSON, trailing
JSON values, and invalid UTF-8.

The API must return the authoritative `SubmitResult`, including whether the
request was a replay. Do not generate a second request hash in the handler.

Recommended mapping:

```text
new submission      -> 201 Created
idempotent replay   -> 200 OK
invalid request     -> 400 Bad Request
idempotency conflict-> 409 Conflict
unauthorized        -> 401 Unauthorized
not authorized      -> 404 Not Found
persistence failure -> 503 Service Unavailable or documented 500
```

The exact status chosen for persistence-unavailable must be consistent and
covered by tests. Do not expose hashes, body content, or internal SQL errors in
conflict/error messages unless the public contract explicitly requires them.

### Status response

`GET /reviews/{session_id}` returns the released deterministic status model.
It must include the persisted session state, cancellation flag, counts, timing
fields allowed by the public contract, and role states. It must not compose a
report or modify rows.

Recommended mapping:

```text
found              -> 200 OK
unknown/foreign    -> 404 Not Found
unauthorized       -> 401 Unauthorized
persistence failure-> 503/500 according to one documented policy
```

### Report response

`GET /report` returns the deterministic terminal report only.

```text
terminal report    -> 200 OK
queued/reviewing   -> 409 Conflict with stable not_finished code
unknown/foreign    -> 404 Not Found
unauthorized       -> 401 Unauthorized
```

The handler must not call the composer when the report is requested. A non-
terminal read maps the existing `ErrNotFinished`/`ErrNotTerminal` contract to a
stable JSON error code such as `not_finished`.

### Cancel response

`POST /cancel` calls the existing scoped cancellation method exactly once.

```text
queued/reviewing changed       -> 202 Accepted, effective=true
already requested or terminal -> 200 OK, effective=false
unknown/foreign               -> 404 Not Found
unauthorized                  -> 401 Unauthorized
persistence failure           -> 503/500 according to one documented policy
```

Cancellation does not directly interrupt roles, change session status, delete
findings, or compose a report. The response reports the committed cancellation
request result only.

### Health response

`GET /healthz` must be deterministic, cheap, and free of review mutations. A
minimal successful response is:

```json
{"status":"ok"}
```

Do not claim the worker is healthy unless W7 has a real, injected health check
for it. Do not expose database paths, provider configuration, credentials, or
internal process state.

## Error response contract

Every non-2xx response must be JSON with a stable shape, for example:

```json
{
  "error": {
    "code": "not_finished",
    "message": "review report is not finished"
  }
}
```

The exact shape may follow an already frozen contract if one exists. Otherwise,
choose one shape and use it for every handler.

Requirements:

- set `Content-Type: application/json` before writing JSON;
- never write a success body after an error status;
- do not leak raw database or provider errors;
- preserve correlation/request IDs only if an existing contract provides them;
- do not log credentials, authorization headers, request bodies, or DSNs;
- method errors return 405 and an `Allow` header;
- malformed JSON returns 400;
- unsupported content types are rejected if the handler requires JSON;
- response bodies are deterministic and tested.

## HTTP lifecycle and safety

`NewServer` must validate required dependencies before returning a server.
Required dependencies include the persistence boundary, authenticator, and
authorizer for protected routes.

The handler must be safe for concurrent requests:

- no mutable request state is stored globally;
- no request identity is stored in a process-global variable;
- no shared response buffer is reused without synchronization;
- request contexts are passed to all downstream calls;
- one slow request must not block unrelated health or read requests;
- server shutdown must not leak goroutines if a listener helper is provided.

Do not add a background worker goroutine in W7. The API may submit and read
sessions while the separately supervised worker is absent; it must return the
truthful persisted state.

## Forbidden scope

Do not modify or add:

```text
internal/storage/sqlite/**
internal/storage/sqlite/migrations/**
internal/worker/**
internal/provider/**
internal/review/**
internal/domain/**
internal/evidence/**
```

Do not add:

- authentication/account implementation;
- authorization database or project-membership tables;
- real provider networking;
- worker startup, process locks, recovery, claims, dispatch, or supervision;
- automatic polling or background report generation;
- schema migrations;
- direct SQL or database/sql in API code;
- browser/UI code;
- framework dependencies;
- fake production persistence;
- empty future packages;
- unrelated formatting or cleanup.

Do not edit `docs/CANONICAL-FLOW.md` or the public README.

## Allowed files

The final implementation must be limited to:

```text
internal/api/server.go
internal/api/server_test.go
docs/ENGINE-CONTRACT.md
docs/cline/worker/00-BUILD-MAP.md
```

If a separate DTO or auth-boundary file is genuinely required, use only:

```text
internal/api/types.go
internal/api/auth.go
```

Do not create more files for aesthetics. If another production package must
change, stop and report `BLOCKED` with the exact path and reason.

## Required tests

Use `httptest.NewRequest`, `httptest.NewRecorder`, and the real `Handler()`.
Do not test only helper functions. Every route test must cross the real router,
authentication boundary, authorization boundary, persistence interface, and
response writer.

At minimum, prove:

1. health returns the exact successful JSON response and never touches storage;
2. route/method rejection returns deterministic 404/405 responses;
3. protected requests reject missing/invalid identity before storage access;
4. foreign project access returns 404 and does not reveal authorization state;
5. submit creates one request through the scoped persistence seam;
6. submit preserves title/content bytes and route project ID;
7. malformed JSON, unknown fields, trailing JSON, missing fields, and invalid
   UTF-8 return 400 without persistence calls;
8. new submission returns 201 with authoritative result;
9. same idempotency replay returns the documented replay response without a
   second semantic submission;
10. idempotency conflict maps to 409 without leaking sensitive body data;
11. status returns the persisted scoped status and performs no mutation;
12. report returns 200 only for a terminal report;
13. non-terminal report maps to 409 with stable `not_finished` code;
14. report handler never calls composition or provider code;
15. cancel calls scoped cancellation exactly once;
16. effective cancellation returns 202;
17. repeated/terminal cancellation returns 200 with `effective=false`;
18. cancellation does not mutate role rows or compose a report in the API;
19. unknown/foreign session maps to 404 for status, report, and cancel;
20. persistence errors map consistently and do not leak raw internals;
21. request context cancellation reaches the persistence seam;
22. concurrent requests do not race or share identity/response state;
23. no credentials, authorization headers, DSNs, or request content appear in
    error responses or logs;
24. every existing W1–W6 test remains green.

Use call counters and barriers rather than sleeps. Test both success and every
error mapping directly.

## Verification ladder

Run from `D:\PROJECT\SpecCouncil`:

```bash
git status --short --branch
git diff --check

test -z "$(gofmt -l .)"
go test -v ./internal/api -count=1
go test -v ./internal/api -count=10
go test ./... -count=1
go vet ./...
git diff --check
```

Run race tests:

```bash
go test -race ./internal/api -count=1
go test -race ./...
```

Native Windows race is expected to require CGO on this machine. If it returns:

```text
go: -race requires cgo; enable cgo by setting CGO_ENABLED=1
```

report native Windows as `BLOCKED`, not `PASS`, and run the exact race commands
in WSL Ubuntu:

```bash
/c/Windows/System32/wsl.exe -d Ubuntu -- bash -lc \
  'cd /mnt/d/PROJECT/SpecCouncil && export CGO_ENABLED=1 && \
   go test -race ./internal/api -count=1 && go test -race ./...'
```

Report both environments separately. A timeout, nonzero exit, or race report is
not a pass.

## Scope audit

Before committing:

```bash
git diff --name-only
git diff --check
grep -R -n 'database/sql\|modernc.org/sqlite\|internal/worker\|internal/provider' \
  internal/api --include='*.go' || true
git status --short
```

Direct database/sql, SQLite, worker, or provider imports in API production code
are forbidden unless the exact packet is amended before implementation. Tests
may use fakes but must not create a substitute persistence runtime.

Any off-scope file, dirty unrelated work, failed test, failed vet, formatting
drift, leaked secret, or unverified race gate means `BLOCKED` and no code commit.

## Documentation requirements

After implementation and verification, update `docs/ENGINE-CONTRACT.md` with a
short W7 section covering:

- real `net/http` composition root and handler;
- protected route authentication and project authorization boundaries;
- 404 anti-enumeration behavior;
- scoped submit/status/report/cancel persistence calls;
- non-terminal report -> 409 `not_finished`;
- cancellation request semantics;
- no worker/provider invocation from HTTP;
- no concrete auth/account system yet.

Mark W7 `COMPLETE` in the build map only after the full gate passes. Do not mark
it complete from API package tests alone.

## Exact commit rule

After all gates pass, commit exactly:

```text
Add HTTP API composition boundary
```

Do not push.

## Required completion report

Return exactly this structure:

```text
W7 RESULT: COMPLETE | BLOCKED | FAILED
Commit: <sha or none>
Changed files:
- <exact path>

Focused tests:
- <command>: PASS/FAIL/BLOCKED

Full gate:
- <command>: PASS/FAIL/BLOCKED

Race gate:
- native Windows: PASS/FAIL/BLOCKED
- WSL Ubuntu: PASS/FAIL/BLOCKED

Acceptance evidence:
- real HTTP handler composition:
- authentication boundary:
- project authorization and 404 anti-enumeration:
- submit and idempotency mapping:
- status read mapping:
- terminal report and not_finished mapping:
- cancellation mapping:
- health endpoint:
- malformed input and method handling:
- context propagation:
- no worker/provider/direct-SQL scope:
- concurrency and cleanup:
- scope audit:

Git status:
<exact output>

Not implemented by W7:
- account/password/OAuth/JWT authentication
- authorization persistence or membership management
- worker daemon or automatic restart
- real provider/network calls
- UI
```

Do not claim `COMPLETE` when an endpoint was only unit-tested below the real
handler, when auth/authorization was bypassed, or when a required race gate is
blocked. Do not push.
