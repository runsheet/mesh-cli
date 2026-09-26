---
summary: The sandboxed-sessions e2e — scripts/e2e-sandbox.sh builds or pulls the node image through EnsureImage, runs the probe, sidecar and node-path e2e tests (a build and its evaluation on runsheet's Postgres and on a Redis fixture, no pod left after), the operator's live checklist on the mini, and the check-then-remove-by-hand runbook for retiring latchkey-e2e-pg.
---

# The sandbox e2e

The goal `sandboxed-sessions` is accepted on the mini: a runsheet build
item and its evaluation pass in sandboxes with their Postgres from
runsheet's own test workflow, and a fixture repository with Redis
passes. This is how that is run and recorded, and how the old shared
Postgres (`latchkey-e2e-pg` on the host's `:5432`) goes once nothing
uses it. The design is mesh-specs ADR 007 and
[ADR-007 here](adr/adr-007-the-sandbox-on-the-node.md).

## Running the e2e

**Prerequisites**

- Rootless podman (`podman --version` answers as the node's user, and
  `podman unshare` works: `/etc/subuid` and `/etc/subgid` carry the
  user). The node's own sandbox uses the same runtime.
- The node image. By default the script builds
  `ghcr.io/runsheet/mesh-node:dev` here from the embedded
  Containerfile, exactly as `sandbox.image: build` does
  (`SandboxConfig.EnsureImage`); `MESH_SANDBOX_ENSURE=pull` pulls it
  instead (`podman login ghcr.io` first while the package is private),
  `skip` uses the one already present.
- Go at go.mod's version, and the network to pull `postgres:17-alpine`
  and `redis:7-alpine` from docker.io (cached after the first run).

**Run it**

```sh
scripts/e2e-sandbox.sh
# or: MESH_SANDBOX_ENSURE=pull MESH_SANDBOX_IMAGE=ghcr.io/runsheet/mesh-node:v0.5.0 scripts/e2e-sandbox.sh
```

Like a node's update, ensuring the image prunes every other `mesh-node`
tag but the running node's (`MESH_SANDBOX_KEEP_VERSION`, by default
`mesh --version`), so the live node's sessions keep theirs.

It runs, with `MESH_SANDBOX_E2E=1`, every `E2E` test of
`internal/sandbox` and `internal/node`:

| test | what it shows |
|------|---------------|
| `TestProbeSessionE2E` | a session cannot read `~/.config/gh`, `~/.mesh/token`, another workspace or the Docker socket; its env is the allow-list; HTTPS out works, the LAN and the host do not |
| `TestServicesSidecarsE2E` | the sandbox package alone: Postgres and Redis sidecars health-checked and reachable on 127.0.0.1, the pod gone after |
| `TestSandboxedSessionsE2E` | the node's own path, for **a build (`Runner.RunItem`) and its evaluation (`Evaluator.Evaluate`)**: `OpenSessionPod` resolves the checkout's services, starts the sidecars, waits for their health checks; the ClaudeCode harness runs a stub agent in the pod (`ClaudeCode.SandboxBinary`) which reaches Postgres on 127.0.0.1:5432 (its SSLRequest answered) or Redis on 127.0.0.1:6379 (`PING` → `+PONG`) and says `VERDICT: PASS`; the evaluation is recorded passed; afterwards `podman pod ls` and `podman ps -a` hold nothing of the session and no session HOME is left |

`TestSandboxedSessionsE2E` runs twice, on the two fixtures in
`internal/node/testdata/`:

- `runsheet-postgres/` — runsheet's `.github/workflows/test.yml`, its
  test job's `services:` block copied verbatim (Postgres 17, `pg_isready`
  health check), so the case needs no network checkout. It resolves to
  Postgres, not required (a workflow's services never are), with the
  test tuning (fsync, synchronous_commit, full_page_writes off).
- `redis-services/` — a `.mesh/services.yml` declaring Redis
  (`redis-cli ping` health check), required.

`TestE2EFixturesResolve` checks both resolutions without podman, in
every `go test ./...`. Without `MESH_SANDBOX_E2E=1` the e2e tests skip:

```
$ go test -run E2E -v ./internal/node
=== RUN   TestE2EFixturesResolve
--- PASS: TestE2EFixturesResolve (0.00s)
=== RUN   TestSandboxedSessionsE2E
    sandbox_e2e_test.go:105: MESH_SANDBOX_E2E=1 runs real pods
--- SKIP: TestSandboxedSessionsE2E (0.00s)
=== RUN   TestEnsureImageE2E
    sandbox_e2e_test.go:180: MESH_SANDBOX_E2E=1 MESH_SANDBOX_ENSURE=build|pull builds or pulls the node image
--- SKIP: TestEnsureImageE2E (0.00s)
PASS
```

**Recorded run.** The session that wrote this (item 9 of
`sandboxed-sessions`, 2026-09-25) ran on the mini's host without
podman installed (`podman: command not found`; docker only), so it
could not run the pods. What it did run: the whole suite and the
skips above; the stub agent's two probes on the host against a real
Postgres (`build OK 127.0.0.1:5432`) and a Redis-speaking listener
(`evaluate OK 127.0.0.1:6379`); and the script's latchkey check, whose
output is under [Retiring latchkey-e2e-pg](#retiring-latchkey-e2e-pg).
The first run with podman pastes its output here.

## Live run on the mini

The e2e above proves the path; the goal's acceptance is the same on
real items. The operator's checklist:

1. **Podman and the image.** Install rootless podman for `chris`, then
   `scripts/e2e-sandbox.sh` until it passes. Paste its output into
   *Recorded run* above.
2. **A sandboxed node.** Deploy the release carrying the sandbox as
   AGENTS.md says (build linux/amd64, `scp` to `~/bin/mesh.new`, `mv`,
   `mesh node drain`). In `~/.mesh/node.yaml` set `sandbox: on` (an
   explicit on refuses to start without podman, so nothing runs
   unsandboxed by accident) and, while the package is private,
   `image: build`. `mesh node update <running version>` makes the
   image. Check `node.log` says `sandbox: on — podman version …, image
   …` and the console's Nodes page shows the mode.
3. **A runsheet build item.** Let the node claim a ready runsheet item
   (or queue a small one). In `node.log`: `services from workflow:
   postgres`, `service postgres (postgres:17-alpine) ready at
   127.0.0.1:5432`, then the build. While it runs, `podman pod ps`
   shows `mesh-<goal>-<seq>-…` with its postgres sidecar, and the
   session's `go test ./...` reaches `RUNSHEET_TEST_PG` on
   127.0.0.1:5432 **inside the pod**, not latchkey-e2e-pg. After it,
   `podman pod ps -a` has no pod of it.
4. **Its evaluation.** The same item's evaluation runs in its own pod
   with its own Postgres (`<label> evaluate` in the pod name) and
   passes; the item's events carry the verdict, and again no pod is
   left.
5. **The Redis fixture.** Point a scratch goal at a fixture repository
   whose `.mesh/services.yml` is `internal/node/testdata/redis-services`'s
   and whose test PINGs 127.0.0.1:6379. Its build and evaluation pass
   with `service redis (redis:7-alpine) ready at 127.0.0.1:6379`, no
   pod left.
6. **Record** the item links and the log lines in the goal's record;
   then retire latchkey-e2e-pg, below.

## Retiring latchkey-e2e-pg

`latchkey-e2e-pg` is the long-lived Postgres every session shared on
the host's `:5432`. A sandboxed session never reaches it: the pod's
network refuses the host, and a repository that needs Postgres gets its
own sidecar on the pod's 127.0.0.1:5432. It goes when nothing uses it.

`scripts/e2e-sandbox.sh` ends with the check; it **never removes the
container itself**:

- every podman and docker container publishing the host's `:5432`;
- `~/.mesh/node.yaml` naming `DATABASE_URL` or a Postgres URL on
  localhost/127.0.0.1:5432;
- the workspaces' `.env`, `.env.*` and `.envrc` under `~/.mesh/work`
  with such a URL, in a checkout that declares no services (one that
  does gets a sidecar on that same address, which is fine);
- a running host `claude`/`codex` session (`sandbox: off`) with such a
  URL in its environment.

It exits 2 and names each user while one remains, else prints the
command. The check's output on the mini on 2026-09-25:

```
== who still uses latchkey-e2e-pg
  docker container on :5432: latchkey-e2e-pg (Up 3 hours) 0.0.0.0:5432->5432/tcp  <- the one to retire
== nothing uses latchkey-e2e-pg. Remove it by hand with:
   docker rm -f latchkey-e2e-pg
```

The runbook:

1. Every node on the mini runs with `sandbox: on` (a node with
   `sandbox: off` runs sessions on the host, which can still reach
   `:5432`); `mesh node status` and the Nodes page say so.
2. The live run above has passed: runsheet's build and evaluation had
   their own Postgres.
3. `scripts/e2e-sandbox.sh` ends with `nothing uses latchkey-e2e-pg`.
   Anything it names is fixed first — a `DATABASE_URL` in node.yaml
   removed, a repository given a `.mesh/services.yml` or a workflow
   `services:` block.
4. Remove it by hand with the command printed (`docker rm -f
   latchkey-e2e-pg`, or `podman rm -f` when podman holds it), and its
   volume if one is listed by `docker inspect latchkey-e2e-pg`.
5. Re-run the check: nothing is on `:5432`.
