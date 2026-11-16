package nodev1

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/relaysidecar"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/scheduler"
	"projectmesh.io/mesh-cli/internal/sockettest"
)

// A node restart, end to end, across real processes (ADR-044's
// start-up adoption; mesh-cli#1931): what dl360 saw on 16 Nov, when
// every lease was "adopted N of N" at start and yet no adopted session
// called its model again, and Mesh's stall reaper handed them back.
//
// This test binary plays three parts beside the test itself, each in a
// process of its own, as on a node:
//
//   - the node (restartNodeEnv): a nodev1.Runtime on a fake pod runtime,
//     with --sandbox --relay-sidecar --actors as dl360 runs it. It exits
//     on SIGHUP the way `mesh node upgrade` has it exit (UpgradeSignal:
//     every lease's hand-off written, nothing stopped), leaving its state
//     on disk for the next node process to adopt;
//   - the lease's agent (restartSessionEnv): a stand-in for claude in the
//     lease's pod, which keeps calling the three things a session calls
//     through its relay -- the model at ANTHROPIC_BASE_URL, an MCP server
//     (the product's, on the session's token, and the node's own tools)
//     and `git fetch` from its checkout's relay remote -- and writes its
//     stream to its stdout, as the harness does;
//   - the relay sidecar (restartSidecarEnv): relaysidecar.Run, as `mesh
//     relay-sidecar` runs it in the pod, started by the node's fake pod
//     runtime, trusting the test's TLS front.
//
// The agent, the sidecar and the lease's egress process are processes
// of their own group, as the pod's containers and the egress process
// are, so the node's exit leaves them running: KillMode=process. The
// fakes (Mesh, Latchkey, the product, the code host, the model) are
// this test's own, reached over loopback.
const (
	restartNodeEnv    = "MESH_V1_RESTART_NODE"
	restartSessionEnv = "MESH_V1_RESTART_SESSION"
	restartSidecarEnv = "MESH_V1_RESTART_SIDECAR"
	restartConfigEnv  = "MESH_V1_RESTART_CONFIG"
	// restartSidecarArgsEnv carries the sidecar's own options: its
	// credentials directory, the lease's node and control sockets, and
	// the pod's loopback address.
	restartSidecarArgsEnv = "MESH_V1_RESTART_SIDECAR_ARGS"
	// restartUpstream is the one host the lease's upstreams -- the
	// product's MCP server and the code host -- are reached at, on 443
	// (the only port a lease's egress lets out): the test's TLS front,
	// whose certificate names it, every egress dial redirected there.
	restartUpstream = "example.com"
)

// restartConfig is what the parent test hands each node process and,
// through it, the agent.
type restartConfig struct {
	MeshURL     string            `json:"mesh_url"`
	WorkRoot    string            `json:"work_root"`
	SocketRoot  string            `json:"socket_root"`
	MeshBinary  string            `json:"mesh_binary"`
	GitUpstream string            `json:"git_upstream"`
	Models      map[string]string `json:"models"`
	GitPort     int               `json:"git_port"`
	// FrontAddr and FrontCA are the test's TLS front for
	// restartUpstream: where every egress dial is redirected, and the
	// certificate the sidecar and the node's own git relay trust.
	FrontAddr string `json:"front_addr"`
	FrontCA   string `json:"front_ca"`
	// Self is this test binary, the agent's stand-in.
	Self string `json:"self"`
	// Dir holds what the agent reports (results), the file that asks
	// it to finish (stop), the processes the node started for the pod
	// (pids) and their logs.
	Dir string `json:"dir"`
}

func (c restartConfig) results() string { return filepath.Join(c.Dir, "results.jsonl") }
func (c restartConfig) stop() string    { return filepath.Join(c.Dir, "stop") }
func (c restartConfig) pids() string    { return filepath.Join(c.Dir, "pids") }

// restartRound is one round of the agent's calls through its relay.
type restartRound struct {
	Round int       `json:"round"`
	At    time.Time `json:"at"`
	// Model is the model call's HTTP status; MCP the product's tool
	// call's and Tool the node's own tool's, each with its JSON-RPC
	// error, if any; Git is `git fetch`'s exit code (0 fetched).
	Model    int    `json:"model"`
	MCP      int    `json:"mcp"`
	MCPError string `json:"mcp_error,omitempty"`
	Tool     int    `json:"tool"`
	ToolErr  string `json:"tool_error,omitempty"`
	Git      int    `json:"git"`
	GitOut   string `json:"git_out,omitempty"`
}

func (r restartRound) ok() bool {
	return r.Model == http.StatusOK && r.MCP == http.StatusOK && r.MCPError == "" &&
		r.Tool == http.StatusOK && r.ToolErr == "" && r.Git == 0
}

// maybeRunRestartRole runs this test binary as the node or the agent
// when the parent test started it as one (TestMain calls it); it never
// returns then.
func maybeRunRestartRole() {
	switch {
	case os.Getenv(restartNodeEnv) == "1":
		os.Exit(runRestartNode())
	case os.Getenv(restartSessionEnv) == "1":
		os.Exit(runRestartSession())
	case os.Getenv(restartSidecarEnv) == "1":
		os.Exit(runRestartSidecar())
	}
}

// runRestartNode is the node process: a Runtime like dl360's, its pods
// on a fake runtime that runs the relay sidecar and the agent as real
// processes of their own.
func runRestartNode() int {
	var cfg restartConfig
	if err := json.Unmarshal([]byte(os.Getenv(restartConfigEnv)), &cfg); err != nil {
		fmt.Fprintln(os.Stderr, "restart node: config:", err)
		return 2
	}
	var logMu sync.Mutex
	logf := func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}
	notePID := func(pid int) {
		f, err := os.OpenFile(cfg.pids(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintln(f, pid)
			f.Close()
		}
	}
	logFile := func(name string) *os.File {
		f, _ := os.OpenFile(filepath.Join(cfg.Dir, name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		return f
	}
	sb := &sandbox.Fake{}
	// the sidecar container: the real relay sidecar, in a process group
	// of its own, on the lease's socket directory and the pod's loopback
	// port, as `podman run` would leave it
	sb.RelayInstalled = func(pod sandbox.Pod, creds []byte) error {
		spec := sb.Relays[len(sb.Relays)-1]
		var c map[string]any
		if err := json.Unmarshal(creds, &c); err != nil {
			return err
		}
		if gm, ok := c["git"].(map[string]any); ok {
			gm["checkout"] = spec.Checkout
		}
		raw, _ := json.Marshal(c)
		dir := filepath.Join(cfg.Dir, "relay-"+pod.Name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := relaysidecar.Install(bytes.NewReader(raw), dir); err != nil {
			return err
		}
		args, err := json.Marshal(restartSidecarArgs{
			CredentialsDir: dir,
			NodeSocket:     filepath.Join(spec.SocketDir, relayNodeSocket),
			ControlSocket:  filepath.Join(spec.SocketDir, relayControlDir, relayControlSocket),
			Listen:         "127.0.0.1:" + strconv.Itoa(cfg.GitPort),
		})
		if err != nil {
			return err
		}
		cmd := exec.Command(cfg.Self, "-test.run=^$")
		cmd.Env = append(withoutEnv(os.Environ(), restartNodeEnv), restartSidecarEnv+"=1", restartSidecarArgsEnv+"="+string(args))
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Stdout, cmd.Stderr = logFile("sidecar.log"), logFile("sidecar.log")
		if err := cmd.Start(); err != nil {
			return err
		}
		notePID(cmd.Process.Pid)
		logf("restart node: the relay sidecar runs as pid %d", cmd.Process.Pid)
		go cmd.Wait()
		return nil
	}
	// the agent's container: this binary as the stand-in session, on the
	// agent's own environment, workdir and stdio files
	sb.Agent = func(ctx context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		cmd := exec.Command(cfg.Self, "-test.run=^$")
		cmd.Dir = spec.Workdir
		cmd.Env = append(append([]string(nil), spec.Env...),
			restartSessionEnv+"=1", restartConfigEnv+"="+os.Getenv(restartConfigEnv),
			"PATH="+os.Getenv("PATH"), "HOME="+cfg.Dir)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = spec.Stdin, spec.Stdout, logFile("agent.log")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			logf("restart node: starting the agent: %v", err)
			return sandbox.Exit{}, err
		}
		notePID(cmd.Process.Pid)
		logf("restart node: the agent runs as pid %d in %s", cmd.Process.Pid, spec.Workdir)
		err := cmd.Wait()
		logf("restart node: the agent exited: %v", err)
		if ee, ok := err.(*exec.ExitError); ok {
			return sandbox.Exit{Code: ee.ExitCode()}, nil
		}
		return sandbox.Exit{}, err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintln(os.Stderr, "restart node: key:", err)
		return 2
	}
	// the lease's egress process, a child of this one, dials the front
	// for every host (TestEgressDialEnv); the node's own git relay
	// reaches the code host there too
	os.Setenv(TestEgressDialEnv, cfg.FrontAddr)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(cfg.FrontCA))
	gitHTTP := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, cfg.FrontAddr)
		},
	}}
	rt := &Runtime{
		ModelUpstreams: cfg.Models,
		SigningKey:     key,
		API:            &meshapi.Client{BaseURL: cfg.MeshURL, Token: func(context.Context) (string, error) { return "machine", nil }},
		Node:           meshapi.RegisterRequest{Name: "test-node", Version: "v0.0.0-test", CapacityTotal: meshapi.Capacity{CPUs: 4, MemGB: 8, Slots: 1}, SigningKey: "ed25519:AAAA"},
		WorkRoot:       cfg.WorkRoot,
		SocketRoot:     cfg.SocketRoot,
		MeshBinary:     cfg.MeshBinary,
		Session:        &ClaudeSession{PodBinary: cfg.Self, Logf: logf},
		Logf:           logf,
		Backoff:        100 * time.Millisecond,
		GitUpstream:    cfg.GitUpstream,
		GitHTTP:        gitHTTP,
		HeartbeatEvery: 200 * time.Millisecond,
		Actors:         true,
		Sandbox:        sb,
		PodWatchEvery:  50 * time.Millisecond,
		Scheduler:      scheduler.New(scheduler.Capacity{CPUs: 4, MemGB: 8, Slots: 1}),
		Pods: &sandbox.Sandbox{Runtime: sb, Logf: logf, Config: sandbox.Config{
			Image:          "mesh-node:test",
			Limits:         sandbox.Limits{CPUs: 2, Memory: "6g", Pids: 4096},
			ServicesLimits: sandbox.Limits{CPUs: 2, Memory: "4g"},
			Registries:     sandbox.DefaultRegistries,
			ServiceTimeout: 5 * time.Second,
		}},
		RelaySidecar:  true,
		gitBridgePort: cfg.GitPort,
	}
	// SIGHUP is `mesh node upgrade`'s restart (noderun.go): every lease's
	// hand-off written and the process gone, nothing of the lease
	// stopped
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		<-hup
		rt.UpgradeSignal("the test restarts the node")
	}()
	if err := rt.Run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "restart node: run:", err)
		return 1
	}
	return 0
}

// restartSidecarArgs are the sidecar's flags, as `mesh relay-sidecar`
// takes them.
type restartSidecarArgs struct {
	CredentialsDir string `json:"credentials_dir"`
	NodeSocket     string `json:"node_socket"`
	ControlSocket  string `json:"control_socket"`
	Listen         string `json:"listen"`
}

// runRestartSidecar is the lease's relay sidecar container: what `mesh
// relay-sidecar` runs, trusting the test's front as its upstreams' CA.
func runRestartSidecar() int {
	var cfg restartConfig
	var args restartSidecarArgs
	if err := json.Unmarshal([]byte(os.Getenv(restartConfigEnv)), &cfg); err != nil {
		fmt.Fprintln(os.Stderr, "sidecar: config:", err)
		return 2
	}
	if err := json.Unmarshal([]byte(os.Getenv(restartSidecarArgsEnv)), &args); err != nil {
		fmt.Fprintln(os.Stderr, "sidecar: options:", err)
		return 2
	}
	o := relaysidecar.Options{CredentialsDir: args.CredentialsDir, NodeSocket: args.NodeSocket, ControlSocket: args.ControlSocket, Listen: args.Listen}
	o.RootCAs = x509.NewCertPool()
	o.RootCAs.AppendCertsFromPEM([]byte(cfg.FrontCA))
	o.Logf = func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	if err := relaysidecar.Run(ctx, o); err != nil {
		fmt.Fprintln(os.Stderr, "sidecar:", err)
		return 1
	}
	return 0
}

// runRestartSession is the stand-in agent: round after round, the
// model, the product's MCP tool and the node's own, and a git fetch,
// all through the lease's relay sidecar as a session in the pod reaches
// it; each round a line of its stream on stdout and one of results.
// Once the stop file appears it calls the product's finish tool and
// ends, as a session that has done its work.
func runRestartSession() int {
	signal.Ignore(syscall.SIGPIPE)
	var cfg restartConfig
	if err := json.Unmarshal([]byte(os.Getenv(restartConfigEnv)), &cfg); err != nil {
		fmt.Fprintln(os.Stderr, "stand-in: config:", err)
		return 2
	}
	relay := os.Getenv(RelayURLEnv)
	modelBase := os.Getenv("ANTHROPIC_BASE_URL")
	if relay == "" || modelBase == "" {
		fmt.Fprintf(os.Stderr, "stand-in: no relay (%q) or model base (%q) in the environment\n", relay, modelBase)
		return 2
	}
	client := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: false}}
	emit := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintln(os.Stdout, string(b))
	}
	model := func() int {
		req, _ := http.NewRequest(http.MethodPost, strings.TrimRight(modelBase, "/")+"/v1/messages",
			strings.NewReader(`{"model":"claude-haiku","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Anthropic-Version", "2023-06-01")
		for _, v := range []string{"ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"} {
			if s := os.Getenv(v); s != "" {
				req.Header.Set("Authorization", "Bearer "+s)
			}
		}
		if s := os.Getenv("ANTHROPIC_API_KEY"); s != "" {
			req.Header.Set("X-Api-Key", s)
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	mcp := func(server, tool, args string, id int) (int, string) {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, id, tool, args)
		req, _ := http.NewRequest(http.MethodPost, strings.TrimRight(relay, "/")+"/mcp/"+server, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := client.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var ans struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
			Result *struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		// a streamed answer's data line
		raw := b
		if i := bytes.Index(b, []byte("data: ")); i >= 0 {
			raw = bytes.TrimSpace(bytes.SplitN(b[i+len("data: "):], []byte("\n"), 2)[0])
		}
		if resp.StatusCode != http.StatusOK {
			return resp.StatusCode, strings.TrimSpace(string(b))
		}
		if err := json.Unmarshal(raw, &ans); err != nil {
			return resp.StatusCode, "unreadable answer: " + string(b)
		}
		switch {
		case ans.Error != nil:
			return resp.StatusCode, ans.Error.Message
		case ans.Result == nil:
			return resp.StatusCode, "no result: " + string(b)
		case ans.Result.IsError:
			text := ""
			if len(ans.Result.Content) > 0 {
				text = ans.Result.Content[0].Text
			}
			return resp.StatusCode, "tool error: " + text
		}
		return resp.StatusCode, ""
	}
	gitFetch := func() (int, string) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		out, err := exec.CommandContext(ctx, "git", "fetch", "-q", "origin").CombinedOutput()
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), strings.TrimSpace(string(out))
		}
		if err != nil {
			return -1, err.Error()
		}
		return 0, ""
	}
	results, err := os.OpenFile(cfg.results(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "stand-in: results:", err)
		return 2
	}
	defer results.Close()
	emit(map[string]any{"type": "system", "subtype": "init", "session_id": "stand-in", "model": "claude-haiku"})
	for round := 1; ; round++ {
		r := restartRound{Round: round, At: time.Now().UTC()}
		r.Model = model()
		r.MCP, r.MCPError = mcp("conform", "brief", "{}", round)
		r.Tool, r.ToolErr = mcp(NodeToolsServer, "progress", fmt.Sprintf(`{"note":"round %d"}`, round), round)
		r.Git, r.GitOut = gitFetch()
		line, _ := json.Marshal(r)
		results.Write(append(line, '\n'))
		emit(map[string]any{"type": "assistant", "session_id": "stand-in", "message": map[string]any{
			"id": fmt.Sprintf("msg-%d", round), "role": "assistant", "model": "claude-haiku",
			"content": []map[string]any{{"type": "text", "text": fmt.Sprintf("round %d", round)}}}})
		if _, err := os.Stat(cfg.stop()); err == nil {
			status, why := mcp("conform", "submit", `{"sha256":"5f0c"}`, round+1000)
			emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": fmt.Sprintf("submitted: %d %s", status, why),
				"session_id": "stand-in", "num_turns": round, "total_cost_usd": 0.01})
			return 0
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// restartNode is one node process the test started.
type restartNode struct {
	cmd  *exec.Cmd
	done chan struct{}
	mu   sync.Mutex
	logs []string
}

func startRestartNode(t *testing.T, name string, cfg restartConfig) *restartNode {
	t.Helper()
	raw, _ := json.Marshal(cfg)
	cmd := exec.Command(cfg.Self, "-test.run=^$")
	cmd.Env = append(os.Environ(), restartNodeEnv+"=1", restartConfigEnv+"="+string(raw))
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting node %s: %v", name, err)
	}
	n := &restartNode{cmd: cmd, done: make(chan struct{})}
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			n.mu.Lock()
			n.logs = append(n.logs, sc.Text())
			n.mu.Unlock()
			t.Logf("node %s: %s", name, sc.Text())
		}
	}()
	go func() { cmd.Wait(); close(n.done) }()
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-n.done
	})
	return n
}

// logged is the first line of the node's log holding substr.
func (n *restartNode) logged(substr string) (string, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, l := range n.logs {
		if strings.Contains(l, substr) {
			return l, true
		}
	}
	return "", false
}

// readRounds are the agent's rounds so far.
func readRounds(t *testing.T, cfg restartConfig) []restartRound {
	t.Helper()
	b, err := os.ReadFile(cfg.results())
	if err != nil {
		return nil
	}
	var out []restartRound
	for _, line := range bytes.Split(b, []byte("\n")) {
		var r restartRound
		if json.Unmarshal(line, &r) == nil && r.Round > 0 {
			out = append(out, r)
		}
	}
	return out
}

// waitUntil polls cond for up to d.
func waitUntil(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", d, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A session left running by a node restart keeps working once the next
// node process adopts its lease: its model calls are answered, its MCP
// servers -- the product's and the node's own tools -- answer, its git
// remote fetches, and its progress keeps reaching Mesh in the node's
// heartbeats, so Mesh's stall reaper has nothing to hand back. Then it
// finishes, and the adopting node reports the finish.
func TestASessionSurvivesANodeRestartE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("starts real node, sidecar and agent processes")
	}
	clearModelEnv(t)
	g := newRig(t, nil)
	var modelMu sync.Mutex
	var modelCalls []time.Time
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.URL.Path != "/v1/messages" || r.Header.Get("Authorization") != "Bearer "+fakeModelSecret {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"not the job's account"}}`)
			return
		}
		modelMu.Lock()
		modelCalls = append(modelCalls, time.Now())
		modelMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","content":[]}`)
	}))
	t.Cleanup(model.Close)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	// the product's MCP server and the code host, behind one TLS front
	// on restartUpstream: a lease's egress lets port 443 out alone
	prod := &product{l: g.latchkey, url: "https://" + restartUpstream + "/mcp"}
	ghURL, _ := url.Parse(g.github.srv.URL)
	gh := httputil.NewSingleHostReverseProxy(ghURL)
	front := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp" {
			prod.ServeHTTP(w, r)
			return
		}
		gh.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	cfg := restartConfig{
		MeshURL:     g.rt.API.BaseURL,
		WorkRoot:    g.work,
		SocketRoot:  sockettest.Dir(t, "rs"),
		MeshBinary:  testMeshBinary(t),
		GitUpstream: "https://" + restartUpstream,
		Models:      map[string]string{"anthropic": model.URL},
		GitPort:     port,
		FrontAddr:   front.Listener.Addr().String(),
		FrontCA:     string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: front.Certificate().Raw})),
		Self:        self,
		Dir:         t.TempDir(),
	}
	// whatever the node processes left running (the pod's processes,
	// the lease's egress) goes with the test
	t.Cleanup(func() {
		b, _ := os.ReadFile(cfg.pids())
		for _, f := range strings.Fields(string(b)) {
			if pid, err := strconv.Atoi(f); err == nil {
				syscall.Kill(-pid, syscall.SIGKILL)
			}
		}
		killLeaseEgress(cfg.WorkRoot)
		if b, err := os.ReadFile(filepath.Join(cfg.Dir, "sidecar.log")); err == nil {
			t.Logf("the relay sidecar's log:\n%s", b)
		}
		if b, err := os.ReadFile(filepath.Join(cfg.Dir, "agent.log")); err == nil && len(b) > 0 {
			t.Logf("the agent's stderr:\n%s", b)
		}
		if b, err := os.ReadFile(cfg.results()); err == nil {
			t.Logf("the agent's rounds:\n%s", b)
		}
		if calls, failures := prod.snapshot(); len(failures) > 0 {
			t.Logf("the product refused %d of %d call(s): %v", len(failures), len(calls), failures)
		}
	})
	job := g.offer(t, func(b *meshapi.Brief) {
		b.Servers = []meshapi.Server{{Name: "conform", URL: prod.url}}
		b.RenewEveryS = 1
	})

	// --- the first node takes the lease; its session works through the
	// relay
	a := startRestartNode(t, "A", cfg)
	waitUntil(t, 5*time.Minute, "the session's first good rounds on node A", func() bool {
		rs := readRounds(t, cfg)
		good := 0
		for _, r := range rs {
			if r.ok() {
				good++
			}
		}
		return good >= 2
	})
	lease := g.fake.LeaseOf(job)
	for _, r := range readRounds(t, cfg) {
		if !r.ok() {
			t.Logf("a round on node A was not all good (before the restart): %+v", r)
		}
	}

	// --- the restart: SIGHUP as `mesh node upgrade` sends it; the
	// process writes the hand-off and exits, the pod and the egress
	// process left running
	if err := a.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.done:
	case <-time.After(2 * time.Minute):
		t.Fatal("node A did not exit on its restart signal")
	}
	if n := len(g.fake.Calls("hand-back")) + len(g.fake.Calls("finish")) + len(g.fake.Calls("fail")); n != 0 {
		t.Fatalf("node A reported the lease's end on its restart (%d call(s)): it should leave it running", n)
	}
	before := len(readRounds(t, cfg))
	waitUntil(t, 3*time.Minute, "the session to go on running with no node", func() bool {
		return len(readRounds(t, cfg)) >= before+2
	})

	// --- the next node process adopts it
	b := startRestartNode(t, "B", cfg)
	waitUntil(t, 3*time.Minute, "node B to adopt the lease", func() bool {
		_, ok := b.logged("lease " + lease + " of job " + job)
		return ok
	})
	if l, ok := b.logged("adopted 1 of 1 hand-off(s) at start"); !ok {
		t.Fatalf("node B's aggregate adoption line is not %q (got %q)", "adopted 1 of 1", l)
	}
	adoptedAt := time.Now()
	adopted := len(readRounds(t, cfg))
	// the round in flight as B adopted may have reached the node in
	// between: judge the ones that started after it
	waitUntil(t, 5*time.Minute, "the session's rounds after the adoption", func() bool {
		return len(readRounds(t, cfg)) >= adopted+5
	})
	after := readRounds(t, cfg)[adopted+1:]
	var broken []string
	for _, r := range after {
		if r.Model != http.StatusOK {
			broken = append(broken, fmt.Sprintf("round %d: the model answered %d", r.Round, r.Model))
		}
		if r.MCP != http.StatusOK || r.MCPError != "" {
			broken = append(broken, fmt.Sprintf("round %d: the product's MCP answered %d %s", r.Round, r.MCP, r.MCPError))
		}
		if r.Tool != http.StatusOK || r.ToolErr != "" {
			broken = append(broken, fmt.Sprintf("round %d: the node's own tools answered %d %s", r.Round, r.Tool, r.ToolErr))
		}
		if r.Git != 0 {
			broken = append(broken, fmt.Sprintf("round %d: git fetch exited %d: %s", r.Round, r.Git, r.GitOut))
		}
	}
	if len(broken) > 0 {
		t.Errorf("after node B adopted the lease, the session's calls through its relay failed:\n  %s", strings.Join(broken, "\n  "))
	}
	modelMu.Lock()
	var modelAfter int
	for _, at := range modelCalls {
		if at.After(adoptedAt) {
			modelAfter++
		}
	}
	modelMu.Unlock()
	if modelAfter == 0 {
		t.Errorf("the model heard no call after node B adopted the lease")
	}

	// the session's progress reaches Mesh: node B's heartbeats carry a
	// progress_at for the lease that moves on after the adoption, so the
	// stall reaper (MESH_STALL_LIMIT) never finds it quiet
	var progressAt time.Time
	waitUntil(t, time.Minute, "a heartbeat from node B", func() bool {
		for _, c := range g.fake.Calls("heartbeat") {
			var hb meshapi.HeartbeatRequest
			if json.Unmarshal(c.Body, &hb) != nil {
				continue
			}
			for _, l := range hb.Leases {
				if l.LeaseID == lease && l.ProgressAt.After(progressAt) {
					progressAt = l.ProgressAt
				}
			}
		}
		return !progressAt.IsZero()
	})
	waitUntil(t, time.Minute, "the lease's progress to move on after the adoption", func() bool {
		for _, c := range g.fake.Calls("heartbeat") {
			var hb meshapi.HeartbeatRequest
			if json.Unmarshal(c.Body, &hb) != nil {
				continue
			}
			for _, l := range hb.Leases {
				if l.LeaseID == lease && l.ProgressAt.After(progressAt) {
					progressAt = l.ProgressAt
				}
			}
		}
		return progressAt.After(adoptedAt.Add(time.Second))
	})

	// --- the session finishes its work; the adopting node reports it
	if err := os.WriteFile(cfg.stop(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 3*time.Minute, "node B to report the lease's finish", func() bool {
		return g.fake.Ended(lease) != ""
	})
	if end := g.fake.Ended(lease); end != "finish" {
		t.Fatalf("the adopted lease ended %q, not finish; hand-backs %+v", end, g.fake.Calls("hand-back"))
	}
	if l, ok := b.logged("handed back"); ok {
		t.Fatalf("node B handed something back: %s", l)
	}
}

// withoutEnv is env without name: the node's own role must not reach
// the processes it starts for the pod.
func withoutEnv(env []string, name string) []string {
	var out []string
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k != name {
			out = append(out, kv)
		}
	}
	return out
}

// killLeaseEgress stops every lease-egress process still running for a
// lease under workRoot's leases (the test's own, by the pid each lease's
// actor.json names).
func killLeaseEgress(workRoot string) {
	entries, _ := os.ReadDir(filepath.Join(workRoot, "leases"))
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(workRoot, "leases", e.Name(), "actor.json"))
		if err != nil {
			continue
		}
		var rec struct {
			EgressPID int `json:"egressPid"`
		}
		if json.Unmarshal(b, &rec) == nil && rec.EgressPID > 0 {
			syscall.Kill(-rec.EgressPID, syscall.SIGKILL)
		}
		b, err = os.ReadFile(filepath.Join(workRoot, "leases", e.Name(), handoffFile))
		if err != nil {
			continue
		}
		var st handoffState
		if json.Unmarshal(b, &st) == nil && st.EgressPID > 0 {
			syscall.Kill(-st.EgressPID, syscall.SIGKILL)
		}
	}
}
