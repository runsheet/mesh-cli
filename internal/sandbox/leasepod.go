package sandbox

import (
	"context"
	"fmt"
	"io"
	"time"
)

// A Mesh v1 node (internal/nodev1, mesh-cli ADR-044) runs each lease's
// session as a job actor that owns the lease's pod: the actor names the
// pod before it exists, so actor.json can name it for a node that
// restarts, and labels it with the job, the node and the lease, so the
// supervisor's reconcile can tell the node's pods and their actors from
// any other. The CPUs and memory a lease's agent may use are the ones
// the node's scheduler leased it, not the sandbox's one size for every
// session. And the session's agent is long-lived and streaming: it runs
// beside its services, and beside the short commands its node tools run
// in the same pod (Exec), which a Run would remove the pod under. None
// of this changes a session that does not ask for it.

// podPin is a session's pinned pod name and extra labels (Pin).
type podPin struct {
	name   string
	labels map[string]string
}

// podName is the name of the session's nth pod: the pinned name for the
// first, name-<n> for each after.
func (p *podPin) podName(n int) string {
	if n == 0 {
		return p.name
	}
	return fmt.Sprintf("%s-%d", p.name, n)
}

// Pin names the session's pods — its first pod name, each one after
// name-<n> — and labels them with labels beside SessionLabel and
// mesh.kind, which they keep. Call it before Start or the first Run;
// an empty name keeps the session's own.
func (s *Session) Pin(name string, labels map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == "" {
		name = s.name
	}
	copied := make(map[string]string, len(labels))
	for k, v := range labels {
		copied[k] = v
	}
	s.pin = &podPin{name: name, labels: copied}
}

// PodName is the name the session's first pod has, or will have once it
// is made.
func (s *Session) PodName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pods) > 0 {
		return s.pods[0].Name
	}
	if s.pin != nil {
		return s.pin.podName(0)
	}
	return s.name + "-0"
}

// SetLimits replaces the sandbox's agent Limits for this session's
// agent, Runs and Execs: a field left zero keeps the sandbox's own.
func (s *Session) SetLimits(l Limits) {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := s.sb.Config.Limits
	if l.CPUs > 0 {
		base.CPUs = l.CPUs
	}
	if l.Memory != "" {
		base.Memory = l.Memory
	}
	if l.Pids > 0 {
		base.Pids = l.Pids
	}
	s.agentLimits = &base
}

// limits are the session's agent limits: SetLimits', else the
// sandbox's.
func (s *Session) limits() Limits {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agentLimits != nil {
		return *s.agentLimits
	}
	return s.sb.Config.Limits
}

// RunAgent runs the session's agent to its end, streaming, in the
// session's pod — the one Start made with its services, else the one an
// Exec made, else one made now and kept — as AgentName(pod), so the
// pod's watchers find it by that name. The pod is never removed here,
// and an Exec while the agent runs runs beside it: Close removes the
// pod. A cancelled ctx stops the agent's container and returns
// ctx.Err(); a kill by a limit is a *LimitError; any other non-zero exit
// an *ExitError.
func (s *Session) RunAgent(ctx context.Context, spec RunSpec) error {
	cfg := s.sb.Config
	mounts, err := s.CheckedMounts(spec.Workspace)
	if err != nil {
		return err
	}
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	var pod Pod
	if started != nil {
		pod = *started
	} else {
		if pod, err = s.createPod(ctx); err != nil {
			return err
		}
		s.mu.Lock()
		s.started = &pod
		s.mu.Unlock()
	}
	name := AgentName(pod)
	env := SetEnv(s.Env(spec.Env), spec.Vars)
	tail := &tailBuffer{max: 8 << 10}
	stderr := io.Writer(tail)
	if spec.Stderr != nil {
		stderr = io.MultiWriter(spec.Stderr, tail)
	}
	start := time.Now()
	exit, err := s.sb.Runtime.RunAgent(ctx, pod, AgentSpec{
		Name:           name,
		Image:          cfg.Image,
		Workdir:        spec.Workspace,
		Mounts:         mounts,
		Env:            env,
		Command:        spec.Command,
		Limits:         s.limits(),
		TimeoutSeconds: runtimeTimeout(spec.Timeout),
		Stdin:          spec.Stdin,
		Stdout:         spec.Stdout,
		Stderr:         stderr,
	})
	if ctx.Err() != nil {
		// killing podman's client can leave the agent running beside the
		// services: removed here, so it stops when its run is stopped
		s.removeRun(ctx, name)
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("sandbox: agent: %w", err)
	}
	return classify(exit, time.Since(start), spec.Timeout, s.limits(), tail.String())
}
