# mesh-cli — the node

The operator tool for Mesh. `mesh node start` makes this machine a node of
the runsheet platform: it speaks to the gateway (`api.runsheet.dev/graphql`,
`/mesh/v1`, `/mcp`) on the credential in `~/.mesh/token` — the Latchkey
machine credential `mesh node login` registers for this computer, or the
operator's `rs_*` console token — claims work items, builds each in a fresh `claude -p`
session, evaluates it in a second, plans goals in a third. The platform
side (goals, items, nodes, the scheduler, the integrator) is
`runsheet/runsheet`; the design is `runsheet/mesh-specs` (prd, trd,
adr-001, adr-002) and it is binding.

```sh
go vet ./... && go build ./cmd/mesh/ && go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o mesh-linux ./cmd/mesh   # the mini runs linux/amd64
```

## How it is shaped

- `internal/harness/prompt.go`: the five prompts — `BuildPrompt`,
  `EvaluatePrompt`, `PlanPrompt`, `ResearchPrompt`, `RebasePrompt`. Their
  contract lines are load-bearing: the evaluator ends with
  `VERDICT: PASS|FAIL` and, when it fails, one
  `FAILED: <clause> || SATISFY: <what would satisfy it>` line per failed
  acceptance clause above it (`ParseFailedClauses`; the node records them
  with the verdict as `evaluateWorkItem`'s `failedClauses`, and a retry's
  evaluator is given the clauses earlier attempts named and judges that
  attempt on those and on regressions, never on a gap nobody named); a
  clause that reads the host — load, wall time, a ratio of durations —
  is judged on its mechanism and its reading written above the verdict
  as `READING: <clause> || <what it measured, when, under what load>`
  (`ClauseReading`, `ParseReadings`), never failed for the host's number,
  and a retry that fails a clause an earlier attempt failed opens its
  review with `fails on the same clause as attempt N: <clause>`
  (`SameClauseLine`; `SameClause` finds the repeat and `NoteSameClause`
  prepends the line when the model forgot it — the node applies it in
  `afterEvaluate` on either family before `EvaluateWorkItem` and logs
  `fails on the same clause as attempt N`, and one `reading recorded:
  <clause>` line per READING, which rides the notes; ADR-020), the planner ends with the JSON graph in a fenced
  block — `items` and, beside them, `"questions": [{"clause", "question"}]`
  for a clause no PR can satisfy (`ParsePlanQuestions`; sent with
  `planGoal` only when its input has `questions`, `TakesPlanQuestions`,
  else each logged `plan <goal>: question for the person: …`), its rule
  3 and re-ask saying an item's acceptance names only what a PR can
  change and a host reading is a `Measure: …` item (`planHostRule`;
  the node's `hostReadingAcceptance` flags a threshold on a reading in
  any other item for the re-ask, ADR-020), and saying a lineage's
  attempts shrink — a failed item is replaced by two or more items
  partitioning its acceptance, never recreated at the same acceptance,
  an item that cannot be halved carrying `"atomic": "<reason>"`
  (`PlannedSpec.Atomic`) exempt once, a failed item's acceptance and
  lineage shown in the items so far, and a narrow re-plan's
  `PlanInput.Replacing` written as `## Split this item` — the planner's narrow mode: a goal carrying `replacingItemId`
  (`Goal.ReplacingItemID`) is planned as a split of that item alone,
  its evaluations' failed clauses as `ReplacingClauses`, an answer of
  fewer than two items and no once-only `atomic` one asked again and
  then refused, and only the replacements handed in, `PlanGoal`
  rewiring the dependents (runsheet ADR 023)
  (`planSplitRule`, ADR-021; the node's `sameAcceptanceAsFailed` flags
  a new item whose `normaliseAcceptance` equals a failed, superseded or
  erred-cancelled item's of the goal, naming its lineage from
  `WorkItem.LineageRootSeq`/`LineageAttempt`, for the re-ask, an
  `atomic` item let through unless that item's brief opened `atomic:`,
  and a second answer still re-landing is refused — `PlanGoal` not
  called, the refusal a `*PlanRefused` (`PlanRefusedOf`); the same
  refusal twice running for a goal (`normaliseRefusal`, the last kept in
  `.plans/refusals.json` under the work root) releases the plan claim
  with its text, `releaseGoalPlan`, logged `goal <slug>: plan refused
  twice with the same reason; released, not re-planned: <reason>` (a
  platform without it `platform has no releaseGoalPlan; held on the
  node`, once), and `planRequested` passes the goal over and holds it,
  a `HeldGoal` of kind `plan`, while `Planner.Released` — the goal's
  `updatedAt` unchanged, else still in planning — says so
  (`Loop.PlanReleased`; ADR-023, a refused plan is released, not
  re-planned); the reason rides the brief's first line `atomic: <reason>`,
  `gateway.BriefWithAtomic`/`AtomicReason`), and its rule 3 and re-ask
  carrying four more rules after `planSplitRule`: a plan over the
  project's `maxItems` is answered `split` — goal drafts, each
  `{slug, title, statement, acceptance, repo}`, each at most `maxItems`
  items and one repo, for the node to propose as goals (`planSizeRule`,
  `PlanInput.MaxItems` naming the cap in rule 2, `OverCap` turning the
  re-ask into an ask for `split`, `GoalDraft`, `ParsePlanSplit` reading
  `{"split": [...]}` from the same candidates as `ParsePlan`, so a
  `split` answer with no `items` is not a parse error; the node's
  `overCap(specs, cap)` flags a graph over `project.ItemCap()`, read at
  claim time into `MaxItems` — 8 until the platform serves `maxItems` —
  for the re-ask with `OverCap` set, a final answer still over it
  refused (`overCapRefusal`), nothing handed in; `readPlan` reads a
  `split` answer first, each draft with a slug, title and acceptance
  naming exactly one of the project's repos (`splitDrafts`) or the
  answer refused naming the cap, and `handInSplit` sends the drafts with
  `ProposeGoalSplit` when `TakesGoalSplit`, `planGoal` never called,
  logging `goal <slug>: over the cap (N items > M); proposed a split
  into K goals: …`, else logs each `plan <goal>: proposed goal <slug>:
  <title>` and errs, the goal left in planning; ADR-023's amendment
  2026-10-04); a docs-only
  close-out item — amending documents alone to say the branches read as
  one design or describe what landed — is refused, docs landing in the
  pull request of the change they describe, folded into that item
  instead (`planCloseOutRule`; the node's `closeOutAcceptance` flags an
  item whose title or acceptance matches a close-out phrase and whose
  acceptance names no code or test check — a `go test`/`go build`/`go
  vet`, a `.go`/`.ts`/`.sh` file, `npm test`, `make`, or a backticked
  command other than `grep`/`cat`/`ls` on a `.md` — for the re-ask,
  appended beside `hostReadingAcceptance`; an ADR amended beside its own
  code or test check is never flagged, ADR-023's amendment); a clause waiting on a person's
  confirmation or a production observation is never an item, becoming
  `"precondition": "<clause>"` on the item that depends on it
  (`PlannedSpec.Precondition`, kept by `ParsePlan`) or a `pr` checkpoint
  on the goal beside `items` as `"checkpoints": ["pr"]`
  (`ParsePlanCheckpoints`, read like `ParsePlanQuestions`;
  release-tag-mirrors item 14's gate no builder could open is the
  example, `planCutOverRule`; the node's `cutOverAcceptance` flags a
  sentence waiting on a person or production — "the operator deletes",
  "in production", "STEP 0 reading", "the gate is closed" and the like,
  a `Measure:` item exempt — for the re-ask, beside
  `closeOutAcceptance`; a `precondition` rides the brief's first line
  `precondition: <clause>`, after `atomic:`'s
  (`gateway.BriefWithPrecondition`/`PreconditionOf`), and is sent as
  the item's `precondition` only when `PlannedItemInput` takes it
  (`TakesItemPrecondition`); a `pr` checkpoint the goal lacks is set by
  `SetGoalCheckpoints` before `PlanGoal`, logged `goal <slug>: pr
  checkpoint set: a person approves each pull request`, any other value
  logged and dropped, ADR-023's amendment); and an item naming a repo other than the
  goal's is a different goal, left out or drafted into the `split`
  (`planRepoRule`; ADR-023 (d) — the node's plan check,
  `internal/node/plan_check.go`'s `otherRepoItems(specs, itemRepo,
  goalRepo)`, flags each one for the re-ask and, a final answer — the
  re-ask's, or an adopted session's — still holding one is refused
  outright like a re-land, `plan names repo <owner/name> outside the
  goal's <owner/name>; a different goal; refused`, nothing handed in;
  `goalRepo` is `Goal.RepoID` among the project's repos when the
  platform names one, else the project's one repo when it has exactly
  one, else the rule does not apply — `Goal.RepoID` rides no selected
  field yet, so it is always "" until `fields/GoalIndex.txt` gains
  `repoId?`, which the gateway's `maxItems`/`ProposeGoalSplit`/
  `SetGoalCheckpoints` and the hand-in of a `split` answer are later
  items of the same goal to land); and two more after it, the planner
  naming each item's files and one writer of the shared documents
  (ADR-025, `docs/adr/adr-025-items-that-share-a-file-are-planned-in-a-line.md`):
  `planFilesRule` — each item lists `files`
  (`PlannedSpec.Files`, json `files`; `ExpectedFiles` reads them, else
  the paths its brief names; `IsDocumentPath` says which are documents:
  `.md`/`.mdx`/`.txt`, under `docs/`/`specs/`/`.mesh/`, the bare
  AGENTS.md/README.md/CLAUDE.md), and `planDependencyRule` says sharing
  a code file is a dependency the node adds, sharing a document is not —
  and `planSharedDocsRule` — the ADR, the TRD section, AGENTS.md and
  README are written by one item marked `"writes_shared_docs": true`
  (`PlannedSpec.WritesSharedDocs`), every other brief saying not to and
  to leave a `mesh_progress` note opening `for the docs writer:`; the
  node's plan check, `readPlan` after `explainedDeps`, runs
  `sharedCodeFiles(specs)` — two items of one repo naming the same code
  file (`namesCodeFile`) with no dependency either way, transitively,
  get one, the later on the earlier, `Depends on #N because the plan
  check found both name <file>; …` on its brief, documents never
  chained — and `sharedDocsWriter(specs)` — the marked item, else the
  highest-seq item nothing depends on, two marked flagged for the
  re-ask (`sharedDocsWriterFlags`) — whose brief
  `gateway.BriefWithSharedDocsWriter` stamps with its siblings and every
  other's `gateway.BriefWithoutSharedDocs`, read back by
  `gateway.SharedDocsWriterOf`/`SharedDocsSiblings`; `handIn`'s
  `planned, N items` line ends `, K dependencies added by the plan
  check (3→2 internal/x.go; …)` or `, none added by the plan check`;
  and the writer's build prompt carries `PromptInput.FoldNotes`
  (`harness.ItemNote`), each sibling's `lastNote` opening `for the docs
  writer:` with its `prUrl`, the runner's `foldNotes` reading
  `GoalWorkItems`, rendered after the brief as `## Notes to fold in`;
  no platform half, the mesh-specs TRD §3 paragraph owed to a follow-up),
  the director ends with its pitches and answers in one for a
  propose-mode direction, its proposals in one for a set-mode one, the rebase session
  ends with `REBASED: <sha>` and `RESOLVED: <files>` or
  `REBASE FAILED: <why>`, the builder never pushes and checks main
  first, ending with `ALREADY DONE: <evidence>` when the acceptance
  already holds there (`ParseAlreadyDone`; the runner then finishes the
  item `already_done` with no push, adr-003). The builder ends by
  calling the hosted `mesh_finish(item_id, branch, head_sha)` — or
  `mesh_finish(item_id, outcome=already_done, evidence)`, or
  `mesh_fail(item_id, error)` — on `runsheet` (BuildPrompt's
  `# Finish by tool call`): the harness keeps each mesh-work tool's input
  (`tools.IsWorkTool`: mesh_finish, mesh_fail, mesh_verdict, mesh_plan,
  mesh_progress) on `Result.WorkCalls` by tool name, from Claude's
  `tool_use` and Codex's `mcp_tool_call` alike, and `Runner.afterBuild`
  takes a `mesh_finish` call as the finish whatever the final message
  says (`finishCallOf`; the node still pushes and hands in the head it
  pushed), a `mesh_fail` call as the item's failure, and with no call
  parses the text as before, logging `item <label>: no mesh_finish call;
  parsed the final message` once per session; the call records nothing
  (a build's `mesh_finish` is the node's own `mesh-node` server's), so
  the node validates, pushes, and only on the sha `Push` returned sends
  `FinishWorkItem` — a push that fails fails the item `push <branch>:
  <err>` with no finish sent, and a refused finish is an error (ADR-023,
  finish by tool call, Decision 2 amended 2026-10-10). The push is
  `Runner.pushBuild`: `ItemCred.ForPush` rewrites the checkout's helper
  with the build's own token (issued at claim, or its `Refresh`) just
  before `Workspacer.Push`, so a token written there since is never
  used; a push refused for its credential (`IsCredentialRefusal`, `Write
  access to repository not granted` among them) is `Reissue`d once and
  retried, and still refused is a `*PushCredentialError` naming the
  repo, branch and the token's access, issue time and expiry
  (`TokenFacts`, `gateway.GitHubToken.Access`/`IssuedAt`), never the
  token; logged `item <label>: pushed <sha>; finished <branch> ($x)` or
  `item <label>: push refused (<token facts>): <git's words>; not
  finished` (ADR-014's amendment 2026-10-10: push → attest → finish). The evaluator ends the same
  way by `mesh_verdict(item_id, passed, notes, failed_clauses: [{clause,
  satisfy}], readings: [{clause, reading}])` (EvaluatePrompt's
  `# Finish by tool call`): `verdictOf`, on the first evaluation and
  the second opinion of either family, takes the call as the verdict
  with an empty final message (`harness.VerdictCall`, its readings
  appended to the notes as `READING:` lines so `logReadings` and
  `NoteSameClause` run as on the text), parses the text with no call,
  logging `item <label>: no mesh_verdict call; parsed the final message`
  once per session, and with neither is no verdict; an
  `EvaluateWorkItem` refused after a call is `evaluated by tool call on
  the platform`. The planner ends by `mesh_plan(goal_id, items,
  questions)` (PlanPrompt's `# Finish by tool call`): `readPlan`
  renders the call as the fenced block (`harness.PlanCall`) and reads
  it through `ParsePlan` and every plan check, else parses the text,
  logging `plan <goal>: no mesh_plan call; parsed the final message`.
  The `VERDICT:`/`FAILED:`/`READING:` lines and the fenced JSON stay
  load-bearing as the fallbacks. Both `mesh_finish` calls carry
  a `description` — the PR body `FinishDescription` renders
  (`internal/node/finishdesc.go`): a `## Who` section naming who built
  the item (its actor, model and adapter) and who it was paired with,
  the evaluator left to `EvaluateWorkItem`'s own actor rather than named
  here — sent only when the introspected `FinishWorkItemInput` takes
  `description` (every-actor-has-a-name item 3, adr-015's amendment).
  Change wording freely; never the contract. (A later goal moves the
  guidance onto the
  platform as agent definitions.)
- `internal/latchkey/` + `internal/auth/token.go`: the machine
  credential — Latchkey's device flow, the rotating refresh grant, and
  the `~/.mesh/token` that holds either shape behind one `TokenSource`
  (latchkey `adr-machine-identity`).
- `internal/node/`: the loop (`runner`; a workspace that cannot be prepared before the harness starts — `PrepareWithRetry`, `Identify`, `WriteContext` — releases the claim with `workspace: <error>` by `ReleaseOnWorkspace`, never `FailWorkItem` unless the release is refused, and `Declines.HoldFor` keeps the item off the node for `WorkspaceReleaseHold`, ten minutes; ADR-024, a workspace failure releases the claim), the workspace — each new item's
  `~/.mesh/work/<goal>/<seq>` a `git worktree add` of one shared clone
  per repo at `~/.mesh/repos/<owner>/<name>` (`ReposRoot()`,
  `Git.ReposRoot`, `SharedClonePath`; `syncSharedClone` clones it once
  — `gh repo clone`, or `git clone` on an issued item token — and
  fetches it under the per-repo lock, then `addWorktree`; ADR-028,
  `docs/adr/adr-028-one-clone-per-repo-one-worktree-per-item.md`,
  following mesh-specs ADR 017), each worktree's credential helper its
  own `git config --worktree` (`writeHelper`,
  `extensions.worktreeConfig` on the clone, whose config holds no
  token), and nothing else of a session's in the shared clone's
  `.git/config` (ADR-030,
  `docs/adr/adr-030-a-session-never-writes-the-shared-clones-config.md`):
  `Identify`'s `user.name`/`user.email`/`core.hooksPath` go
  `--worktree` too, the item token rides each command's `GIT_CONFIG_*`
  env (`withGitToken`, `OnItemToken`), origin and
  `extensions.worktreeConfig` are read first and written once per clone
  under the repo lock (`sharedOrigin`, `syncSharedClone`,
  `addWorktree`), a git lock older than a minute is removed — `removed
  stale git lock <path> (age <d>)` — and the write retried once
  (`ClearStaleGitLock`, `withStaleLockRetry`), and a wait on the repo
  lock logs `repo lock <key>: waiting on <goal>/<seq> (pid <n>, held
  <d>)` (`WithLockHolder`, `SetRepoLockLogf`);
  `TestConcurrentSessionsOnOneRepoE2E` (`-tags e2e`) starts two sessions
  of one repo at once twenty times over, a rework reusing its worktree and keeping unpushed commits,
  a full clone from before the layout used in place until swept, and
  `Git.RemoveWorkspace` the sweep's remover — a worktree by `git
  worktree remove --force` + `git worktree prune` under the repo's
  lock, a full clone by RemoveAll, a path under the repos root refused,
  never the shared clone — its evaluation's
  at `<goal>/<seq>-eval` (`EvalWorkspaceSpec`, the same Root, ADR-027),
  a clone of its own, as are the goal doc's `<goal>/0` on the base and
  plan and research checkouts (`Git.sharedClone` returns ""), so an
  evaluation never rewrites the build's helper and git is never asked
  for a branch another worktree holds — every `gh repo
  clone` and `git fetch` `Git` runs going through `fetchLocked` under a
  per-repo fetch lock (`repolock.go`: a process-wide mutex keyed by
  `RepoLockKey`, owner/name, and an exclusive flock on
  `<root>/.locks/<owner>--<name>.lock`, `Git.LockRoot` else the spec's
  Root else the checkout's grandparent; released before any checkout,
  never held across a session or a push; ADR-023, fetches of one repo
  are serialised on the node) — a main validation takes none of it:
  it runs in a scratch dir of the node's own under `<work
  root>/.scratch` from the served diff (ADR-034, Main validation below),
  a worktree only as its fallback, so the work root holds nothing of a
  validation's once it ends but an empty `.scratch` — and the project's
  context (`workspace.go`: `FetchContext`, `RenderIndex`, ADR-015 — on disk only the instructions of the session's own repos (`ContextOptions.Repos`), keyed by repo and path — a slug several repos share, as every AGENTS.md's `agents` is, read from the session's own checkout (`ContextOptions.Checkouts`), never by slug from the gateway, which answers one repo's body for all — each as `instructions-<owner>-<repo>-<path>.md` (`contextFileName`: the path without `.md`, so one repo's two paths of one slug are two files), and, in INDEX.md, its own goal (`ContextOptions.Goal`: statement, acceptance, items; none for research); no ADR, PRD or TRD body is fetched; INDEX.md is a screen with no document table and no other goal — what is on disk, `mesh_search`/`mesh_document`/`mesh_documents`/`mesh_projects`/`mesh_items` for the rest, the ADRs and TRD binding, one count line; node.yaml `context:` (`ContextConfig`, re-read each tick, `Loop.ContextBudget`) `bytes_max` is the cap, default 80KB, INDEX.md included, an instructions document past it left whole to INDEX.md as binding, to fetch (`Context.Dropped`), and the cap held on what is written — `RenderIndex` never past the room the files leave (the goal's item table cut at a row with the rest to `mesh_items`, then its statement and acceptance; at last a cut), `Context.fit` again in `WriteContext`, which measures what it rendered and writes nothing past the cap, and clears `.mesh/context` first, an empty context included; past the 40KB target (`ContextBytesTarget`) a session logs `ContextWritten.TargetWarning`; `doc_bytes_max` holds each instructions document),
  evaluate, plan and research sessions, the service install (`launchd` /
  `systemd --user`). A project's repos — for the planner, the director
  and `mesh goal plan` — are read from `project.repoIds` through
  `ProjectRepos`, each owned by its own account's login, never the
  org's account: an org links several GitHub accounts (the runsheet
  org's loom project lives at go-apis/loom). `internal/gateway/client.go`: the platform client.
  Every read — a query, and the `issueItemRepoToken`/`planGitHubToken`/
  `researchGitHubToken` mints — retries a transport error, timeout, 429
  or 5xx (`retryRead`: `ReadRetryFor` 2m, `ReadTryTimeout` 30s,
  `ReadBackoff`), every other mutation sent once; a spent window is
  `after N tries over D: …`, a claim-time read's item failed with it
  (ADR-028, a gateway read retries a blip).
  Its entity selections are generated (`fields_gen.go`, never hand-edited)
  from `schema.json` and `fields/<Type>.txt` (one name per line, sorted,
  `?` for optional): add a line, `go generate ./internal/gateway/...`
  (mesh-cli ADR-010). `ClaimWorkItemEvaluation(ctx, itemID, nodeID)` is
  the claim a node that did not build an item makes to evaluate it,
  modelled on `ClaimWorkItemRebase`: the same node again converges,
  another node or an item not `evaluating` is refused with a
  `gateway.Error` (`IsRefusal`); the `gatewaytest` fake takes it the
  same way, recording it in `Calls` and `Claims` as `claimWorkItem`
  does (goal `a-node-says-which-sessions-it-takes`). On a machine
  credential every `RegisterNode` carries `RegisterNodeInput.CredentialID`
  (`node.Registrar.CredentialID`, the token's machine id) as
  `credentialId` only when `TakesNodeCredential` says the schema has it,
  and a `not yours` refusal names the id sent (`node.NotYoursLine`;
  ADR-015's amendment 2026-10-07, runsheet#2290). The `gatewaytest`
  fake checks every `registerNode`, `heartbeatNode` and `claimWorkItem`
  input against `internal/gateway/gatewaytest/testdata/node-mutations.json`
  — per mutation and bearer kind (person or machine) the fields the
  gateway requires and refuses — refusing a miss or a refused field by
  name, and `TestNodeMutationsOnBothBearers` runs the node's register,
  heartbeat and claim on both kinds (mesh-cli ADR-026).
  A change to a node mutation's rule regenerates node-mutations.json in runsheet (specs/contracts/, from gateway/scope_node.go, checked by TestContracts) and arrives here through the schema-refresh pull request;
  `genfields -refresh-rules` rewrites the copy — never hand-edit it
  to make a test pass. The loop's
  `evaluateHeld` makes it for every `evaluating` item before the
  evaluation starts — the builder claims its own item's evaluation too,
  the platform converging for the same node, so the claim is the one
  lock and two nodes never both evaluate an item — re-reading the item
  after it, a refusal ordinary and silent; a refusal on an item this
  node built means another node has it: nothing starts, and it is said
  once per item, `item <goal>/<seq>: evaluation is node <x>'s; not
  evaluated here` (`Loop.sayEvaluationElsewhere`). Only when the
  `Mutation` has it (`TakesEvaluationClaim`, kept an hour), else own
  items only with no claim, said once per run `the platform takes no
  evaluation claim; evaluating only what this node holds`; an adopted
  evaluation is not claimed again (ADR-025, amended 2026-10-14). A
  `not yours` before the session leaves the item to its holder: the
  evaluator's `issueItemRepoToken` refused `not yours`
  (`evaluationNotYours`) starts no session and sends nothing, logged
  `item <label>: evaluation is another node's (<err>); left to it`, and
  an `evaluateWorkItem` refused `not yours` with no `mesh_verdict` call
  is the race lost after a session ran — logged once, `verdict refused:
  another node holds the evaluation`, the item forgotten, never retried
  (ADR-001's and ADR-023's amendments). `evaluate_one_node_e2e_test.go`
  ticks two loops — the builder and an evaluate-only node — on one fake
  gateway, in both orders and at once, and sees one `evaluate on`, one
  `evaluateWorkItem` and no `not yours` (goal
  an-items-evaluation-runs-on-one-node).
  `internal/channel/`: channel mode (one session per org, turns).
  `internal/meshmcp/`: the stdio MCP server (`Server`, `Tool`,
  `ObjectSchema`) channel mode and `mesh mcp` share — only channel mode
  sets `Server.Channel`, advertising `claude/channel`; `mesh mcp`'s
  initialize answers `serverInfo.name` `runsheet` (`meshmcp.ServerName`),
  channel mode's stays `mesh` (`channel.ServerName`, the
  `--dangerously-load-development-channels server:mesh` flag's key;
  ADR-023's amendment 2026-10-17) — and `mesh mcp`'s
  personal tools (`NewServer`, `Personal`: `mesh_goals` and
  `mesh_status` read the gateway and `~/.mesh/mcp-state.json`,
  `mesh_join` claims an item and makes its checkout in
  `~/.mesh/work/<goal>/<seq>` — or the cwd when it is the item's repo —
  on the item token, with the context written (`join.go`, `Join`), and
  the work tools on mcp-state's held items (`work.go`): `mesh_status`
  (path, branch, `uncommitted`, `commitsPastBase`), `mesh_progress`
  and `mesh_ask` (`reportWorkItemProgress`, the question opening
  `question: `), `mesh_finish` (`Git.Push`, then `finishWorkItem`),
  `mesh_release` (`releaseWorkItem`) and `mesh_switch` (push, note,
  release `switched`, join); a gateway refusal is the answer `<tool>
  refused: <reason>`; `pair: true` on an item another node holds joins
  its branch with no claim, `paired` in the state, and a push git
  refuses because the branch moved (`node.PushRefusedError`) is
  `push refused: …; run git pull --rebase in <path> and finish again`,
  the item still held — ADR-023 Pairing; the hosted work tools when
  `/mcp` serves them, `hosted.go`, ADR-023 Hosted path), served by
  `internal/commands/mcpcmd.go` on `platformClient` (ADR-023, mesh mcp).
  `skills/mesh/SKILL.md` is the `/mesh` skill Claude Code runs against
  these tools, embedded (`internal/meshmcp/skill.go`'s `Skill()`, by way
  of `skills/mesh/embed.go` colocated with the file) and written by
  `mesh mcp install` to `$CLAUDE_CONFIG_DIR/skills/mesh/SKILL.md`, else
  `~/.claude/skills/mesh/SKILL.md`, refusing to overwrite a file that
  differs unless `--force` (ADR-023, Skill). `mesh mcp install
  --client claude|codex|cursor|all [--dry-run]` works from one table,
  `internal/mcpinstall/clients.go`'s `Clients()` — each row a client's
  config dir, MCP file, format, entry keys or TOML table, instruction
  files and binaries (`Detect`) — `Plan` upserting each file
  (`Change`, never clobbering; an `*UnparseableError` printed `<path>:
  cannot parse (<err>); left alone`, exit 1) and `Apply` writing it
  (ADR-025, mesh mcp install is one table per client); the entry is
  `runsheet` (`ServerName`: `mcpServers.runsheet`,
  `[mcp_servers.runsheet]`), and after the upsert every other entry of
  the servers map that `IsRunsheetEntry` — a mesh binary's `mcp`/
  `mcp-relay`, or `/mcp` on `api.runsheet.dev` or the configured
  gateway (`HostOf`) — is removed byte for byte (`RemoveJSON`,
  `RemoveTOMLTable`, its own trailing comments through EOF), printed
  `<client>: removed <name> from <path>`, and a `LegacyNames` entry
  (`mesh`, then the built-in's old names, `tools.BuiltinAliases()`)
  pointing elsewhere kept,
  `<client>: kept <name> (not runsheet's)` (ADR-025, Legacy entries); `--remote
  [--url]` writes `RemoteEntry` for the gateway's `/mcp`
  (`node.MeshMCPURL`) instead, `DiscoverAuth` asked once — OAuth, the
  URL alone, when the issuer's metadata has a `registration_endpoint`,
  else a bearer naming `$MESH_TOKEN` (`BearerEnvVar`), never a token in
  a file (ADR-025, Remote). `mesh mcp doctor [--json]`
  first names who runsheet acts as, by runsheet ADR 056 and Latchkey
  ADR 013 (`doctorIdentity`: `auth.LoadCredential`, an `rs_*` token's
  user by `gateway.Client.Me`, a machine's operator — its `userEmail`,
  else `me` — `identity: <email> (person, via machine <id>)`, then
  `projects: <org>/<slug>, …` from `MyOrgs`/`Projects`; none is
  `identity: not signed in; run mesh auth login`, exit 1; a gateway
  error `identity: <email> (projects unavailable: <err>)`, not fatal;
  `mcpLoadCredential`/`mcpIdentityGateway` the tests' fakes), then
  (`mcpinstall.Doctor`, `Report`) reads the same table: per client
  found its entry `configured`/`missing`/`unparseable`/`duplicate`
  (runsheet's beside a legacy one, `Report.Duplicates`)/`legacy` (a
  legacy name alone) with the binary or URL, and each instruction file
  `current`/`stale`/`missing` — a marked section judged only between
  its markers — exiting 1 unless signed in and every report is
  `Healthy`; `--json` is `{"identity": {person, via, projects, error},
  "clients": [...]}` (ADR-025, Doctor).
  **A draining node claims nothing** (`Loop.Drain`): see the drain
  below. **A node low on disk claims nothing** (ADR-035,
  `docs/adr/adr-035-a-node-low-on-disk-claims-nothing.md`,
  `internal/node/diskfloor.go`): node.yaml's `disk: {free_min_gb,
  path}` (`DiskConfig`, `free_min_gb` default 10 GiB
  (`DefaultDiskFreeMinGB`), `path` default the loop's `WorkRoot`; a
  negative floor refused at load naming `disk.free_min_gb`; re-read
  each tick, `Loop.DiskThresholds`, logged `disk: free_min_gb <old> →
  <new>` and `disk: path <old> → <new>`) is read every tick, after
  `hostBackoff` and before any claim (`Loop.diskFloor`), through the
  quartermaster's `StatfsFunc` (`Loop.Statfs`, the real statfs when
  nil, injectable); free space under `FloorBytes()` skips every claim
  path — `evaluateHeld`, `rebaseRequested`, `planRequested`,
  `researchDue`, `validateBatches` and `claimReady` — running sessions
  untouched, logged once on entering, `disk_low: <path> <free> free <
  floor <floor>; claiming nothing` (at the first tick when the node
  starts under it), and once on leaving, `disk ok: <path> <free> free
  >= floor <floor>; claiming again`; a failed read keeps the last
  judgement, `disk: cannot read <path>: <err>; keeping the last
  judgement` once per error; while low `Loop.DiskFindings()` is one
  critical `disk_low` Finding (subject the path, bytes the free space)
  for the quartermaster's report once its loop wiring folds it in;
  `disklow_e2e_test.go`'s `TestDiskLowE2E` runs it on `Loop.Run`. **Capacity counts the live sessions** (ADR-029,
  `internal/node/liveproc.go`): `Loop.Live` (`LiveProbe{Systemctl,
  Root, Find, SweepEvery}`) counts the build sessions alive on the host
  each tick — active `mesh-session-build-*` scopes, else
  `FoundSessions` of kind build whose pid is alive — and `free =
  Capacity() − max(Running(), live)`, orphans included, logged once per
  change `capacity: <n> live agent sessions (<m> orphans) against
  capacity <c>; claiming nothing` (`Loop.LiveSessions()`); every
  `SweepEvery` ticks (default 5) an untracked live build whose item is
  terminal, unknown, or another node's while a state file of ours names
  it is stopped (`judgeItem`, `stopScope`, `FoundSession.Kill`), logged
  `capacity: orphan: stopped <unit> (…)`; a sibling node's scope on the
  same host is neither counted nor stopped. node.yaml's `claim: {unmerged_per_repo: N}` (`ClaimConfig`,
  re-read each tick, `Loop.UnmergedPerRepo`; absent or 0 is no limit
  and no read) holds fresh claims for a repo with N items in `review`,
  counted from the node's own listing, never `RepoIndex.reviewPile`;
  started work there is still claimed (ADR-017).
  node.yaml's `takes:` (`Takes`: a list of kinds or a per-kind cap map,
  re-read each tick, `Loop.Takes`; left out, every kind) skips a kind
  not taken before its claim path lists anything — `claimReady`,
  `evaluateHeld`, `rebaseRequested`, `planRequested`, `researchDue`
  return at `takesKind` — logged on change `takes: <old> → <new>`;
  a kind's cap (`Takes.Cap`) bounds it inside capacity — `runningOf`/
  `kindRoom`, the room read once per path and lowered by each start, a
  build's `free` cut to it in Tick — and what it holds back is passed
  over as `at node.yaml's takes cap of N <kind>(s)`, logged `takes:
  <kind> at its cap of N; M waiting` once a tick (ADR-025).
  **Every org gets its share** (`internal/node/shares.go`, ADR-023,
  amending ADR-017's order): `claimReady` orders the pooled ready items
  by `shareOrder(ready, Floors(Capacity(), Scopes, Weights()),
  heldByOrg())` — floor first, then priority: an org holding fewer
  sessions than its floor with ready work has its best item (by
  `claimOrder`) claimed before any other org's whatever the priority,
  the org furthest below first, then everything left in `claimOrder`.
  `Floors` splits capacity by weight (keyed by the org's `Name()`,
  missing or zero is 1, nil equal shares; `Loop.Weights` is node.yaml's
  `shares:` (`Config.Shares map[string]int`, keyed by a served org's
  login or slug, re-read every tick like the claim limits and logged
  on change as `shares <old> → <new>`, each side sorted `a 3, b 1` or
  `none`; a zero or negative weight refused at load naming the org,
  `shares: <org>: must be a positive integer`; a weight naming an org
  this node does not serve logged once, `shares: <org> is not a served
  org`, and dropped), over by the owner answer's `shares` when the
  platform's schema carries it (a later item of the goal)), the largest
  remainder to the orgs in listed order. `Loop.sessionOrg` (subject →
  org) is set where builds, plans and research are marked —
  `start`/`Adopt`, `startPlan`/`startResearch`, `AdoptSessions` by the
  state's node id — and cleared with them; `heldByOrg` counts those
  three, never evaluations or rebases. A claim for a floor logs
  `<org>: claimed item N "title" (below its floor of F, holding H)`. Each
  pass's rows per org — `gateway.HoldingOrg{org, orgId, floor, held,
  ready}`, ready -1 when the org's read failed — ride the holding report
  as `holding.orgs` (only when the gateway's holding input takes `orgs`,
  `takesHoldingOrgs`) and `Loop.Shares` → `RunState.WriteShares` →
  `~/.mesh/node.shares`, printed by `mesh node status` as `ORG FLOOR
  HELD READY` (`sharesLines`) or `shares: none recorded` (ADR-023,
  Reporting).
- **A failed check resumes the session that built it** (ADR-030,
  `docs/adr/adr-030-a-failed-check-resumes-the-session-that-built-it.md`):
  every build keeps its session in `ResumeStore` (`RunState.Resumes()`,
  `~/.mesh/resume/<item>/`: `ResumeRecord{sessionId, adapter,
  credentialId, worktree, transcript, keptAt}` and the transcript's copy,
  ids and paths only; `Runner.keepSession` before the account and pod
  go; `Sweep` forgets merged/cancelled/failed/superseded items or past
  `ResumeKeepFor`, 7 days). A failing validation step (ADR-014) and a
  claim whose last evaluation failed (`retryAfterEvaluation`,
  `internal/node/retryturn.go`) resume it — `Job.Resume`, the
  transcript restored by `harness.RestoreTranscript`, same adapter,
  credential and worktree (`Runner.resumable`) — with one message,
  `harness.RetryTurnPrompt` (what failed, where, the acceptance); else
  a fresh session gets the build prompt and the same message, logged
  and noted `item <label>: session not resumed (<reason>); fresh session
  with the failure` (`no record`, `adapter changed`, `credential
  changed`, `worktree gone`, `transcript missing`, `session not found`).
  Each retry turn is `Loop.NoteRetryTurn`'d: the heartbeat's `resumed
  [{itemId, kind, sessionId, fresh}]` (`takesResumed`) and one session
  in `Loop.Running`, each item once. `internal/node/resume_e2e_test.go`
  runs it end to end.
- **A death names itself** (`internal/crash/`): `defer crash.Guard()`
  at the top of `mesh node start` and of every goroutine the node
  starts turns a panic into one node.log line, `mesh node: fatal:
  <value> (version <v>)`, the stack, and exit 70 (`crash.ExitPanic`),
  never Go's 2 (systemd's `INVALIDARGUMENT`); a new node goroutine
  defers it first (goal node-yaml-takes-effect-or-says-why-not, The
  04:00:22Z exit).
- **The director** (`internal/node/research.go`, runsheet trd §6c): a
  project carries **directions** — several, each its own aggregate with
  its own words, window, timezone, gap, ceiling and research claim
  (`Directions(org, project)` lists them; the id is
  `direction:<project>:<slug>`). On every tick the loop offers each live
  direction with room to spare — `ClaimDirectionResearch`; the platform
  refuses it outside the hours, inside the gap, when the direction is
  off or archived, or when another node holds it, so *a refusal is the
  ordinary answer and is not logged as an error*. A session that wins
  the claim opens its credential first (`Credentials.Open`, before
  `ProjectRepos` and any `WS.Fetch`): a refused read — none issued
  (`ErrNoCredential`) or none with headroom (`ErrNoAccount`) — logs
  `<project>/<direction>: <err>; handing in an empty research` and hands
  in an empty research costing nothing, no usage, its notes `no issued
  credential has headroom` or the no-credential text, so the direction
  is not left claimed while the hold keeps it unclaimed. Then it reads
  the direction's own GitHub tokens (`researchTokens`, adr-022) — one
  per repo of the project, each checkout `Authorize`d on its row before
  `Fetch`, the session's `GH_TOKEN` the first sorted repo's with its own
  `GH_CONFIG_DIR`, the operator's `gh` never consulted — and a refused
  read, or a repo with no row, hands in an empty research naming it
  (`refusedToken`) the same way a refused credential does; it then
  fetches the repos and reads the context; a `propose`
  direction's session pitches and answers (`ParsePitches`: problem,
  evidence, bet, scope, estimate, risks, alternatives, signal, and its
  answers to open questions on earlier pitches) and a `set` one
  proposes goals (`ParseProposals`); `RecordDirectionResearch` hands
  them in with the cost — `pitches` and `answers`, or `proposals` — and
  the platform decides what becomes of them. An older platform without
  `pitches` gets the pitches as proposals (`ErrNoPitches`), no error,
  said once per run (`takes no pitches`). A session whose output cannot
  be read hands in an empty research anyway — otherwise the claim holds
  the direction for 45 minutes and the night is lost. **Two ceilings
  hold and `RoomFor` takes the smaller**: the direction's own
  `maxLiveGoals` less the live goals it set (`Goal.DirectionID`), and
  the project's `maxLiveGoals` — its total over every direction, on the
  autonomy policy beside `replanCap` — less every live goal on it. The
  guard is by the direction's `mode` (runsheet ADR 022,
  `Direction.Proposes`): a `set` direction — or one with no mode, as an
  older platform serves it — with no room is not offered at all, since
  every goal it set would be dropped; a `propose` direction is offered
  whatever the room, since its pitches (or proposals) wait for a person
  and take no live-goal slot, and its session is told the room as
  information (`ResearchInput.Proposes`). Each direction works in
  its own checkout, `ResearchDir(root, project, direction)`, because two
  of one project may be inside their hours at once.
- **The drain** (`Loop.Drain`, trd §3 Drain): `SIGUSR1` or `mesh node
  drain` (which signals the pid in `~/.mesh/node.pid`) stops every new
  start — items, plans, research, evaluations, rebases — and `Run` returns nil
  once no session of *any* kind is left (planners and evaluators do not
  count against capacity, so `Running()` is not the exit test); the
  process exits 0 and its service starts the new binary.
  `~/.mesh/node.draining` marks it for `mesh node status`, and every
  heartbeat says `draining`. Drain is for taking a machine out of
  service, not for upgrades: **an upgrade restarts at once and adopts**
  (`Loop.Restart`, adr-002's amendment) — `SIGHUP` or `mesh node
  upgrade` cancels every context the node runs sessions in
  (`Loop.Context`), each `Job.Outlive` session detaches, `Run` returns
  `ErrRestart` without waiting and `mesh node start` execs its binary
  again; the new process adopts. `DrainAndWait(ctx)` is a drain that
  blocks until every session ends — the loop's self-update no longer
  calls it, sandboxed or not; `Resume` undoes a drain
  that has not yet ended `Run`. The heartbeat's extras (`gateway.HeartbeatExtras`: `draining`,
  `updateError`, `limits` — the latest Claude plan limits per
  account, each with the priorities the known account serves
  (`priorities`, empty for one serving all; sent only when the
  gateway's `NodeAccountLimitsInput` has the field, introspected and
  kept an hour like `scaling` — adr-005's amendment 2026-10,
  `limits[].priorities`, for the Nodes page's account gauges) — and
  `scaling` — a ranged node's `{capacity, min, max,
  reason, accounts}`, the capacity in force, node.yaml's range, why
  and each account's slots — and a fixed node's too, on any number of
  accounts, as `{capacity: N, min: N, max: N, reason: fixed}`, so a
  capacity changed in node.yaml reaches the platform on the next
  heartbeat and not only at the next registration (mesh-cli ADR-018;
  before it only a range or several accounts sent it) — and `host
  {load, cores, temperatureC, hot}` beside `accounts`, the machine's
  own load average, core count, package temperature and whether
  either crossed node.yaml's `host:` threshold, nil when the node's
  own isn't hot (`gateway.HostScaling`, adr-004's amendment
  2026-10-03; The node backs off a hot host) — sent only when the gateway's
  schema has the field (introspected, kept an hour: adr-005
  §Heartbeat, `host` the same way via `takesScaling`); both for runsheet goal
  `limits-on-the-console` — and `runtimes` and `adapters` — the tool
  runtimes (npx, uvx, docker) and agent CLIs (claude-code, codex)
  `Loop.LookPath` finds, probed every tick, for `tools-in-the-console`
  — and `commit` — the short sha the binary was built from
  (`Loop.Commit`, `version.Commit()`), beside `version`, which is
  `version.Resolved()`: the release tag when built from one, else that
  sha; sent only when the gateway's `HeartbeatNodeInput` has the field,
  introspected and kept an hour like `scaling` (mesh-specs ADR 013,
  adr-002's tag-or-sha amendment)),
  `adopted` (below) and `holding` — the node's holding decision as data,
  not a log line: each account's status, reset time and what the node
  still takes on it, the summary judgement (`HoldingReport`, adr-005's
  amendment, for runsheet goal `node-says-why-work-waits`), and each
  `holding.unclaimed[]` entry's own `until` beside its reason — the
  hold's own until when it has one, the earliest when several holds of
  a goal and kind fold into one entry, folded into the report's own
  `until` the same way accounts' `resetsAt` are, dropped from an entry
  whose schema's `unclaimed` element type does not take it
  (`takesHoldingUnclaimedUntil`, adr-005's amendment 2026-10-03) — are
  sent only when set,
  because they are fields the platform half adds. No reloadable field
  needs a drain: `Reload` (`Reloader` over `LoadConfig`) re-reads
  node.yaml every tick and the next tick runs on it — the capacity
  (`capacity N → M`, a range's clamp), the limits thresholds (`limits:
  pace five-hour a → b, stop c → d`), the accounts (`accounts a, b → a,
  c` for membership, and one `account <label>: <field> <old> → <new>,
  …` line per account kept whose kind, token_file, api_key_env,
  daily_cap, monthly_cap — as in force for its kind, an api key's defaults applied and a subscription's none — or project
  changed), the session budgets and project budgets (`budget
  <kind>/<tier> $4.00 → $7.00`, `project budget <project> none →
  $20.00`, the effective figures with defaults applied), the context
  budget (`context: bytes_max … → …, doc_bytes_max … → …`), the claim
  limit (`claim: unmerged_per_repo … → …`) and the orgs restriction
  (`orgs a, b → a`). Each change is logged once, at the tick it takes
  effect, with its old and new value; a field left alone logs nothing
  (`reload_fields_test.go`, a test per field from disk).
  A reload that cannot be applied — a node.yaml that does
  not parse, a failing `MyOrgs`, no org left to serve — logs one
  `reload config: <reason>; <reason>` line per tick while it lasts (the
  reasons joined with `; `, a cause's own newlines folded in, so it is
  one physical line) and keeps the last good values: a bad node.yaml
  keeps the capacity, limits, accounts, context and claim limits, a
  failed `MyOrgs` keeps the orgs and still applies a good node.yaml.
- **Adoption** (`internal/node/adopt.go`, mesh-cli ADR-009, mesh-specs
  ADR 008): a session's unit and output file own it, not the node
  process. Build, evaluate, plan, research and rebase jobs are
  `Job.Outlive`: a node shutting down detaches (the unit runs on, its
  `<workdir>.session` and temp dirs stay) and only the job's own
  timeout stops it. On `mesh node start`, before the first tick,
  `FindSessions` walks the work root for `state.json` naming one of the
  node's ids — then `SweepScopes` (`sweep.go`, ADR-008 The start-up
  sweep) stops every other active mesh-session scope whose item or goal
  is terminal or unknown on the platform (`SessionLive`, `GoalLive`; a
  read error leaves it; `mesh node status` lists the scopes the same way
  by `ScopeReport`, `sessions: <n> live scope(s), <m> leaked`) — and `Loop.AdoptSessions` takes each up: marked as the
  loop's own (a build in `Running()`, every kind in the drain's
  `sessions()`, never claimed or started again, and `Loop.Adopt` skips
  it), the item heartbeated at once so the stall window never elapses,
  then `AdoptWith` → `Runner.AdoptItem`, `Evaluator.AdoptEvaluation`,
  `Planner.AdoptPlan`, `Director.AdoptResearch`, `Rebaser.AdoptRebase`:
  `ClaudeCode.Attach` follows `output.jsonl` from the saved offset (the
  offset is saved after every event) with the usual heartbeat ticker,
  and the end goes through the *same* tail as a live session —
  `afterBuild`, `afterEvaluate`, `readPlan`/`handIn`, the director's
  `handIn`, `afterRebase`. A session whose pid is gone is read from its
  file: a result finishes it, none fails it with its last text. The
  session dir goes (`harness.RemoveSession`) only once the end is
  handled. The heartbeat's optional `adopted [{itemId, kind, since}]`
  lists them while they run. **A sandboxed session outlives the node
  in its pod** (ADR-009's and ADR-007's amendments 2026-10-13, goal
  a-sandboxed-session-outlives-the-node): with `ClaudeCode.DetachSandboxed`
  (set by `mesh node start` with a sandbox) an `Outlive` job's agent
  runs detached (`Session.StartDetached`, `podman run -d` named
  `sandbox.AgentName(pod)`, its prompt in `<workdir>.session/.prompt`,
  removed after the first event, its streams `output.jsonl` and
  `stderr.log`), `state.json` `runner: podman`
  (`sessionunit.SandboxRunner`), pid 0, `unit`/`container` the agent's
  container, `pod` and `home`; a cancel `Session.Detach`es, leaving pod,
  sidecars and HOME. `FindSessionsIn(root, ids, sb)` judges a sandboxed
  session by its container (`AgentState`; `Kill` = `StopAgent`;
  `Remove` = `harness.RemoveSandboxedSession`, the pod, HOME and
  session dir), `ClaudeCode.Attach` follows it by its container,
  `Runner.AdoptItem` reattaches pod and HOME (`Sandbox.Reattach`, else
  `ReattachHome`; `harness.WithPodKept`) so validation runs there and
  both go after the finish, `SweepSandbox` keeps found sessions' pods,
  containers and HOMEs (`sandbox.SweepKeeping`), and `LiveProbe.Sandbox`
  counts a sandboxed build by its container.
  `sandboxed_swap_e2e_test.go` proves an upgrade mid sandboxed build
  finishing on the new node. Codex sessions still end with the node.
  **Claims outlive the node too** (`claims.go`, ADR-009
  Claims): a plan or research claim is recorded as a `ClaimRecord` in
  `<root>/.claims/` (`Loop.ClaimsDir`) the moment it is granted and
  removed once its session's end is handled (kept when the context is
  done); after `AdoptSessions`, `FindClaims` + `Loop.ReopenClaims`
  re-claim each record with no found session by the same node id, log
  `re-opening the plan claim of goal <slug> held since <t>` (or `the
  research claim of <project>/<direction>`) and run it fresh; a refused
  re-claim removes the record and says why.
- **Self-update** (`internal/node/selfupdate.go`, `internal/selfupdate/`,
  adr-002): after each heartbeat the loop reads the node's desired
  version (`GetDesiredVersion`; empty, the running one or `dev` does
  nothing). A different one is fetched and verified (the signed
  `SHA256SUMS` of the tag release) first, then the node swaps its
  resolved executable keeping `<path>.prev` and `Run` returns
  `ErrRestartForUpdate` at once — no drain, its sessions left for the
  new binary to adopt — so the process exits 0 for its service to start
  it — a sandboxed node too, its line ending ` (sandbox: on)`, its pods
  carried across (ADR-002's amendment 2026-10-13). A refusal heartbeats `updateError`
  and waits an hour on that version. `mesh node start` counts starts in
  `~/.mesh/update.json` (`CheckStart`): the third unconfirmed one rolls
  back to `.prev`, reports it and exits 0; the first heartbeat confirms
  — unless it serves fewer orgs than `update.json`'s `served` (the
  names `Begin` recorded at the swap): that is a failed confirm,
  `*selfupdate.FewerOrgsError` (`serving fewer orgs than before the
  update: before grapevine, after none`), logged and recorded like a
  failed heartbeat and sent as `updateError` (adr-002's amendment
  2026-10-03, an update that serves fewer orgs is not confirmed).
  The loop's `Update` is an interface; tests inject a fake.
  **`update.json`'s `State` carries why, not just that**: `startError`
  (`NoteStartFailure`) is why the last start of the pending update ended
  before it could confirm — a config it could not read, a panic, an
  exited process — and `confirmError` (`NoteConfirmError`) is the last
  error a heartbeat hit while unconfirmed — a refusal or a timeout, the
  platform being slow rather than the binary failing to start. Neither
  affects `Pending()`, and `Confirm` clears both. A confirm error naming
  an id (`registers at <uuid>`, `selfupdate.NamesID`) sets
  `retryAfterAdopt`, which `Rollback` keeps: that rollback is not final
  (`StartCheck.RetryNow`, its `Reason` ending ` (owner id adopted; the
  update is tried again)`) and the loop tries the version again on the
  next tick, once (`NoteUpdateFailure(…, final, retryNow)`; adr-002's
  amendment 2026-10-03). `Rollback` takes the
  confirm window as a parameter and names the real cause when one is
  recorded — `'<to> did not start: <startError>'`, or `'<to> did not
  confirm within <window>: <confirmError>'` — falling back to the old
  `'<to> did not start; rolled back to <from>'` when neither is set.
  `node.StartCheck` (from `CheckStart`) carries `Pending` and `Attempts`
  so the caller can say what it is confirming: node.log's first lines,
  logged by `node.StartLines` immediately after `startUpdates` and
  before `LoadConfig`, `platformClient` or any platform call, name the
  version, the pid, the config path and either `'confirming update <from>
  → <to>: start <n> of 3, rolls back on the third'` or `'no update
  pending'` — so a silent exit before registration can never happen
  again. `internal/commands/nodecmd.go`'s `registerNode` is everything a
  start needs before the loop exists (`LoadConfig`, the accounts'
  secrets, `platformClient`, the work root, registration); an error or a
  panic there while an update is pending is recorded with
  `NoteStartFailure` (a panic is re-raised after recording). node.yaml's
  `update.confirm_within` (`Config.Update.ConfirmWithin`,
  `Config.ConfirmWithin()`) is how long a new process keeps trying to
  confirm before giving up — default 5 minutes
  (`selfupdate.DefaultConfirmWithin`). **The confirm window** opens at
  process start (`Loop.Started`): while an update is pending
  (`Loop.Confirming` = `StartCheck.To`), `registerNode` retries a failed
  registration (60 s per attempt) on the heartbeat interval
  (`confirmWindow.retry`) and the loop keeps heartbeating, each failure
  logged `'self-update: confirming <to>: heartbeat <org>: <err>; retrying
  for <left> more'` (a timeout adds `(the platform is slow)`,
  `PlatformSlow`) and recorded by `Updater.NoteConfirmError`. The first
  heartbeat through confirms. The window elapsing with none through is
  `ErrUnconfirmed` (`*UnconfirmedError`, naming the last error) from
  `registerNode` or `Run`; `endRun` logs `'self-update: <to> did not
  confirm within <window>: <err>; exiting for the service to start it
  again'` and the process exits 1 — the start count rolls back on the
  third, naming that error. Only a failure to *run* (`NoteStartFailure`)
  is "did not start"; a slow platform never is. With no update pending
  a failed heartbeat or registration is as before. `mesh node upgrade`
  prints the window and `~/.mesh/node.log` after the restart.
  `mesh node update [version]` (`internal/commands/nodeupdate.go`) does
  it by hand on the same `ReleaseUpdater`: verify, drain the pidfile's
  node, swap; `--verify-only` verifies into a temp dir. `mesh node
  upgrade` (`internal/commands/nodeupgrade.go`) is the upgrade without
  a drain: `--version`, the latest release, or `--binary <path>` (once
  `<path> version` answers), swapped keeping `.prev` for the rollback,
  then `systemctl --user restart` of the unit, `launchctl kickstart -k`,
  or `SIGHUP` to the pidfile's node; a sandboxed node only with
  `--force`. The systemd unit's `KillMode=process` and the
  LaunchAgent's `AbandonProcessGroup` keep a service stop off the
  sessions. **The node
  image** (`images/node/Containerfile`, embedded as
  `nodeimage.Containerfile`; `internal/sandbox/image.go`, mesh-cli
  ADR-007): both ensure the *new* version's image after the verify and
  before the drain (`Loop.Image`, `nodeUpdate.Image` →
  `SandboxConfig.EnsureImage` → `sandbox.EnsureImage`/`BuildImage` on
  an injected `Runner`): pull `ghcr.io/runsheet/mesh-node:<version>`
  (node.yaml `sandbox.image`: empty that, `build` built here, else as
  given), prune older tags but the running one's. The pull is on the
  platform's registry credential: `RegistryBrokerOf` asks
  `nodeRegistryToken(registry)` for the image's registry
  (`sandbox.RegistryHost`) and `pullImage` passes it by `--authfile` on
  a temporary `auth.json`, removed after the pull; a platform without the
  read, or none for the registry, pulls on podman's own login (ADR-007's
  amendment 2026-10-13). A failed pull is an
  `updateError` — a refused one opening `image_pull_refused: `
  (`kindedText`, ADR-030): no drain, no swap, never a silent build. `sandbox:
  off` pulls nothing. release-tag.yml's `image` job pushes and
  cosign-signs it before `publish`. The image carries, beside Go, Node
  LTS with npm, Python, `gh` and the agent CLIs, a Postgres 17 server
  (`initdb`/`pg_ctl`/`psql` linked into `/usr/local/bin`, the sandbox's
  own PATH; no cluster made) and Debian's headless `chromium` — what
  latchkey's conformance, the console's `npm run check` and runsheet's
  integration shard need — proven inside the built image by
  `scripts/image-tools-check.sh` (`TestImageToolsE2E`, `-tags e2e`;
  `docs/e2e-sandbox.md`).
- **The pace** (`internal/node/limits.go`, trd §3 Pacing by the plan's
  limits): every session's `rate_limit_event` lands in `Loop.Limits`
  (latest per account — its `MESH_ACCOUNT` label, else
  `CLAUDE_CONFIG_DIR` or `default` — fed through `ObserveLimits` around
  each one-shot harness and the channel session's `OnRateLimit`), and
  `pace` judges each account: *paced* at `limits.pace_five_hour` of the
  five-hour window (urgent/high work only, no research), *stopped* at
  `limits.stop` (0.98) of either window or a status not `allowed`
  (nothing on it) until the window's reset. **Each account is also
  paced against a clock, not just the brake** (goal
  every-subscription-lands-full-at-its-reset): `Standing`/`StandingOf`
  turns a reading into each window's target line — its elapsed fraction
  less `limits.reserve` (default 0.05), or the credential's own pace
  point where it has one — and the account's `Distance` from it,
  negative **behind** the line (room to spend) and positive ahead of it
  (spent too fast); `Push` marks an account still behind with a fifth or
  less of the window left before reset. Every start picks its account
  (`ChooseAccount`, below); the node's own mode (`nodePace`, the most
  lenient subscription) halves the capacity when paced, and a crossing
  is logged once. A Codex session's readings land under
  `codex:<account>` (`CodexLimitsAccount`, `ObserveLimitsAs`):
  `Limits.Readings` leaves them out, so they never pace, stop, choose,
  scale or stand against the clock; the heartbeat carries them.
- **The scale** (`internal/node/headroom.go`, `scaling.go`, adr-004):
  a node.yaml `capacity: {min, max}` runs the `Scaler` once a minute
  inside the tick (`Loop.scale`, by `Loop.Now`) on the highest
  utilization across accounts per window, stepping the capacity in
  force one slot per 5 minutes; a shrink only stops claiming, at min
  only urgent/high items are claimed, and Reload clamps into a new
  range at once. `Loop.Scaling()` is what the heartbeat's `scaling`
  carries, and `Loop.Scaled` leaves it in `~/.mesh/node.scaling` for
  `mesh node status` (`in-force/min–max (reason)`). A
  plain `capacity: N` runs none of it. The hard stops of the pace stay
  above it. A ranged node that lists `accounts:` runs the
  `AccountsScaler` instead (`capacity.go`, `spending.go`, adr-005
  capacity across accounts): each subscription by its own windows, each
  api key by its money left under `daily_cap`/`monthly_cap` and the
  project's `project_budgets`, from the spend ledger
  (`ledger.go`, `~/.mesh/ledger.json`, local time, fed by
  `ObserveSpend`). **The push**: a subscription window behind its
  target line (`Standing`'s `Push`, above) with a fifth or less of its
  length left affords `(stop − utilization) ÷ minutes to reset ÷ burn`
  in place of the ordinary target's rate, and at least one slot more
  than unpushed, so it lands near full at reset instead of wasting the
  window's last hours (`ReasonPush`, `capacity.go`; logged once per
  crossing, `capacity: pushing <label> (…)`, `Loop.Pushed`).
  `Loop.spendable` narrows every choice — no key at
  its cap, the last 10% of a key's day for urgent/high, and for an item
  only an account with a slot free. **A hot host backs it off**
  (`hostbackoff.go`, ADR-023, amended by ADR-031): every tick, while
  `MachineReading.Hot` at node.yaml's `host:` thresholds — the node
  user's slice's CPU pressure past `host.cpu_some_avg60_max` (default
  25, `host hot: cpu pressure 31%/25%`) when it is read, else load per
  core past `load_per_core_max`, then the package temperature — and,
  once hot, until the pressure or load falls below `host.quiet_fraction`
  (0.8) of its threshold, `claimReady` is skipped and the capacity in
  force drops one slot per `HostStepEvery` (1 minute; to max(min, 1), a
  fixed node's 1);
  quiet, it climbs one slot per `HostRecoverEvery` back to the plan
  figure, `Scaling.Host` carrying the reading; the scalers keep only
  the free-memory cap (`low memory`). **An overloaded host starves a
  session, and the item is not blamed** (ADR-029,
  `docs/adr/adr-029-a-session-the-node-gives-up-on-is-stopped.md`):
  `MachineReading.Overloaded(HostConfig)` is true past node.yaml's
  `host.overload_per_core` (default 1.0, `PerCore()` = load1 ÷ cores)
  or, when set, `host.overload_cpu_some_avg60` (`/proc/pressure/cpu`),
  its reason `host overloaded: load/core 1.44 (138.2 on 96 cores), cpu
  some avg60 71%`, and the heartbeat's `scaling.host` carries `perCore`
  and `cpuSomeAvg60` when the schema takes them; a build whose stream
  (the last event passed on, or `output.jsonl`'s last write) has said
  nothing for `host.starve_after` (default 15m, `HostConfig.StarveAfter`,
  reloaded and logged `host: starve_after <old> → <new>`) while the host
  reads overloaded at that tick (`internal/node/starve.go`,
  `checkStarved` after `checkClaim`; `Runner.Machine`, `Runner.Host`) is
  cancelled with `ErrStarved` (an `ErrStopSession`: unit stopped and
  killed, pod closed, slot freed) and `afterStarved` sends
  `ReleaseWorkItem` noted `stalled: <reason>` — a release, no attempt
  charged — logged `item <label>: stalled on an overloaded host;
  released: <reason>`; a silent build on a host not overloaded runs on.
  A claim lost while the host is overloaded names it in `afterLost`'s
  line, `claim lost (item is ready; host overloaded: …)`.
- **The quartermaster keeps the node fit** (`internal/node/quartermaster*.go`,
  ADR-034, `docs/adr/adr-034-the-quartermaster-keeps-the-node-fit.md`):
  `Loop.Quartermaster` (`Quartermaster{Checks, Every}`) runs from `Tick`
  after the heartbeat, its checks only once per `Every`
  (`DefaultQuartermasterEvery`, 10m; `RunIfDue`), each `Check` yielding
  `Finding{Kind, Severity ok|warn|critical, Subject, Detail, Bytes,
  Reclaimed, At}`, a panicking check one `critical` `quartermaster`
  finding. node start's four (`nodeQuartermaster`, nodecmd.go): `disk`
  (`DiskCheck` over the work root, /tmp and home, by device; warn <15%
  free, critical <5%), `scratch` (`ScratchCheck`: `runsheet-*`,
  `latchkey-*`, `go-build*`, `mesh-*` dirs under /tmp of the node's uid
  past `ScratchMinAge`, 6h, reaped — never a symlink, another uid's, an
  `InUse` path or `mesh-home-*`, left to `SweepSandbox` run each time
  keeping found sessions; warn past 20G left), `dead_checkout`
  (`DeadCheckoutCheck`: an item worktree whose item is merged, done,
  already_done, cancelled, superseded or unknown, idle `DeadCheckoutIdle`
  (1h) and not `Loop.LiveCheckout`, removed by `Git.RemoveWorkspace`,
  gone worktrees pruned; a platform that cannot answer removes nothing,
  warn) and `version` (`VersionCheck`: warn when the desired version has
  differed an hour or a dev build runs where a release is desired,
  critical when the same one has failed to update for two). Each run's
  findings go to `Loop.Findings` → `RunState.WriteFindings` →
  `~/.mesh/node.findings`, printed by `mesh node status` as `findings:`
  and `  <kind> <severity> <subject>: <detail>` lines, critical first
  (`node.FindingsLines`), or `findings: none`; each non-ok one is logged
  `quartermaster: <kind> <severity> <subject>: <detail>` once per change
  and each reclaim `quartermaster: reclaimed <bytes> (<kind>)`; the
  heartbeat's `findings` (`gateway.Finding`) only when
  `TakesHeartbeatFindings` (introspected, kept `ScalingProbeEvery`),
  else said once, `platform has no heartbeat findings; kept on the
  node`. It replaces the dl360's `/etc/cron.d/mesh-tmp-clean` and
  `/etc/cron.hourly/mesh-home-sweep`. `quartermaster_e2e_test.go`
  (`-tags e2e`) runs a node full of leftovers end to end.
- **Held goals are a record** (`internal/node/held.go`, goal
  a-refused-plan-is-released-not-replanned): `HeldGoal{GoalID, Slug,
  Org, Kind, Reason, Since, Until}` is a goal this node holds off
  claiming for — a plan claim released by a refusal that keeps
  repeating, or an item claim refused for budget, pause or approval,
  held until the goal's row changes — `Kind` `plan` or `item`.
  `Loop.NoteHeld` replaces any entry of the same `GoalID` and `Kind`
  and logs `held: <org> goal <slug> (<kind>): <reason>` the first time
  a goal and kind is held, not on a later update of the same one;
  `Loop.ClearHeld` removes it and logs `held: <org> goal <slug>
  (<kind>) released`; `Loop.HeldGoals()` is every entry, sorted by org,
  slug, kind. `Loop.Held func([]HeldGoal)` is told them every tick, as
  `Scaled` is told the capacity in force; `mesh node start` sets it to
  `RunState.WriteHeld`, which leaves them atomically at
  `~/.mesh/node.held` beside `node.scaling` (an empty list removes the
  file, as an unranged `WriteScaling` does), read back by
  `RunState.Held`; `Stopped` removes it. `mesh node status` prints them,
  while a node runs here, as a **Held goals** table — org, goal, kind,
  since, until, why — or `Held goals: none.` with nothing held.
- **The rebase** (`internal/node/rebase.go`, runsheet ADR 008): an item
  whose pull request conflicts in a way the integrator could not
  resolve itself carries `rebase_state: requested`. Every tick the loop
  lists them in each served org (`RequestedRebases`), in goal priority
  then seq (`rebaseOrder`), and claims each with `ClaimWorkItemRebase`
  — a refusal is the ordinary answer, not an error. The session works
  in the item's own checkout on its token, rebases onto the base,
  re-runs the acceptance and pushes with `--force-with-lease`; the node
  reads the pushed head back (`Workspacer.PushedHead`) and hands it in
  with `FinishWorkItemRebase`, or `FailWorkItemRebase` with the reason
  for *anything* else but `ErrNoCredential` (a wait: the claim lapses)
  — a held claim waits out the platform's 15-minute timer. Its heartbeat and its cost are
  `ReportWorkItemProgress`. Like evaluations, rebases do not count
  against capacity. **The platform does not yet issue the item's token
  to the rebase holder** (runsheet `gateway/itemtoken.go` refuses an
  item in `review`; ADR-001 asks for it), so until it does every
  rebase fails at `github token:` and goes to its builder; the fake
  mirrors that unless a test sets `RebaseTokens`.
- **Accounts** (`internal/node/accounts.go`, adr-005): node.yaml's
  `accounts:` ({label, kind subscription|api, token_file|api_key_env})
  are the Claude accounts sessions run on; none is the operator's own
  login, unchanged. `OpenAccount` → `PrepareAccount` gives a session
  the secret in its env only, a temp `CLAUDE_CONFIG_DIR` removed by
  `Cleanup`, `Job.Unset` of the inherited auth variables and
  `Job.Secrets` to redact; `MESH_ACCOUNT` makes `Account` key limits by
  label. **Which account** (`internal/node/select.go`, adr-005, goal
  every-subscription-lands-full-at-its-reset): before every claim —
  item, plan, research, rebase — and every evaluation the loop calls
  `ChooseAccount` (subscriptions ordered by `Standing`'s `Distance`
  behind their target line, furthest behind first; an account ahead of
  its line is chosen only once every other fitting subscription is also
  ahead or stopped, then the least ahead of them; an account never read
  stands as full headroom; an api account only for urgent/high work or
  when every subscription is stopped and work waits) and hands the
  choice to the session on its context (`WithAccount` → `OpenAccount`).
  **The holdback**: once every subscription serving the priority but
  one sits within 10% of `stop`, the one with the latest reset is
  **held back** — excluded from `ChooseAccount` below urgent priority —
  so the node is never left with every subscription stopped at once
  (`heldBack`; logged `accounts: <label> held back for urgent work
  (the others within 10% of stop); <n> item(s) left unclaimed until
  <earliest reset>`). **An account serves named
  priorities** (`AccountConfig.Priorities`, `Serves`; empty serves all,
  research's `""` runs anywhere): `ChooseAccount`, `accountToFinish`'s
  fallback and `codexChooser` never pick one for work it does not
  serve, whatever its headroom — nor does `Credentials.Open` given no
  chooser, which takes the session's priority from ctx (`withPriority`:
  an evaluation on Codex room, a held item `Adopt`ed by its goal's
  priority; `priorities_loop_test.go` proves it per session kind) — and an api key's "every subscription
  stopped" is judged over the subscriptions serving the priority; its
  own pace point (`PaceFiveHour`, `PaceSevenDay`, the credential's
  windowTargets) paces it in place of node.yaml's
  (`LimitsConfig.For`), `limits.stop` still node-wide (adr-005's
  amendment 2026-10-02, a credential names the priorities it serves).
  Both ride the claim-time read: `gateway.Credential` carries
  `Priorities []string` and `WindowTargets []WindowTarget`
  (`WindowTarget{Window, Percent}`) from the row's `priorities` and
  `windowTargets` (`String()` names what it serves, never the secret),
  `IssuedAccount` copies `Priorities` straight across and turns each
  `WindowTarget`'s `Percent / 100` into `PaceFiveHour` or `PaceSevenDay`
  by its window, the known accounts persist both in `accounts.json` so
  a restart has them before its first claim, and `mesh node start` logs
  one `accounts: issued <label> (<kind>, serves <urgent, high|all>,
  paces at <50%> five-hour|node-wide)` line per known issued account
  (`node.IssuedAccountLines`, adr-005's amendment 2026-10-02, item 2).
  **No account with headroom
  means no claim**: the item stays ready, not failed or released, and
  the tick logs one `accounts: no account has headroom …` line — or,
  when the accounts with headroom were skipped for their priorities
  alone, `accounts: no account serves <priority> work (<label> serves
  urgent and high, …); <n> item(s) left unclaimed until one does`, and
  the holding reason (`heldReason`, the goal's waiting_reason) says
  `<label> serves only urgent and high`. When the node's own choice had
  headroom but the claim-time read refuses every issued credential
  (`ErrNoHeadroom`), the refusal is **a hold per project, kind and
  adapter** until the earliest `resets_at` among the refused
  credentials, else 10 minutes: what was claimed is released (an item by
  `ReleaseWorkItem`, attempts unchanged; a plan's claim lapses, no try
  counted), and every claim path — `claimReady` (build),
  `evaluateHeld` (evaluate, unless Codex has room), `rebaseRequested`
  (rebase), `planRequested` (plan), `researchDue` (research, the
  direction not offered) — asks `NoCredentials.Holds` first, passes the
  work over on `holding.unclaimed` and says the reason once per goal or
  direction; at the until **one probe** claims and the rest wait on its
  read (adr-005's amendments 2026-10-03). **A claim refused for
  budget, pause or approval is a hold** (`internal/node/claimhold.go`,
  ADR-024): `claimReady` classes a `ClaimWorkItem` refusal by
  `claimHoldKind` (`budget`, `pause`, `approval`, else `""`), notes the
  goal in `Loop.ClaimHolds` with its row from the tick's GoalIndex read,
  records it by `NoteHeld` (kind `item`, reason `<kind>: <message>`)
  and logs once `claim: <org> goal <slug> held (<kind>): <message>; not
  asked again until the goal changes`; before `accountFor` an item of a
  held goal is passed over silently, until the goal's `updatedAt`
  changes (else its status and paused reason, on an older platform) or
  the refused item leaves the ready listing for a tick and is listed
  again — then `ClearHeld` and `claim: <org> goal <slug> hold lifted`,
  and the item is claimed that tick. Any other refusal is still `not
  ours`, asked again next tick (`claimhold_loop_test.go`).
  **The claim-time read** (`internal/node/credentials.go`, adr-005):
  every one-shot session opens its account through
  `Credentials.Open`, which asks the platform (`gateway.Session`: the
  platform's `itemCredentials`/`itemTools`, `planCredentials`/`planTools`
  or `researchCredentials`/`researchTools`, by what the session works on)
  for the claimed session's credentials and chooses among them by the
  loop's own rule (`withChooser`); an api key at its cap is never
  chosen, and none left releases a build. A read that answers with
  none for the session's adapter is `ErrNoCredential` (`nocredential.go`,
  adr-005's amendment 2026-10-02) — `org <org>'s project <project> has
  no <adapter> credential open to this node`, never the machine's own
  login: the build is released, and `NoCredentials` (shared with
  `Loop.NoCredential`) holds that project's items, plans and rebases
  unclaimed for 10 minutes with the reason on `holding.unclaimed`, said
  once per goal per run; then one claim probes, the rest still held.
  A read whose issued credentials for the adapter have no headroom is
  `ErrNoHeadroom` (adr-005's amendment 2026-10-03): it `Is`
  `ErrNoAccount`, names the org, project, session kind, adapter, the
  refused labels and `Until`, the earliest `resumeAt` among their
  readings (`NoHeadroomOf`), and `NoteHeadroom` holds that (project,
  kind, adapter) — the store is keyed so, kind `""` being the
  no-credential hold over every kind — until `Until` (else 10 minutes);
  `Holds(project, kind, adapter)` gives the reason and its until and
  lets one probe through when it passes, and a successful open clears
  it. Only a platform without the read leaves node.yaml,
  said once per run. At the end
  the session's `SessionAccount.Report` (`gateway.UsageReport`:
  credential id and kind, tokens, tool calls, wall time, list and
  actual cents, windows before and after) rides its finishing command
  as `usage` — `FinishWorkItem`/`FinishWorkItemAlreadyDone`,
  `EvaluateWorkItem` (the Codex second opinion folded in), `PlanGoal`,
  `FinishWorkItemRebase`, `RecordDirectionResearch` — and
  `SessionAccount.Finish` logs every session's record, carried or not,
  as one `<label>: usage <kind> {…}` line (`SetLabel`; a carried
  session's is in its carrier's; `ErrNoUsageLedger` said once per run),
  and `mesh node usage` (`ReadUsage`, `usagereading.go`) reads them back
  into sessions, input tokens, cache reads, context bytes and documents
  fetched by tool (`documentsFetched`: `mesh_document` and `mesh_search`
  calls, with `toolNames` beside it on the report), and `tool calls` and
  `tools called` (every tool but the built-in, from `tools`) by kind for a window
  (adr-016's amendments).
- **Codex** (`internal/harness/codex.go`, goals `codex-adapter` and
  `codex-sessions-reach-the-context-tool`): the
  `codex` harness, `codex exec --json --sandbox workspace-write` in the
  workspace, the prompt on stdin, the stream read by
  `ParseCodexStream` (`docs/codex-events.md`). Each run gets a fresh
  temporary `CODEX_HOME`, removed after it: a plan account's
  `auth.json` copied in (and written back when Codex refreshed it), or
  an API key in the environment only (`CODEX_API_KEY` and
  `OPENAI_API_KEY`), plus the MCP servers as `config.toml`, secrets by
  variable name (mesh-specs ADR 006): the built-in by `CodexMCPConfig`
  (a `${VAR}` resolved from the job's env or refused), console tools by
  `tools.CodexConfig`, every table `default_tools_approval_mode =
  "approve"` — `exec` under `workspace-write` refuses an unapproved
  MCP call (docs/codex-events.md, "Why the context tool failed").
  The sandboxed commands get the network (`-c
  sandbox_workspace_write.network_access=true`, `harness.SandboxNetwork`,
  on `exec` and on the probe alike): without it Codex's seccomp filter
  refuses `socket()` and no test that binds loopback runs — the price
  is a sandbox that can reach the network (ADR-011's amendment
  2026-09-29, item 9).
  A **provider** (docs/adr/adr-015) is an OpenAI-compatible endpoint an
  account runs against instead of OpenAI: `codex.providers.<name>` in
  node.yaml gives a `base_url` (vLLM and SGLang serve OpenAI's API under
  `/v1`), an optional `wire_api` (`chat`, the default, or `responses`)
  and the list prices of the models it serves; an account names one with
  `provider:` and must be an api-key account, because the key rides
  `OPENAI_API_KEY` and a plan login has none to send. The provider is
  declared first in `config.toml` as `model_provider` and its table, and
  on its own makes the file worth writing. Its price table is the only
  one its models are costed at — an unpriced model records tokens and no
  money — so `CodexPrices` stays OpenAI's alone. An adapter is the CLI
  and a provider is where the tokens come from: Kimi behind vLLM is a
  provider, not a third adapter.
  Each session's event stream and stderr are kept for 24 hours under
  `<work root>/kept-sessions/<name>/` (`output.jsonl`, `stderr.log`;
  `KeepSessionOutput`), as a Claude session's output is. Before a Codex
  evaluation starts it is preflighted (`Codex.Preflight`, below) and
  falls back to Claude when the context server cannot be reached.
  node.yaml's `codex:` block
  (`CodexConfig`: binary, accounts, `models {opus, sonnet, haiku}`)
  and `CodexModel` map the Claude tiers to Codex models, by default
  opus → `gpt-6-sol` and sonnet/haiku → `gpt-6-luna`, astra only when
  pinned. A session whose context tool failed on every call gives no
  verdict: a tool failure is never a verdict (the evaluator's
  `toolBroken`, below). Codex has no
  budget flag, so `MaxBudgetUSD` is not enforced. An issued openai
  credential serving `codex` is a Codex account too (adr-005,
  `PrepareCodexAccount`): `Credentials.Open` with adapter codex chooses
  among those alone, the login lands as the session's
  `CODEX_HOME/auth.json` (0600, gone with the home; a refreshed one
  kept at `CodexLoginCache` per id and secret version), a key as
  `CODEX_API_KEY`/`OPENAI_API_KEY`, and the harness writes only
  `config.toml` into a home the job names.
- **The evaluator's family** (`internal/node/evaluate.go`,
  `docs/adr/adr-011-evaluations-run-on-the-family-with-headroom.md`,
  amending `adr-005-codex-second-opinion.md`; goal
  `codex-sessions-reach-the-context-tool`): `Evaluator.Codex` is the
  Codex harness (`codexSecondOpinion` in `nodecmd.go`: built whenever
  the codex binary is found, with or without a `codex:` block, with no
  account of its own) and `CodexAccounts` is
  `Loop.CodexCandidates` — the issued Codex accounts `Known` holds and
  only those: no Codex session runs on the machine's own login, and a
  Codex job with no placed `CODEX_HOME` is refused by the harness
  (`harness.ErrNoIssuedCredential`; ADR-011's amendment 2026-10-02). Before an evaluation starts
  the evaluator judges them with `ChooseAccount` over
  `Limits.CodexReadings` for the goal's priority: one with headroom
  makes it a Codex session on `CodexModel` of the evaluate model
  (defaults gpt-6-sol for opus, gpt-6-luna for sonnet and haiku — astra
  costs $1.29 for one evaluation where sol costs about $0.45, so it
  runs only when pinned), opened by `Credentials.Open`
  (adapter codex), none issued `ErrNoAccount`; else Claude as before. A full `gpt-…`
  evaluate/item/agent model pins Codex (`EvaluatorPin`; no headroom is
  `ErrFamilyWaits`, the item stays in evaluating, said once per item by
  `Waits`), a `claude-…` one pins Claude; a bare tier pins nothing. The
  loop starts an evaluation on Codex room (`Loop.CodexRoom`) when no
  Claude account has any. Before any Codex session starts,
  `Evaluator.CodexPreflight` (`Codex.Preflight`: `codex mcp get
  runsheet` and an MCP tools/list handshake in the session's own home
  and env, then the sandbox probe `codex sandbox -c
  sandbox_mode="workspace-write" -c
  sandbox_workspace_write.network_access=true -- mesh sandbox-probe`, the
  session's own policy on the hidden subcommand that binds 127.0.0.1:0 —
  a `bwrap:` line or a non-zero exit is `sandbox: <why>`, the bwrap
  line, else the socket error) must pass: a failure moves an unpinned evaluation to Claude
  (`Codex preflight failed (<why>); evaluating on Claude`), keeps a
  pinned one waiting like `ErrFamilyWaits`, and makes a Codex second
  opinion `unavailable: preflight …`. An item is evaluated again on the other family
  with the same prompt and workspace by the rule (`secondOpinionWanted`,
  ADR-005's and ADR-011's amendments 2026-10-02): its goal `urgent` or
  `high`, a protected touch (`WorkItem.ProtectedFiles`, or a changed
  file — `Workspacer.ChangedPaths` — matching its `ProtectedRulePaths`,
  the project's `ProtectedPaths` or the checkout's `.mesh/config.yml`
  `protected_paths:`, `globMatch`), a first FAIL on a
  clause that is not a host reading (`hostReadingClause`, the planner's
  lexical test), or the checkout's `.mesh/config.yml` `second_opinion:
  always` (`readCheckoutConfig`; default `rule`, until ProjectIndex
  serves the field) — else one verdict. The second verdict decides:
  after a first pass a second fail is failed, after a first fail a
  second fail is failed on the union of clauses and a second pass is
  passed. The choice is one notes line before the `(evaluated on …)`
  tail and a node.log line, `evaluators: one — normal/low goal, no
  protected path` or `evaluators: two — <reason>`, and every
  `EvaluateWorkItem` carries `EvaluateChoice{EvaluatedModel}`; the notes carry both, `Codex (first evaluation, gpt-6-sol):
  VERDICT …` and `Claude (second opinion, opus): VERDICT …` (or Claude
  first, Codex second), truncated together to 4000, and the cost and
  tokens are the sum — all in the one `EvaluateWorkItem`, whose usage
  names `adapter: codex` and the gpt-6 model for a Codex evaluation. A
  second session that errors, times out or reaches no verdict, or no
  Codex account with headroom, keeps the first verdict with `<Family>
  second opinion unavailable: <reason>`. After a failed verdict is handed in,
  an item's second failed evaluation — the earlier records with no
  `VERDICT: PASS` that are not a no-verdict's reason, plus this one,
  exactly 2 — asks for a split, `RequestPlanReplacing(goal, node,
  item)` (`askSplit`; `replaceItem` sent only when `RequestPlanInput`
  has it, introspected and kept an hour; a refusal logged once per item
  as `split refused by the platform`, never an error; ADR-021's
  amendment 2026-10-02). **A tool failure is never a
  verdict** (`toolBroken`): a session whose context server (or a
  required console tool) errored and was never reached — no call it
  answered, clean (`ToolStat.Clean`) or with its own error result
  (`ToolStat.Answered`: a Codex `failed` call with `error` null and a
  result, a Claude `is_error` result from a server init named
  `connected` that is not Claude Code's own refusal) — or, on Codex,
  every command of which bubblewrap refused, or whose test commands
  that executed every one refused a socket — a runner's name in quoted
  text is no test, a declined one or one with no exit code never
  executed (`Result.SandboxBroken`, `sandbox: <bwrap or socket line>`) — has its verdict discarded; a
  tool that answered was reached (`ToolStat.Reached`), so the
  built-in's `not found` for a slug it does not have is the tool
  working, never a tool failure (ADR-011's amendment 2026-09-29, a tool
  that answered was reached). For a broken one the same job runs on the other family
  as the evaluation proper, its notes opening `<Family> (first attempt,
  <model>): no verdict — <tool>: <error>`; with no other family it
  is **no verdict** (`Evaluator.noVerdict`, ADR-011's amendment
  2026-09-29): `EvaluateWorkItemNoVerdict` sends `evaluateWorkItem`
  `passed: false`, `noVerdict: true` and `noVerdictReason` (runsheet
  ADR 035; `gateway.NoVerdictField`, `NoVerdictReasonField`) with the
  reason in `notes` too and the cost and usage, never a fail — as does a
  deciding session with no `VERDICT:` line. A gateway whose
  `EvaluateWorkItemInput` lacks `noVerdict` (introspected, kept an hour)
  gets nothing (`ErrNoVerdictOutcome`, said once per run): the item
  stays in evaluating (`ErrNoContext`, said once per item by `Blind`).
  A family blind on the first attempt is not asked for the second
  opinion: the other family's verdict stands. With neither a `codex:` block nor
  an issued Codex account the node evaluates as before. Builders on
  Codex come later, behind a per-project setting.
- **Tools** (`internal/tools/`, ADR-006, trd §3 Tools): the
  claim-time read `gateway.Session` hands a session its credentials and
  its `tools` (`[]tools.Tool`, secrets decrypted for the claiming
  node); a platform without `tools` gets none and is asked for
  credentials alone from then on. `Loop.Runtimes()` is what the last
  probe found. Build, evaluate, plan and research sessions get theirs
  (`SelectTools` by `tools.RoleFor(kind)` — builder, evaluator, planner,
  director — and runtimes, `SessionTools.Apply`: runsheet
  plus each tool in `Job.MCPServers`, secret values in `Job.Env` and
  `Job.Secrets` only, the tools on `Job.Tools` for Codex); a
  nice-to-have tool the node cannot start is omitted and noted on the
  item (logged for a plan or research), a required one releases the
  item — a plan's claim goes back by `releasePlan` (`ToolsDeclinedError`,
  no try counted), a research hands in an empty one with the reason as
  its note — and `Declines` keeps the loop off the item, goal or
  direction while the runtimes are unchanged, the reason on the plan's
  holding line. Rebase and channel sessions keep runsheet alone.
  `tools.IsBuiltin(name)`
  knows the built-in by its name, `runsheet` (`tools.Builtin`), and by
  its old names, `mesh-context` and the one before it — one declaration,
  `tools.builtinAliases` (`BuiltinAliases()`, `BuiltinAlias()` the
  oldest, so no other file spells it) — read only from the stream and
  state of a session an older node started and this one adopted
  (`Result.ToolNames`/`DocumentsFetched` folded into runsheet, a
  `mcp__mesh-context__mesh_finish` a work call like
  `mcp__runsheet__mesh_finish`, `Evaluator.toolBroken`, `UseNote`); a
  console tool named `runsheet` clashes, one named as an old name is an
  ordinary tool (ADR-006's amendment 2026-10-15, goal
  the-node-calls-the-context-server-runsheet).
  `internal/node/runsheet_server_e2e_test.go` (`-tags e2e`) runs a build
  through the real Claude Code harness on the gatewaytest fake: its
  `--mcp-config` names `runsheet` and no old name, its relay's
  tools/list reaches the fake's `/mcp` (`Fake.MCPRequestsNamed`), its
  `mcp__runsheet__mesh_finish` is the finish handed in, and a session
  adopted with `mesh-context.token` and a stream naming `mesh-context`
  still finishes.
- **Tool results** (`internal/harness/trim.go`, node.yaml
  `tool_results.max_kb`, default 16, 0 off): a host build session gets
  a `PreToolUse` hook on `Bash` in its `--settings` (`Job.Settings`,
  `ToolTrimSettings`) — `mesh tool-trim --max-kb N --hook` rewrites the
  command to `( <cmd>\n) 2>&1 | mesh tool-trim --max-kb N`, which keeps
  the first 60% and last 40% of the cap (`TrimToolResult`) — and every
  build session `MAX_MCP_OUTPUT_TOKENS` and `BASH_MAX_OUTPUT_LENGTH`
  (`ToolResultsEnv`). Channel mode gets neither; a sandboxed one the env.
- **The hand-back** (`internal/harness/handback.go`, adr-012
  Hand-back): a host build's turns are priced by tier (`listPrices`,
  dated); past `Job.HandBackAt` (the runner's
  `harness.DefaultHandBackAt`, 0.6) of `MaxBudgetUSD` with
  `Job.HasCommit` false (`HasCommitSince`: `git rev-list --count
  origin/<base>..HEAD`) the unit is stopped and `--resume`d once on the
  budget left with `HandBackPrompt`. `Result.HandedBack`/`SpentOn`; the
  runner notes `SpentOn` on the item, keeps it in
  `~/.mesh/items/<item>.last-session` (`RunState.RecordLastSession`)
  and the next claim's `BuildPrompt` renders it as "The last session on
  this item". No commit still is an empty finish, back to `ready` on
  the platform. Sandboxed, adopted and Codex sessions do not hand back.
- **Validation** (`internal/node/validation.go`, `internal/validate/`,
  docs/adr/adr-013 and adr-014, runsheet ADR 032): after a successful,
  not already_done build and before the push, `.runsheet/validate.yml`'s
  suites (the platform's schema: `suites:` with name, kind, command,
  paths, reports, jobs, plus the node's timeout, always, artifacts,
  results; `steps:` an alias) the diff affects (`Workspacer.ChangedPaths`;
  a suite with no paths is every change) plus the always ones run on
  HEAD — none affected still attests, `commands: []` — and the
  attestation's `suiteHash` is `validate.SuiteHash`, the platform's
  `validatespec.SuiteHash` of the file (ADR-013's amendment) — in the session's pod (`sandboxRunner`, runner
  image by `sandbox.ImageDigest`) or on the host (`host:<hostname>`,
  the session's env less its secrets). The attestation goes to
  `<workdir>.session/attestation.json` and, after the push, to
  `RecordValidationAttestation` on the node's own token —
  `recordValidationAttestation(workItemId, input)`, the shape of
  `specs/contracts/recordValidationAttestation.json` (runsheet's,
  byte for byte), asked of the Mutation's introspection once an hour,
  no REST path. A failing step
  gets one retry turn (`harness.ValidationRetryPrompt`, the session
  resumed when it can be), then `FailWorkItem` with `validation failed:
  step <name> exited <code>`; per-step ms ride the usage report's
  `validation`. No validate.yml finishes as before. A rebase re-attests
  the head it pushed (`Rebaser.validateRebased`, after `PushedHead` and
  before `FinishWorkItemRebase`; ADR 032 carries no attestation across a
  rebase): the same steps and upload, a failing step logged and still
  uploaded, never the rebase's failure (adr-014, After a rebase).
  After the last push of a build, rework or rebase the node attests
  exactly the pushed sha (`attestPushed`, validated again when the
  push is not the commit validated) before the finish, answers a
  `<sha> is not the head of <branch> (GitHub has X)` refusal once by
  fetching, validating and attesting X (`answerNewerHead`), and the
  upload retries a 429/5xx/transport error for `AttestationRetryFor`,
  2 minutes; an attestation never fails an item. `attestation recorded
  <sha> <suites> <status>` and `attestation refused <reason>` are
  counted for the hour (`AttestationCounts`, `ValidationNotices.Counts`,
  `~/.mesh/node.attestations`): the loop's health line, every
  `DefaultHealthEvery`, ends `attestations recorded <n> refused <m>
  (1h)` and `mesh node status` prints `attestations: <n> recorded, <m>
  refused (last hour)` (adr-014's amendment 2026-10-09; the gateway's
  half is runsheet goal an-attestation-is-about-a-commit-not-an-item-state).
  The validator's fetches — the diff's base (`changedPaths`) and a newer
  head (`fetchHead`) — run on `ItemCred.ForFetch`'s token and the build's
  push on `ForPush`'s, each by `OnItemToken` (`GIT_CONFIG_COUNT` at the
  command's scope, never the shared clone's config), whatever the
  checkout's helper holds by then (ADR-001's amendment). **An
  environment failure names its kind** (`internal/validate/envkind.go`,
  `docs/adr/adr-030-an-environment-failure-names-its-kind.md`): the
  closed set `validate.EnvironmentKind` — `step_timeout` (a step's exit
  124), `tool_missing` (127), `credential_refused`
  (`validate.IsCredentialRefusal`, which `IsCredentialRefusal` reads, or
  a `*CredentialError`/`*PushCredentialError`) and `image_pull_refused`
  (`validate.IsImagePullRefusal`: an error, or podman's exit 125, naming
  a refused pull) — classified by `EnvironmentKindOf(step, err)`, set on
  a failing `StepResult.EnvironmentKind` by `validate.Run` (its runner
  error `Classify`d), sent as the attestation command's
  `environmentKind` (`gateway.ValidationCommand`, the contract's second
  command; left out for a pass or a code failure), and opening every
  reason as `<kind>: ` (`validate.WithKind`/`Reason`/`KindOfReason`) —
  `Runner.fail`'s `failWorkItem`, `ReleaseOnCredential`'s
  `releaseWorkItem`, a refused pull's `updateError`; node.log's step
  line ends ` (<kind>)`. A `tool_missing` reason names the missing
  tool and the command (ADR-030's amendment 2026-10-16):
  `validationFailure.Error()` renders ``validation failed: step <name>
  exited 127: command not found: <tool> (in `<command>`)``, the tool
  read from the step's output by `validate.MissingTool` (sh's, bash's
  and Go exec's "not found"), else the command alone, on a build's
  `failWorkItem` and a batch candidate's or main validation's failure
  alike (`toolmissing_e2e_test.go`'s `TestToolMissingE2E`). Any other
  non-zero exit carries none; the
  platform half, runsheet goal
  an-item-that-fails-on-its-environment-is-parked-not-replanned, parks
  on it. **A merged dependency's gone branch fails nothing**
  (`docs/adr/adr-033-a-gone-base-branch-is-read-as-merged.md`): an
  item built on a dependency's branch that merged and was deleted
  mid-build is diffed against the repo's default branch. A base fetch
  that fails is `*BaseGoneError` only when `git ls-remote --exit-code
  origin refs/heads/<base>` exits 2 (`Git.baseGone`; a credential or
  network refusal keeps its own error and kind), and then validation's
  changed paths (`validationTarget.defaultBranch`, `defaultBranchOf`),
  the evaluation's diff, changed and protected paths and prompt
  `BaseRef` (`goneBaseFallback`) and the retry turn's changed paths
  (`retryChangedPaths`) are taken from `origin/<default>`, and
  `Git.Prepare` continues a branch already on origin — the evaluation's
  `<seq>-eval` checkout, a rework — without its base (a new branch
  still needs it). Each says so: `item <label>: validation: base <dep>
  is gone (merged); changed paths from origin/<default>` (once per
  build, and on the item), `item <label>: evaluate: base <dep> is gone
  (merged); diff from origin/<default>`, `item <label>: retry turn:
  base <dep> is gone (merged); changed paths from origin/<default>`,
  `workspace <dir>: base <dep> is gone (merged); continuing
  origin/<branch>`. A squash-merged dependency's files count as changed
  too — a superset of suites; the attestation still names HEAD.
  `internal/node/basegone_e2e_test.go`'s
  `TestBaseGoneMergedDependencyValidatesOnMainE2E` (`-tags e2e`) deletes
  the dependency's branch between the build's commit and validation and
  sees the head attested, finished and evaluated passing.
- **Main validation** (`internal/node/mainvalidation.go`,
  `mainvalidation_loop.go`,
  `docs/adr/adr-032-a-node-validates-a-merge-batch-candidate.md`; runsheet
  ADR 048 decision 3, ADR 054's amendment 2026-10-13): the gateway's
  `internal/gateway/mainvalidation.go` reads `mainValidations(nodeId)`
  (`MainValidations`: `batches[]` — `MainValidationBatch{MergeBatchID,
  Number, CandidateBranch, SHA, Attempt, RepoFullName, RepoID, OrgID,
  DefaultBranch, RequestedAt}` — and `requests[]`, a merged item's
  `MainValidationRequest`) and sends `Claim`/`Finish`/
  `FailMergeBatchValidation` (`nodeId` on all three), their WorkItem twins
  `Claim`/`Finish`/`FailWorkItemMainValidation` (finish and fail take no
  `nodeId`) and `RecordBatchCandidateAttestation`
  (`recordValidationAttestation` with `mergeBatchId` as the argument and
  inside the input; the receipt's `branch` the candidate's), all gated by
  `TakesMainValidation` (the query and the six mutations, kept
  `ScalingProbeEvery`; a no sends nothing). The contracts are
  `specs/contracts/mainValidations.json`, the three batch commands,
  `recordValidationAttestation-batchCandidate.json` and the three twins —
  written from the live schema, the `requests[]` shape and the receipt's
  `branch` still to check against runsheet's. The fake's
  `SeedMergeBatch`/`SeedMainValidationRequest` serve them, a claim
  converging on one node and refused for another, a fail releasing it
  back to `requested`, a finish refusing a sha not the row's.
  Every tick, after `inUse` and before `claimReady`, while not draining,
  `Loop.validateBatches` reads each node id's queue and — only when
  `takes:` takes `evaluate`, a slot is free and the host is not hot —
  claims each entry of a served org (every batch before any request)
  under the `evaluate` cap, its run a `mainJob` (`startMainJob`,
  `runMainJob`) counted in `Running()`, `sessions()` and
  `runningOf(evaluate)`; a refused claim is ordinary and silent. A batch
  runs `Loop.ValidateBatch` = `Runner.ValidateCandidate`: the candidate a
  detached worktree of the shared clone at `<work
  root>/merge-batches/<mergeBatchId>-<attempt>` (`CandidateDir`,
  `Git.AddCandidateWorktree`; a fetched head other than the batch's sha
  `*CandidateHeadError`, nothing run), the suites the diff from
  `origin/<defaultBranch>` selects plus the `always` ones run in an
  `OpenSessionPod` pod of kind `validate` with its services
  (`validator.start`/`run`), attested with `mergeBatchId`. A request runs
  `Loop.ValidateMain` = `Runner.ValidateMainRequest`: the merge commit at
  `<root>/main-validations/<workItemId>-<attempt>`
  (`Git.AddCommitWorktree`; not on the default branch
  `*CommitNotOnBranchError`), suites by the merge's diff from its first
  parent (`validationTarget.diffFrom`), attested as the item's, no
  `mergeBatchId`. A batch's candidate is fetched on the batch token
  the platform mints (`gateway.IssueMergeBatchRepoToken`,
  `issueItemRepoToken(mergeBatchId:)`, read access on its repo for the
  claim's node; `NewBatchCred` → an `ItemCred` renewed by the same form
  before each fetch), on every fetch of the run by `OnItemToken`, logged
  `merge batch <n> (<repo>): candidate fetched on minted token (<repo>,
  <access>)`; a platform that mints none (`IsUnsupported`) fetches on the
  operator's login, said once per run (`TokenFallback.NoteBatch`,
  `Runner.BatchFallback`); a refused mint or renewal — or one
  `checkBatchToken` refuses, empty, malformed (`tokenShape`) or for
  another repo — fetches nothing and fails the validation `github token for merge batch <n>: …` (ADR-032's
  amendment 2026-10-14). A request runs on the operator's credentials.
  The worktree is removed on every path. **Both are first validated
  from the served diff** (`internal/node/serveddiff.go`,
  `checkoutServed`, ADR-034,
  `docs/adr/adr-034-a-node-validates-from-a-served-diff-with-a-shared-cache.md`,
  amending ADR-028 and ADR-032): the commit and `compare
  <base>...<sha>` read from GitHub's Git Data API
  (`internal/gitdata`: `Commit`, `Compare`, `Tree`, `Blob`; a capped
  diff `DiffTooLargeError`, a truncated tree `TruncatedError`) on the
  run's minted token (`gitdata.WithToken`, `validator.servedToken`), the
  merge base materialised from the node's one shared tree cache
  (`internal/node/sharedcache.go`, `SharedCache`: `SharedCacheRoot()`,
  `<user cache dir>/mesh/cache`, `blobs/`, `trees/`, `commits/` by sha,
  a miss filled once from the repo's shared clone else the `BlobSource`,
  `GitDataSource`; `Runner.ServedDiff`, `NewServedDiffCache`) into
  `MakeScratch(ScratchRoot(WorkRoot), "served", <subject>)`, the diff
  applied through the cache (`ApplyDiff`) and checked against the
  commit's tree (`checkMaterialized`) — logged `<prefix>: materialised
  <sha7> from <base7> and <n> changed file(s) of the served diff` — no
  worktree, no fetch, no repo lock; the suites chosen by the compare's
  paths, the scratch removed on every path (`validator.removeScratch`,
  after a sandboxed run's pod). The cache's hits and misses
  (`CacheCounts`) persist to `~/.mesh/node.cache`
  (`RunState.WriteCacheCounts`); `SharedCache.Enforce` holds the cache
  root, `go/` included, under node.yaml's `validation_cache_max_bytes`
  (`Config.ValidationCacheCap`, default 20 GiB) after every
  materialisation and applied diff — least recently used entries
  evicted, logged `cache: evicted <n> least recently used entries
  (<size>) to hold the cap of <cap>; <size> now`, then `go clean
  -cache`, still over the finding `cache over cap: <size> of <cap>`,
  once until back under. A diff past the cap, a truncated tree, no
  minted token (a request today), no cache, no parent or a tree
  mismatch takes the worktree above, `<prefix>: served diff: <reason>;
  validating from a worktree (said once per run)` (`servedFallback`,
  `noteServedFallback`). A sandboxed validation's pod mounts
  `sandbox.ValidationCache` (`RunSpec.Cache`): its scratch rw (TMPDIR,
  GOTMPDIR), the Go build cache rw (GOCACHE), the cache's `blobs/`
  read-only, nothing else of the cache root, the scratch removed by
  `Sandbox.RemoveTree`. `mesh node status` prints `validation cache: 75%
  hits (30/40), 1.2 GiB of 20.0 GiB` or `validation cache: no lookups
  yet`. `internal/node/serveddiff_e2e_test.go`'s
  `TestServedDiffValidationsShareOneCacheAndLeaveNoResidueE2E` (`go test
  ./internal/node -run 'ServedDiff|SharedCache'`) runs a request and
  three candidates on one base: all green, hits rising, GitHub asked
  only for each diff's new blobs, a 32 KiB cap held by eviction, and
  nothing left in TMPDIR or the work root but an empty `.scratch`. An
  evaluation's validation still runs on its own checkout. Green and
  red are both finished on the attested sha (the attestation is the
  verdict); an environment failure (ADR-030), a classified fetch failure,
  a required service down or any error is failed with `validate.Reason`,
  releasing the claim. `watchMainClaim` re-claims every
  `BatchCheckEvery` (default a minute): refused is the claim lost, the
  run stopped and nothing sent; past `MainValidationClaimAfter` (10
  minutes) it is stopped and failed. node.log: `batch <repo>#<n> attempt
  <a>: claimed candidate <branch>@<sha7>`, `… attested green|red
  (<suites>)`, `… finished`, `… failed: <reason>`, `… stopped: claim lost
  …|ran past …`; a request's prefix `main <repo>@<sha7> item <seq>: `,
  its claim line `claimed`; once per run `the platform serves no main
  validation; validating no merge batch`.
  `internal/node/mergebatch_e2e_test.go`'s
  `TestNodeValidatesAMergeBatchCandidate` runs it end to end on the
  Loop, and `TestNodeFetchesABatchCandidateOnItsMintedToken` against a
  `git http-backend` remote that takes the minted token alone (else, on
  a platform that mints none, the operator's login alone).
- **Services** (`internal/sandbox/services.go`, mesh-specs ADR 007):
  `Resolve(repoDir, allowed)` reads what a repository declares, first
  found wins — `.mesh/services.yml`, else the `services:` of its
  workflows' test jobs (merged by name, not required), else none — into
  one `Service` shape with a parsed `HealthCheck`, test tuning for
  Postgres and MySQL/MariaDB, a postgres or postgis service's own
  `PGDATA`/`POSTGRES_INITDB_ARGS` defaulted to a path writable under
  keep-id when it sets neither (`postgresDefaults`, docs/adr/adr-007's
  2026-10-16 amendment), and a disallowed registry returned `Refused`
  with a note. Pure parsing; the fixtures are under
  `internal/sandbox/testdata/` (their `.mesh/` needs `git add -f`).
- **A session commits as its agent** (`internal/node/identity.go`,
  docs/adr/adr-024): `AgentIdentity(role, slug)` is `Mesh <role>
  (<slug>) <mesh-<role>+<slug>@runsheet.dev>`; the Runner and the
  Rebaser call `Workspacer.Identify` after `Prepare` — `git config
  --local user.name/user.email`, `core.hooksPath` to `<workdir>.hooks`
  whose `prepare-commit-msg` first drops every line matching
  `claudeTrailer` — `Co-Authored-By: Claude … <noreply@anthropic.com>`,
  any case, POSIX sh and `grep -v -i -E` so it runs in the pod too; a
  pairer named Claude at another email stays — then adds one
  `Co-Authored-By` per `Pairers` (node.yaml `pair: [{name, email}]`
  until the platform serves a paired field) with `git interpret-trailers
  --if-exists addIfDifferent` — and put `GIT_AUTHOR_*`/`GIT_COMMITTER_*`
  on `Job.Env`; the sandbox's `.gitconfig` is the same agent
  (`Sandbox.Open`'s `GitIdentity`, the hooks mounted read-only), never
  the operator's. Evaluations get none. git runs the hook for `commit
  -m`, `--no-verify`, the node's own `Git.Commit` and every commit a
  rebase's sequencer re-creates (a fast-forwarded pick is left as it
  was). And every Claude Code session, host or sandbox, every role, is
  started with `--settings` carrying `"attribution": {"commit": "",
  "pr": "", "sessionUrl": false}` (`harness.NoAttributionSettings`,
  merged over `Job.Settings` by `harness.SessionSettings`; the
  empty-string form because `attribution: false` needs CLI v2.1.281+),
  so Claude Code adds no trailer or PR text of its own: the pairer's is
  a commit's only Co-Authored-By (ADR-024's amendment 2026-10-15;
  `TestBuildCommitDropsClaudeCoAuthorTrailer`,
  `TestBuildSessionCommitCarriesNoCoAuthorClaude`,
  `TestSessionSettingsCarryNoCoAuthor`).
- **A session's commands name their actor** (`internal/node/identity.go`,
  docs/adr/adr-015's amendment 2026-10-03): every command a session
  sends carries `agent:<role>@<slug>` — its ctx marked once where the
  session opens with `actorContext(ctx, cfg, role)` over
  `gateway.WithActor`, the slug `agentSlug` (node.yaml's, else `node`,
  as `identityFor`'s). The Loop's claim, the Runner, the Rebaser and an
  adopted build's progress act as `builder`, the Evaluator as
  `evaluator`, the Planner as `planner`; `mesh mcp`'s pass-through
  tools as none. A new session kind marks its ctx the same way
  (`internal/node/actor_test.go`).
- **Session units** (`internal/sessionunit/`, mesh-specs ADR 008,
  `docs/adr/adr-008-sessions-in-their-own-unit.md`): a host `claude -p`
  is not the node's child. `ClaudeCode.Run` starts it through a `Unit`
  (`Start` → `Handle{PID, UnitName}`, `Alive`, `Stop`):
  `SystemdScope` (`systemd-run --user --scope
  --unit=mesh-session-<kind>-<subject>-<rand> --collect`, outside the
  node service's cgroup) when `systemctl --user` answers, else
  `Detached` (setsid, its own process group); the sandbox's pod becomes
  a third later. Its stdout goes to `<workdir>.session/output.jsonl`,
  stderr to `stderr.log`, and `state.json` (`State`, written
  atomically: node, item/goal/direction, claim, kind, model, account,
  pid, unit, startedAt, offset — never the prompt or a secret) from
  `Job.Meta`; the prompt is an unlinked file on stdin. `Follow` tails
  the output a line per read until the process is gone and the file
  stops growing, `ParseStream` reads it, and the offset is saved as it
  goes. A timeout or cancelled ctx `Stop`s the unit, and every end
  but a detach `Kill`s it: `Result.Killed` names what the agent left
  running there (a failed kill goes to Stderr, else `KillError`,
  never failing the session), and the node logs what it killed, a
  line per session (`logKilled` on `Result.KilledPerSession()`:
  `<subject>: session end killed <n> process(es) the agent left
  running: <pid> <comm>, …`, and `kill: <KillError>`); a build and a
  rebase also send it as a progress note, an evaluation puts it in the
  verdict's notes when it records one and only logs it (held for the
  next verdict, `LeftRunning`) when it records none, a plan and
  research log only (ADR-008, The session's end) — an adopted session
  whose subject is no longer the node's is `FoundSession.Kill`ed and
  logged the same way; the directory is
  removed when `Run` returns — unless the job is `Outlive`: see the
  adoption. **A build the node gives up on is stopped, not detached**
  (ADR-029, `internal/node/lost.go`): every `ProgressEvery` it reads its
  item back (`lostByRead`: terminal, cancelled, ready/pending, or
  claimed/running on another node id) and heartbeats only while held,
  a heartbeat refused as not held (`lostRefusal`) losing it too; the
  session's ctx is cancelled with `ErrClaimLost`, which `Is`
  `harness.ErrStopSession`, so `ClaudeCode.follow` kills an `Outlive`
  unit (`harness.StopRequested`) where a plain cancel still detaches;
  `afterLost` logs `item <label>: claim lost (<why>); session stopped`
  and sends nothing, and the slot frees as `RunItem`/`AdoptItem`
  return, a pod by its deferred `SessionPod.Close`
  (`gatewaytest.Fake.SetItem` sets a row as the platform would). A
  build starved on an overloaded host is stopped the same way and
  released (`ErrStarved`, see the hot host under the scale).
- **The sandbox** (`internal/sandbox/`, mesh-specs ADR 007,
  `docs/adr/adr-007-the-sandbox-on-the-node.md`): with a sandbox,
  `harness.ClaudeCode` runs `claude` in the session's own rootless
  Podman pod (`Sandbox.Open` → `Session.Run` → `Close`) from the node
  image: mounts only the workspace and a temporary HOME (the MCP config
  file, `CLAUDE_CONFIG_DIR`, `GH_CONFIG_DIR`, `.gitconfig`) — and, for
  a worktree, its shared clone's git dir read-write at its own path
  (`sandbox.GitCommonDir`, "" for a full clone) with its
  `.git/worktrees` read-only over it and only the session's own entry
  (`sandbox.GitDir`) read-write over that, so a `git worktree prune` in
  the pod, where every sibling's work tree is absent, deletes no live
  entry (`worktreeMounts`, deepest mount wins in `agentArgs`; ADR-034,
  `docs/adr/adr-034-a-session-cannot-prune-the-shared-clones-worktrees.md`,
  `TestPodCannotPruneWorktrees`; pruning stays the node's, on the host
  under the repo lock), and the node's one
  pnpm store and npm cache, `~/.mesh/store/{pnpm,npm}` (`StoreRoot()`,
  `PackageStores`, `sandbox.Stores`: `npm_config_store_dir`,
  `pnpm_config_store_dir`, `PNPM_HOME`, `npm_config_cache` in
  `BaseEnv`; separate mounts, so pnpm copies rather than hardlinks in
  a pod, EXDEV, ADR-028) — an env
  from the allow-list (`AllowedEnv`, `MESH_TOOL_*`, `BaseEnv`; never
  the node's), `--cpus/--memory/--pids-limit` from node.yaml
  `sandbox:` (`SandboxConfig.Settings`) with the limit told to the
  tools inside — `GOMAXPROCS`, `OMP_NUM_THREADS` and `MAKEFLAGS=-j<n>`
  at the ceiling of `--cpus` (`LimitEnv`, a name the spec's env sets
  left alone; ADR-031) — and nftables rules an init
  container loads (`NetworkPolicy.Ruleset`: the LAN, the host and the
  special ranges refused, 443/80 out). `Runtime` is `Podman` or the
  test `Fake`. `OpenSandbox` decides at start: `sandbox: off` is nil
  and today's exec path byte-for-byte; the mode left out is on when
  `podman --version` answers, else off with a log line; an explicit
  `on` without podman refuses to start. A limit kill
  (`*sandbox.LimitError`: OOM, pids, the runtime's timeout) is the
  node's condition: the runner releases the item, holds it off
  (`Declines`) and `Loop.NoteCondition` puts the reason on the
  heartbeat for `ConditionHold`; the heartbeat also carries `sandbox
  {mode, runtime, image}`. `Session.Close` removes the pods
  (`RemoveArgs`: the containers with `--volumes`, then the pod) and the
  HOME (`removeHome`: RemoveAll, u+w, `Runtime.RemoveTree`), logging
  `sandbox: session <label>: close: <err>`; `mesh node start`, after
  adoption, runs `node.SweepSandbox` → `sandbox.Sweep` (exited mesh
  containers, mesh pods with no running container, `mesh-home-*` dirs no
  running container mounts) and logs `sandbox: swept N homes, M pods, K
  containers at start (L homes in use)` (ADR-007's amendment
  2026-10-10). Channel-mode sessions and Codex are not in
  the pod yet.
- **Sidecars** (`internal/sandbox/sidecars.go`, `internal/node/services.go`,
  mesh-specs ADR 007 Services): build, evaluate and rebase call
  `OpenSessionPod` before the agent — `Resolve` the checkout, then
  `Session.Start`: the pod, each allowed service a sidecar in it
  (`AddSidecar`, `--pull missing`) under `SplitLimits` of the services
  budget, then every health check at once (`--health-cmd` by
  `ExecSidecar`, else `ProbeTCP` on the first port) within
  `sandbox.service_timeout`; `Run` runs the agent in that pod
  (`Job.Session`) and the node's `Close` removes it on every path. The
  prompts' `# Services` section (`ServicesLine`) and
  `$HOME/.mesh-services` (`ServicesListing`) list them. A missing
  optional one is a progress note (the verdict's notes for an
  evaluation); a required one releases a build, declines an evaluation
  (`Declines`) or fails a rebase, naming it, and no agent starts; a
  required refused one makes no pod at all.
- Every session gets the built-in context server, `runsheet`
  (`tools.Builtin`, `harness.BuiltinMCP`; `mcp__runsheet__*`), as an
  MCP server — the stdio relay
  `<mesh> mcp-relay --url <MeshMCPURL> --token-file <path>`
  (`node.MeshContextMCP` → `harness.MeshContextRelay`; `internal/mcprelay`,
  NDJSON JSON-RPC POSTed on the file's bearer read per request,
  `Mcp-Session-Id` carried, a 401 re-reading the file once, a forgotten
  session re-initialised once, a missing or empty file a JSON-RPC error),
  in a pod `node <HOME>/runsheet-relay.js <url> <path>`
  (`harness.RelayScript`, `sandboxRelayServer`), for Codex the same
  stdio table (`[mcp_servers.runsheet]`) — on `runsheet.token`, 0600 in the session's own
  directory (`harness.PlaceMeshContext`: `<workdir>.session/`, the pod's
  HOME, the CODEX_HOME, the channel session's own temp dir), written
  from `gw.Bearer` by `node.TokenFiles` (the harnesses' and
  `channel.Session`'s `Tokens`, `sessionTokens` in nodecmd.go),
  rewritten atomically every `TokenRefreshEvery` (`auth.RefreshSkew`/4,
  15s) the bearer rotated, a failed start releasing it,
  removed when the session's end is handled and left on a detach;
  `state.json`'s `tokenFile` names it and `ClaudeCode.Attach` keeps an
  adopted session's (`keepAdopted`, an older session's
  `mesh-context.token` path as its state names it) — no bearer in any config, so a
  session outlives a rotation (ADR-029,
  `docs/adr/adr-029-a-sessions-mcp-token-is-refreshed-by-the-node.md`;
  `meshmcp.Hosted` keeps the http form, `node.MeshContextHTTP`; the
  gateway's context reads are `/mesh/v1` only, the `/prologue/v1`
  fallback removed, ADR-006's amendment 2026-10-15) — and
  the project's context on disk under `.mesh/context/` with `INDEX.md`:
  its own repos' instructions, then the screen, then `## Goal` — the
  session's own goal and its items (seq, title, status, repo, branch),
  read per session through the gateway (adr-003 decision 3, ADR-015).
  A failed items read leaves the goal's statement and acceptance, with a
  warning; `.mesh` is excluded from git, so none of it is ever committed.
  `WriteContext` returns what that cost (`ContextWritten`: bytes on
  disk with INDEX.md, files, documents left to fetch); each session
  logs it once (`<label>: context <n> bytes in <k> files, <m> documents
  to fetch`) and its usage report carries `contextBytes`, dropped for a
  platform without the field (`ErrNoContextUsage`, said once per run;
  docs/adr/adr-016).
- Costs: each session's `total_cost_usd` is reported to the platform per
  item (`ReportWorkItemProgress`, `EvaluateWorkItem`, `PlanGoal`).
  Priced tool calls are tool usage records: the harness counts each
  session's MCP calls per server (`Result.ToolCalls` — Claude's
  `tool_use` named `mcp__<server>__…`, Codex's completed
  `mcp_tool_call`, each call's id on `Result.ToolCallIDs`), and after a
  build or evaluate session `SessionAccount.Finish` sends one
  `RecordToolUsage(subject, toolCallId, tool, report)` (mutation
  `recordToolUsage`; the subject the item, else the plan's goal, else
  the research's direction, ADR-006's amendment 2026-10-03) per call of a rendered tool with
  `pricePerCallCents > 0` — the session's report at the tool's price,
  no tokens, no actual cost; runsheet and unpriced tools are none.
  A platform without `recordToolUsage` is said once per run and each
  call logged instead; it never fails the item.
  Every server's calls, errors, ms and first error's text (200 chars,
  redacted; `Result.ToolReport()`) ride the usage report as `tools`
  beside the total (`SessionAccount.Add` merges carried sessions' rows,
  the first error kept); a platform without the field gets it resent
  without (`ErrNoToolUsage`, said once, the list logged), and an entry's
  `error` goes only when the introspected `UsageReportInput` takes it
  (kept an hour). A Codex call's error is its `error.message`, and a
  server Codex says failed to start is an error with no call. A served
  console tool never called, or whose first call errored, and any other
  server that errored — the built-in `runsheet` first — is named by
  `SessionTools.UseNote` — a build's progress note, the evaluation's
  notes, the research hand-in's notes and its log line, a plan's log
  line (`planGoal` takes no notes) (ADR-006 amendments
  2026-09-28).
- **Models are per kind of session.** `Models` in `~/.mesh/node.yaml`
  (`default`, `build`, `evaluate`, `plan`, `channel`, `research`,
  `rebase`) or the matching
  `--model*` flags on `node start`; `mesh node model` shows and sets
  them. A kind with none takes `default`; no default leaves the CLI's
  own — except the planner, which without an explicit `plan` runs on
  the strongest configured (`Models.PlanModel`), else `opus`. The
  planner names a `model` per item by the rubric in `PlanPrompt`; the
  node drops one outside haiku/sonnet/opus/`claude-*` with a log line
  and hands the rest in on `PlannedItem.Model`. The value is an alias the CLI knows (`opus`, `sonnet`, `haiku`)
  or a full id (`claude-opus-5`). An item's build and rebase run on
  the item's own `model`, then its agent's, then node.yaml's `build`
  (or `default`) (`BuildModel`); the evaluator on the stronger of that
  and `sonnet` (`EvaluateModel`); node.yaml is the fallback. The model
  used is in node.log and on the item's events.
- **The node's log** (`internal/node/logfile.go`, goal
  `node-yaml-takes-effect-or-says-why-not`): `mesh node start` opens
  `~/.mesh/node.log` (`ServiceLog`) itself through `RotatingFile` —
  rotated at 10 MB (`NodeLogMaxBytes`) to `node.log.1` … `node.log.5`
  (`NodeLogKeep`), one already past the cap rotated on open, no write
  past the cap (one larger is cut to it; one it cannot rotate for is
  refused with the error, said once on stderr) — and
  `~/.mesh/sessions.log` (`SessionsLog`, 20 MB, 2 kept) beside it
  (`OpenNodeLogs`). node.log is the node's own lines, each session's
  summaries included; what sessions say — channel mode's `session:`
  text (`channelText`), `Git.Log`, the harnesses' `Stderr` — goes to
  `sessionsOut`, sessions.log under `node start`, stderr for every
  other command. The service templates never append to node.log: the
  systemd unit's `StandardOutput=journal`/`StandardError=journal`, the
  LaunchAgent's `~/.mesh/node.crash.log` (`CrashLog`), for a panic's
  stack and the `restarting: exec` line (written to node.log too). A
  unit installed earlier keeps its append redirect until `mesh node
  service install` runs again. `mesh node usage` reads the rotated
  files oldest first (`LogFiles`).

## Rules

- No third-party integrations in this repo (see CONTRIBUTING.md): no
  Discord, no email, no API keys. A session reaches GitHub only with its
  item's own token: a GitHub App installation token the platform mints
  per item (`issueItemRepoToken`), scoped to the item's one repo,
  contents write + pull requests read, an hour, renewed near expiry. The
  node puts it on the checkout's own git credential helper (clearing the
  operator's `gh auth git-credential`) and gives the session `GH_TOKEN`
  and an empty, per-session `GH_CONFIG_DIR`, so the operator's gh login
  is out of reach (adr-001-item-scoped-github-token). On a platform that
  does not know the mutation the node falls back to the operator's own
  `gh`/`git` credentials and logs that once per run (`TokenFallback`) —
  the rollout is not a flag day; a refused token fails the item instead.
  `mesh mcp`'s work tools have a second fallback of the same shape: they
  call the platform's hosted `mesh_claim`, `mesh_progress`, `mesh_ask`,
  `mesh_finish` and `mesh_release` on `/mcp` when its `tools/list`
  names all five (`meshmcp.Hosted`, re-probed hourly; the arguments in
  `specs/contracts/hostedworktools.json`), else today's mutations, said
  once per run on stderr, `the platform serves no hosted work tools;
  using claimWorkItem` (`HostedFallbackLine`; ADR-023, Hosted path).
  The planner's project-wide fetch runs on the goal's own tokens
  (`PlanGitHubTokens`, adr-022): each repo's plan checkout
  `Authorize`d on its row (`PlanRepoSpec`) before `Fetch`, so `git
  fetch` and never `gh repo clone`, and the session `SessionEnv` of the
  first sorted repo's row with a fresh `GH_CONFIG_DIR`; a refusal or a
  repo with no row is the plan's error naming it, before any session; a
  platform without the read plans on the operator's `gh`, said once per
  run (`TokenFallback.NotePlan`). A plan that fails there — the token,
  the `Authorize` or the `Fetch` (`PlanFetchError{Repo, Err}`) — is
  reported, not left to lapse: `ReleasePlan(goal, node, <err>)`
  (mutation `releasePlan`, `{nodeId, note}`) releases the plan claim
  with the error, the goal's `planError` on the platform, logged `goal
  <slug>: fetch failed: <err>; plan claim released`, no try counted
  against `MaxPlanAttempts`; a platform without it is said once per run
  (`takes no plan release`) and the try counted, a refused release
  logged with its error (adr-022, A fetch that fails is reported). The director's research fetch runs
  the same way (`Director.researchTokens`, adr-022): each repo's
  research checkout `Authorize`d on its row before `Fetch`, the
  session's `SessionEnv` of the first sorted repo's row with a fresh
  `GH_CONFIG_DIR`, the operator's `gh` never consulted; a refused read,
  or a repo with no row, hands in an empty research naming the repo
  (`refusedToken`) rather than failing the claim, the same way a
  refused credential does, so the direction is not left held; an old
  platform researches on the operator's `gh` as before, said once per
  run (`TokenFallback.NoteResearch`).
- A change to what the node reports or claims changes the platform's
  contract too; land the platform side first (a process reacts only to
  events after its deploy).
- Tests that render the context index or the prompts assert exact rows
  and lines; update them with the change, not around it.
- A commit's only Co-Authored-By trailer is the paired person the node
  sets; a session adds none of its own.
- **Release path** (adr-002, amendment `node-releases-on-merge`): a
  merge to main touching `cmd/`, `internal/`, `images/node/`, `go.mod`
  or `go.sum` is tagged by `release-on-merge.yml`
  (`scripts/release-on-merge.sh`: patch, or minor on the PR label
  `release:minor` or a goal doc's `release: minor`; test prereleases
  with a `-` never count), pushed with `RELEASE_TAG_TOKEN` — never
  `GITHUB_TOKEN`, whose pushes start no workflow — and `release-tag.yml`,
  the only signer, builds it; docs-only merges cut none. `git tag` by
  hand is the fallback.
- Deploying to the mini by hand is the fallback to the release path:
  build for linux/amd64, `scp` to
  `~/bin/mesh.new` and `mesh node upgrade --binary ~/bin/mesh.new`: the
  binary is swapped (`.prev` kept), the node restarts at once and
  adopts its running `claude -p` sessions (README, "Upgrading a node
  without ending sessions"). A sandboxed node's build sessions are
  adopted too (`mesh node upgrade` still asks it for `--force`); a
  channel-mode session still ends with the node: for that, `mv` it over
  `~/bin/mesh` and `mesh node drain` instead.
