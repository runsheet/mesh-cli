---
title: Goal — The node says no verdict when its tools failed, and the two tooling failures stop happening
kind: plan
status: draft
parent: prd
summary: "Tainted evaluations were 246 of 564 failed evaluations (43.6%) before (read 2026-09-29T16:53:03Z) and 3 of 135 (2.2%) in after window A, from the mini's first v0.2.193-or-later start (v0.2.208, 2026-09-30T00:00:05Z) to 2026-10-02T04:23:02Z; no no-verdict outcome was recorded, because the node sent `outcome`, which the platform's EvaluateWorkItemInput never took (it takes `noVerdict`/`noVerdictReason`, runsheet ADR 035) — item 17 (#545, squash 640a8eb, merged 2026-10-02T04:33:31Z, first in v0.2.213) fixes it, and window B is owed until the mini starts v0.2.213 or later (it ran v0.2.210 at 04:37:17Z). `prologue: not found` is (b) in the narrow sense, not (a): #305's tool-failure rule read the context tool's own `not found` answer as an unreachable tool."
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

Five squashed merges on runsheet/mesh-cli `main`, times from `git log
origin/main --format='%h %cI %s'` (each merge's committer date, UTC),
and the release each first appears in (`git merge-base --is-ancestor
<sha> <tag>`):

| PR | Squash commit | Merged (main, UTC) | First release | What |
|---|---|---|---|---|
| #428 | 11aa560 | 2026-09-29 12:46:44Z | v0.2.189 | An evaluation that reached no verdict is recorded as no verdict with its reason (`evaluateWorkItem` `outcome: no_verdict`), never a fail; a pass in the record is never sent as a fail |
| #432 | d5dd020 | 2026-09-29 13:28:08Z | v0.2.190 | A Codex session whose bubblewrap sandbox cannot start is never a verdict: probed before the session, caught in the stream (`Result.SandboxBroken`), moved to Claude |
| #444 | cdf0152 | 2026-09-29 14:28:15Z | v0.2.192 | A context tool that answered with its own error was reached (`ToolStat.Clean`, `Answered`, `Reached()`): `prologue: not found` keeps its verdict; each family's evaluation session proved to get a reachable mesh-context |
| #449 | ebb1948 | 2026-09-29 14:56:00Z | v0.2.193 | This document's `prologue: not found` answer (docs only) |
| #450 | e3e3f97 | 2026-09-29 15:15:12Z | v0.2.193 | A Codex evaluation's sandbox lets its tests bind loopback (`sandbox_workspace_write.network_access=true`), the probe binds a socket (`mesh sandbox-probe`), the mini's AppArmor profile recorded |
| mesh-specs #108 | — | — | — | ADR 011 and the TRD record the third evaluation outcome, the Codex sandbox probe, network-on evaluation sandboxes and the reached-tool rule |
| #545 (item 17) | 640a8eb | 2026-10-02 04:33:31Z | v0.2.213 | The no-verdict outcome reaches the platform as runsheet ADR 035 defines it: `evaluateWorkItem` sends `passed: false`, `noVerdict: true`, `noVerdictReason`, probed on `noVerdict` — never `outcome`, which the gateway never took |
| mesh-specs, item 18 | — (open: no pull request yet; the item is `running` at 2026-10-02T04:37Z) | — | — | ADR 011 and the TRD record the third outcome in the platform's shape (`noVerdict`, `noVerdictReason`, `passed: false`) |

## The mini's upgrade

From the mini's `~/.mesh/node.log` start lines (the host's clock is
UTC; `grep -a '^mesh node: 2026/09/29 [0-9:]* mesh node [v0-9a-f.]*,
pid' ~/.mesh/node.log`), read 2026-09-29 16:52:20Z, and
`~/.mesh/update.json`:

| Release | Tagged commit | First start on 2026-09-29 (UTC) | Other starts that day | Carries e3e3f97 (#450) |
|---|---|---|---|---|
| v0.2.188 | 5a8c29e | 00:51:59Z | 04:00:32Z; 14:14:06Z, 14:19:33Z, 14:24:10Z | no |
| v0.2.191 | 8eba662 | 14:13:19Z | 14:17:04Z, 14:21:24Z, 14:25:00Z (confirmed 14:25:31Z, `update.json` `confirmedAt`) | no |

Between 14:13Z and 14:25Z the node alternated between the two
(`confirming update v0.2.191 → v0.2.188` and back) before v0.2.191
confirmed; it has run v0.2.191 since. **Neither release the mini
started on 2026-09-29 carries e3e3f97.** The first release that does
is v0.2.193 (tagged on e3e3f97 itself); v0.2.194–v0.2.197 carry it
too. `mesh_health_nodes` at 2026-09-29T16:52:11Z gives the mini
`version: v0.2.191`, `desired_version: v0.2.191`, 7 commits behind
main, the oldest unrun merge 14:28:15Z (#444): self-update follows the
desired version, so the mini will not start v0.2.193 or later until
its desired version is raised. node.log has no `mesh node v0.2.193`
(or later) start line.

**The sandbox fix on the host**, as ADR 011's amendment "an
evaluation's sandbox has the network, and the probe binds a socket"
records it: since 12:59Z on 2026-09-29 the mini has
`/etc/apparmor.d/codex-bwrap`, a profile with `userns,` on the
bubblewrap Codex ships (`~/.local/share/codex/codex-resources/bwrap`,
`flags=(unconfined)`; README, The sandbox on Ubuntu). With it bwrap
starts — kept streams from 13:13Z show `go vet` and `go build` exiting
0 — and `bwrap: loopback: Failed RTM_NEWADDR` stops: the latest failed
evaluation carrying `bwrap` in the before read below is at 13:55Z. It
does not give the sandbox loopback: `go test ./...` still dies with
`socket: operation not permitted` because workspace-write has the
network off. That half is #450's `network_access=true`, which reaches
the mini only with v0.2.193 or later — and the loopback-socket notes
in the before read run on to 16:44:00Z.

**The first start of v0.2.193 or later** (read 2026-10-02T04:22Z,
`grep -a '^mesh node: 2026/[0-9/]* [0-9:]* mesh node v[0-9.]*, pid'
~/.mesh/node.log.1 ~/.mesh/node.log`, the first line whose version is
v0.2.193 or later; the host's clock is UTC):

```
mesh node: 2026/09/30 00:00:05 mesh node v0.2.208, pid 1945297, config /home/chris/.mesh/node.yaml
mesh node: 2026/09/30 00:00:05 confirming update v0.2.191 → v0.2.208: start 1 of 3, rolls back on the third
```

So the mini first started a release carrying e3e3f97 (#450) at
**2026-09-30T00:00:05Z**, on **v0.2.208** (38f854d); it went straight
from v0.2.191 to v0.2.208 and never started v0.2.193–v0.2.207. Every
start since is v0.2.208 or later (v0.2.209 first at 01:41:23Z on
2026-09-30, alternating with v0.2.208 until 02:41:17Z and alone from
then, v0.2.210 from 2026-10-01 14:23:52Z, alternating with
v0.2.211 between 03:31Z and 03:46Z on 2026-10-02 before settling on
v0.2.210), and each of those carries e3e3f97. **The version at the
read:** `mesh_health_nodes` at 2026-10-02T04:22:34Z gives the mini
`version: v0.2.210`, `desired_version: v0.2.210`, 1 commit behind
main, latest release v0.2.211; `~/.mesh/update.json` has it confirmed
at 2026-10-02T03:49:08Z.

## Tainted evaluations before and after

### The rules

A **failed evaluation** is one entry of an item's `evaluations`
list as `mesh_items` returns it — "every failed evaluation so far",
on every item of every goal, whatever the item's status now (merged,
failed, cancelled, ready, review, …). Each entry's `notes` is
classified by the goal statement's three classes (the research note,
runsheet/handbook `research/2026-09-29-evaluations-fail-on-plumbing-not-quality.md`,
could not be fetched from this session: `mesh_document` answers `not
found` and `gh` cannot see runsheet/handbook), by literal,
case-insensitive substring match, the first class that matches:

1. **Tooling — the evaluator could not run or reach its tools.** The
   notes contain any of `bwrap`, `RTM_NEWADDR` (the sandbox would not
   start), `no verdict —` (the node's fallback note: the first family
   saw no context tool, and the job ran again on the other family),
   `loopback`, `socket: operation not permitted`, `local socket`,
   `listening port`, `listening socket`, `test-server socket` (the
   sandbox refused the tests their sockets). A note that also names a
   real defect still counts here: the acceptance's commands could not
   run, so the verdict was not reached on them.
2. **Carries `VERDICT: PASS` yet recorded as fail.** Not tooling, and
   the notes contain `VERDICT: PASS`.
3. **Substantive.** Everything else: an evaluator's review finding a
   defect, and — shown in its own row — a CI check recorded as a
   failed evaluation (notes starting `The check "`).

Tainted is classes 1 and 2. The match is lexical: a note that says it
could not reach Postgres, or GitHub, in words none of the strings
above catches is counted substantive, so the tainted count is a floor.

**No verdict, two different things.** A *fallback note* (`no verdict —
<tool>: <error>` at the head of the notes, followed by the other
family's real `VERDICT: FAIL`) is a failed evaluation and is counted in
class 1. A *recorded no_verdict outcome* (#428: `evaluateWorkItem`
`outcome: no_verdict`) is not a failed evaluation at all — the
platform spends no attempt on it and `mesh_items` does not list it — so
it is counted apart, from node.log's `evaluation reached no verdict on
<family>, recorded as no verdict` lines.

### Before

Read 2026-09-29T16:53:03Z: 7 projects, 216 goals, 1,653 items, 564
failed evaluations (the earliest 2026-09-26T03:31:48Z, the latest
2026-09-29T16:50:59Z). Every one precedes the mini's first start of a
release carrying e3e3f97, which has not happened.

| Before, read 2026-09-29T16:53:03Z | Failed evaluations |
|---|---|
| Tooling: `bwrap` / `RTM_NEWADDR` | 197 |
| Tooling: `no verdict —` fallback note, no `bwrap` (31 carry the note in all; 22 of them also carry `bwrap` and are in the row above) | 9 |
| Tooling: a loopback / socket string, neither of the above | 38 |
| `VERDICT: PASS` recorded as fail | 2 |
| **Tainted** | **246** |
| Substantive: an evaluator's review | 216 |
| Substantive: a CI check (`The check "…" failed`) | 102 |
| **Substantive** | **318** |
| **Total failed evaluations** | **564** |
| **Tainted share** (246 / 564) | **43.6%** |
| Recorded `no_verdict` outcomes (node.log, not failed evaluations) | 0 |

246 + 318 = 564. The research note's 90% is a different unit — the 59
items whose last word was `evaluation failed 3 times`, by their
recorded text — so 43.6% over every failed evaluation on every item is
the baseline this goal's after is measured against, not a move from 90%.
No recorded `no_verdict` outcome exists yet: the mini's v0.2.191
carries #428, but node.log has no `recorded as no verdict` line through
the read.

To reproduce, on the mini (`~/.mesh/token` is the node's credential;
`mesh_items` and `mesh_goals` are mesh-context's tools, called over the
gateway's `/mcp`):

```sh
# tool: mesh-context mesh_goals + mesh_items; read time 2026-09-29T16:53:03Z
mkdir -p /tmp/eval && cd /tmp/eval
call() { curl -s https://api.runsheet.dev/mcp \
  -H "Authorization: Bearer $(tr -d '\n' < ~/.mesh/token)" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"$1\",\"arguments\":$2}}" \
  | jq -r '.result.content[0].text'; }
for p in foghornid/foghorn latchkeyid/latchkey purserid/purser runsheet/loom \
         runsheet/mesh runsheet/runsheet wardroomid/wardroom; do
  f=$(echo $p | tr / _); call mesh_goals "{\"project\":\"$p\"}" > goals_$f.json
  for g in $(jq -r '.goals[].slug' goals_$f.json); do
    call mesh_items "{\"project\":\"$p\",\"goal\":\"$g\"}" > items_${f}__$g.json
  done
done
for f in items_*.json; do jq -r --arg p "$f" '.items[] as $i | $i.evaluations[]?
  | [$p, ($i.seq|tostring), $i.status, .at, .attempt, (.notes|gsub("[\n\t]";" "))]
  | @tsv' "$f"; done > evals.tsv
python3 - <<'EOF'
from datetime import datetime, timezone
CUTOFF = datetime(2026, 9, 29, 16, 53, 3, tzinfo=timezone.utc)   # the read time
def at(s): return datetime.fromisoformat(s.replace('Z', '+00:00'))
rows = [l.rstrip('\n').split('\t') for l in open('evals.tsv')]
rows = [r for r in rows if at(r[3]) <= CUTOFF]                   # evaluations at or before the read
classes = [('tooling', ['bwrap', 'rtm_newaddr', 'no verdict —', 'loopback',
                        'socket: operation not permitted', 'local socket',
                        'listening port', 'listening socket', 'test-server socket']),
           ('pass', ['verdict: pass'])]
n = {}
for r in rows:
    notes = r[5].lower()
    c = next((k for k, ss in classes if any(s in notes for s in ss)),
             'ci' if r[5].startswith('The check "') else 'substantive')
    n[c] = n.get(c, 0) + 1
t = n['tooling'] + n['pass']
print(n, 'total', len(rows), 'tainted', t, 'share %.1f%%' % (100 * t / len(rows)))
EOF
# recorded no_verdict outcomes, apart, through the same read time (node.log is UTC on the mini):
grep -a '^mesh node: 2026/09/2[0-9] [0-9:]* item [^ ]*: evaluation reached no verdict on' ~/.mesh/node.log \
  | awk '$3" "$4 <= "2026/09/29 16:53:03"' | wc -l
```

Both counts apply the cutoff in the steps themselves: the Python keeps
only evaluations whose `at` is at or before 2026-09-29T16:53:03Z, and
the `awk` keeps only node.log lines at or before the same second, so a
later read gives this row back unchanged — 564, 246, 318, 43.6%, and 0.

### After

The after windows open only at a start of the release they measure,
never before it.

#### Window A — from the mini's first start of a release carrying #450

Window A opens at **2026-09-30T00:00:05Z**, the mini's first start of
v0.2.193 or later (v0.2.208, above), and closes at the read,
**2026-10-02T04:23:02Z**: the same rules, the same three classes, the
same fetch, Python and awk as the before block, over every failed
evaluation whose `at` is at or after the start and at or before the
read. That read gave 7 projects, 233 goals and 794 failed evaluations
in all; 135 fall in the window (the earliest 2026-09-30T00:03:47Z, the
latest 2026-10-02T04:22:54Z). The same fetch with the before block's
cutoff alone gives 564, 246, 318, 43.6% back unchanged.

| After, window A: 2026-09-30T00:00:05Z – 2026-10-02T04:23:02Z | Failed evaluations |
|---|---|
| Tooling: `bwrap` / `RTM_NEWADDR` | 1 |
| Tooling: `no verdict —` fallback note, no `bwrap` (0 carry the note in all) | 0 |
| Tooling: a loopback / socket string, neither of the above | 0 |
| `VERDICT: PASS` recorded as fail | 2 |
| **Tainted** | **3** |
| Substantive: an evaluator's review | 127 |
| Substantive: a CI check (`The check "…" failed`) | 5 |
| **Substantive** | **132** |
| **Total failed evaluations** | **135** |
| **Tainted share** (3 / 135) | **2.2%** |
| Recorded `no_verdict` outcomes (node.log, not failed evaluations) | 0 |

1 + 0 + 0 + 2 = 3 tainted; 127 + 5 = 132 substantive; 3 + 132 = 135.
The share moved from 43.6% to 2.2%: no `RTM_NEWADDR`, no loopback or
socket refusal and no `no verdict —` fallback note in the window. The
one `bwrap` row is the lexical rule at work, not a sandbox that would
not start — it is item 17 of this goal (attempt 1, 2026-10-02T03:54:16Z),
a review that ran the suite and failed the item because the
sandbox-start (`bwrap`) case lacked a no-verdict test; it is kept in
class 1 so the rule stays the before block's. The two `VERDICT: PASS`
rows are a Codex first evaluation that passed and a Claude second
opinion that failed (`rigger-sets-up-a-business` item 19 at
2026-09-30T04:20:16Z, and this goal's item 17 at
2026-10-02T04:02:43Z): both families' notes are recorded, the pass
first.

To reproduce, on the mini: fetch `evals.tsv` exactly as the before
block does (any later read), then apply both cutoffs in their own
steps:

```sh
# window A: start 2026-09-30T00:00:05Z (first v0.2.193+ start), read 2026-10-02T04:23:02Z
cd /tmp/eval   # evals.tsv from the before block's fetch
python3 - <<'EOF'
from datetime import datetime, timezone
START  = datetime(2026, 9, 30, 0, 0, 5, tzinfo=timezone.utc)     # the mini's first v0.2.193+ start
CUTOFF = datetime(2026, 10, 2, 4, 23, 2, tzinfo=timezone.utc)    # the read time
def at(s): return datetime.fromisoformat(s.replace('Z', '+00:00'))
rows = [l.rstrip('\n').split('\t') for l in open('evals.tsv')]
rows = [r for r in rows if at(r[3]) >= START]                    # at or after the window's start
rows = [r for r in rows if at(r[3]) <= CUTOFF]                   # at or before the read
classes = [('tooling', ['bwrap', 'rtm_newaddr', 'no verdict —', 'loopback',
                        'socket: operation not permitted', 'local socket',
                        'listening port', 'listening socket', 'test-server socket']),
           ('pass', ['verdict: pass'])]
n = {}
for r in rows:
    notes = r[5].lower()
    c = next((k for k, ss in classes if any(s in notes for s in ss)),
             'ci' if r[5].startswith('The check "') else 'substantive')
    n[c] = n.get(c, 0) + 1
t = n.get('tooling', 0) + n.get('pass', 0)
print(n, 'total', len(rows), 'tainted', t, 'share %.1f%%' % (100 * t / len(rows)))
EOF
# node.log over the same instants (UTC on the mini; the window may have rotated into node.log.N):
grep -a -h '^mesh node: 2026/[0-9/]* [0-9:]* item [^ ]*: evaluation reached no verdict on' ~/.mesh/node.log* \
  | awk '$3" "$4 >= "2026/09/30 00:00:05"' | awk '$3" "$4 <= "2026/10/02 04:23:02"' | wc -l      # 0
grep -a -h '^mesh node: 2026/[0-9/]* [0-9:]* the platform takes no no-verdict outcome' ~/.mesh/node.log* \
  | awk '$3" "$4 >= "2026/09/30 00:00:05"' | awk '$3" "$4 <= "2026/10/02 04:23:02"' | wc -l      # 1
```

It prints `{'substantive': 127, 'tooling': 1, 'pass': 2, 'ci': 5} total
135 tainted 3 share 2.2%`, then 0 and 1.

#### Recorded no-verdict outcomes — why the count stayed 0

In window A node.log has **0** `evaluation reached no verdict on`
lines and **1** `the platform takes no no-verdict outcome` line —
`mesh node: 2026/09/30 07:09:29 the platform takes no no-verdict
outcome; a no-verdict evaluation waits in evaluating (said once per
run)`. That is the finding item 17 fixes. From v0.2.189 (#428, 11aa560)
the node sent the outcome as `evaluateWorkItem` `outcome: no_verdict`
and probed the gateway's `EvaluateWorkItemInput` for `outcome`; the
platform never took `outcome` — its input takes `noVerdict` and
`noVerdictReason` beside `passed: false` (runsheet ADR 035). So the
probe found no field, the node said so once per run, and every
evaluation that reached no verdict waited in evaluating instead of
being recorded: `outcome` was never accepted, and the recorded
no-verdict count is 0 until the mini runs a release carrying item 17.
Item 17 is runsheet/mesh-cli **#545**, which sends `passed: false`,
`noVerdict: true` and `noVerdictReason`, probed on `noVerdict`. It
merged after window A's read, as squash commit **640a8eb** at
2026-10-02T04:33:31Z (`git log origin/main --format='%h %cI %s' | grep
-i noVerdict`), and the first release carrying it is **v0.2.213**
(`git tag --contains 640a8eb | sort -V | head -1`; tagged on 640a8eb
itself, its release build still running at 04:37Z). No release the
mini has started carries it, so every no-verdict before the mini's
first v0.2.213-or-later start waited in evaluating and none was
recorded.

#### Window B — Owed

**Owed.** At the read, 2026-10-02T04:37:17Z (`mesh_health_nodes`),
the mini runs **v0.2.210** with desired version **v0.2.210** (3
commits behind main; the platform's latest release v0.2.211, since
v0.2.212 and v0.2.213 were still building). The first release carrying
item 17's 640a8eb is **v0.2.213**, and node.log has no start line of it
or any later release (`grep -a -h '^mesh node: 2026/[0-9/]* [0-9:]*
mesh node v0\.2\.2\(1[3-9]\|[2-9][0-9]\), pid' ~/.mesh/node.log*`
prints nothing; the latest start is `mesh node: 2026/10/02 03:46:27
mesh node v0.2.210, pid 3563663`). Window B opens at the mini's first
node.log line `mesh node: <UTC date time> mesh node v0.2.213, pid …`
— or of any later release, whichever comes first — and closes at a
stated read. Its row is the table above over the failed evaluations
from that start to the read, with the recorded `no_verdict` outcomes
(node.log's `evaluation reached no verdict on` lines over the same
instants) in their own row, and the status goes to `approved` with it.
Self-update follows the desired version, so that start waits on the
mini's desired version reaching v0.2.213 or later.

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
- 2026-09-29 · item 16: filled `What landed` (#428, #432, #444, #449,
  #450 from `git log` on main, mesh-specs #108 by number), `The mini's
  upgrade` (v0.2.188 first started 00:51:59Z, v0.2.191 14:13:19Z,
  neither carrying e3e3f97; the `codex-bwrap` profile as ADR 011
  records it) and the before row: 564 failed evaluations on every goal
  of every project through `mesh_items` at 16:53:03Z, 246 tainted,
  318 substantive, 43.6%, no recorded `no_verdict` outcome yet. The
  after share is owed: the mini runs v0.2.191 and has not started
  v0.2.193, the first release carrying #450. Item 14's three failures
  designed out: the branch is this file alone against its merge-base,
  no after row is written over a window before that start, and the
  before count covers items that later merged.
- 2026-09-29 · item 16, second attempt: the evaluator found the
  reproducing block had no cutoff, so a later read counted two later
  evaluations (566, 43.5%). The block now applies the UTC read time
  2026-09-29T16:53:03Z in its own steps — the Python drops every
  evaluation whose `at` is later, and the node.log `no_verdict` count
  goes through `awk` on the same second — so it gives 564, 246, 318,
  43.6% and 0 back at any later read. The mini re-read at 17:08:02Z
  still runs v0.2.191; the after share stays owed.
- 2026-10-02 · item 19: the mini first started a release carrying
  #450 at 2026-09-30T00:00:05Z (v0.2.208; it never ran v0.2.193–207),
  and runs v0.2.210 (desired v0.2.210) at 04:22:34Z. Window A, from
  that start to the read 2026-10-02T04:23:02Z, by the before block's
  rules: 135 failed evaluations, 3 tainted (1 lexical `bwrap`, 2
  `VERDICT: PASS`), 132 substantive, 2.2% against the before 43.6%,
  which the same fetch gives back unchanged. No no-verdict outcome was
  recorded: the node sent `outcome`, which the platform never took
  (it takes `noVerdict`/`noVerdictReason`, ADR 035), and node.log says
  so once at 2026-09-30 07:09:29. Item 17 (#545) fixes it and is open,
  so window B is owed and the status stays `draft`. `What landed` has
  #545 and mesh-specs item 18 (no pull request yet). The branch is
  this file alone against main: item 17's commits are on #545, not
  here.
- 2026-10-02 · item 19, second attempt: #545 merged at 04:33:31Z,
  after the first attempt's read, as 640a8eb, first released in
  v0.2.213; the branch merged `origin/main`, and the summary, `What
  landed`, the no-verdict paragraph and the Owed paragraph now name
  640a8eb and v0.2.213. Re-read at 04:37:17Z: the mini runs v0.2.210
  (desired v0.2.210) and node.log has no v0.2.213-or-later start, so
  window B stays owed and the status `draft`. Corrected the upgrade
  section: v0.2.209 first started at 01:41:23Z, alternating with
  v0.2.208 until 02:41:17Z. Window A and the before row unchanged.
