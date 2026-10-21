package node

import (
	"context"
	"fmt"
	"strings"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/tools"
)

// SessionPod is a sandboxed session opened by the node before its agent
// (ADR 007 Services): the checkout's services resolved, started as
// sidecars in the session's pod and health-checked. The node decides
// from it whether the agent starts, tells the agent what it has, and
// Close removes the pod and its sidecars on every path.
type SessionPod struct {
	Session  *sandbox.Session
	Statuses []sandbox.ServiceStatus
	// logf is the node's log, where a Close that fails says so.
	logf func(string, ...any)
}

// openOrgSessionPod is OpenSessionPod for a session of the org whose id
// is orgID: its pod mounts and names its own job's package store.
// With no sandbox on a node serving two or more orgs it opens nothing
// and is a *HostFallbackError, whatever the session's kind (ADR 066,
// decision 10: more than one tenant means every session is sandboxed) —
// a build, an evaluation, a rebase and a main validation alike.
func openOrgSessionPod(ctx context.Context, sb *sandbox.Sandbox, l *Layout, scopes Scopes, orgID, dir, label string, kind harness.Kind, git sandbox.GitIdentity, logf func(string, ...any)) (*SessionPod, error) {
	if err := HostFallback(sb, scopes, orgID, dir); err != nil {
		return nil, err
	}
	// the job's own store, its directory alone (OpenSessionPod's confineTo)
	return OpenSessionPod(ctx, sb, dir, label, kind, git, logf)
}

// OpenSessionPod resolves the services the checkout at dir declares
// (sandbox.Resolve on the node's registries) and starts them in the
// session's pod. A nil sandbox (`sandbox: off`) is a nil pod: no
// services, as before. A services.yml that cannot be read is a required
// service that failed. The error is the sandbox itself failing (the
// HOME, the pod) or ctx ending; the pod, when there is one, is still
// closed by the caller. git is the session's commit identity (ADR-024),
// empty for an evaluation's read-only checkout.
func OpenSessionPod(ctx context.Context, sb *sandbox.Sandbox, dir, label string, kind harness.Kind, git sandbox.GitIdentity, logf func(string, ...any)) (*SessionPod, error) {
	if sb == nil {
		return nil, nil
	}
	// a checkout in a job gets a pod confined to the job and its
	// connection's caches: every mount outside them refused (ADR-039)
	sb = confineTo(sb, dir)
	sess, err := sb.Open(label, string(kind), git)
	if err != nil {
		return nil, err
	}
	logf("sandbox: %s", sess.Describe())
	p := &SessionPod{Session: sess, logf: logf}
	svcs, err := sandbox.Resolve(dir, sb.Config.Registries)
	if err != nil {
		p.Statuses = []sandbox.ServiceStatus{{Name: sandbox.ServicesFile, Image: "-", Required: true, Why: err.Error()}}
		return p, nil
	}
	for _, n := range svcs.Notes {
		logf("item %s: services: %s", label, n)
	}
	if len(svcs.Services) == 0 {
		return p, nil
	}
	var names []string
	for _, s := range svcs.Services {
		names = append(names, s.Name)
		for _, n := range s.Notes {
			logf("item %s: service %s: %s", label, s.Name, n)
		}
	}
	logf("item %s: services from %s: %s", label, svcs.Source, strings.Join(names, ", "))
	p.Statuses, err = sess.Start(ctx, svcs.Services, sb.Config.ServiceTimeout)
	if err != nil {
		return p, err
	}
	if line := sandbox.ServicesListing(p.Statuses); len(line) > 0 {
		if _, err := sess.WriteFile(sandbox.ServicesFileName, line); err != nil {
			return p, fmt.Errorf("sandbox: %s: %w", sandbox.ServicesFileName, err)
		}
	}
	for _, st := range p.Statuses {
		if st.Ready {
			logf("item %s: service %s ready at %s", label, st.Describe(), st.Addrs())
		}
	}
	return p, nil
}

// Close removes the pod, its sidecars, their volumes and the session's
// HOME; what it could not remove is logged, `sandbox: session <label>:
// close: <err>`, never dropped.
func (p *SessionPod) Close() {
	if p == nil || p.Session == nil {
		return
	}
	if err := p.Session.Close(); err != nil && p.logf != nil {
		p.logf("%s", sandbox.CloseLine(p.Session.Label, err))
	}
}

// Apply puts the session on the job, for the harness to run in.
func (p *SessionPod) Apply(job *harness.Job) {
	if p != nil {
		job.Session = p.Session
	}
}

// Prompt is the prompt's Services line ("" when there are none).
func (p *SessionPod) Prompt() string {
	if p == nil {
		return ""
	}
	return sandbox.ServicesLine(p.Statuses)
}

// RequiredFailure is the first required service that is not ready: the
// agent must not start.
func (p *SessionPod) RequiredFailure() (sandbox.ServiceStatus, bool) {
	if p == nil {
		return sandbox.ServiceStatus{}, false
	}
	for _, st := range p.Statuses {
		if st.Required && !st.Ready {
			return st, true
		}
	}
	return sandbox.ServiceStatus{}, false
}

// ReleaseReason is what a claim a required service failed is handed
// back with, naming the service and why.
func (p *SessionPod) ReleaseReason() string {
	st, _ := p.RequiredFailure()
	return fmt.Sprintf("services: required service %s did not come up: %s", st.Describe(), st.Why)
}

// Declined is the decline a required service's failure records, so the
// node does not take the item again while the same cause holds.
func (p *SessionPod) Declined() []tools.Omission {
	st, _ := p.RequiredFailure()
	return []tools.Omission{{Tool: "service:" + st.Name, Runtime: st.Why}}
}

// MissingNote is the item's progress note of the services that are not
// ready but not required, each named with why ("" when none): the
// session goes on without them.
func (p *SessionPod) MissingNote() string {
	if p == nil {
		return ""
	}
	var missing []string
	for _, st := range p.Statuses {
		if !st.Ready && !st.Required {
			missing = append(missing, fmt.Sprintf("%s: %s", st.Describe(), st.Why))
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return "services missing (not required; the session goes on without them): " + strings.Join(missing, "; ")
}
