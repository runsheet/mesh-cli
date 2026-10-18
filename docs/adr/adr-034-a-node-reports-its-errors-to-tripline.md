---
title: "ADR-034: A node reports its errors to Tripline"
summary: A node sends four failure classes — infrastructure, harness_unavailable, sandbox_broken, session_failed — to Tripline (a Sentry-protocol ingest) through internal/tripline's Reporter, configured by node.yaml's `tripline: {dsn, enabled}` or MESH_TRIPLINE_DSN, each event tagged node, item, seq, goal, repo, role and class, fingerprinted [class, Normalise(message)] so alike failures on different items are one issue, and scrubbed by a BeforeSend Scrubber no secret survives; `mesh node start` logs `tripline reporting on (<host>)` or `off` and `mesh node status` prints `reporting: on (tripline <host>)` or why it is off.
status: accepted
---

# ADR-034: A node reports its errors to Tripline

2026-10-17, goal `a-node-reports-its-errors-to-tripline` (items 1–5).
Builds on [ADR-024](adr-024-a-workspace-failure-releases-the-claim.md)
(an infrastructure release) and
[ADR-030](adr-030-an-environment-failure-names-its-kind.md) (an
environment failure names its kind).

## Context

A node's failures were in node.log alone. An operator with several
nodes learnt that every item of a repo had been released for the same
stale `.git/config.lock` only by reading each machine's log. Tripline
speaks the Sentry envelope protocol; a node can send it what it already
tells apart, grouped, without a secret leaving the machine.

## Decision

1. **The classes** are a closed set, `tripline.Class`, each reported
   where node.log already names it:

   | class | where |
   |-------|-------|
   | `infrastructure` | a build's, evaluation's or rebase's `releaseInfrastructure` (`infrastructure: <error>`, a token that could not be written into the repo cache) and a build's workspace release (`workspace: <error>`), as the hold is taken (`holdInfrastructure`) |
   | `harness_unavailable` | a build's or evaluation's session ending in `harness.UnavailableError` (`harness.AsUnavailable`: out of credit, rate-limited) |
   | `sandbox_broken` | an evaluation's `sandbox broken (<why>)` (`Result.SandboxBroken`) |
   | `session_failed` | a build's `session ended without success` |

   Nothing else is sent: no panic hook yet, no ordinary refusal.

2. **One Reporter per node** (`tripline.New(cfg, slug, version)`), its
   own sentry-go client and Hub, never the global. `mesh node start`
   builds it after registration, logs `tripline reporting on (<host>)`
   or `tripline reporting off` (`node.ReportingStartLine`), hands it to
   the Loop and from there to the Runner, Evaluator and Rebaser
   (`Tripline` on each; nil reports nothing), and `Flush`es it for up to
   five seconds as the process ends. A DSN tripline.New refuses is
   logged and reporting is off; it never stops a start.

3. **Fingerprint**: `[class, tripline.Normalise(message, item id, goal
   slug, repo, node)]` — the literal values, then UUIDs, paths, durations,
   hex shas and numbers written as placeholders — so the same failure on
   three items is one issue with a count of three. Each event is tagged
   `class`, `node`, `item`, `goal`, and `seq`, `repo`, `role` when set.

4. **Scrubbing**: every event passes `tripline.Scrubber` as BeforeSend:
   `harness.Redact`'s and `tools.Redact`'s patterns, the literal secrets
   the node gives it (the bearer of `~/.mesh/token` at start), URL
   userinfo, query tokens, cookies, nested maps and stack-frame vars, the
   fingerprint and tags included; only the node's slug survives as a
   server name. `TestScrubberAdversarial` holds it.

5. **Config**: node.yaml

   ```yaml
   tripline:
     dsn: https://<key>@tripline.example/<project>
     enabled: true   # left out, a dsn is on
   ```

   `MESH_TRIPLINE_DSN` is in force over `dsn` and never written back.
   No dsn, or `enabled: false`, reports nothing. `mesh node status`
   prints `reporting: on (tripline <host>)`, `reporting: off (no
   tripline dsn)`, `reporting: off (disabled in node.yaml)` or
   `reporting: off (bad tripline dsn)` (`node.ReportingStatusLine`) —
   the host only, never the DSN's key.

## Proof

`internal/node/tripline_e2e_test.go`'s
`TestNodeReportsInfrastructureToTripline` (`-tags e2e`) ticks a node
loop on the gatewaytest fake with three ready items whose token cannot
be written into the repo cache — git's words quoting the token in the
remote — against `internal/tripline/triplinetest`'s ingest: one Issue,
Count 3, tagged `node` and `item`, each item's id on its own event, no
planted secret in any payload, and the status line `reporting: on`.

## Consequences

- An operator sees a fleet's alike failures as one issue per class and
  cause, with the items behind it.
- A new failure class is a new `tripline.Class` and one `reportFailure`
  call where its log line is; a class not in the set sends nothing.
- crash.Guard does not report a panic yet; a later item may.
