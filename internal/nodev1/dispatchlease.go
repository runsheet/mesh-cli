package nodev1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/sessionproxy"
)

// A dispatcher lease's sub-agents (dispatcher.go). The lease's work
// opens what any lease opens — its own session for the model account,
// its pod and relay — but runs no session of its own: serveDispatcher
// runs in its place, and each ask's sub-agent is a one-shot session
// beside the others in the lease's pod, in a container of its own
// (sandbox.Session.Exec), with:
//
//   - its own Latchkey session (POST /v1/leases/{id}/asks/{ask}/session)
//     on a DPoP key of its own, which the product scopes to that ask
//     alone;
//   - its own sessionproxy, serving the brief's product servers on that
//     session's token and nothing else — never the dispatcher's own
//     session, no node tools, no git — on a socket directory mounted
//     into its container alone;
//   - its own working directory, <workspace>/asks/<ask>, removed at its
//     end;
//   - the dispatcher's model account, through the lease's model relay: a
//     model account is no project's access.
//
// Its first message is the kind's guidance, the same bytes for every
// ask, then the ask's own line, so the model's prompt cache serves the
// shared part.

// newLeaseAsks is the ask queue of a dispatcher's lease, nil for any
// other lease.
func (r *Runtime) newLeaseAsks(b *meshapi.Brief) *askQueue {
	if b == nil || b.Dispatcher == nil {
		return nil
	}
	return newAskQueue(dispatcherCap(b.Dispatcher.Cap, r.DispatcherCap), b.Dispatcher.AckedThrough, nil)
}

// isDispatcher is whether b is a dispatcher job's.
func isDispatcher(b *meshapi.Brief) bool { return b != nil && b.Dispatcher != nil }

// serveDispatcher runs the lease's asks in place of its session, with
// tmpl the session the lease would have run — its model, account
// environment and pod — until the lease ends or parks.
func (r *Runtime) serveDispatcher(ctx context.Context, b *meshapi.Brief, tmpl SessionSpec, lp *leasePod) (SessionResult, error) {
	q := r.leaseAsks(b.LeaseID)
	if q == nil {
		return SessionResult{}, errors.New("the dispatcher's lease holds no ask queue")
	}
	if lp != nil && !r.sidecarMode() {
		// beside each other in one pod, sub-agents reach the model only
		// through the relay sidecar on its loopback: the per-agent bridge
		// binds one port, which a second sub-agent could not
		return SessionResult{}, &toolUnavailableError{tool: "dispatcher", why: "a dispatcher's sub-agents in a pod need the relay sidecar (--relay-sidecar)"}
	}
	d := &dispatcher{lease: b.LeaseID, q: q, logf: r.logf,
		run: func(ctx context.Context, t meshapi.Turn) (askEnd, error) { return r.runAsk(ctx, b, tmpl, lp, t) },
		finish: func(ctx context.Context, t meshapi.Turn, e askEnd) error {
			_, err := r.API.FinishAsk(ctx, b.LeaseID, t.TurnID, meshapi.AskFinishRequest{End: e.end, Result: e.result})
			return err
		},
	}
	if b.Thread != nil {
		d.parkAfter = time.Duration(b.Thread.ParkAfterS) * time.Second
	}
	r.logf("nodev1: dispatcher %s of %s (%s): %d sub-agents at once, from turn %d", b.LeaseID, b.Tenant, b.Kind, q.cap, b.Dispatcher.AckedThrough)
	// the asks that came before the lease: its first renewal, now, not
	// a renew period from now, brings them (a renewal that fails leaves
	// them to the next)
	if _, err := r.renewLease(ctx, b.LeaseID); err != nil && ctx.Err() == nil {
		r.logf("nodev1: dispatcher %s: its first renewal: %v", b.LeaseID, err)
	}
	began := time.Now()
	parked, err := d.serve(ctx)
	if parked {
		r.logf("nodev1: dispatcher %s had no ask for %s: it parks", b.LeaseID, d.parkAfter)
	}
	return SessionResult{Parked: parked, Duration: time.Since(began)}, err
}

// runAsk answers t through a sub-agent of its own. A lease that ends
// under it is errAskNotRun; an ask whose own session or proxy could not
// be opened is handed back, for the product to answer another way.
func (r *Runtime) runAsk(ctx context.Context, b *meshapi.Brief, tmpl SessionSpec, lp *leasePod, t meshapi.Turn) (askEnd, error) {
	tokens, err := r.openAskSession(ctx, b.LeaseID, t.TurnID)
	if err != nil {
		if ctx.Err() != nil {
			return askEnd{}, errAskNotRun
		}
		return endedWith(meshapi.AskEndHandedBack, "", "the ask's session: "+err.Error()), nil
	}
	servers, sockDir, closeProxy, err := r.openAskProxy(ctx, b, tokens, t.TurnID)
	if err != nil {
		if ctx.Err() != nil {
			return askEnd{}, errAskNotRun
		}
		return endedWith(meshapi.AskEndHandedBack, "", "the ask's sessionproxy: "+err.Error()), nil
	}
	defer closeProxy()
	workspace := filepath.Join(tmpl.Workspace, "asks", t.TurnID)
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return endedWith(meshapi.AskEndHandedBack, "", "the ask's directory: "+err.Error()), nil
	}
	defer os.RemoveAll(workspace)

	spec := tmpl
	spec.Label = b.LeaseID + "/ask/" + t.TurnID
	spec.Workspace = workspace
	spec.Prompt = askPrompt(b, t)
	spec.Servers = servers
	spec.Mailbox, spec.Thread, spec.ParkAfter, spec.Resume = nil, false, 0, ""
	spec.Settings, spec.OnLine, spec.Ladder = nil, nil, nil
	// each sub-agent's output is the lease's liveness alone: the lease's
	// stop rules and transcript read one session's stream, never several
	// interleaved
	lease := b.LeaseID
	spec.OnEvent = func(harness.Event) { r.touch(lease) }
	if lp != nil {
		if spec.Servers, err = lp.askServers(servers); err != nil {
			return endedWith(meshapi.AskEndHandedBack, "", err.Error()), nil
		}
		spec.InPod = lp.askRun(workspace, sockDir)
	}
	sess, err := r.sessionFor(b)
	if err != nil {
		return endedWith(meshapi.AskEndHandedBack, "", err.Error()), nil
	}
	res, err := sess.Run(ctx, spec)
	if ctx.Err() != nil {
		return askEnd{}, errAskNotRun
	}
	if err != nil {
		return endedWith(meshapi.AskEndFailed, res.SessionID, clip(err.Error(), 2000)), nil
	}
	out, _ := json.Marshal(map[string]any{"session_id": res.SessionID, "turns": res.Turns, "cost_usd": res.CostUSD})
	return askEnd{end: meshapi.AskEndAnswered, result: out}, nil
}

// endedWith is an ask's end with its reason in its result.
func endedWith(end, sessionID, reason string) askEnd {
	out, _ := json.Marshal(map[string]string{"session_id": sessionID, "reason": reason})
	return askEnd{end: end, result: out}
}

// askPrompt is a sub-agent's first message: the kind's guidance — the
// stable prefix every ask of the tenant shares — then its one ask.
func askPrompt(b *meshapi.Brief, t meshapi.Turn) string {
	tool := turnToolFor(b)
	if tool == "" {
		tool = "the product's turn tool"
	}
	var s strings.Builder
	if g := strings.TrimSpace(b.Guidance); g != "" {
		s.WriteString(g)
		s.WriteString("\n\n")
	}
	fmt.Fprintf(&s, "You answer one ask of %s's assistant, alone: ask %s (turn %d). Read it with %s(%s), answer it where it was asked, and end.\n",
		b.Tenant, t.TurnID, t.Seq, tool, t.TurnID)
	return s.String()
}

// openAskSession makes the sub-agent's key and has Mesh open its ask's
// session on it.
func (r *Runtime) openAskSession(ctx context.Context, leaseID, askID string) (*sessionTokens, error) {
	key, err := dpop.NewKey()
	if err != nil {
		return nil, err
	}
	cred, err := r.API.AskSession(ctx, leaseID, askID, key.Thumbprint())
	if err != nil {
		return nil, err
	}
	hc := r.LatchkeyHTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &sessionTokens{cred: cred, key: key, http: hc}, nil
}

// openAskProxy serves the brief's product servers on the ask's own
// session, on a socket of the ask's own: the servers' MCP config, the
// socket's directory, and its close.
func (r *Runtime) openAskProxy(ctx context.Context, b *meshapi.Brief, tokens *sessionTokens, askID string) (map[string]any, string, func(), error) {
	ups := map[string]sessionproxy.Upstream{}
	for _, s := range b.Servers {
		if s.Name == "" || s.URL == "" || s.Name == NodeToolsServer || !mcpAllowed(b, s.Name) {
			continue
		}
		ups[s.Name] = sessionproxy.Product{URL: s.URL, Bearer: tokens.Token, Key: tokens.Key()}
	}
	sockDir, err := r.leaseSocketDir(b.LeaseID + "/ask/" + askID)
	if err != nil {
		return nil, "", nil, err
	}
	sock := filepath.Join(sockDir, leaseSocketName)
	p := sessionproxy.New(ups)
	p.Logf = r.Logf
	closeAll := func() { p.Close(); os.RemoveAll(sockDir) }
	if err := p.Listen(sock); err != nil {
		closeAll()
		return nil, "", nil, err
	}
	if err := waitListening(ctx, sock, socketReadyWait); err != nil {
		closeAll()
		return nil, "", nil, err
	}
	servers := map[string]any{}
	for name := range ups {
		servers[name] = harness.ProxyRelay(r.MeshBinary, sock, name)
	}
	return servers, sockDir, closeAll, nil
}

// askServers are an ask's servers as its container reaches them: each
// entry rewritten onto the pod's proxy shim (podServers), its socket's
// directory mounted into that container alone (askRun), never shared
// with the pod.
func (p *leasePod) askServers(servers map[string]any) (map[string]any, error) {
	if len(servers) == 0 {
		return servers, nil
	}
	if p.shim == "" {
		shim, err := p.sess.WriteFile(harness.ProxyShimScriptName, harness.ProxyShimScript())
		if err != nil {
			return nil, fmt.Errorf("the pod's proxy shim: %w", err)
		}
		p.shim = shim
	}
	out := make(map[string]any, len(servers))
	for name, srv := range servers {
		socket, server, ok := harness.ProxiedOf(srv)
		if !ok {
			out[name] = srv
			continue
		}
		out[name] = harness.ProxiedServer(p.shim, socket, server)
	}
	return out, nil
}

// askRun is a sub-agent's PodRun: its process in a container of its own
// in the lease's pod, beside the other sub-agents, with its own socket
// directory mounted. The model is the lease's, through the relay
// sidecar on the pod's loopback (serveDispatcher refuses a pod with no
// sidecar).
func (p *leasePod) askRun(workspace, sockDir string) PodRun {
	return func(ctx context.Context, argv, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
		var jobEnv, vars []string
		for _, kv := range env {
			name, _, _ := strings.Cut(kv, "=")
			if sandbox.Allowed(name) {
				jobEnv = append(jobEnv, kv)
			} else {
				vars = append(vars, kv)
			}
		}
		vars = append(vars, p.proxyEnv...)
		vars = append(vars, p.registryEnv...)
		vars = append(vars, RelayURLEnv+"="+p.bridgeBase())
		return p.sess.Exec(ctx, sandbox.RunSpec{Workspace: workspace, Env: jobEnv, Vars: vars, Command: argv,
			Mounts: []sandbox.Mount{{Source: sockDir, Target: sockDir}}, Stdin: stdin, Stdout: stdout, Stderr: stderr})
	}
}
