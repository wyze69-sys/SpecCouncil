# M2-3 — One safe real-provider adapter (Cline / OpenAI-compatible)

Slice: M2-3 (product)
Start commit: current `master` HEAD at release time (`7ab4ad2` when written).
Depends on: M2-2 (`84affd4`) — complete and verified.
Module: `github.com/wyze69-sys/SpecCouncil`, Go 1.27.1
Windows toolchain: `export PATH="/c/Users/User/sdk/go/bin:$PATH"` before every Go command.

## Mission

Add exactly one real `provider.Provider` implementation that calls the Cline
OpenAI-compatible HTTP endpoint, so a review can run against a live model instead
of the fake. This is the first slice that talks to a real model. It must be a
drop-in for the existing `provider.Provider` interface: **no change to the worker,
the engine retry logic, validation, persistence, or the fake.** The engine already
owns retry/repair/budget; the adapter only turns one `provider.Request` into one
HTTP call and one `provider.Response`, and classifies failures into the existing
`domain.ErrorCategory` values so the engine's own logic reacts correctly.

## Ground truth established by a live smoke test (2026-09-21)

These are real observations against `https://api.cline.bot/api/v1`, not assumptions.
Build to them exactly:

1. Auth is a bearer token: header `Authorization: Bearer <key>`. `GET /models`
   returned HTTP 200 with the key, proving the key and base URL.
2. The correct model id is **`deepseek/deepseek-v4.1-flash`** (there is NO
   `cline-pass/` prefix — that is a routing label, not the API model id).
3. **The success body is NOT standard OpenAI shape.** The choices are nested under
   a top-level `data` key, and there is a `provider_metadata` cost block:
   ```json
   {"data":{"choices":[{"finish_reason":"stop","index":0,
     "message":{"content":"4","provider_metadata":{...cost...}}}]}}
   ```
   A stock OpenAI client that reads top-level `choices` will break. Parse
   `data.choices[0].message.content`.
4. **The model is intermittently flaky.** The identical request returned
   HTTP 500 `{"error":"empty response content","success":false}` on one call and
   HTTP 200 with correct content on the immediate retry. This is a transient
   upstream fault, so it MUST be classified `domain.ErrTransport` (retryable), not
   `ErrProviderRejected`. The engine's existing transport-retry (`PurposeTransportRetry`,
   `MaxProviderCallsPerRole = 2`) then handles it — the adapter itself performs NO
   internal retry loop; it makes one HTTP call per `Call` and returns.
5. A wrong/unknown model id also returns HTTP 500 with a body naming a
   `model_not_found` upstream — see error mapping below; do not treat every 500 as
   transport blindly, but the specific `"empty response content"` message is
   transport.

## The adapter

New package: `internal/provider/cline/`, one file `cline.go` (plus its test file).
It implements `provider.Provider` (`Call(ctx, provider.Request) (provider.Response, error)`).

### Constructor

```go
// Config is the frozen, per-review configuration of the Cline adapter.
type Config struct {
    BaseURL    string        // e.g. "https://api.cline.bot/api/v1"
    APIKey     string        // bearer token; never logged
    Model      string        // e.g. "deepseek/deepseek-v4.1-flash"
    HTTPClient *http.Client  // optional; if nil, a client with Timeout is built
    Timeout    time.Duration // per-call ceiling; if zero, default 60s
    MaxTokens  int           // output cap; if zero, a sane default (e.g. 1024)
}

func New(cfg Config) (*Provider, error)
```

`New` validates: non-empty `BaseURL`, `APIKey`, `Model`; returns a plain error on
violation. It trims a trailing slash from `BaseURL`. It never reads the key from
env or disk itself — the caller supplies it (the composition root reads the file).

### `Call` behavior

1. If `ctx.Err() != nil`, return `provider.NewError(domain.ErrTimeout, ...)`.
2. Build the request body:
   ```json
   {"model": cfg.Model,
    "messages": [{"role":"user","content": req.Prompt}],
    "max_tokens": cfg.MaxTokens,
    "temperature": 0,
    "stream": false}
   ```
   Deterministic: `temperature:0`, no random fields, stable key order via a struct.
3. `POST {BaseURL}/chat/completions` with headers `Authorization: Bearer <key>`,
   `Content-Type: application/json`. Use `http.NewRequestWithContext(ctx, ...)` so
   the call is cancelled on deadline.
4. On transport-level failure (dial error, `ctx` deadline, client `Do` error):
   - if the error is a context deadline/cancel → `domain.ErrTimeout`;
   - otherwise → `domain.ErrTransport`.
5. Read the body fully. Map by HTTP status:
   - **200**: parse the `data` envelope (below). If it yields non-empty content,
     return a `provider.Response{Body: <the content bytes>, Model: cfg.Model,
     TokensIn/Out from usage if present else 0}`. Note: `Body` is the model's
     answer text (the JSON the review schema expects the model to emit), exactly as
     the fake returns its `Body`; parsing/validation stays downstream.
   - **200 but content empty or envelope missing** → `domain.ErrTransport`
     ("empty response content") so the engine retries.
   - **429** or **500/502/503/504** → `domain.ErrTransport` (retryable), EXCEPT:
     if the 500 body contains `model_not_found` (or `"type":"model_not_found"`),
     that is a fatal misconfiguration → `domain.ErrProviderRejected`.
   - **500 with `"empty response content"`** → `domain.ErrTransport` (the observed
     flaky case).
   - **400 / 401 / 403 / 404** → `domain.ErrProviderRejected` (auth/config faults,
     not retryable).
   - any other 4xx → `domain.ErrProviderRejected`.
6. Never log the API key or full prompt. Error messages may include HTTP status and
   a truncated (<=200 char) upstream error message, never the key.

### Response envelope parsing

Parse defensively — treat any of {malformed JSON, missing `data`, empty `choices`,
missing `message.content`, whitespace-only content} as the empty-content transport
case (step 5). Do not import an OpenAI SDK; hand-roll the minimal struct:

```go
type clineResponse struct {
    Data struct {
        Choices []struct {
            Message struct {
                Content string `json:"content"`
            } `json:"message"`
        } `json:"choices"`
        Usage struct {
            PromptTokens     int `json:"prompt_tokens"`
            CompletionTokens int `json:"completion_tokens"`
        } `json:"usage"`
    } `json:"data"`
}
```
(If `usage` is absent, tokens are 0 — acceptable.)

### Compile-time interface check

`var _ provider.Provider = (*Provider)(nil)`.

## Tests — `internal/provider/cline/cline_test.go`

All tests run against an `httptest.NewServer`; NO test performs a network call to
`api.cline.bot`, and NO test reads the real key. Cover:

1. `New` validation: empty BaseURL/APIKey/Model each return an error; a good config
   returns a usable provider; trailing slash on BaseURL is trimmed.
2. Happy path: stub server returns the real `data`-envelope shape; assert
   `Response.Body == "the content"`, `Model == cfg.Model`, and tokens parsed from
   `usage` when present.
3. Request shape: the adapter sends `Authorization: Bearer <key>`, the right model,
   `temperature:0`, `stream:false`, and the prompt in `messages[0].content`
   (assert by decoding the request the stub server received).
4. Flaky 500 empty-content → returns an error whose `provider.CategoryOf` is
   `domain.ErrTransport` (so the engine will retry).
5. 200-but-empty-content and malformed-JSON body → both `domain.ErrTransport`.
6. `model_not_found` 500 body → `domain.ErrProviderRejected` (NOT transport).
7. 401 and 400 → `domain.ErrProviderRejected`.
8. Context already cancelled → `domain.ErrTimeout`, and a stub that sleeps past a
   tiny `Timeout` → `domain.ErrTimeout` (use a short deadline, no real waiting).
9. Key hygiene: the adapter's returned error strings never contain the key value.

## Live smoke test — NOT a unit test, opt-in only

Add `internal/provider/cline/livesmoke_test.go` guarded so it never runs in CI or
the normal suite:

```go
//go:build clinelive

func TestClineLiveSmoke(t *testing.T) { ... }
```

It reads the key from `SPECCOUNCIL_CLINE_KEY` env (skip with `t.Skip` if empty),
does ONE real `Call` with a trivial prompt, and asserts a non-empty body OR a
transport error (the model is flaky, so a single transient failure is acceptable —
retry once inside the test). This proves the wire format against the live endpoint
without polluting the default `go test ./...`. The default suite (no build tag)
must never hit the network.

## Composition (wire it, but keep the demo safe)

Do NOT change the fake-based demo default. Add a separate, explicitly-opt-in path:
`cmd/speccouncil` gains a flag or env switch (e.g. `SPECCOUNCIL_PROVIDER=cline`)
that, when set, reads the key from `.secrets/cline_api_key` (path overridable via
`SPECCOUNCIL_CLINE_KEY_FILE`), constructs `cline.New(...)`, and uses it instead of
the fake. When the switch is unset, behavior is byte-for-byte the current fake demo.
Reading the key: read the file, `TrimSpace`, error clearly if empty/placeholder.
The key file stays git-ignored (already done: `/.secrets/` in `.gitignore`).

If wiring the demo cleanly is more than a few lines or risks touching frozen worker
code, STOP after the adapter+tests and report — the adapter package with full unit
tests is the core deliverable; the demo switch is secondary.

## Gates

```bash
export PATH="/c/Users/User/sdk/go/bin:$PATH"
cd /d/PROJECT/SpecCouncil
gofmt -l .                                  # empty
go vet ./...                                # clean
go test ./internal/provider/... -v -count=1 # new adapter tests pass
go test ./... -count=1                      # every package green, network untouched
go build ./...                              # builds
# WSL race (gcc present): go test -race ./internal/provider/...
```

The default `go test ./...` MUST NOT require network or the real key. The live
smoke test only runs with `-tags clinelive` and the env key set.

## Allowed paths

- `internal/provider/cline/cline.go` (new)
- `internal/provider/cline/cline_test.go` (new)
- `internal/provider/cline/livesmoke_test.go` (new, build-tagged)
- `cmd/speccouncil/main.go` (opt-in provider switch ONLY; fake default unchanged)

## Forbidden

`internal/provider/provider.go` (the interface is frozen — do not change it),
`internal/provider/fake/**`, `internal/worker/**`, `internal/review/**`,
`internal/domain/**`, `internal/storage/**`, `internal/api/**`, `internal/ingest/**`,
`internal/evidence/**`, migrations, `go.mod`/`go.sum` (no new deps — stdlib
`net/http` + `encoding/json` only), any doc. No new third-party dependency. No push.
No committing the key. If the adapter cannot satisfy the interface without changing
`provider.go`, STOP and report — that would be a real interface gap.

## Commit

One commit, message exactly:

```
Add Cline real-provider adapter behind the frozen provider interface
```

## Completion report format

```
SLICE: M2-3
START: <sha> / FINAL: <sha>
CHANGED: <git diff --cached --name-only>
INTERFACE: <confirm provider.go unchanged; var _ provider.Provider assertion present>
ENVELOPE: <confirm parses data.choices[0].message.content, not top-level choices>
ERROR MAP: <table: HTTP status/body -> domain.ErrorCategory, incl. flaky-500->transport, model_not_found->rejected, 401->rejected, timeout->timeout>
TESTS: <each unit test and what it asserts; confirm all use httptest, none hit the network, none read the real key>
LIVESMOKE: <confirm build-tagged clinelive, skips without env key, NOT in default suite>
COMPOSITION: <what the opt-in switch does; confirm fake demo default byte-for-byte unchanged, or report STOPPED-before-wiring>
GATES:
  gofmt -l .                    -> <empty|output>
  go vet ./...                  -> <clean|output>
  go test ./internal/provider/... -> <PASS|FAIL>
  go test ./... -count=1        -> <per-package>
  go build ./...                -> <ok|err>
RACE: <WSL output | UNKNOWN/BLOCKED with reason>
PUSHED: no
NOTES: <deviations, or NONE>
```
