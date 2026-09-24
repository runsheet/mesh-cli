---
title: Goal — A node drains instead of dying, and reads its config live
kind: plan
status: draft
parent: prd
---

# Goal — A node drains instead of dying, and reads its config live

Mesh goal `node-drains`, running.

## Statement

On 2026-09-23 the mini's capacity went from 3 to 6 and the go-apis org was added, and neither reached the node for an hour: the node reads capacity and resolves its orgs only at start, has no drain, and a restart kills its running claude sessions. A watcher waiting for zero sessions never saw one (planners and evaluators don't count against capacity, so the node is rarely idle), and the node was finally restarted by hand mid-session. Give mesh-cli: (1) a drain — SIGUSR1 or `mesh node drain` stops new claims, lets running sessions finish, then exits so systemd restarts it on the new binary/config; (2) capacity and the org list re-read on each tick (node.yaml and MyOrgs), so neither needs a restart; (3) `mesh node status` shows draining.

## Acceptance

Tests: a draining loop claims nothing new and exits after the last running session ends; a capacity change in node.yaml takes effect on the next tick; a newly claimed org is served on the next tick without restart. README documents swapping the binary with a drain instead of a restart.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | TRD §3: document drain and live config reload | failed | — | — | $0.00 |
| 2 | mesh node drain: SIGUSR1 stops new claims, running sessions finish, then the process exits | failed | — | — | $0.00 |
| 3 | Capacity and the served org list are re-read every tick, not just at start | cancelled | — | — | $0.00 |
| 4 | TRD §3: document drain and live config reload | failed | — | — | $0.00 |
| 5 | Capacity and the served org list are re-read every tick, not just at start | failed | — | — | $0.00 |
| 6 | mesh node drain: SIGUSR1 or `mesh node drain` stops new claims, running sessions finish, then the process exits | cancelled | — | — | $0.00 |
| 7 | Capacity and the served org list are re-read every tick | merged | mesh/node-drains-7 | https://github.com/projectmesh-io/mesh-cli/pull/60 | $1.45 |
| 8 | mesh node drain: SIGUSR1 or the command stops new claims, sessions finish, process exits; status shows draining | claimed | — | — | $0.00 |
| 9 | TRD §3: document drain and live config reload | merged | mesh/node-drains-9 | https://github.com/projectmesh-io/mesh-specs/pull/14 | $0.50 |

## Log

- 2026-09-24 · item 7 · session started
- 2026-09-24 · item 7 · The node now re-reads its capacity and the list of orgs it serves at the start of every tick, so a change to either no longer needs a restart. The acceptance check passes: `gofmt -l .` is empty, `go vet ./... && go build ./... && go test -count=1 ./...` passes, and `go test -race` on `internal/node` is clean. The work is committed on `mesh/node-drains-7` as `62ee31d`, with no Co-Authored-By trailer because AGENTS.md forbids one. **What changed** - **The loop** (`internal/node/loop.go`): `Loop` has an optional `Reload` hook that runs at the start of each tick, before heartbeats and claims. The loop logs a line when either value changes, such as `capacity 1 → 2` or `orgs acme → acme, globex`. Two new methods, `Capacity()` and `CurrentScopes()`, let other parts of the node read the values in force now. - **The reloader** (`internal/node/config.go`, `node.Reloader`): - It reads node.yaml again for the capacity and the `orgs` restriction. If the file can't be read, it logs that and keeps the last good values. - It calls `MyOrgs` again. An org seen for the first time gets the node registered in it before being served; if registration fails, it tries again next tick. An org that is no longer listed, or falls outside the restriction, is dropped. - If `MyOrgs` fails, or would leave no org to serve, the node keeps the orgs it had. - **Wiring** (`internal/commands/nodecmd.go`): both poll and channel mode use the reloader, and new orgs are registered the same way `start` does it. Builds, evaluations and channel mode's re-push of held items now use the current org list. Without that, an item from a newly added org would have had no org name to work with. - **README**: the Start section says capacity and orgs are re-read every tick. **Tests** (in `internal/node/loop_test.go`, against the fake gateway): - **(a)** Changing capacity in node.yaml from 1 to 2 makes the second tick claim two items. - **(b)** An org that appears in `MyOrgs` between ticks is registered in, and its ready item is claimed on the second tick. The same test also checks that an org that leaves, or falls outside the node.yaml restriction, is dropped. - **(c)** A node.yaml that can't be read keeps the last good capacity, and the error is logged. **Limits** - **Channel mode:** the per-org goal watch still starts only at launch, so a newly added org gets no goal notifications in the session until the next restart. Its planning, claims and evaluations do work straight away, and the README says so. - **Models:** they are still read only at start. - **Drain:** untouched, as asked (item 8). - **Design docs:** I didn't amend the TRD because it lives in `mesh-specs`, outside this workspace, and item 9 of this goal covers TRD §3.
- 2026-09-24 · item 8 · session started
