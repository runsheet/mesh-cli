---
title: "ADR-038: A builder tests what it touched"
summary: A host build session's settings carry a second PreToolUse Bash hook, `mesh test-guard --hook`, beside the tool-trim one, refusing a whole-suite test run (`go test ./...`, bare `npm test`/`make test`/`pytest`) or any test run put in the background, its reason naming the validate.yml suites the change touched; the build prompt says the same — test the packages you touched, in the foreground, never the whole suite — and is the rule alone in a pod, which has no mesh binary to run the guard; evaluate, plan and rebase sessions get no guard.
status: accepted
---

# ADR-038: A builder tests what it touched

2026-10-19. Goal a-builder-tests-what-it-touched-and-never-waits-idle.
Sits beside the tool-results hook (docs/spec.md, Tool results on a
node; trd §3 Tool results). Numbered 038 beside
[ADR-038](adr-038-a-session-reaches-every-mcp-server-through-one-proxy.md),
as the 034s and 035s share their numbers.

## Context

Build sessions on dl360-anythink spent their minutes on two habits:
running `go test ./...` over the whole module — several minutes on a
loaded host, most of it packages the change never touched — and putting
a test run in the background and then waiting on it, idle, with the
session's clock running. Neither buys anything: after a build the node
runs `.runsheet/validate.yml`'s suites the diff affects (ADR-013,
ADR-014), so the whole-suite run is done again anyway, and a backgrounded
run is read only once it ends.

## Decision

1. **The guard** (item 1, `internal/harness/testguard.go`,
   `internal/commands/testguard.go`): `mesh test-guard --hook` is a
   Claude Code `PreToolUse` hook on Bash. `harness.TestGuardHook` reads
   the hook's JSON and answers `permissionDecision: "deny"` for
   - a whole-suite run: `go test` with a bare `./...` target, any flags,
     in any statement of the command (`&&`, `||`, `;`, pipes and
     subshells split it, so `cd x && go test ./...` is caught), or a
     statement that is exactly `npm|yarn|pnpm [run] test`, `make test`
     or `pytest`; a scoped target, `./internal/foo/...`, runs;
   - any test command (`go test`, the package managers' `test`, `make
     test`, `pytest`, `jest`, `mocha`, `cargo test`, `mvn test`,
     `gradle test`) with `run_in_background: true`.

   The reason sends the builder to the suites its change touched: the
   checkout's `.runsheet/validate.yml` suites whose paths match the
   files changed since the merge base (`git diff --name-only
   <upstream or origin/HEAD>...HEAD`), else a generic "test the packages
   this change touched". Anything else gets no answer and runs as it is;
   a hook that cannot read its input says so on stderr and lets the call
   run — the guard never fails a session.

2. **Build sessions run it** (item 2): `Runner.toolResults` sets a host
   build job's settings to `BuildSessionSettings(bin, maxKB)` — one Bash
   matcher whose hooks are the guard (`harness.TestGuardCommand`,
   `<mesh> test-guard --hook`, 10 s) and, when `tool_results.max_kb` is
   not 0, the trim (`ToolTrimSettings`). The guard does not depend on
   the cap: `max_kb: 0` turns the trim off and keeps the guard. Both
   hooks see the call as the agent wrote it; a deny wins over the trim's
   rewrite.

3. **Only builds.** The Evaluator, Planner and Rebaser never call
   `toolResults`: an evaluation, a plan's research or a rebase may need
   the whole suite because its acceptance names it, and runs it.

4. **The prompt says it too.** `BuildPrompt` carries `# Test what you
   touched` before the services and the rules: run the tests of the
   packages you touched — `go test ./internal/<pkg>/` or the
   acceptance's own command — never `go test ./...` or another
   whole-suite run, since the node validates validate.yml's suites after
   the finish; never put tests in the background and wait — run them in
   the foreground and keep working on the result.

5. **A sandboxed build has the prompt alone.** The pod runs the node
   image, which carries no mesh binary, and nothing of the host's is
   mounted there (the MCP relay in a pod is a node script for the same
   reason, ADR-029); the trim hook is host-only for that reason already.
   A sandboxed build gets the CLI's output limits in its env and the
   prompt's rule, not the guard. Putting the binary in the image, or
   mounting it, is a later decision.

## Consequences

- A host builder's `go test ./...` or backgrounded test run is refused
  with the suites to run instead, and the session goes on; its count in
  node.log's sessions is the goal's reading.
- An acceptance that literally names `go test ./...` cannot be run by
  the builder on the host; the evaluator and the node's validation still
  run it. Planners should write acceptance by package.
- Tests: `TestHookRefusesWholeSuite`, `TestHookRefusesBackgroundTests`,
  `TestHookReadsValidateYml` (the guard), `TestGuardHookOnBuildNotEvaluate`
  (a build job's settings carry it, an evaluate job's do not),
  `TestBuildPromptTestsWhatItTouched` (the prompt).
