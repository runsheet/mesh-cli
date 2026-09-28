---
title: Goal — Codex sessions reach the context tool: MCP errors are recorded, a session that cannot reach it falls back to Claude, and a tool failure is never a verdict
kind: plan
status: draft
parent: prd
summary: Codex evaluations on the mini failed due to an approval policy refusal; the node now preflights the tool, records errors, and re-evaluates on the other family when the tool fails, keeping a 24-hour stream of each Codex session for debugging.
---

# Goal — Codex sessions reach the context tool: MCP errors are recorded, a session that cannot reach it falls back to Claude, and a tool failure is never a verdict

Mesh goal `codex-sessions-reach-the-context-tool`, running.

## Statement

2026-09-28 07:18Z the mini went to mesh-cli 9ed5f8b and evaluations started on Codex (gpt-6-astra) from the issued `codex - chatgpt pro` credential. The new per-tool call counter shows the built-in context tool failing on every call inside Codex sessions — `prologue: 3 calls, 3 errors` in 31–92 ms, `prologue: 1 call, 1 error` in 2 ms, and Codex's own `codex_apps` erroring too — so the evaluator judged blind: of four Codex evaluations two failed cheaply ($0.29, $0.39) and sent their items (evidence-record-per-merged-change/9, a-process-sends-only-what-it-declares/9) back to `ready`, a rework each. Chris parked the Codex credential (limitCredentialNodes to the retired laptop node) at 07:35Z so evaluations fall back to Claude until this is fixed; un-park it when done. What is known: the rendering is right — the exact config.toml the harness writes (`[mcp_servers.prologue] url = … env_http_headers = { Authorization = MESH_CODEX_MCP_0_TOKEN }`, harness.CodexMCPConfig) with the node's rs_ token in that variable lists and calls the mesh_* tools from `codex exec` on the mini itself (codex-cli 0.157.0) and on the laptop (0.157.1); an empty variable gives 'Auth required during MCP startup'. So in a real evaluation session the variable is missing or wrong, or the call is rejected locally (≤2 ms says no network): candidates are the env the harness builds for a platform-issued account (prepareHome's jobCodexHome path vs the owned-home path, SessionEnviron's Unset list), a token that is not the node's (the built-in's Authorization for an evaluation), Codex's approval policy for MCP calls in `exec`, or the sandbox mode the harness passes. Do: (1) capture the truth — the harness records each MCP call's error text (first 200 chars) in the usage report's tool entry and the item's note names the first failing tool and its error; the harness keeps the session's Codex event stream under the session's work dir for 24 h as Claude sessions' output is kept; (2) preflight — before a Codex session starts, the harness runs `codex mcp get <builtin>` (or a tools/list handshake) in the prepared CODEX_HOME with the session env and, if the context server is not reachable or refuses auth, the evaluator does not run on Codex for that item (it logs why and uses Claude), so a tooling failure can never become a verdict; (3) find and fix the root cause with the captured error; (4) evaluate on gpt-6-sol by default (CodexModels: opus tier → sol, sonnet/haiku → luna), astra only when an item pins it — astra cost $1.29 for one evaluation where opus costs ~$0.45; (5) an evaluation that fails with a tool error is not a verdict: the item stays `evaluating` and is re-evaluated on the other family instead of going back to `ready`. Amend mesh-specs adr-011 and the TRD's Codex section; the mini needs the by-hand upgrade (or the v0.1.x release) after merge, then un-park the credential.

## Acceptance

mesh-cli tests: a Codex session whose MCP server refuses auth records the error text on the tool entry and the item note; the preflight refuses Codex and falls back to Claude when the context server is unreachable; the default Codex model for an opus-tier evaluation is gpt-6-sol; a tool-error evaluation re-evaluates on the other family rather than failing the item. Live: with the credential un-parked, a Codex evaluation on the mini shows prologue calls with 0 errors in its usage record, and the item's evaluation note reads like one that saw the code.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | Harness: a Codex MCP call's error text is kept, reported on the tool entry and named in the item note | merged | mesh/codex-sessions-reach-the-context-tool-1 | https://github.com/runsheet/mesh-cli/pull/285 | $2.63 |
| 2 | Harness: keep a Codex session's whole event stream and stderr 24h | merged | mesh/codex-sessions-reach-the-context-tool-2 | https://github.com/runsheet/mesh-cli/pull/288 | $5.85 |
| 3 | Node: a Codex evaluation is preflighted against the built-in context server, and falls back to Claude when it cannot reach it | merged | mesh/codex-sessions-reach-the-context-tool-3 | https://github.com/runsheet/mesh-cli/pull/286 | $2.50 |
| 4 | Harness: find and fix why the built-in context tool fails inside a real Codex evaluation session | merged | mesh/codex-sessions-reach-the-context-tool-4 | https://github.com/runsheet/mesh-cli/pull/289 | $3.63 |
| 5 | Node: the default Codex model for an opus-tier evaluation is gpt-6-sol; sonnet and haiku map to gpt-6-luna; astra only when pinned | merged | mesh/codex-sessions-reach-the-context-tool-5 | https://github.com/runsheet/mesh-cli/pull/287 | $3.20 |
| 6 | Node: a tool-error evaluation re-evaluates on the other family, not failing the item, and the item stays evaluating | failed | mesh/codex-sessions-reach-the-context-tool-6 | https://github.com/runsheet/mesh-cli/pull/290 | $5.10 |
| 7 | (replaced by re-plan) | replaced | — | — | — |
| 8 | (replaced by re-plan) | replaced | — | — | — |
| 9 | Node: an evaluation whose context tool never worked is not a verdict; it runs again on the other family and the item stays in evaluating (item 6's work rebased onto main) | merged | mesh/codex-sessions-reach-the-context-tool-9 | https://github.com/runsheet/mesh-cli/pull/305 | $1.14 |
| 10 | mesh-specs: ADR 011 and the TRD's Codex adapter section record the preflight, the kept error and stream, the model defaults, the root cause and that a tool failure is never a verdict | merged | mesh/codex-sessions-reach-the-context-tool-10 | https://github.com/runsheet/mesh-specs/pull/70 | $2.58 |
| 11 | Goal doc: codex-sessions-reach-the-context-tool — what landed, the root cause, the mini's by-hand upgrade and un-parking the Codex credential | in progress | mesh/codex-sessions-reach-the-context-tool-11 | — | — |

## What was found

Item 4 established the root cause: Codex's approval policy for MCP calls in `exec` with `--sandbox workspace-write` requires each server to carry `default_tools_approval_mode = "approve"` in its table. The sandbox is a condition (exec approves calls itself only when there is no sandbox to protect), not the cause. The candidates per `docs/codex-events.md`:

- **Ruled in: the cause** — Codex's approval policy for MCP calls in `exec`.
- **Ruled in, as the condition** — `--sandbox workspace-write` (a call needs approval under it; without the sandbox exec approves calls itself).
- **Ruled in as a defect, not this failure** — Console tools rendered as `${…}` variable references instead of resolved values until the Codex renderer was wired in item 4 (`tools.CodexConfig`, `CodexMCPConfig` resolves `${VAR}` or refuses it).
- **Ruled out** — The env the harness builds (`prepareHome`'s job-home path vs the owned-home path, the Unset list in `SessionEnviron`), the token on the built-in, and a startup timeout.

## After merge

The mini's by-hand upgrade: build for linux/amd64 (`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o mesh-linux ./cmd/mesh`), `scp` to `~/bin/mesh.new`, and `mesh node upgrade --binary ~/bin/mesh.new` per README.md's "Upgrading a node without ending sessions" (line ~523). The first evaluation after the upgrade runs on whichever family has headroom; with no headroom on Codex it falls back to Claude (the `ChooseAccount` step pre-filters by headroom).

Then un-park the Codex credential by removing the `limitCredentialNodes` restriction to the retired laptop node on the console (Settings → Credentials → codex - chatgpt pro → edit), so evaluations can use it again.

What to look for: a Codex evaluation's usage record on the mini with prologue calls and 0 errors (the `tools` list in the usage mutation); an evaluation note that reads like one that saw the code (not blind, referring to the actual logic); node.log lines `Codex preflight` (successfully passed) and `evaluating on` (family choice); the kept stream under the work root for 24 h (`<work root>/kept-sessions/<name>/output.jsonl` and `stderr.log`).

## What this project did not build

The platform's consumption of the tool entry's `error` field and a console view of it are runsheet's — the error text is on the usage report but the console does not yet render it.

## Log

- 2026-09-28 07:18Z · mini to mesh-cli 9ed5f8b, Codex evaluations begin, context tool fails on every call
- 2026-09-28 07:35Z · Codex credential parked to the laptop node
- 2026-09-28 · item 11 · session started
