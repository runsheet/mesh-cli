---
summary: One run of scripts/builder-idle-reading.sh, 2026-10-19, in this item's own build/evaluation container (not dl360-anythink) — recorded whatever its numbers showed (ADR-020); no threshold is judged here, the goal's own day-after-release reading is separate.
---

# Measurement: scripts/builder-idle-reading.sh

Taken in this item's own build/evaluation container
(`mesh-a-builder-tests-what-it-touched-and-neve-63379e3a-0`, `uname -a`
`Linux … 6.8.0-146-generic #146-Ubuntu SMP PREEMPT_DYNAMIC Thu Sep 3
16:12:30 UTC 2026 x86_64 GNU/Linux`), 2026-10-19T18:05:29Z, with:

```
./scripts/builder-idle-reading.sh
```

Verbatim output:

```
== builder-idle-reading taken 2026-10-19 18:05:29 +0000 on mesh-a-builder-tests-what-it-touched-and-neve-63379e3a-0
root: /tmp/mesh-home-3203042557/.mesh
log: /tmp/mesh-home-3203042557/.mesh/node.log
since: 2026/10/18 18:05:29
until: 2026/10/19 18:05:29

whole-suite calls: 0
log: /tmp/mesh-home-3203042557/.mesh/node.log not found
median build minutes: n/a
```

## Reading this

This container is this item's own build session, not a running `mesh
node` on dl360-anythink: there is no `~/.mesh/node.log` here and no
build session directories under `~/.mesh` for the script to find, so
both numbers read as the empty case — `whole-suite calls: 0` because no
build transcript was found at all (not because transcripts were found
and came up clean), and `median build minutes: n/a` because node.log
itself is absent. Neither is the goal's own evidence: the goal's
acceptance is this same script's reading taken the day after release on
dl360-anythink, where real build sessions and a real node.log exist.
This recording only proves the script runs cleanly (exit 0, both lines
present) outside that environment, as `scripts/builder_idle_reading_test.go`'s
`--dry` run against the checked-in fixture
(`scripts/testdata/builder-idle-reading/`) proves the counting logic
itself: two whole-suite `go test ./...` Bash calls across two build
session transcripts (a third session's matching call is correctly
excluded for being kind `evaluate`, not `build`), and a median of 10.0
minutes from node.log's three `usage build` lines (5, 10 and 15 minutes
of wall time).
