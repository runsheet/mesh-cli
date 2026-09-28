---
title: Goal — Tools show their use: every MCP call is counted per tool, the Tools page says served, called, last used and last error, and a tool can be tested from its page
kind: plan
status: draft
parent: prd
summary: Make tool use visible across the node, platform, and console — the node counts every MCP tool call per server, the platform keeps per-tool usage records, the console's Tools page shows served/called/last-used/last-error, and a Test-tool button runs health checks.
---

# Goal — Tools show their use: every MCP call is counted per tool, the Tools page says served, called, last used and last error, and a tool can be tested from its page

Mesh goal `tools-show-their-use`, running.

## Statement

Chris, 2026-09-28: 'How do we know if brave and perplexity and the other tools are working?' Today nobody can tell from the console. What exists: the item row's sessionTools (what the claim-time read served — this weekend mesh-context to every session, playwright+semgrep to evaluations, context7 to builds); recordToolUsage, which the node sends only for priced tools (pricePerCallCents > 0 — none is priced, so UsageIndex has zero tool rows); and the session transcript, where the real calls are (tool_use events named mcp__<server>__<tool>). Brave and Perplexity have never appeared in a served list because of the role mismatch fixed on 2026-09-28 (PR 'Tool roles speak the console's words'), and there is no page that would have shown it. Make tool use visible: (1) node (mesh-cli): the harness counts every MCP tool call in the stream by server and tool name — calls, errors (tool_result is_error), and wall time — and the usage report that rides the finishing command carries `toolCalls: [{tool, calls, errors, ms}]` beside the total; a server that was served but never called, and a server whose first call errored (bad key, npx failed to start, runtime missing), are named in the item's notes; (2) platform: UsageRecord keeps the per-tool counts (a `tool_calls` list, not only the total), folded onto a ToolUsageIndex per tool per project per day (calls, errors, sessions that had it, sessions that used it); the Tool aggregate's row gets lastUsedAt and lastErrorAt/lastError (redacted of secrets); (3) console: the Tools page shows per tool — attached to which projects and roles, served to N sessions this week, called M times, last used, last error — and a red state for a tool that is served but never called or errors on every call; the tool's page lists the last 20 sessions that called it with links; the item page shows which served tools the session called; (4) a 'Test tool' button on the tool's page runs one call through the platform (a remote MCP's initialize/list_tools with the account's headers; for a local one the node runs it once as a check session) and shows the result; (5) the scorecard's by-model rows gain a tools column (calls per item). Amend mesh-specs trd §3 Tools / adr-006 and runsheet trd §6j, console-trd §3 Tools. Mini needs the by-hand upgrade after the node half merges.

## Acceptance

mesh-cli tests: a stream with three mcp__brave__search calls (one erroring) yields toolCalls [{brave,3,1,ms}] in the usage report, and a served-but-uncalled server is noted on the item; runsheet e2e: the report folds to ToolUsageIndex and the tool row's lastUsedAt; console tests: Tools page shows served/called/last-used/last-error per tool and the red state; the Test-tool button reports a remote tool's list_tools. Live: after the next research run, the Tools page shows Brave and Perplexity with calls > 0 or a named error.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | Harness: every MCP tool call is counted per server with errors and wall time, in both streams | merged | mesh/tools-show-their-use-1 | https://github.com/runsheet/mesh-cli/pull/249 | $0.75 |
| 2 | Gateway: the usage report carries per-tool calls as tools, resent without them to an older platform | merged | mesh/tools-show-their-use-2 | https://github.com/runsheet/mesh-cli/pull/254 | $0.65 |
| 3 | Node: the usage report carries per-tool calls, and a served tool never called or erroring is noted | merged | mesh/tools-show-their-use-2 | https://github.com/runsheet/mesh-cli/pull/254 | $1.85 |

## After merge

After this goal merges the node half lands the per-tool counts in its UsageReport. mesh-cli has no deploy — releases go out as signed GitHub artifacts only. Build for linux/amd64, `scp` to `~/bin/mesh.new`, and `mesh node upgrade --binary ~/bin/mesh.new` (see README.md's "Upgrading a node without ending sessions"). The first per-tool rows in the platform's ToolUsageIndex — and the Tools page showing calls > 0 — are the sign it worked. Until then, the node counts the calls and logs them in a `usage: tools` line per session (items 1–3's code), but the platform sees no `tools` list because the platform half has not landed yet.

## What this project did not build

The platform's ToolUsageIndex, the Tool aggregate's lastUsedAt and lastErrorAt fields, the console's Tools page and red state (served but never called, or errors on every call), the Test-tool button, and the scorecard's tools column live in runsheet/runsheet and runsheet/console. They need a separate goal in each project. Until they land, the platform's UsageReportInput refuses the `tools` list on the usage mutation and the node falls back to sending the total only, logging each session's tools list instead (`ErrNoToolUsage`, said once per run).

## Log

- 2026-09-28 · item 4 · session started
