package nodev1

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/actor"
	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/egress"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/relaysidecar"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// sidecarRig is a pod rig whose node runs each lease's relay as a
// sidecar (`--relay-sidecar`): the fake runtime's install of the
// sidecar's credentials starts the real relay sidecar in process, on the
// lease's socket directory and the pod's loopback port, as the sidecar
// container would. installed are the credentials it was handed.
func sidecarRig(t *testing.T) (*rig, *sandbox.Fake, *[][]byte) {
	t.Helper()
	g, sb, _ := podRig(t)
	g.rt.RelaySidecar = true
	g.rt.MeshBinary = testMeshBinary(t)
	installed := &[][]byte{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	started := false
	t.Cleanup(func() {
		cancel()
		if started {
			<-done
		}
	})
	sb.RelayInstalled = func(pod sandbox.Pod, creds []byte) error {
		*installed = append(*installed, creds)
		spec := sb.Relays[len(sb.Relays)-1]
		// the container's paths, as the host has them
		var c map[string]any
		if err := json.Unmarshal(creds, &c); err != nil {
			return err
		}
		if gm, ok := c["git"].(map[string]any); ok {
			gm["checkout"] = spec.Checkout
		}
		raw, _ := json.Marshal(c)
		dir := filepath.Join(t.TempDir(), "relay")
		if err := os.Mkdir(dir, 0o700); err != nil {
			return err
		}
		if err := relaysidecar.Install(bytes.NewReader(raw), dir); err != nil {
			return err
		}
		ready := make(chan struct{})
		started = true
		go func() {
			defer close(done)
			err := relaysidecar.Run(ctx, relaysidecar.Options{
				CredentialsDir: dir,
				NodeSocket:     filepath.Join(spec.SocketDir, relayNodeSocket),
				ControlSocket:  filepath.Join(spec.SocketDir, relayControlDir, relayControlSocket),
				Listener:       takeGitBridgeListener(t),
				Logf:           t.Logf,
				Ready:          func(string) { close(ready) },
				RootCAs:        g.sidecarRoots,
			})
			if err != nil {
				t.Logf("the relay sidecar: %v", err)
			}
		}()
		select {
		case <-ready:
		case <-time.After(10 * time.Second):
			t.Error("the relay sidecar did not start")
		}
		return nil
	}
	return g, sb, installed
}

// sidecarEgressDial redirects every egress dial to addr instead of the real
// host: the in-process Runtime hook a podRig test uses, and — since a
// sidecar lease's egress runs as a process of its own (ADR-051) and cannot
// reach the test's own closures across the process boundary — the same
// redirect told to that subprocess over its environment.
func sidecarEgressDial(t *testing.T, g *rig, addr string) {
	t.Helper()
	g.rt.EgressDial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	t.Setenv(TestEgressDialEnv, addr)
}

// A lease whose relay is a sidecar: its pod has no network at all and
// shares only its network namespace; the sidecar runs on the node's own
// binary with the lease's socket directory and its checkout (read-only)
// and is handed only lease-scoped credentials, through its stdin; the
// agent has no socket and no credential, only the sidecar's loopback as
// its proxy; through it a host the kind names is reached by the node's
// tunnel and one it does not is refused; and the lease's end removes the
// socket directory and revokes every git token minted for the sidecar.
func TestALeaseWhoseRelayIsASidecar(t *testing.T) {
	g, sb, installed := sidecarRig(t)
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "reached "+r.Host)
	}))
	defer host.Close()
	sidecarEgressDial(t, g, host.Listener.Addr().String())
	dir := t.TempDir()
	probe, answers := filepath.Join(dir, "probe.js"), filepath.Join(dir, "answers")
	os.WriteFile(probe, []byte(egressProbe), 0o600)
	out := filepath.Join(dir, "claude")
	envDump := filepath.Join(dir, "env")
	sb.Agent = hostAgent(t, out, `env > "`+envDump+`"; node "`+probe+`" "`+answers+`"`)
	job := g.offer(t, func(b *meshapi.Brief) {
		b.FinishTool, b.FinishSchema = "", nil
		b.Egress = &meshapi.Egress{Hosts: []string{"proxy.golang.org"}, MCP: []string{"conform"}, Model: true}
	})
	run(t, g, context.Background())
	if n := len(g.fake.Calls("finish")); n != 1 {
		t.Fatalf("%d finishes; hand-backs %+v", n, g.fake.Calls("hand-back"))
	}
	lease := g.fake.LeaseOf(job)

	specs := sb.PodSpecs()
	if len(specs) != 1 || !specs[0].Network.None {
		t.Fatalf("the pod is not isolated: %+v", specs)
	}
	if len(sb.Relays) != 1 {
		t.Fatalf("relay sidecars %+v", sb.Relays)
	}
	rel := sb.Relays[0]
	if filepath.Base(rel.SocketDir) != g.rt.leaseSocketLeaf(lease) || rel.Checkout == "" {
		t.Fatalf("the sidecar mounts %+v", rel)
	}
	if _, err := os.Stat(rel.Binary); err != nil {
		t.Fatalf("the sidecar's binary %s: %v", rel.Binary, err)
	}
	if !slices.Contains(rel.Args, "127.0.0.1:"+strconv.Itoa(g.rt.gitBridgePort)) {
		t.Fatalf("the sidecar does not listen on the pod's loopback port: %q", rel.Args)
	}

	// what the sidecar was handed: the lease's, and nothing of the node's
	if len(*installed) != 1 {
		t.Fatalf("%d installs", len(*installed))
	}
	creds := string((*installed)[0])
	var c relaysidecar.Credentials
	if err := json.Unmarshal((*installed)[0], &c); err != nil {
		t.Fatal(err)
	}
	if c.Lease != lease || c.Session.Value == "" || c.Git == nil || c.Git.Token.Value == "" || c.Git.Policy.Repo != "acme/fixture" {
		t.Fatalf("the sidecar's credentials lack the lease's own: lease %q session %v git %+v", c.Lease, c.Session.Value != "", c.Git)
	}
	machine, _ := g.rt.API.Token(context.Background())
	for _, never := range []string{"lksr_", machine} {
		if never != "" && strings.Contains(creds, never) {
			t.Fatalf("the sidecar was handed %q", never)
		}
	}

	// the agent: no socket, no credential, the sidecar its only exit
	a := sb.AgentSpecs()[0]
	for _, m := range a.Mounts {
		if filepath.Base(m.Source) == g.rt.leaseSocketLeaf(lease) {
			t.Fatalf("the lease's socket directory is mounted into the agent: %+v", m)
		}
	}
	base := "http://127.0.0.1:" + strconv.Itoa(g.rt.gitBridgePort)
	if envValue(a.Env, "HTTPS_PROXY") != base || envValue(a.Env, RelayURLEnv) != base || envValue(a.Env, RelaySocketEnv) != "" {
		t.Fatalf("the agent's relay env: proxy %q url %q socket %q", envValue(a.Env, "HTTPS_PROXY"), envValue(a.Env, RelayURLEnv), envValue(a.Env, RelaySocketEnv))
	}
	if a.Command[0] == "node" && strings.HasSuffix(a.Command[1], gitBridgeName) {
		t.Fatalf("the agent runs under the git bridge: %q", a.Command)
	}
	env, _ := os.ReadFile(envDump)
	for _, secret := range []string{c.Session.Value, c.Git.Token.Value, c.Key.D} {
		if strings.Contains(string(env), secret) || strings.Contains(strings.Join(a.Env, "\n"), secret) {
			t.Fatal("a credential of the sidecar's is in the agent's environment")
		}
	}

	got, _ := os.ReadFile(answers)
	if want := "registry.npmjs.org 403\nproxy.golang.org reached\n"; string(got) != want {
		t.Fatalf("through the sidecar the pod got %q, want %q", got, want)
	}

	// the lease's end
	if _, err := os.Stat(rel.SocketDir); !os.IsNotExist(err) {
		t.Fatalf("the socket directory outlived the lease: %v", err)
	}
	g.latchkey.mu.Lock()
	revoked := slices.Clone(g.latchkey.revoked)
	g.latchkey.mu.Unlock()
	if !slices.Contains(revoked, c.Git.Token.Value) {
		t.Fatalf("the sidecar's git token was not revoked: %v", revoked)
	}
	assertReleased(t, g, job)
}

// With a relay sidecar the session's model calls go to the sidecar's
// loopback, which forwards them to the node's model forwarder: the
// account's key is added on the node and never reaches the pod or the
// sidecar's credentials.
func TestASidecarSessionReachesItsModelThroughTheNode(t *testing.T) {
	clearModelEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", nodeOwnSecret)
	g, sb, installed := sidecarRig(t)
	model := newFakeModel(t, func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer "+fakeModelSecret })
	g.rt.ModelUpstreams = map[string]string{"anthropic": model.srv.URL}
	out := filepath.Join(t.TempDir(), "claude")
	sb.Agent = hostAgent(t, out, "")
	g.offer(t, func(b *meshapi.Brief) { b.FinishTool, b.FinishSchema = "", nil })
	run(t, g, context.Background())
	if n := len(g.fake.Calls("finish")); n != 1 {
		t.Fatalf("%d finishes; hand-backs %+v", n, g.fake.Calls("hand-back"))
	}
	a := sb.AgentSpecs()[0]
	want := "http://127.0.0.1:" + strconv.Itoa(g.rt.gitBridgePort) + "/model/anthropic"
	if envValue(a.Env, "ANTHROPIC_BASE_URL") != want {
		t.Fatalf("the pod's model base %q, want %q", envValue(a.Env, "ANTHROPIC_BASE_URL"), want)
	}
	for _, kv := range a.Env {
		if strings.Contains(kv, fakeModelSecret) || strings.Contains(kv, nodeOwnSecret) {
			t.Fatalf("the pod's environment holds a model key: %s", strings.SplitN(kv, "=", 2)[0])
		}
	}
	for _, c := range *installed {
		if bytes.Contains(c, []byte(fakeModelSecret)) || bytes.Contains(c, []byte(nodeOwnSecret)) {
			t.Fatal("the relay sidecar was handed a model key")
		}
	}
	answer, _ := os.ReadFile(out + ".model")
	if !strings.HasPrefix(string(answer), "200 ") {
		t.Fatalf("the session's model call answered %q", answer)
	}
	seen, dummy := model.calls()
	if len(seen) != 1 || seen[0] != "Bearer "+fakeModelSecret+"|" || dummy {
		t.Fatalf("the model saw %d call(s), the dummy %v", len(seen), dummy)
	}
}

// With a relay sidecar the session's Stop hook posts to the sidecar's
// loopback, which forwards it to the node's gate: while the finish is
// owed, it is refused.
func TestASidecarSessionsStopHookReachesTheGate(t *testing.T) {
	g, sb, _ := sidecarRig(t)
	out := filepath.Join(t.TempDir(), "claude")
	agent := hostAgent(t, out, "")
	nodeBin, _ := exec.LookPath("node")
	var command, answer string
	sb.Agent = func(ctx context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		i := slices.Index(spec.Command, "--settings")
		var settings struct {
			Hooks struct {
				Stop []struct {
					Hooks []struct{ Command string } `json:"hooks"`
				} `json:"Stop"`
			} `json:"hooks"`
		}
		if i >= 0 {
			json.Unmarshal([]byte(spec.Command[i+1]), &settings)
		}
		if len(settings.Hooks.Stop) == 1 && len(settings.Hooks.Stop[0].Hooks) == 1 {
			command = settings.Hooks.Stop[0].Hooks[0].Command
			hook := exec.CommandContext(ctx, "sh", "-c", command)
			hook.Env = append(slices.Clone(spec.Env), "PATH="+filepath.Dir(nodeBin)+":"+sandbox.DefaultPath)
			hook.Stdin = strings.NewReader(`{"hook_event_name":"Stop"}`)
			b, _ := hook.Output()
			answer = string(b)
		}
		return agent(ctx, spec)
	}
	g.offer(t, nil)
	run(t, g, context.Background())
	base := "http://127.0.0.1:" + strconv.Itoa(g.rt.gitBridgePort)
	if !strings.Contains(command, stopHookName) || !strings.Contains(command, base) {
		t.Fatalf("the pod's Stop hook is %q", command)
	}
	if !strings.Contains(answer, `"decision":"block"`) {
		t.Fatalf("the Stop hook through the sidecar answered %q, want a refusal", answer)
	}
}

// A sidecar lease's filled tool slots (ADR 001 amendment, slot bindings)
// go into the sidecar's credentials as names, descriptions and binding
// refs with Latchkey's origin, never a connection's credential; an
// unfilled optional slot is not handed over.
func TestASidecarIsHandedItsFilledSlots(t *testing.T) {
	g, sb, installed := sidecarRig(t)
	sb.Agent = hostAgent(t, filepath.Join(t.TempDir(), "claude"), "")
	g.offer(t, func(b *meshapi.Brief) {
		b.FinishTool, b.FinishSchema = "", nil
		b.Tools = &meshapi.BriefTools{Optional: []meshapi.SlotTool{
			{Name: "web_search", From: meshapi.ToolFromConnection, Description: "Search the web.", Binding: "bind-1"},
			{Name: "library_docs", From: meshapi.ToolFromConnection, Description: "Read docs."},
		}}
	})
	run(t, g, context.Background())
	if len(*installed) == 0 {
		t.Fatal("the sidecar was handed nothing")
	}
	var c relaysidecar.Credentials
	if err := json.Unmarshal((*installed)[0], &c); err != nil {
		t.Fatal(err)
	}
	want := []relaysidecar.Slot{{Name: "web_search", Description: "Search the web.", Binding: "bind-1"}}
	if !slices.Equal(c.Slots, want) || c.Latchkey == "" {
		t.Fatalf("the sidecar's slots %+v (latchkey %q), want %+v", c.Slots, c.Latchkey, want)
	}
}

// A relay sidecar's lanes reach the lease's egress on a socket of their
// own (lanes.go): CONNECT alone, on the lease's tunnel and its policy,
// never the node side's other routes (renewal, node tools, the model),
// and gone with the lease.
func TestTheLanesTunnelServesTheLeasesEgressAlone(t *testing.T) {
	dir, err := os.MkdirTemp("", "lt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	tunnel := &egress.Tunnel{Policy: egress.NewPolicy([]string{"proxy.golang.org"}, nil, nil),
		Dial: func(_ context.Context, _, _ string) (net.Conn, error) {
			c, s := net.Pipe()
			go func() { io.Copy(io.Discard, s); s.Close() }()
			return c, nil
		}}
	rs := &relaySidecar{r: &Runtime{Logf: t.Logf}, b: &meshapi.Brief{LeaseID: "lease-1"}, dir: dir, tunnel: tunnel}
	sock, err := rs.openLaneTunnel()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(sock) != filepath.Join(dir, relayLaneDir) {
		t.Fatalf("the lanes' socket %s is not in a directory of its own", sock)
	}
	ask := func(req string) string {
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		io.WriteString(c, req)
		line := make([]byte, 64)
		n, _ := c.Read(line)
		return string(line[:n])
	}
	for _, req := range []string{"POST " + relaysidecar.RenewPath + " HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\n\r\n",
		"GET /mcp/mesh-node HTTP/1.1\r\nHost: x\r\n\r\n"} {
		if got := ask(req); !strings.Contains(got, " 403 ") {
			t.Fatalf("%q answered %q, want 403", req, got)
		}
	}
	if got := ask("CONNECT evil.example:443 HTTP/1.1\r\nHost: evil.example:443\r\n\r\n"); strings.Contains(got, " 200 ") {
		t.Fatalf("a host the lease's policy refuses was tunnelled: %q", got)
	}
	if got := ask("CONNECT proxy.golang.org:443 HTTP/1.1\r\nHost: proxy.golang.org:443\r\n\r\n"); !strings.Contains(got, " 200 ") {
		t.Fatalf("the lease's allowed host answered %q, want 200", got)
	}
	rs.close()
	if _, err := net.Dial("unix", sock); err == nil {
		t.Fatal("the lanes' tunnel outlived the lease")
	}
}

// A sidecar lease's egress is a process of its own (ADR-051 decisions 1
// and 3): the lanes' socket routes through the real mesh lease-egress
// subprocess startEgressProcess starts, never a manually-built
// in-process tunnel, and that subprocess is never a child of this
// process's own process group, so a kill of this process (standing in
// for the node) would leave it, and anything it carries, running.
func TestTheLanesTunnelServesTheRealEgressProcess(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("no /proc")
	}
	dir, err := os.MkdirTemp("", "lt2")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "reached")
	}))
	t.Cleanup(upstream.Close)
	t.Setenv(TestEgressDialEnv, upstream.Listener.Addr().String())

	rs := &relaySidecar{r: &Runtime{Logf: t.Logf, MeshBinary: testMeshBinary(t)},
		b:    &meshapi.Brief{LeaseID: "lease-2", Egress: &meshapi.Egress{Hosts: []string{"proxy.golang.org"}}},
		dir:  dir,
		node: &relaysidecar.Node{}}
	key, err := dpop.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	handoff, err := key.Handoff()
	if err != nil {
		t.Fatal(err)
	}
	creds := relaysidecar.Credentials{Key: handoff, Session: relaysidecar.Token{Value: "lease-2-token"}}
	policy := egress.NewPolicy([]string{"proxy.golang.org"}, nil, nil)
	noop := func(egress.Call) {}
	if err := rs.startEgressProcess(context.Background(), policy, rs.b, creds, noop, noop); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rs.close() })

	if rs.egressCmd == nil || rs.egressCmd.Process == nil {
		t.Fatal("no egress process started")
	}
	childPgid, err := syscall.Getpgid(rs.egressCmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	ownPgid, err := syscall.Getpgid(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if childPgid == ownPgid {
		t.Fatalf("the egress process (pid %d, group %d) is in this process's own group %d", rs.egressCmd.Process.Pid, childPgid, ownPgid)
	}

	sock, err := rs.openLaneTunnel()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(sock) != filepath.Join(dir, relayLaneDir) {
		t.Fatalf("the lanes' socket %s is not in a directory of its own", sock)
	}
	ask := func(req string) string {
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		io.WriteString(c, req)
		line := make([]byte, 64)
		n, _ := c.Read(line)
		return string(line[:n])
	}
	for _, req := range []string{"POST " + relaysidecar.RenewPath + " HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\n\r\n",
		"GET /mcp/mesh-node HTTP/1.1\r\nHost: x\r\n\r\n"} {
		if got := ask(req); !strings.Contains(got, " 403 ") {
			t.Fatalf("%q answered %q, want 403", req, got)
		}
	}
	if got := ask("CONNECT evil.example:443 HTTP/1.1\r\nHost: evil.example:443\r\n\r\n"); strings.Contains(got, " 200 ") {
		t.Fatalf("a host the lease's policy refuses was tunnelled: %q", got)
	}

	// an actual HTTPS transfer through the lane socket, spliced by the
	// real subprocess (never terminated there) all the way to the
	// upstream it dialled: a real TLS handshake naming the lease's
	// allowed host, then a request and response over it.
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, "CONNECT proxy.golang.org:443 HTTP/1.1\r\nHost: proxy.golang.org:443\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("the lease's allowed host: %v, status %+v", err, resp)
	}
	tlsConn := tls.Client(&bufferedConn{c, br}, &tls.Config{ServerName: "proxy.golang.org", InsecureSkipVerify: true})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("the TLS handshake through the lane socket: %v", err)
	}
	io.WriteString(tlsConn, "GET / HTTP/1.1\r\nHost: proxy.golang.org\r\nConnection: close\r\n\r\n")
	body, err := io.ReadAll(tlsConn)
	if err != nil || !strings.Contains(string(body), "reached") {
		t.Fatalf("the HTTPS transfer through the real egress process got %q, %v", body, err)
	}

	rs.close()
	if _, err := net.Dial("unix", sock); err == nil {
		t.Fatal("the lanes' tunnel outlived the lease")
	}
}

// A lease's egress process killed from outside — its own process group,
// never the actor's own cause (ADR-051 decision 3) — ends the lease
// egress_died, not pod_killed and not stalled: the actor still reaches
// releasing and then done, and nothing it held outlives it.
func TestTheLeasesEgressProcessKilledEndsTheLeaseEgressDied(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("no /proc")
	}
	g, sb, installed := sidecarRig(t)
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "reached "+r.Host)
	}))
	defer host.Close()
	sidecarEgressDial(t, g, host.Listener.Addr().String())
	job := g.offer(t, func(b *meshapi.Brief) {
		b.FinishTool, b.FinishSchema = "", nil
		b.Egress = &meshapi.Egress{Hosts: []string{"proxy.golang.org"}}
	})
	// no sb.Agent: the agent container stands until stopped, holding the
	// lease in actor.Running long enough to kill its egress process.
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(context.Background()) }()
	waitFor(t, "the lease to be taken", func() bool { return g.fake.LeaseOf(job) != "" })
	lease := g.fake.LeaseOf(job)
	actorFile := filepath.Join(g.rt.leaseDir(lease), actor.FileName)
	var rec actor.Record
	waitFor(t, "the lease's egress process", func() bool {
		b, err := os.ReadFile(actorFile)
		if err != nil {
			return false
		}
		var r actor.Record
		if json.Unmarshal(b, &r) != nil || r.Egress == nil || r.Egress.PID == 0 {
			return false
		}
		rec = r
		return true
	})
	if len(*installed) == 0 {
		t.Fatal("the sidecar was never installed")
	}
	if err := syscall.Kill(rec.Egress.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("killing the egress process %d: %v", rec.Egress.PID, err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the lease outlived its killed egress process")
	}
	hb := onlyHandBack(t, g, job)
	if hb.Cause != meshapi.CauseEgressDied {
		t.Fatalf("hand-back %+v, want cause %q", hb, meshapi.CauseEgressDied)
	}
	if hb.Cause == meshapi.CausePodKilled || hb.Cause == meshapi.CauseStalled {
		t.Fatalf("hand-back wrongly blamed the pod or a stall: %+v", hb)
	}
	assertReleased(t, g, job)
	_ = sb
}

// A node that stops without releasing (ADR-051 decision 5) leaves its
// pod, its sidecar and its lease's egress process running untouched: a
// registry transfer already in flight through the lane socket, genuinely
// terminated and signed by the real egress process, completes in full
// with nothing left to drive the Runtime any further — "killing" it is
// faithfully only ever abandoning its driving goroutine, never cancelling
// its context, since Run's own deferred leases.Wait() means it cannot
// return while a lease is cleanly releasing. Only the next Runtime —
// sharing no state with the first beyond the same on-disk leases and the
// same (simulated) podman — finds the orphaned actor.json and, through
// releaseLeftovers, stops the egress process and hands the lease back
// (acceptance check 1 of the goal).
func TestARuntimeKilledMidTransferLeavesPodSidecarAndEgressRunningUntilTheNextRuntimeReleasesIt(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("no /proc")
	}
	g, sb, installed := sidecarRig(t)
	reached := make(chan struct{})
	release := make(chan struct{})
	heldRegistryHostRig(t, g, reached, release)
	var laneDir string
	wrap := sb.RelayInstalled
	sb.RelayInstalled = func(pod sandbox.Pod, creds []byte) error {
		laneDir = sb.Relays[len(sb.Relays)-1].SocketDir
		return wrap(pod, creds)
	}
	job := g.offer(t, registryBrief)
	// no sb.Agent: the agent container stands until stopped, holding the
	// lease in actor.Running indefinitely — there is no node left to stop
	// it this test.
	go func() { _ = g.rt.Run(context.Background()) }()
	waitFor(t, "the lease to be taken", func() bool { return g.fake.LeaseOf(job) != "" })
	lease := g.fake.LeaseOf(job)
	actorFile := filepath.Join(g.rt.leaseDir(lease), actor.FileName)
	var rec actor.Record
	waitFor(t, "the lease's egress process", func() bool {
		b, err := os.ReadFile(actorFile)
		if err != nil {
			return false
		}
		var r actor.Record
		if json.Unmarshal(b, &r) != nil || r.Egress == nil || r.Egress.PID == 0 {
			return false
		}
		rec = r
		return true
	})
	if len(*installed) == 0 {
		t.Fatal("the sidecar was never installed")
	}
	waitFor(t, "the lane socket", func() bool { return laneDir != "" })
	sock := filepath.Join(laneDir, relayLaneDir, relayLaneSocket)
	bodyCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		body, err := laneTransfer(sock, "api.runsheet.dev", "/npm/@anythink%2fshell")
		bodyCh <- body
		errCh <- err
	}()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the registry was never reached")
	}

	// The transfer is genuinely in flight: the request already reached
	// the real egress process, was signed and forwarded, and the upstream
	// is holding its answer open. This is the moment the node's Runtime
	// is "killed" — nothing below touches g.rt again.
	if err := syscall.Kill(rec.Egress.PID, 0); err != nil {
		t.Fatalf("the egress process %d is not running before the kill: %v", rec.Egress.PID, err)
	}
	if len(sb.Removed) != 0 {
		t.Fatalf("the pod was removed before the kill: %v", sb.Removed)
	}
	close(release)

	var body string
	select {
	case body = <-bodyCh:
	case <-time.After(10 * time.Second):
		t.Fatal("the transfer outlived the kill")
	}
	if err := <-errCh; err != nil {
		t.Fatalf("the HTTPS transfer through the lane socket: %v", err)
	}
	if !strings.HasSuffix(body, "signed") {
		t.Fatalf("the lane socket's terminated transfer got %q, want it to end %q", body, "signed")
	}

	// Still nothing has driven the Runtime to release anything: the pod,
	// the sidecar (proven by the transfer it just carried) and the egress
	// process are all still running, and the platform has heard nothing.
	if err := syscall.Kill(rec.Egress.PID, 0); err != nil {
		t.Fatalf("the egress process %d died on its own: %v", rec.Egress.PID, err)
	}
	if len(sb.Removed) != 0 {
		t.Fatalf("the pod was removed with no node left to remove it: %v", sb.Removed)
	}
	if n := len(g.fake.Calls("hand-back")) + len(g.fake.Calls("finish")) + len(g.fake.Calls("fail")); n != 0 {
		t.Fatalf("the lease ended with no node left to end it: %d call(s)", n)
	}

	// The next Runtime: a node restart, sharing nothing with the first but
	// the same API, the same work root and the same (simulated) podman.
	next := &Runtime{API: g.rt.API, WorkRoot: g.rt.WorkRoot, Sandbox: sb, Logf: t.Logf}
	next.releaseLeftovers(nil)

	if _, err := os.Stat(g.rt.leaseDir(lease)); !os.IsNotExist(err) {
		t.Fatalf("the lease directory outlived the next Runtime's release: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(rec.Egress.PID, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("the egress process %d outlived the next Runtime's release", rec.Egress.PID)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// releaseLeftovers hands the lease back before it ever stops the
	// egress process (actorlease.go), so this call is recorded before
	// the kill signal is even sent, whatever the first Runtime's own
	// abandoned goroutine — still alive in this one test process, never
	// truly killed the way a real node's would be — goes on to also
	// attempt once it notices the same death.
	calls := g.fake.Calls("hand-back")
	if len(calls) == 0 {
		t.Fatalf("the next Runtime's releaseLeftovers handed nothing back")
	}
	var hb meshapi.HandBackRequest
	json.Unmarshal(calls[0].Body, &hb)
	if hb.Cause != meshapi.CauseNodeRestart {
		t.Fatalf("hand-back cause %q, want %q", hb.Cause, meshapi.CauseNodeRestart)
	}
}

// bufferedConn is a net.Conn whose reads come from a *bufio.Reader
// already primed over it (by reading the CONNECT response), so no
// byte the reader buffered past that response is lost to a later TLS
// handshake over the same connection.
type bufferedConn struct {
	net.Conn
	br *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.br.Read(p) }
