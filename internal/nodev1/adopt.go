package nodev1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/actor"
	"projectmesh.io/mesh-cli/internal/egress"
	"projectmesh.io/mesh-cli/internal/escalate"
	"projectmesh.io/mesh-cli/internal/gitrelay"
	"projectmesh.io/mesh-cli/internal/mcptool"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/nodetools"
	"projectmesh.io/mesh-cli/internal/relaysidecar"
)

// Start-up adoption (ADR-044, the start-up amendment): a lease an
// upgrade left running (Upgrade, handoff.go) is picked back up by the
// node that starts next, under --actors. Nothing of the lease's pod,
// sidecar or agent is touched -- they were never stopped -- only the
// node's own side of it is replumbed from nothing: a fresh session (a
// new DPoP key, never the hand-off's old one, so a process that no
// longer exists can never again be mistaken for this one), pushed to
// the sidecar as its lease's credentials from now on
// (relaysidecar.ReplaceCredentials), its room in the scheduler taken
// again, its agent's output followed from where the hand-off left it
// (each line the session's progress, followAgentOutput), and a fresh
// relaysidecar.Node listening on the sidecar's own node socket, with
// what the sidecar forwards to the node -- the lease's model account's
// forwarder and the node tools that need nothing the old process held
// (nodeProxy, adoptedNodeTools) -- its AccessRecords, its token
// renewal pull (Renew, mirroring the
// sidecar's own rs.renew), its Stop gate (openStopGate, as a fresh
// start would open it, with no earlier hand-back request or evidence
// record to carry across the restart) and, for a lease whose egress
// runs as a process of its own (ADR-051 decisions 1 and 4), fresh
// credentials pushed to that process's control socket and its node
// socket's CONNECT forwarded onto the egress process's own
// (forwardConnect, relaypod.go) -- beside Observe, Security and
// Canary as before. A claimed egress process is noted in actor.json
// (actor.NoteEgress) and held the same way a freshly started one is,
// so a second crash -- the adopting node itself dying before it
// releases -- still finds and stops it. A lease whose egress process
// has died since the hand-off is still adopted with no egress tunnel,
// logged, never handed back over it alone; one that is alive but
// refuses the fresh credentials, or cannot be reached at all, is
// stopped instead (stopUnclaimedEgress): Upgrade disowned it the
// moment the hand-off was written (handoff.go), so nothing else is
// left to claim or stop it (ADR-051 decision 4). The lease's pod
// session is not reattached, so the node tools that run through it
// (run_suite, stop_suite) are not served again. A hand-off that cannot
// be adopted this way -- unreadable, no sidecar named, its model
// account unreadable, or the sidecar refuses the new credentials -- is
// handed back node_update instead, the same cause
// the drain's wait hands a lease back with, and its job directory
// removed.

// adoptHandoffs picks up every hand-off a prior process left under the
// node's leases directory, each on its own goroutine so one lease's
// adoption never waits on another's. It returns the lease ids it is
// adopting, built synchronously before any adoptOne goroutine is
// spawned, so the caller can keep releaseLeftovers -- which runs right
// after, on the local filesystem alone -- from treating one of them as
// abandoned before adoptOne has had the chance to register it in
// r.leases: adoptOne's first network call (openSession) can take long
// enough for releaseLeftovers to run and read the same actor.json
// first. It logs exactly one aggregate line before it returns, "adopted
// N of M hand-off(s) at start" -- M every hand-off file it found
// readable, whether or not it could then be parsed, N the ones it is
// adopting -- so a start with none logs "adopted 0 of 0" rather than
// nothing at all.
func (r *Runtime) adoptHandoffs(ctx context.Context) map[string]bool {
	adopting := map[string]bool{}
	found := 0
	defer func() {
		r.logf("nodev1: adopted %d of %d hand-off(s) at start", len(adopting), found)
	}()
	root := filepath.Join(r.WorkRoot, "leases")
	entries, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) {
			r.logf("nodev1: reading %s for hand-offs to adopt: %v", root, err)
		}
		r.finishAdoptScan(0)
		return adopting
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		dir := filepath.Join(root, id)
		path := filepath.Join(dir, handoffFile)
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				r.logf("nodev1: lease %s: reading its hand-off: %v", id, err)
			}
			continue
		}
		found++
		var st handoffState
		if err := json.Unmarshal(data, &st); err != nil || st.Brief == nil {
			r.logf("nodev1: lease %s: its hand-off could not be read; handed back", id)
			r.handBack(id, meshapi.CauseNodeUpdate, "its hand-off could not be read", "")
			_ = os.RemoveAll(dir)
			continue
		}
		adopting[id] = true
		go r.adoptOne(ctx, id, dir, &st)
	}
	r.finishAdoptScan(len(adopting))
	return adopting
}

// finishAdoptScan marks adoptHandoffs' synchronous scan-and-spawn phase
// done, n the hand-offs it is adopting (0 when it found none, or could
// not read the leases directory at all): until this runs, Status reads
// adoption as still in flight, since a lease newly visible in leases is
// not by itself proof that every hand-off has been picked up yet.
func (r *Runtime) finishAdoptScan(n int) {
	r.mu.Lock()
	r.adoptScanned = true
	r.adoptPending = n
	r.mu.Unlock()
	r.publishStatus()
}

// adoptionDecided marks one hand-off's adoption as decided, whether it
// was adopted or handed back; Status reads adoption as complete once
// every one adoptHandoffs spawned has reached this.
func (r *Runtime) adoptionDecided() {
	r.mu.Lock()
	if r.adoptPending > 0 {
		r.adoptPending--
	}
	r.mu.Unlock()
	r.publishStatus()
}

// cannotAdopt hands a hand-off back node_update and removes its job
// directory: the way out for every hand-off adoptOne cannot pick back
// up before it has registered a running lease or started its actor.
func (r *Runtime) cannotAdopt(id, dir, reason string) {
	r.logf("nodev1: lease %s: %s; handed back", id, reason)
	r.handBack(id, meshapi.CauseNodeUpdate, reason, "")
	_ = os.RemoveAll(dir)
}

// adoptOne picks up one lease's hand-off: it runs exactly the shape of
// runActor's own tail (renew, wait for the mailbox to finish or the
// lease's context to end, then report and release) once the lease's
// session, its sidecar's credentials and its node socket are in place.
func (r *Runtime) adoptOne(ctx context.Context, id, dir string, st *handoffState) {
	var decideOnce sync.Once
	decide := func() { decideOnce.Do(r.adoptionDecided) }
	defer decide()
	b := st.Brief
	if st.SidecarDir == "" {
		r.cannotAdopt(id, dir, "its hand-off named no relay sidecar to adopt")
		return
	}
	tokens, err := openSession(ctx, r.API, id, r.LatchkeyHTTP)
	if err != nil {
		r.cannotAdopt(id, dir, fmt.Sprintf("opening its session again: %v", err))
		return
	}
	tok, exp, err := tokens.current(ctx)
	if err != nil {
		r.cannotAdopt(id, dir, fmt.Sprintf("its new session token: %v", err))
		return
	}
	// the lease's model account, read again on its new session: the
	// session's model calls reach the node's own forwarder (nodeProxy),
	// which held the account in the memory of the process that is gone
	account, err := openAccount(ctx, b.CredentialRef, tokens, os.Environ())
	if err != nil {
		r.cannotAdopt(id, dir, fmt.Sprintf("its model account: %v", err))
		return
	}
	handoffKey, err := tokens.Key().Handoff()
	if err != nil {
		r.cannotAdopt(id, dir, fmt.Sprintf("its new key: %v", err))
		return
	}
	socket := filepath.Join(st.SidecarDir, relayControlDir, relayControlSocket)
	req := relaysidecar.ReplaceCredentialsRequest{Key: handoffKey, Session: relaysidecar.Token{Value: tok, ExpiresAt: exp}}
	res, err := relaysidecar.ReplaceCredentialsClient{Socket: socket}.Replace(ctx, req)
	if err != nil {
		r.cannotAdopt(id, dir, fmt.Sprintf("its relay sidecar: %v", err))
		return
	}
	if !res.Replaced {
		r.cannotAdopt(id, dir, fmt.Sprintf("its relay sidecar refused its new credentials: %s", res.Refused))
		return
	}
	a, err := actor.Start(dir, actor.Record{LeaseID: id, JobID: b.JobID, Kind: b.Kind})
	if err != nil {
		r.cannotAdopt(id, dir, fmt.Sprintf("its actor: %v", err))
		return
	}
	a.Logf = r.Logf
	defer a.Release()
	defer func() {
		if p := recover(); p != nil {
			r.logf("nodev1: adopted lease %s panicked: %v\n%s", id, p, debug.Stack())
			end := actor.End{Report: actor.ReportHandBack, Cause: meshapi.CausePanic, Reason: fmt.Sprintf("the node panicked: %v", p)}
			r.endActor(a, id, end, func() { r.handBack(id, end.Cause, end.Reason, "") })
		}
	}()

	leaseCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	mb := NewMailbox()
	mb.turns = restoreTurnLedger(st.Turns)
	fin := &finishCapture{tool: b.FinishTool, learningsTool: b.LearningsTool, onFinish: mb.Finish}
	hb := &sessionHandBack{}

	// the hand-off's egress process is the lease's to the supervisor
	// from here, while its adoption decides whether to keep it
	// (setEgressStarting)
	r.mu.Lock()
	r.leases.Put(id, &running{cancel: cancel, progressAt: time.Now(), mailbox: mb, turnTool: turnToolFor(b),
		jobID: b.JobID, kind: b.Kind, started: time.Now(), actor: a, brief: b,
		pushHeads: st.PushHeads, sidecarDir: st.SidecarDir, egressStarting: st.EgressPID})
	r.mu.Unlock()
	r.publishStatus()
	var sec = r.openSecurity(id)
	defer r.closeSecurity(sec)
	v := r.openInjected(id)
	defer r.closeInjected(v)
	defer func() {
		r.mu.Lock()
		r.leases.Delete(id)
		r.mu.Unlock()
		r.publishStatus()
	}()

	var gitReq SourceRequest
	if b.Git != nil && b.Git.Repo != "" {
		policy := policyOf(b.Git)
		policy.Heads = st.PushHeads
		gitReq = sourceRequest(b.Git, policy)
	}
	var mintMu sync.Mutex
	var minted []gitrelay.Token
	mintGit := func(ctx context.Context, req SourceRequest) (relaysidecar.Token, error) {
		t, err := tokens.MintSource(ctx, req)
		if err != nil {
			return relaysidecar.Token{}, err
		}
		v.addGitToken(b.Git, req.Repository, t)
		mintMu.Lock()
		minted = append(minted, t)
		mintMu.Unlock()
		return relaysidecar.Token{Value: t.Value, ExpiresAt: t.ExpiresAt}, nil
	}
	gitRequestFor := func(repo string) (SourceRequest, error) {
		if repo == "" || strings.EqualFold(repo, gitReq.Repository) {
			return gitReq, nil
		}
		for _, rr := range b.Git.ReadRepos {
			if strings.EqualFold(repo, rr) {
				return sourceRequestFor(b.Git, rr, gitrelay.Read), nil
			}
		}
		return SourceRequest{}, fmt.Errorf("the brief names no read repo %s", repo)
	}
	// renew is adoptOne's analog of the sidecar's own rs.renew
	// (relaypod.go): the lease's session token always, and a git
	// token when asked -- the job's own repository's, or a read-only
	// one of a read_repo the brief names. Nothing once the lease has
	// ended.
	renew := func(ctx context.Context, req relaysidecar.RenewRequest) (relaysidecar.Renewal, error) {
		if leaseCtx.Err() != nil {
			return relaysidecar.Renewal{}, errors.New("the lease has ended")
		}
		tok, exp, err := tokens.current(ctx)
		if err != nil {
			return relaysidecar.Renewal{}, err
		}
		out := relaysidecar.Renewal{Session: relaysidecar.Token{Value: tok, ExpiresAt: exp}}
		if req.Git {
			if gitReq.Repository == "" {
				return relaysidecar.Renewal{}, errors.New("the lease has no git")
			}
			mint, err := gitRequestFor(req.Repo)
			if err != nil {
				return relaysidecar.Renewal{}, err
			}
			t, err := mintGit(ctx, mint)
			if err != nil {
				return relaysidecar.Renewal{}, err
			}
			out.Git = &t
		}
		return out, nil
	}

	nodeSock := filepath.Join(st.SidecarDir, relayNodeSocket)

	// the node's side of the lease's socket past the tunnel, as rs.open
	// serves it for a lease it starts: the model forwarder and the node
	// tools the session reaches through the sidecar (nodeProxy), whose
	// first making died with the process that made it. Without them the
	// sidecar's every model call and node tool call is answered 404.
	if v != nil {
		account.inspect = v.inspectModel
	}
	// the job's account refused by its vendor ends the lease, handed
	// back account_refused (accountrefused.go)
	account.onRefused = func(err error) { cancel(err) }
	tools, rec := r.adoptedTools(leaseCtx, b, filepath.Join(dir, "work"), tokens, mb, fin, hb, st.SidecarDir)
	np, err := r.nodeProxy(b, tools, account, "")
	if err != nil {
		end := actor.End{Report: actor.ReportHandBack, Cause: meshapi.CauseNodeUpdate, Reason: fmt.Sprintf("its model relay: %v", err)}
		r.endActor(a, id, end, func() { r.handBack(id, end.Cause, end.Reason, "") })
		return
	}
	gate := r.openStopGate(b, nodeSock, fin, hb, rec)

	node := &relaysidecar.Node{Proxy: np, Observe: fin.observe, Security: r.securityReporter(id), Canary: v.canary(), Logf: r.logf, Renew: renew,
		Access: func(rec meshapi.AccessRecord) { r.keepAccess(id, rec) }}
	if gate != nil {
		node.Stop = gate.handler()
	}
	if st.EgressControl != "" {
		if !egressAlive(st.EgressPID) {
			r.logf("nodev1: lease %s: its egress process (pid %d) is gone; adopted with no egress tunnel", id, st.EgressPID)
		} else {
			egRes, egErr := LeaseEgressClient{Socket: st.EgressControl}.Credentials(ctx, LeaseEgressCredentialsRequest{Key: handoffKey, Session: relaysidecar.Token{Value: tok, ExpiresAt: exp}})
			switch {
			case egErr != nil:
				r.logf("nodev1: lease %s: pushing fresh credentials to its egress process: %v; adopted with no egress tunnel", id, egErr)
				stopUnclaimedEgress(r, id, st.EgressPID)
			case !egRes.Replaced:
				r.logf("nodev1: lease %s: its egress process refused its new credentials: %s; adopted with no egress tunnel", id, egRes.Refused)
				stopUnclaimedEgress(r, id, st.EgressPID)
			default:
				node.Tunnel = forwardConnect(filepath.Join(filepath.Dir(st.EgressControl), egressNodeSocket), func(req *http.Request) bool { return egress.IsSpliceOnly(req.Context()) })
			}
		}
	}
	// the egress process is claimed -- named in actor.json and held --
	// before the node socket serves anything through it, so no request
	// ever rides an egress process a second crash would not find
	if node.Tunnel != nil {
		r.setEgress(id, st.EgressPID, st.EgressControl)
		a.NoteEgress(st.EgressPID, st.EgressControl)
		a.Hold("the lease's egress process", func() error { return stopLeftoverEgress(st.EgressPID) })
	}
	if err := node.Listen(nodeSock); err != nil {
		end := actor.End{Report: actor.ReportHandBack, Cause: meshapi.CauseNodeUpdate, Reason: fmt.Sprintf("its node socket: %v", err)}
		r.endActor(a, id, end, func() { r.handBack(id, end.Cause, end.Reason, "") })
		return
	}
	defer node.Close()
	if gate != nil {
		a.Hold("the stop gate", func() error { gate.close(); return nil })
	}

	if n, err := r.pastOffset(id, st.AgentStdoutOffset); err != nil {
		r.logf("nodev1: lease %s: resuming its agent output: %v", id, err)
	} else if n > 0 {
		r.logf("nodev1: lease %s: resumed its agent output, %d byte(s) past offset %d", id, n, st.AgentStdoutOffset)
	}
	// the session's own stream moves the lease's progress from here, as
	// it did for the process that started it: without it the adopted
	// lease's progress_at stays where adoption set it, and Mesh's stall
	// reaper hands it back however busy the session is
	followed := make(chan struct{})
	go func() {
		defer close(followed)
		r.followAgentOutput(leaseCtx, id, st.AgentStdoutOffset)
	}()

	lease := &meshapi.Lease{LeaseID: b.LeaseID, JobID: b.JobID, N: b.LeaseN, ExpiresAt: b.LeaseExpiresAt, RenewEveryS: b.RenewEveryS}
	room, roomErr := r.holdCapacity(leaseCtx, b, lease)
	if roomErr != nil {
		end := actor.End{Report: actor.ReportHandBack, Cause: meshapi.CauseCannotRun, Reason: "the node had no room for the adopted lease: " + roomErr.Error()}
		r.endActor(a, id, end, func() { r.handBack(id, end.Cause, end.Reason, "") })
		return
	}
	if room != nil {
		a.Hold("room", func() error { room.Release(); return nil })
	}
	r.adoptLanes(leaseCtx, a, id, st)
	r.logf("nodev1: lease %s of job %s (%s) adopted at start", id, b.JobID, b.Kind)
	decide()

	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		r.renew(leaseCtx, lease, cancel, room)
	}()

	select {
	case <-mb.Finished():
	case <-leaseCtx.Done():
	}
	// why the lease's context ended, when it ended before the session's
	// finish: Mesh took the lease back, cancelled it, an upgrade handed
	// it back, or its account was refused -- reported as runActor
	// reports each (report)
	var cause error
	if leaseCtx.Err() != nil {
		cause = context.Cause(leaseCtx)
	}
	cancel(nil)
	<-renewDone
	<-followed

	mintMu.Lock()
	toRevoke := minted
	mintMu.Unlock()
	if len(toRevoke) > 0 {
		rctx, rcancel := context.WithTimeout(context.Background(), reportTimeout)
		if err := tokens.RevokeSource(rctx, toRevoke); err != nil {
			r.logf("nodev1: lease %s: revoking its relay sidecar's git tokens: %v", id, err)
		}
		rcancel()
	}

	out := outcome{finish: fin.result(), handBack: hb.asked(), cause: cause, evidence: rec.IDs(), account: account, harness: b.Harness.Name, provider: providerOf(account, b.Harness.Name)}
	out.nodeEnding = ctx.Err() != nil
	a.ReleaseNow(holdProxy, holdGrant)
	r.releaseCarried(id)
	out.access = r.takeAccess(id)
	reportSandbox(sec, out.err)
	if err := a.Enter(actor.Reporting); err != nil {
		r.logf("nodev1: lease %s: %v", id, err)
	}
	end := r.report(id, b, out)
	a.Ended(end)
	a.Reported()
	r.settleThread(b, out.result.SessionID, end.Cause == meshapi.CauseParked)
}

// stopUnclaimedEgress stops a hand-off's egress process once adoptOne
// has decided not to claim it -- confirmed alive (egressAlive already
// checked) but refusing fresh credentials, or unreachable over its
// control socket. Upgrade (handoff.go) disowns a lease's egress
// process the moment its hand-off is written, whether or not adoption
// goes on to succeed: from then on nothing but adoptOne's own Hold
// (the success path, above) or this call stops it, so a process this
// branch left alone would never be found by either process's
// clean-up again (ADR-051 decision 4).
func stopUnclaimedEgress(r *Runtime, id string, pid int) {
	if err := stopLeftoverEgress(pid); err != nil {
		r.logf("nodev1: lease %s: stopping its unclaimed egress process %d: %v", id, pid, err)
	}
}

// restoreTurnLedger rebuilds a lease's turn ledger from its hand-off,
// nil for a lease with none (handoff.go's turnsForHandoff only writes
// one when newTurnLedger would have made one).
func restoreTurnLedger(ht *handoffTurns) *turnLedger {
	if ht == nil {
		return nil
	}
	l := &turnLedger{seen: ht.Seen, given: map[int64]bool{}, pending: map[int64]bool{}, closed: map[int64]bool{}}
	for _, seq := range ht.Given {
		l.given[seq] = true
	}
	for _, seq := range ht.Closed {
		l.closed[seq] = true
	}
	return l
}

// adoptedNodeTools are the node tools a node serves again for a lease it
// adopts, those of the kind's that need nothing but the hand-off, the
// lease's new session and its relay sidecar: result, hand_back and
// progress, through which a session ends its lease or says how far it
// got, and push, which every sidecar lease's session makes through the
// sidecar anyway (relaysidecar.PushClient). The rest stay unserved after
// a restart, each needing what only the process that started the lease
// held: run_suite and stop_suite its pod session, apply_onto its own git
// grant, request_time its time budget, escalate its harness, which read
// the mailbox the escalation is left in, and sign_release its Fulcio
// signer's session. A session calling one of those after a restart is
// told the node does not have it, where before every node tool was
// answered 404.
var adoptedNodeTools = []string{nodetools.ResultTool, nodetools.HandBackTool, nodetools.ProgressTool, nodetools.PushTool}

// adoptedTools are the adopted lease's node tools (adoptedNodeTools of
// the kind's), the kind's tool slots applied as at a fresh start
// (slots.go), and the recorder their evidence goes to.
func (r *Runtime) adoptedTools(ctx context.Context, b *meshapi.Brief, workspace string, tokens *sessionTokens, mb *Mailbox, fin *finishCapture, hb *sessionHandBack, sidecarDir string) ([]mcptool.Tool, *nodetools.Recorder) {
	served := *b
	served.NodeTools = nil
	var unserved []string
	for _, name := range b.NodeTools {
		if slices.Contains(adoptedNodeTools, name) {
			served.NodeTools = append(served.NodeTools, name)
		} else {
			unserved = append(unserved, name)
		}
	}
	if len(unserved) > 0 {
		r.logf("nodev1: lease %s: adopted without node tool(s) %s, which only the node that started it could serve", b.LeaseID, strings.Join(unserved, ", "))
	}
	pusher := sidecarPusher(filepath.Join(sidecarDir, relayControlDir, relayControlSocket))
	tools, rec := r.nodeTools(&served, nil, workspace, tokens, escalate.New(b.Harness.Model), mb, nil, pusher, leaseClock{ctx: ctx, began: time.Now(), budget: r.suiteBudget(&served, workspace)}, nil, nil)
	tools = append(tools, r.endTools(&served, fin, hb, mb)...)
	if sl, err := slotsOf(b); err == nil {
		tools = sl.nodeTools(tools)
	}
	return tools, rec
}
