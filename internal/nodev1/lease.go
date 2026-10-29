package nodev1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"projectmesh.io/mesh-cli/internal/gitrelay"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/mcptool"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sessionproxy"
)

// reportTimeout bounds each report to Mesh after the session ends: the
// node's own ctx may be over (a restart's hand-back) and the report must
// still go.
const reportTimeout = 30 * time.Second

// maxReason bounds a hand-back's or failure's reason.
const maxReason = 2000

// outcome is how a lease's work ended, before it is reported.
type outcome struct {
	result SessionResult
	// finish is what the product's finish tool answered, nil when the
	// session never had it answer.
	finish     json.RawMessage
	err        error
	cause      error // the lease ctx's cause when it was cancelled
	nodeEnding bool
	// access are the relay's AccessRecords of the lease.
	access []meshapi.AccessRecord
}

// runLease runs one lease to its report: the lease's directory and
// sessionproxy, the renewals, the session, then finish, hand-back or
// nothing (the lease is no longer this node's). A panic hands the lease
// back with cause panic.
func (r *Runtime) runLease(ctx context.Context, tk *meshapi.Take) {
	b, lease := tk.Brief, tk.Lease
	id := lease.LeaseID
	r.logf("nodev1: took lease %s of job %s (%s, %s/%s)", id, b.JobID, b.Kind, b.Subject.Type, b.Subject.ID)
	leaseCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	r.mu.Lock()
	r.live[id] = &running{cancel: cancel, progressAt: time.Now()}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.live, id)
		r.mu.Unlock()
	}()
	defer func() {
		if p := recover(); p != nil {
			r.logf("nodev1: lease %s panicked: %v\n%s", id, p, debug.Stack())
			r.handBack(id, meshapi.CausePanic, fmt.Sprintf("the node panicked: %v", p), "")
		}
	}()

	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		r.renew(leaseCtx, lease, cancel)
	}()
	out := r.work(leaseCtx, b)
	cancel(nil)
	<-renewDone
	out.nodeEnding = ctx.Err() != nil
	out.access = r.takeAccess(id)
	r.report(id, b, out)
}

// work prepares the lease and runs its session: the lease's Latchkey
// session (its DPoP key, request credential and session tokens, held
// here alone), its git grant on the node's relay and the checkout
// through it, its sessionproxy, then the session. Leaving, it closes the
// proxy, then the grant — which revokes the git tokens it minted — and
// removes the lease's directory.
func (r *Runtime) work(ctx context.Context, b *meshapi.Brief) outcome {
	dir := filepath.Join(r.WorkRoot, "leases", b.LeaseID)
	workspace := filepath.Join(dir, "work")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return outcome{err: fmt.Errorf("the lease's directory: %w", err)}
	}
	defer os.RemoveAll(dir)

	var tokens *sessionTokens
	if len(b.Servers) > 0 || b.Git != nil {
		var err error
		if tokens, err = openSession(ctx, r.API, b.LeaseID, r.LatchkeyHTTP); err != nil {
			return outcome{err: err, cause: context.Cause(ctx)}
		}
	}
	var grant *gitrelay.Grant
	if b.Git != nil && b.Git.Repo != "" {
		policy := policyOf(b.Git)
		mint := func(ctx context.Context) (gitrelay.Token, error) {
			return tokens.MintSource(ctx, b.Git.Repo, string(policy.Access))
		}
		g, err := r.relay.Open(b.LeaseID, policy, mint, tokens.RevokeSource)
		if err != nil {
			return outcome{err: fmt.Errorf("the lease's git relay: %w", err)}
		}
		grant = g
		defer func() {
			if err := grant.Close(); err != nil {
				r.logf("nodev1: lease %s: %v", b.LeaseID, err)
			}
		}()
		if err := checkout(ctx, grant, b, workspace); err != nil {
			return outcome{err: fmt.Errorf("checking out %s: %w", b.Git.Repo, err), cause: context.Cause(ctx)}
		}
		// the relay proves a fast-forward-only push in this checkout
		grant.SetCheckout(workspace)
	}

	fin := &finishCapture{tool: b.FinishTool}
	servers, closeProxy, err := r.openProxy(b, tokens, grant, r.nodeTools(b, grant, workspace), fin.observe)
	if err != nil {
		return outcome{err: fmt.Errorf("the lease's sessionproxy: %w", err)}
	}
	defer closeProxy()

	spec := SessionSpec{
		Label:     b.LeaseID,
		Workspace: workspace,
		Prompt:    Prompt(b),
		Model:     b.Harness.Model.Default,
		BudgetUSD: b.Harness.Budget.USD,
		Servers:   servers,
		Env:       []string{"GIT_TERMINAL_PROMPT=0"},
		Unset:     gitEnvUnset,
	}
	sctx := ctx
	if m := b.Harness.Budget.Minutes; m > 0 {
		var stop context.CancelFunc
		sctx, stop = context.WithTimeout(ctx, time.Duration(m)*time.Minute)
		defer stop()
	}
	res, err := r.Session.Run(sctx, spec)
	o := outcome{result: res, err: err, finish: fin.result()}
	if ctx.Err() != nil {
		o.cause = context.Cause(ctx)
	} else if errors.Is(err, context.DeadlineExceeded) || sctx.Err() != nil {
		o.err = fmt.Errorf("the session ran past its %d-minute budget", b.Harness.Budget.Minutes)
	}
	return o
}

// openProxy serves the brief's servers on a sessionproxy of the lease's
// own, a unix socket in a short directory of the lease's own
// (leaseSocketDir), and answers the session's
// MCP config: each server a `mesh mcp-relay --socket` entry naming it.
// Each product server is reached on the lease's mesh-session token with
// a DPoP proof signed for the server's own URL; the node's own tools,
// when the kind names any, are served in process at NodeToolsServer; the
// grant, when there is one, serves the proxy's /git/ route.
func (r *Runtime) openProxy(b *meshapi.Brief, tokens *sessionTokens, grant *gitrelay.Grant, tools []mcptool.Tool, observe func(string, []byte, []byte)) (map[string]any, func(), error) {
	ups := map[string]sessionproxy.Upstream{}
	for _, s := range b.Servers {
		if s.Name == "" || s.URL == "" {
			return nil, nil, fmt.Errorf("the brief names a server without a name or url: %+v", s)
		}
		if s.Name == NodeToolsServer {
			return nil, nil, fmt.Errorf("the brief names a server %q, the node's own tools' name", s.Name)
		}
		ups[s.Name] = sessionproxy.Product{URL: s.URL, Bearer: tokens.Token, Key: tokens.Key()}
	}
	if len(tools) > 0 {
		ups[NodeToolsServer] = sessionproxy.InProcess{Handler: &mcptool.Handler{Name: NodeToolsServer, Tools: tools}}
	}
	sockDir, err := r.leaseSocketDir(b.LeaseID)
	if err != nil {
		return nil, nil, err
	}
	sock := filepath.Join(sockDir, leaseSocketName)
	p := sessionproxy.New(ups)
	p.Logf = r.Logf
	p.Observe = observe
	if grant != nil {
		p.Git = grant
	}
	if err := p.Listen(sock); err != nil {
		os.RemoveAll(sockDir)
		return nil, nil, err
	}
	servers := map[string]any{}
	for name := range ups {
		servers[name] = harness.ProxyRelay(r.MeshBinary, sock, name)
	}
	return servers, func() { p.Close(); os.RemoveAll(sockDir) }, nil
}

// nodeTools are the node tools the brief's kind names that this node
// has: push, through the lease's grant. A tool the node does not have
// yet (run_suite, apply_onto, …: wave 3) is left out and logged.
func (r *Runtime) nodeTools(b *meshapi.Brief, grant *gitrelay.Grant, workspace string) []mcptool.Tool {
	var out []mcptool.Tool
	for _, name := range b.NodeTools {
		switch name {
		case PushTool:
			out = append(out, r.pushTool(b, grant, workspace))
		default:
			r.logf("nodev1: lease %s: the kind names node tool %q, which this node does not have", b.LeaseID, name)
		}
	}
	return out
}

// renew extends the lease every renew_every_s until ctx ends. Mesh
// saying the lease is gone cancels the lease's work; a renewal that
// cannot reach Mesh is tried again until the lease's own expiry passes,
// which cancels it too.
func (r *Runtime) renew(ctx context.Context, lease *meshapi.Lease, cancel context.CancelCauseFunc) {
	every := time.Duration(lease.RenewEveryS) * time.Second
	if every <= 0 {
		every = 30 * time.Second
	}
	expires := lease.ExpiresAt
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		resp, err := r.API.Renew(ctx, lease.LeaseID)
		switch {
		case err == nil:
			expires = resp.ExpiresAt
			if resp.RenewEveryS > 0 && time.Duration(resp.RenewEveryS)*time.Second != every {
				every = time.Duration(resp.RenewEveryS) * time.Second
				t.Reset(every)
			}
			r.mu.Lock()
			if l := r.live[lease.LeaseID]; l != nil {
				l.progressAt = time.Now()
			}
			r.mu.Unlock()
		case meshapi.LeaseGone(err):
			r.logf("nodev1: lease %s: %v", lease.LeaseID, err)
			cancel(errLeaseGone)
			return
		case ctx.Err() != nil:
			return
		default:
			r.logf("nodev1: renewing lease %s: %v", lease.LeaseID, err)
			if !expires.IsZero() && time.Now().After(expires) {
				cancel(errLeaseGone)
				return
			}
		}
	}
}

// report tells Mesh how the lease ended: usage then finish on success; a
// hand-back with its cause when the session could not run or the node
// is ending; nothing when the lease is no longer the node's.
func (r *Runtime) report(id string, b *meshapi.Brief, o outcome) {
	switch {
	case errors.Is(o.cause, errLeaseGone):
		r.logf("nodev1: lease %s ended without a report: %v", id, o.cause)
		return
	case errors.Is(o.cause, errCancelled):
		r.postAccess(id, o.access)
		r.handBack(id, meshapi.CauseCancelled, r.cancelReason(id), o.result.SessionID)
		return
	case o.nodeEnding:
		r.postAccess(id, o.access)
		r.handBack(id, meshapi.CauseNodeRestart, "the node stopped while the session ran", o.result.SessionID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
	defer cancel()
	r.postAccess(id, o.access)
	if u := usageOf(o.result); u != nil {
		if err := r.API.Usage(ctx, id, *u); err != nil {
			r.logf("nodev1: lease %s: usage: %v", id, err)
		}
	}
	if o.err != nil {
		r.handBack(id, meshapi.CauseCannotRun, o.err.Error(), o.result.SessionID)
		return
	}
	result := o.finish
	if result == nil {
		result, _ = json.Marshal(map[string]string{"text": clip(o.result.Text, 16<<10)})
	}
	st, err := r.API.Finish(ctx, id, meshapi.FinishRequest{Result: result, SessionRef: o.result.SessionID})
	if err != nil {
		r.logf("nodev1: lease %s: finish: %v", id, err)
		return
	}
	r.logf("nodev1: lease %s finished; job %s is %s", id, st.JobID, st.Status)
}

// postAccess posts the relay's AccessRecords of the lease to Mesh.
func (r *Runtime) postAccess(id string, records []meshapi.AccessRecord) {
	if len(records) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
	defer cancel()
	if err := r.API.Access(ctx, id, records); err != nil {
		r.logf("nodev1: lease %s: access records: %v", id, err)
	}
}

func (r *Runtime) handBack(id, cause, reason, sessionRef string) {
	ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
	defer cancel()
	st, err := r.API.HandBack(ctx, id, meshapi.HandBackRequest{Cause: cause, Reason: clip(reason, maxReason), SessionRef: sessionRef})
	if err != nil && meshapi.LeaseGone(err) {
		r.logf("nodev1: lease %s was no longer the node's to hand back (%s)", id, cause)
		return
	}
	if err != nil {
		r.logf("nodev1: lease %s: hand-back (%s): %v", id, cause, err)
		return
	}
	r.logf("nodev1: lease %s handed back (%s); job %s is %s", id, cause, st.JobID, st.Status)
}

// usageOf is what the session spent, by model; nil when it reported nothing.
func usageOf(res SessionResult) *meshapi.UsageRequest {
	if res.Usage == nil && res.CostUSD == 0 {
		return nil
	}
	m := meshapi.ModelUsage{CostUSD: res.CostUSD}
	if u := res.Usage; u != nil {
		m.Model = u.Model
		m.InputTokens, m.CachedInputTokens, m.CacheWriteInputTokens = u.InputTokens, u.CachedInputTokens, u.CacheWriteInputTokens
		m.OutputTokens = u.OutputTokens
	}
	return &meshapi.UsageRequest{Usage: []meshapi.ModelUsage{m}, Turns: res.Turns, DurationMS: res.Duration.Milliseconds()}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
