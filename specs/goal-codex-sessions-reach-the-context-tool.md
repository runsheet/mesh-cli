---
title: Goal — Codex sessions reach the context tool: MCP errors are recorded, a session that cannot reach it falls back to Claude, and a tool failure is never a verdict
kind: plan
status: draft
parent: prd
summary: Codex evaluation sessions record MCP errors and keep their stream 24h, the preflight detects when the context tool is unreachable and falls back to Claude, the default Codex model for opus evaluations is gpt-6-sol, and tool-failure evaluations re-evaluate on the other family instead of failing.
---

# Goal — Codex sessions reach the context tool: MCP errors are recorded, a session that cannot reach it falls back to Claude, and a tool failure is never a verdict

Mesh goal `codex-sessions-reach-the-context-tool`, running.

## Statement

On 2026-09-28 07:18Z the mini began evaluating on Codex (gpt-6-astra), but the built-in context tool was unreachable inside Codex sessions — the MCP call was failing with 'Auth required' or network errors in ≤2 ms. Two evaluations failed cheaply ($0.29, $0.39) and sent items back to ready for rework. The evaluator judged these sessions **blind** (no verdict because the context tool never worked) rather than failing them. Chris parked the Codex credential until this is fixed.

Make Codex evaluation sessions visible and resilient: (1) **Harness**: record each MCP call's error text (first 200 chars) in the usage report and name the first failing tool in the item note; keep the session's Codex event stream and stderr under the work dir for 24 hours like Claude sessions. (2) **Evaluator**: before a Codex session starts, the harness runs `codex mcp get` on the context tool in the prepared home with the session env; if the tool is unreachable or refuses auth, log why and fall back to Claude for that evaluation — **a tool failure never becomes a verdict**. (3) **Defaults**: Codex evaluations on opus-tier accounts default to **gpt-6-sol** (cost ~$0.45 vs astra's ~$1.29), with sonnet → gpt-6-luna and haiku → gpt-6-luna; astra only when an item pins it. (4) **No-verdict rule**: an evaluation that fails because the context tool never worked is not a verdict — the same item runs again on the other family (Claude), and stays in evaluating if the other family also fails or has no account. Amend mesh-specs adr-011 and the TRD; the mini needs the by-hand node upgrade after merge, then un-park the credential.

## Acceptance

mesh-cli tests: a Codex session's MCP call errors are recorded in the usage report and the item note names the first failing tool; the session's event stream and stderr are kept 24 hours; a preflight against the context tool refuses Codex and falls back to Claude when the tool is unreachable; the default Codex model for an opus evaluation is gpt-6-sol; a tool-error evaluation re-evaluates on the other family rather than failing the item. go test ./... passes. Live: with the credential un-parked, a Codex evaluation on the mini shows prologue calls with 0 errors in its usage record, and the item's evaluation note reads like one that saw the code (not a blind fallback).

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | Harness: a Codex MCP call's error text is kept, reported on the tool entry and named in the item note | merged | mesh/codex-sessions-reach-the-context-tool-1 | https://github.com/runsheet/mesh-cli/pull/285 | $1.50 |
| 2 | Harness: a Codex session's event stream and stderr are kept under the work root for 24 hours | merged | mesh/codex-sessions-reach-the-context-tool-2 | https://github.com/runsheet/mesh-cli/pull/288 | $1.20 |
| 3 | Node: a Codex evaluation is preflighted against the built-in context server, and falls back to Claude when it cannot reach it | merged | mesh/codex-sessions-reach-the-context-tool-3 | https://github.com/runsheet/mesh-cli/pull/286 | $2.10 |
| 4 | Harness: find and fix why the built-in context tool fails inside a real Codex evaluation session | merged | mesh/codex-sessions-reach-the-context-tool-4 | https://github.com/runsheet/mesh-cli/pull/289 | $0.95 |
| 5 | Node: the default Codex model for an opus-tier evaluation is gpt-6-sol; sonnet and haiku map to gpt-6-luna; astra only when pinned | merged | mesh/codex-sessions-reach-the-context-tool-5 | https://github.com/runsheet/mesh-cli/pull/287 | $1.80 |
| 6 | Node: an evaluation whose context tool never worked is not a verdict; it runs again on the other family and the item stays in evaluating | — | mesh/codex-sessions-reach-the-context-tool-6 | https://github.com/runsheet/mesh-cli/pull/290 | $2.40 |

## After merge: the mini

After this goal merges, mesh-cli has no deploy and the mini's binary must be upgraded by hand. Build for linux/amd64 (`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o mesh-linux ./cmd/mesh`), `scp` to `~/bin/mesh.new`, and `mesh node upgrade --binary ~/bin/mesh.new` (see README.md's "Upgrading a node without ending sessions", line ~523). Sessions are adopted seamlessly; the evaluator's first evaluation after the upgrade runs the preflight and chooses its family based on headroom and tool reachability. Codex evaluations default to gpt-6-sol; astra is never chosen unless the planner pins it.

## After merge: un-park the credential

Chris parked the `codex - chatgpt pro` credential (limitCredentialNodes to the retired laptop node) at 2026-09-28 07:35Z. After the upgrade, return it to active service: in the runsheet console, **Settings → Credentials**, find `codex - chatgpt pro`, click to edit, clear the `limitCredentialNodes` field, and save. The node will discover it on the next claim-time read and offer Codex evaluations when it has headroom.

## Live check

After the upgrade and un-park:
1. Trigger a Codex evaluation on the mini (e.g., evaluate a work item).
2. On the item page, check the evaluation note: it should read like a note that understood the code (e.g., naming specific files, functions, or logic), not a fallback note like "no verdict — prologue: Auth required".
3. Check the usage report's `tools` entry for `prologue` (the built-in context server): it should show 0 errors and a call count matching the evaluation's tool calls.

## Owed on the runsheet project

This goal could not carry the runsheet-side pieces:

- **runsheet/gateway**: The usage report's tool entry should carry an `error` field (the first tool error's text, ≤200 chars, redacted, or null if no error). The existing `calls`, `errors`, and `ms` fields roll up across calls; the `error` field names *why* the first one failed (e.g., 'Auth required', 'connection refused', '500 Internal Server Error'). Schema addition, introspection, and usage record mutation.
- **runsheet/console**: Nodes page and Work Item details should show tool errors from the usage report — both the count and the text of the first error — so operators can quickly spot tool-related issues.

## Design

This goal amends mesh-specs' `trd` §1 (second-opinion paragraph, tool-failure rule) and §3 (Codex defaults), and updates mesh-cli ADR-011. See items 1–6's PRs and the `.mesh/context/` copies.

**Error recording**: Every MCP call's error is captured (first 200 chars) in the harness's usage report. The item note names the first failing tool and its error so an operator can quickly see what went wrong. This is the ground truth for debugging tool-related evaluation failures.

**Event stream**: A Codex session's event stream (all harness output, including Codex's internal logging) and stderr are kept under the session work directory for 24 hours, like Claude sessions, for debugging and auditability. The path is `<workdir>.session/codex-stream.jsonl` and `<workdir>.session/stderr.log`.

**Preflight**: Before any Codex evaluation starts, the harness runs `codex mcp get mesh-context` (or a tools/list handshake) in the prepared CODEX_HOME with the session's environment. If the context tool is unreachable, refuses auth, or never connects (fast failure), the evaluator logs the preflight failure and does not run on Codex for that item — it falls back to Claude instead. A pinned family with a failed preflight waits (not claimed, not failed), like a family with no headroom. This prevents tool-failure evaluations from ever becoming verdicts.

**No-verdict rule**: An evaluation session whose built-in context tool (or a required console tool) errored on every call, or never connected, has its verdict discarded. The same job runs again on the other family (the evaluation proper), and the item's note opens with the failed attempt and the usage report folds both sessions' costs and tool calls. If the other family also fails or has no account, the item stays in evaluating (one blind per node, per item, per mode). This ensures tool issues are surfaced, not masked.

**Defaults**: Codex models for evaluations:
- `opus` → `gpt-6-sol` (default, ~$0.45 per evaluation)
- `sonnet` → `gpt-6-luna` (default, cheaper)
- `haiku` → `gpt-6-luna` (default, cheapest)
- `astra` → `gpt-6-astra` (only when explicitly pinned by the planner, ~$1.29 per evaluation)

The planner may pin a specific model (e.g., `gpt-6-astra`) to override these defaults.

## Node.yaml: unchanged

The `codex:` block in node.yaml is optional. The defaults above apply to all Codex accounts (issued credentials and node-owned). No schema changes.
