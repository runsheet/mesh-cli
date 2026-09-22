---
title: Goal — Machine identity in Latchkey
kind: plan
status: draft
---

# Goal — Machine identity in Latchkey

Mesh goal `machine-identity-in-latchkey`, paused.

## Statement

Every non-person principal of the runsheet estate — a Mesh node, the Terraform CLI after `terraform login` (terrastore), a Prologue MCP client, a script — authenticates with a credential Latchkey issues natively, instead of runsheet's own rs_* console tokens. The credential is bound to an org and to the person who registered it (the operator), is revocable and listed in the Latchkey console, and its tokens carry the machine as subject, the org's ns claim and the operator. The runsheet gateway, terrastore's write gate and Prologue's surface recognise that principal within the operator's scope; `terraform login` and `mesh node login` obtain it through Latchkey's flows.

## Acceptance

In Latchkey: a machine credential type with registration by an org member, revocation, listing; tokens with a machine subject, the org ns claim and the operator; the `terraform login` authorization-code flow served by Latchkey for the terraform-cli client; e2e covers issue, use, revoke. In runsheet: gateway.Verify, terrastore's Permissions and Prologue's Authorizer accept the machine token scoped as the operator; rs_* keeps working during the transition; e2e covers a machine token at all three. In mesh-cli: `mesh node login` completes Latchkey's flow and the node runs on the machine token; README updated. All suites pass.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | ADR: machine identity as a Latchkey credential | merged | mesh/machine-identity-in-latchkey-1 | https://github.com/latchkeyid/latchkey/pull/95 | $7.34 |
| 2 | Latchkey: machine credentials, tokens and the CLI flows | merged | mesh/machine-identity-in-latchkey-2 | https://github.com/latchkeyid/latchkey/pull/96 | $21.36 |
| 3 | runsheet: the machine principal at the gateway, terrastore and Prologue | merged | mesh/machine-identity-in-latchkey-3 | https://github.com/runsheet/runsheet/pull/22 | $10.71 |
| 4 | Latchkey console: list and revoke machine credentials | review | mesh/machine-identity-in-latchkey-4 | https://github.com/latchkeyid/ui/pull/50 | $5.69 |
| 5 | mesh-cli: node login on the Latchkey credential | running | — | — | $5.12 |

## Log

- 2026-09-22 · item 5 · session started
- 2026-09-22 · item 5 · Done. The branch already carried most of item 5 from earlier sessions; this session audited it end to end, fixed three real gaps, and verified the acceptance. ## What I found already in place `internal/latchkey/` (device flow, refresh grant, claim parsing, a `latchkeytest` fake), `internal/auth/token.go` (both credential shapes in `~/.mesh/token`, lock-and-refresh `Source`), `mesh node login` with and without an `rs_*` argument, the gateway client on a `TokenSource`, and the README's two-credential section. ## What I changed - **`fix(node): a poll-mode build takes its Prologue bearer per item`** (`internal/node/runner.go:66`) — the Runner held one MCP map built when the node started. With an `rs_*` token that was harmless; a machine access token dies within the hour, so every poll-mode build a node started after its first hour handed `claude` a Prologue server with a dead bearer. Runner now has `MCPFunc`, the seam `channel.Session` already had, and poll mode wires it to `prologueMCP`. Test: `TestRunnerAsksForItsMCPServersPerItem`. - **`fix(node): the Latchkey issuer and client survive mesh node register`** (`internal/node/config.go`) — login recorded them onto a *loaded* `node.yaml`, but the first login happens before any registration (registering runs on the credential), and register then wrote a fresh config over them. Either way `--client-id` was needed at every login, contrary to the README. Split the read: `LoadConfig` for a registration, `LoadSettings` for what the file holds; `SaveLatchkey` merges; register carries them over. Tests in `internal/node/config_test.go` and an assertion in the device-flow test. - **`feat(node): mesh node status names the credential the node runs on`** — both shapes are live for the whole rollout, and nothing told an operator which one a node held or what to revoke in the Latchkey console. Status now opens with that line. - README updated for the last two. ## Acceptance `go vet ./... && go build ./cmd/mesh/ && go test ./...` — clean, all packages `ok`, with the fake Latchkey (`internal/latchkey/latchkeytest`) covering discovery, the device flow (pending, `slow_down`, denial, expiry), the refresh rotation and refusal after revocation, plus `internal/commands` driving `mesh node login` and the gateway client against it. README documents both credentials (`README.md:22-71`). No ADR change was needed: everything here is internal seams inside latchkey `adr-machine-identity`'s rollout phase 2 — no aggregate, policy, process or public contract moved.
