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
| 5 | mesh-cli: node login on the Latchkey credential | running | — | — | $0.00 |

## Log

- 2026-09-22 · item 5 · session started
