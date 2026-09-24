---
title: Goal — A node that cannot pay for a session stops, instead of failing items
kind: plan
status: draft
parent: prd
---

# Goal — A node that cannot pay for a session stops, instead of failing items

Mesh goal `node-out-of-credit`, running.

## Statement

2026-09-23 18:10–21:00Z: the mini's Claude account hit its monthly spend limit, and for three hours every session ended at once with "You've hit your monthly spend limit · raise it at claude.ai/settings/usage" — 8,813 times in node.log. Each was treated as the item's own failure: 8 items failed across 5 goals, each goal spent a re-plan, one planner was retried every few seconds for three hours, and nobody was told. Detect the harness's out-of-credit and rate-limit messages (and a non-zero exit with no output twice running) as the node's condition, not the item's: release the claim without counting an attempt, stop claiming new work, report it on the node's heartbeat (a harness_unavailable status and reason, which the platform turns into one needs_person row — coordinate the field with the platform), and probe once every ten minutes with a minimal session, resuming on success.

## Acceptance

Tests: a session ending with the spend-limit message releases its item with attempts unchanged and puts the node in harness_unavailable; no new claims while in it; a successful probe resumes claiming; the heartbeat carries the reason. README: what an operator sees and does.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | Spec: the node's harness-unavailable condition — ReleaseWorkItem, HeartbeatNode status/reason, detection and the probe | merged | mesh/node-out-of-credit-1 | https://github.com/projectmesh-io/mesh-specs/pull/9 | $0.56 |
| 2 | harness: classify a session's exit as out-of-credit/rate-limited or silently failed | running | — | — | $0.47 |
| 3 | gateway: ReleaseWorkItem and a HeartbeatNode that carries status and reason | evaluating | mesh/node-out-of-credit-3 | — | $0.39 |
| 4 | node: stop on harness-unavailable — release, gate claiming, heartbeat the reason, probe and resume | pending | — | — | $0.00 |

## Log

- 2026-09-24 · item 2 · session started
- 2026-09-24 · item 2 · `ClaudeCode.Run` can now tell a session the account can't pay for apart from a session that simply failed. All the acceptance checks pass: `go vet ./...` and `go build ./cmd/mesh/` succeed, and so do `go test ./internal/harness/...` and the full `go test ./...`. It's one commit on `mesh/node-out-of-credit-2` (`ed09e1a`), not pushed. **What changed:** - **New error type** (`internal/harness/unavailable.go`): `UnavailableError{Reason}` and `AsUnavailable(err)`. A message counts as out-of-credit or rate-limited if it contains, ignoring case, "monthly spend limit", "spend limit", "credit balance is too low", "out of credit", "usage limit" or "rate limit" (also spelled "rate-limit" or "rate_limit"). - **Where it's used** (`claudecode.go`): when the process exits non-zero with no usable result, a match in stderr now returns `UnavailableError` with that stderr text as the reason. Stdout is also checked, but only when the stream held no events at all. That way a session whose own text mentions a rate limit isn't mistaken for an account limit. Unrelated stderr still returns the same plain error as before. - **Deviation from the brief:** in that path `Run` returns the parsed `Result`, not an empty `Result{}`. That lets `EmptyOutput` reach the caller alongside the error, as the brief also asks. - **`Result.EmptyOutput`** (`harness.go`, `stream.go`): `ParseStream` sets it to true when the stream contains no event it can parse. There is no counting across sessions; that's item 4. **Tests** (`unavailable_test.go`, table-driven against a fake `claude` script): - The incident's exact spend-limit sentence on stderr is classified as unavailable, and the sentence appears in `Reason`. - A rate-limit message and a usage-limit message on stderr are classified the same way; so is the spend-limit sentence arriving on stdout. - An unrelated stderr message is not unavailable. - A session whose own text mentions a rate limit is not unavailable. - `EmptyOutput` is true for a silent exit and for a spend-limit exit, and false when a result event arrives; the same holds when `ParseStream` is called directly. - `AsUnavailable` finds the error through a `%w` wrap and returns false for nil. **Design docs:** unchanged. TRD §3 (from item 1) already describes this detection, and the new names are internal Go API, not a gateway contract. Per the repo rules, the commit has no Co-Authored-By trailer.
