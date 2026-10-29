package nodev1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"time"

	"projectmesh.io/mesh-cli/internal/escalate"
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
	// escalations are the model steps the session took.
	escalations []meshapi.Escalation
	// evidence are the ids of every ToolRecord the lease's node tools
	// posted, which the finish names.
	evidence []string
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
	mb := NewMailbox()
	r.mu.Lock()
	r.live[id] = &running{cancel: cancel, progressAt: time.Now(), mailbox: mb, turnTool: b.TurnTool}
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
	out := r.work(leaseCtx, b, mb)
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
//
// The session streams (PR 2.10): the prompt is its first message, the
// lease's mailbox feeds it while it runs, every event of its output is
// the lease's progress, and the product's answer to the finish tool
// closes its input. Its model walks the kind's ladder (PR 2.11).
func (r *Runtime) work(ctx context.Context, b *meshapi.Brief, mb *Mailbox) outcome {
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

	ladder := escalate.New(b.Harness.Model)
	fin := &finishCapture{tool: b.FinishTool, onFinish: mb.Finish}
	tools, rec := r.nodeTools(b, grant, workspace, tokens, ladder, mb)
	servers, closeProxy, err := r.openProxy(b, tokens, grant, tools, fin.observe)
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
		Mailbox:   mb,
		Ladder:    ladder,
		Thread:    b.Thread != nil,
		OnEvent:   func(harness.Event) { r.touch(b.LeaseID) },
	}
	sctx := ctx
	if m := b.Harness.Budget.Minutes; m > 0 {
		var stop context.CancelFunc
		sctx, stop = context.WithTimeout(ctx, time.Duration(m)*time.Minute)
		defer stop()
	}
	res, err := r.Session.Run(sctx, spec)
	o := outcome{result: res, err: err, finish: fin.result(), escalations: taken(ladder.Steps()), evidence: rec.IDs()}
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
	if u := usageOf(o.result, o.escalations); u != nil {
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
	st, err := r.API.Finish(ctx, id, meshapi.FinishRequest{Result: result, SessionRef: o.result.SessionID, Evidence: o.evidence})
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

// usageOf is what the session spent, by model, and the escalations it
// took; nil when it reported nothing. A streaming session names its
// spend by model; a session that did not (an older CLI) has one model's.
func usageOf(res SessionResult, escalations []meshapi.Escalation) *meshapi.UsageRequest {
	if res.Usage == nil && res.CostUSD == 0 && len(res.Models) == 0 && len(escalations) == 0 {
		return nil
	}
	u := &meshapi.UsageRequest{Turns: res.Turns, DurationMS: res.Duration.Milliseconds(), Escalations: escalations}
	if len(res.Models) > 0 {
		names := make([]string, 0, len(res.Models))
		for name := range res.Models {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			m := res.Models[name]
			u.Usage = append(u.Usage, meshapi.ModelUsage{
				Model:                 name,
				InputTokens:           m.InputTokens + m.CacheReadInputTokens + m.CacheCreationInputTokens,
				CachedInputTokens:     m.CacheReadInputTokens,
				CacheWriteInputTokens: m.CacheCreationInputTokens,
				OutputTokens:          m.OutputTokens,
				CostUSD:               m.CostUSD,
			})
		}
		return u
	}
	m := meshapi.ModelUsage{CostUSD: res.CostUSD}
	if r := res.Usage; r != nil {
		m.Model = r.Model
		m.InputTokens, m.CachedInputTokens, m.CacheWriteInputTokens = r.InputTokens, r.CachedInputTokens, r.CacheWriteInputTokens
		m.OutputTokens = r.OutputTokens
	}
	u.Usage = []meshapi.ModelUsage{m}
	return u
}

// taken are the escalations the session took, leaving those it refused.
func taken(steps []meshapi.Escalation) []meshapi.Escalation {
	var out []meshapi.Escalation
	for _, e := range steps {
		if e.Failed == "" {
			out = append(out, e)
		}
	}
	return out
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
