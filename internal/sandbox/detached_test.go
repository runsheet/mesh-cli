package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func flagValues(args []string, flag string) []string {
	var v []string
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			v = append(v, args[i+1])
		}
	}
	return v
}

// A detached agent is `podman run -d` with AgentRunArgs' flags — the
// pod, the limits, --timeout, the mounts, the env by name — and no -i;
// its stream files' directory is mounted beside them, its command run
// by sh with the streams on those files, and no secret is on the
// command line. The pod it runs in keeps its keep-id mapping exactly.
func TestStartAgentArgs(t *testing.T) {
	ws, home, sessDir := "/work/goal/1", "/tmp/mesh-home-x", "/work/goal/1.session"
	spec := AgentSpec{
		Name: "mesh-p-0-agent", Image: "ghcr.io/runsheet/mesh-node:v1", Workdir: ws,
		Mounts:  []Mount{{Source: ws, Target: ws}, {Source: home, Target: home}, {Source: "/work/goal/1.hooks", Target: "/work/goal/1.hooks", ReadOnly: true}},
		Env:     []string{"HOME=" + home, "CLAUDE_CODE_OAUTH_TOKEN=sekrit-oauth", "GH_TOKEN=ghs_sekrit"},
		Command: []string{"claude", "-p", "--output-format", "stream-json"},
		Limits:  Limits{CPUs: 2, Memory: "6g", Pids: 4096}, TimeoutSeconds: 3630,
		StdinFile: sessDir + "/prompt", StdoutFile: sessDir + "/output.jsonl", StderrFile: sessDir + "/stderr.log",
	}
	pod := Pod{Name: "mesh-p-0"}
	args := StartAgentArgs(pod, spec)
	attached := AgentRunArgs(pod, spec)

	if args[0] != "run" || args[1] != "-d" {
		t.Fatalf("args open %v, want run -d", args[:2])
	}
	if slices.Contains(args, "-i") || slices.Contains(args, "--interactive") {
		t.Fatalf("detached args carry -i: %v", args)
	}
	joined := strings.Join(args, " ")
	for _, secret := range []string{"sekrit-oauth", "ghs_sekrit"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("a secret is on the command line: %v", args)
		}
	}
	for _, flag := range []string{"--pod", "--name", "--timeout", "--cpus", "--memory", "--memory-swap", "--pids-limit", "--workdir", "--cap-drop", "--security-opt", "--pull"} {
		if a, b := flagValues(args, flag), flagValues(attached, flag); !slices.Equal(a, b) {
			t.Errorf("%s = %v, AgentRunArgs has %v", flag, a, b)
		}
	}
	if got := flagValues(args, "--timeout"); !slices.Equal(got, []string{"3630"}) {
		t.Errorf("--timeout = %v", got)
	}
	if !slices.Contains(args, "--unsetenv-all") {
		t.Error("no --unsetenv-all")
	}
	if a, b := flagValues(args, "--env"), flagValues(attached, "--env"); !slices.Equal(a, b) {
		t.Errorf("--env = %v, AgentRunArgs has %v", a, b)
	}
	vols, want := flagValues(args, "--volume"), append(flagValues(attached, "--volume"), sessDir+":"+sessDir+":rw")
	if !slices.Equal(vols, want) {
		t.Errorf("--volume = %v, want %v", vols, want)
	}
	i := slices.Index(args, spec.Image)
	if i < 0 {
		t.Fatalf("no image in %v", args)
	}
	cmd := args[i+1:]
	wantCmd := append([]string{"/bin/sh", "-c",
		`exec "$@" <'` + sessDir + `/prompt' >'` + sessDir + `/output.jsonl' 2>'` + sessDir + `/stderr.log'`,
		"mesh-agent"}, spec.Command...)
	if !slices.Equal(cmd, wantCmd) {
		t.Fatalf("command = %q, want %q", cmd, wantCmd)
	}

	// The pod: --userns exactly as KeepIDUserns builds it, no other
	// userns flag (the dl360's podman shim rewrites it).
	pc := PodCreateArgs(PodSpec{Name: "mesh-p-0", UID: 1000, GID: 1000})
	if got := flagValues(pc, "--userns"); !slices.Equal(got, []string{"keep-id:uid=1000,gid=1000"}) || got[0] != KeepIDUserns(1000, 1000) {
		t.Fatalf("pod --userns = %v", got)
	}
	for _, a := range args {
		if strings.HasPrefix(a, "--userns") || strings.HasPrefix(a, "--uidmap") {
			t.Fatalf("the agent's container sets its own userns: %v", args)
		}
	}
}

// A stream file inside a mount already there mounts nothing more.
func TestStartAgentArgsReuseAMount(t *testing.T) {
	home := "/tmp/mesh-home-y"
	args := StartAgentArgs(Pod{Name: "p"}, AgentSpec{Name: "a", Image: "img",
		Mounts:     []Mount{{Source: home, Target: home}},
		StdoutFile: home + "/out/output.jsonl"})
	if got := flagValues(args, "--volume"); !slices.Equal(got, []string{home + ":" + home + ":rw"}) {
		t.Fatalf("--volume = %v", got)
	}
	if !strings.Contains(strings.Join(args, " "), `</dev/null`) {
		t.Fatalf("no stdin is /dev/null: %v", args)
	}
}

// The script the container runs takes its stdin from the prompt file,
// writes stdout and stderr to theirs, and exits as the command does.
func TestDetachedScriptRedirects(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "it's a dir")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	spec := AgentSpec{StdinFile: filepath.Join(dir, "prompt"), StdoutFile: filepath.Join(dir, "output.jsonl"), StderrFile: filepath.Join(dir, "stderr.log")}
	if err := os.WriteFile(spec.StdinFile, []byte("the prompt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", detachedScript(spec), "mesh-agent", "sh", "-c", "cat; echo oops >&2; exit 3")
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Fatalf("exit = %v, want 3", err)
	}
	if b, _ := os.ReadFile(spec.StdoutFile); string(b) != "the prompt\n" {
		t.Fatalf("stdout file = %q", b)
	}
	if b, _ := os.ReadFile(spec.StderrFile); string(b) != "oops\n" {
		t.Fatalf("stderr file = %q", b)
	}
}

// Podman.StartAgent runs StartAgentArgs with the secret in podman's own
// environment and the node's HOME kept for its store.
func TestPodmanStartAgentKeepsSecretsInTheEnv(t *testing.T) {
	t.Setenv("HOME", "/home/node")
	var gotArgs, gotEnv []string
	p := &Podman{Exec: func(_ context.Context, args, env []string, _ io.Reader, _, _ io.Writer) error {
		gotArgs, gotEnv = args, env
		return nil
	}}
	spec := AgentSpec{Name: "a", Image: "img", Env: []string{"HOME=/tmp/mesh-home-z", "GH_TOKEN=ghs_x"}, Command: []string{"claude"}}
	if err := p.StartAgent(context.Background(), Pod{Name: "p"}, spec); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(gotArgs, StartAgentArgs(Pod{Name: "p"}, spec)) {
		t.Fatalf("args = %v", gotArgs)
	}
	if !slices.Contains(gotEnv, "GH_TOKEN=ghs_x") || !slices.Contains(gotEnv, "HOME=/home/node") || slices.Contains(gotEnv, "HOME=/tmp/mesh-home-z") {
		t.Fatalf("podman env = %v", gotEnv)
	}
}

// AgentState reads podman inspect: running, exited with its code, and
// the OOM killer's; a name podman does not have is ErrNoAgent.
func TestParseAgentState(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		want      AgentState
	}{
		{"running", `[{"Name":"a","State":{"Status":"running","Running":true,"ExitCode":0,"OOMKilled":false}}]`,
			AgentState{Status: "running", Running: true}},
		{"exited", `[{"Name":"a","State":{"Status":"exited","Running":false,"ExitCode":3,"OOMKilled":false}}]`,
			AgentState{Status: "exited", Exit: Exit{Code: 3}}},
		{"oom", `[{"Name":"a","State":{"Status":"exited","Running":false,"ExitCode":137,"OOMKilled":true}}]`,
			AgentState{Status: "exited", Exit: Exit{Code: 137, OOMKilled: true}}},
	} {
		got, err := ParseAgentState(tc.out)
		if err != nil || got != tc.want {
			t.Errorf("%s: %+v, %v; want %+v", tc.name, got, err, tc.want)
		}
	}
	if _, err := ParseAgentState("[]"); !errors.Is(err, ErrNoAgent) {
		t.Errorf("empty inspect: %v, want ErrNoAgent", err)
	}

	var gotArgs []string
	p := &Podman{Exec: func(_ context.Context, args, _ []string, _ io.Reader, stdout, stderr io.Writer) error {
		gotArgs = args
		if args[len(args)-1] == "gone" {
			io.WriteString(stderr, "Error: no such container gone\n")
			return errors.New("exit status 125")
		}
		io.WriteString(stdout, `[{"State":{"Status":"exited","ExitCode":137,"OOMKilled":true}}]`)
		return nil
	}}
	st, err := p.AgentState(context.Background(), "mesh-p-0-agent")
	if err != nil || st.Exit != (Exit{Code: 137, OOMKilled: true}) {
		t.Fatalf("AgentState = %+v, %v", st, err)
	}
	if !slices.Equal(gotArgs, []string{"container", "inspect", "--format", "json", "mesh-p-0-agent"}) {
		t.Fatalf("inspect args = %v", gotArgs)
	}
	if _, err := p.AgentState(context.Background(), "gone"); !errors.Is(err, ErrNoAgent) {
		t.Fatalf("gone: %v, want ErrNoAgent", err)
	}
	if err := p.StopAgent(context.Background(), "gone"); err != nil {
		t.Fatalf("stopping a gone agent: %v", err)
	}
	if err := p.StopAgent(context.Background(), "mesh-p-0-agent"); err != nil || !slices.Equal(gotArgs, []string{"stop", "--time", "10", "mesh-p-0-agent"}) {
		t.Fatalf("stop: %v, args %v", err, gotArgs)
	}
}

func startDetached(t *testing.T, f *Fake) (*Sandbox, *Session, DetachedAgent) {
	t.Helper()
	sb := testSandbox(t, f)
	sess, err := sb.Open("ship-it/2", "build", GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	sessDir := ws + ".session"
	a, err := sess.StartDetached(context.Background(), DetachedSpec{
		Workspace: ws, Command: []string{"claude", "-p"}, Timeout: time.Hour,
		StdinFile: sessDir + "/prompt", StdoutFile: sessDir + "/output.jsonl", StderrFile: sessDir + "/stderr.log",
	})
	if err != nil {
		t.Fatal(err)
	}
	return sb, sess, a
}

// A detached session's Close removes neither its pod nor its HOME; the
// session Reattached from them removes both on Close.
func TestDetachedCloseLeavesThePodAndHome(t *testing.T) {
	f := &Fake{}
	sb, sess, a := startDetached(t, f)
	if a.Name != a.Pod.Name+"-agent" || len(f.Detached) != 1 || f.Detached[0].Name != a.Name {
		t.Fatalf("agent %+v, started %+v", a, f.Detached)
	}
	if got := f.Detached[0].TimeoutSeconds; got != int((time.Hour+TimeoutGrace)/time.Second) {
		t.Fatalf("timeout %d", got)
	}
	if !slices.Equal(sess.Pods(), []Pod{a.Pod}) {
		t.Fatalf("pods %v", sess.Pods())
	}
	sess.Detach()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	if len(f.Removed) != 0 {
		t.Fatalf("a detached Close removed %v", f.Removed)
	}
	if !exists(sess.Home) {
		t.Fatal("a detached Close removed the HOME")
	}
	if st, err := f.AgentState(context.Background(), a.Name); err != nil || !st.Running {
		t.Fatalf("agent after a detached Close: %+v, %v", st, err)
	}

	again := sb.Reattach(a.Pod, sess.Home, sess.Label)
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.Removed, []string{a.Pod.Name}) {
		t.Fatalf("a reattached Close removed %v, want the pod", f.Removed)
	}
	if exists(sess.Home) {
		t.Fatal("a reattached Close left the HOME")
	}
}

// An undetached session's Close removes its detached agent's pod as
// before.
func TestStartedSessionCloseRemovesThePod(t *testing.T) {
	f := &Fake{}
	_, sess, a := startDetached(t, f)
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.Removed, []string{a.Pod.Name}) || exists(sess.Home) {
		t.Fatalf("removed %v, home there %v", f.Removed, exists(sess.Home))
	}
}

// A start that fails removes the pod it made.
func TestStartDetachedFailureRemovesThePod(t *testing.T) {
	f := &Fake{StartErr: errors.New("image not known")}
	sb := testSandbox(t, f)
	sess, err := sb.Open("ship-it/2", "build", GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if _, err := sess.StartDetached(context.Background(), DetachedSpec{Workspace: t.TempDir()}); err == nil {
		t.Fatal("no error")
	}
	if len(f.Removed) != 1 || len(sess.Pods()) != 0 {
		t.Fatalf("removed %v, pods %v", f.Removed, sess.Pods())
	}
}

// A node start's sweep keeps the pod of an agent that runs detached and
// the HOME it mounts.
func TestSweepKeepsADetachedAgent(t *testing.T) {
	f := &Fake{}
	sb, sess, a := startDetached(t, f)
	sess.Detach()
	_ = sess.Close()
	res, err := Sweep(context.Background(), f, sb.TempDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Removed) != 0 || len(f.RemovedContainers) != 0 {
		t.Fatalf("sweep removed pods %v, containers %v", f.Removed, f.RemovedContainers)
	}
	if !exists(sess.Home) || res.HomesInUse != 1 || res.Homes != 0 {
		t.Fatalf("sweep %+v, home there %v", res, exists(sess.Home))
	}

	// once it has ended, the next sweep takes them
	if err := f.StopAgent(context.Background(), a.Name); err != nil {
		t.Fatal(err)
	}
	res, err = Sweep(context.Background(), f, sb.TempDir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Pods != 1 || res.Homes != 1 || exists(sess.Home) {
		t.Fatalf("sweep after the end %+v", res)
	}
}

// WaitAgent returns the exit once the agent stops running.
func TestWaitAgent(t *testing.T) {
	f := &Fake{}
	_, sess, a := startDetached(t, f)
	defer sess.Close()
	go func() {
		time.Sleep(20 * time.Millisecond)
		f.mu.Lock()
		f.States = map[string]AgentState{a.Name: {Status: "exited", Exit: Exit{Code: 137, OOMKilled: true}}}
		f.mu.Unlock()
	}()
	exit, err := WaitAgent(context.Background(), f, a.Name, 5*time.Millisecond)
	if err != nil || exit != (Exit{Code: 137, OOMKilled: true}) {
		t.Fatalf("WaitAgent = %+v, %v", exit, err)
	}
	var le *LimitError
	if err := sess.Ended(exit, time.Minute, time.Hour, ""); !errors.As(err, &le) || le.Limit != "memory" {
		t.Fatalf("Ended = %v", err)
	}
	if _, err := WaitAgent(context.Background(), f, "gone", time.Millisecond); !errors.Is(err, ErrNoAgent) {
		t.Fatalf("gone: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.mu.Lock()
	f.States = nil
	f.mu.Unlock()
	if _, err := WaitAgent(ctx, f, a.Name, time.Millisecond); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}
