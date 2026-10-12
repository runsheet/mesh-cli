---
summary: The first reading of scripts/mcp-auth-reading.sh, taken on the build machine for this item against the test fixture — neither a DL360 node.log nor gcloud/a GCP project are reachable here, so this exercises the script's fixture and "gcloud missing" paths, not the dl360's own node.log or Cloud Logging's /mcp 401 count.
---

# mcp-auth-reading: the build machine for item 4

Command:

```
scripts/mcp-auth-reading.sh --log scripts/testdata/mcp-auth-node.log --now "2026-10-11 06:00:00"
```

Taken: 2026-10-10T17:17:58Z, on host
`mesh-a-sessions-mcp-token-outlives-the-sessio-df21cd31-0` (the
container this item's build session ran in), as user `mesh`.

Output as printed:

```
== reading taken 2026-10-11 06:00:00 +0000 on mesh-a-sessions-mcp-token-outlives-the-sessio-df21cd31-0
window: last 24h, 2026/10/10 06:00:00 to 2026/10/11 06:00:00

== node.log
log: scripts/testdata/mcp-auth-node.log
rejected Authorization header lines: 4

== /mcp 401s (Cloud Logging)
filter: httpRequest.requestMethod="POST" AND httpRequest.requestUrl:"/mcp" AND httpRequest.status=401 AND timestamp>="2026-10-10T06:00:00Z" AND timestamp<"2026-10-11T06:00:00Z"
skipped: no gcloud on PATH
exit: 0
```

Honest notes on this reading:

- This build machine is not a DL360 node and has no `~/.mesh/node.log`
  of its own, and no `gcloud` binary on `PATH` at all — so `--log` is
  pointed at the test fixture, `scripts/testdata/mcp-auth-node.log`,
  and `--now` is fixed to the fixture's own window (2026-10-10
  06:00:00Z to 2026-10-11 06:00:00Z) rather than the real clock, so the
  count is the fixture's known number (4), not a live reading. The
  Cloud Logging half is exercised on its "no gcloud on PATH" path, not
  against a real project — printed as `skipped`, not a failure, which
  is the script's documented behaviour for this case (and for "no
  gcloud" is the same behaviour whether or not `--project` is given).
- The fixture's 4 counted lines are, by design, the ones inside the
  stated window; the script also has to skip one before the window
  (05:00:00, an hour short), one that lands exactly on the window's
  exclusive upper bound (06:00:00 the next day), one an hour after it,
  and one continuation line with no `mesh node: ` timestamp prefix at
  all (a panic's stack, as `internal/node/logfile.go` and ADR-019
  describe such lines landing) — `go test ./scripts/...`'s
  `TestMcpAuthReadingFixtureCount` is what proves the count against the
  fixture, this is that same run recorded by hand.
- This is not the goal's own dl360 observation (the goal statement's
  ten sessions across both DL360 nodes' real node.log, and Cloud
  Logging's ~14 POST 401s per 40 minutes on the real `/mcp`): that
  reading belongs to the goal, taken on a DL360 itself with a real
  `--project` once the rest of this goal's items land there, not to
  this item. This item's acceptance is that the script exists, runs,
  and is tested against a fixture of a known count, with one reading
  recorded honestly, whatever it showed — proven above and by
  `go test ./scripts/...`.
