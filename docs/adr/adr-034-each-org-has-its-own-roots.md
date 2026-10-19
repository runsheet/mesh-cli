---
summary: A node serving several orgs lays each org's work, repos, scratch, package store, kept sessions, resume records and MCP data under ~/.mesh/orgs/<org>/ (OrgRoots, keyed by the scope's Name() and refused unless it is one path segment unique in case), so one org's worktree is only ever made from its own org's clone and a repo served for two orgs is two clones; a node serving one org keeps today's paths and nothing moves. Amends ADR-028.
status: accepted
---

# ADR-034: Each org has its own roots

2026-10-17, goal `one-node-serves-several-orgs-each-sandboxed-from-the-rest`,
item 1. Amends ADR-028 (`adr-028-one-clone-per-repo-one-worktree-per-item.md`)
and, through it, mesh-specs ADR 017's one clone per repo: on a node that
serves several orgs it is one clone per repo per org. Keys orgs as ADR-023
(`adr-023-every-org-gets-its-share.md`) does, by the scope's `Name()`; the
orgs a node serves are the ones it registered for under its owner
(ADR-015, `adr-015-a-node-registers-once-with-an-owner.md`).

## Context

A node may serve several orgs (ADR-015: one registration, an owner, the
orgs node.yaml's `orgs:` lets through). Its roots were one per node:
`WorkRoot()` `~/.mesh/work`, `ReposRoot()` `~/.mesh/repos`, `StoreRoot()`
`~/.mesh/store`, `ScratchRoot` `<work root>/.scratch`. So org A's
checkouts and org B's were siblings under one `~/.mesh/work`, their shared
clones siblings under one `~/.mesh/repos`, and a repo both orgs reach —
a fork, a shared library, a mirror — was **one** clone whose objects,
refs and worktree registrations both orgs' sessions shared. A sandbox
that mounts a session's checkout and its clone's git dir (ADR-007,
ADR-028 "The sandbox mounts the clone's git dir") cannot keep one org
from another while their files share a directory, and one shared clone
serving two orgs is the same clone in both orgs' pods.

## Decision

### One tree per org

`internal/node/orgroots.go`'s `OrgRoots` are one org's directories:

| root    | path                                   |
| ------- | -------------------------------------- |
| Work    | `~/.mesh/orgs/<org>/work`              |
| Repos   | `~/.mesh/orgs/<org>/repos`             |
| Scratch | `~/.mesh/orgs/<org>/work/.scratch`     |
| Store   | `~/.mesh/orgs/<org>/store`             |
| Kept    | `~/.mesh/orgs/<org>/work/kept-sessions`|
| Resumes | `~/.mesh/orgs/<org>/resume`            |
| MCP     | `~/.mesh/orgs/<org>/mcp`               |

`RootsForOrg(meshDir, org)` lays one out; `OrgRootSet`
(`NewOrgRootSet(meshDir, orgs)`, `NewOrgRootSetFor(scopes)`) holds every
served org's, keyed by `Scope.Name()` — the login, else the slug, as
ADR-023's shares are. `For(org)` and `ForOrgID(scopes, orgID)` answer an
org's roots and **refuse** an org the node does not serve, naming it;
never another org's roots in its place.

### The org name is one path segment

`ValidateOrgName` refuses an empty name, `.` or `..`, a name with `/`,
`\` or NUL, and one with surrounding space, each with an error naming
it. `NewOrgRootSet` refuses two served names that differ only in case,
naming both — on a case-insensitive filesystem (macOS) they would be one
directory — and the same name served twice. The check runs for a single
org too, so a name is refused before a second org makes it matter.

### One clone per repo per org

The session kinds take an optional `Roots *OrgRootSet` (`Runner`,
`Evaluator`, `Rebaser`, `Planner`, `Director`; the validator by the
Runner's). With it set:

- a build's and a rebase's `WorkspaceSpec` is put under the item's org
  by `orgSpec` — `Root` its org's Work, the new `WorkspaceSpec.Repos` its
  org's Repos — so `Git.sharedClone` is `<org repos>/<owner>/<name>`,
  whatever `Git.ReposRoot` says;
- an evaluation's `<seq>-eval` (`EvalWorkspaceSpec`), a plan's
  `PlanDir` and a research's `ResearchDir` are under the org's Work
  (`workRootFor`, `workRootOfScope`); they stay clones of their own, as
  ADR-027 and ADR-028 decide;
- a merge batch's candidate and a main validation request's worktree
  are under the org's Work, so `Git.reposRootFor` — which now reads
  `~/.mesh/orgs/<org>/repos` off any dir under
  `~/.mesh/orgs/<org>/work/<x>/<y>` before `Git.ReposRoot`
  (`orgReposRootOf`) — adds them from that org's clone, and
  `RemoveWorkspace`'s refusal and registration sweep look in that org's
  repos root;
- a host validation's and a served diff's scratch is the org's Scratch
  (`scratchRootFor`).

An item whose org the node does not serve is failed at its workspace
(`workspace: org roots: org id "<id>" is not served by this node`),
never given another org's roots. With `Roots` nil every path is today's
`WorkRoot`, byte for byte.

### A node serving one org keeps today's paths

`NewOrgRootSet` with one org answers `SingleOrgRoots(meshDir)`: Work
`~/.mesh/work`, Repos `~/.mesh/repos`, Store `~/.mesh/store`, Resumes
`~/.mesh/resume`, Scratch and Kept under the work root as now, MCP
`~/.mesh/mcp`. There is no other org to keep it from, its clones,
worktree registrations and sessions' `state.json` paths stay valid, and
nothing is moved, re-cloned or re-adopted. Moving them was the other
choice: it buys a uniform layout at the price of re-cloning every repo
and leaving every live worktree's registration and every adoptable
session's paths pointing at the old tree, for no isolation a one-org
node lacks. A node that goes from one org to two lays both out under
`~/.mesh/orgs/`; what was under `~/.mesh/work` is left to finish there
and to the sweep.

### Refusals

Laying the trees out apart is half of it; the other half is that a
session of one org is never handed a path of another's. ADR-017's §2
refuses a pod a mount of any other item's worktree; on a node serving
several orgs the same refusal is drawn at the org's tree (item 3 of the
goal).

**A pod mounts nothing outside its org's root.** `sandbox.OrgRoot{Org,
Path}` is the one tree a session may mount from — `Path` the org's
`OrgRoots.Base`, `~/.mesh/orgs/<org>`. The node confines a session to
it by its checkout (`orgRootOf(dir)`: the org whose `work` root holds
the dir; `confineTo` → `Sandbox.WithOrg`), in `OpenSessionPod` — build,
evaluate, rebase, a merge batch's candidate and a main validation — and
in a validation that opens its own pod. A confined sandbox makes its
sessions' HOMEs under `<org root>/homes` (`OrgRoot.HomesDir`, 0700),
and `Session.Run`, `Session.StartDetached` and `Session.Exec` take their mounts from
`Session.CheckedMounts`, which runs every source through
`OrgRoot.Check(what, path)` **before a pod is made**:
`filepath.EvalSymlinks` of the source must be the org's resolved root
or under it. The first that is not refuses the session with a
`*sandbox.MountOutsideOrgError` — `errors.Is(err,
sandbox.ErrMountOutsideOrg)`, naming what it was, the path (and what it
resolved to), the org and its root — and no pod, agent or other mount
is made in its place. Refused, each by its own test in
`internal/sandbox/orgroot_test.go`:

| mount | refused when | test |
| ----- | ------------ | ---- |
| workspace | the checkout is outside the org's root (another org's work) | `TestConfinedSessionRefusesAWorkspaceOutsideTheOrgRoot` (Run and StartDetached start nothing) |
| home | the session's HOME is outside it | `TestConfinedSessionRefusesAHomeOutsideTheOrgRoot` |
| git common dir | a worktree in the org's work whose `.git` names a clone in another org's repos root | `TestConfinedSessionRefusesACommonDirInAnotherOrgsRepos` |
| package store | a store dir of another org (`Config.Stores`) | `TestConfinedSessionRefusesAnotherOrgsStore` |
| hooks | the checkout's hooks dir is outside it | `TestConfinedSessionRefusesAHooksDirOutsideTheOrgRoot` |
| any, by a symlink | a path inside the root that is a symlink out of it — followed, and refused naming the link and where it resolves | `TestConfinedSessionRefusesASymlinkOutOfTheOrgRoot` |

The worktree's own admin entry (`git dir`) and a detached agent's stream
directories (`stream dir`) are checked the same way. A source that
cannot be resolved is refused too. A checkout under today's single
`~/.mesh/work` — a node serving one org — is confined by nothing
(`TestUnconfinedSessionChecksNothing`, `TestOrgRootOfACheckout`). Not
checked: a validation's shared cache (`RunSpec.Cache`: the Go build
cache and the tree cache's read-only blobs, node-wide and
content-addressed, ADR-034 served diff), split per org later if at all.

**The host fallback refuses a second org.** A node with no sandbox —
`sandbox: off`, or a mode left out on a machine whose runtime does not
answer — runs sessions on the host as the node user, who can read every
org's checkouts, clones, kept sessions, stores and MCP data. So a node
serving two or more orgs (`len(Scopes) >= 2`) with no sandbox runs no
build and no rebase: `HostFallback(sb, scopes, orgID, dir)` answers a
`*HostFallbackError` (`errors.Is` `ErrHostServesSeveralOrgs`) naming the
org, the checkout and every org served, kinded `sandbox_required`
(`validate.SandboxRequired`, ADR-030's sixth kind), checked after the
item's spec and before its token, checkout or session. A build is
released (`ReleaseOnHostFallback`: `ReleaseWorkItem`, attempts
unchanged, reason `sandbox_required: <refusal>`; a refused release
fails it with the same reason) and held off the node for
`WorkspaceReleaseHold`; a rebase fails back to its builder with the same
reason. node.log says it once per run, `sandbox: off on a node serving
N orgs (…); … refused; every session is handed back until the sandbox
is on …`, each item's release one short line. A node serving one org
runs on the host as before. `internal/node/orgsandbox_refusal_test.go`:
`TestHostFallbackRefusesATwoOrgNodeWithSandboxOff`,
`TestHostFallbackAllowsAOneOrgNode`,
`TestRunItemOnATwoOrgNodeWithSandboxOffIsReleased` (no session, no
checkout, released, held, said once) and
`TestRunItemOnAOneOrgNodeWithSandboxOffRuns`. Evaluations, plans and
research are not refused here: an evaluation cannot be released by its
evaluator, and plan and research move into pods in the goal's item 4.

### What this item leaves to the goal's later items

`mesh node start` does not set `Roots` yet: until adoption
(`FindSessions`), the claims dir, the live probe, the disk floor and the
quartermaster's checks walk every org's Work, a node serving several
orgs keeps the one `~/.mesh/work`. The shared tree cache of ADR-034
(served diff) reads blobs by sha from `Git.ReposRoot`'s clones and is
content-addressed; it is not split here.

### What the goal's later items laid on it (amended 2026-10-18)

With `Roots` set, the rest of each org's tree is used as well as laid
out:

- **Package stores** (item 7). Each org's sandboxed sessions mount and
  name their own `~/.mesh/orgs/<org>/store/{pnpm,npm}`: `orgSandbox`
  (`sandbox.WithStores`, a per-org copy of the node's `Sandbox`) and
  `openOrgSessionPod`, used by build, evaluate, rebase, validation and
  main-validation pods and an adopted build's reattach; plan and
  research by `openScopePod`. One org, or nil `Roots`, keeps
  `~/.mesh/store`; an unserved org opens no pod (`package store: org
  roots: …`). `TestTwoOrgsSessionsInstallAgainstTheirOwnStore`.
- **Kept sessions** (items 11, 12). `harness.Job.KeptRoot` is the org's
  `OrgRoots.Kept` (`keptRootFor` by org id for build, evaluate and
  rebase, `keptRootOfScope` for plan and research; an unserved org
  refused; no roots `""`, the harness's `<work root>/kept-sessions`),
  and the harness writes and sweeps a kept stream there.
  `OrgRootSet.SweepKeptSessions` sweeps each org's Kept; the
  quartermaster's fifth check, `kept_sessions` (`KeptSessionsCheck`),
  runs it beside `<work root>/kept-sessions`.
- **Resume records** (item 9). Kept, looked up and swept per org under
  `OrgRoots.Resumes` (`OrgRootSet.SweepResumes`), each org's records
  only.
- **Plan and research in pods** (item 10). A plan and a research run in
  their org's pod (`openScopePod`, opened before the harness); a pod
  that cannot start releases the plan claim (`PodStartError`, no try
  counted) or hands in an empty research, each held off the node for
  `WorkspaceReleaseHold`; on a multi-org node with no sandbox both are
  refused `sandbox_required` as a build is.
- **MCP data** (item 5, ADR-035). A build's token file holds its own
  mesh session's bearer, under `OrgRoots.MCP`'s
  `sessions/<item>-<attempt>/` on the host or in the pod's HOME under
  `OrgRoot.HomesDir`.

`internal/node/two_orgs_sandboxed_e2e_test.go`
(`TestTwoOrgsSandboxedNodeNeitherOrgReadsTheOthersFiles`, item 6) holds
it together: one sandboxed node of an enterprise owner serving acme and
beta runs a build of each at once, then a plan and a research of each,
on the gatewaytest fake and the fake runtime. For every session it
asserts the job carries a pod; its mounts are its own org's checkout,
HOME, stores and — for a build's worktree — clone git dir, nothing
under the other org's tree or the node-wide store; its `KeptRoot` and
token file are its own org's; a build's token is its own mesh
session's id token, the two orgs' different and neither the machine's
(a plan's and a research's still the machine bearer, as ADR-035
leaves them); and, from inside the session, `Session.Run` on the
other org's work root, checkout, clone, kept sessions, pnpm store, npm
cache, MCP dir, a token file there, its HOMEs and the node-wide store
is each refused `ErrMountOutsideOrg` with no pod made.

## Consequences

- One org's checkout is never a worktree of another org's clone, and two
  orgs' clones of one repo share no objects, refs or `.git/worktrees`:
  the precondition for mounting one org's tree alone into its pods.
- A repo served for two orgs costs two clones' disk.
- `TestOrgRootsForTwoOrgsAreDisjoint`, `TestOrgRootsForOneOrgKeepTodaysPaths`,
  `TestOrgRootsRefuseABadOrgName`,
  `TestPrepareSameRepoUnderTwoOrgsMakesTwoClones` and
  `TestOrgSpecRefusesAnUnservedOrgAndFindsTheOrgsRepos`
  (`internal/node/orgroots_test.go`) hold it.
