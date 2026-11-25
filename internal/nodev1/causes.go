package nodev1

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"projectmesh.io/mesh-cli/internal/actor"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/nodetools"
	"projectmesh.io/mesh-cli/internal/validate"
)

// The node says when a failure is the infrastructure's (mesh-specs
// mesh-v1-gaps-for-products.md, gap 14), as a hand-back cause Mesh v1
// takes (projectmesh-io/api server/leases.go), never as text a product
// must match:
//
//   - a session, its pod or a run_suite step killed by SIGKILL or SIGTERM
//     (exit 137 or 143) that the node did not cause is pod_killed, and
//     carries that exit_code (the host_killed class);
//   - a credential the lease needs — its Latchkey session, its model
//     account, its git token — that cannot be minted before the session
//     starts is credential_timeout: the session never runs;
//   - a node tool the kind names that this node cannot serve — one it
//     does not have, run_suite with nowhere to run its steps, its
//     runner failing under it, or a run_suite lane (a step or a shard)
//     that spent its own hard timeout having run not a single test —
//     stalled silent before a test ever started, so validate's own
//     stall watch, which only watches tests already running, never
//     named it — is tool_unavailable: the environment gave the lease
//     no verdict to report, so it is never the builder's failure.
//
// Each is a hand-back: the job keeps its place and is offered again,
// perhaps to a node that has what this one lacked.

// credentialError is a credential the lease needs that was not minted
// before its session started: the infrastructure's, or — product set —
// the product's configuration that Latchkey refused (a grant, a ref off
// the job, a connection to reconnect; mesh-specs gap 28 item 7).
type credentialError struct {
	err     error
	product bool
}

func (e *credentialError) Error() string { return e.err.Error() }
func (e *credentialError) Unwrap() error { return e.err }

// credentialFailed marks err, when there is one, as a credential not
// minted.
func credentialFailed(err error) error {
	if err == nil {
		return nil
	}
	return &credentialError{err: err}
}

// toolUnavailableError is a node tool the kind names that the node
// cannot serve.
type toolUnavailableError struct {
	tool, why string
}

func (e *toolUnavailableError) Error() string { return e.tool + ": " + e.why }

// killedError is errPodKilled with the exit code the agent or step was
// killed with.
type killedError struct {
	code int
	why  string
}

func (e *killedError) Error() string { return e.why }
func (e *killedError) Unwrap() error { return errPodKilled }

// unservedTool is the first node tool the brief's kind names that this
// node does not have, "" when it has them all.
func unservedTool(b *meshapi.Brief) string {
	for _, name := range b.NodeTools {
		switch {
		case isEndTool(name), slices.Contains([]string{nodetools.PushTool, nodetools.RunSuiteTool, nodetools.ApplyOntoTool, nodetools.SignReleaseTool, EscalateTool, RequestTimeTool}, name):
		default:
			return name
		}
	}
	return ""
}

// unservedSuites says why run_suite, when the kind names it, cannot be
// served: the lease has no runner for its steps and the node runs none
// on the host (podsuites.go). nil when it can.
func (r *Runtime) unservedSuites(b *meshapi.Brief, suites nodetools.SuiteRunner) error {
	if !slices.Contains(b.NodeTools, nodetools.RunSuiteTool) || suites != nil || r.hostSuites() {
		return nil
	}
	return &toolUnavailableError{tool: nodetools.RunSuiteTool, why: nodetools.ErrNoSuiteRunner.Error()}
}

// watchSuites is run's runner that ends the lease when the run says the
// infrastructure failed it: a step killed by SIGKILL or SIGTERM that the
// lease's own ctx did not stop ends it pod_killed with the step's exit
// code; a runner that could not run a step, a broken container runtime,
// or a lane that spent its own hard timeout having run not a single
// test, ends it tool_unavailable. The session still hears the run's
// answer; the lease is then handed back.
func watchSuites(run nodetools.SuiteRunner, end context.CancelCauseFunc) nodetools.SuiteRunner {
	if run == nil {
		return nil
	}
	return func(ctx context.Context, workspace string, steps []validate.Step, env []string) ([]validate.StepResult, error) {
		res, err := run(ctx, workspace, steps, env)
		if ctx.Err() != nil {
			return res, err
		}
		for _, s := range res {
			if actor.KilledExit(s.ExitCode) {
				end(&killedError{code: s.ExitCode, why: fmt.Sprintf("run_suite's step %s exited %d: killed, not by the node", s.Name, s.ExitCode)})
				return res, err
			}
		}
		if suitesUnavailable(res, err) {
			end(&toolUnavailableError{tool: nodetools.RunSuiteTool, why: suitesUnavailableWhy(res, err)})
		}
		return res, err
	}
}

// suitesUnavailableWhy is suitesUnavailable's own reason: err's, or, when
// it was a timed-out-empty lane that said so with no error of its own,
// naming the lane.
func suitesUnavailableWhy(res []validate.StepResult, err error) string {
	if err != nil {
		return err.Error()
	}
	for _, s := range res {
		if len(s.Shards) > 0 {
			for _, sh := range s.Shards {
				if sh.TimedOut && sh.TestsRun == 0 {
					return fmt.Sprintf("%s shard %d/%d timed out having run no test", s.Name, sh.Index+1, sh.Total)
				}
			}
			continue
		}
		if s.TimedOut && s.TestsRun == 0 {
			return fmt.Sprintf("%s timed out having run no test", s.Name)
		}
	}
	return "a run_suite lane timed out having run no test"
}

// suitesUnavailable says whether a run's error is its runner's, not its
// steps': a step the runner could not run at all (exit -1), a container
// runtime that is broken or refused its image, or a lane that spent its
// own hard timeout having run not a single test (timedOutBeforeATest).
func suitesUnavailable(res []validate.StepResult, err error) bool {
	if timedOutBeforeATest(res) {
		return true
	}
	if err == nil {
		return false
	}
	switch validate.Kinded(err) {
	case validate.RuntimeBroken, validate.ImagePullRefused:
		return true
	}
	return slices.ContainsFunc(res, func(s validate.StepResult) bool { return s.ExitCode < 0 })
}

// timedOutBeforeATest says whether any step or shard of res hit its own
// hard timeout (validate.StepResult.TimedOut) having reported no test at
// all: not a code failure the step could stand behind, since nothing it
// ran was ever judged, nor one validate's own stall watch could name —
// that watch only follows tests already running, and never opens one
// without a go test "run" event. A sharded step's TestsRun is the
// shards' sum, which can hide one empty shard behind a loud one, so each
// shard is read on its own (StepResult.Shards) rather than the merged
// step alone.
func timedOutBeforeATest(res []validate.StepResult) bool {
	for _, s := range res {
		if len(s.Shards) > 0 {
			if slices.ContainsFunc(s.Shards, func(sh validate.ShardResult) bool {
				return sh.TimedOut && sh.TestsRun == 0
			}) {
				return true
			}
			continue
		}
		if s.TimedOut && s.TestsRun == 0 {
			return true
		}
	}
	return false
}

// infrastructureCause is the hand-back cause of a lease whose session
// could not start, or was stopped, for the infrastructure's sake:
// credential_timeout (product, when Latchkey refused the product's
// configuration), tool_unavailable, a harnessError's own cause
// (cannot_run, model_rate_limited; harnessroute.go), or none ("").
func infrastructureCause(err error) string {
	var ce *credentialError
	var te *toolUnavailableError
	var he *harnessError
	switch {
	case errors.As(err, &he):
		return he.cause
	case errors.As(err, &ce) && ce.product:
		return meshapi.CauseProduct
	case errors.As(err, &ce):
		return meshapi.CauseCredentialTimeout
	case errors.As(err, &te):
		return meshapi.CauseToolUnavailable
	}
	return ""
}
