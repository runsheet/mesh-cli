---
summary: Every session logs what its context cost on disk (`contextBytes`) and, since 2026-10-02, carries the documents it fetched by tool (`documentsFetched`, `toolNames`) on its usage report and in `mesh node usage`'s `documents fetched median` and `fetched by tool` columns; a platform without a field refuses it once, the report lands without it, and the node says so once per run.
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

**Amendment 2026-10-02 (goal `context-is-fetched-not-shipped`, item
1): the bytes are the inverted default's.** INDEX.md no longer carries
two document tables and every live goal's items: per ADR-015 as
rewritten, it is a screen — what is on disk, how to fetch the rest, one
count line — and the session's own goal with its items; the only
document files are the session's own repos' instructions. `Bytes` is
still every file written, INDEX.md included, and `ToFetch` every indexed
document not on disk, which is now nearly all of them. The context line
is unchanged; a context past ADR-015's 40KB target adds one line after
it, `<label>: warning: context <n> bytes, past the 40960-byte target:
<file> took it past`.

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

## Amended 2026-10-02

Goal `context-is-fetched-not-shipped` (items 3 and 7): the usage
report also carries how many documents a session fetched by tool, and
which tools it called. Context bytes says what a session had on disk;
it says nothing about what the session pulled in through the MCP
tools — the number this goal's inversion (the disk holds only the
repos' instructions and the session's own goal; search before you
read, fetch two or three documents instead of forty) moves the reading
onto. Without a count beside `contextBytes`, the only evidence the
inversion worked would be the input-tokens median moving, which a
reader cannot attribute to fetches versus everything else that moved
the same week.

- **`documentsFetched`.** `harness.Result.DocumentsFetched` counts the
  session's `mesh_document` and `mesh_search` calls on the built-in
  context server — the two reads that replace what the context
  directory used to ship; `mesh_items` and the rest are not counted.
  The `prologue` alias is folded into `mesh-context`
  (`tools.IsBuiltin`), as `Result.ToolCalls` already does. It is set on
  the Claude stream (`stream.go`, each `tool_use` named
  `mcp__<server>__<tool>`) and the Codex stream (`codexstream.go`, each
  completed `mcp_tool_call`'s `server` and `tool`). Two `mesh_document`
  calls and one `mesh_search` are 3. A carried session's count is
  summed into its carrier's (`SessionAccount.Add`). The report's
  `DocumentsFetched int64 json:"documentsFetched,omitempty"` is left
  out at zero.
- **`toolNames`.** `harness.Result.ToolNames` is the session's MCP
  calls per `<server>__<tool>` (e.g. `mesh-context__mesh_document`),
  finer than `tools`' per-server totals, so a reading can tell a
  document from a search. The report's `ToolNames []ToolNameCount
  json:"toolNames,omitempty"` (`{name, calls}`, sorted by name).
- **The older-platform fallback.** `mutateWithUsage` treats each field
  as it treats `contextBytes`: its own refusal flag
  (`usageWithoutDocumentsFetched`, `usageWithoutToolNames`) and its
  own error (`ErrNoDocumentsFetched`, `ErrNoToolNames`) in the same
  optional-field loop. A platform whose `UsageReportInput` lacks the
  field refuses it once ("Unknown field"), the command is sent again
  without it and lands, and every later report on that client omits it
  with no retry (`TestUsageReportWithoutDocumentsFetched`,
  `TestUsageReportWithoutToolNames`). The errors come joined with
  whatever else the report lost — `contextBytes`, `validation`, the
  per-tool list, the turn detail — and `errors.Is` finds each.
  `SessionAccount.Carried` takes `ErrNoDocumentsFetched` and
  `ErrNoToolNames`, alone or joined with `ErrNoContextUsage`, as a
  landed command, not a finishing failure, and `Finish` says each loss
  once per run on the node's logger: `usage: the platform takes no
  documents-fetched count on the usage report yet; sent without it (said
  once, for every session this run)` and `usage: the platform takes no
  per-tool call names on the usage report yet; sent without them (said
  once, for every session this run)`.
- **The log.** The session's `usage: turns=…` line ends `, <n>
  documents fetched`, and the session's record line carries
  `documentsFetched` and `toolNames` when set, so `mesh node usage`
  reads them back whether the platform took them or not.
- **The new usage columns.** `mesh node usage` adds two columns after
  `with context bytes`: `documents fetched median` — the median
  `documentsFetched` over the kind's sessions that fetched any, `-`
  when none did — and `fetched by tool`, how many of the kind's
  sessions fetched any. Because the field is left out at zero, a
  session that fetched nothing and one from a node older than the count
  read alike; `sessions` less `fetched by tool` is both.
- **Owed to runsheet/runsheet**, beside `contextBytes`:
  `UsageReportInput.documentsFetched` (`Int`) and `.toolNames` (a list
  of `{name, calls}`). Until they land, the first report of a run that
  carries either is refused once and re-sent without it.
