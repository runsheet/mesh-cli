---
title: "ADR-030: A failed check resumes the session that built it"
summary: The node keeps each item's last build session — id, adapter, credential, worktree and a copy of its transcript — in ~/.mesh/resume until the item is merged, cancelled, failed or superseded, or seven days; a failing validation step and a retry after a failed evaluation resume that session (`claude --resume`, `codex exec resume`) with one message — what failed, where, the item's acceptance — under the same credential and worktree, else run a fresh session with the brief and the same message and log `item <label>: session not resumed (<reason>); fresh session with the failure`; validation re-runs on the new head, and every retry turn rides the heartbeat's `resumed` and counts as one build session. Amended 2026-10: each item's record.jsonl keeps every note, question, answer, check, hand-back and turn verbatim, read under RecordBudget clipped from the front (else from the platform's activity rows), and a fresh fallback is told it and the branch's state in `# What earlier sessions did` and `# What the branch holds`.
status: accepted
---

# ADR-030: A failed check resumes the session that built it

2026-10-13, goal `a-failed-check-returns-to-the-session-that-built-it`
(items 2, 4, 5, 7–11 and 6). Amends ADR-014 (a failing step's retry
turn), ADR-008 (what a session's end leaves) and ADR-029 (capacity
counts the build sessions); follows ADR-012's hand-back, which is where
`Job.Resume` came from. (ADR-030 is the third decision at this number:
`adr-030-a-session-never-writes-the-shared-clones-config.md` and
`adr-030-an-environment-failure-names-its-kind.md` landed beside it.)

## Context

A poll-mode build is one `claude -p` (or `codex exec`). When the node's
validation failed, ADR-014's retry turn resumed only a host Claude
session; a sandboxed or Codex one got a fresh session told the failure
after its brief. When the evaluator failed the item, the next claim was a
fresh session at full context cost — the largest single spend per item —
that re-derived everything and often repeated the same mistake. The
evaluator's findings reached the next evaluator (ADR-020) but never the
builder that could act on them, and nothing on the node remembered which
session built the item once its session directory was removed (ADR-008).

## Decision

**The resume store** (`internal/node/resumestore.go`). `ResumeStore`
(`RunState.Resumes()`, `~/.mesh/resume/<url-escaped item id>/`) keeps one
`ResumeRecord` per item — `itemId`, `sessionId` (Claude Code's
session_id, Codex's thread id), `adapter` (`claude-code` or `codex`),
`credentialId` (the issued credential, adr-005; "" on the machine's own
login), `worktree` (`~/.mesh/work/<goal>/<seq>`), `transcript` and
`keptAt` — beside a copy of the session's transcript under its source's
own name (a Codex rollout's name carries the date
`harness.RestoreTranscript` reads). Ids and paths only: never a token,
a secret or an env value. `Keep` replaces an item's record whole (built
in a temp dir and renamed in), `Lookup` reads it, `Forget` removes it.
Every build keeps its record after its end is handled and before the
account's config dir and the pod are removed (`Runner.keepSession`,
`harness.TranscriptSource`); a node shutting down keeps nothing, its
session left running for the next process to adopt (ADR-009).

**Its lifetime.** A record lives until the item is merged or given up:
`ResumeStore.Sweep(ctx, statusOf, now)` forgets a record whose item reads
`merged`, `cancelled`, `failed` or `superseded` (`resumeDone`), or that
was kept more than `ResumeKeepFor` — seven days, past which the agent's
own session has long expired — and keeps one whose status read errs, as
the start-up sweep leaves a scope it cannot judge.

**When a retry resumes.** Two checks hand a failure back:

1. *A failing validation step* (ADR-014): the build's own session, in
   the same run. Any `claude-code` or `codex` build whose session id is
   known resumes — a host one in its config dir, a sandboxed one in its
   own pod (the kept transcript put back in the pod's
   `CLAUDE_CONFIG_DIR` from the store when it is gone), a Codex one by
   `codex exec resume <id>` on the build's own `CODEX_HOME`.
2. *A retry after a failed evaluation*: a claim of an item with
   `attempts > 0` whose latest recorded evaluation failed
   (`retryAfterEvaluation`, `internal/node/retryturn.go`). It resumes the
   kept session when the record is this node's, on the harness's
   adapter, on the claim's credential, in the checkout this claim was
   given (ADR-028: one worktree per item, kept for its rework), and its
   transcript restores into the new session's config dir — the account's
   `CLAUDE_CONFIG_DIR`/`CODEX_HOME`, or the pod's (`Runner.resumable`,
   `harness.RestoreTranscript`). The turn runs under that credential, in
   that worktree, on the item's token as any build does.

**When it starts fresh, and saying why.** Otherwise the retry runs a
fresh session whose prompt is the build prompt, then `# The last attempt
failed` (for an evaluation; `# Your last session's validation failed`
for a step) and the same one message, and node.log and the item's
progress note both say

    item <label>: session not resumed (<reason>); fresh session with the failure

with the reason one of `no record` (a restart lost it, another node
built the item, or it was swept), `adapter changed`, `credential
changed`, `worktree gone`, `transcript missing`, `session not found` (the
agent refused the id: `harness.IsUnknownSession`, Claude's `No
conversation found with session ID`, Codex's missing rollout — the
fresh session is run once, in the same claim), or, for a validation
turn, `no session id` / an adapter with no resume / `the agent did not
resume session <id>: <its words>`.

**One message** (`harness.RetryTurnPrompt`, `harness.RetryTurn`): what
failed — the step, its command, exit code and the end of its output, or
each failed clause with its `SATISFY` text and the evaluator's review —
where — the branch, the head that was judged and its changed paths — and
the item's acceptance verbatim, with ADR-012's hand-back words: fix it
and commit, or commit what works and hand back; never push. A resumed
session gets it alone, never the brief again.

**Validation re-runs on the new head.** The retry's commit goes through
the normal build tail: validated (ADR-014), pushed, attested on exactly
the pushed sha, finished. A turn that lands no new commit is the
failure it was.

**The heartbeat and capacity.** Every retry turn — validation's or
evaluation's, resumed or fresh — is noted for its length
(`Loop.NoteRetryTurn`, wired as the Runner's retry-turn hook) and rides
each heartbeat as `resumed [{itemId, kind, sessionId, fresh}]`
(`gateway.HeartbeatExtras.Resumed`, `ResumedSession`), sent only when
the gateway's `HeartbeatNodeInput` has `resumed` (`takesResumed`,
introspected and kept an hour; an older platform gets no key and no
error). `Loop.Running` — the heartbeat's busy count, `runningOf(build)`
and the drain's sessions — counts the items of running builds and of
noted retry turns, each item once, so a turn inside its build is one
session and a turn on its own is one too (busy-counts-every-session-kind).

`internal/node/resume_e2e_test.go` runs all of it on the fake gateway
and a real checkout: a failing step resumed in the same session and the
new head validated and attested; a failed evaluation re-claimed on the
same node resuming the kept session; the same retry after a restart with
the record deleted, and with the credential changed, running fresh with
the same message and the `session not resumed (` line; and the heartbeat
during each retry turn carrying it while the loop counts one session.

## Consequences

- A retry spends the next turn of a session that already holds the
  item's context instead of a cold start; the builder sees the
  evaluator's clauses and review, not only the next evaluator.
- `~/.mesh/resume` holds transcripts — the agent's own record of the
  session, as sensitive as the session directory it outlives. They stay
  on the node, mode 0600, and go with the item or after seven days.
- A retry claimed by another node, or after the record was lost, is no
  worse than before: a fresh session, now told the failure, and the
  reason is on the item for whoever reads it.
- A sandboxed session's pod does not outlive its build; its transcript
  does, in the store, which is what a later pod resumes from. A Codex
  session resumes by its thread id on a fresh `CODEX_HOME` the rollout is
  restored into.
- The platform half — `HeartbeatNodeInput.resumed: [ResumedSessionInput!]`
  — is runsheet's to add; until then the busy count still includes the
  turn through `Running`.
- Not doing: changing the evaluator, or talking to a session mid-run
  (the ask/answer path, goal
  a-builder-asks-and-the-answer-resumes-its-session, reuses this
  store and `Job.Resume`).

## Amendment (2026-10): a fresh session starts from the item's record

Goal `a-fresh-session-starts-from-the-items-record` (items 1–8). A
session that cannot be resumed — no record, another adapter or
credential, the worktree or transcript gone, the agent refusing the id
— got the brief and the failure and nothing of what earlier sessions
did, so it started the work again. The node had the notes and the
branch; it did not hand them over. Now it does, and it never paraphrases
them: an entry is kept or dropped whole, never shortened or reworded.

**record.jsonl** (`internal/node/record.go`, `recordwrite.go`). Beside
`record.json` in `~/.mesh/resume/<item>/` the node keeps
`record.jsonl`: one JSON line per entry, `{at, kind, text}` (`at` RFC
3339, `RecordEntry`), appended `O_APPEND` by `ResumeStore.AppendRecord`
as it happens and **never rewritten** — a line already read stays
byte-equal. `Keep` carries it across a record's replacement, and
`Forget`/`Sweep` take it with the item's dir. The build's `recorder`
writes, in the order things happen:

- `note` — every `mesh_progress` note the session makes, read off the
  stream as it is made (`harness.Progress.WorkCall`, Claude's and
  Codex's streams alike), not only the last;
- `question` — every `mesh_ask` question; `answer` — its answer and
  answerer when the platform serves one on the item (the writer is
  there; the platform serves none yet);
- `check` — a failed validation step: `step <name> (<command>) exited
  <code>`, its environment kind, then its output's last twenty lines;
- `handback` — a hand-back's reason (`SpentOn`) and the session's final
  text;
- `turn` — each session's start (`start: <adapter>, fresh session
  (<why>)` or `resumed session <id>`) and end (session id, resumed or
  fresh, how it ended), and, when a turn ends on a hand-back or a
  question with no note opening `state:`, a turn entry saying so.

The text is the session's or the tool's own words. **No secret is
written**: no env value the node set, no token or credential — the
session account's own secrets are redacted (`SessionAccount.Redact`), the
same rule `record.json` keeps; a note that quotes a value is kept as the
note said it.

**The `state:` note** (`internal/harness/prompt.go`, `handback.go`).
`BuildPrompt`'s `mesh_progress` guidance and `HandBackPrompt` ask for a
last note opening `state:` naming what is done, what is next and what
is open, so the platform's `last_note` and the record both carry where
the work stands. Nothing is refused for its absence; the turn entry
above records it.

**RecordBudget and the read.** `ItemRecord` reads record.jsonl whole
when it is within `RecordBudget` (6,000 characters); past it, the oldest
whole entries are dropped from the front until it fits, the record's
first `turn` entry always kept, and the read says how many were dropped
(`clipRecord`). **The platform fallback**: with no local record —
another node built the item, or the file is gone — `Runner.itemRecord`
reads the item's `MeshActivityIndex` rows by `workItemId`
(`gateway.Client.ItemActivity`, `fields/MeshActivityIndex.txt`) into the
same entries — a progress row a `note`, one opening `question: ` a
`question`, an answer row an `answer` — in time order, clipped the same
way, logged `item <label>: record read from the platform (N entries)`.

**BranchState** (`internal/node/branchstate.go`). `git log --oneline
<base>..HEAD`, oldest first, at most thirty lines (the newest kept), and
`git diff --stat <base>...HEAD`, at most forty, on the item's checkout;
against the repo's default branch when the base is gone (merged), by
the same rule as the retry's changed paths, logged `branch state: base
<dep> is gone (merged); read against origin/<default>`; none when the
checkout is gone, logged `branch state: checkout is gone; none read`.

**The fallback prompt's two sections** (`internal/node/retryturn.go`).
`freshRetryPrompt` — on both the not-resumable path and the
unknown-session fallback in `Runner.RunItem` — is the build prompt,
then `recordSections(freshContext)`, then `# The last attempt failed`
and the retry message:

- `# What earlier sessions did` — the record's entries oldest first as
  `<at> <kind>: <text>`, after a line `(N older entries dropped to fit
  the record's budget)` when any were;
- `# What the branch holds` — the commits, then the diff stat;
- one line: continue from the last entry, do not redo what the commits
  already hold, and run the checks before any new work.

A section with nothing in it is left out. A resumed session gets
neither (its transcript has them). node.log says `item <label>: fresh
session with the item's record (N entries, M commits)`, else `fresh
session with no record (<why>; M commits)`. `freshContext` and
`recordSections` are the shape the ask goal's fresh fallback (draft
`the-node-resumes-a-session-when-its-question-is-answered`) reuses.
`TestFreshRetryPromptCarriesTheRecord` proves the order and the log
line, and that a resumed turn carries neither heading.

