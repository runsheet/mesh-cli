---
summary: The first reading of scripts/mesh-home-reading.sh, taken on the build machine for this item — podman is not installed here, so the reading is itself an example of the "podman missing" path the script documents.
---

# mesh-home-reading: the build machine for item 4

Command:

```
scripts/mesh-home-reading.sh
```

Taken: 2026-10-10T09:40:58Z, on host
`mesh-a-sessions-temporary-home-is-removed-whe-2e948555-0` (the
container this item's build session ran in), as user `mesh`.

Output as printed:

```
== mesh-home dirs
/tmp/mesh-home-1536386016	bytes=544815593	mounted=no
dirs: 1	bytes: 544815593	mounted: 0/1

== running mesh pods
(none)

== pods not running
(none)

== exited mesh containers
(none)

note: podman: podman not found

stale
/tmp/mesh-home-1536386016	reason=not mounted
exit: 0
```

Honest notes on this reading:

- This build machine is not a mesh node running sandboxed sessions in
  podman — it is this very build session's own container, with no
  `podman` binary on `PATH` at all. The reading therefore exercises the
  script's "podman missing" path (its `note: podman: … not found`
  line), not the dl360's actual podman state; it is recorded anyway,
  because the brief this script answers (and ADR-020) says a podman
  that cannot be read is a reading too, not a reason to skip one.
- The one `/tmp/mesh-home-*` directory found, 544,815,593 bytes
  (~520 MiB), is `mesh-home-1536386016` — almost certainly this session
  (or a sibling session on the same box) working-set files, not a
  sandbox HOME the dl360 incident describes; with no podman to ask, the
  script correctly cannot tell the two apart and reads it as unmounted
  (`mounted=no`), hence `stale`.
- `exit: 0` whatever the verdict (`ok` or `stale`): this is the
  measurement item (ADR-020's `Measure:` kind), never a merge gate on
  its own number.

This is not the goal's own day-long dl360 observation (the goal
statement's 370 dirs / 150 GB / 27+21 exited containers / 11+8
non-running pods figures, and the acceptance that `/tmp/mesh-home-*`
stays at the number of running sessions over a day): that reading
belongs to the goal, taken on the dl360 itself once items 1–3 of this
goal are in place there, not to this item. This item's acceptance is
that the script exists, runs, and is tested against fixtures — proven
above and by `go test ./scripts/...` — with one reading recorded
honestly, whatever it showed.
