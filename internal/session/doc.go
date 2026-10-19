// Package session runs a build as an ordered list of steps, like an Actions
// job: checkout, exchange, fetch, pod, run, validate, finish, release. Main
// steps run strictly in order and stop at the first failure; steps marked to
// always run (cleanup, release) still run afterwards, in order, whatever came
// before. Run returns one record per step — name, started, ended, outcome,
// error — the node logs as `step <n> <name>: <outcome>` (runsheet
// adr-061-mesh-is-a-job-system §3). The node's build runs checkout,
// exchange, pod, run, validate, finish and release (node.Runner.RunItem,
// mesh-cli ADR-038); End lets a step end the job without failing it.
package session
