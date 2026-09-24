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
| 2 | Gateway: fetch a work item's GitHub installation token | running | — | — | $0.65 |
| 3 | Poll-mode and evaluator sessions run on the item's own token | pending | — | — | $0.00 |
| 4 | Channel mode: the shared session never inherits the operator's gh config | pending | — | — | $0.00 |
| 5 | Fallback logged once, and the README says access changed | pending | — | — | $0.00 |

## Log

- 2026-09-24 · item 2 · session started
- 2026-09-24 · item 2 · The node can now ask the platform for a claimed item's GitHub token and tell an old platform apart from a refusal. `go vet`, the build, and `go test ./...` all pass, including `./internal/gateway/...`. **The platform side hasn't landed yet.** On runsheet, goal `item-scoped-github-token` is still running: item 1 is ready and item 2 (the `issueItemRepoToken` mutation) is pending. So I wrote the call from that goal's own wording, and no real platform has answered it yet. The query is `issueItemRepoToken(input: {aggregateId, namespace, nodeId}) { token expiresAt repository }`. If the platform ships a different shape, only the query and decoding inside `Client.IssueItemRepoToken` need to change; callers only see the Go type. **What changed:** - **`internal/gateway/client.go`:** - A `GitHubToken` type holding the token, its expiry and the repo it's restricted to (`owner/name`). - `Client.IssueItemRepoToken(ctx, itemID, nodeID)`, which fetches it. - `IsUnsupported(err)`, which follows the existing `IsRefusal`/`Error` pattern: `Error` gains an `Unsupported` flag, set only when the GraphQL error is the schema saying it doesn't know `issueItemRepoToken` or its input type. That error also counts as a refusal, so callers should check `IsUnsupported` first. - **`internal/node/workspace.go`:** the method is added to the `Gateway` interface. The real `*gateway.Client` is the only implementer; the channel tests' fake wraps `gatewaytest.Fake` and doesn't implement it. - **`internal/gateway/gatewaytest/fake.go`:** - `GitHubTokens` sets the token the fake returns for each item. The fake only hands it to the node that holds the item's claim. - `Unsupported` makes the fake reject a mutation the way an old platform would ("Cannot query field …"). - **`internal/gateway/githubtoken_test.go`** covers four things: - A successful fetch decodes the token, expiry and repo. - The old-platform case reports `IsUnsupported`. - A refusal (another node's item, or a no from the platform that names the mutation) is a refusal but not unsupported, and a transport failure is neither. - A compile-time check that `*gateway.Client` satisfies `node.Gateway`. - **`docs/adr/adr-001-item-scoped-github-token.md`:** a new ADR recording the contract and the fall-back-to-the-operator's-login rule. It's the first ADR in this repo. It's in one commit, `b043153`, with no Co-Authored-By trailer as this repo's rules require.
