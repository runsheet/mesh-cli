package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// A plan or research session runs in its org's pod (ADR-034, Refusals):
// the node opens the pod — its HOME under the org's tree, every mount
// checked against the org's root, the pod itself created — before the
// harness is called, and hands it to the job (Job.Session). A pod that
// cannot start is a *PodStartError and the session never runs: never on
// the host in its place.

// PodStartError is a plan's or research's pod that could not start: its
// HOME, a mount outside the org's root, or the pod itself. No harness
// was called. A *sandbox.RuntimeError beneath it is still found by
// RuntimeErrorOf.
type PodStartError struct {
	Kind  harness.Kind
	Label string
	Err   error
}

func (e *PodStartError) Error() string {
	return fmt.Sprintf("sandbox: %s pod for %s did not start: %v", e.Kind, e.Label, e.Err)
}

func (e *PodStartError) Unwrap() error { return e.Err }

// PodStartErrorOf is the *PodStartError in err's chain.
func PodStartErrorOf(err error) (*PodStartError, bool) {
	var pe *PodStartError
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}

// sessionSandbox is the sandbox a plan or research of scope opens its pod
// on: sb itself when no roots are laid out or the node serves one org,
// else sb on scope's org's own package store. An org the node does not
// serve is refused. A nil sb is nil.
func sessionSandbox(sb *sandbox.Sandbox, roots *OrgRootSet, scope Scope) (*sandbox.Sandbox, error) {
	if sb == nil || roots == nil || roots.Single() {
		return sb, nil
	}
	r, err := roots.For(scope.Name())
	if err != nil {
		return nil, err
	}
	return sb.WithStores(PackageStores(r.Store)), nil
}

// openScopePod opens the pod a plan or research session of scope runs
// in, working in dir: confined to the org whose work root holds dir, its
// mounts checked and the pod created, all before any harness call. A
// nil sb is a nil pod — the host, as today — unless the node serves two
// or more orgs (served, or roots laying out more than one), which is a
// *HostFallbackError: a host session could read the other orgs' files
// (ADR 066, decision 10). Every other failure is a *PodStartError; a pod
// that was opened is returned with it for the caller to Close.
func openScopePod(ctx context.Context, sb *sandbox.Sandbox, roots *OrgRootSet, served Scopes, scope Scope, dir, label string, kind harness.Kind, logf func(string, ...any)) (*SessionPod, error) {
	if sb == nil {
		if roots != nil && !roots.Single() && len(roots.Orgs()) > 1 {
			return nil, &HostFallbackError{Org: scope.Name(), Orgs: roots.Orgs(), Path: dir}
		}
		return nil, HostFallback(nil, served, scope.ID, dir)
	}
	osb, err := sessionSandbox(sb, roots, scope)
	if err != nil {
		return nil, &PodStartError{Kind: kind, Label: label, Err: fmt.Errorf("package store: %w", err)}
	}
	osb = confineTo(osb, dir)
	sess, err := osb.Open(label, string(kind), sandbox.GitIdentity{})
	if err != nil {
		return nil, &PodStartError{Kind: kind, Label: label, Err: err}
	}
	p := &SessionPod{Session: sess, logf: logf}
	if _, err := sess.CheckedMounts(dir); err != nil {
		return p, &PodStartError{Kind: kind, Label: label, Err: err}
	}
	if _, err := sess.Start(ctx, nil, 0); err != nil {
		return p, &PodStartError{Kind: kind, Label: label, Err: err}
	}
	if logf != nil {
		logf("sandbox: %s", sess.Describe())
	}
	return p, nil
}

// sessionGHConfigDir is a plan's or research's empty GH_CONFIG_DIR: on a
// sandboxed node under the org's scratch root (the pod gives the
// session its own under its HOME, and nothing of it leaves the org's
// tree), else in the system temp dir as before (NewGHConfigDir).
func sessionGHConfigDir(sb *sandbox.Sandbox, scratchRoot string) (string, error) {
	if sb == nil {
		return NewGHConfigDir()
	}
	if err := os.MkdirAll(scratchRoot, 0o700); err != nil {
		return "", err
	}
	return os.MkdirTemp(scratchRoot, "mesh-gh-")
}

// releasePodFailure answers a plan whose pod could not start, or that a
// multi-org node with no sandbox will not run on the host: the plan
// claim released with reason (releasePlan), no try counted, and the
// goal held off this node for WorkspaceReleaseHold.
func (p *Planner) releasePodFailure(ctx context.Context, goal gateway.Goal, scope Scope, reason string) {
	reason = truncate(reason, 2000)
	switch rerr := p.GW.ReleasePlan(ctx, goal.ID, scope.NodeID, reason); {
	case rerr == nil:
		p.logf("goal %s: %s; plan claim released, no try counted", goal.Slug, oneLine(reason))
	case gateway.IsUnsupported(rerr):
		p.logf("goal %s: %s; the claim lapses", goal.Slug, oneLine(reason))
	default:
		p.logf("goal %s: %s; plan release refused: %v", goal.Slug, oneLine(reason), rerr)
	}
	p.Declines.HoldFor(goal.ID, reason, time.Now(), WorkspaceReleaseHold)
}

// servedOf is the orgs served answers, none when it is nil.
func servedOf(served func() Scopes) Scopes {
	if served == nil {
		return nil
	}
	return served()
}
