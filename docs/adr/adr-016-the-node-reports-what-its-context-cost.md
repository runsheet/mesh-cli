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

**Amendment 2026-10-02, goal `context-is-fetched-not-shipped`: the
report also carries how many documents a session fetched, and by which
tool.** Context bytes says what a session wrote to disk; it says
nothing about what the session pulled back in through the MCP tools —
the number this goal's inversion (search before you read, fetch two or
three documents instead of forty) is meant to shrink. Without a count
beside `contextBytes`, the only evidence the inversion worked would be
the input-tokens median moving, which a reader cannot attribute to
fetches versus everything else that moved the same week.

- `harness.Result` gains `ToolNames map[string]int` — calls per
  `<server>__<tool>` (e.g. `mesh-context__mesh_document`), the
  `prologue` alias folded into `mesh-context` as `Result.ToolCalls`
  already does — and `DocumentsFetched int`, the count of those calls
  that were `mesh_document` on the built-in context server: document
  bodies fetched, not `mesh_search` queries. Both are set on the Claude
  stream (`stream.go`, from `toolUseNames`) and the Codex stream
  (`codexstream.go`, from each `mcp_tool_call`'s `Server`/`ToolName`).
- `gateway.UsageReport` gains matching fields: `DocumentsFetched int64
  json:"documentsFetched,omitempty"` and `ToolNames []ToolNameCount
  json:"toolNames,omitempty"` (`ToolNameCount{Name, Calls}`, one row
  per `<server>__<tool>`). `mutateWithUsage` treats each exactly as
  `contextBytes`: its own `atomic.Bool` (`usageWithoutDocumentsFetched`,
  `usageWithoutToolNames`) and its own error (`ErrNoDocumentsFetched`,
  `ErrNoToolNames`), in the same optional-field loop — a platform that
  knows neither is sent the command again without each in turn, every
  later report goes without both and without a retry, and the two
  errors come joined with whatever else the report lost (contextBytes,
  validation, the per-tool list, the turn detail); `errors.Is` finds
  each one in the joined error.
- `SessionAccount.Carried` takes `ErrNoDocumentsFetched` and
  `ErrNoToolNames` as landed, exactly as it does `ErrNoContextUsage` —
  the command is not a failure because one older platform does not
  store a count yet. (Attempt 1 of this item left them out of
  `Carried`'s switch: an evaluator caught the gap, since every
  finishing command rode the plain `err` back to the caller, retrying a
  command that had in fact already landed and failing nothing by
  accident alone.) `Finish` logs each fallback once per run, folded
  into the same notice style as `contextBytes`'s: `usage: the platform
  takes no documents-fetched count on the usage report yet; sent
  without it` and `usage: the platform takes no per-tool call names on
  the usage report yet; sent without them`.
- The session's usage log line (`turnUsageSummary`) ends `, <n>
  documents fetched` beside `turns=… input/turn mean=… max=…
  largest=…`.
- `mesh node usage` adds two columns after `with context bytes`:
  `documents fetched median` (the median `Report.DocumentsFetched`
  over the sessions that fetched at least one) and `with documents`
  (how many of the kind's sessions did), on the same `-` for none as
  the context-bytes columns.
- **Owed to runsheet/runsheet**, beside `contextBytes`:
  `UsageReportInput.documentsFetched` and `.toolNames` (an `Int` and a
  list of `{name, calls}`), for the same scorecard reading. Until they
  land, every report tries both, is refused, and is re-sent without
  them.
