---
summary: A session hands its result to the node through one tool, mesh_result, that the node itself serves on mesh-node through the session's proxy (internal/jobresult, sessionproxy.InProcess); its input is the kind's finish shape, the product's finish_schema validates its part, a call that does not fit is refused to the session with the reason, and the node finishes the job on its own credential with what was recorded. The node's text parsers go finish by finish, and each kind's prompt becomes the brief's guidance first, the job's facts, and one fixed finish section — runsheet ADR 067 decision 10 and migration steps 4a and 4b on the node; the step runner for a kind the node has no code for is the same path.
status: accepted
---

# ADR-041: A session hands its result to the node by tool call

2026-10-23, runsheet ADR 067 (a product publishes its catalogue to
Mesh), decision 10 and migration steps 4a–4c, the node half; the rest
of goal `a-node-runs-any-job` that ADR-040's "Not yet" left. Amends
ADR-023 (sessions finish by tool call), whose hosted-tool finishes and
final-message fallbacks this retires kind by kind.

## Context

Every session's result was read from text: the thread turn's last JSON
block (`ParseThreadReply`), the evaluator's `VERDICT:` and `FAILED:`
lines, the planner's fenced graph, the research JSON block, the rebase
session's `REBASED:`/`REBASE FAILED:` lines. ADR-023 put the hosted
runsheet tools beside them (`mesh_verdict`, `mesh_plan`) and kept the
text as the fallback, so every prompt carried its contract lines, and a
product's guidance could never replace the node's prompt without
breaking a contract the node parsed. runsheet now refuses `mesh_finish`,
`mesh_verdict` and `mesh_release` to a session token
(`contextapi/sessionscope.go`): a session never finishes a job.
runsheet's brief carries the kind's `guidance` text and its
`finishSchema` (specs/contracts/claimJob.json), and runsheet publishes
its own kinds' guidance (`.mesh/agents/<kind>.md`).

## Decision

1. **One result tool, the node's.** `internal/jobresult` holds a job's
   result. `jobresult.New(kind, finish, finishSchema)` is a `Holder`
   for the brief's finish, whose `Tools()` are `mesh_result` and
   `mesh_fail`. `mesh_result` takes the finish's shape — the same keys
   finishJob takes for the kind's finish (runsheet `finishShapes`:
   `{verdict}`, `{plan}`, `{research}`, `{rebasedHead}`, `{answer and/or
   proposal}`, …) — and, when the brief carries a `finishSchema`,
   validates the product's part (the proposal of an answer-or-proposal
   kind, the whole `result` of a `result` kind) against it with
   `github.com/google/jsonschema-go`, as runsheet's finishJob does. A
   call that does not fit is refused to the session as the tool's
   error, naming the shape, the key or the field, so the session
   corrects it and calls again; an accepted call is held by the node
   and a second one replaces the first. `mesh_fail(error)` says the job
   cannot be done. The holder's `Result()` is what the node's
   `finishJob` sends, after its own checks, on its own credential: the
   session never finishes.
2. **Served over the session's proxy.** The tools are served on the
   node's own server name, `mesh-node` (`harness.NodeWorkMCP`), as an
   in-process upstream of the session's sessionproxy
   (`sessionproxy.InProcess`, an `http.Handler` speaking streamable
   HTTP: `mcptool.Handler`), so a session on the host or in a pod reaches
   them on the one path it reaches every other server (ADR-038).
   `SessionTools.NodeTools` carries them into `UseVia`, which adds the
   upstream and the server's shim entry; a session whose other servers
   are not proxied gets a proxy of the node tools' own
   (`serveNodeToolsAlone`). `Job.NodeTools` carries them for a test
   double standing in for a session. A build keeps its stdio `mesh-node`
   (ADR-023's amendment): its finish is the node's push, no session
   result.
3. **The prompt is the guidance, the facts and the finish.** A kind
   whose finish is a tool runs on the brief's `guidance` first,
   verbatim; then the job's facts (the item, the goal, the thread
   transcript, the repo, the context rule and the tools section); then
   the one fixed finish section (`Holder.FinishSection`: call
   `mesh_result` once with the shape, its keys named, the schema when
   there is one, `mesh_fail` when the job cannot be done, nothing in the
   final message read). The node's built-in role text for that kind is
   removed. A brief with empty guidance (a platform before ADR 067) runs
   the built-in prompt with the finish section in place of its contract
   lines, logged once per kind (`brief carries no guidance; running the
   built-in prompt`).
4. **Finish by finish.** Each kind moves only once its result is the
   tool, so removing its role text removes no contract; its parser is
   deleted with it, and a test proves the result arrives through the
   tool and the prompt carries the guidance with none of the removed
   text. Order: goal-chat (`ParseThreadReply`), then evaluate and plan,
   then research and rebase, and build's role text (its finish stays
   the node's own validation and push).
5. **The step runner.** A thread job of a kind the node has no code
   for runs on the same path, built from the brief alone: harness,
   guidance, transcript, pods and access as the brief says, mesh_result
   for the finish, finishJob with the result.

## Consequences

- A product's guidance is free prose: no line of it is parsed, so none
  of it can break the contract, which is the finish section's alone.
- A session that forgets the call hands nothing in: the job ends as
  "no result (no mesh_result call)" for its kind — a thread answered
  that it could not answer, an evaluation with no verdict — never read
  from the text. The fallback log lines of ADR-023 go with the fallbacks.
- `Result.WorkCalls` still records a `mesh_result` call from the stream
  (tools.IsWorkTool), for the record; what the node acts on is the
  holder's.
- An adopted session's result (ADR-009) lives in the node that served
  its tools: the adopter serves a fresh holder, and a call made before
  the hand-over is lost — the session's end then reads as no result.

## Done so far

- goal-chat: `mesh_result` on the relay, `finishSchema` in the brief and
  the vendored fixtures (`claimJob.json`, `jobKinds.json`), the prompt
  on the brief's guidance, `ParseThreadReply` gone.
