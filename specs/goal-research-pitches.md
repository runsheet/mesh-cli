---
title: Goal — Research sessions pitch: a propose-mode direction records pitches with evidence, bet, scope, estimate, risks and signal — not bare goal proposals
kind: plan
status: draft
parent: prd
summary: The platform's pitches (runsheet goal pitches, ADR 026, trd §6c) are live; the node writes proposals instead. Change the research prompt and RecordDirectionResearch for a propose-mode direction to ask for and hand in pitches in the platform's shape — problem, evidence with sources, the strategy bet, scope, estimate, risks, the signal to move, and answers to open questions on earlier pitches — while a set-mode direction keeps proposals.
---

# Goal — Research sessions pitch: a propose-mode direction records pitches with evidence, bet, scope, estimate, risks and signal — not bare goal proposals

Mesh goal `research-pitches`, running.

## Statement

The platform's pitches (runsheet goal pitches, ADR 026, trd §6c) are live: RecordDirectionResearchInput takes `pitches` (and `answers`) beside the old `proposals`, `pitchIndexs` serves them, and the console has /orgs/<org>/pitches. The node (mesh-cli internal/node/research.go, harness research prompt) still writes only `proposals`, so 2026-09-27's runs of `toward-the-vision` produced GoalProposals ('the director proposes, you set' Needs-you rows) and `scout` produced nothing — zero pitches exist. Change the node: the research prompt for a propose-mode direction asks for pitches in the platform's shape (read the input type by introspection at https://api.runsheet.dev/graphql and runsheet specs/trd.md §6c Pitches: problem, evidence with sources, the strategy bet it serves — a slug H2 of handbook specs/strategy.md — scope, estimate, risks, the signal it expects to move, and the goal or initiative it would set), including answers to open questions on its earlier pitches (the platform hands them in the research brief — check what the direction read returns), and RecordDirectionResearch carries `pitches` + `answers`; a set-mode direction keeps proposals. Against an older platform without `pitches` fall back to proposals (unknownField), said once. Update mesh-specs trd §6c-facing text and the research prompt tests.

## Acceptance

mesh-cli tests: a propose-mode research session's output is parsed into pitches and sent as `pitches` with every field, answers included; a set-mode direction still sends proposals; a platform refusing `pitches` gets proposals. go build/vet/test green; the goal doc says the mini needs the by-hand upgrade. After upgrade, one run of toward-the-vision records ≥1 pitch visible at /orgs/runsheet/pitches.

## Items

| Seq | Title | Status | Branch | PR |
| --- | --- | --- | --- | --- |
| 1 | Harness: a propose-mode research prompt asks for pitches and answers in the platform's shape, and ParsePitches reads them | merged | mesh/research-pitches-1 | https://github.com/runsheet/mesh-cli/pull/233 |
| 2 | Gateway: RecordDirectionResearch carries pitches and answers, falls back to proposals on a platform without them, and schema.json knows the pitch inputs | merged | mesh/research-pitches-2 | https://github.com/runsheet/mesh-cli/pull/234 |
| 3 | Node: a propose-mode research session hands in pitches and answers; a set-mode one keeps proposals; a platform without pitches is said once | review | mesh/research-pitches-3 | https://github.com/runsheet/mesh-cli/pull/235 |
| 4 | mesh-specs: TRD 'Directions on the node' describes pitching, and ADR 010 records that a propose-mode session pitches | merged | mesh/research-pitches-4 | https://github.com/runsheet/mesh-specs/pull/51 |

## Log

- 2026-09-27 · After merge the mini's binary must be upgraded by hand — mesh-cli has no deploy, so releases go out as signed GitHub artifacts only. Build for linux/amd64, `scp` to `~/bin/mesh.new`, and `mesh node upgrade --binary ~/bin/mesh.new` (see README.md's "Upgrading a node without ending sessions"). Until then, the node will not record pitches from toward-the-vision and scout. The goal's last acceptance (one run of toward-the-vision recording ≥1 pitch at /orgs/runsheet/pitches) is checked by hand after the upgrade.
