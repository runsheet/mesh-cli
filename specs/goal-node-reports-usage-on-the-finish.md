---
title: Goal — The node reports each session's usage on the command that ends it, so the platform's ledger fills
kind: plan
status: draft
parent: prd
summary: Second half of the 2026-09-27 contract fix — the node reports each session's usage on the finishing command, so the platform's ledger can fill usage records and the console's Usage page shows what actually happened on the mini.
---

# Goal — The node reports each session's usage on the command that ends it, so the platform's ledger fills

Mesh goal `node-reports-usage-on-the-finish`, running.

## Statement

Second half of the 2026-09-27 contract fix (runsheet docs/HANDOFF.md, '2026-09-27: every session ran on the mini's own account'). mesh-cli's internal/gateway/client.go RecordSessionUsage posts a `recordSessionUsage(input: RecordSessionUsageInput!)` mutation, and RecordUsage posts `recordUsage` for priced tool calls. Neither is a field a node may call on runsheet's gateway: `recordUsage` is RegisterPlatformOnly (gateway/scope_usage.go) and `recordSessionUsage` does not exist, so every session logs 'usage: the platform keeps no usage ledger' and the console's Usage page and goal costs see nothing from the node. runsheet's ledger (specs/trd.md §6a 'The usage ledger'; ADR 018 (e)(f), ADR 019) takes a `usage: UsageReport` as an optional input of the command that ends the session — FinishWorkItem (build), EvaluateWorkItem, PlanGoal, FinishWorkItemRebase and RecordDirectionResearch — and each paid tool call as `recordToolUsage { tool_call_id, tool, usage }` on the item. UsageReport (platform/loomgen/models_gen.go) is: session_id, adapter, model, credential_id, credential_kind (subscription|api|unset), input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, tool_calls, wall_ms, list_cost_cents, actual_cost_cents (0 on a subscription), five_hour_before/after, seven_day_before/after (0..1) and their *_resets_at_before/after. Change the node so the UsageRecord it builds today (internal/node/credentials.go Finish, gateway.UsageRecord) travels as `usage` on those five mutations (check the gateway's input field names with introspection at https://api.runsheet.dev/graphql — the schema is public), with credential_id = the id itemCredentials/planCredentials/researchCredentials handed the session and dollars converted to cents; priced tool calls go to recordToolUsage. Keep the log-line fallback for a platform whose finishing command has no `usage` field (unknownField on 'usage'), said once per run. Update the gatewaytest fake to accept `usage` on those mutations and refuse the two old mutations as the platform does. Amend mesh-specs trd §3 Accounts 'The usage record' and adr-005 to name the real path. After merge the mini's binary must be upgraded by hand (mesh-cli has no deploy): note that in the goal doc.

## Acceptance

mesh-cli tests: a build, an evaluation, a plan, a rebase and a research session each send one `usage` report on their finishing mutation with credential_id, tokens, list_cost_cents and the windows before/after, and never the secret; a priced tool call sends recordToolUsage; no test or code path calls recordSessionUsage or recordUsage any more; against a fake whose finishing mutation refuses `usage` the node still finishes the session and logs the record once per run. go build ./... && go vet ./... && go test ./... green on the mini's runners.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | Gateway: a UsageReport rides `usage` on the five finishing mutations, and recordToolUsage | merged | mesh/node-reports-usage-on-the-finish-1 | https://github.com/runsheet/mesh-cli/pull/214 | $0.81 |
| 2 | Harness: each MCP tool call keeps its id, so a priced call can be reported one by one | merged | mesh/node-reports-usage-on-the-finish-2 | https://github.com/runsheet/mesh-cli/pull/213 | $0.53 |
| 3 | Node: the session's usage report rides its finishing command; priced tool calls go to recordToolUsage; the old mutations are gone | review | mesh/node-reports-usage-on-the-finish-3 | https://github.com/runsheet/mesh-cli/pull/215 | $3.92 |
| 4 | mesh-specs: the TRD's usage record and priced calls name the finishing command and recordToolUsage | merged | mesh/node-reports-usage-on-the-finish-4 | https://github.com/runsheet/mesh-specs/pull/45 | $0.36 |
| 5 | Goal doc and README: the mini's binary is upgraded by hand after this merges | claimed | — | — | $0.00 |

## Log

- 2026-09-27 · item 5 · session started
- 2026-09-27 · After merge the mini's binary must be upgraded by hand — mesh-cli has no deploy, so releases go out as signed GitHub artifacts only. Build for linux/amd64, `scp` to `~/bin/mesh.new`, and `mesh node upgrade --binary ~/bin/mesh.new` (see README.md's "Upgrading a node without ending sessions"). The first usage rows on the console's Usage page are the sign it worked. Until then, every session's usage is only in the node's log on the mini, a `usage:` line that names the tokens and windows but mentions the platform keeps no ledger (items 1–4's fallback path).
