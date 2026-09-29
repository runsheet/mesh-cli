---
title: Goal — A session's context has a budget that actually holds, and the node reports what it cost
kind: plan
status: draft
parent: prd
summary: The node's reading (mesh node usage over the mini's node.log, filtered to runsheet/mesh's goals, 2026-09-22..2026-09-28) puts the before median at 1,221,759 input tokens per build session over 48 paired build sessions of 359 finished; the after reading is not yet due (2026-10-07), and the platform's mesh-bench.py reading is the operator's and is owed.
---

# Goal — A session's context has a budget that actually holds, and the node reports what it cost

Mesh goal `context-budget-that-holds`, running.

## Statement

A day of Mesh on 2026-09-28: 500 sessions, 1.16 billion input tokens against 7.3 million output — 160 to 1 — with 97% of the input cache reads. The plan windows are the real currency on a subscription, and one account is already at 83% of its seven-day window with work left unclaimed for lack of headroom. Almost none of that is the model thinking. It is the same context, re-read every turn of every session.

`internal/node/workspace.go` has the budget, and the budget does not hold:

- `ContextBytesMax = 240 * 1024` and `DocBytesMax = 60 * 1024` are **compile-time constants**. An operator cannot trim them without a release, so the one lever that touches every session kind at once cannot be pulled.
- **Instructions and ADRs are exempt from the budget entirely** — `binding := d.Kind == "instructions" || d.Kind == "adr"`, and a binding document is written to disk whatever its size and whatever the running total. So the real ceiling is 240KB *plus every ADR and every AGENTS.md in the project, without limit*. This grows every time the estate writes an ADR, which it does most days, and it grows for every session of every kind. That is the leak.
- `RenderIndex` writes `INDEX.md` listing **every** indexed document of the project with kind, title, summary, slug and path. It is a table that grows linearly with the estate and is injected whole, in every session.

Make the budget hold:

- **Configurable.** `context:` in node.yaml, read again each tick the way capacity and the accounts are: `bytes_max` and `doc_bytes_max`, the current values as defaults so nothing changes on upgrade.
- **Bound the binding set.** Instructions and ADRs stay exempt from `doc_bytes_max` — they are binding and truncating one is worse than dropping it — but they must not be exempt from the *total*. Prefer the ones the item touches: the AGENTS.md and ADRs of the repos in the item's own work, then project-wide ones, then the rest by recency until the total is reached. What does not fit is listed in the index and fetched on demand with `mesh_document`, which already works.
- **Trim the index.** One row per document is fine; the summary is what makes it heavy. Keep the summary for the kinds a session is likely to fetch and drop it for the rest, or cap the table and say how many were omitted.
- **Say what it cost.** Log the bytes written and the count dropped, per session, and put the bytes on the usage record so the benchmark can see the effect rather than infer it.

The measurement matters as much as the change: `mesh-bench.py` reports cost per merged item and input tokens per session. Take a reading before and after on the same repositories, and put both in the goal doc. If input tokens per build session do not fall, the change was wrong and should be reverted rather than kept for tidiness.

## Acceptance

`go vet ./...` clean, `gofmt -l .` empty, `go build ./cmd/mesh/` and the linux/amd64 cross-build fine, `go test ./...` green.

Config: `context: {bytes_max, doc_bytes_max}` loads from node.yaml, is re-read by `Reload` without a restart (a test proves the new value is in force on the next tick), and its absence keeps today's 240KB and 60KB exactly.

Budget: a test with more binding documents than the total budget proves the total is honoured — the item's own repos' instructions and ADRs are on disk, the rest are in the index and absent from disk — and that no binding document is ever truncated mid-file. A project whose binding documents alone exceed the budget still produces a usable context: the item's own repo wins.

Index: `INDEX.md` renders every document as a row, and the assertion for the trimmed summaries is exact (the tests here assert whole lines, per AGENTS.md).

Reporting: the node logs one line per session with the bytes written and the number of documents left to fetch, and the bytes ride the usage record.

Evidence in the goal doc: `mesh-bench.py` output from before the change and after it, on the same project, with median input tokens per build session in both. Name the number that moved.

## Items

As `mesh_items` (project `runsheet/mesh`, goal `context-budget-that-holds`) records them at 2026-09-29T14:56Z.

| Seq | Title | Status | PR | Cost |
| --- | --- | --- | --- | --- |
| 1 | Node: `context:` in node.yaml — bytes_max and doc_bytes_max, read again each tick like the accounts, defaults 240KB and 60KB; FetchContext takes the budget instead of constants | merged | [mesh-cli#421](https://github.com/runsheet/mesh-cli/pull/421) | $2.35 |
| 2 | Node: instructions and ADRs count against the total — the item's own repos first, then the rest by recency; what does not fit is in the index, never truncated; ADR-015 | merged | [mesh-cli#423](https://github.com/runsheet/mesh-cli/pull/423) | $1.63 |
| 3 | Node: INDEX.md keeps one row per document but carries a summary only for the kinds a session fetches and that are not on disk | merged | [mesh-cli#422](https://github.com/runsheet/mesh-cli/pull/422) | $2.63 |
| 4 | Node: one log line per session with the context bytes written and the documents left to fetch, and `contextBytes` on the usage record with the older-platform fallback; ADR-016 | merged | [mesh-cli#429](https://github.com/runsheet/mesh-cli/pull/429) | $5.23 |
| 5 | Goal doc: context-budget-that-holds — the reading before and after on the same project, the number that moved, what landed and what is owed; ADR-015 records the index trim and the reporting | failed — three evaluations: its "before" was a cost-paired sample of node.log presented as the reading, and its diff carried Go changes | — | $7.15 |
| 6 | Node: every session's usage record is logged on its label with its kind, whatever became of the report, and `mesh node usage` reads node.log into the reading — sessions, median and mean input tokens, cache-read share and median context bytes by kind, for a window; ADR-016 amended | review | [mesh-cli#448](https://github.com/runsheet/mesh-cli/pull/448) | $3.09 |
| 7 | mesh-specs: the TRD's context-on-disk paragraphs and adr-003 name the context budget — node.yaml `context:`, the binding order, never truncated, the trimmed index, the per-session log line and `contextBytes` owed on UsageReportInput | merged | [mesh-specs#101](https://github.com/runsheet/mesh-specs/pull/101) | $1.65 |
| 8 | Goal doc: context-budget-that-holds — the node's reading before and after from `mesh node usage` on the same project with its coverage stated, the platform's mesh-bench.py reading named as the operator's and owed, what landed and what is owed | running (this document) | — | — |

## Measurement

**The reading the goal names is not one this project can take.** `mesh-bench.py` is in none of this project's repositories (mesh-cli, mesh-portal, mesh-specs). The platform does keep the records it would read: the gateway schema (`internal/gateway/schema.json`) has the `recordUsage` mutation and, on `Query`, `usageRecord`, `usageIndex`, `usageIndexs`, `usageWork`, `usageModels`, `usageSubscriptions` and `usageApiKeys`. What stands in the way is not a missing query but everything around it: the checked-in schema lists fields without their arguments, so how those queries filter by project, kind and window is not recorded here; the node's client (`internal/gateway/client.go`, `fields_gen.go`) selects none of them and no mesh-context tool serves them, so a session has no read of the ledger; `UsageRecord` carries no `contextBytes` (it is owed on `UsageReportInput`), so the platform's records cannot show the budget's effect yet; and cost per merged item joins those records to merged items, which is what mesh-bench.py does and where it runs is the operator's. So the platform's mesh-bench.py reading — cost per merged item and median input tokens per build session — is the operator's to take on the platform, and it is **owed** (see Owed). No number in this document is mesh-bench.py's.

What this project can take is the node's own reading: `mesh node usage` (item 6, ADR-016's amendment) over this host's `~/.mesh/node.log` — the node on this machine, the mini, ran the sessions. Everything below is **the node's reading**, taken 2026-09-29T14:56Z.

### Taking it for project runsheet/mesh

Item 6 is in review (mesh-cli#448), not on main, so the command is built from its branch:

```sh
git fetch origin mesh/context-budget-that-holds-6
mkdir -p /tmp/mesh6 && git archive FETCH_HEAD | tar -x -C /tmp/mesh6
cd /tmp/mesh6 && go build -o mesh ./cmd/mesh/
```

`mesh node usage` has no project filter — the mini serves several projects into one node.log, and `--goal` takes one slug — so the log is first narrowed to this project. A session's lines carry its goal's slug (`item <goal>/<seq>: …`, `goal <goal>: …`); `runsheet-mesh-goals.txt` is every goal `mesh_goals` lists for `runsheet/mesh` at the time of writing, one slug a line. The awk keeps the log's line count (the command pairs a legacy record with an end line at most 12 lines away) and blanks every line the node wrote on another project's goal, so another project's end line pairs nothing and its record falls to `unknown`:

```awk
# project-log.awk: blanks node.log lines labelled with a goal not in the first file
NR == FNR { goal[$1] = 1; next }
match($0, /^mesh node: [0-9\/]+ [0-9:]+ (item|goal) [^ \/:]+/) {
	s = substr($0, RSTART, RLENGTH); sub(/.* (item|goal) /, "", s)
	if (!(s in goal)) { print "-"; next }
}
{ print }
```

```sh
awk -f project-log.awk runsheet-mesh-goals.txt ~/.mesh/node.log > node-runsheet-mesh.log
```

Every goal slug labelled in the before window is one project's: of the 178 distinct slugs, 41 are runsheet/mesh's, 123 runsheet/runsheet's and 14 runsheet/loom's, latchkeyid/latchkey's or wardroomid/wardroom's, and no slug is in two projects' lists.

<details><summary>runsheet-mesh-goals.txt (54 goals, mesh_goals at 2026-09-29T14:56Z)</summary>

```text
acceptance-the-evaluator-can-see
a-finished-session-leaves-no-process
a-node-serves-the-orgs-that-enrol-it
builtin-is-mesh-context
bursting-across-accounts
codex-adapter
codex-provider-for-any-openai-compatible-model
codex-sessions-reach-the-context-tool
context-budget-that-holds
directions-on-the-node
evaluation-reports-no-verdict
evaluation-sees-the-merge-base
evaluations-run-on-codex
finish-before-you-start
goal-docs-on-finish
loop-schedules-on-issued-accounts
mesh-cli-crypto-upgrade
mesh-cli-lives-in-runsheet
mesh-spends-less-per-item
node-attests-its-validation
node-builds-tool-config
node-claims-by-priority
node-client-fields-are-generated
node-drains
node-out-of-credit
node-plans-from-the-truth
node-reads-its-limits
node-releases-on-merge
node-reports-usage-on-the-finish
node-runs-on-many-accounts
node-runs-rebase-sessions
node-runs-the-items-model
node-sandboxes-sessions
node-says-why-work-waits
node-scales-with-headroom
node-sees-every-linked-accounts-repos
node-self-update
node-updates-from-a-private-repo
node-upgrades-keep-sessions
node-uses-item-tokens
node-yaml-takes-effect-or-says-why-not
pitching-needs-no-room
plans-depend-only-on-what-they-use
public-releases-on-the-mesh-repo
release-tag-mirrors-with-a-rigger-binding
research-pitches
sandboxed-sessions
signed-releases-on-runsheet-mesh
the-mirror-publishes-without-a-stored-token
the-node-serves-its-enterprise
the-node-serves-its-owners-orgs
token-fetch-retries
tools-show-their-use
upgrades-survive-a-slow-platform
```

</details>

How the node's reading counts: every record in these windows is a legacy `usage: {…}` line (no node running here writes item 6's `<label>: usage <kind> {…}` yet), so the command gives a record a kind and a label only when it pairs it, by cost to the cent, with a session's end line nearby; one it cannot pair is `unknown`. The `build` row is therefore the paired build sessions of runsheet/mesh, not every build session. The `unknown` row cannot be scoped to a project — an unpaired record names no goal — so it is every project's unpaired records.

### The first context line

The node's first `context … bytes in` line (anchored to the node's own prefix; unanchored, the grep also finds a diff a session printed into the log), in the whole log and in runsheet/mesh's:

```sh
grep -anE '^mesh node: [0-9/]+ [0-9:]+ .*context [0-9]+ bytes in' ~/.mesh/node.log | head -1
grep -anE '^mesh node: [0-9/]+ [0-9:]+ .*context [0-9]+ bytes in' node-runsheet-mesh.log | head -1
```

```text
6282066:mesh node: 2026/09/29 14:13:56 item evaluation-reports-no-verdict-platform/11: context 0 bytes in 0 files, 0 documents to fetch
6288269:mesh node: 2026/09/29 14:18:43 item evaluation-reports-no-verdict/9: context 406182 bytes in 19 files, 661 documents to fetch
```

The first is the node's first session on `v0.2.191` (started 14:13:19), the first release to run here carrying items 1–4 (items 1, 2 and 4 are in `v0.2.189`, item 3 in `v0.2.190`); it is a runsheet/runsheet item. The second is runsheet/mesh's first. Its size is more than the default `bytes_max` — the count is bytes on disk with `INDEX.md`, and the index lists every document — which the after reading's `context bytes median` should be read against (see Owed).

### Before

The log begins 2026-09-21T04:11 and the first context line is 2026-09-29T14:13, so the full days before it are 2026-09-22 through 2026-09-28:

```sh
./mesh node usage --log node-runsheet-mesh.log --from 2026-09-22 --to 2026-09-29 --kind build
```

```text
window 2026-09-22..2026-09-29  records 2026-09-28T11:57:50..2026-09-28T23:47:05
kind   sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes
build  48        1221759              2199174            97.0          -                     0
unparsed 0
```

The coverage, as the command reports it — the records it could not pair over the same window (every project's, as above):

```sh
./mesh node usage --log node-runsheet-mesh.log --from 2026-09-22 --to 2026-09-29 --kind unknown
```

```text
window 2026-09-22..2026-09-29  records 2026-09-25T23:53:26..2026-09-28T23:59:23
kind     sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes
unknown  1971      326708               944812             95.6          -                     0
unparsed 0
```

and runsheet/mesh's build sessions the node finished in the window, one `item …: finished …($x)` line each:

```sh
grep -aE '^mesh node: 2026/09/2[2-8] ' node-runsheet-mesh.log | grep -acE 'item [^ ]+: finished .*\(\$[0-9.]+\)'
```

```text
359
```

So the node's before reading for runsheet/mesh stands on 48 paired build sessions, beside 1971 unpaired records (all projects) and 359 finished runsheet/mesh builds in the window: about 13% of the project's build sessions, all of them from 2026-09-28 11:57 on (the log carries no usage record before 2026-09-25T23:53 and pairs none of this project's as a build before 2026-09-28T11:57). It is the node's reading of the project's last day before the change, not of the week.

### After

The window from the first context line to now — whole days, so 2026-09-29:

```sh
./mesh node usage --log node-runsheet-mesh.log --from 2026-09-29 --to 2026-09-30 --kind build
```

```text
window 2026-09-29..2026-09-30  records 2026-09-29T00:18:38..2026-09-29T14:11:48
kind   sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes
build  90        1098056              1618278            96.2          -                     0
unparsed 0
```

Every one of these build records is from before the first context line (the last is 14:11:48; runsheet/mesh's first context line is 14:18:43): **no runsheet/mesh build session after the change is in the build row — zero, fewer than 20 — so the after reading is not yet due**, and no context bytes median is on the row for the same reason. The sessions after the change are in the `unknown` row so far; the build row fills from them only as the cost pairing finds their end lines, or fully once a release carrying item 6 runs on the mini.

Seven days from runsheet/mesh's first context line is 2026-10-06T14:18. The command counts whole days and 2026-09-29 is mostly before the line, so the after reading is **due on 2026-10-07**, over the seven whole days after it — the log narrowed again first, since new goals will have been set:

```sh
awk -f project-log.awk runsheet-mesh-goals.txt ~/.mesh/node.log > node-runsheet-mesh.log   # the goal list re-read from mesh_goals
./mesh node usage --log node-runsheet-mesh.log --from 2026-09-30 --to 2026-10-07 --kind build
```

Then name the number that moved — median input tokens per build session, before → after — and apply the goal's rule: if it did not fall, the change is reverted.

## The numbers

The node's reading for runsheet/mesh, from the blocks above. None of these numbers is mesh-bench.py's.

| Reading | Sessions | Median input tokens | Mean input tokens | Cache read % | Context bytes median | Coverage |
| --- | --- | --- | --- | --- | --- | --- |
| before — the node's reading, 2026-09-22..2026-09-28 | 48 | 1,221,759 | 2,199,174 | 97.0 | — (not yet logged) | 48 build rows beside 1971 unknown (all projects) and 359 finished runsheet/mesh builds; records from 2026-09-28T11:57 |
| after — the node's reading, 2026-09-30..2026-10-06 | not yet due (0 build sessions after 2026-09-29T14:18:43) | — | — | — | — | due 2026-10-07 |

## Owed

- **runsheet/runsheet: `UsageReportInput.contextBytes`** — an `Int` stored on the usage record (ADR-016). Until it lands each report is refused with the field and resent without it, so the bytes are only in node.log.
- **The platform's mesh-bench.py reading, by the operator.** The exact ask: cost per merged item and median input tokens per build session for project `runsheet/mesh`, over the seven days before and the seven days after the first node release carrying items 1–4, **`v0.2.190`** (`gh release list --repo runsheet/mesh-cli --limit 10`: `v0.2.189` 2026-09-29T13:50Z carries items 1, 2 and 4, `v0.2.190` 2026-09-29T14:03Z adds item 3; the mini first ran it as `v0.2.191` at 2026-09-29T14:13Z). This is the reading the goal's acceptance names; this document does not stand in for it.
- **The node's after reading** on 2026-10-07 by the commands in Measurement, and the revert if median input tokens per build session did not fall.
- **Why runsheet/mesh's first context is over `bytes_max`.** Its first context line reports more bytes on disk than the default total; whether that is `INDEX.md` alone (every document is a row) or the budget not holding is for the after reading's context bytes median to show, and for a follow-up if the median is above `bytes_max`.
- **Item 6 on main and in a release** (mesh-cli#448, in review). Until `<label>: usage <kind> {…}` runs on the mini, the node's reading covers only the records the cost pairing finds, and it is built from the item's branch.
- **Nodes on an older binary: none.** `mesh_health_nodes` for `runsheet/mesh` at 2026-09-29T14:56Z lists one node, the mini, on `v0.2.191` with desired version `v0.2.191` — past `v0.2.190` (the latest release is `v0.2.192`).

## Log

- 2026-09-29 12:04Z — item 1 merged, mesh-cli#421: node.yaml `context:`.
- 2026-09-29 12:27Z — item 2 merged, mesh-cli#423: binding documents count against the total; ADR-015.
- 2026-09-29 12:59Z — item 4 merged, mesh-cli#429: the per-session context line and `contextBytes`; ADR-016.
- 2026-09-29 13:26Z — item 3 merged, mesh-cli#422: the trimmed index.
- 2026-09-29 14:10Z — item 5 failed its third evaluation: a cost-paired node.log sample presented as the reading, and Go changes in a docs item.
- 2026-09-29 14:13Z — the mini starts `v0.2.191` and logs its first context line; runsheet/mesh's first at 14:18Z.
- 2026-09-29 14:39Z — item 6 in review, mesh-cli#448: labelled usage records and `mesh node usage`.
- 2026-09-29 — item 7 merged, mesh-specs#101: the TRD and adr-003 name the budget.
- 2026-09-29 14:45Z — item 8's first attempt rejected: its before reading spanned every project on the host, and its diff carried item 6's Go changes.
- 2026-09-29 14:56Z — item 8's second attempt: this document on main alone, with the node's before reading narrowed to runsheet/mesh; the after reading due 2026-10-07; the platform reading owed.
- 2026-09-29 15:03Z — item 8's third attempt: Measurement corrected — the schema has usage queries (`usageRecord`, `usageIndexs`, `usageWork`); the limit is that the node reads none of them, their filters are not in the schema, and `UsageRecord` has no `contextBytes`. Items re-read from `mesh_items`, unchanged.
