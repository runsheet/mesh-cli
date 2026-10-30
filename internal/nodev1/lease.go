package nodev1

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/actor"
	"projectmesh.io/mesh-cli/internal/escalate"
	"projectmesh.io/mesh-cli/internal/gitrelay"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/mcptool"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/scheduler"
	"projectmesh.io/mesh-cli/internal/sessionproxy"
)

// What a job actor holds of work's, by name.
const (
	holdGrant = "the relay grant"
	holdProxy = "the sessionproxy"
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
	// finish is what the product's finish tool answered, or the node's
	// result tool kept, nil when the session never had either answer. A
	// one-shot session's settle fills it from the session's last message
	// when that fits the kind's finish_schema (settle.go).
	finish json.RawMessage
	// handBack is the hand-back the session asked for with the node's
	// hand_back tool, nil when it asked none.
	handBack *meshapi.HandBackRequest
	// noFinish is why a one-shot session ended with no finish: the lease
	// fails no_finish, never finishes with prose.
	noFinish   string
	err        error
	cause      error // the lease ctx's cause when it was cancelled
	nodeEnding bool
	// parked is a thread's session that ended after its park_after of
	// quiet: the lease is handed back parked with its session_ref.
	parked bool
	// access are the relay's AccessRecords of the lease.
	access []meshapi.AccessRecord
	// escalations are the model steps the session took.
	escalations []meshapi.Escalation
	// evidence are the ids of every ToolRecord the lease's node tools
	// posted, which the finish names.
	evidence []string
	// account is what the session ran on: the job's model account or
	// the node's own login.
	account *sessionAccount
	// harness and model are the session's harness and the model it
	// ended on, for the usage report.
	harness, model string
	// counts are the session's content-free counts and stopped the stop
	// rule that ended it, nil when none did (stoprules.go).
	counts  meshapi.Counts
	stopped *ruleStop
}

// runLease runs one lease to its report: the lease's directory and
// sessionproxy, the renewals, the session, then finish, hand-back or
// nothing (the lease is no longer this node's). A panic hands the lease
// back with cause panic. room is the lease's hold on the scheduler (nil
// without one); roomErr, why the scheduler had no room, hands the lease
// back cannot_run.
func (r *Runtime) runLease(ctx context.Context, tk *meshapi.Take, room *scheduler.Lease, roomErr error) {
	if r.Actors {
		r.runActor(ctx, tk, room, roomErr)
		return
	}
	b, lease := tk.Brief, tk.Lease
	id := lease.LeaseID
	r.logf("nodev1: took lease %s of job %s (%s, %s/%s)", id, b.JobID, b.Kind, b.Subject.Type, b.Subject.ID)
	leaseCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	mb := NewMailbox()
	mb.turns = newTurnLedger(b)
	r.mu.Lock()
	r.leases.Put(id, &running{cancel: cancel, progressAt: time.Now(), mailbox: mb, turnTool: turnToolFor(b), jobID: b.JobID})
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.leases.Delete(id)
		r.mu.Unlock()
	}()
	defer func() {
		if p := recover(); p != nil {
			r.logf("nodev1: lease %s panicked: %v\n%s", id, p, debug.Stack())
			r.handBack(id, meshapi.CausePanic, fmt.Sprintf("the node panicked: %v", p), "")
		}
	}()

	defer room.Release()
	if roomErr != nil {
		r.handBack(id, meshapi.CauseCannotRun, "the node had no room for the lease: "+roomErr.Error(), "")
		return
	}

	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		r.renew(leaseCtx, lease, cancel, room)
	}()
	out := r.work(leaseCtx, b, mb, nil, "")
	cancel(nil)
	<-renewDone
	out.nodeEnding = ctx.Err() != nil
	out.access = r.takeAccess(id)
	end := r.report(id, b, out)
	r.settleThread(b, out.result.SessionID, end.Cause == meshapi.CauseParked)
	// the account's standing as the session last read it reaches Mesh now,
	// before the node's next take, not a heartbeat period later
	if len(r.unsentAccounts()) > 0 && ctx.Err() == nil {
		r.heartbeat(ctx)
	}
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
//
// Under a job actor (a non-nil; ADR-044, PR 3.2) the actor holds what
// work opens — the grant and the proxy — and releases them, with the
// lease's directory, in its releasing state after the report; work
// enters running as the session starts and runs it in the lease's pod
// when the actor opened one. Without, work closes them as it leaves.
func (r *Runtime) work(ctx context.Context, b *meshapi.Brief, mb *Mailbox, a *actor.Actor, pod string) outcome {
	var closers []func()
	keep := func(name string, release func() error) {
		if a != nil {
			a.Hold(name, release)
			return
		}
		closers = append(closers, func() { release() })
	}
	defer func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}()
	// a node tool the kind names that this node does not have is handed
	// back tool_unavailable before anything is minted (causes.go)
	if name := unservedTool(b); name != "" {
		return outcome{err: &toolUnavailableError{tool: name, why: "this node does not have the node tool"}}
	}
	// the kind's tool slots: a required one the node cannot fill is
	// handed back tool_unavailable here too (slots.go)
	sl, slotErr := slotsOf(b)
	if slotErr != nil {
		return outcome{err: slotErr}
	}
	dir := filepath.Join(r.WorkRoot, "leases", b.LeaseID)
	workspace := filepath.Join(dir, "work")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return outcome{err: fmt.Errorf("the lease's directory: %w", err)}
	}
	// a thread's workspace is its job's, kept across a park so its
	// session resumes where it ran (thread.go)
	var resume string
	if b.Thread != nil {
		var err error
		if workspace, resume, err = r.threadWorkspace(b); err != nil {
			return outcome{err: err}
		}
	}
	if a == nil {
		keep("the lease's directory", func() error { return os.RemoveAll(dir) })
	}

	var tokens *sessionTokens
	if len(b.Servers) > 0 || b.Git != nil || len(apiRules(b)) > 0 || b.CredentialRef != "" {
		var err error
		if tokens, err = openSession(ctx, r.API, b.LeaseID, r.LatchkeyHTTP); err != nil {
			return outcome{err: credentialFailed(err), cause: context.Cause(ctx)}
		}
	}
	// the job's model account, when Mesh picked one: read now, so a lease
	// whose account Latchkey refuses is handed back before any checkout
	account, err := openAccount(ctx, b.CredentialRef, tokens, os.Environ())
	if err != nil {
		return outcome{err: credentialFailed(err), cause: context.Cause(ctx)}
	}
	var grant *gitrelay.Grant
	var resumed *resumePoint
	if b.Git != nil && b.Git.Repo != "" {
		policy := policyOf(b.Git)
		// a checkout that failed because its git token was not minted is
		// credential_timeout, not the checkout's own failure
		var mintMu sync.Mutex
		var mintErr error
		mint := func(ctx context.Context) (gitrelay.Token, error) {
			tok, err := tokens.MintSource(ctx, sourceRequest(b.Git, policy))
			mintMu.Lock()
			mintErr = err
			mintMu.Unlock()
			return tok, err
		}
		g, err := r.relay.Open(b.LeaseID, policy, mint, tokens.RevokeSource)
		if err != nil {
			return outcome{err: fmt.Errorf("the lease's git relay: %w", err)}
		}
		grant = g
		keep(holdGrant, func() error {
			if err := grant.Close(); err != nil {
				r.logf("nodev1: lease %s: %v", b.LeaseID, err)
			}
			return nil
		})
		if resume != "" {
			if err := reattach(ctx, grant, b, workspace); err != nil {
				r.logf("nodev1: lease %s: the parked thread's checkout is not reused (%v); starting afresh", b.LeaseID, err)
				resume = ""
				_ = os.RemoveAll(workspace)
				_ = os.MkdirAll(workspace, 0o700)
			}
		}
		if resume == "" {
			if resumed, err = checkout(ctx, grant, b, workspace, r.mirrors, r.logf); err != nil {
				err = fmt.Errorf("checking out %s: %w", b.Git.Repo, err)
				mintMu.Lock()
				if mintErr != nil {
					err = credentialFailed(fmt.Errorf("%w (its git token was not minted: %v)", err, mintErr))
				}
				mintMu.Unlock()
				return outcome{err: err, cause: context.Cause(ctx)}
			}
		}
		// the relay proves a fast-forward-only push in this checkout
		grant.SetCheckout(workspace)
	}
	// the lease's pod, with --sandbox: its services start once the
	// checkout declares them, before the session (podsession.go)
	var lp *leasePod
	if r.Pods != nil {
		var podCancel context.CancelCauseFunc
		ctx, podCancel = context.WithCancelCause(ctx)
		defer podCancel(nil)
		lp, err = r.openLeasePod(ctx, b, workspace, a, keep)
		if lp != nil {
			r.setPod(b.LeaseID, lp.name)
			go r.watchPod(ctx, lp.name, podCancel)
		}
		if err != nil {
			return outcome{err: fmt.Errorf("the lease's pod: %w", err), cause: context.Cause(ctx)}
		}
	}

	ladder := escalate.New(b.Harness.Model)
	fin := &finishCapture{tool: b.FinishTool, learningsTool: b.LearningsTool, onFinish: mb.Finish}
	// run_suite's steps run in the lease's pod, never on a sandboxed
	// node's host (podsuites.go)
	suites := r.suiteRunner(b, pod)
	if lp != nil {
		// the session's own pod, beside its services (podsession.go)
		suites = lp.suites
	}
	// run_suite with nowhere to run its steps is handed back
	// tool_unavailable before the session; a step killed under it, or its
	// runner failing, ends the lease as the infrastructure's (causes.go)
	if err := r.unservedSuites(b, suites); err != nil {
		return outcome{err: err}
	}
	ctx, endLease := context.WithCancelCause(ctx)
	defer endLease(nil)
	suites = watchSuites(suites, endLease)
	tools, rec := r.nodeTools(b, grant, workspace, tokens, ladder, mb, suites)
	hb := &sessionHandBack{}
	tools = sl.nodeTools(append(tools, r.endTools(b, fin, hb, mb)...))
	servers, sock, closeProxy, err := r.openProxy(ctx, b, tokens, grant, tools, fin.observe, account, sl)
	if err != nil {
		return outcome{err: fmt.Errorf("the lease's sessionproxy: %w", err), cause: context.Cause(ctx)}
	}
	keep(holdProxy, func() error { closeProxy(); return nil })
	gate := r.openStopGate(b, sock, fin, hb)
	if gate != nil {
		keep("the stop gate", func() error { gate.close(); return nil })
	}
	env := []string{"GIT_TERMINAL_PROMPT=0"}
	if len(apiRules(b)) > 0 {
		env = append(env, RelaySocketEnv+"="+sock)
	}
	prompt := promptWithRelay(b, sock, resumed)
	settings := gate.sessionSettings(r.MeshBinary)
	var inPod PodRun
	if lp != nil {
		// the pod has no mesh binary: its Stop hook is the node image's
		// node on a shim in the HOME, on the gate's socket, which is in
		// the mounted socket directory at its own path
		if settings, err = lp.stopSettings(gate); err != nil {
			return outcome{err: err, cause: context.Cause(ctx)}
		}
		if servers, err = lp.podServers(servers, filepath.Dir(sock)); err != nil {
			return outcome{err: err, cause: context.Cause(ctx)}
		}
		gitBase := ""
		if grant != nil {
			gitBase = grant.Base()
		}
		if inPod, err = lp.run(workspace, gitBase, sock, account.relays()); err != nil {
			return outcome{err: err, cause: context.Cause(ctx)}
		}
		if line := sandbox.ServicesLine(lp.statuses); line != "" {
			prompt += "\n" + line + "\n"
		}
		pod = lp.name
	}

	spec := SessionSpec{
		Label:     b.LeaseID,
		Workspace: workspace,
		Prompt:    threadPrompt(b, prompt, resume),
		Resume:    resume,
		Model:     b.Harness.Model.Default,
		BudgetUSD: b.Harness.Budget.USD,
		Servers:   servers,
		Env:       env,
		Unset:     gitEnvUnset,
		Mailbox:   mb,
		Ladder:    ladder,
		Thread:    b.Thread != nil,
		Settings:  settings,
		OnEvent: func(ev harness.Event) {
			r.touch(b.LeaseID)
			r.readAccount(account.ref(), ev)
		},
		Pod:   pod,
		InPod: inPod,
	}
	if b.Thread != nil {
		spec.ParkAfter = time.Duration(b.Thread.ParkAfterS) * time.Second
	}
	if resume != "" {
		r.logf("nodev1: lease %s resumes the thread's parked session %s", b.LeaseID, resume)
	}
	account.apply(&spec)
	if b.Egress != nil && lp == nil {
		r.logf("nodev1: lease %s: the kind declares its egress, but this node runs sessions on the host (no --sandbox): nothing confines the session to its relay", b.LeaseID)
	}
	if lp != nil {
		// the pod reaches the model relay through its bridge, not the
		// host's loopback the account was served on
		spec.Env = account.podEnv(spec.Env, lp.bridgeBase())
	}
	sctx := ctx
	if m := b.Harness.Budget.Minutes; m > 0 {
		var stop context.CancelFunc
		sctx, stop = context.WithTimeout(ctx, time.Duration(m)*time.Minute)
		defer stop()
	}
	if a != nil {
		if err := a.Enter(actor.Running); err != nil {
			// a cancelled actor does not run its session
			return outcome{err: err, cause: context.Cause(ctx)}
		}
	}
	sctx, watch := r.watchLease(sctx, b, workspace, mb, ladder, &spec, tokens, grant, account)
	res, err := r.Session.Run(sctx, spec)
	o := outcome{result: res, err: err, finish: fin.result(), handBack: hb.asked(), escalations: taken(ladder.Steps()), evidence: rec.IDs(),
		account: account, harness: b.Harness.Name, model: ladder.Model(), parked: res.Parked, counts: watch.close()}
	o.counts.Tools = sl.usage(o.counts.Tools)
	if ctx.Err() != nil {
		o.cause = context.Cause(ctx)
	} else if rs := stoppedBy(sctx); rs != nil {
		o.stopped, o.err = rs, nil
	} else if errors.Is(err, context.DeadlineExceeded) || sctx.Err() != nil {
		o.err = fmt.Errorf("the session ran past its %d-minute budget", b.Harness.Budget.Minutes)
	}
	if o.err == nil && o.cause == nil && o.stopped == nil && b.Thread == nil && !res.Parked && o.handBack == nil {
		// a one-shot session's finish is its finish tool's answer, or its
		// last message when that fits the kind's finish_schema, or none
		// (settle.go); the learnings the finish carried go to the
		// product's learnings tool while the proxy is still open
		st := settle(b, fin, res.Text, gate.refused())
		o.finish, o.noFinish = st.result, st.noFinish
		r.handInLearnings(ctx, b, sock, fin, st.learnings)
	}
	return o
}

// openProxy serves the brief's servers on a sessionproxy of the lease's
// own — one per lease, a unix socket in a short directory of the lease's
// own (leaseSocketDir) — and answers the session's MCP config (each
// server a `mesh mcp-relay --socket` entry naming it) and the socket.
// Each product server is reached on the lease's mesh-session token with
// a DPoP proof signed for the server's own URL; the node's own tools,
// when the kind names any, are served in process at NodeToolsServer; the
// grant, when there is one, serves the proxy's /git/ route; with sl, the
// product servers serve only the kind's tools (slots.go); and when the
// brief's access names vendor APIs, the proxy's /latchkey/<vendor>/
// route reaches Latchkey's proxy on the session token (wave 2.7). It
// answers only once the socket does (waitListening).
func (r *Runtime) openProxy(ctx context.Context, b *meshapi.Brief, tokens *sessionTokens, grant *gitrelay.Grant, tools []mcptool.Tool, observe func(string, []byte, []byte), account *sessionAccount, sl *slots) (map[string]any, string, func(), error) {
	ups := map[string]sessionproxy.Upstream{}
	for _, s := range b.Servers {
		if s.Name == "" || s.URL == "" {
			return nil, "", nil, fmt.Errorf("the brief names a server without a name or url: %+v", s)
		}
		if s.Name == NodeToolsServer {
			return nil, "", nil, fmt.Errorf("the brief names a server %q, the node's own tools' name", s.Name)
		}
		if !mcpAllowed(b, s.Name) {
			r.logf("nodev1: lease %s: the kind's egress names no MCP of %s; server %s is not served", b.LeaseID, b.Product, s.Name)
			continue
		}
		ups[s.Name] = sessionproxy.Product{URL: s.URL, Bearer: tokens.Token, Key: tokens.Key()}
	}
	if len(tools) > 0 {
		ups[NodeToolsServer] = sessionproxy.InProcess{Handler: &mcptool.Handler{Name: NodeToolsServer, Tools: tools}}
	}
	sockDir, err := r.leaseSocketDir(b.LeaseID)
	if err != nil {
		return nil, "", nil, err
	}
	sock := filepath.Join(sockDir, leaseSocketName)
	p := sessionproxy.New(ups)
	p.Logf = r.Logf
	p.Observe = observe
	// the kind's product tools alone, under its descriptions (slots.go)
	if sl != nil {
		p.Tools, sl.proxy = sl.filter(), p
	}
	// the kind's egress: the relay is the session's only exit (egress.go)
	tunnel := r.egressTunnel(b)
	if tunnel != nil {
		// the harness always reaches the lease's model through the relay:
		// every job is harnessed, so egress.model never closes the route
		// the session's own harness runs on (a model provider's host is
		// reached directly only when egress.hosts names it)
		p.Egress = tunnel
		r.logf("nodev1: lease %s: egress %s", b.LeaseID, egressLine(b.Egress))
	}
	closeAll := func() {
		p.Close()
		if tunnel != nil {
			tunnel.Close()
		}
	}
	if grant != nil {
		p.Git = grant
	}
	if rules := apiRules(b); len(rules) > 0 && tokens != nil {
		iss, err := tokens.issuer()
		if err != nil {
			os.RemoveAll(sockDir)
			return nil, "", nil, err
		}
		p.Latchkey = &sessionproxy.Latchkey{
			URL: iss, Token: tokens.Token, Key: tokens.Key(),
			Allow: apiAllows(rules),
			Audit: func(c sessionproxy.APICall) { r.auditAPI(b.LeaseID, c) },
		}
	}
	// the lease's model account, held here and never by the session
	// (modelrelay.go)
	models := r.ModelUpstreams
	if models == nil {
		models = modelUpstreams()
	}
	if err := account.serve(p, models, r.logf, b.LeaseID); err != nil {
		closeAll()
		os.RemoveAll(sockDir)
		return nil, "", nil, err
	}
	if err := p.Listen(sock); err != nil {
		closeAll()
		os.RemoveAll(sockDir)
		return nil, "", nil, err
	}
	if err := waitListening(ctx, sock, socketReadyWait); err != nil {
		closeAll()
		os.RemoveAll(sockDir)
		return nil, "", nil, err
	}
	servers := map[string]any{}
	for name := range ups {
		servers[name] = harness.ProxyRelay(r.MeshBinary, sock, name)
	}
	return servers, sock, func() { closeAll(); os.RemoveAll(sockDir) }, nil
}

// renew extends the lease every renew_every_s until ctx ends. Mesh
// saying the lease is gone cancels the lease's work; a renewal that
// cannot reach Mesh is tried again until the lease's own expiry passes,
// which cancels it too.
//
// Each tick also renews the lease's room in the scheduler, when it
// holds one: a room that lapsed (the renewals stalled past its TTL) is
// taken again, and logged, since the lease runs on whatever it holds.
func (r *Runtime) renew(ctx context.Context, lease *meshapi.Lease, cancel context.CancelCauseFunc, room *scheduler.Lease) {
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
		if room != nil && !room.Renew() {
			r.logf("nodev1: lease %s: its room in the scheduler lapsed; the node counts it again", lease.LeaseID)
			if again, err := r.Scheduler.TryTake(room.Ask()); err == nil {
				defer again.Release()
				room = again
			} else {
				r.logf("nodev1: lease %s runs past the scheduler: %v", lease.LeaseID, err)
				room = nil
			}
		}
		resp, err := r.renewLease(ctx, lease.LeaseID)
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
//
// A lease that failed after an irreversible write (landedWrites) is not
// handed back cannot_run, which says the job never started and has Mesh
// offer it to run again from scratch: it fails with cause
// infrastructure, a reason naming every write, and a log line that says
// so. Mesh counts infrastructure as a hand-back too (Mesh v1 spec, "Job
// and Lease"): the job keeps its place, the kind breaker hears a node
// fault rather than a red verdict, and max_handbacks still bounds it. A
// work failure would end the job instead, but would charge the work
// with the node's fault. What makes the retry safe is that it builds on
// the writes: Mesh records the lease's pushes from its AccessRecords,
// and the next lease's checkout starts from the most recent of them (the
// brief's pushed; checkout).
func (r *Runtime) report(id string, b *meshapi.Brief, o outcome) actor.End {
	writes := landedWrites(o.access)
	switch {
	case errors.Is(o.cause, errLeaseGone):
		r.logf("nodev1: lease %s ended without a report: %v", id, o.cause)
		return actor.End{Report: actor.ReportNone, Reason: o.cause.Error()}
	case errors.Is(o.cause, errNodeUpdate):
		r.postAccess(id, o.access)
		r.postUsage(id, o)
		reason := afterWrites(r.cancelReason(id), writes)
		r.handBack(id, meshapi.CauseNodeUpdate, reason, o.result.SessionID)
		return actor.End{Report: actor.ReportHandBack, Cause: meshapi.CauseNodeUpdate, Reason: reason}
	case errors.Is(o.cause, errStalled):
		r.postAccess(id, o.access)
		r.postUsage(id, o)
		reason := afterWrites(r.cancelReason(id), writes)
		r.handBack(id, meshapi.CauseStalled, reason, o.result.SessionID)
		return actor.End{Report: actor.ReportHandBack, Cause: meshapi.CauseStalled, Reason: reason}
	case errors.Is(o.cause, errCancelled):
		r.postAccess(id, o.access)
		r.postUsage(id, o)
		reason := afterWrites(r.cancelReason(id), writes)
		r.handBack(id, meshapi.CauseCancelled, reason, o.result.SessionID)
		return actor.End{Report: actor.ReportHandBack, Cause: meshapi.CauseCancelled, Reason: reason}
	case o.nodeEnding:
		r.postAccess(id, o.access)
		r.postUsage(id, o)
		reason := afterWrites("the node stopped while the session ran", writes)
		r.handBack(id, meshapi.CauseNodeRestart, reason, o.result.SessionID)
		return actor.End{Report: actor.ReportHandBack, Cause: meshapi.CauseNodeRestart, Reason: reason}
	case infrastructureCause(o.cause) != "":
		// a node tool's runner failed under the session (causes.go)
		r.postAccess(id, o.access)
		r.postUsage(id, o)
		cause, reason := infrastructureCause(o.cause), afterWrites(o.cause.Error(), writes)
		r.logf("nodev1: lease %s: %s", id, reason)
		r.handBack(id, cause, reason, o.result.SessionID)
		return actor.End{Report: actor.ReportHandBack, Cause: cause, Reason: reason}
	}
	// a stop rule's end fails the job, the rule named first in the
	// reason (projectmesh-io/api adr-001-amendment-learnings-and-
	// transcripts.md)
	if o.stopped != nil {
		r.postAccess(id, o.access)
		r.postUsage(id, o)
		reason := afterWrites(o.stopped.fired.Reason(), writes)
		r.fail(id, meshapi.CauseStoppedByRule, reason)
		return actor.End{Report: actor.ReportFail, Cause: meshapi.CauseStoppedByRule, Reason: reason}
	}
	// a thread quiet for its park_after hands back parked, keeping its
	// session for the next turn (projectmesh-io/api ADR 001, Threads)
	if o.parked && o.err == nil && o.cause == nil {
		r.postAccess(id, o.access)
		r.postUsage(id, o)
		reason := "the thread was quiet for its park_after"
		r.handBack(id, meshapi.CauseParked, reason, o.result.SessionID)
		return actor.End{Report: actor.ReportHandBack, Cause: meshapi.CauseParked, Reason: reason}
	}
	// a job actor's session, its pod or a run_suite step killed from
	// outside is the node's condition, never the work's (ADR-044): handed
	// back pod_killed with the exit code it was killed with (mesh-specs
	// gap 14), writes or not, since the next lease builds on what was
	// pushed
	if why, code, ok := podKilled(o); ok && r.Actors {
		r.postAccess(id, o.access)
		r.postUsage(id, o)
		reason := afterWrites(why, writes)
		r.logf("nodev1: lease %s: %s", id, reason)
		r.handBackExit(id, meshapi.CausePodKilled, reason, o.result.SessionID, code)
		return actor.End{Report: actor.ReportHandBack, Cause: meshapi.CausePodKilled, Reason: reason}
	}
	// a session that gave the job back with the node's hand_back tool is
	// handed back with the cause it claimed, however its session then
	// exited (projectmesh-io/api ADR 001 amendment, node tools); a
	// cannot_run after an irreversible write fails infrastructure, as the
	// node's own would below
	if hb := o.handBack; hb != nil {
		r.postAccess(id, o.access)
		r.postUsage(id, o)
		reason := afterWrites(hb.Reason, writes)
		if hb.Cause == meshapi.CauseCannotRun && len(writes) > 0 {
			r.logf("nodev1: lease %s: the session handed back cannot_run AFTER IRREVERSIBLE WRITES (%s)", id, strings.Join(writes, "; "))
			r.fail(id, meshapi.CauseInfrastructure, reason)
			return actor.End{Report: actor.ReportFail, Cause: meshapi.CauseInfrastructure, Reason: reason}
		}
		r.logf("nodev1: lease %s: the session handed the job back (%s): %s", id, hb.Cause, clip(hb.Reason, 200))
		r.handBack(id, hb.Cause, reason, o.result.SessionID)
		return actor.End{Report: actor.ReportHandBack, Cause: hb.Cause, Reason: reason}
	}
	ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
	defer cancel()
	r.postAccess(id, o.access)
	r.postUsage(id, o)
	if o.err != nil && len(writes) > 0 {
		r.logf("nodev1: lease %s failed AFTER IRREVERSIBLE WRITES (%s): %v", id, strings.Join(writes, "; "), o.err)
		reason := afterWrites(o.err.Error(), writes)
		r.fail(id, meshapi.CauseInfrastructure, reason)
		return actor.End{Report: actor.ReportFail, Cause: meshapi.CauseInfrastructure, Reason: reason}
	}
	if o.err != nil {
		// a credential not minted or a node tool not served before the
		// session started says so (causes.go); anything else could not run
		cause := cmp.Or(infrastructureCause(o.err), meshapi.CauseCannotRun)
		r.handBack(id, cause, o.err.Error(), o.result.SessionID)
		return actor.End{Report: actor.ReportHandBack, Cause: cause, Reason: o.err.Error()}
	}
	if o.noFinish != "" {
		r.logf("nodev1: lease %s: no finish: %s", id, o.noFinish)
		r.fail(id, meshapi.CauseNoFinish, o.noFinish)
		return actor.End{Report: actor.ReportFail, Cause: meshapi.CauseNoFinish, Reason: o.noFinish}
	}
	result := o.finish
	if result == nil {
		result, _ = json.Marshal(map[string]string{"text": clip(o.result.Text, 16<<10)})
	}
	st, err := r.API.Finish(ctx, id, meshapi.FinishRequest{Result: result, SessionRef: o.result.SessionID, Evidence: o.evidence})
	if err != nil {
		r.logf("nodev1: lease %s: finish: %v", id, err)
		return actor.End{Report: actor.ReportFinish, Reason: err.Error()}
	}
	r.logf("nodev1: lease %s finished; job %s is %s", id, st.JobID, st.Status)
	return actor.End{Report: actor.ReportFinish}
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

// postUsage reports what the lease's session spent, when it spent
// anything: the lease's whole usage so far, which replaces any earlier
// report at Mesh. A hand-back's spend is the job's too.
func (r *Runtime) postUsage(id string, o outcome) {
	u := usageOf(o)
	if u == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
	defer cancel()
	if err := r.API.Usage(ctx, id, *u); err != nil {
		r.logf("nodev1: lease %s: usage: %v", id, err)
	}
}

// fail reports the lease failed with cause: work ends the job, any
// other cause Mesh takes as a hand-back.
func (r *Runtime) fail(id, cause, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
	defer cancel()
	st, err := r.API.Fail(ctx, id, meshapi.FailRequest{Cause: cause, Reason: clip(reason, maxReason)})
	if err != nil && meshapi.LeaseGone(err) {
		r.logf("nodev1: lease %s was no longer the node's to fail (%s)", id, cause)
		return
	}
	if err != nil {
		r.logf("nodev1: lease %s: fail (%s): %v", id, cause, err)
		return
	}
	r.logf("nodev1: lease %s failed (%s); job %s is %s", id, cause, st.JobID, st.Status)
}

func (r *Runtime) handBack(id, cause, reason, sessionRef string) {
	r.handBackExit(id, cause, reason, sessionRef, 0)
}

// handBackExit is handBack carrying the exit code (137, 143) the
// session, its pod or a step was killed with; 0 is none.
func (r *Runtime) handBackExit(id, cause, reason, sessionRef string, exitCode int) {
	ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
	defer cancel()
	st, err := r.API.HandBack(ctx, id, meshapi.HandBackRequest{Cause: cause, Reason: clip(reason, maxReason), SessionRef: sessionRef, ExitCode: exitCode})
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

// usageOf is what the session spent — summed, by model, and the
// escalations it took — in Mesh's JobUsage shape (projectmesh-io/api
// contracts/v1/lease-usage.json); nil when it reported nothing. A
// streaming session names its spend by model; a session that did not
// (an older CLI) has one model's. Input tokens count the cached ones
// too. The list cost is the CLI's; the actual cost is the list cost on
// an api-key account and nothing on a subscription or the node's own
// login.
func usageOf(o outcome) *meshapi.UsageRequest {
	res := o.result
	c := o.counts
	if res.Usage == nil && res.CostUSD == 0 && len(res.Models) == 0 && len(o.escalations) == 0 &&
		c.ToolCalls == 0 && len(c.Tools) == 0 && len(c.StopRules) == 0 && c.Transcript == nil {
		return nil
	}
	apiKey := o.account.apiKey()
	actual := func(list int64) int64 {
		if apiKey {
			return list
		}
		return 0
	}
	u := &meshapi.UsageRequest{
		Harness: o.harness, Model: o.model, AccountRef: o.account.ref(),
		Turns: int64(res.Turns), DurationMS: res.Duration.Milliseconds(),
		Models: []meshapi.ModelUsage{}, Escalations: o.escalations, Counts: o.counts,
	}
	if u.Harness == "" {
		u.Harness = "claude-code"
	}
	if u.Escalations == nil {
		u.Escalations = []meshapi.Escalation{}
	}
	if len(res.Models) > 0 {
		names := make([]string, 0, len(res.Models))
		for name := range res.Models {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			m := res.Models[name]
			list := cents(m.CostUSD)
			u.Models = append(u.Models, meshapi.ModelUsage{
				Model:            name,
				TokensInput:      m.InputTokens + m.CacheReadInputTokens + m.CacheCreationInputTokens,
				TokensOutput:     m.OutputTokens,
				TokensCacheRead:  m.CacheReadInputTokens,
				TokensCacheWrite: m.CacheCreationInputTokens,
				CostListCents:    list,
				CostActualCents:  actual(list),
				Turns:            int64(res.ModelTurns[name]),
			})
		}
	} else if r := res.Usage; r != nil || res.CostUSD > 0 {
		m := meshapi.ModelUsage{CostListCents: cents(res.CostUSD), Turns: int64(res.Turns)}
		if r != nil {
			m.Model = r.Model
			m.TokensInput, m.TokensCacheRead, m.TokensCacheWrite = r.InputTokens, r.CachedInputTokens, r.CacheWriteInputTokens
			m.TokensOutput = r.OutputTokens
		}
		m.CostActualCents = actual(m.CostListCents)
		if m.Model == "" {
			m.Model = cmp.Or(o.model, "unknown")
		}
		u.Models = []meshapi.ModelUsage{m}
	}
	for _, m := range u.Models {
		u.TokensInput += m.TokensInput
		u.TokensOutput += m.TokensOutput
		u.TokensCacheRead += m.TokensCacheRead
		u.TokensCacheWrite += m.TokensCacheWrite
		u.CostListCents += m.CostListCents
		u.CostActualCents += m.CostActualCents
	}
	if u.Model == "" && len(u.Models) == 1 {
		u.Model = u.Models[0].Model
	}
	return u
}

// cents is a dollar amount in whole cents, rounded.
func cents(usd float64) int64 {
	return int64(math.Round(usd * 100))
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
