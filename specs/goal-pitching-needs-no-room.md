---
title: Goal — A propose-mode direction researches whatever the project's live-goal ceiling says: pitches take no room
kind: plan
status: draft
parent: prd
summary: A propose-mode direction's proposals take no live-goal slot and never consume the project's ceiling — the node offers one whenever its window is open and an account has headroom, regardless of the project's live-goal room; a set-mode direction keeps the guard. The research session still learns how much room the project has, so a pitch can say it would wait.
---

# Goal — A propose-mode direction researches whatever the project's live-goal ceiling says: pitches take no room

Mesh goal `pitching-needs-no-room`, running.

## Statement

Found 2026-09-27: the mini never offered `scout` or `toward-the-vision` (runsheet project, mode propose, never researched) a session because internal/node/loop.go researchDue skips a direction when RoomFor(d, p, goals) <= 0, and RoomFor (internal/node/research.go) subtracts every live goal of the project from the project's ceiling — 14 running against a ceiling of 8 gives 0. That guard exists so a set-mode session at the ceiling does not pay for proposals the platform would drop (ADR 022). A propose-mode direction sets nothing: its output is pitches (runsheet goal pitches) or, on an older platform, GoalProposals a person accepts, neither of which takes a live-goal slot. Change researchDue: a direction in mode `propose` is offered whenever its window is open and an account has headroom, regardless of RoomFor; a `set` direction keeps the guard. The research session's brief still tells it how much room the project has (RoomFor as information), so a pitch can say it would wait. Amend mesh-specs trd §6c-facing text (Directions on the node) and adr-005/adr-002 wherever RoomFor is described.

## Acceptance

mesh-cli tests: a propose-mode direction on a project at its ceiling is claimed and researched; a set-mode one on the same project is not; the room figure reaches the session's brief in both cases. go build/vet/test green; goal doc notes the mini needs the by-hand upgrade.

## Items

| Seq | Title | Status | Branch | PR |
| --- | --- | --- | --- | --- |
| 1 | Gateway: refresh schema.json so DirectionIndex carries mode (DirectionMode: propose \| set) | merged | mesh/pitching-needs-no-room-1 | https://github.com/runsheet/mesh-cli/pull/223 |
| 2 | Node: a propose-mode direction is offered regardless of RoomFor; a set-mode one keeps the guard; the room still reaches the brief | merged | mesh/pitching-needs-no-room-2 | https://github.com/runsheet/mesh-cli/pull/226 |
| 3 | specs: trd §6c (Directions on the node) and adr-005/adr-002 amendments for propose-mode | — | — | — |
| 4 | Goal doc: the mini needs the by-hand upgrade after this merges | claimed | mesh/pitching-needs-no-room-4 | — |

## Log

- 2026-09-27 · After merge the mini's binary must be upgraded by hand — mesh-cli has no deploy, so releases go out as signed GitHub artifacts only. Build for linux/amd64, `scp` to `~/bin/mesh.new`, and `mesh node upgrade --binary ~/bin/mesh.new` (see README.md's "Upgrading a node without ending sessions"). Until then, the node will not offer propose-mode directions a research session.
