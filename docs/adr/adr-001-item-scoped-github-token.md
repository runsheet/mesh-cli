---
summary: A session reaches GitHub with its item's own installation token, fetched from the platform with issueItemRepoToken; an old platform means the operator's login, with a warning.
status: accepted
---

# ADR-001: A session touches GitHub only with its item's token

## Context

A node cloned and pushed with its operator's own `gh` login, so a session
could reach every repo its operator can. The platform (runsheet goal
`item-scoped-github-token`) mints a GitHub App installation token per
item: through the installation of the org that owns the item's repo,
restricted to that one repository, contents write and pull requests read,
living an hour.

## Decision

- The node asks for it with one gateway mutation, only while it holds the
  item's claim:
  `mutation($in: IssueItemRepoTokenInput!) { issueItemRepoToken(input: $in) { token expiresAt repository } }`
  with `in = { aggregateId: <item id>, namespace: "runsheet", nodeId }`.
  `expiresAt` is RFC 3339; `repository` is `owner/name`. The client
  returns it as `gateway.GitHubToken{Token, ExpiresAt, Repository}`
  (`Client.IssueItemRepoToken`, on the `node.Gateway` seam) — callers
  depend on that type, not on the wire shape, which follows the platform
  if it lands differently.
- Anything the platform says no to (not this node's claim, no
  installation reaches the repo) is a refusal (`gateway.IsRefusal`).
- A gateway that does not know the mutation at all — the schema's
  "Cannot query field" / "Unknown type" naming it — is an older platform:
  `gateway.IsUnsupported` reports it (the `Error` carries
  `Unsupported`), and the node falls back to the operator's login and
  says so once. The rollout is not a flag day.

## Consequences

The platform side lands first; until it does, every node takes the
fallback. Once every platform serves the mutation the fallback can go.
