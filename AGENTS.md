# mesh-cli — the node

The operator tool for Mesh. `mesh node run` makes this machine a Mesh v1
node (`internal/nodev1`, ADR-044): it registers with Mesh
(`api.projectmesh.io`) on the credential in `~/.mesh/token` — the Latchkey
machine credential `mesh node login` registers for this computer — takes
one lease at a time, runs the lease's own harnessed session (a
`claude -p`) with the brief's product MCP servers through the lease's own
sessionproxy, renews the lease while the session runs, and finishes or
hands it back. It speaks only `internal/meshapi`, Mesh's v1 node API; a
guard test refuses any import of the legacy `internal/gateway` or
`internal/node` from `internal/nodev1`. This is wave 0's minimal
runtime — one lease at a time; the job actor, scheduler and supervisor
(`internal/actor`, `internal/scheduler`, `internal/supervisor`) are wave
3's. The platform side is Mesh itself (`projectmesh-io/api`); the design
is the Mesh v1 spec ("The mesh-cli node") and `docs/adr/adr-044-*.md` on
this repo, and both are binding.

The legacy runtime this superseded — `mesh node start`, a gateway-GraphQL
claim/build/evaluate/plan loop against `runsheet/runsheet` — is deleted
(goal `the-v0-node-is-removed-from-mesh-cli`): `internal/jobresult`,
`internal/session` and `internal/nodecaps`, and the `Loop`/`Runner`/
`Evaluator`/`Planner`/`Director` types of `internal/node`, are gone.
`internal/gateway` and parts of `internal/node` survive only as the
legacy job-API client a person's own `mesh mcp`/`mesh join` still uses
(below); `runsheet/mesh-specs` is binding for that surviving path alone.

```sh
go vet ./... && go build ./cmd/mesh/ && go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o mesh-linux ./cmd/mesh   # the mini runs linux/amd64
```

## How it is shaped

- `internal/nodetools` + `internal/finishschema`: how a v1 session's
  result reaches the node (ADR-044, mesh-specs "Node API", superseding
  ADR-041's `internal/jobresult` and `jobresult.Holder`, deleted with
  the legacy Loop): `nodetools.result.go`'s `result` tool is the node's
  own, named in the job kind's `node_tools`, rather than a product's —
  the session hands in the job's result, `internal/finishschema`
  resolves the kind's `finish_schema` (JSON Schema, `projectmesh-io/api`
  ADR 001's amendment) the result must fit, a value that does not fit is
  refused with the reason, and a later accepted call replaces an earlier
  one; the node finishes the lease with it once the session ends.
  `internal/mcptool` (not deleted — the same generic, typed-MCP-tool
  framework ADR-041 described, now `internal/nodetools`' and
  `internal/nodev1`'s shared plumbing rather than
  `internal/jobresult`'s) serves it, and `push.go`, `handback.go`,
  `progress.go`, `stopsuite.go`, `runsuite.go` and `applyonto.go` are its
  siblings — the node's own tools (`push`, `hand_back`, `progress`,
  `run_suite`, `apply_onto`) a job session calls directly. A kind whose
  finish is the tool runs on the brief's `guidance` first, then the
  job's facts, with no built-in per-kind role text: the five
  v0 prompts this bullet once described (`BuildPrompt`, `EvaluatePrompt`,
  `PlanPrompt`, `ResearchPrompt`, `RebasePrompt`, their `VERDICT:`/
  `REBASED:`/fenced-graph text contracts, the plan-check rules
  `plan_check.go` ran against a planner's graph, the evaluator's
  second-opinion and same-clause bookkeeping, and the acceptance-split,
  docs-writer and close-out rules a planner's answer was checked
  against) belonged to the deleted v0 Loop's `Planner`/`Evaluator`/
  `Rebaser`, all gone with it. A v1 job kind's prompt is its product's
  own `guidance`, published to Mesh and fetched with the brief; the
  node runs no kind-specific contract on top of it beyond the one
  finish-schema check (above) and the node's own tools (`push`,
  `hand_back`, `progress`, `run_suite`, `apply_onto`, `result`). What a
  kind's guidance should say, and what a product's catalogue validates
  before publishing it, is `internal/catalogue`'s and the product's own
  concern, not this node's; see ADR-044 and the Mesh v1 node spec for
  the job-kind and brief shapes this node actually runs against.
- `internal/latchkey/` + `internal/auth/token.go`: the machine
  credential — Latchkey's device flow, the rotating refresh grant, and
  the `~/.mesh/token` that holds either shape behind one `TokenSource`
  (latchkey `adr-machine-identity`).
- `internal/node/`: what survives of the legacy runtime now that the
  v0 Loop types (`Runner`/`Evaluator`/`Planner`/`Director`/`Rebaser`)
  and the work sweep that ran inside `mesh node start` (`PlanJobSweep`,
  `ApplyWorkSweep`, their files) are deleted: the legacy gateway client
  below, which a person's own `mesh mcp`/`mesh join` still calls
  (ADR-040's "Not yet" — a person's own join, which holds no job, still
  asks its item token itself through this package, and still reads
  project context through this package's shared context render,
  `FetchContext`/`RenderIndex`/`WriteContext` in `workspace.go`, which
  is live and unchanged), and `ProjectRepos` for `mesh goal plan` (a
  person's own plan upload, not an autonomous planner). What is not
  deleted, and not called by anything either: the per-job workspace
  tree's own layout (`layout.go`'s `Layout`/`JobDirs`, `jobdirs.go`)
  and the quartermaster, scratch, rebase-worktree and second-opinion
  helpers that tree once served (`quartermaster.go` and its sibling
  `quartermaster_*.go` files, `scratch.go`, `rebaseworktree.go`,
  `second_opinion.go`, `holding.go`) — every exported and unexported
  func in each still compiles but calls nowhere a built `mesh` binary
  reaches (`go run golang.org/x/tools/cmd/deadcode@latest ./cmd/mesh`
  names every one of them unreachable); they are leftover from the v0
  Loop, not wired to `mesh mcp`/`mesh join` or to `mesh node run`, and a
  later item should either delete them or give them a caller. The
  service install (`launchd`/`systemd --user`) installs `mesh node
  run`. A project's repos — for `mesh goal plan` alone now — are read
  from `project.repoIds` through `ProjectRepos`, each owned by its own
  account's login, never the org's account: an org links several
  GitHub accounts (the runsheet org's loom project lives at
  go-apis/loom). The v1 node's own per-lease workspace, context and job
  directory are `internal/nodev1`'s and `internal/actor`'s (ADR-044):
  each lease owns its own job directory (`leases/<lease_id>`), closed
  in reverse order the moment the lease ends, no shared clone or shared
  context tree to sweep.
  `internal/gateway/client.go`: the platform client.
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
  to make a test pass. The fakes also speak runsheet's published contracts
  (runsheet ADR 026, ADR 069): `scripts/contracts-pull.sh [ref]` (main
  by default; a branch, tag or commit otherwise) copies runsheet's
  `specs/contracts/*.json` byte for byte into `specs/contracts/runsheet/`
  with `PULLED` naming the ref and commit — committed, never
  hand-edited; mesh-cli's own hand-kept copies in `specs/contracts/`
  are untouched. `gatewaytest/contracts.go` checks every request and
  answer on `ContractedFields` (claimJob, finishJob, jobAccess,
  planGoal, claimPlan, recordValidationAttestation) and
  `ContractedRoutes` (the changeset routes, matched by the http
  fixtures' method and path) against the fixtures: a key the fixture
  lacks (a typed argument's input type in schema.json joined in) or a
  value of another JSON type is refused (a request) or replaced by a
  500 (an answer), as is an http answer status the fixture does not
  name, and the fake's `/mcp` `tools/list` may list only tools a
  runsheet server lists (`mcp-tools-*.json`, ADR 069); and `Fake.Close` panics on any violation a test did
  not `TakeContractViolations`. `PendingFixtures` names a contracted
  route runsheet publishes no fixture for yet (served unchecked; none
  today);
  `FixtureGaps` names keys runsheet's code reads or writes that its one
  example omits, with where, and keys the node sends ahead of runsheet
  (`Ahead`: `environmentKind`, ADR-030, which runsheet does not read
  yet) — both fail their tests once a pull makes
  the entry stale. The claim-converging, one-evaluation-lock mechanics
  this paragraph once described (`evaluateHeld`, `Loop.sayEvaluationElsewhere`)
  belonged to the deleted v0 Loop's Evaluator; a v1 lease's `jobAccess`
  exchange (ADR-040) is the one lock now — an access entry the platform
  did not serve is the claim's refusal, ordinary and silent.
  `internal/meshmcp/`: the stdio MCP server (`Server`, `Tool`,
  `ObjectSchema`) of `mesh mcp`, whose initialize answers
  `serverInfo.name` `runsheet` (`meshmcp.ServerName`; ADR-023's
  amendment 2026-10-17) — and `mesh mcp`'s
  personal tools (`NewServer`, `Personal`: `mesh_goals` and
  `mesh_status` read the gateway and `~/.mesh/mcp-state.json`,
  `mesh_join` claims an item and makes its checkout in
  `~/.mesh/work/<goal>/<seq>` (a person's machine, not a node's jobs
  tree; `JoinDeps.WorkRoot`) — or the cwd when it is the item's repo —
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
  **A death names itself** (`internal/crash/`): `defer crash.Guard()`
  at the top of `mesh node run` and of every goroutine the node
  starts turns a panic into one node.log line, `mesh node: fatal:
  <value> (version <v>)`, the stack, and exit 70 (`crash.ExitPanic`),
  never Go's 2 (systemd's `INVALIDARGUMENT`); a new node goroutine
  defers it first.
- **The v0 Loop's architecture — drain, self-update, pace, scale, the
  work sweep, accounts, Codex, the evaluator's family, tool results,
  the hand-back, validation, main validation, services, sidecars,
  session units and the sandbox, all as this section once described
  them run — is deleted with `internal/node`'s `Loop`/`Runner`/
  `Evaluator`/`Planner`/`Director`/`Rebaser` types** (goal
  `the-v0-node-is-removed-from-mesh-cli`); scratch, the quartermaster
  and held goals are not deleted, only unreferenced leftovers of that
  Loop (`scratch.go`, `quartermaster.go`, `holding.go` — see
  `internal/node/`, above).
  What runs today in their place is `mesh node run`'s v1 runtime
  (ADR-044, `status: proposed` — wave 0's minimal shape is live; the
  job-actor/scheduler/supervisor machinery below is wave 3's design and
  lands behind a node flag, off by default, so read its current state
  in the code and the ADR before relying on any of it as today's
  default):
  - `internal/nodev1`: the entry point `mesh node run` drives — claims
    one lease at a time (wave 0) against Mesh's v1 node API
    (`internal/meshapi`, `DefaultURL = "https://api.projectmesh.io"`),
    runs the lease's harnessed session, renews the lease while it
    runs, and finishes or hands it back.
  - `internal/actor`: a lease's own job actor (wave 3) — states
    taken → preparing → running → reporting → releasing → done,
    persisted as `actor.json` in the lease's own job directory;
    releasing is the only way out, including from a panic, so a lease
    is never left claimed with nothing watching it.
  - `internal/scheduler`: capacity as TTL'd leases over CPUs, memory,
    job slots, account slots and the build cache; every take reports
    the `free` room left.
  - `internal/supervisor`: watches each lease's `progress_at`,
    escalates a stalled one through its mailbox and then stops it with
    cause `stalled` (cancel mode) or only logs it (warn mode);
    reconciles on a timer — killing orphan or sidecar-only pods and
    removing job directories and relay grants nothing still claims.
  - `internal/resources`: resource actors outside any one lease — a
    self-update drains the node of new claims and hands back
    `node_update` rather than finishing a lease mid-swap; the build
    cache is bounded live, evicted by `WatchBuildCache` as entries are
    added.
  - `internal/catalogue` + `internal/finishschema`: a job kind's
    `guidance` and `finish_schema` are the product's, fetched with the
    brief; the node runs no kind-specific prompt contract of its own
    beyond the one finish-schema check and its own tools (`push`,
    `hand_back`, `progress`, `run_suite`, `apply_onto`, `result` —
    `internal/nodetools`).
  - The relay (`internal/egress`, `internal/gitrelay`,
    `internal/relaysidecar`): a job's own sidecar terminates TLS for
    the hosts a rule names (ADR-045), injects the credentials the job
    carried rather than placing them in the job's own environment or
    config (ADR-046), and routes a job's git traffic to the code host
    its brief names (ADR-047) — the v1 analogue of the v0 item-scoped
    GitHub token below, scoped per lease rather than per node command.
  - `internal/sandbox` (shared with the legacy runtime, ADR-007): a v1
    lease runs its session in its own pod the same way, per ADR-044.

  See `docs/adr/adr-044-a-v1-node-runs-each-lease-as-a-job-actor.md`
  and the Mesh v1 node spec ("The mesh-cli node") for the job-kind,
  brief and lease shapes this runtime actually runs against; this
  AGENTS.md does not restate their detail, to avoid the documentation
  drifting from the code again (goal
  `the-v0-node-is-removed-from-mesh-cli`, item 4).
- **The node's log** (`internal/node/logfile.go`, `internal/node/service.go`):
  `mesh node run` logs to stderr (`log.New(os.Stderr, …)`); the
  systemd unit's `StandardOutput=journal`/`StandardError=journal` and
  the LaunchAgent's crash log capture it. `mesh node usage` reads
  `~/.mesh/node.log` and its rotated siblings for historical usage
  records, and `mesh node service install` still names that path; the
  v0-specific `mesh node start` in-process rotation and the
  per-node.yaml-field reload logging this bullet once described
  (capacity, limits, accounts, context budgets, claim limits, shares)
  belonged to the deleted Loop and are gone with it.

## Rules

- No third-party integrations in this repo (see CONTRIBUTING.md): no
  Discord, no email, no API keys. A v1 lease's job never holds a bare
  credential of its own: the relay injects what its job offer carried
  (mesh-specs gap 28; ADR-046, `internal/nodev1/inject.go`). Each
  Latchkey secret ref in the offer's `access.secrets` and each scoped
  connection in `access.mint` is asked of Latchkey on the lease's own
  session token (`POST <Latchkey>/v1/credentials/for-job`, DPoP-proofed)
  and becomes an `egress.InjectRule` on the lease's tunnel, live only
  over the hosts the job kind's egress allows; the answer is held in the
  relay process's memory alone — never on disk, in a log line, an event
  or evidence, and never in the job's own pod, which sees only
  `egress.Placeholder` where a tool insists on a variable. A ref Latchkey
  will not give hands the lease back before the session runs
  (`credential_timeout` when Latchkey or the vendor could not be
  reached, `product` when Latchkey refused the configuration); a live
  one is re-asked past its `expires_at` or past `injectReask` (15
  minutes) since it was last asked, so a grace-0 rotation reaches a
  running job, and a re-ask that fails drops what was held and refuses
  the rule's requests until one succeeds. See ADR-045 (the relay's TLS
  termination for named hosts) and ADR-047 (git-host routing per the
  job's brief) for the rest of the tunnel this rides.
  `mesh mcp`'s work tools (a legacy, gateway-backed path, still live)
  have a fallback of their own: they call the platform's hosted
  `mesh_claim`, `mesh_progress`, `mesh_ask`, `mesh_finish` and
  `mesh_release` on `/mcp` when its `tools/list` names all five
  (`meshmcp.Hosted`, re-probed hourly; the arguments in
  `specs/contracts/hostedworktools.json`), else today's mutations, said
  once per run on stderr, `the platform serves no hosted work tools;
  using claimWorkItem` (`HostedFallbackLine`; ADR-023, Hosted path).
- A change to what the node reports or claims changes the platform's
  contract too; land the platform side first (a process reacts only to
  events after its deploy).
- The unit suite passes in the node's own validation sandbox, which
  attests every mesh-cli PR: TMPDIR is the run's scratch deep in the
  job dir, HOME a fresh one naming no git identity. A test binds a unix
  socket under `sockettest.Dir` (never `t.TempDir()` or
  `os.MkdirTemp("", …)`, which run past sun_path there), and a test
  that commits names its own identity (`git -c user.name=… -c
  user.email=…`, or the `GIT_AUTHOR_*`/`GIT_COMMITTER_*` variables).
- A testdata fixture never names a path component literally `.git`:
  `git add` (`-f` included) silently drops everything under it as a
  nested repository; use a stand-in file (e.g. `worktree-list.txt`).
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
  build for linux/amd64, `scp` to `~/bin/mesh.new` and `mesh node
  upgrade --binary ~/bin/mesh.new` (`internal/commands/nodeupgrade.go`,
  adr-002, adr-009, adr-044): the binary is swapped atomically, the
  previous one kept at `<path>.prev`, and the Mesh v1 node (`mesh node
  run`) is restarted at once — `SIGHUP` to its own pidfile
  (`~/.mesh/node-v1/node.pid`) — with nothing drained: every lease it
  holds is left running, its brief, sidecar, push heads and turn ledger
  written to `leases/<id>/handoff.json` for the new process to adopt.
  A lease the hand-off cannot adopt — one with no relay sidecar, or
  whose tunnel intercepts with no egress process of its own still alive
  — refuses the upgrade, naming it, unless `--force`, which restarts
  anyway and ends it. The new process confirms the upgrade on its first
  heartbeat, retried for node.yaml's `update.confirm_within` (5m by
  default) while the platform is slow; one that cannot run, or cannot
  confirm inside the window on three starts, is rolled back to the
  previous binary, and `~/.mesh/update.json` says why. `mesh node
  upgrade` then polls the new process's own status and reports how many
  of the leases left running it actually adopted, failing loudly if
  fewer arrived than were adoptable. `mesh node drain` stays the way to
  take a machine out of service instead (no restart, no adoption).
