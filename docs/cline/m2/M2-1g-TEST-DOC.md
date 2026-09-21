# M2-1g manual test document

Purpose: a submission document for manually exercising deterministic ingestion
after the M2-1g repair (`8c7165e`). Expected result: HTTP 201, splitter version
`2`, snapshot hash starting `b52e67a76269`, and the 11 units listed below.

Submit the fenced block below as the `content` field (for example
`POST /v1/projects/<project>/reviews` with `idempotency_key` and `title`, or
through the ingestion tests).

## Content to submit

```text
# Payments Service Design

REQ-12 The service shall settle payments within 200ms.

## Constraints

- Retries use exponential backoff
- Failed settlements are queued for manual review

| field | rule |
| --- | --- |
| amount | positive only |
| currency | ISO-4217 |

```go
func settle() error { return nil }
```

```   
```

REQ-13 The service shall emit an audit record for every settlement.
```

The line inside the second fence holds three spaces. Keep them: a fence whose
text is blank but not empty is exactly the input that used to answer HTTP 503.

## Expected units (exact)

| # | ID | Kind | Text |
|---|---|---|---|
| 0 | `u0` | brief | `Payments Service Design` |
| 1 | `REQ-12` | requirement | `REQ-12 The service shall settle payments within 200ms.` |
| 2 | `u2` | brief | `Constraints` |
| 3 | `u3` | requirement | `Retries use exponential backoff` |
| 4 | `u4` | requirement | `Failed settlements are queued for manual review` |
| 5 | `u5` | data_rule | `\| field \| rule \|` |
| 6 | `u6` | data_rule | `\| --- \| --- \|` |
| 7 | `u7` | data_rule | `\| amount \| positive only \|` |
| 8 | `u8` | data_rule | `\| currency \| ISO-4217 \|` |
| 9 | `u9` | constraint | `func settle() error { return nil }` |
| 10 | `REQ-13` | requirement | `REQ-13 The service shall emit an audit record for every settlement.` |

The blank fence contributes no unit and consumes no order, which is why the
Constraints heading is `u2` and the code block is `u9` rather than shifting.

Two notes:

- `u6` is the Markdown table separator row becoming its own `data_rule` unit.
  That is pre-existing parser behavior, not part of this repair, but it is noise
  worth deciding on in a later slice.
- A document containing only the blank fence must answer HTTP 400
  `bad_request` ("content produced no reviewable evidence") and persist nothing.
