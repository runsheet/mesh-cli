# mesh-cli — the node

The operator tool for Mesh. `mesh node start` makes this machine a node of
the runsheet platform: it speaks to the gateway (`api.runsheet.dev/graphql`,
`/prologue/v1`, `/mcp`) as the person whose `rs_*` console token it holds
(`~/.mesh/token`), claims work items, builds each in a fresh `claude -p`
session, evaluates it in a second, plans goals in a third. The platform
side (goals, items, nodes, the scheduler, the integrator) is
`runsheet/runsheet`; the design is `projectmesh-io/mesh-specs` (prd, trd,
adr-001, adr-002) and it is binding.

```sh
go vet ./... && go build ./cmd/mesh/ && go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o mesh-linux ./cmd/mesh   # the mini runs linux/amd64
```

## How it is shaped

- `internal/harness/prompt.go`: the three prompts — `BuildPrompt`,
  `EvaluatePrompt`, `PlanPrompt`. Their contract lines are load-bearing:
  the evaluator ends with `VERDICT: PASS|FAIL`, the planner ends with the
  JSON graph in a fenced block, the builder never pushes. Change wording
  freely; never the contract. (A later goal moves the guidance onto the
  platform as agent definitions.)
- `internal/node/`: the loop (`runner`), the workspace and Prologue
  context (`workspace.go`: `FetchContext`, `RenderIndex`, the size budget),
  evaluate and plan sessions, the service install (`launchd` / `systemd
  --user`). `internal/gateway/client.go`: the platform client.
  `internal/channel/`: channel mode (one session per org, turns).
- Every session gets Prologue as an MCP server with the node's token and
  the project's context on disk under `.mesh/context/` with `INDEX.md`.
- Costs: each session's `total_cost_usd` is reported to the platform per
  item (`ReportWorkItemProgress`, `EvaluateWorkItem`, `PlanGoal`).

## Rules

- No third-party integrations in this repo (see CONTRIBUTING.md): no
  Discord, no email, no API keys. GitHub is reached through `gh` and `git`
  with the operator's own credentials.
- A change to what the node reports or claims changes the platform's
  contract too; land the platform side first (a process reacts only to
  events after its deploy).
- Tests that render the context index or the prompts assert exact rows
  and lines; update them with the change, not around it.
- Commits carry no Co-Authored-By trailer in this repo.
- Deploying to the mini is by hand: build for linux/amd64, `scp` to
  `~/bin/mesh.new`, and swap plus `systemctl --user restart
  io.projectmesh.mesh-node.service` only when no `claude -p` session is
  running (a restart re-pushes running items; an unpushed build is lost).
