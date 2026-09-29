---
title: Goal — The node says no verdict when its tools failed, and the two tooling failures stop happening
kind: plan
status: draft
parent: prd
summary: "The after-window share of tainted evaluations is pending: the mini still runs v0.2.191 at 16:20Z, and v0.2.193, the release carrying #450, was published at 16:09:40Z. Before it, 57 of the 68 failed items with evaluation text are tainted (84%). `prologue: not found` is (b) in the narrow sense, not (a): codex-sessions-reach-the-context-tool covered the evaluation path, but its tool-failure rule read the context tool's own `not found` answer as an unreachable tool."
---

# Goal — The node says no verdict when its tools failed, and the two tooling failures stop happening

Mesh goal `evaluation-reports-no-verdict`, running. The evidence is
runsheet/handbook `research/2026-09-29-evaluations-fail-on-plumbing-not-quality.md`
(PR #52). The two recurring texts are `no verdict — prologue: not
found` and `bwrap: loopback: Failed RTM_NEWADDR`; this document
answers which of the goal's two readings is true for the first.

## prologue: not found — which is true

It is **(b) in the narrow sense and not (a)**: every occurrence
postdates the mini running v0.2.188, which carries
`codex-sessions-reach-the-context-tool`, from 2026-09-29 00:51:59Z
(the earliest occurrence is 01:14:55Z), and that goal did cover the
evaluation path — #286 preflights a Codex evaluation, #289 approves its
MCP calls, #305 adds `Evaluator.toolBroken` — but #305's rule read the
context tool's own `not found` answer to a wrong `mesh_document` slug
as a tool that was never reached, and discarded the verdict.

So (b) holds only narrowed to that one case: the path was neither
unbuilt nor unwatched, and the tool was reachable; the goal's own rule
on that path misclassified the tool answering.

### The items

Read through `mesh_items` on 2026-09-29 at 14:17Z (project
`runsheet/mesh` for `a-finished-session-leaves-no-process`,
`builtin-is-mesh-context` and `signed-releases-on-runsheet-mesh`;
project `runsheet/runsheet` for the rest). These are the items whose
`last_error` is `evaluation failed 3 times` and whose evaluation notes
carry `prologue: not found`; every such note opens `Codex (first
attempt, gpt-6-sol): no verdict — prologue: not found`. Times are UTC,
from each evaluation's `at`; **P** marks the evaluations carrying that
text.

| Project | Item | Evaluations (2026-09-29 unless noted) |
|---|---|---|
| runsheet/mesh | a-finished-session-leaves-no-process/24 | 09:48:45, 10:20:15 **P**, 10:32:27 **P** |
| runsheet/mesh | builtin-is-mesh-context/1 | 09-28 22:28:08, 06:22:16 **P** |
| runsheet/mesh | signed-releases-on-runsheet-mesh/18 | 01:40:25 **P**, 01:46:28, 02:19:39 |
| runsheet/runsheet | a-node-has-an-owner/1 | 02:42:31, 04:29:45, 05:06:18 **P** |
| runsheet/runsheet | a-node-has-an-owner/8 | 08:23:11, 09:22:50 **P**, 10:41:25 **P** |
| runsheet/runsheet | attested-validation-replaces-duplicate-ci/4 | 01:37:40, 02:11:18 **P**, 03:23:29 **P** |
| runsheet/runsheet | attested-validation-replaces-duplicate-ci/26 | 08:28:33, 10:25:04 **P**, 10:34:22 |
| runsheet/runsheet | credentials-follow-the-enterprise/2 | 02:51:30 **P**, 04:54:35, 05:49:05 |
| runsheet/runsheet | credentials-follow-the-enterprise/3 | 01:40:32, 03:27:22, 04:16:00 **P** |
| runsheet/runsheet | enterprise-creation-makes-its-tenant/3 | 09-28 13:27:47, 01:36:14 **P**, 03:11:09 **P** |
| runsheet/runsheet | enterprise-creation-makes-its-tenant/8 | 09:39:16 **P**, 10:10:26, 10:17:13 |
| runsheet/runsheet | slipway-first-build/7 | 08:07:31 **P**, 08:33:47, 08:42:58 |
| runsheet/runsheet | slipway-first-build/8 | 08:57:39, 09:21:49 **P**, 09:29:02 |

`builtin-is-mesh-context/1` lists only its second and third
evaluations; the first is not on the record. Beyond these 13, the
earliest evaluation carrying the text on any item is 2026-09-29
01:14:55Z (`a-finished-session-leaves-no-process/17`, its only
evaluation, the item failed for another reason), and the latest the
same read found is 11:39:30Z (`attested-validation-replaces-duplicate-ci/32`,
still `ready`). None falls before 09-29 01:14Z.

### Against the merges and the mini's version

Merge times, `gh pr view <n> --repo runsheet/mesh-cli --json mergedAt`:

| What | Merged (main) |
|---|---|
| #258 prompts name `mesh_document`/`mesh_search` on mesh-context | 2026-09-28 07:28:56Z |
| #285 a Codex MCP call's error text is kept | 2026-09-28 10:57:35Z |
| #286 a Codex evaluation is preflighted | 2026-09-28 11:00:40Z |
| #289 the root cause: `default_tools_approval_mode = "approve"` | 2026-09-28 11:17:09Z |
| #305 a tool failure is never a verdict (`toolBroken`) | 2026-09-28 22:19:35Z |
| #264 the served built-in is mesh-context | 2026-09-29 01:19:46Z |
| #396 the same, re-landed with the prologue alias | 2026-09-29 06:52:27Z |

What the mini ran, from `~/.mesh/node.log`'s start lines
(`grep -a ' mesh node v0\.\| mesh node [0-9a-f]\{7\},' ~/.mesh/node.log`)
and `specs/goal-codex-sessions-reach-the-context-tool.md`:

- **From 2026-09-28 07:18Z, 9ed5f8b.** That predates every merge above,
  #258 included (merged eleven minutes later, 07:28:56Z).
- **From 2026-09-28 11:36:23Z, 4d8d435** (node.log: `mesh node
  4d8d435`). It predates #305, so it could not write `no verdict —
  <tool>: <error>`.
- **From 2026-09-29 00:51:59Z, v0.2.188** (commit 5a8c29e; node.log:
  `2026/09/29 00:51:59 mesh node v0.2.188` and `confirming update
  4d8d435 → v0.2.188: start 1 of 3`, restarted on the same version at
  04:00:32Z). It contains #258, #285, #286, #289 and #305, not #264 or
  #396, so it still renders the built-in as server `prologue`.
- From 2026-09-29 14:13:19Z the mini is moving to v0.2.191; that is
  after every count in this section's evidence and is item 12's
  subject.

The text `no verdict — <tool>: <error>` exists only from #305 onward,
and the first binary on the mini carrying #305 is v0.2.188, started
00:51:59Z; the earliest occurrence is 01:14:55Z. The failures therefore
do not predate the goal — not (a) — and were written by the binary
that preflights, approves and judges the evaluation path.

### The code path that said `not found`

The mini keeps each Codex session's stream 24 hours under
`~/.mesh/work/kept-sessions/<session>/output.jsonl`, so the count
below is dated: counted on the mini on 2026-09-29 at 14:16Z, over the
files last written at or before the cutoff **2026-09-29 14:00:00Z**
(the oldest kept stream then was last written 01:01:52Z), every one
written by v0.2.188.

```sh
cd ~/.mesh/work/kept-sessions
FILES=$(find . -path './*/output.jsonl' ! -newermt '2026-09-29 14:00:00Z' \
  | xargs grep -l '"server":"prologue"')
echo "$FILES" | wc -l                                   # sessions
Q='select(.type=="item.completed" and .item.type=="mcp_tool_call" and .item.server=="prologue")'
cat $FILES | jq -c "$Q | select(.item.status==\"completed\")" | wc -l
cat $FILES | jq -r "$Q | select(.item.status==\"failed\" and .item.error==null)
  | [(.item.result.content[0].text // \"<none>\" | .[0:70]), .item.tool] | @tsv" | sort | uniq -c
cat $FILES | jq -r "$Q | select(.item.status==\"failed\" and .item.error!=null)
  | .item.tool" | sort | uniq -c
```

| Kept streams to 2026-09-29 14:00:00Z | Count |
|---|---|
| Sessions with a call to server `prologue` | 320 |
| Calls `completed` | 1,330 |
| Calls `failed` with `error` null (the server answered with its own error result) | 425 |
| — `not found` | 403 |
| —— `mesh_document` | 383 |
| —— `mesh_items` | 17 |
| —— `mesh_documents` | 3 |
| — any other text | 22 |
| —— `mesh_health_service answers only the estate (a token with ns.runsheet)` | 8 |
| —— `a project is named org/slug` (4 `mesh_document`, 2 `mesh_documents`) | 6 |
| —— `validating "arguments": … required: missing properties` (2 `mesh_document`, 2 `mesh_items`) | 4 |
| —— `validating "arguments": … unexpected additional properties` (1 each `mesh_search`, `mesh_items`, `mesh_health_nodes`, `mesh_evidence`) | 4 |
| Calls `failed` with a real `error` (`tool call failed for prologue/…`; 3 `mesh_health_stuck`, 1 `mesh_goals`, 1 `mesh_documents`) | 5 |

No call ended in any other status. So the tool answered: of the 425
null-error failures, 403 are its `not found`, 383 of those to
`mesh_document`, and only 5 calls never reached it. The `mesh_document`
misses asked, for the most part, for a context file's *name* with
`.md` dropped (`instructions-runsheet-runsheet-agents`,
`adr-adr-003-enterprises`) rather than the slug in INDEX.md's `slug`
column — the file `adr-adr-032-….md` is slug `adr-032-…`.

The chain, in code as it stood before this goal's fix:

1. `ParseCodexStream` (`internal/harness/codexstream.go`) counts a
   `failed` `mcp_tool_call` as an error. These calls carry no `error`,
   so `codexCallError` falls back to the result's text and the
   server's `FirstError` becomes `not found`. `ParseStream`
   (`internal/harness/stream.go`) does the same for a Claude
   `is_error` tool_result.
2. `Evaluator.toolBroken` (`internal/node/evaluate.go`) called a server
   broken whenever `Errors >= Calls` on it, so a session whose only
   context calls asked for wrong slugs looked exactly like one that
   never reached the tool.
3. `afterEvaluate` discarded that verdict, wrote `Codex (first attempt,
   gpt-6-sol): no verdict — prologue: not found` at the head of the
   item's notes, and re-ran the job on Claude — whose verdict, often
   itself blocked by `RTM_NEWADDR`, became the recorded fail.

### What closes it

Items 6 and 8 of this goal (on this branch's base,
`mesh/evaluation-reports-no-verdict-8`) change step 2. `harness.ToolStat`
(`internal/harness/harness.go`) counts, beside `Calls` and `Errors`:

- `Clean` — the calls the server answered **without** an error (a
  Claude tool_result not `is_error`, a Codex `mcp_tool_call`
  `completed`), counted as they happen, never `Calls` less `Errors`;
- `Answered` — the calls the server itself answered **with** an error
  result (MCP `isError`, e.g. `not found` for a slug it does not
  have); each is one of `Errors` too. A call refused before the server
  (approval, permission, transport, not connected) is neither.

`Reached()` is the one line

```go
return s.Clean+s.Answered > 0
```

and `Evaluator.toolBroken` now calls a server broken only when
`s.Errors > 0 && !s.Reached()`. A `not found` answer is `Answered`, so
the server was reached and the verdict is kept; a call refused before
the server still counts as broken. The context line every prompt
carries (`internal/harness/prompt.go`) now names where the slug comes
from: "the document's slug — INDEX.md's `slug` column, never a context
file's name: a file name is not a slug, `adr-adr-001-….md` is slug
`adr-001-…`".

## What landed

Merge times from `gh pr view <n> --repo runsheet/mesh-cli --json
mergedAt`; the first tag carrying each merge from `git tag --contains
<sha> --sort=v:refname`.

| Item | PR | What | Merged (main) | First tag |
|---|---|---|---|---|
| 1 | #428 | an evaluation that reached no verdict is sent as `evaluateWorkItem` `outcome: no_verdict` with its reason; the input is introspected, and a gateway without the field is said once per run (`the platform takes no no-verdict outcome`) and the item waits in evaluating | 2026-09-29 12:46:44Z | v0.2.189 |
| 3 | #432 | the Codex preflight probes its sandbox; `Result.SandboxBroken` (every command refused by bubblewrap) is never a verdict; an unpinned evaluation whose probe fails moves to Claude | 2026-09-29 13:28:08Z | v0.2.190 |
| 8 | #444 | `ToolStat.Clean`, `Answered` and `Reached()`: a context tool that answered `not found` was reached and its verdict kept; rendering tests prove an evaluation session on each family gets a reachable mesh-context | 2026-09-29 14:28:15Z | v0.2.192 |
| 11 | #449 | this document's `prologue: not found` answer — (b) in the narrow sense, not (a) | 2026-09-29 14:56:00Z | v0.2.193 |
| 9 | #450 | Codex evaluation sandboxes get the network (`sandbox_workspace_write.network_access=true`), the probe binds a socket (`mesh sandbox-probe`), a session whose tests all died on `socket:` is `SandboxBroken`, and the mini's `codex-bwrap` AppArmor profile is recorded in ADR-011 | 2026-09-29 15:15:12Z | v0.2.193 |

Items that failed, as `mesh_items` records them (each `evaluation
failed 3 times`):

- **Item 2** (`prologue: not found` with evidence, and the
  both-family test): its third evaluation found `ToolStat.Reached`
  did not hold. Item 6 took its code half (#433, cancelled) and
  item 8 landed it as #444.
- **Item 7** (the `prologue: not found` goal doc): failed on
  consistency, as the Log below says; item 11 rewrote it as #449.
- **Item 10** (mesh-specs: ADR 011 and the TRD): its evaluations
  found the network amendment recorded wrongly and the TRD still
  contradicting the new contract. Item 13 retries it.

### Owed

- **Item 13, mesh-specs: ADR 011 and the TRD record the third
  evaluation outcome.** `mesh_items` at 16:19Z reads it as
  `evaluating` with no pull request. `gh pr list --repo
  runsheet/mesh-specs --state all --search
  evaluation-reports-no-verdict` printed `[]`, but that proves
  nothing: this session's item token cannot see runsheet/mesh-specs
  (`gh repo view runsheet/mesh-specs` gives `Could not resolve to a
  Repository`).
- **The platform half**, runsheet goal
  `evaluation-reports-no-verdict-platform`, is `running`. Its
  third-outcome items are still in review (#1504 and #1532 on
  runsheet/runsheet). **The gateway's `EvaluateWorkItemInput` does not
  have `outcome` today.** The introspection the node uses
  (`takesInputField`), asked at 2026-09-29 16:20:41Z:

  ```sh
  curl -s https://api.runsheet.dev/graphql -H "Authorization: Bearer $TOKEN" \
    -H 'Content-Type: application/json' \
    -d '{"query":"{ __type(name: \"EvaluateWorkItemInput\") { inputFields { name } } }"}' \
    | jq -r '.data.__type.inputFields[].name' | grep -x outcome   # prints nothing
  ```

  The fields are `notes files failedClauses sessionMs costCents passed
  namespace turns usage inputTokens aggregateId evaluatedModel
  outputTokens cacheReadTokens cacheWriteTokens`. The node itself has
  not yet logged `the platform takes no no-verdict outcome` (`grep -a
  '^mesh node: .*takes no no-verdict outcome' ~/.mesh/node.log` finds
  nothing), because since v0.2.191 it has had no no-verdict
  evaluation to send.

## The mini's upgrade

The mini does not run #450 yet. v0.2.193, the release carrying it, was
published at 16:09:40Z, and at 16:20Z the mini still ran v0.2.191. The
release tags come from `gh release list --repo runsheet/mesh` and the
tags on runsheet/mesh-cli. The start lines come from `grep -a
'^mesh node: 2026/09/29 .* mesh node v0\.' ~/.mesh/node.log`, read on
the mini (clock UTC) at 16:16Z. The `^mesh node:` anchor drops session
output that quotes start lines.

| Tag | Commit | Carries | Published | Mini first ran it |
|---|---|---|---|---|
| v0.2.189 | 531ab15 | #428 | 2026-09-29 13:50:40Z | never |
| v0.2.190 | d5dd020 | #428, #432 | 2026-09-29 14:03:23Z | never |
| v0.2.191 | 8eba662 | #428, #432 | 2026-09-29 14:07:50Z | 2026-09-29 14:13:19Z |
| v0.2.192 | cdf0152 | #428, #432, #444 | 2026-09-29 14:49:47Z | never |
| v0.2.193 | e3e3f97 | #428, #432, #444, #449, #450 | 2026-09-29 16:09:40Z | not yet at 16:20:57Z |

From 14:13:19Z the mini switched back and forth between v0.2.191 and
v0.2.188: it started v0.2.188 at 14:14:06Z, 14:19:33Z and 14:24:10Z,
and v0.2.191 at 14:17:04Z, 14:21:24Z and 14:25:00Z. It has run v0.2.191
since 14:25:00Z (pid 794933, `~/.mesh/update.json` confirmed
14:25:31Z, `mesh version` = `mesh v0.2.191 (commit 8eba662)`).

**The sandbox fix on the mini.** ADR-011's amendment from #450 records
it: `/etc/apparmor.d/codex-bwrap` is a profile with `userns,` on the
bubblewrap Codex ships (`~/.local/share/codex/codex-resources/bwrap`,
`flags=(unconfined)`). The profile lets bwrap start. It does not give
the sandbox loopback; the network setting does that. The profile is on
the mini as recorded. The probe was run on the mini at 16:20:57Z with
the installed binary:

```console
$ codex sandbox -c sandbox_mode="workspace-write" -c sandbox_workspace_write.network_access=true -- mesh sandbox-probe
Error: unknown command "sandbox-probe" for "mesh"
Run 'mesh --help' for usage.
$ echo $?
1
```

The installed binary fails because v0.2.191 has no `sandbox-probe`;
#450 adds it. Here is the same probe with a binary built from main
first on `PATH` (`mesh 10a9ba2 (dev build)`, which carries #450), run
with the network on and then off:

```console
$ PATH=/tmp/mesh-probe:$PATH codex sandbox -c sandbox_mode="workspace-write" -c sandbox_workspace_write.network_access=true -- mesh sandbox-probe
127.0.0.1:39519
$ echo $?
0
$ PATH=/tmp/mesh-probe:$PATH codex sandbox -c sandbox_mode="workspace-write" -- mesh sandbox-probe
Error: listen tcp 127.0.0.1:0: socket: operation not permitted
$ echo $?
1
```

With the profile, bwrap starts. With the network on, the sandbox binds
loopback. With it off, the socket error from the record comes back.

## Tainted evaluations before and after

**The after window has not opened, so its measurement is pending.** The
mini has not run v0.2.193, the release carrying #450. This section
gives no after row and no after share, and nothing here is
extrapolated. The goal's evidence is owed until the mini starts
v0.2.193 and failed evaluations accumulate after that start.

- **Before-window cutoff:** the mini's first start of v0.2.193 (the
  first `mesh node v0.2.193` start line in node.log). That start has
  not happened. At the read, the before window therefore runs to the
  read's end, **2026-09-29T16:19:33Z**.
- **After-window cutoff:** the same instant. It opens at that start and
  runs to the time of the read that measures it. Today it is empty by
  definition, and it is not reported as zero.
- **Read time:** `mesh_items` for every goal `mesh_goals` lists in
  runsheet/mesh and runsheet/runsheet (199 goals), read from
  2026-09-29T16:16:52Z to 2026-09-29T16:19:33Z. The latest evaluation
  in the read is at 16:16:45Z.

**The classification is the research note's, applied unchanged as the
brief states it.** The note's unit is the item whose last word is
`evaluation failed 3 times` (its 59). Each item is judged by its
recorded evaluation text, here the last evaluation `mesh_items` holds
for it. The item is **tainted** when that text says the evaluator
could not run or reach its tools: `no verdict —`, `bwrap:`, `could not
run` or `unverifiable`. It is also tainted when it carries `VERDICT:
PASS` under the fail. Otherwise it is **substantive**, a real defect
found. The items the platform failed before it recorded evaluation text
(2026-09-23 to 09-25) cannot be classified, and they are counted apart.

| Window | Failed evaluations (items, `evaluation failed 3 times`) | Tainted | Substantive | No evaluation text | Tainted / failed with text |
|---|---|---|---|---|---|
| Before (to 16:19:33Z) | 87 | 57 | 11 | 19 | 57/68 = 84% |
| After (from the mini's first v0.2.193 start) | pending | pending | pending | pending | pending |

Checked: 57 + 11 + 19 = 87 and 57 + 11 = 68. The 57 tainted items, by
the first rule each matched:

| Rule | Items |
|---|---|
| `no verdict —` | 7 |
| `bwrap:` | 25 |
| `VERDICT: PASS` | 0 |
| `could not run` / `unverifiable` | 25 |
| Total | 57 |

**Why the before share is 84% and not the note's 90%.** The note itself
cannot be read from this session. `gh api
repos/runsheet/handbook/contents/research/2026-09-29-evaluations-fail-on-plumbing-not-quality.md`
answers `Not Found` on the item token, and `mesh_search` and
`mesh_document` do not index it. Its 53/59 therefore cannot be
re-derived line by line, and two differences remain:

- The record has grown since the note was written: 68 items carry
  evaluation text now, against the note's 59.
- The rule here is the note's rule applied by keyword, where the note
  read each text. Judged by keyword, the 59 earliest of the 68 by last
  evaluation (through 2026-09-29T13:44:05Z) are 49 tainted and 10
  substantive, 83%. So the keyword rule counts fewer items tainted than
  the note's reading did. It misses an evaluator who could not run the
  tests but words it another way. In the direction that matters, 84% is
  the conservative baseline: an after share below it cannot be an
  artefact of the rule.

**No-verdict outcomes in the after window.** These are items whose
evaluations carry `no verdict —`, whose status is not failed, and whose
attempts did not grow past that evaluation. A correctly reported no
verdict is the goal's success case, not a taint.

| Window | No-verdict outcomes |
|---|---|
| After (from the mini's first v0.2.193 start) | pending: the window has not opened |

The same query over the before window finds 8 items. Each is
`Codex (first attempt, gpt-6-sol): no verdict — prologue: not found`,
followed by the other family's verdict in the same evaluation. That is
the fall-over, not a no-verdict outcome sent to the platform. The
gateway takes no `outcome` yet (What landed, Owed).

The query. `fetch.sh` reads every goal's items through mesh-context's
`mesh_items`, exactly as a session calls it, on the session's
mesh-context bearer. `classify.jq` classifies the failed items, and
`noverdict.jq` finds the no-verdict outcomes. `C` is the cutoff. Until
the mini starts v0.2.193, it is the read's end.

```sh
# fetch.sh — one JSON line per goal: {project, goal, items: [...]}
U=https://api.runsheet.dev/mcp
call(){ curl -s $U -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"$1\",\"arguments\":$2}}" \
  | sed -n 's/^data: //p;/^{/p' | jq -r '.result.content[0].text'; }
date -u +%FT%TZ > read-start
for p in runsheet/mesh runsheet/runsheet; do
  call mesh_goals "{\"project\":\"$p\"}" | jq -r '.goals[].slug' | while read g; do
    call mesh_items "{\"project\":\"$p\",\"goal\":\"$g\"}" | jq -c --arg p "$p" '{project:$p}+.'
  done
done > items.jsonl
date -u +%FT%TZ > read-end
```

```jq
# classify.jq — one row per item whose last word is `evaluation failed 3 times`,
# judged by its last recorded evaluation (the research note's unit and rules)
def why: if   test("no verdict —") then "no verdict —"
         elif test("bwrap:") then "bwrap:"
         elif test("VERDICT:\\s*PASS") then "VERDICT: PASS"
         elif test("could not run|unverifiable"; "i") then "could not run / unverifiable"
         else "" end;
.project as $p | .goal.slug as $g | (.items // [])[] |
  select(.status == "failed" and ((.last_error // "") | startswith("evaluation failed 3 times"))) |
  if (.evaluations | length) == 0
  then { project: $p, item: "\($g)/\(.seq)", at: .heartbeat_at, class: "no evaluation text" }
  else (.evaluations | max_by(.at)) as $e |
       { project: $p, item: "\($g)/\(.seq)", at: $e.at, why: ($e.notes | why) }
       | .class = (if .why != "" then "tainted" else "substantive" end)
  end
```

```jq
# noverdict.jq — a no-verdict outcome: an evaluation carrying `no verdict —` on an
# item that is not failed and whose attempts did not grow past that evaluation's
.goal.slug as $g | (.items // [])[] | . as $it | (.evaluations // [])[] |
  select((.notes | test("no verdict —")) and $it.status != "failed" and .attempt == $it.attempts) |
  { item: "\($g)/\($it.seq)", status: $it.status, attempts: $it.attempts, at }
```

```sh
jq -c -f classify.jq items.jsonl > failed.jsonl
# C: the first v0.2.193 start on the mini, once there is one; until then the read's end
C=$(grep -a -m1 '^mesh node: .* mesh node v0\.2\.193,' ~/.mesh/node.log \
      | awk '{gsub("/","-",$3); print $3 "T" $4 "Z"}')
C=${C:-$(cat read-end)}
jq -s --arg c "$C" 'def w: {failed: length,
    tainted: map(select(.class=="tainted"))|length,
    substantive: map(select(.class=="substantive"))|length,
    no_text: map(select(.class=="no evaluation text"))|length};
  {before: (map(select(.at < $c))|w), after: (map(select(.at >= $c))|w)}' failed.jsonl
jq -s -r 'map(select(.class=="tainted"))|group_by(.why)|map("\(.[0].why) \(length)")[]' failed.jsonl
jq -c -f noverdict.jq items.jsonl | jq -s --arg c "$C" \
  '{before: map(select(.at < $c))|length, after: map(select(.at >= $c))}'
```

## Log

- 2026-09-29 · item 7: first answer — (b) in the narrow sense, not
  (a) — with the items through `mesh_items`, the merges through `gh
  pr view` and a kept-stream count; three evaluations failed it on
  consistency (a count against no stated cutoff, the #258 chronology,
  totals that did not add up, `Clean` described as errored calls).
- 2026-09-29 · item 11: rewrote from item 7's last revision.
  Re-verified the 13 items through `mesh_items` at 14:17Z and the
  merges through `gh pr view`; added 4d8d435 from node.log to the
  mini's versions; recounted the kept streams once at the 14:00:00Z
  cutoff into one table (320 sessions, 1,330 completed, 425 answered
  with an error — 403 `not found`, 22 other — and 5 real errors); and
  described `Clean`, `Answered` and `Reached()` as `harness.go` has
  them.
- 2026-09-29 · item 11, second attempt: the answer, the tables and the
  counts are unchanged. Item 8's work has since landed on main as one
  squashed merge, so the branch merged `origin/main` and `git diff
  origin/main --stat` names this document alone; the `Reached()` line
  quoted above is re-checked against `harness.go` on main.
- 2026-09-29 · item 14: filled What landed, The mini's upgrade and
  Tainted evaluations before and after. The placeholders named item
  12; item 12 is still `pending`, and item 14 does its work. The
  `prologue: not found` section is unchanged. One fact in it is
  superseded: the line that the mini "is moving to v0.2.191" is now
  settled, since the mini has run v0.2.191 since 14:25:00Z (The mini's
  upgrade).
  The after window is empty because v0.2.193, carrying #450, is tagged
  but not published, so the goal's evidence is still owed. `status:`
  stays `draft` for two reasons: item 13, the mesh-specs amendment, is
  `evaluating` with no merged pull request, and the platform half has
  not landed `outcome`.
- 2026-09-29 · item 14, second attempt: the evaluator failed the first
  attempt on four points. It reported an after row with no cutoff
  instant, and its command set the cutoff to the v0.2.191 start while
  calling it v0.2.193. It gave no command for the no-verdict table. It
  used a per-evaluation keyword count in place of the research note's
  classification. This attempt re-read all 199 goals through
  `mesh_items` (16:16:52Z to 16:19:33Z) and applies the note's unit,
  the item whose last word is `evaluation failed 3 times`. It reports
  the after window as pending, because the mini has not started
  v0.2.193. The command reads the cutoff from node.log's first v0.2.193
  start line, and it falls back to the read's end until that line
  exists. It adds `noverdict.jq`. Superseded by this entry: the first
  attempt's line that v0.2.193 is "tagged but not published". It was
  published at 16:09:40Z. The sandbox probe was re-run on the mini at
  16:20:57Z, and the gateway was re-introspected at 16:20:41Z, still
  without `outcome`. `status:` stays `draft` for two reasons: item 13 is
  still `evaluating` with no merged mesh-specs pull request, and the
  after window has not opened.
