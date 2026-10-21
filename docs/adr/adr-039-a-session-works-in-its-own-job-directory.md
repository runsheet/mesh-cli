---
summary: Every unit of work (an item, a goal's plan, a research direction, a merge batch) has its own ~/.mesh/jobs/<job-id>/, named by a job.json that says whose it is (connection, unit, item or goal or direction or batch), and its sessions mount only that and its connection's caches under ~/.mesh/connections/<connection-id>/{repos,store}; a thread keeps its state across sessions in ~/.mesh/threads/<thread-id>/ as a cache of what the product holds. Supersedes ADR-034's per-org roots; no single-org layout and no migration.
status: proposed
---

# ADR-039: A session works in its own job directory

2026-10-21. Supersedes ADR-034 (`adr-034-each-org-has-its-own-roots.md`)
and the paths of ADR-028 (`adr-028-one-clone-per-repo-one-worktree-per-item.md`).
Follows runsheet ADR 066: a node is connected to (tenant, product) pairs,
and a connection grants that product's jobs for that tenant and nothing
else.

## Context

ADR-034 gave each org a tree, `~/.mesh/orgs/<org>/`, and kept today's
paths for a node serving one org. It was laid out but never used:
`mesh node start` did not set `Roots`, because adoption, the claims dir,
the live probe, the disk floor and the quartermaster all walked one
`~/.mesh/work`. Meanwhile ADR 066 made the boundary the connection, not
the org: the same tenant can be served for two products, and a product's
session tokens, kept streams and resume records are that product's, not
the tenant's in general.

Keying by org name also cost: names had to be one path segment, unique
in case (macOS), and a renamed org moved its tree.

Chris, 2026-10-21: "maybe jobs and threads just have an id we use?" —
and, with every node down and the server cleaned, "we can go to end
state now": no sessions to adopt from the old paths, nothing to migrate.

## Decision

### Three trees, each keyed by an id

| tree                                 | holds                                                             | lives                                   |
| ------------------------------------ | ----------------------------------------------------------------- | --------------------------------------- |
| `~/.mesh/jobs/<job-id>/`             | one unit of work's checkouts, scratch, kept streams, resume record, MCP token files, session dirs | until its item, plan or batch is done (the sweep) |
| `~/.mesh/threads/<thread-id>/`       | a thread's state between its sessions                             | until the thread closes, its connection is revoked, or it is idle 14 days |
| `~/.mesh/connections/<connection-id>/` | the connection's shared clones (`repos/`), package stores (`store/`) and repo locks (`.locks/`) | until the connection is revoked          |

No path carries an org or product name. Every id is a uuid.

### A job

A job is one unit of work and every session of it: an item (its
builds, their validations and resumes, its evaluation, its rebases), a
goal's plan, a research direction, a merge batch's candidate or a merged
item's main validation. Its key is the platform's id of that work — the
item's, goal's, direction's or batch's (`<item>-main` for a main
validation, which outlives its item's job) — so a build retried, a
session adopted after a restart, or an evaluation of the same item all
find the same directory. `job.json` says whose it is:

```json
{"id": "…", "unit": "item", "connection": "…", "orgId": "…",
 "product": "runsheet", "itemId": "…", "goalId": "…", "threadId": "",
 "label": "ship-it/3", "started": "2026-10-21T10:00:00Z"}
```

Under it:

| path                 | what                                                  |
| -------------------- | ----------------------------------------------------- |
| `checkout/`          | an item's build checkout, a worktree of the connection's clone |
| `eval/`, `rebase/`   | its evaluation's clone, its rebase's worktree          |
| `plan/`, `research/` | a plan's or research's full clones under `repos/`      |
| `<what>-<attempt>/`  | a batch candidate's or main validation's checkout      |
| `<dir>.session`      | each session's state, beside its checkout (sessionunit)|
| `.scratch/`          | host runs' TMPDIRs                                     |
| `kept-sessions/`     | kept streams                                           |
| `resume/`            | a build's resume record                                |
| `mcp/`               | session token files and proxy sockets                  |
| `homes/`             | sandboxed sessions' HOMEs                              |

`openJob` writes `job.json` before anything is checked out; a job is
never reopened for another connection.

What walked `~/.mesh/work` walks `~/.mesh/jobs`:

- **Adoption** after a restart (`FindSessions`) and the **live probe**
  find `*.session` dirs under the jobs tree.
- **Plan and research claims** are recorded in `~/.mesh/jobs/.claims`,
  as they were in the work root's.
- **The work sweep** (`PlanJobSweep`) reads each `job.json` and asks the
  platform for its item or goal by id: an item's job goes once it is
  merged, cancelled or superseded, or failed a week; a plan's once its
  goal is not planning; a batch's six hours on; a research's stays (its
  clones are fetched over for the next research). Any job whose
  connection is not among the node's answer goes, once nothing in it
  runs. A directory with no `job.json` is left and logged.
- **The dead-checkout check** reads item jobs' checkouts by item id and
  prunes each connection's clones under that connection's locks.
- **Kept sessions** are swept per job.
- **The disk floor** measures the jobs tree.
- **The stop list** (ADR 066, decision 8) names sessions by kind and
  item, as before.

A session's pod is confined to its job's directory and its
connection's directory (`sandbox.Confinement`): every mount outside them
is refused with `ErrMountOutsideJob`, naming the job.

### A thread

A thread is a product's conversation with a tenant (Wardroom's thread
sessions, ADR 066's `(grapevine, wardroom)`): many sessions over time,
one at a time. Each turn is a job whose `job.json` names the thread; its
pod also mounts `~/.mesh/threads/<thread-id>/`, which holds
`thread.json` (connection, product, thread id, last turn) and whatever
the product's agent keeps between turns: the harness's resume state,
notes, a long-lived checkout.

The product holds the thread; the node's directory is a cache. A thread
may move to another node, which starts it from what the product serves
(context is fetched, not shipped). So a thread directory is dropped when
the thread closes, when its connection is revoked, or when it has been
idle 14 days, and losing it costs a cold start, not the thread. A node
runs one turn of a thread at a time (a lock in the thread directory).

### Two kinds, by how long they live

A node runs jobs and threads and nothing else; what a session is for
does not make a third kind. A job remembers nothing past itself; a
thread remembers across its turns. What starts a thread's turn is the
product's business, not the node's:

- **a person's message**: a chat;
- **an event**: a standing role (steward, quartermaster, treasurer,
  herald, Tripline's SRE) is a thread whose turns come from alerts,
  merges and incidents rather than messages, keeping its memory for its
  tenant in its thread directory;
- **a schedule**: a digest or review that remembers the last one is a
  thread; a nightly sweep that does not is a job.

### A thread starts jobs

A thread may hand work off: a chat or a role asks for a build. The job's
`job.json` names its parent (`threadId`), and the job's result is
reported to that thread by the product. The job gets its own directory
and does not mount the thread's.

### Graphs are the product's

A graph of jobs (a goal's items, goals that depend on goals, a merge
batch, any product's pipeline) is held by the product, which offers each
step when it is ready; a node runs each step as a job and knows nothing
of the graph. So a step's output reaches the next step through the
product (a changeset or blob in its store, runsheet ADR 058), never
through a node's disk: the next step may run on another node, under
another connection, hours later. No job reads another job's directory.

### A person may watch or take a job

Following a running job live, or stepping in to steer it, is a flag on
the job (`attach`: `none`, `watch`, `steer`), set by the product, not a
kind of its own. It says only who may attach to the session's stream
and whether their input reaches it. Not built yet.

### Not sessions

Long-running processes that are not agents (an app's preview
environment, a service a session uses) are not jobs or threads; a
session's services stay its pod's sidecars (`.mesh/services.yml`).

### Connection caches

Clones, package stores and repo locks stay shared, since a clone per
job would re-clone every session, but only within a connection: a worktree's
`.git` points into its clone, which the pod mounts, and a package store
can be poisoned for whoever installs from it next. The node's answer
names each scope's connection id (`ids.NodeConnectionID(node, org,
product)`); `connection.json` beside `repos/` and `store/` records the
org and product. Revoking a connection removes its tree once its jobs
are gone. The served-diff tree cache stays content-addressed and shared,
as ADR-034 had it.

### One layout and one mode

Every node uses it, serving one connection or many. `~/.mesh/work`,
`~/.mesh/repos`, `~/.mesh/store`, `~/.mesh/resume`, `~/.mesh/mcp` and
`~/.mesh/orgs/` are no longer read or written by a node; `OrgRoots`,
`OrgRootSet`, `SingleOrgRoots` and `ValidateOrgName` are removed. A
scope the platform names no connection for opens no job: the node's
answer must carry `connectionId` (runsheet serves it with `cap` and
`product`).

Channel mode is removed with it (Chris, 2026-10-21): one host session
across every item's checkout cannot be confined to a job. Every node
runs one session per job (what `--mode poll` was); `--mode` is kept,
deprecated, so an installed service unit still starts, and refuses
anything but `poll`.

The Go build cache and the served-diff tree cache stay under the user
cache dir, outside `~/.mesh`, content-addressed and not split.
