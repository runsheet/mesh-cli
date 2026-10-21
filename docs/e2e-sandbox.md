---
summary: The sandboxed-sessions e2e — scripts/e2e-sandbox.sh builds or pulls the node image through EnsureImage, runs the probe, sidecar and node-path e2e tests (the job container holding no ghs_ token or session bearer, a build logged as its session steps; ADR-038 a session runs as ordered steps), the session-steps case (TestBuildRunsAsSessionStepsE2E, -tags e2e: seven step records, no credential in the job container, a changeset recorded on the fake gateway, nothing pushed) (a build and its evaluation on runsheet's Postgres and on a Redis fixture, no pod left after), the operator's live checklist on the mini, the image tools check (TestImageToolsE2E: node/npm with npm ci && npx tsc, Postgres, headless Chromium, with today's image's failing output), the session proxy case (TestSessionProxySandboxedE2E, -tags e2e: a build's stub agent reaches runsheet, a remote and a local tool through the pod shim, each fake upstream on the host gets its credential from the proxy, and podman exec greps of the running agent container for mesh-session and each secret find nothing; ADR-038), the Codex case (TestCodexEvaluationInAPodE2E: a runsheet evaluation on Codex in a pod from the node image on a credential from MESH_E2E_CODEX_AUTH or MESH_E2E_CODEX_API_KEY, its usage naming adapter codex and a gpt- model, no pod or HOME left; recorded 2026-10-17), and the check-then-remove-by-hand runbook for retiring latchkey-e2e-pg.
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
| `TestProbeSessionE2E` | the job container's env and files hold no `ghs_` token though the job's env carried one, and no `HeldOutEnv` name (ADR-038); a session cannot read `~/.config/gh`, `~/.mesh/token`, another workspace or the Docker socket; its env is the allow-list; HTTPS out works, the LAN and the host do not |
| `TestLocalOnlyImageE2E` | a session starts from a local-only image under an unresolvable registry name (`localOnlyImage` retags the node image as `mesh-local.invalid/…`) with no pull: the agent runs and podman's stderr has no `Trying to pull`, though the session's env sets its own `HOME` — podman keeps the node's (`podmanEnv`) |
| `TestServicesSidecarsE2E` | the sandbox package alone: Postgres and Redis sidecars health-checked and reachable on 127.0.0.1, the pod gone after |
| `TestSandboxedSessionsE2E` | the node's own path, for **a build (`Runner.RunItem`) and its evaluation (`Evaluator.Evaluate`)**: `OpenSessionPod` resolves the checkout's services, starts the sidecars, waits for their health checks; the ClaudeCode harness runs a stub agent in the pod (`ClaudeCode.SandboxBinary`) which reaches Postgres on 127.0.0.1:5432 (its SSLRequest answered) or Redis on 127.0.0.1:6379 (`PING` → `+PONG`) and says `VERDICT: PASS`; the stub agent greps its own container for `ghs_` and `mesh-session` and finds neither (the item's token issued to the build); the build logs its seven step records succeeded; the evaluation is recorded passed; afterwards `podman pod ls` and `podman ps -a` hold nothing of the session and no session HOME is left |
| `TestBuildRunsAsSessionStepsE2E` | `-tags e2e`, run apart as the script's step 2c: a build run as the session's steps (mesh-cli ADR-038) on `sandbox.Fake`, the node's own Git and a stand-in GitHub that answers only the item's `ghs_` token — the records `checkout`, `exchange`, `pod`, `run`, `validate`, `finish`, `release` each succeeded, neither `ghs_` nor the session bearer in the job container's env or any file it mounts, the finish a changeset recorded on the fake gateway (`Fake.Changesets`) whose tree is the agent's commit's, and nothing pushed. Needs no podman |
| `TestSessionProxySandboxedE2E` | `-tags e2e`, so run apart as the script's step 2b — see [The session proxy case](#the-session-proxy-case) |
| `TestCodexEvaluationInAPodE2E` | run apart, as the script's step 3 — see [The Codex case](#the-codex-case) |

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

**Recorded run, `TestLocalOnlyImageE2E`** (item 10 of
`podman-keeps-the-nodes-home`, 2026-10-10). Not run on podman: the
builder's host (`mesh-podman-keeps-the-nodes-home-10-744eecad-0`, Linux
6.8.0-146, x86_64) has no podman (`podman: command not found`) and no
`sudo` to install it, so no pod could start and podman's stderr could
not be read for `Trying to pull`. What was run there:

```
$ MESH_SANDBOX_E2E=1 go test -count=1 -run TestLocalOnlyImageE2E -v ./internal/sandbox/
=== RUN   TestLocalOnlyImageE2E
    localimage_e2e_test.go:65: podman: podman --version: exec: "podman": executable file not found in $PATH
--- FAIL: TestLocalOnlyImageE2E (0.00s)
FAIL

$ go test -count=1 -run 'PodmanEnv|Env' -v ./internal/sandbox/
--- PASS: TestAgentEnvIsTheAllowList (0.00s)
--- PASS: TestPodmanEnvKeepsTheNodesPassthrough (0.00s)
--- PASS: TestGitIdentityFromEnv (0.00s)
ok  	projectmesh.io/mesh-cli/internal/sandbox
```

Item 9 had put the test behind an `e2e` build tag, so neither this
command nor `scripts/e2e-sandbox.sh` (which passes no tags) compiled
it — `go test` said `no tests to run`. The tag is gone: the test now
skips on `MESH_SANDBOX_E2E` alone, as the others here do. The unit test that holds
the cause — a container `HOME=/tmp/x` against a node `HOME=/home/node`
leaves podman's `HOME=/home/node` and the container's on `--env HOME` —
passes; the pull itself is unproven until a host with rootless podman
runs `MESH_SANDBOX_ENSURE=skip scripts/e2e-sandbox.sh` (or the command
above) and pastes its output here, in place of this paragraph.

## The session proxy case

Goal `a-session-reaches-every-mcp-server-through-one-proxy`, item 6
([ADR-038](adr/adr-038-a-session-reaches-every-mcp-server-through-one-proxy.md)).
`scripts/e2e-sandbox.sh`'s step 2b runs
**`TestSessionProxySandboxedE2E`**
(`internal/node/session_proxy_sandboxed_e2e_test.go`, build tag `e2e`,
so step 2's untagged run never compiles it):

```sh
MESH_SANDBOX_E2E=1 go test -tags e2e -count=1 -v -run '^TestSessionProxySandboxedE2E$' ./internal/node
```

The node's own `Runner.RunItem` runs a build on podman with the node
image (`MESH_SANDBOX_IMAGE`), its `SessionProxies` on, the item served a
remote tool (`search`, a secret `Authorization` header) and a local one
(`fs-tools`, a secret `FS_API_KEY`, `npx` faked on the test's `PATH`)
beside the built-in runsheet, whose bearer is the build's own bound
session bearer. The agent is a stub (`proxyStubAgent`, a Node script as
`ClaudeCode.SandboxBinary`): it reads `--mcp-config`, starts each
entry — the pod shim, `node <HOME>/mcp-proxy-shim.js <socket> <name>` —
and speaks MCP on its stdio (initialize, initialized, one tools/call),
then waits while the test greps its container, and ends in claude's
stream-json with an init naming each server `connected` and a
tool_use/tool_result per call. It passes when:

- each of runsheet, search and fs-tools answered the agent through the
  shim, and each fake upstream on the host received its credential from
  the proxy — search its header, runsheet the session's bearer (never
  the machine's), fs-tools its env in the child the proxy ran (printed
  as `upstream <name> received its credential: …`);
- `podman exec <agent> sh -c 'grep -rlF -f - / 2>/dev/null'`, the
  pattern on stdin, found nothing for `mesh-session` and for each secret
  (printed as `podman exec … <<< mesh-session: found nothing` and one
  line per secret). The pattern rides stdin, not argv, because grep's
  own `/proc/<pid>/cmdline` would hold it and match;
- the build's notes and the node's log carry no `runsheet served,
  never called` and no secret;
- afterwards no pod or container `mesh-<slug>-…`, no proxy directory
  under the MCP root and no session HOME is left.

`TestAgentContainerHoldsNoCredential` checks the same over the fake
runtime in every `go test ./...`.

**Recorded run, 2026-10-19** (item 6's build session, on
dl360-anythink inside its own build pod, Linux 6.8.0-146, x86_64).
**Not run on podman:** the session's pod has no podman (`podman: command
not found`) and no root to install it, so no real pod started. What was
run there: `go vet -tags e2e ./internal/node/`; the test, which skips
without `MESH_SANDBOX_E2E=1`; and the same stub agent, shim, proxy and
fake upstreams with the agent run on the host by the fake runtime (a
throwaway test, not kept) — every server answered through the shim
(`fs key ok`, `runsheet answered`, `search answered`), search saw
`Bearer <its secret>`, runsheet `Bearer <the session's bearer>`, and the
note read `fs-tools ok, runsheet ok, search ok` with no `never called`.
The first run with podman pastes its output here.

## The Codex case

Goal `codex-sessions-run-in-the-sandbox` (item 5; ADR-007's amendment
2026-10-17): a runsheet item's evaluation on Codex in a pod from the
node image. `scripts/e2e-sandbox.sh`'s step 3 runs
**`TestCodexEvaluationInAPodE2E`** (`internal/node/codex_sandbox_e2e_test.go`)
on its own, after the other e2e tests (step 2 skips it), when a Codex
credential is given:

```sh
MESH_E2E_CODEX_AUTH=$HOME/codex-e2e-auth.json scripts/e2e-sandbox.sh   # a copy of a ChatGPT login's auth.json
# or: MESH_E2E_CODEX_API_KEY=sk-… scripts/e2e-sandbox.sh
```

The credential is the platform's issued Codex credential in shape — a
ChatGPT login (`oauth_token`, the "codex - chatgpt pro" row) or an
OpenAI key (`api_key`) — seeded on the gatewaytest fake as item 1's
session credential beside a Claude one, and known to the node with
headroom, so the evaluator chooses Codex by ADR-011's rule (a normal
goal, sonnet → `gpt-6-luna`). Without one the script says
`== codex: skipped — no Codex credential given; …` and goes on; the
test itself skips naming both variables. Codex may refresh the login:
the test keeps the refresh in its own `XDG_CACHE_HOME`, so give it a
copy.

What it runs, on podman with the node image (`MESH_SANDBOX_IMAGE`): the
node's own `Evaluator.Evaluate` with a sandboxed node's evaluator — the
real `harness.Codex` with `Sandbox` set, its `Preflight` the
evaluator's — in a checkout of `testdata/runsheet-postgres` (so the pod
gets its Postgres sidecar) with a README the acceptance names. The pod
is opened once; the preflight's sandbox probe (`codex sandbox … --
node -e …`) runs in it under the session's HOME and env, then the
image's `codex exec --json --sandbox workspace-write` evaluates. The
fake gateway listens on the host's loopback, which the pod's network
refuses, so this session is given no `runsheet` MCP server and the
preflight's `codex mcp get`/tools/list steps have nothing to check;
`TestSandboxedCodexEvaluationPreflightsInItsPod` covers them over the
fake runtime. It passes when:

- `evaluateWorkItem` is called once with a verdict (`passed` true or
  false, never `noVerdict`) and the Claude harness never ran;
- its usage names `"adapter":"codex"`, a `gpt-` model and the
  credential (printed as `usage record: {…}`, beside `verdict recorded:
  passed=…`);
- the credential's secret is nowhere in the node's log;
- afterwards no session HOME is left under the sandbox's HOME root and
  podman holds no pod or container named `mesh-e2e-codex-…`
  (`assertNoPods`; the script prints `podman pod ls` after it).

The same path runs in every `go test` over the fake runtime as
**`TestCodexEvaluationInAPodOverTheFake`**: the fake image's codex
answering the probe and the session, the pod created once and removed,
its containers the probe then `codex exec`.

**Recorded run, 2026-10-17** (item 5's build session, on dl360-anythink
inside its own build pod, Linux 6.8.0-146, x86_64). **Not run on
podman:** the session's pod has no podman (`podman: command not
found`), no root to install it, and no Codex credential in its
environment, so no real pod started and no real Codex turn was taken.
What was run there, on this branch (the second command's node log lines cut to the Codex ones):

```
$ scripts/e2e-sandbox.sh; echo "exit $?"
e2e-sandbox: podman is not installed; the sandbox e2e needs rootless podman
exit 1

$ MESH_SANDBOX_E2E=1 go test -count=1 -run CodexEvaluationInAPod -v ./internal/node
=== RUN   TestCodexEvaluationInAPodE2E
    codex_sandbox_e2e_test.go:61: no Codex credential given: set MESH_E2E_CODEX_AUTH=<a ChatGPT login's auth.json> or MESH_E2E_CODEX_API_KEY=<an OpenAI key> to run a Codex evaluation in a pod
--- SKIP: TestCodexEvaluationInAPodE2E (0.00s)
=== RUN   TestCodexEvaluationInAPodOverTheFake
    codex_sandbox_e2e_test.go:195: item e2e-codex-174856/1: evaluate on Codex gpt-6-luna: mesh/e2e-codex-174856-1 (52 bytes of diff)
    codex_sandbox_e2e_test.go:195: item e2e-codex-174856/1: evaluation passed on Codex gpt-6-luna ($0.00)
    codex_sandbox_e2e_test.go:82: verdict recorded: passed=true; notes: README.md says what the fixture is for.
    codex_sandbox_e2e_test.go:82: usage record: {"actualCostCents":0,"adapter":"codex","cacheReadTokens":0,"cacheWriteTokens":0,"contextBytes":1124,"credentialId":"oa-1","credentialKind":"subscription","inputPerTurnMax":10,"inputPerTurnMean":10,"inputTokens":10,"listCostCents":0,"model":"gpt-6-luna","outputTokens":5,"sessionId":"t1","toolCalls":0,"turns":1,"wallMs":1}
--- PASS: TestCodexEvaluationInAPodOverTheFake (0.01s)
PASS
ok  	projectmesh.io/mesh-cli/internal/node	0.028s
```

The live run is owed: on a host with rootless podman (the dl360 as the
node's user, outside any pod), `MESH_SANDBOX_ENSURE=skip
MESH_E2E_CODEX_AUTH=<copy of the issued login> scripts/e2e-sandbox.sh`
— or `build` to make the image first — and paste its `== codex:` lines
(the usage record, the verdict, `podman pod ls after`) here in place of
this paragraph.

## Tools check

What an enrolled repo's checks need from the image — Node with npm,
a Postgres server, a headless browser — is checked inside it by
`scripts/image-tools-check.sh`, and the test that builds the image and
runs it there is **`TestImageToolsE2E`**
(`internal/sandbox/tools_e2e_test.go`, under the `e2e` build tag):

```sh
go test -tags e2e -run TestImageToolsE2E -v ./internal/sandbox/
# or on an image already here:
MESH_TOOLS_IMAGE=ghcr.io/runsheet/mesh-node:<version> go test -tags e2e -run TestImageToolsE2E -v ./internal/sandbox/
# or the script alone, in any image or on a host:
podman run --rm --security-opt label=disable -v "$PWD:/check:ro" <image> \
  bash /check/scripts/image-tools-check.sh /check/images/node/testdata/tools-fixture
```

Without `MESH_TOOLS_IMAGE` the test builds the embedded Containerfile
as `localhost/mesh-node-tools-e2e:test` — never a `mesh-node` tag, so
no prune touches the node's own — runs the script in it as the image's
user `mesh` (uid 1000), and removes the tag after. It skips without
podman. The script prints one line per check and exits 1 when any
failed:

| check | what it runs |
|-------|--------------|
| `node` | `node --version`, `npm --version`, and `npm ci && npx tsc --version` in a copy of `images/node/testdata/tools-fixture` (a lockfile pinning `typescript` 5.6.3) |
| `postgres` | `initdb` into a temp dir as the session's user, `pg_ctl start` on a unix socket there, `psql -c 'select 1'`, stopped and removed after |
| `chrome` | the first of `chromium`, `chromium-browser`, `google-chrome`, `chrome` on PATH: `--headless --version`, then a headless `--dump-dom` of a `data:` page |

**Today's image fails it** (2026-10-13, item 1 of
`a-node-validates-every-enrolled-repo-with-the-tools-it-needs`). Run
as `mesh` inside the node image this build session's own pod ran on
dl360-anythink — node v0.2.403, whose `images/node/Containerfile` is
byte for byte main's at 86511b9, i.e. `ghcr.io/runsheet/mesh-node:v0.2.403`:

```
$ scripts/image-tools-check.sh; echo "exit $?"
PASS node: node v24.21.0, npm 11.19.0, npm ci && npx tsc --version: Version 5.6.3
FAIL postgres: initdb not on PATH
FAIL chrome: no chromium, chromium-browser, google-chrome or chrome on PATH
exit 1
```

**The amended image passes it, unproven in podman.** The builder's
pod has no podman and no root, so it could not build the image. What
it ran instead, in that same image: the Containerfile's new packages'
Debian trixie amd64 debs (`postgresql-17` 17.11-0+deb13u1,
`chromium` 150.0.7871.181-1~deb13u1 and the 158 packages they pull in
that the image lacks) unpacked with `dpkg-deb -x` into `/tmp/root`, the
server's binaries linked into a directory on PATH as the Containerfile
links them into `/usr/local/bin`, `chromium` a shim to
`/tmp/root/usr/lib/chromium/chromium`, the libraries on
`LD_LIBRARY_PATH`:

```
$ PATH=/tmp/shim:/tmp/lbin:$PATH LD_LIBRARY_PATH=… scripts/image-tools-check.sh; echo "exit $?"
PASS node: node v24.21.0, npm 11.19.0, npm ci && npx tsc --version: Version 5.6.3
PASS postgres: postgres (PostgreSQL) 17.11 (Debian 17.11-0+deb13u1), select 1 = 1 as mesh
PASS chrome: Chromium 150.0.7871.181; chromium --headless --dump-dom rendered a page
exit 0
```

The first host with podman to run `TestImageToolsE2E` pastes its
output here, in place of this paragraph.

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
  with such a URL (the script still reads that tree; a node's checkouts
  are under `~/.mesh/jobs` since ADR-039), in a checkout that declares no services (one that
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
