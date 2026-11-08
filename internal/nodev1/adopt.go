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
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/actor"
	"projectmesh.io/mesh-cli/internal/egress"
	"projectmesh.io/mesh-cli/internal/gitrelay"
	"projectmesh.io/mesh-cli/internal/meshapi"
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
// again, its agent output tailed from where the hand-off left it, and
// a fresh relaysidecar.Node listening on the sidecar's own node
// socket, this time with its token renewal pull (Renew, mirroring the
// sidecar's own rs.renew), its Stop gate (openStopGate, as a fresh
// start would open it, with no earlier hand-back request or evidence
// record to carry across the restart) and, for a lease whose egress
// runs as a process of its own (ADR-051 decisions 1 and 4), fresh
// credentials pushed to that process's control socket and its node
// socket's CONNECT forwarded onto the egress process's own
// (forwardConnect, relaypod.go) -- beside Observe, Security and
// Canary as before. A lease whose egress process has died since the
// hand-off, or that refuses its new credentials, is still adopted:
// it just runs with no egress tunnel from here on, logged, never
// handed back over it alone. Its sessionproxy is not reattached:
// rebuilding it means rebuilding the pod session itself, which
// adoption does not do. A hand-off that cannot be adopted this way --
// unreadable, no sidecar named, or the sidecar refuses the new
// credentials -- is handed back node_update instead, the same cause
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
	return adopting
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

	r.mu.Lock()
	r.leases.Put(id, &running{cancel: cancel, progressAt: time.Now(), mailbox: mb, turnTool: turnToolFor(b),
		jobID: b.JobID, kind: b.Kind, started: time.Now(), actor: a, brief: b,
		pushHeads: st.PushHeads, sidecarDir: st.SidecarDir})
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
	gate := r.openStopGate(b, nodeSock, fin, nil, nil)

	node := &relaysidecar.Node{Observe: fin.observe, Security: r.securityReporter(id), Canary: v.canary(), Logf: r.logf, Renew: renew}
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
			case !egRes.Replaced:
				r.logf("nodev1: lease %s: its egress process refused its new credentials: %s; adopted with no egress tunnel", id, egRes.Refused)
			default:
				node.Tunnel = forwardConnect(filepath.Join(filepath.Dir(st.EgressControl), egressNodeSocket), func(req *http.Request) bool { return egress.IsSpliceOnly(req.Context()) })
			}
		}
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
	if node.Tunnel != nil {
		r.setEgress(id, st.EgressPID, st.EgressControl)
		a.Hold("the lease's egress process", func() error { return stopLeftoverEgress(st.EgressPID) })
	}

	if n, err := r.tailAgentOutput(id, st.AgentStdoutOffset); err != nil {
		r.logf("nodev1: lease %s: resuming its agent output: %v", id, err)
	} else if len(n) > 0 {
		r.logf("nodev1: lease %s: resumed its agent output, %d byte(s) past offset %d", id, len(n), st.AgentStdoutOffset)
	}

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
	r.logf("nodev1: lease %s of job %s (%s) adopted at start", id, b.JobID, b.Kind)

	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		r.renew(leaseCtx, lease, cancel, room)
	}()

	select {
	case <-mb.Finished():
	case <-leaseCtx.Done():
	}
	cancel(nil)
	<-renewDone

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

	out := outcome{finish: fin.result()}
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
