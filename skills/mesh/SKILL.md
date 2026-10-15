---
name: mesh
description: Work a Mesh goal's items from Claude Code — list goals, join an item, check status, switch items and finish — on the mesh_* tools mesh mcp serves.
---

# Work a Mesh goal from Claude Code

`mesh mcp` (added once with `claude mcp add runsheet -- mesh mcp`) serves
`mesh_goals`, `mesh_join`, `mesh_switch`, `mesh_status`, `mesh_progress`,
`mesh_ask`, `mesh_finish` and `mesh_release` on your own login. This
skill is the five everyday forms.

## `/mesh goals`

Call `mesh_goals` (`org`, `goal` both optional — narrow to one org's
login or one goal's slug). Tell the person each live goal, its open
items and their status, so they can pick one to join.

## `/mesh join <goal> [seq] [--pair]`

Call `mesh_join` with `goal` (required), `item` set to `seq` when one
was given (else the goal's next ready item), and `pair: true` when
`--pair` was given (joins the branch another holder is already working
on, with no claim of your own). The answer carries `path`, `branch`,
`brief`, `acceptance` and `context`. Tell the person: `cd` to the
answered `path`, read the brief and the acceptance so they know what
the item is asking for, and that they are on `branch` — commits go
there, not on `main`.

## `/mesh switch <goal> [seq]`

Call `mesh_switch` with `goal` (required) and `item` set to `seq` when
one was given (else the target's next ready item); `from` only when you
hold more than one item and need to name which to leave. This pushes
the held item's commits, notes why, releases it, and joins the target —
answering like `/mesh join`. Tell the person the same as a join: `cd`
to the new `path`, read the brief and acceptance, work on `branch`.

## `/mesh status`

Call `mesh_status` with no arguments. Tell the person each held item's
goal, seq, path and branch, and flag any with uncommitted changes or
commits not yet pushed past base.

## `/mesh finish`

Before calling the tool, make sure the person's work is committed —
`mesh_finish` pushes the branch as it stands; nothing uncommitted goes
with it. Then call `mesh_finish` (`item` only when several are held;
`outcome: already_done` and `evidence` instead, with no commit needed,
when the acceptance already holds on `main`). If the answer is a
`push refused:` message, relay it to the person verbatim — it names the
`git pull --rebase` to run and that they should finish again after.
Otherwise tell them the item is finished and what was pushed.
