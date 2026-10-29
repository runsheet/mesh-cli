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
	"time"

	"projectmesh.io/mesh-cli/internal/actor"
	"projectmesh.io/mesh-cli/internal/escalate"
	"projectmesh.io/mesh-cli/internal/gitrelay"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/mcptool"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/nodetools"
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
	// account is what the session ran on: the job's model account or
	// the node's own login.
	account *sessionAccount
	// harness and model are the session's harness and the model it
	// ended on, for the usage report.
	harness, model string
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
	r.mu.Lock()
	r.leases.Put(id, &running{cancel: cancel, progressAt: time.Now(), mailbox: mb, turnTool: b.TurnTool})
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
	r.report(id, b, out)
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
	dir := filepath.Join(r.WorkRoot, "leases", b.LeaseID)
	workspace := filepath.Join(dir, "work")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return outcome{err: fmt.Errorf("the lease's directory: %w", err)}
	}
	if a == nil {
		keep("the lease's directory", func() error { return os.RemoveAll(dir) })
	}

	var tokens *sessionTokens
	if len(b.Servers) > 0 || b.Git != nil || len(apiRules(b)) > 0 || b.CredentialRef != "" {
		var err error
		if tokens, err = openSession(ctx, r.API, b.LeaseID, r.LatchkeyHTTP); err != nil {
			return outcome{err: err, cause: context.Cause(ctx)}
		}
	}
	// the job's model account, when Mesh picked one: read now, so a lease
	// whose account Latchkey refuses is handed back before any checkout
	account, err := openAccount(ctx, b.CredentialRef, tokens)
	if err != nil {
		return outcome{err: err, cause: context.Cause(ctx)}
	}
	var grant *gitrelay.Grant
	var resumed *resumePoint
	if b.Git != nil && b.Git.Repo != "" {
		policy := policyOf(b.Git)
		mint := func(ctx context.Context) (gitrelay.Token, error) {
			return tokens.MintSource(ctx, sourceRequest(b.Git, policy))
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
		if resumed, err = checkout(ctx, grant, b, workspace, r.mirrors, r.logf); err != nil {
			return outcome{err: fmt.Errorf("checking out %s: %w", b.Git.Repo, err), cause: context.Cause(ctx)}
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
	fin := &finishCapture{tool: b.FinishTool, onFinish: mb.Finish}
	var suites nodetools.SuiteRunner
	if lp != nil {
		suites = lp.suites
	}
	tools, rec := r.nodeTools(b, grant, workspace, tokens, ladder, mb, suites)
	servers, sock, closeProxy, err := r.openProxy(ctx, b, tokens, grant, tools, fin.observe)
	if err != nil {
		return outcome{err: fmt.Errorf("the lease's sessionproxy: %w", err), cause: context.Cause(ctx)}
	}
	keep(holdProxy, func() error { closeProxy(); return nil })
	env := []string{"GIT_TERMINAL_PROMPT=0"}
	if len(apiRules(b)) > 0 {
		env = append(env, RelaySocketEnv+"="+sock)
	}
	prompt := promptWithRelay(b, sock, resumed)
	var inPod PodRun
	if lp != nil {
		if servers, err = lp.podServers(servers, filepath.Dir(sock)); err != nil {
			return outcome{err: err, cause: context.Cause(ctx)}
		}
		gitBase := ""
		if grant != nil {
			gitBase = grant.Base()
		}
		if inPod, err = lp.run(workspace, gitBase, sock); err != nil {
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
		Prompt:    prompt,
		Model:     b.Harness.Model.Default,
		BudgetUSD: b.Harness.Budget.USD,
		Servers:   servers,
		Env:       env,
		Unset:     gitEnvUnset,
		Mailbox:   mb,
		Ladder:    ladder,
		Thread:    b.Thread != nil,
		OnEvent: func(ev harness.Event) {
			r.touch(b.LeaseID)
			r.readAccount(account.ref(), ev)
		},
		Pod:   pod,
		InPod: inPod,
	}
	account.apply(&spec)
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
	res, err := r.Session.Run(sctx, spec)
	o := outcome{result: res, err: err, finish: fin.result(), escalations: taken(ladder.Steps()), evidence: rec.IDs(),
		account: account, harness: b.Harness.Name, model: ladder.Model()}
	if ctx.Err() != nil {
		o.cause = context.Cause(ctx)
	} else if errors.Is(err, context.DeadlineExceeded) || sctx.Err() != nil {
		o.err = fmt.Errorf("the session ran past its %d-minute budget", b.Harness.Budget.Minutes)
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
// grant, when there is one, serves the proxy's /git/ route; and when the
// brief's access names vendor APIs, the proxy's /latchkey/<vendor>/
// route reaches Latchkey's proxy on the session token (wave 2.7). It
// answers only once the socket does (waitListening).
func (r *Runtime) openProxy(ctx context.Context, b *meshapi.Brief, tokens *sessionTokens, grant *gitrelay.Grant, tools []mcptool.Tool, observe func(string, []byte, []byte)) (map[string]any, string, func(), error) {
	ups := map[string]sessionproxy.Upstream{}
	for _, s := range b.Servers {
		if s.Name == "" || s.URL == "" {
			return nil, "", nil, fmt.Errorf("the brief names a server without a name or url: %+v", s)
		}
		if s.Name == NodeToolsServer {
			return nil, "", nil, fmt.Errorf("the brief names a server %q, the node's own tools' name", s.Name)
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
	if err := p.Listen(sock); err != nil {
		os.RemoveAll(sockDir)
		return nil, "", nil, err
	}
	if err := waitListening(ctx, sock, socketReadyWait); err != nil {
		p.Close()
		os.RemoveAll(sockDir)
		return nil, "", nil, err
	}
	servers := map[string]any{}
	for name := range ups {
		servers[name] = harness.ProxyRelay(r.MeshBinary, sock, name)
	}
	return servers, sock, func() { p.Close(); os.RemoveAll(sockDir) }, nil
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
	}
	// a job actor's session or pod killed from outside is the node's
	// condition, never the work's (ADR-044): handed back pod_killed,
	// writes or not, since the next lease builds on what was pushed
	if why, ok := podKilled(o); ok && r.Actors {
		r.postAccess(id, o.access)
		r.postUsage(id, o)
		reason := afterWrites(why, writes)
		r.logf("nodev1: lease %s: %s", id, reason)
		r.handBack(id, meshapi.CausePodKilled, reason, o.result.SessionID)
		return actor.End{Report: actor.ReportHandBack, Cause: meshapi.CausePodKilled, Reason: reason}
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
		r.handBack(id, meshapi.CauseCannotRun, o.err.Error(), o.result.SessionID)
		return actor.End{Report: actor.ReportHandBack, Cause: meshapi.CauseCannotRun, Reason: o.err.Error()}
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
	if res.Usage == nil && res.CostUSD == 0 && len(res.Models) == 0 && len(o.escalations) == 0 {
		return nil
	}
	apiKey := o.account != nil && o.account.cred.apiKey()
	actual := func(list int64) int64 {
		if apiKey {
			return list
		}
		return 0
	}
	u := &meshapi.UsageRequest{
		Harness: o.harness, Model: o.model, AccountRef: o.account.ref(),
		Turns: int64(res.Turns), DurationMS: res.Duration.Milliseconds(),
		Models: []meshapi.ModelUsage{}, Escalations: o.escalations,
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
