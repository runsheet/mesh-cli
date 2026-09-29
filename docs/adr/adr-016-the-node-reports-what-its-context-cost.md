---
summary: Every session that writes a project's context logs one line after it — `<label>: context <bytes> bytes in <files> files, <n> documents to fetch` — and carries the bytes on its usage report as `contextBytes`; a platform whose `UsageReportInput` has no such field gets the command again without it, loses only that number, and the node says so once per run.
status: accepted
---

# ADR-016: The node reports what its context cost

2026-09-29, goal `context-budget-that-holds` (item 4). Amends the
usage record the finishing commands carry
([ADR-005](adr-005-accounts-on-the-node.md), the claim-time read and the
usage record) and the context every session gets (`WriteContext`,
adr-003 decision 3).

## Context

On 2026-09-28 Mesh ran 500 sessions for 1.16 billion input tokens
against 7.3 million output — 160 to 1, 97% of the input cache reads.
Most of that is the same context re-read every turn: the documents
under `.mesh/context/` and `INDEX.md`, written for every session of
every kind. The goal makes that context's budget hold; this ADR is the
half that makes its cost visible.

Until now nothing said how big a session's context was. `mesh-bench.py`
could only infer it — input tokens per session, turns, cache reads —
and an inference cannot tell a change in the context from a change in
the work. A budget whose effect cannot be seen cannot be tuned, and the
goal's rule (if input tokens per build session do not fall, revert)
needs the number beside the tokens.

## Decision

- `WriteContext` returns, beside the file names, a `ContextWritten`:
  `Bytes`, every document file written plus `INDEX.md`, byte for byte
  what is on disk; `Files`, the document files (`INDEX.md` not among
  them); `ToFetch`, the indexed documents left off disk
  (`len(cx.Index) - len(cx.OnDisk)`), which a session fetches on demand
  with `mesh_document`.
- Every session that writes context — build (`runner.go`), evaluate,
  rebase, plan and research — logs exactly one line right after it, on
  its usual label:

  ```
  item <goal>/<seq>: context 187392 bytes in 21 files, 43 documents to fetch
  goal <slug>: context … bytes in … files, … documents to fetch
  project <slug>: context … bytes in … files, … documents to fetch
  ```

- The session's account carries the bytes (`SessionAccount.SetContextBytes`)
  onto its usage report, `gateway.UsageReport.ContextBytes`
  (`contextBytes`, omitted when zero), on whichever finishing command
  carries it — `FinishWorkItem`/`FinishWorkItemAlreadyDone`,
  `EvaluateWorkItem`, `FinishWorkItemRebase`, `PlanGoal`,
  `RecordDirectionResearch` — and in the `usage: {…}` line when none
  did. A priced tool call's record (`RecordToolUsage`) does not repeat
  it: the context is counted once, on the session's report.
- `mutateWithUsage` follows `validation`'s fallback exactly: a platform
  answering unknownField on `contextBytes` gets the command again
  without it; `usageWithoutContext` remembers that, so every later
  report goes without the field and without a retry; and the command
  returns `ErrNoContextUsage`. `SessionAccount.Carried` takes it as
  landed and `Finish` says once per run `usage: the platform takes no
  context bytes on the usage report yet; sent without them`. A platform
  without the field loses that one number and nothing else: the
  command, the rest of the report and the log line all stand. When a
  report lost more than one optional part (tools, validation, context
  bytes) the errors come joined, so each is said. A platform refuses one
  unknown field per attempt, and in no order the node can count on: each
  refusal naming an optional field still sent drops it and sends the
  command again, until it lands or the refusal names none of them. The
  turn and tool-result detail (`ErrNoUsageDetail`) is one more such
  refusal in the same loop, dropped for that report alone: a platform
  that refuses `contextBytes` and then the detail gets both said,
  `ErrNoContextUsage` joined with `ErrNoUsageDetail`, never one for the
  other.

**Amendment 2026-09-29 (item 6): the record is always logged, on its
label, and `mesh node usage` reads it.** `SessionAccount.Finish` logged
the `usage: {…}` record only when its finishing command had not carried
it, and with no kind, goal or seq — so node.log could not say what a
build session cost in input tokens, and a goal doc read from a partial
sample was failed three times for it. Now:

- `Finish` logs every session's record, after the once-per-run notices,
  as `<label>: usage <kind> {…}` — `item <goal>/<seq>: usage build`,
  `… evaluate`, `… rebase`, `goal <slug>: usage plan`,
  `project <slug>: usage research` — whatever became of the report:
  carried, refused (`ErrNoUsageLedger`) or never sent (a fail path).
  Each session path names its label and kind once, beside
  `SetContextBytes` (`SessionAccount.SetLabel`). The JSON is the
  `gateway.UsageReport` exactly as sent, redacted: the label and kind
  are on the log line, never on the report, whose input the platform
  holds to its schema. A session another carries (`Carries`: a second
  opinion, a plan asked twice) logs no record of its own — its figures
  are in the carrier's, and two would count them twice. A session with
  no label keeps the older `usage: {…}` line. The `usage: turns=…`
  summary line is unchanged.
- `mesh node usage --from <day> --to <day, exclusive> [--log]
  [--goal] [--kind]` (`node.ReadUsage`, `UsageReading.Lines`) reads
  those records back: one row per kind — sessions, median and mean
  input tokens, the cache-read share, the median context bytes over the
  sessions that carry them — the span of the records read, and how many
  did not parse. An older binary's unlabelled `usage: {…}` takes the
  label and kind of the nearest `finished`/`evaluation`/`rebased …($x)`
  line within 12 lines whose `$x` is its `listCostCents` to the cent,
  else `unknown`. The platform exposes no usage query, so this is the
  node-side reading the goal doc quotes, before and after.
- (Item 11) The reading is precise enough to quote. `--from` and `--to`
  take a date (`YYYY-MM-DD`, local midnight) or an RFC 3339 timestamp
  (`2026-09-29T14:18:43Z`, or with an offset), so an after-reading can
  start at a release's first context line rather than sweep in the
  morning's pre-change sessions; the `window` line prints the two as
  given when either has a time part. `--project org/slug` keeps the
  records of the project's goals — the org by `OrgByLogin`, the project
  by slug from `Projects`, its goals from `Goals` by `projectId` — and
  its research records (`project <slug>: usage research`) by the
  project's own slug; `--goals <file>` (one goal slug a line, `#`
  comments) does the same offline, without research, and the two
  together are refused. The command resolves the list;
  `UsageQuery.Goals` (nil is every goal) and `Project` keep
  `ReadUsage` off the network. And the reading names its sample size:
  `UsageReading.Ended` counts, over the same window and filter, the
  labelled session-end lines of each kind — `finished …($x)` (build),
  `evaluation …($x)` (evaluate), `rebased …($x)` (rebase), `goal
  <slug>: planned, <n> items ($x)` (plan), `<project>/<direction>:
  pitched …`/`proposed … goal(s) ($x)` (research) — and after the table
  one line per kind in either count, `unknown` aside: `coverage build:
  48 records of 359 sessions ended in the window`. A session that ended
  with no such line (a failure, a release) has its record read but is
  not among the sessions ended, so records can exceed them. The
  record's format and the legacy pairing are unchanged.

## Consequences

- The log line is there on every node from this release, whatever the
  platform: node.log alone answers "what did this session's context
  cost".
- **Owed to runsheet/runsheet**: the platform's
  `UsageReportInput.contextBytes` (an `Int`, stored on the usage
  record), and its use in the scorecard and `mesh-bench.py`'s reading —
  context bytes per session beside input tokens per session. Until it
  lands, every report is sent once with the field, refused, re-sent
  without it, and every later one goes without; `schema.json` here is
  not changed until the platform serves the field.
- The bytes are what was written, not what the model read: a session
  that fetches documents with `mesh_document` pays for them in tokens
  the report already counts, not in `contextBytes`.
- (Amendment 2026-09-29) node.log grows by one record line per
  session, carried or not; it is the only place a node's reading comes
  from until the platform serves one, and `mesh node usage` reads only
  what the node that wrote the log still has — a rotated or lost log is
  a gap in the reading, not a zero.
