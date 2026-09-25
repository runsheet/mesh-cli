# mesh-cli — the node

The operator tool for Mesh. `mesh node start` makes this machine a node of
the runsheet platform: it speaks to the gateway (`api.runsheet.dev/graphql`,
`/prologue/v1`, `/mcp`) on the credential in `~/.mesh/token` — the Latchkey
machine credential `mesh node login` registers for this computer, or the
operator's `rs_*` console token — claims work items, builds each in a fresh `claude -p`
session, evaluates it in a second, plans goals in a third. The platform
side (goals, items, nodes, the scheduler, the integrator) is
`runsheet/runsheet`; the design is `projectmesh-io/mesh-specs` (prd, trd,
adr-001, adr-002) and it is binding.

```sh
go vet ./... && go build ./cmd/mesh/ && go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o mesh-linux ./cmd/mesh   # the mini runs linux/amd64
```

## How it is shaped

- `internal/harness/prompt.go`: the five prompts — `BuildPrompt`,
  `EvaluatePrompt`, `PlanPrompt`, `ResearchPrompt`, `RebasePrompt`. Their
  contract lines are load-bearing: the evaluator ends with
  `VERDICT: PASS|FAIL`, the planner ends with the JSON graph in a fenced
  block, the director ends with its proposals in one, the rebase session
  ends with `REBASED: <sha>` and `RESOLVED: <files>` or
  `REBASE FAILED: <why>`, the builder never pushes. Change wording
  freely; never the contract. (A later goal moves the guidance onto the
  platform as agent definitions.)
- `internal/latchkey/` + `internal/auth/token.go`: the machine
  credential — Latchkey's device flow, the rotating refresh grant, and
  the `~/.mesh/token` that holds either shape behind one `TokenSource`
  (latchkey `adr-machine-identity`).
- `internal/node/`: the loop (`runner`), the workspace and Prologue
  context (`workspace.go`: `FetchContext`, `RenderIndex`, the size budget),
  evaluate, plan and research sessions, the service install (`launchd` /
  `systemd --user`). `internal/gateway/client.go`: the platform client.
  `internal/channel/`: channel mode (one session per org, turns).
  **A draining node claims nothing** (`Loop.Drain`): see the drain
  below.
- **The director** (`internal/node/research.go`, runsheet trd §6c): a
  project carries **directions** — several, each its own aggregate with
  its own words, window, timezone, gap, ceiling and research claim
  (`Directions(org, project)` lists them; the id is
  `direction:<project>:<slug>`). On every tick the loop offers each live
  direction with room to spare — `ClaimDirectionResearch`; the platform
  refuses it outside the hours, inside the gap, when the direction is
  off or archived, or when another node holds it, so *a refusal is the
  ordinary answer and is not logged as an error*. A session that wins
  the claim reads the direction, the repos and the context and proposes
  goals; `RecordDirectionResearch` hands them in with the cost, and the
  platform decides how many become goals. A session whose output cannot
  be read hands in an empty research anyway — otherwise the claim holds
  the direction for 45 minutes and the night is lost. **Two ceilings
  hold and `RoomFor` takes the smaller**: the direction's own
  `maxLiveGoals` less the live goals it set (`Goal.DirectionID`), and
  the project's `maxLiveGoals` — its total over every direction, on the
  autonomy policy beside `replanCap` — less every live goal on it. A
  direction with no room is not offered at all. Each direction works in
  its own checkout, `ResearchDir(root, project, direction)`, because two
  of one project may be inside their hours at once.
- **The drain** (`Loop.Drain`, trd §3 Drain): `SIGUSR1` or `mesh node
  drain` (which signals the pid in `~/.mesh/node.pid`) stops every new
  start — items, plans, research, evaluations, rebases — and `Run` returns nil
  once no session of *any* kind is left (planners and evaluators do not
  count against capacity, so `Running()` is not the exit test); the
  process exits 0 and its service starts the new binary.
  `~/.mesh/node.draining` marks it for `mesh node status`, and every
  heartbeat says `draining`. `DrainAndWait(ctx)` is the drain a
  self-update blocks on; `Resume` undoes a drain that has not yet ended
  `Run`. The heartbeat's extras (`gateway.HeartbeatExtras`: `draining`,
  `updateError`, and `limits` — the latest Claude plan limits per
  account, runsheet `limits-on-the-console`) are sent only when set,
  because they are fields the platform half adds. Capacity
  and the orgs need no drain: `Reload` re-reads them every tick.
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
  for *anything* else — a held claim waits out the platform's
  15-minute timer. Its heartbeat and its cost are
  `ReportWorkItemProgress`. Like evaluations, rebases do not count
  against capacity. **The platform does not yet issue the item's token
  to the rebase holder** (runsheet `gateway/itemtoken.go` refuses an
  item in `review`; ADR-001 asks for it), so until it does every
  rebase fails at `github token:` and goes to its builder; the fake
  mirrors that unless a test sets `RebaseTokens`.
- Every session gets Prologue as an MCP server with the node's token and
  the project's context on disk under `.mesh/context/` with `INDEX.md`:
  the documents, then `## Live goals and items` — each live goal's items
  (seq, title, status, branch, PR) and the finished goals one line each,
  read per session through the gateway (`AddLive`, adr-003 decision 3).
  A failed read leaves the documents only, with a warning; `.mesh` is
  excluded from git, so none of it is ever committed.
- Costs: each session's `total_cost_usd` is reported to the platform per
  item (`ReportWorkItemProgress`, `EvaluateWorkItem`, `PlanGoal`).
- **Models are per kind of session.** `Models` in `~/.mesh/node.yaml`
  (`default`, `build`, `evaluate`, `plan`, `channel`, `research`,
  `rebase`) or the matching
  `--model*` flags on `node start`; `mesh node model` shows and sets
  them. A kind with none takes `default`; no default leaves the CLI's
  own. The value is an alias the CLI knows (`opus`, `sonnet`, `haiku`)
  or a full id (`claude-opus-5`). The platform's agent definitions will
  carry the model once they land (runsheet's `agents-on-the-platform`);
  these stay as the fallback.

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
  The planner's and the director's project-wide fetches stay on the
  operator's `gh` and `git`.
- A change to what the node reports or claims changes the platform's
  contract too; land the platform side first (a process reacts only to
  events after its deploy).
- Tests that render the context index or the prompts assert exact rows
  and lines; update them with the change, not around it.
- Commits carry no Co-Authored-By trailer in this repo.
- Deploying to the mini is by hand: build for linux/amd64, `scp` to
  `~/bin/mesh.new`, `mv` it over `~/bin/mesh` and `mesh node drain`:
  the node finishes its sessions, exits, and systemd starts the new
  binary. Never `systemctl --user restart` it while a `claude -p`
  session runs (a restart re-pushes running items; an unpushed build is
  lost).
