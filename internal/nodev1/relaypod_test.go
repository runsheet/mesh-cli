package nodev1

import (
	"bytes"
	"context"
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
	"testing"
	"time"

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
				Listen:         "127.0.0.1:" + strconv.Itoa(g.rt.gitBridgePort),
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
	g.rt.EgressDial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, host.Listener.Addr().String())
	}
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
