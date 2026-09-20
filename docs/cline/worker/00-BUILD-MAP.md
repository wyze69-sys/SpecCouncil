# SpecCouncil Worker Build Map for Cline

## Purpose

Build the post-persistence worker phase in small, independently verified slices.
The worker owns one session at a time, serializes dispatch decisions, and is the
only layer allowed to invoke provider work. HTTP, authentication, live provider
configuration, and UI remain later phases.

## Authority

Read in order:

1. `docs/CANONICAL-FLOW.md` — runtime authority.
2. The current packet in this directory.
3. Existing Go code and tests.
4. `docs/ENGINE-CONTRACT.md` — verified seams and gaps only.
5. `docs/cline/persistence/00-BUILD-MAP.md` — persistence contract.

## Global rules

- Repository: `D:\PROJECT\SpecCouncil`; Go 1.27.1.
- Persistence APIs are used as released; do not weaken or bypass SQLite guards.
- No provider call occurs while a SQLite transaction is open.
- One worker process handles one review session at a time.
- The process lock must be released automatically when the process exits or
  crashes. A stale PID or database flag is not an acceptable substitute.
- No HTTP, auth, UI, live provider, supervisor, broker, lease, heartbeat, or
  cross-review behavior in the first worker slices.
- Use barriers and deterministic fakes; never sleeps for race proof.
- Do not edit `docs/CANONICAL-FLOW.md` or add progress narrative to README.
- Workers commit but never push. Hermes verifies before releasing the successor.

## Gate after every slice

```bash
test -z "$(gofmt -l .)"
go test ./... -count=1
go vet ./...
go test -race ./...
git diff --check
git status --short
```

## DAG

```text
W1 exclusive process ownership + crash-safe lock COMPLETE (8798053)
 |
 v
W2 restart recovery + single-session claim loop COMPLETE (236ad36)
 |
 v
W3 serialized guarded dispatch loop COMPLETE (150303f)
 |
 v
W4 role execution deadlines and provider-attempt budget COMPLETE
 |
 v
W5 publication/composition integration and supervisor boundary READY

W5 packet: `docs/cline/worker/W5-PUBLICATION-COMPOSITION.md`
```

## Definition of done

- Every worker slice has focused executable tests and an independently verified
  commit.
- The worker never claims a second session while one is active.
- A second process exits cleanly when ownership is held.
- A crashed/exited owner leaves the lock acquirable without manual cleanup.
- No later-phase API, provider, or UI code is smuggled into a worker slice.
- Hermes independently reruns focused and full gates before release.
