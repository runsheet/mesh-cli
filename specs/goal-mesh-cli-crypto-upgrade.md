---
title: Goal — mesh-cli's golang.org/x/crypto is current, and its 13 alerts close
kind: plan
status: draft
parent: prd
---

# Goal — mesh-cli's golang.org/x/crypto is current, and its 13 alerts close

Mesh goal `mesh-cli-crypto-upgrade`, running.

## Statement

GitHub reports 13 open Dependabot alerts on projectmesh-io/mesh-cli (7 critical, 2 high, 4 moderate), every one in golang.org/x/crypto (the ssh package: agent and key constraints not enforced, @revoked not honoured, deadlocks and panics). Upgrade golang.org/x/crypto (and whatever go mod tidy then requires of golang.org/x/*) to the latest release, run the full suite, and check whether the node actually uses x/crypto/ssh; if it does not, say so in the goal doc.

## Acceptance

go.mod pins the latest golang.org/x/crypto; go vet, go build and go test ./... pass; after merge the repo's open Dependabot alerts for golang.org/x/crypto are zero.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | Upgrade golang.org/x/crypto to latest and record whether the node uses x/crypto/ssh | claimed | — | — | $0.00 |

## Log

- 2026-09-24 · item 1 · session started
