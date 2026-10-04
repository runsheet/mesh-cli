---
summary: One run of scripts/host-backoff-reading.sh on the Mini, 2026-10-04, recorded whatever its numbers showed (ADR-020); the deployed node binary there predates this goal's heartbeat and status changes, so its output carries no `host:` line yet.
---

# Measurement: scripts/host-backoff-reading.sh

Taken on the Mini (`hostname` `mini`, `uname -a` `Linux mini 6.8.0-142-generic
#142-Ubuntu SMP PREEMPT_DYNAMIC Wed Sep 2 14:24:27 UTC 2026 x86_64`),
2026-10-04T17:09:21Z, with:

```
./scripts/host-backoff-reading.sh --count 1
```

Verbatim output:

```
== host-backoff-reading taken 2026-10-04T17:09:21Z on mini
load: 1min=1.14 5min=0.94 15min=1.94
cores: 18
temperature: 55.0°C
node:
owned by chris@runsheet.dev (person); serving purserid, wardroomid, latchkeyid, projectmesh-io, foghornid, chriskolenko, go-apis, anythinkio, triplineid, runsheet
Credential: an rs_* console token (~/.mesh/token) — the operator's own bearer, the fallback. `mesh node login` registers a Latchkey machine instead.
This machine: node running (pid 2438852).
ORG             NAME            SLUG            STATUS   RUNNING  CAPACITY  VERSION   LAST_SEEN
purserid        Mini            mini            retired  2        8         v0.2.247  2026-10-03T22:19:51.11309+00:00
wardroomid      Mini            mini            retired  2        8         v0.2.247  2026-10-03T22:20:29.553238+00:00
latchkeyid      Mini            mini            retired  2        8         v0.2.247  2026-10-03T22:19:44.42951+00:00
latchkeyid      Chris's laptop  chris-s-laptop  retired  0        1         dev       2026-09-21T05:59:12.316599+00:00
projectmesh-io  Mini            mini            offline  0        8         v0.2.286  2026-10-03T22:26:38.213709+00:00
projectmesh-io  Chris's laptop  chris-s-laptop  retired  0        1         dev       2026-09-21T05:59:13.09809+00:00
foghornid       Mini            mini            retired  2        8         v0.2.247  2026-10-03T22:20:15.102392+00:00
foghornid       grapevine       grapevine       retired  0        4         v0.2.264  2026-10-03T13:57:27.351381+00:00
chriskolenko    Mini            mini            retired  2        8         v0.2.247  2026-10-03T22:20:09.702581+00:00
chriskolenko    Chris's laptop  chris-s-laptop  retired  0        1         dev       2026-09-21T05:59:12.702269+00:00
go-apis         Mini            mini            retired  2        8         v0.2.247  2026-10-03T22:20:20.018554+00:00
anythinkio      Mini            mini            retired  2        8         v0.2.247  2026-10-03T22:20:06.483171+00:00
triplineid      Mini            mini            retired  2        8         v0.2.247  2026-10-03T22:20:03.351158+00:00
runsheet        Mini            mini            retired  2        8         v0.2.247  2026-10-03T22:19:57.935035+00:00
runsheet        Chris's laptop  chris-s-laptop  retired  0        1         dev       2026-09-21T05:59:11.911927+00:00

SCOPE                                                             KIND   SUBJECT                               PLATFORM
mesh-session-build-a300139f-2e1a-4c1d-9d8f-639bb05aa579-4344cef7  build  a300139f-2e1a-4c1d-9d8f-639bb05aa579  live (item running)
mesh-session-build-e395730f-2ea2-4988-9cf3-b61098739ec1-94443056  build  e395730f-2ea2-4988-9cf3-b61098739ec1  live (item running)
sessions: 2 live scope(s), 0 leaked
host: no host: section in /home/chris/.mesh/node.yaml
```

## Reading this

The deployed node on the Mini (`v0.2.247`, pid 2438852) was built before
this goal's items 1–4 landed: it runs no `host:` back-off yet, so its
`mesh node status` output above shows the plain `CAPACITY` column (`8`)
with no reason — and `~/.mesh/node.yaml` there carries no `host:` block,
so the script's own `host:` line falls back to saying so. The package
temperature read directly (55.0°C, the `x86_pkg_temp` or `TCPU` zone)
moved between 55°C and 92°C across runs taken seconds apart while this
goal's own builder sessions ran on the same box — past this goal's
default `package_temp_max_c` (90) on one of those runs and under it on
others, which is the ordinary weather a shared host has, not a fault in
the script or the probe; ADR-020 treats this reading as informational;
no item depends on its number.

**What the capacity line means once a node with this goal's changes
runs here**: `mesh node status` and the heartbeat's `scaling` carry the
capacity *in force* next to the reason it is held there. A line like

```
CAPACITY
4/1–8 (host hot: load 55/18 cores)
```

means the plan figure (the range, or a fixed node's `N`) is `1–8`, but
the back-off has stepped the capacity actually claimed down to `4`
because the load or package temperature is past its threshold — not
because the plan window lowered it. Once the host is quiet for five
minutes running, the figure climbs back a slot at a time toward `8` and
the reason returns to the plan's own (`max`, a pace reason, or none).
