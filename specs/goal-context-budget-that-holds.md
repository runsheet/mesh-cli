---
title: A session's context has a budget that actually holds, and the node reports what it cost
kind: plan
status: draft
parent: prd
summary: node.yaml's context budget bounds every binding document, INDEX.md's summaries are trimmed and the node reports what its context cost; the before reading from the node's usage log is 854,171 median input tokens per build session and $2.69 per merged item, and no node has yet run a binary carrying the change, so the after reading is owed.
---

# A session's context has a budget that actually holds, and the node reports what it cost

Mesh goal `context-budget-that-holds`, running.

## Statement

A day of Mesh on 2026-09-28: 500 sessions, 1.16 billion input tokens against 7.3 million output — 160 to 1, with 97% of the input cache reads. The plan windows are the real currency on a subscription, and one account is already at 83% of its seven-day window with work left unclaimed for lack of headroom. Almost none of that is the model thinking. It is the same context, re-read every turn of every session.

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

Statuses as `mesh_items` records them (project `runsheet/mesh`, goal `context-budget-that-holds`) at 2026-09-29T14:00Z.

- **Item 1** (merged): Node: `context:` in node.yaml — `bytes_max` and `doc_bytes_max`, read again each tick like the accounts, defaults 240KB and 60KB; `FetchContext` takes the budget instead of constants — [mesh-cli#421](https://github.com/runsheet/mesh-cli/pull/421).
- **Item 2** (merged): Node: instructions and ADRs count against the total — the item's own repos first, then the rest by recency; what does not fit is in the index, never truncated; ADR-015 — [mesh-cli#423](https://github.com/runsheet/mesh-cli/pull/423).
- **Item 3** (merged): Node: `INDEX.md` keeps one row per document but carries a summary only for the kinds a session fetches and that are not on disk — [mesh-cli#422](https://github.com/runsheet/mesh-cli/pull/422).
- **Item 4** (merged): Node: one log line per session with the context bytes written and the documents left to fetch, and `contextBytes` on the usage record with the older-platform fallback; ADR-016 — [mesh-cli#429](https://github.com/runsheet/mesh-cli/pull/429).
- **Item 5** (running): this document and the ADR-015 amendment.

## Measurement

### Where the figures come from

**`mesh-bench.py` was not found, so it was not run.** It is in none of mesh-cli, mesh-portal or mesh-specs; `mesh_search` for it (and for "cost per merged item", "input tokens per session", "benchmark") across the org finds only ADR-016 referring to it; no `runsheet/runsheet` checkout under `~/.mesh/work/` tracks a file named `*bench*`; and the operator's notes on the mini (`~/.mesh/`, `~/bin`, the home directory) hold no `mesh-bench*`.

**The figures are derived instead from the node's own usage ledger, `~/.mesh/node.log` on the mini** — not from `mesh_evidence`, whose records carry no token count and reach back only to 2026-09-28T05:02Z. Every build session this node ran whose usage record was logged there has one line `usage: {…}` with its `inputTokens` (every input token of the session, cache reads and writes included: `inputTokens = cacheReadTokens + cacheWriteTokens + uncached`), and every build that pushed has a line `item <goal>/<seq>: finished <branch>@<sha> ($x)`. The mini is the only node that built or evaluated runsheet/mesh in the window (all 75 `mesh_evidence` records name evaluator node `313f7f4d…`). The derivation:

- **Build sessions** are the pushed builds of runsheet/mesh's goals (the 53 slugs `mesh_goals` lists for the project), each paired with the logged usage record of the same cost to the cent within 12 log lines (the binary before 2026-09-28 logs the record just before the item's line, the one since just after). A build whose record rode its finish to the platform and was not logged is not counted, nor is a build that pushed nothing. Records were first logged 2026-09-25T23:53Z, so the token half of the window starts there.
- **Cost per merged item** is, for each runsheet/mesh item whose pull request merged in the window, every `($x)` the log names for it in the window — its pushed builds, its evaluations, its rebases — summed; then the mean and median over those items. Plans and builds that pushed nothing are not in it. The merged items are the `mesh/<goal>-<seq>` heads of runsheet/mesh-cli's pull requests merged in the window (`gh pr list`) and the mesh-specs and mesh-portal records `mesh_evidence` holds (the operator's `gh` cannot read those two repos), kept to the project's goal slugs: 170 items.
- **Before** is the seven days ending the day items 1–4 merged: 2026-09-23 to 2026-09-29 inclusive, up to the time of the run. Every session in it ran a binary without the change: the mini has run `v0.2.188` since 2026-09-29T00:51Z (`mesh node v0.2.188, pid 3303985` at 04:00:32, and `~/bin/mesh version` answers `mesh v0.2.188`), and items 1, 2 and 4 are first in `v0.2.189`, item 3 in `v0.2.190`. No line in node.log carries `context … bytes`, and no usage record carries `contextBytes`.
- **After** is every build session of an item whose `item <goal>/<seq>: context N bytes in F files, D documents to fetch` line (item 4, ADR-016) the log shows before it finished — the proof the session ran the budget's code.

The script, `context-bench.py`, written for this reading (the exact file that was run):

```python
#!/usr/bin/env python3
# context-bench.py — median input tokens per build session and cost per
# merged item for one project, from the node's own ~/.mesh/node.log.
#
#   context-bench.py --log ~/.mesh/node.log --goals goals.txt \
#       --merged merged.txt --from 2026-09-23 --to 2026-09-30 [--after]
#
# goals.txt: the project's goal slugs, one per line (mesh_goals).
# merged.txt: "<goal>/<seq>" per item merged in the window, one per line.
# A build session is a line "item <goal>/<seq>: finished <branch>@<sha>
# ($x)" and the node's logged usage record of the same cost beside it;
# its inputTokens is every input token of the session, cache reads and
# writes included. A build whose record rode its finish to the platform
# and was not logged is not counted. Cost per merged item is, per merged item, every
# "($x)" the log names for it (builds that pushed, evaluations,
# rebases) in the window, summed. --after keeps only the builds of an
# item that logged "item <goal>/<seq>: context N bytes in …" (adr-016)
# before it finished, the proof the session ran the budget's code.
import argparse, json, re, statistics

LINE = re.compile(r'^mesh node: (\d{4})/(\d\d)/(\d\d) (\d\d:\d\d:\d\d) (.*)$')
ITEM = re.compile(r'^item ([^ /]+)/(\d+): (.*)$')
COST = re.compile(r'\(\$([0-9.]+)\)')

ap = argparse.ArgumentParser()
ap.add_argument('--log', required=True)
ap.add_argument('--goals', required=True)
ap.add_argument('--merged', required=True)
ap.add_argument('--from', dest='start', required=True)
ap.add_argument('--to', dest='end', required=True)
ap.add_argument('--after', action='store_true')
a = ap.parse_args()
goals = set(open(a.goals).read().split())
merged = set(open(a.merged).read().split())

lines = []
for raw in open(a.log, errors='replace'):
    m = LINE.match(raw.rstrip('\n'))
    if m and a.start <= f'{m[1]}-{m[2]}-{m[3]}' < a.end:
        lines.append(m[5])

def usage(r):
    if not r.startswith('usage: {'):
        return None
    try:
        return json.loads(r[len('usage: '):])
    except ValueError:
        return None

builds, spend, ctx, used = [], {}, set(), set()
for n, rest in enumerate(lines):
    i = ITEM.match(rest)
    if not i or i[1] not in goals:
        continue
    key, what = f'{i[1]}/{i[2]}', i[3]
    if what.startswith('context ') and ' bytes in ' in what:
        ctx.add(key)
    c = COST.search(what)
    if not c or not what.startswith(('finished ', 'evaluation ', 'rebased ')):
        continue
    if a.after and key not in ctx:
        continue
    spend[key] = spend.get(key, 0.0) + float(c[1])
    if not what.startswith('finished '):
        continue
    # The session's usage record is logged just before the item's line
    # (a binary before 2026-09-28) or just after it (since): the one
    # within 12 lines whose cost is the finished line's, to the cent.
    cents = float(c[1]) * 100
    near = range(max(0, n - 12), min(len(lines), n + 13))
    for j in sorted(near, key=lambda j: abs(j - n)):
        u = None if j in used else usage(lines[j])
        if u is None or u.get('kind', 'build') != 'build':
            continue
        cost = u['costUsd'] * 100 if 'costUsd' in u else u.get('listCostCents', -1)
        if abs(cost - cents) <= 1.0:
            used.add(j)
            builds.append((key, u['inputTokens']))
            break

tok = [t for _, t in builds]
per = [spend[k] for k in merged if k in spend]
print(f'window {a.start}..{a.end}{" (after: context line seen)" if a.after else ""}')
print(f'build sessions {len(tok)}')
if tok:
    print(f'  median input tokens {statistics.median(tok):,.0f}')
    print(f'  mean input tokens   {statistics.mean(tok):,.0f}')
print(f'merged items {len(merged)}, with logged spend {len(per)}')
if per:
    print(f'  cost per merged item mean   ${statistics.mean(per):.2f}')
    print(f'  cost per merged item median ${statistics.median(per):.2f}')
```

The inputs and the exact command, on the mini (`G` and `E` are `mesh_goals project=runsheet/mesh` and `mesh_evidence project=runsheet/mesh limit=500` saved as JSON):

```sh
jq -r '.goals[].slug' "$G" > mesh-goals.txt
gh pr list --repo runsheet/mesh-cli --state merged --search 'merged:2026-09-23..2026-09-29' \
  --limit 500 --json number,headRefName,mergedAt > prs-mesh-cli.json
{ jq -r '.[]|select(.headRefName|startswith("mesh/"))|.headRefName|sub("^mesh/";"")
         |capture("^(?<g>.+)-(?<s>[0-9]+)$")|"\(.g)/\(.s)"' prs-mesh-cli.json
  jq -r '.records[]|select(.repo_full_name!="runsheet/mesh-cli")|"\(.goal_slug)/\(.item_seq)"' "$E"
} | sort -u | awk -F/ 'NR==FNR{g[$1]=1;next} ($1 in g)' mesh-goals.txt - > merged-before.txt
python3 context-bench.py --log ~/.mesh/node.log --goals mesh-goals.txt \
  --merged merged-before.txt --from 2026-09-23 --to 2026-09-30
```

### Before

Run 2026-09-29T14:05Z on the mini:

```text
window 2026-09-23..2026-09-30
build sessions 215
  median input tokens 854,171
  mean input tokens   1,503,472
merged items 170, with logged spend 170
  cost per merged item mean   $2.69
  cost per merged item median $2.08
```

### After

**No node has run a binary carrying items 1–4 yet.** The mini is on `v0.2.188`; its `~/.mesh/update.json` holds a desired version of `v0.1.0` it refuses (`release v0.1.0 has no SHA256SUMS on runsheet/mesh`), so it has not followed `v0.2.189` or later by itself. The same script with `--after` over the same window finds nothing:

```text
window 2026-09-23..2026-09-30 (after: context line seen)
build sessions 0
merged items 170, with logged spend 0
```

The exact steps for the after reading:

1. Put a release at or past `v0.2.190` (the first carrying all four items) on the mini: set the node's desired version to it on the console, or by hand `mesh node upgrade --version v0.2.191` (the binary is swapped keeping `.prev`, the node restarts at once and adopts its sessions).
2. Confirm it: node.log's start line reads `mesh node v0.2.190` or later, and `grep -aE '^mesh node: [0-9/]+ [0-9:]+ item [^ ]+: context [0-9]+ bytes in' ~/.mesh/node.log` prints lines. Note the day of the first one: `<start>`.
3. Leave the node seven days from `<start>`, so the after window is as long as the before one.
4. Take the merged items of that window exactly as above, with `merged:<start>..<start+6 days>` and the `mesh_evidence` records merged in it, into `merged-after.txt`.
5. Run `python3 context-bench.py --log ~/.mesh/node.log --goals mesh-goals.txt --merged merged-after.txt --from <start> --to <start+7 days> --after` and put its output here as the after block.
6. Fill the after row of the table. Once runsheet/runsheet stores `contextBytes` (Owed), the usage record's `contextBytes` is the proof as well as the log line.

### The numbers

| reading | build sessions | median input tokens / build session | cost per merged item (mean / median) | merged items |
|---|---|---|---|---|
| before (2026-09-23 → 2026-09-29, v0.2.188 and earlier) | 215 | 854,171 | $2.69 / $2.08 | 170 |
| after | — | not yet taken: no node has run the new binary | not yet taken | — |

No number has moved yet, because no session has run the change; the number to watch is median input tokens per build session, 854,171 before. If the after reading does not show it lower, the goal's own rule holds: the change was wrong and is reverted, not kept for tidiness.

## Owed

- **runsheet/runsheet: `UsageReportInput.contextBytes`** (an `Int`, stored on the usage record; ADR-016). Until it lands every report is sent once with the field, refused, and sent again without it, and the bytes are only in node.log.
- **The after reading**, by the steps in Measurement, once the mini runs `v0.2.190` or later — and the revert if median input tokens per build session did not fall.
- **The mini's desired version.** It asks for `v0.1.0`, which the node refuses, so the release path does not reach the mini by itself; set it to a current release.
- **Input tokens on the platform.** Neither `mesh_evidence` nor `mesh_items` carries a session's tokens, and a usage record that rides its finish is not logged on the node, so this reading counts only the sessions whose record the node logged. A token count per session on the platform's usage ledger, readable by a tool, would let the reading cover every build and every node.
