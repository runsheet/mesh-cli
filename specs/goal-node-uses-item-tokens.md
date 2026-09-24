---
title: Goal — A session touches GitHub only with its item's token
kind: plan
status: draft
parent: prd
---

# Goal — A session touches GitHub only with its item's token

Mesh goal `node-uses-item-tokens`, running.

## Statement

Scoping, 2026-09-24: a Mesh node clones and pushes with its operator's own gh login — on the mini, Chris's personal GitHub account, which reaches every org — so what an agent session can touch on GitHub is whatever its operator can, not what its item is for. Replace that with a per-item credential: a GitHub App installation token minted by the platform through the installation of the item's repo's own org, restricted to that one repository (the installation-token API's repositories parameter) and to contents:write + pull_requests:read, living an hour. Node half (the platform half is goal item-scoped-github-token on the runsheet project; this goal's items must follow it). The node asks for the item's token when it claims, refreshes it before expiry for long sessions, and gives the session a git credential helper and GH_TOKEN scoped to it — the session's environment must not inherit the operator's gh config (set GH_CONFIG_DIR to an empty per-session dir). Clone, fetch and push use only that token; a node on a platform without the mutation falls back to the operator's login with a warning, so the rollout is not a flag day.

## Acceptance

Tests: a session's environment carries the item token and no operator gh config; a push to a second repo from inside a session fails; a token near expiry is refreshed; against an old platform the node logs the fallback once and still works. README documents that an operator's personal GitHub access is no longer what a session can reach.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | TRD: describe the item-scoped GitHub token, not the operator's gh | review | mesh/node-uses-item-tokens-1 | https://github.com/projectmesh-io/mesh-specs/pull/8 | $0.55 |
| 2 | Gateway: fetch a work item's GitHub installation token | claimed | — | — | $0.00 |
| 3 | Poll-mode and evaluator sessions run on the item's own token | pending | — | — | $0.00 |
| 4 | Channel mode: the shared session never inherits the operator's gh config | pending | — | — | $0.00 |
| 5 | Fallback logged once, and the README says access changed | pending | — | — | $0.00 |

## Log

- 2026-09-24 · item 2 · session started
