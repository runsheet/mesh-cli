package nodev1

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/nodetools"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/validate"
)

// Where run_suite runs (Mesh v1 spec, "Harness on every job": node tools
// are deterministic actions the node runs). Its steps come from
// .runsheet/validate.yml in the lease's checkout, which the session
// itself writes, so they are the session's commands: they run where the
// session's work runs, never as the node.
//
//   - A lease with a pod runs each step in it: a short container of the
//     pod's image beside the brief's sidecars, the checkout mounted at
//     its own path and nothing of the node's environment.
//   - A node with a sandbox whose lease has no pod refuses run_suite
//     (nodetools.ErrNoSuiteRunner): there is nowhere the steps may run.
//   - Only a node explicitly unsandboxed (HostSuites, and no Sandbox),
//     whose sessions already run on the host, runs them on the host.

// suiteRunner is run_suite's runner for a lease: pod's when the lease
// has one, else nil — which the tool runs on the host only when
// hostSuites says so (nodeTools).
func (r *Runtime) suiteRunner(b *meshapi.Brief, pod string) nodetools.SuiteRunner {
	if pod == "" || r.Sandbox == nil {
		return nil
	}
	return r.podSuites(pod, suiteLimits(b.Resources))
}

// hostSuites says whether run_suite may run its steps on the host: only
// on a node explicitly unsandboxed (HostSuites) that has no sandbox.
func (r *Runtime) hostSuites() bool {
	return r.HostSuites && r.Sandbox == nil
}

// podSuites runs validate's steps in the lease's pod, every step run
// whatever an earlier one did.
func (r *Runtime) podSuites(pod string, limits sandbox.Limits) nodetools.SuiteRunner {
	var n atomic.Int64
	return func(ctx context.Context, workspace string, steps []validate.Step, env []string) ([]validate.StepResult, error) {
		run := podStep{rt: r.Sandbox, pod: pod, image: r.PodImage, dir: workspace, limits: limits, n: &n, env: env}
		return validate.Run(ctx, steps, run, validate.Options{Dir: workspace, ContinueOnFailure: true})
	}
}

// suiteEnv is a pod step's whole environment: none of the node's.
var suiteEnv = []string{"PATH=" + sandbox.DefaultPath, "HOME=/tmp", "LANG=C.UTF-8", "TERM=dumb", "GIT_TERMINAL_PROMPT=0"}

// podStep is a validate.Runner whose every step is a container in the
// lease's pod.
type podStep struct {
	rt     sandbox.Runtime
	pod    string
	image  string
	dir    string
	limits sandbox.Limits
	n      *atomic.Int64
	// env is the run's selection (nodetools.SelectionEnv), added to
	// suiteEnv.
	env []string
}

// Run implements validate.Runner: a step past its timeout is exit 124,
// as HostRunner's.
func (s podStep) Run(ctx context.Context, cmd string, timeout time.Duration, stdout, stderr io.Writer) (int, error) {
	if timeout <= 0 {
		timeout = validate.DefaultTimeout
	}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	exit, err := s.rt.RunAgent(sctx, sandbox.Pod{Name: s.pod}, sandbox.AgentSpec{
		Name:           fmt.Sprintf("%s-suite-%d", s.pod, s.n.Add(1)),
		Image:          s.image,
		Workdir:        s.dir,
		Mounts:         []sandbox.Mount{{Source: s.dir, Target: s.dir}},
		Env:            append(append([]string(nil), suiteEnv...), s.env...),
		Command:        []string{"sh", "-c", cmd},
		Limits:         s.limits,
		TimeoutSeconds: int(timeout/time.Second) + 30,
		Stdout:         stdout,
		Stderr:         stderr,
	})
	if ctx.Err() == nil && errors.Is(sctx.Err(), context.DeadlineExceeded) {
		return 124, nil
	}
	if err != nil {
		return -1, fmt.Errorf("the lease's pod %s: %w", s.pod, err)
	}
	return exit.Code, nil
}

// suiteLimits are a pod step's limits: the brief's CPUs and memory.
func suiteLimits(res meshapi.Resources) sandbox.Limits {
	var l sandbox.Limits
	if res.CPUs > 0 {
		l.CPUs = res.CPUs
	}
	if res.MemGB > 0 {
		l.Memory = fmt.Sprintf("%dm", int(res.MemGB*1024))
	}
	return l
}
