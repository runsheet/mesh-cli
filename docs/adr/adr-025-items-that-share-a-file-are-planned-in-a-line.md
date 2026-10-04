---
summary: The planner names each item's `files` and marks one item `writes_shared_docs`; the node's plan check chains sibling items that name the same code file (`sharedCodeFiles`, the later on the earlier) and never over a document, stamps one writer's brief and every other's do-not-edit sentence, counts the dependencies it added on the hand-in's summary, and hands the writer the notes its siblings left as `## Notes to fold in` — no platform half.
status: accepted
---

# ADR-025: Items that share a file are planned in a line

2026-10-04, goal `items-that-share-a-file-are-planned-in-a-line`
(items 1–4). Amends the dependency rubric of the planner (mesh-specs
trd §3, 'The planner follows a dependency rubric') and sits beside
ADR-023's plan check (`internal/node/plan_check.go`). The number is
shared with `adr-025-a-node-says-which-sessions-it-takes`, landed the
same day; the slugs differ.

## Context

Measured 2026-10-04 over the 133 items merged in the runsheet org since
2026-10-02 22:00Z: 59 were rebased at least once, 91 rebases in all (17
items two to six times), 29 of them by a node session. Every rebase is
a new head and a full CI run.

The files those rebases conflicted on (`WorkItemIndex.rebaseFiles`) are
mostly documents many items append to, not code: AGENTS.md (14 items),
ADRs under `docs/adr` and `specs` (adr-023 5, adr-005 3, adr-034 3),
goal docs (`specs/goal-*.md`), TRD sections (`specs/trd/6a`, `6l`),
README.md; then generated code (`platform/loomgen/registry_gen.go`,
`models_gen.go`) and a few code files that sibling items of one goal
edit at once (`internal/meshmcp/personal.go` 3,
`internal/node/validation.go` 3).

Two causes are the plan's own. Sibling items of one goal run at once
and edit the same code file, because the rubric said "sharing a
repository or touching the same file is not a dependency" and left the
conflict to the merge queue. And every item of a goal was told to amend
the same ADR, TRD section and AGENTS.md, so every item but the first to
merge conflicted on them.

## Decision

1. **The planner names each item's files** (`planFilesRule`, in
   `PlanPrompt`'s rule 3 and its re-ask). Each planned item carries
   `files`, the repo-relative paths it expects to change
   (`PlannedSpec.Files`); `harness.ExpectedFiles` reads them, falling
   back to the paths its brief names. `harness.IsDocumentPath` says
   which are documents: `.md`, `.mdx`, `.txt`, anything under `docs/`,
   `specs/` or `.mesh/`, and the bare AGENTS.md, README.md and
   CLAUDE.md. Generated code such as `registry_gen.go` is code.
   `planDependencyRule` now says sharing a repository is not a
   dependency; sharing a code file is, and the node adds it when the
   planner did not; sharing a document is not.

2. **The node chains items that name the same code file.**
   `sharedCodeFiles` in `internal/node/plan_check.go`, run by `readPlan`
   after `explainedDeps`, finds every pair of items of one repo naming
   the same code file (`namesCodeFile`: not a document, an extension on
   the last segment, never an owner/name repo, a URL's host or a
   `./pkg/...` pattern) with no dependency between them in either
   direction, transitively. The later depends on the earlier, so the
   second starts from the first's merged work instead of conflicting
   with it. Each added dependency joins the working graph at once, so a
   third sharer chains behind the second and no cycle is made. The
   brief carries `Depends on #N because the plan check found both name
   <file>; it starts from item N's merged work`. **Documents never
   chain:** items that share only a document path stay parallel,
   because decision 3 takes the documents off them.

3. **One item writes the goal's shared documents** (`planSharedDocsRule`).
   The planner marks it `"writes_shared_docs": true`
   (`PlannedSpec.WritesSharedDocs`): the last item, or one that
   depends on the rest. Those documents are the ADR, the TRD section,
   AGENTS.md and README. `sharedDocsWriter` takes the marked item, else
   the highest-seq item nothing depends on. Two marked items are flagged
   for the re-ask: `items N and M both claim the shared documents; one
   writes them`. `readPlan` stamps the briefs through the gateway's
   helpers:
   - `BriefWithSharedDocsWriter` adds a paragraph to the writer's brief
     listing every sibling by seq and title under `Fold in the notes
     left on items`.
   - `BriefWithoutSharedDocs` tells every other item not to edit those
     documents. Instead it leaves what it would have added as a
     `mesh_progress` note opening `for the docs writer:`.

   The writer gets no added dependency from the stamp. Only one the
   planner declared applies.

4. **The writer is handed the notes.** When the build runner sees a
   writer's brief (`gateway.SharedDocsWriterOf`), it reads the goal's
   items for the seqs `gateway.SharedDocsSiblings` names. Each item
   whose `lastNote` opens `for the docs writer:` becomes a
   `PromptInput.FoldNotes` entry (`harness.ItemNote`). `BuildPrompt`
   renders them after the brief as `## Notes to fold in`, one line per
   item, `item <seq> (<title>): <note>`, with the pull request URL when
   there is one, and a line naming each sibling's merged pull request
   description as the other source.

5. **The hand-in says what the check did.** `handIn`'s `planned, N
   items` line adds either `, K dependencies added by the plan check
   (3→2 internal/x.go; …)` or `, none added by the plan check`. A person
   reading node.log can then see a plan that was parallel on paper and
   serial in fact.

## The platform half

There is none. The planner's rules are in `internal/harness/prompt.go`,
and the check that adds dependencies and stamps briefs is in
`internal/node/plan_check.go` and `readPlan`. The platform receives an
ordinary `planGoal` whose `dependsOn` and briefs already carry the
result.

The mesh-specs TRD §3 paragraph 'Sharing a repository or touching the
same file is not a dependency' still says the old rule. It is owed to a
mesh-specs follow-up, since this goal is runsheet/mesh-cli only.

## Consequences

- Items that would have conflicted on a code file run one after the
  other. A goal whose items all touch one file becomes a line, and the
  hand-in's summary says so.
- A goal's ADR, TRD section, AGENTS.md and README are written once, by
  the writer, against the merged work of the rest. The other items'
  pull requests carry code and tests alone.
- The notes are the writer's input, so a builder that does not leave
  one loses nothing the writer cannot read from its merged pull request.
  `lastNote` is only the item's latest note, though: a later progress
  note replaces the `for the docs writer:` one. The prompt section then
  lists fewer siblings than the brief names, and the pull request
  description is the only source left.
- The file list is the planner's estimate. A builder that edits a file
  no item named still conflicts, and the merge queue still rebases it.
