package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/sandbox"
)

// The pod step starts the job container a session's later steps — run,
// validate — execute in, and holds no credential there (runsheet
// adr-061-mesh-is-a-job-system §4, one exchange): the item's GitHub token
// and the session's bearer stay on the node. The container's environment
// is the sandbox's allow-list less every GitHub credential
// (sandbox.HeldOutEnv), its only host paths the workspace, the session's
// own HOME and the git and package mounts beside them (Session.Mounts),
// and no token file is placed in any of them: the MCP relay's bearer is
// read on the node (item 3's endpoint, harness.placePodRelay), so the pod
// reaches the runsheet tools by a socket and never by a token. What the
// agent once asked of gh inside the pod — a pull request, an issue, a
// diff — it asks of the mesh tools; gh there is signed out and says so.

// PodStepName is the pod step's name in a Job and in its Record.
const PodStepName = "pod"

// PodCloseStepName is the always-run step that removes the pod.
const PodCloseStepName = "pod-close"

// PodConfig is what the pod step opens a job container from.
type PodConfig struct {
	// Sandbox is the node's sandbox; its Runtime starts the pod.
	Sandbox *sandbox.Sandbox
	// Label and Kind name the session (sandbox.Sandbox.Open).
	Label string
	Kind  string
	// Git is the agent's commit identity, written to the pod's
	// .gitconfig; never a credential.
	Git sandbox.GitIdentity
	// Env is the session's own environment as the node holds it, the
	// item's GitHub token and gh config dir among it (ghcred.go's
	// SessionEnv); JobEnv filters it before anything reaches the pod.
	Env []string
	// Services are the repository's declared services, started as
	// sidecars in the pod (sandbox.Session.Start); ServiceTimeout bounds
	// their health checks.
	Services       []sandbox.Service
	ServiceTimeout time.Duration
}

// Container is the job container the pod step started: its sandbox
// session, the workspace it runs in and the environment it holds, which
// carries no GitHub credential. It rides State.Pod to the later steps.
type Container struct {
	Session   *sandbox.Session
	Workspace string
	Env       []string
	Services  []sandbox.ServiceStatus
}

// JobEnv is env with every GitHub credential taken out
// (sandbox.HeldOutEnv): the job container's environment before the
// sandbox's own allow-list (sandbox.Session.Env) reads it. The model
// account's key and the MESH_TOOL_* secrets are kept.
func JobEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if heldOut(name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func heldOut(name string) bool {
	for _, h := range sandbox.HeldOutEnv {
		if name == h {
			return true
		}
	}
	return false
}

// ErrNoSandbox is a pod step given no sandbox to start the pod on.
var ErrNoSandbox = errors.New("pod: no sandbox")

// PodStep is the step that starts the job container: it opens the
// session's sandbox (its HOME), starts the pod with the services, and
// leaves a *Container in state.Pod. The workspace is state.WorkspaceDir,
// which the checkout step prepared. A required service refused or a pod
// that cannot be made fails the step, and the session is closed again.
func PodStep(cfg PodConfig) Step {
	return Step{Name: PodStepName, Run: func(ctx context.Context, state *State) error {
		if cfg.Sandbox == nil || cfg.Sandbox.Runtime == nil {
			return ErrNoSandbox
		}
		if state.WorkspaceDir == "" {
			return errors.New("pod: no workspace")
		}
		sess, err := cfg.Sandbox.Open(cfg.Label, cfg.Kind, cfg.Git)
		if err != nil {
			return fmt.Errorf("pod: %w", err)
		}
		sts, err := sess.Start(ctx, cfg.Services, cfg.ServiceTimeout)
		if err == nil && len(sess.Pods()) == 0 {
			err = errors.New("a required service was refused: " + sandbox.ServicesLine(sts))
		}
		if err == nil {
			err = requiredDown(sts)
		}
		if err != nil {
			sess.CloseAndLog()
			return fmt.Errorf("pod: %w", err)
		}
		state.Pod = &Container{Session: sess, Workspace: state.WorkspaceDir, Env: JobEnv(cfg.Env), Services: sts}
		return nil
	}}
}

// requiredDown is the first required service that did not come up.
func requiredDown(sts []sandbox.ServiceStatus) error {
	for _, st := range sts {
		if st.Required && st.Why != "" {
			return fmt.Errorf("required service %s: %s", st.Describe(), st.Why)
		}
	}
	return nil
}

// PodCloseStep is the always-run step that removes the job container's
// pod and HOME, whatever the steps before it did.
func PodCloseStep() Step {
	return Step{Name: PodCloseStepName, Always: true, Run: func(ctx context.Context, state *State) error {
		c, ok := state.Pod.(*Container)
		if !ok || c == nil || c.Session == nil {
			return nil
		}
		state.Pod = nil
		return c.Session.Close()
	}}
}

// Exec runs command in the job container, beside where the agent runs,
// on the container's own environment (no GitHub credential) — a
// validation's step, a preflight. The pod is kept for the next Exec.
func (c *Container) Exec(ctx context.Context, command []string, timeout time.Duration, stdout, stderr io.Writer) error {
	return c.Session.Exec(ctx, sandbox.RunSpec{
		Workspace: c.Workspace,
		Env:       c.Env,
		Command:   command,
		Timeout:   timeout,
		Stdout:    stdout,
		Stderr:    stderr,
	})
}
