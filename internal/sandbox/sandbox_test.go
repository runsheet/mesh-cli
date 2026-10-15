package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/validate"
)

func testSandbox(t *testing.T, rt Runtime) *Sandbox {
	t.Helper()
	return &Sandbox{Runtime: rt, TempDir: t.TempDir(), Config: Config{
		Image:   "ghcr.io/runsheet/mesh-node:v1",
		Limits:  Limits{CPUs: DefaultCPUs, Memory: DefaultMemory, Pids: DefaultPids},
		Network: DefaultNetwork(),
	}}
}

func runOnce(t *testing.T, f *Fake, env []string) (*Session, AgentSpec, string) {
	t.Helper()
	sb := testSandbox(t, f)
	sess, err := sb.Open("ship-it/2", "build", GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	if err := sess.Run(context.Background(), RunSpec{Workspace: ws, Env: env, Command: []string{"claude", "-p"}, Stdin: strings.NewReader("prompt")}); err != nil {
		t.Fatal(err)
	}
	if len(f.Agents) != 1 {
		t.Fatalf("agents: %+v", f.Agents)
	}
	return sess, f.Agents[0], ws
}

// The pod mounts exactly two host paths — the workspace and the
// session's temporary HOME, both read-write — and never the operator's
// home, ~/.mesh, a runner's directory or a container socket.
func TestPodMountsOnlyWorkspaceAndHome(t *testing.T) {
	f := &Fake{}
	sess, spec, ws := runOnce(t, f, nil)
	defer sess.Close()
	want := []Mount{{Source: ws, Target: ws}, {Source: sess.Home, Target: sess.Home}}
	if !slices.Equal(spec.Mounts, want) {
		t.Fatalf("mounts = %+v, want %+v", spec.Mounts, want)
	}
	userHome, _ := os.UserHomeDir()
	args := AgentRunArgs(Pod{Name: "p"}, spec)
	var vols []string
	for i, a := range args {
		if a == "--volume" || a == "-v" || a == "--mount" {
			vols = append(vols, args[i+1])
		}
	}
	if len(vols) != 2 || vols[0] != ws+":"+ws+":rw" || vols[1] != sess.Home+":"+sess.Home+":rw" {
		t.Fatalf("volumes = %v", vols)
	}
	for _, v := range vols {
		src, _, _ := strings.Cut(v, ":")
		for _, bad := range []string{userHome, filepath.Join(userHome, ".mesh"), filepath.Join(userHome, ".config", "gh"), "/var/run/docker.sock", "/run/docker.sock", "actions-runner"} {
			if src == bad || (bad == "actions-runner" && strings.Contains(src, bad)) {
				t.Fatalf("mounted %s", src)
			}
		}
		if src == userHome || src == "/" {
			t.Fatalf("mounted the host home or root: %s", src)
		}
	}
	if strings.HasPrefix(sess.Home, userHome+string(filepath.Separator)) && !strings.HasPrefix(sess.Home, os.TempDir()) {
		t.Fatalf("the temp HOME %s is inside the operator's home", sess.Home)
	}
	for _, a := range args {
		if strings.Contains(a, "docker.sock") || strings.Contains(a, "podman.sock") || a == "--privileged" || a == "--env-host" {
			t.Fatalf("agent args carry %q: %v", a, args)
		}
	}
}

// PodCreateArgs maps the host user onto the image's user: the declared
// uid:gid, else DefaultImageUID for a spec carrying none.
func TestPodCreateArgsUserns(t *testing.T) {
	for _, tc := range []struct {
		spec PodSpec
		want string
	}{
		{PodSpec{Name: "p", UID: 1000, GID: 1000}, "keep-id:uid=1000,gid=1000"},
		{PodSpec{Name: "p"}, "keep-id:uid=1000,gid=1000"},
		{PodSpec{Name: "p", UID: 1001, GID: 1002}, "keep-id:uid=1001,gid=1002"},
	} {
		if got := flagAfter(PodCreateArgs(tc.spec), "--userns"); got != tc.want {
			t.Errorf("PodCreateArgs(uid %d, gid %d) --userns = %q, want %q", tc.spec.UID, tc.spec.GID, got, tc.want)
		}
	}
}

func TestKeepIDUserns(t *testing.T) {
	for _, tc := range []struct {
		uid, gid int
		want     string
	}{
		{uid: 1000, gid: 1000, want: "keep-id:uid=1000,gid=1000"},
		{uid: 1001, gid: 1002, want: "keep-id:uid=1001,gid=1002"},
		{uid: 0, gid: 0, want: "keep-id:uid=1000,gid=1000"},
	} {
		if got := KeepIDUserns(tc.uid, tc.gid); got != tc.want {
			t.Errorf("KeepIDUserns(%d, %d) = %q, want %q", tc.uid, tc.gid, got, tc.want)
		}
	}
}

// stubPodman is a Podman whose Exec records every call's args and
// answers the image inspect with user (or inspectErr), every other
// call with nothing.
func stubPodman(user string, inspectErr error) (*Podman, *[][]string) {
	var calls [][]string
	return &Podman{Exec: func(_ context.Context, args, _ []string, _ io.Reader, stdout, _ io.Writer) error {
		calls = append(calls, append([]string(nil), args...))
		if len(args) > 1 && args[0] == "image" && args[1] == "inspect" {
			if inspectErr != nil {
				return inspectErr
			}
			_, _ = io.WriteString(stdout, user+"\n")
		}
		return nil
	}}, &calls
}

// CreatePod reads the image's declared user through `podman image
// inspect --format {{.Config.User}} <image>` and maps the host user
// onto that uid:gid.
func TestCreatePodReadsTheImageUser(t *testing.T) {
	const image = "ghcr.io/runsheet/mesh-node:v1"
	p, calls := stubPodman("1001:1002", nil)
	if _, err := p.CreatePod(context.Background(), PodSpec{Name: "p", Image: image}); err != nil {
		t.Fatal(err)
	}
	want := []string{"image", "inspect", "--format", "{{.Config.User}}", image}
	if len(*calls) < 2 || !slices.Equal((*calls)[0], want) {
		t.Fatalf("first call = %v, want %v", *calls, want)
	}
	create := (*calls)[1]
	if len(create) < 2 || create[0] != "pod" || create[1] != "create" {
		t.Fatalf("second call = %v, want pod create", create)
	}
	if got := flagAfter(create, "--userns"); got != "keep-id:uid=1001,gid=1002" {
		t.Errorf("pod create --userns = %q, want keep-id:uid=1001,gid=1002", got)
	}
}

// An inspect that fails is no reason not to make the pod: it maps onto
// DefaultImageUID.
func TestCreatePodFallsBackWhenTheInspectFails(t *testing.T) {
	p, calls := stubPodman("", errors.New("no such image"))
	if _, err := p.CreatePod(context.Background(), PodSpec{Name: "p", Image: "missing:v1"}); err != nil {
		t.Fatal(err)
	}
	var create []string
	for _, c := range *calls {
		if len(c) > 1 && c[0] == "pod" && c[1] == "create" {
			create = c
		}
	}
	if create == nil {
		t.Fatalf("no pod create among %v", *calls)
	}
	if got := flagAfter(create, "--userns"); got != "keep-id:uid=1000,gid=1000" {
		t.Errorf("pod create --userns = %q, want keep-id:uid=1000,gid=1000", got)
	}
}

// The agent's environment is built from the allow-list: the node's own
// variables never reach it, nor any job variable outside the list.
func TestAgentEnvIsTheAllowList(t *testing.T) {
	t.Setenv("MESH_DECOY", "decoy-from-node")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("GH_TOKEN", "ghp_operator_admin_org")
	jobEnv := []string{
		"GH_TOKEN=ghs_item", "GH_CONFIG_DIR=/tmp/host-gh-dir",
		"CLAUDE_CODE_OAUTH_TOKEN=oauth", "CLAUDE_CONFIG_DIR=/tmp/host-claude",
		"MESH_ACCOUNT=team", "MESH_TOOL_LINEAR_API_KEY=lin",
		"MESH_DECOY=decoy-in-job", "HOME=/home/chris", "OPENAI_API_KEY=oa",
	}
	f := &Fake{}
	sess, spec, _ := runOnce(t, f, jobEnv)
	defer sess.Close()
	got := map[string]string{}
	for _, kv := range spec.Env {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := got[k]; dup {
			t.Fatalf("%s set twice: %v", k, spec.Env)
		}
		got[k] = v
	}
	names := make([]string, 0, len(got))
	for k := range got {
		names = append(names, k)
	}
	sort.Strings(names)
	want := []string{"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR", "DISABLE_AUTOUPDATER", "GH_CONFIG_DIR", "GH_TOKEN", "HOME", "LANG", "MESH_ACCOUNT", "MESH_ITEM", "MESH_SESSION_KIND", "MESH_TOOL_LINEAR_API_KEY", "OPENAI_API_KEY", "PATH", "TERM", "TZ"}
	if !slices.Equal(names, want) {
		t.Fatalf("env names = %v\nwant %v", names, want)
	}
	if got["GH_TOKEN"] != "ghs_item" || got["HOME"] != sess.Home {
		t.Fatalf("GH_TOKEN %q HOME %q", got["GH_TOKEN"], got["HOME"])
	}
	if got["GH_CONFIG_DIR"] != filepath.Join(sess.Home, ".config", "gh") || got["CLAUDE_CONFIG_DIR"] != filepath.Join(sess.Home, ".claude") {
		t.Fatalf("config dirs must be under the temp HOME: %v", got)
	}
	for _, v := range got {
		if strings.Contains(v, "decoy") || v == "aws-secret" || v == "ghp_operator_admin_org" {
			t.Fatalf("a node or decoy value leaked: %v", got)
		}
	}
	// the podman process carries the values; its command line names only
	args := AgentRunArgs(Pod{Name: "p"}, spec)
	if !slices.Contains(args, "--unsetenv-all") {
		t.Fatalf("image defaults must be dropped: %v", args)
	}
	for _, a := range args {
		if strings.Contains(a, "=oauth") || strings.Contains(a, "=ghs_item") || strings.Contains(a, "=lin") {
			t.Fatalf("a secret is on podman's command line: %v", args)
		}
	}
	penv := podmanEnv(os.Environ(), spec.Env)
	for _, kv := range penv {
		k, _, _ := strings.Cut(kv, "=")
		if k == "MESH_DECOY" || k == "AWS_SECRET_ACCESS_KEY" || k == "SSH_AUTH_SOCK" || k == "DOCKER_HOST" {
			t.Fatalf("podman's own env passes %s on", k)
		}
	}
	if v, _ := lastValue(penv, "GH_TOKEN"); v != "ghs_item" {
		t.Fatalf("podman env GH_TOKEN = %q", v)
	}
	// the container's HOME is podman's too, read by --env HOME; its store
	// stays the node's by XDG_DATA_HOME and XDG_CONFIG_HOME
	if v, _ := lastValue(penv, "HOME"); v != sess.Home {
		t.Fatalf("podman env HOME = %q, want the session's %q for --env HOME", v, sess.Home)
	}
	if v, _ := lastValue(penv, "XDG_DATA_HOME"); v == "" || strings.HasPrefix(v, sess.Home) {
		t.Fatalf("podman env XDG_DATA_HOME = %q: podman's store must stay the node's", v)
	}
}

// TestPodmanEnvKeepsTheNodesStore pins the rule from both sides
// (ADR-007, Env by name): a container env with HOME=/tmp/x and
// PATH=/sandbox/bin against a node env with HOME=/home/node leaves
// podman's own HOME and PATH the container's — AgentRunArgs and
// SidecarRunArgs give them, as every variable, as a bare `--env NAME`,
// with no `--env NAME=value` and no --env-file — while podman's store
// stays the node's: XDG_DATA_HOME and XDG_CONFIG_HOME pinned under
// /home/node, and the node's XDG_RUNTIME_DIR and CONTAINERS_STORAGE_CONF
// kept whatever the container sets, those never passed to it.
func TestPodmanEnvKeepsTheNodesStore(t *testing.T) {
	node := []string{"HOME=/home/node", "USER=node", "XDG_RUNTIME_DIR=/run/user/1000", "CONTAINERS_STORAGE_CONF=/etc/s.conf", "PATH=/usr/bin", "AWS_SECRET_ACCESS_KEY=no"}
	container := []string{"HOME=/tmp/x", "PATH=/sandbox/bin", "GH_TOKEN=t", "XDG_RUNTIME_DIR=/tmp/run", "CONTAINERS_STORAGE_CONF=/tmp/s.conf"}
	penv := podmanEnv(node, container)
	want := map[string]string{
		"HOME": "/tmp/x", "PATH": "/sandbox/bin", "GH_TOKEN": "t", "USER": "node",
		"XDG_RUNTIME_DIR": "/run/user/1000", "CONTAINERS_STORAGE_CONF": "/etc/s.conf",
		"XDG_DATA_HOME": "/home/node/.local/share", "XDG_CONFIG_HOME": "/home/node/.config",
	}
	seen := map[string]bool{}
	for _, kv := range penv {
		k, v, _ := strings.Cut(kv, "=")
		if seen[k] {
			t.Fatalf("%s twice in podman env %v", k, penv)
		}
		seen[k] = true
		if w, ok := want[k]; !ok || v != w {
			t.Fatalf("podman env %s=%q, want %q (%v)", k, v, w, penv)
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("podman env = %v, want %v", penv, want)
	}
	// a node that sets its own XDG_DATA_HOME keeps it
	if v, _ := lastValue(podmanEnv(append(node, "XDG_DATA_HOME=/srv/data"), container), "XDG_DATA_HOME"); v != "/srv/data" {
		t.Fatalf("XDG_DATA_HOME = %q, want the node's /srv/data", v)
	}
	for _, args := range [][]string{
		AgentRunArgs(Pod{Name: "p"}, AgentSpec{Name: "a", Image: "img", Env: container}),
		SidecarRunArgs(Pod{Name: "p"}, SidecarSpec{Name: "s", Image: "img", Env: container}),
	} {
		if got := flagValues(args, "--env"); !slices.Equal(got, []string{"HOME", "PATH", "GH_TOKEN"}) {
			t.Fatalf("--env = %v, want HOME PATH GH_TOKEN by name: %v", got, args)
		}
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "--env-file") || strings.Contains(joined, "=") {
			t.Fatalf("a value is on the command line: %s", joined)
		}
	}
}

func lastValue(env []string, key string) (string, bool) {
	v, ok := "", false
	for _, kv := range env {
		if k, val, found := strings.Cut(kv, "="); found && k == key {
			v, ok = val, true
		}
	}
	return v, ok
}

// The network refuses the three LAN ranges and the host's loopback: the
// pod's pasta maps neither the gateway nor host loopback, and the rules
// loaded into it drop the ranges and accept only HTTPS/HTTP out.
func TestNetworkRefusesTheLAN(t *testing.T) {
	f := &Fake{}
	sess, _, _ := runOnce(t, f, nil)
	defer sess.Close()
	if len(f.Pods) != 1 {
		t.Fatalf("pods: %+v", f.Pods)
	}
	rules := f.Pods[0].Network.Ruleset()
	var drop string
	for _, line := range strings.Split(rules, "\n") {
		if strings.Contains(line, "ip daddr {") && strings.HasSuffix(line, "drop") {
			drop = line
		}
	}
	for _, r := range []string{"192.168.0.0/16", "10.0.0.0/8", "172.16.0.0/12", "127.0.0.0/8", "169.254.0.0/16", "100.64.0.0/10"} {
		if !strings.Contains(drop, r) {
			t.Fatalf("rules do not drop %s:\n%s", r, rules)
		}
	}
	for _, want := range []string{"policy drop;", "oif \"lo\" accept", "tcp dport { 443, 80 } accept", "ip6 daddr { ::1/128, fc00::/7, fe80::/10"} {
		if !strings.Contains(rules, want) {
			t.Fatalf("rules lack %q:\n%s", want, rules)
		}
	}
	// the DNS forwarder is let through before the link-local drop
	if strings.Index(rules, DNSForward) > strings.Index(rules, "169.254.0.0/16") {
		t.Fatalf("DNS accept must come before the drop:\n%s", rules)
	}
	args := PodCreateArgs(f.Pods[0])
	net := flagAfter(args, "--network")
	if !strings.HasPrefix(net, "pasta:") || !strings.Contains(net, "--no-map-gw") || strings.Contains(net, "map-host-loopback") {
		t.Fatalf("pod network = %q", net)
	}
	if !slices.Contains(args, "--no-hosts") || flagAfter(args, "--userns") != "keep-id:uid=1000,gid=1000" {
		t.Fatalf("pod args = %v", args)
	}
	init := NetworkInitArgs(Pod{Name: "p"}, "img")
	if flagAfter(init, "--cap-add") != "NET_ADMIN" || flagAfter(init, "--cap-drop") != "all" {
		t.Fatalf("init args = %v", init)
	}
	agent := AgentRunArgs(Pod{Name: "p"}, AgentSpec{Name: "a", Image: "img"})
	if flagAfter(agent, "--cap-drop") != "all" || slices.Contains(agent, "--cap-add") || flagAfter(agent, "--security-opt") != "no-new-privileges" {
		t.Fatalf("the agent must hold no capability: %v", agent)
	}
}

func flagAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// The pod is removed when the session ends, and the HOME on Close.
func TestSessionCleansUp(t *testing.T) {
	f := &Fake{}
	sess, _, _ := runOnce(t, f, nil)
	if len(f.Removed) != 1 || f.Removed[0] != f.Pods[0].Name {
		t.Fatalf("removed = %v", f.Removed)
	}
	for _, rel := range []string{".gitconfig", ".claude", ".config/gh"} {
		if _, err := os.Stat(filepath.Join(sess.Home, rel)); err != nil {
			t.Fatalf("home lacks %s: %v", rel, err)
		}
	}
	gc, _ := os.ReadFile(filepath.Join(sess.Home, ".gitconfig"))
	if strings.Contains(string(gc), "[user]") || !strings.Contains(string(gc), "directory = *") {
		t.Fatalf(".gitconfig = %s", gc)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sess.Home); !os.IsNotExist(err) {
		t.Fatalf("home outlived its session: %v", err)
	}
}

// A session names the image its agent runs, and Describe says so.
func TestSessionNamesItsImage(t *testing.T) {
	sb := testSandbox(t, &Fake{})
	sb.Config.Image = "ghcr.io/runsheet/mesh-node:v0.2.335"
	sess, err := sb.Open("ship-it/1", "build", GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if got := sess.Image(); got != "ghcr.io/runsheet/mesh-node:v0.2.335" {
		t.Fatalf("Image() = %q", got)
	}
	if got, want := sess.Describe(), "session ship-it/1 — image ghcr.io/runsheet/mesh-node:v0.2.335"; got != want {
		t.Fatalf("Describe() = %q, want %q", got, want)
	}
}

// A kill by a limit is a *LimitError; any other exit an *ExitError.
func TestLimitKillsAreTyped(t *testing.T) {
	l := Limits{CPUs: 2, Memory: "6g", Pids: 4096}
	for _, tc := range []struct {
		exit    Exit
		elapsed time.Duration
		timeout time.Duration
		stderr  string
		limit   string
		code    int
	}{
		{exit: Exit{Code: 137, OOMKilled: true}, limit: "memory"},
		{exit: Exit{Code: 137}, elapsed: time.Minute, timeout: time.Hour, limit: "memory"},
		{exit: Exit{Code: 137}, elapsed: 2 * time.Hour, timeout: time.Hour, limit: "timeout"},
		{exit: Exit{Code: 128}, stderr: "bash: fork: retry: Resource temporarily unavailable", limit: "pids"},
		{exit: Exit{Code: 1}, stderr: "some test failed", code: 1},
	} {
		err := classify(tc.exit, tc.elapsed, tc.timeout, l, tc.stderr)
		var le *LimitError
		var ee *ExitError
		switch {
		case tc.limit != "":
			if !errors.As(err, &le) || le.Limit != tc.limit {
				t.Errorf("%+v: err = %v, want limit %s", tc, err, tc.limit)
			}
		default:
			if !errors.As(err, &ee) || ee.Code != tc.code {
				t.Errorf("%+v: err = %v", tc, err)
			}
		}
	}
	if classify(Exit{}, 0, 0, l, "") != nil {
		t.Fatal("exit 0 is no error")
	}
}

// A run whose agent the runtime OOM-kills returns the LimitError, and
// the pod is still removed.
func TestRunReturnsALimitKill(t *testing.T) {
	f := &Fake{Agent: func(ctx context.Context, spec AgentSpec) (Exit, error) {
		_, _ = io.WriteString(spec.Stdout, "{}\n")
		return Exit{Code: 137, OOMKilled: true}, nil
	}}
	sb := testSandbox(t, f)
	sess, _ := sb.Open("x/1", "build", GitIdentity{})
	defer sess.Close()
	err := sess.Run(context.Background(), RunSpec{Workspace: t.TempDir(), Stdout: io.Discard, Timeout: time.Hour})
	var le *LimitError
	if !errors.As(err, &le) || le.Limit != "memory" || le.Value != "6g" {
		t.Fatalf("err = %v", err)
	}
	if len(f.Removed) != 1 {
		t.Fatalf("pod not removed: %v", f.Calls)
	}
	if f.Agents[0].TimeoutSeconds != int((time.Hour+TimeoutGrace)/time.Second) {
		t.Fatalf("runtime timeout = %d", f.Agents[0].TimeoutSeconds)
	}
}

func TestRegistryAllowed(t *testing.T) {
	for image, want := range map[string]bool{
		"postgres:16":                       true,
		"library/redis:7":                   true,
		"docker.io/library/mysql:8":         true,
		"ghcr.io/acme/svc:1":                true,
		"us-docker.pkg.dev/proj/repo/img:1": true,
		"quay.io/minio/minio":               true,
		"registry.example.com/x:1":          false,
		"localhost:5000/x":                  false,
	} {
		if got := RegistryAllowed(Registry(image), DefaultRegistries); got != want {
			t.Errorf("%s: %v, want %v", image, got, want)
		}
	}
}

// Open writes the agent identity it is given into the session's
// .gitconfig — never the operator's — and mounts the checkout's hooks
// directory read-only at its own path (ADR-024).
func TestOpenWritesTheAgentIdentity(t *testing.T) {
	f := &Fake{}
	sb := testSandbox(t, f)
	hooks := t.TempDir()
	sess, err := sb.Open("ship-it/2", "build", GitIdentity{Name: "Mesh builder (mini)", Email: "mesh-builder+mini@runsheet.dev", HooksDir: hooks})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	gc, err := os.ReadFile(filepath.Join(sess.Home, ".gitconfig"))
	if err != nil {
		t.Fatal(err)
	}
	want := "[user]\n\tname = \"Mesh builder (mini)\"\n\temail = \"mesh-builder+mini@runsheet.dev\"\n[safe]\n\tdirectory = *\n"
	if string(gc) != want {
		t.Fatalf(".gitconfig = %q, want %q", gc, want)
	}
	cmd := exec.Command("git", "config", "--file", filepath.Join(sess.Home, ".gitconfig"), "user.name")
	if out, err := cmd.Output(); err != nil || strings.TrimSpace(string(out)) != "Mesh builder (mini)" {
		t.Fatalf("git reads user.name %q: %v", out, err)
	}
	ws := t.TempDir()
	if err := sess.Run(context.Background(), RunSpec{Workspace: ws, Command: []string{"claude", "-p"}, Stdin: strings.NewReader("prompt")}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.Agents[0].Mounts, Mount{Source: hooks, Target: hooks, ReadOnly: true}) {
		t.Fatalf("mounts = %+v, want the hooks %s read-only", f.Agents[0].Mounts, hooks)
	}
}

func TestGitIdentityFromEnv(t *testing.T) {
	got := GitIdentityFromEnv([]string{"GH_TOKEN=x", "GIT_AUTHOR_NAME=Mesh rebase (mini)", "GIT_AUTHOR_EMAIL=mesh-rebase+mini@runsheet.dev"})
	if got != (GitIdentity{Name: "Mesh rebase (mini)", Email: "mesh-rebase+mini@runsheet.dev"}) {
		t.Fatalf("got %+v", got)
	}
}

// Probe answers nil on a healthy runtime: its version, then a throwaway
// pod labelled ProbeLabel, created and removed.
func TestProbeOnAHealthyRuntime(t *testing.T) {
	f := &Fake{}
	sb := testSandbox(t, f)
	if err := sb.Probe(context.Background()); err != nil {
		t.Fatalf("Probe = %v", err)
	}
	if _, ok := f.Called("version"); !ok {
		t.Fatal("Probe did not ask the runtime's version")
	}
	if len(f.Pods) != 1 || f.Pods[0].Labels[ProbeLabel] != "1" {
		t.Fatalf("Pods = %+v, want one labelled %s", f.Pods, ProbeLabel)
	}
	if len(f.Removed) != 1 || f.Removed[0] != f.Pods[0].Name {
		t.Fatalf("Removed = %v, want the probe's own pod removed", f.Removed)
	}
}

// Probe answers a *RuntimeError when the runtime cannot even start a
// version call or a pod — a Fake with VersionErr or CreatePodErr set,
// as a node without podman or a poisoned rootless store does
// (docs/adr/adr-030's amendment, the grapevine incident 2026-10-15/16).
// A CreatePodErr that is already a *RuntimeError is answered as is.
func TestProbeOnABrokenRuntime(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    *Fake
	}{
		{"no runtime at all", &Fake{VersionErr: errors.New(`exec: "podman": executable file not found in $PATH`)}},
		{"storage broken", &Fake{CreatePodErr: errors.New("Error: error opening storage: layer not known")}},
		{"already a RuntimeError", &Fake{CreatePodErr: &RuntimeError{Op: "pod create", Err: errors.New(`Error: cannot set up namespace using "/usr/bin/newuidmap": exit status 1`)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := testSandbox(t, tc.f)
			err := sb.Probe(context.Background())
			var re *RuntimeError
			if !errors.As(err, &re) {
				t.Fatalf("Probe = %v, want a *RuntimeError", err)
			}
			if got := validate.Kinded(err); got != validate.RuntimeBroken {
				t.Fatalf("Kinded(Probe err) = %q, want %q", got, validate.RuntimeBroken)
			}
		})
	}
}

// A *RuntimeError's reason for the platform opens "runtime_broken: ",
// so failWorkItem and releaseWorkItem read it as an environment failure
// like any other kind (docs/adr/adr-030's amendment).
func TestRuntimeErrorReasonOpensWithItsKind(t *testing.T) {
	err := &RuntimeError{Op: "pod create", Err: errors.New("Error: error opening storage: layer not known")}
	want := "runtime_broken: sandbox: runtime: pod create: Error: error opening storage: layer not known"
	if got := validate.Reason(err); got != want {
		t.Fatalf("Reason(err) = %q, want %q", got, want)
	}
	if !slices.Contains(validate.EnvironmentKinds(), validate.RuntimeBroken) {
		t.Fatal("validate.EnvironmentKinds() is missing runtime_broken")
	}
}
