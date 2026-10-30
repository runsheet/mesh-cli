package nodev1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/harness/session/sessiontest"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/validate"
)

// podRig is an actor rig whose node runs each lease in a pod
// (`--sandbox`) on the fake sandbox runtime, and whose session is the
// streaming ClaudeSession on the fake claude: the fake runtime runs the
// agent's command on the host, as the pod would, with the node image's
// node and claude standing in.
func podRig(t *testing.T) (*rig, *sandbox.Fake, *ClaudeSession) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	sess := &ClaudeSession{PodBinary: self, Logf: t.Logf}
	g, sb := actorRig(t, sess)
	g.rt.Pods = &sandbox.Sandbox{Runtime: sb, Logf: t.Logf, Config: sandbox.Config{
		Image:          "mesh-node:test",
		Limits:         sandbox.Limits{CPUs: 2, Memory: "6g", Pids: 4096},
		ServicesLimits: sandbox.Limits{CPUs: 2, Memory: "4g"},
		Registries:     sandbox.DefaultRegistries,
		ServiceTimeout: 5 * time.Second,
	}}
	g.rt.gitBridgePort = freePort(t)
	return g, sb, sess
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// hostAgent runs a fake pod's agent on the host: its command, with the
// node image's `node` found on this machine, in its workdir, on its
// environment alone plus the fake claude's switch, on its streams. pre,
// set, runs in the agent's environment before the session (sh -c).
func hostAgent(t *testing.T, out, pre string) func(context.Context, sandbox.AgentSpec) (sandbox.Exit, error) {
	t.Helper()
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node on this machine to run the pod's git bridge")
	}
	return func(ctx context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		argv := slices.Clone(spec.Command)
		if argv[0] == "node" {
			argv[0] = nodeBin
		}
		if pre != "" {
			i := slices.Index(argv, "--") + 1
			argv = append(argv[:i:i], append([]string{"sh", "-c", pre + `; exec "$@"`, "sh"}, argv[i:]...)...)
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = spec.Workdir
		cmd.Env = append(slices.Clone(spec.Env), sessiontest.Env+"=1", "FAKE_CLAUDE_OUT="+out, "PATH="+filepath.Dir(nodeBin)+":"+sandbox.DefaultPath)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = spec.Stdin, spec.Stdout, spec.Stderr
		err := cmd.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return sandbox.Exit{Code: ee.ExitCode()}, nil
		}
		return sandbox.Exit{}, err
	}
}

// With --sandbox a lease's session runs in a pod of the lease's own:
// named for the lease and labelled with its job, node and lease, the
// brief's pods its sidecars, its agent on the brief's CPUs and memory
// with none of the node's environment, its MCP servers reached through
// the proxy shim on the lease's socket (whose directory alone is
// mounted), and its git reaching the relay through the bridge onto the
// same socket — the checkout's origin rewritten onto it. The actor holds
// the pod in actor.json and removes it as it releases.
func TestASandboxedLeaseRunsItsSessionAndServicesInItsPod(t *testing.T) {
	g, sb, _ := podRig(t)
	out := filepath.Join(t.TempDir(), "claude")
	lsRemote := filepath.Join(t.TempDir(), "ls-remote")
	var holds []string
	var job string
	agent := hostAgent(t, out, `git ls-remote origin > "`+lsRemote+`" 2>&1`)
	sb.Agent = func(ctx context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		// the agent runs: read what the actor holds
		b, _ := os.ReadFile(filepath.Join(g.rt.leaseDir(g.fake.LeaseOf(job)), "actor.json"))
		var rec struct{ Pods []string }
		json.Unmarshal(b, &rec)
		holds = rec.Pods
		return agent(ctx, spec)
	}
	t.Setenv("MESH_NODE_SECRET_FOR_TEST", "never-in-the-pod")
	job = g.offer(t, func(b *meshapi.Brief) {
		// the fake claude calls no finish tool: a kind with none finishes
		// on the session's end (finish enforcement is its own test)
		b.FinishTool, b.FinishSchema = "", nil
		b.Resources = meshapi.Resources{CPUs: 1, MemGB: 2}
		b.Pods = []json.RawMessage{json.RawMessage(`{"name":"postgres","image":"postgres:17-alpine","env":["POSTGRES_PASSWORD=mysecret"]}`)}
	})
	run(t, g, context.Background())
	lease := g.fake.LeaseOf(job)
	pod := PodName(lease)

	if n := len(g.fake.Calls("finish")); n != 1 {
		t.Fatalf("%d finishes; hand-backs %+v", n, g.fake.Calls("hand-back"))
	}
	specs := sb.PodSpecs()
	if len(specs) != 1 || specs[0].Name != pod {
		t.Fatalf("pods %+v", specs)
	}
	for k, want := range map[string]string{PodLeaseLabel: lease, PodJobLabel: job, PodNodeLabel: g.rt.NodeID(), sandbox.SessionLabel: lease} {
		if specs[0].Labels[k] != want {
			t.Errorf("pod label %s = %q, want %q", k, specs[0].Labels[k], want)
		}
	}
	if !slices.Contains(holds, pod) {
		t.Errorf("actor.json did not name the pod while it ran: %v", holds)
	}
	if len(sb.Sidecars) != 1 || sb.Sidecars[0].Name != "postgres" || !slices.Contains(sb.Sidecars[0].Env, "POSTGRES_PASSWORD=mysecret") {
		t.Fatalf("sidecars %+v", sb.Sidecars)
	}
	agents := sb.AgentSpecs()
	if len(agents) != 1 || agents[0].Name != sandbox.AgentName(sandbox.Pod{Name: pod}) {
		t.Fatalf("agents %+v", agents)
	}
	a := agents[0]
	if a.Limits.CPUs != 1 || a.Limits.Memory != "2048m" || a.Limits.Pids != 4096 {
		t.Errorf("agent limits %+v, want the brief's 1 cpu and 2 GB", a.Limits)
	}
	if a.Command[0] != "node" || !strings.HasSuffix(a.Command[1], gitBridgeName) || a.Command[4] != "--" {
		t.Errorf("the agent does not run under the git bridge: %q", a.Command)
	}
	env := strings.Join(a.Env, "\n")
	if strings.Contains(env, "never-in-the-pod") {
		t.Error("the node's own environment reached the pod")
	}
	for _, want := range []string{"GIT_CONFIG_KEY_0=url.http://127.0.0.1:" + strconv.Itoa(g.rt.gitBridgePort) + "/git/.insteadOf", "GIT_CONFIG_VALUE_0=http://127.0.0.1:", "GIT_TERMINAL_PROMPT=0"} {
		if !strings.Contains(env, want) {
			t.Errorf("pod env lacks %q:\n%s", want, env)
		}
	}
	leaf := g.rt.leaseSocketLeaf(lease)
	mounted := false
	for _, m := range a.Mounts {
		if filepath.Base(m.Source) == leaf && m.Source == m.Target && !m.ReadOnly {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("the lease's socket directory (%s) is not mounted: %+v", leaf, a.Mounts)
	}
	args, _ := os.ReadFile(out + ".args")
	if !strings.Contains(string(args), harnessShim()) || strings.Contains(string(args), "mcp-relay") {
		t.Errorf("the pod's MCP servers are not on the proxy shim:\n%s", args)
	}
	ls, _ := os.ReadFile(lsRemote)
	if !strings.Contains(string(ls), g.github.head) {
		t.Errorf("git in the pod did not reach the relay through the bridge: %s", ls)
	}
	if !slices.Contains(sb.Removed, pod) {
		t.Errorf("the pod was not removed: %v", sb.Removed)
	}
	assertReleased(t, g, job)
}

func harnessShim() string { return "mcp-proxy-shim.js" }

// A required service that does not start hands the lease back
// cannot_run before any session — never a session without its database
// — and the pod goes with the actor's release.
func TestAPodWhoseServiceFailsHandsBackCannotRun(t *testing.T) {
	g, sb, _ := podRig(t)
	sb.SidecarErr = map[string]error{"postgres": errors.New("pull refused")}
	job := g.offer(t, func(b *meshapi.Brief) {
		b.Pods = []json.RawMessage{json.RawMessage(`{"name":"postgres","image":"postgres:17-alpine"}`)}
	})
	run(t, g, context.Background())
	hb := onlyHandBack(t, g, job)
	if hb.Cause != meshapi.CauseCannotRun || !strings.Contains(hb.Reason, "postgres") {
		t.Fatalf("hand-back %+v", hb)
	}
	if n := len(sb.AgentSpecs()); n != 0 {
		t.Fatalf("%d agents ran without their service", n)
	}
	if !slices.Contains(sb.Removed, PodName(g.fake.LeaseOf(job))) {
		t.Fatalf("the pod was not removed: %v", sb.Removed)
	}
	assertReleased(t, g, job)
}

// The pod's agent killed under the session — exit 137 from the runtime,
// or its memory limit — is pod_killed, never the work's red.
func TestAPodSessionKilledIsPodKilled(t *testing.T) {
	for name, exit := range map[string]sandbox.Exit{"exit 137": {Code: 137}, "oom": {Code: 137, OOMKilled: true}} {
		t.Run(name, func(t *testing.T) {
			g, sb, _ := podRig(t)
			sb.Agent = func(ctx context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
				io.Copy(io.Discard, io.LimitReader(spec.Stdin, 1))
				return exit, nil
			}
			job := g.offer(t, nil)
			run(t, g, context.Background())
			hb := onlyHandBack(t, g, job)
			if hb.Cause != meshapi.CausePodKilled || hb.ExitCode != 137 {
				t.Fatalf("hand-back %+v", hb)
			}
			assertReleased(t, g, job)
		})
	}
}

// run_suite's steps run in the lease's pod, beside its services: each a
// container of the pod's (Exec), whose exit is the step's, a limit's
// kill 137 and the runtime's timeout 124.
func TestRunSuiteStepsRunInThePod(t *testing.T) {
	f := &sandbox.Fake{}
	sb := &sandbox.Sandbox{Runtime: f, TempDir: t.TempDir(), Config: sandbox.Config{Image: "mesh-node:test", Limits: sandbox.Limits{CPUs: 2, Memory: "6g"}}}
	sess, err := sb.Open("lease-1", "conform/validate", sandbox.GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	sess.Pin("mesh-v1-lease-1", nil)
	p := &leasePod{sess: sess, name: "mesh-v1-lease-1"}
	ws := t.TempDir()
	exits := map[string]sandbox.Exit{"echo ok": {}, "exit 3": {Code: 3}, "oom": {Code: 137, OOMKilled: true}}
	f.Agent = func(_ context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		return exits[spec.Command[2]], nil
	}
	steps := []validate.Step{{Name: "ok", Command: "echo ok"}, {Name: "red", Command: "exit 3"}, {Name: "big", Command: "oom"}}
	res, err := p.suites(context.Background(), ws, steps)
	if err != nil {
		t.Fatal(err)
	}
	got := []int{}
	for _, r := range res {
		got = append(got, r.ExitCode)
	}
	if !slices.Equal(got, []int{0, 3, 137}) {
		t.Fatalf("exits %v", got)
	}
	for _, a := range f.AgentSpecs() {
		if !strings.HasPrefix(a.Name, "mesh-v1-lease-1-exec-") || a.Command[0] != "sh" || a.Workdir != ws {
			t.Fatalf("step container %+v", a)
		}
	}
	if len(f.Removed) != 0 {
		t.Fatalf("a step removed the pod: %v", f.Removed)
	}
}

// The git bridge pipes the pod's loopback port to the lease's socket and
// runs its command, exiting as the command does — a signal's 128+n.
func TestTheGitBridge(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node on this machine")
	}
	dir := shortDir(t)
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var paths []string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		io.WriteString(w, "via the socket")
	})}
	go srv.Serve(ln)
	defer srv.Close()
	script := filepath.Join(dir, gitBridgeName)
	os.WriteFile(script, gitBridgeScript(), 0o600)
	port := strconv.Itoa(freePort(t))

	fetch := `fetch("http://127.0.0.1:` + port + `/git/acme/fixture.git/info/refs").then(r => r.text()).then(t => { process.stdout.write(t); process.exit(3) })`
	var stdout bytes.Buffer
	cmd := exec.Command(nodeBin, script, sock, port, "--", nodeBin, "-e", fetch)
	cmd.Stdout = &stdout
	err = cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Fatalf("the bridge did not exit as its command: %v", err)
	}
	if stdout.String() != "via the socket" || len(paths) != 1 || paths[0] != "/git/acme/fixture.git/info/refs" {
		t.Fatalf("stdout %q, paths %v", stdout.String(), paths)
	}

	cmd = exec.Command(nodeBin, script, sock, strconv.Itoa(freePort(t)), "--", "sh", "-c", "kill -KILL $$")
	err = cmd.Run()
	if !errors.As(err, &ee) || ee.ExitCode() != 128+int(syscall.SIGKILL) {
		t.Fatalf("a killed command is not 137 through the bridge: %v", err)
	}
}

// A pod session reaches its model through the bridge (the model relay,
// #1691): its environment names http://127.0.0.1:<bridge>/model/anthropic
// as ANTHROPIC_BASE_URL — not the host's loopback, which the pod cannot
// reach — and holds only the relay's dummy and no non-essential traffic,
// never the account's secret nor the node's own key; the fake claude's
// model call goes bridge → lease socket → relay, and the upstream sees
// the account's real bearer.
func TestAPodSessionReachesItsModelThroughTheBridge(t *testing.T) {
	clearModelEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", nodeOwnSecret)
	g, sb, _ := podRig(t)
	model := newFakeModel(t, func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer "+fakeModelSecret })
	g.rt.ModelUpstreams = map[string]string{"anthropic": model.srv.URL}
	out := filepath.Join(t.TempDir(), "claude")
	sb.Agent = hostAgent(t, out, "")
	g.offer(t, func(b *meshapi.Brief) { b.FinishTool, b.FinishSchema = "", nil })
	run(t, g, context.Background())
	if n := len(g.fake.Calls("finish")); n != 1 {
		t.Fatalf("%d finishes; hand-backs %+v", n, g.fake.Calls("hand-back"))
	}
	agents := sb.AgentSpecs()
	if len(agents) != 1 {
		t.Fatalf("agents %+v", agents)
	}
	a := agents[0]
	if a.Command[0] != "node" || !strings.HasSuffix(a.Command[1], gitBridgeName) {
		t.Fatalf("the agent does not run under the bridge: %q", a.Command)
	}
	want := "http://127.0.0.1:" + strconv.Itoa(g.rt.gitBridgePort) + "/model/anthropic"
	if envValue(a.Env, "ANTHROPIC_BASE_URL") != want || envValue(a.Env, "CLAUDE_CODE_OAUTH_TOKEN") != ModelDummy ||
		envValue(a.Env, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC") != "1" || envValue(a.Env, "ANTHROPIC_API_KEY") != "" {
		t.Fatalf("the pod's model environment: base %q, want %q; oauth %q", envValue(a.Env, "ANTHROPIC_BASE_URL"), want, envValue(a.Env, "CLAUDE_CODE_OAUTH_TOKEN"))
	}
	for _, kv := range a.Env {
		if strings.Contains(kv, fakeModelSecret) || strings.Contains(kv, nodeOwnSecret) {
			t.Fatalf("the pod's environment holds a model key: %s", strings.SplitN(kv, "=", 2)[0])
		}
	}
	answer, _ := os.ReadFile(out + ".model")
	if !strings.HasPrefix(string(answer), "200 ") {
		t.Fatalf("the pod session's model call answered %q", answer)
	}
	seen, dummy := model.calls()
	if len(seen) != 1 || seen[0] != "Bearer "+fakeModelSecret+"|" || dummy {
		t.Fatalf("the model saw %d call(s), the dummy %v", len(seen), dummy)
	}
}

// A pod session's Stop hook resolves in the pod (finish enforcement,
// #1692): it is the node image's node on a shim in the session's HOME,
// asking the gate on its socket in the mounted socket directory — no
// mesh binary, no host path the pod lacks. Run as claude would run it
// while the session is live, it is refused: the finish is owed.
func TestAPodSessionsStopHookResolvesInThePod(t *testing.T) {
	g, sb, _ := podRig(t)
	out := filepath.Join(t.TempDir(), "claude")
	agent := hostAgent(t, out, "")
	nodeBin, _ := exec.LookPath("node")
	var command, answer string
	var mounts []sandbox.Mount
	sb.Agent = func(ctx context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		mounts = spec.Mounts
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
			// claude runs the hook in a shell, in the pod's environment
			hook := exec.CommandContext(ctx, "sh", "-c", command)
			hook.Env = append(slices.Clone(spec.Env), "PATH="+filepath.Dir(nodeBin)+":"+sandbox.DefaultPath)
			hook.Stdin = strings.NewReader(`{"hook_event_name":"Stop"}`)
			b, _ := hook.Output()
			answer = string(b)
		}
		return agent(ctx, spec)
	}
	job := g.offer(t, nil)
	run(t, g, context.Background())
	lease := g.fake.LeaseOf(job)
	if !strings.HasPrefix(command, "node '") || !strings.Contains(command, stopHookName) || strings.Contains(command, g.rt.MeshBinary) || strings.Contains(command, StopHookCommand) {
		t.Fatalf("the pod's Stop hook is %q", command)
	}
	fields := strings.Fields(command)
	shim, socket := strings.Trim(fields[1], "'"), strings.Trim(fields[2], "'")
	if filepath.Base(socket) != stopSocketName || filepath.Base(filepath.Dir(socket)) != g.rt.leaseSocketLeaf(lease) {
		t.Fatalf("the hook's socket %s is not the gate's in the lease's socket directory", socket)
	}
	under := func(path string) bool {
		for _, m := range mounts {
			if m.Source == m.Target && strings.HasPrefix(path, m.Target+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}
	if !under(shim) || !under(socket) {
		t.Fatalf("the shim %s or the socket %s is not under a pod mount: %+v", shim, socket, mounts)
	}
	if !strings.Contains(answer, `"decision":"block"`) || !strings.Contains(answer, "Call ") {
		t.Fatalf("the hook in the pod was not refused by the gate: %q", answer)
	}
}
