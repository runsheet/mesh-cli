---
summary: mesh node start opens ~/.mesh/node.log itself and rotates it at 10 MB keeping 5 (node.log.1 … node.log.5); what sessions say goes to ~/.mesh/sessions.log (20 MB, 2 kept); the systemd unit sends stdout and stderr to the journal and the LaunchAgent to ~/.mesh/node.crash.log, never to node.log.
status: accepted
---

# ADR-019: The node writes its own log

2026-09-29, goal `node-yaml-takes-effect-or-says-why-not` (item 4).
Changes the service definitions `mesh node service install` writes
(`internal/node/service.go`) and where `mesh node start` logs.

## Context

The systemd unit said `StandardOutput=append:~/.mesh/node.log` and
`StandardError=append:~/.mesh/node.log`; the LaunchAgent's
`StandardOutPath`/`StandardErrorPath` named the same file. The node
logged to stderr and the service manager appended it. The service held
the descriptor, so the node could not rotate the file, and nothing
else did: on the mini it reached **339 MB**. It is the only place the
node's reasoning is written (the journal held nine lines), and at that
size the absence of a `capacity 6 → 8` line took a machine to confirm.

Much of it was not the node's reasoning at all: channel mode's
`session: …` line per text block, every git command of every workspace
with its output (`Git{Log: os.Stderr}`), and each agent's stderr
mirrored through the harness.

## Decision

- `mesh node start` opens `~/.mesh/node.log` through `RotatingFile`
  (`internal/node/logfile.go`): at 10 MB (`NodeLogMaxBytes`) it is
  renamed `node.log.1`, older ones shift up to `node.log.5`
  (`NodeLogKeep`) and the sixth is deleted. A file already at or past
  the cap when the node opens it is rotated at once. **No write passes
  the cap**: one larger than the cap on its own is cut to it, ending
  `[log: a <n>-byte write cut to the <cap>-byte cap]`, and a write the
  file cannot rotate for is refused with the error — dropped, never
  appended past the cap — said once on stderr (the journal or
  node.crash.log) when the rotation first fails and once when it works
  again; each later write retries the rotation. No dependency: the
  writer is the standard library.
- What sessions say goes to `~/.mesh/sessions.log`, the same writer at
  20 MB keeping 2: channel mode's `session:` text, `Git.Log`, the
  Claude and Codex harnesses' `Stderr`. A line the node writes *about*
  a session — its start, its end, its usage record, what its end killed
  — stays in node.log: `mesh node usage` reads it there, and it is the
  node's own account of what it did.
- The systemd unit's `StandardOutput=journal` and
  `StandardError=journal`; the LaunchAgent's output goes to
  `~/.mesh/node.crash.log`. They get only what the node cannot write
  itself: a panic's stack, and the `restarting: exec` line (written to
  node.log as well). The error that ends a start is written to node.log
  (`exiting: …`) before the process returns it.
- From a terminal both logs are echoed to stderr; under a service they
  are not, so the journal does not get a second copy.

## Consequences

- node.log never exceeds about 60 MB with its rotated files, and
  sessions.log about 60 MB.
- A unit installed earlier keeps its `append:` redirect until `mesh node
  service install` is run again. The node still rotates node.log under
  it, but systemd's descriptor follows the renamed file, so a panic's
  stack lands in whichever `node.log.N` that file has become. The build
  does not touch an installed unit.
- `mesh node usage` reads the rotated node.logs oldest first, then
  node.log, when `--log` is not given.

## Amendment — 2026-10-02 (goal node-yaml-takes-effect-or-says-why-not, item 11)

- 2026-10-02 10:54Z, the mini on v0.2.222 under real work
  (`scripts/node-reload-reading.sh`): node.log grows about 237 KB an
  hour — 4,344,540 bytes in the 66,043 s since the 2026-10-01 16:33:14Z
  rotation that closed `node.log.1` at 10,485,596 bytes — so the 10 MB
  cap is reached about every 44 hours and 10 MB × 5 keeps about nine
  days. sessions.log grows about 9 MB an hour (`sessions.log.1`,
  20,760,274 bytes, in the 2 h 15 min after `sessions.log.2` closed), so
  20 MB × 2 keeps four to six hours. The mini's unit still carries the
  pre-ADR `StandardOutput=append:~/.mesh/node.log` redirect; `mesh node
  service install` is the operator's to run.
