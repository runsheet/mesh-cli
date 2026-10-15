package sandbox

import (
	"context"
	"errors"
	"io"
	"math"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// LimitEnv ceils a fractional CPU limit, floors at 1, and is empty for
// no limit, so Go, OpenMP and make fan out to the container's share of
// the host rather than its full core count.
func TestLimitEnv(t *testing.T) {
	for _, tc := range []struct {
		name string
		l    Limits
		want []string
	}{
		{"two cpus", Limits{CPUs: 2}, []string{"GOMAXPROCS=2", "OMP_NUM_THREADS=2", "MAKEFLAGS=-j2"}},
		{"fractional rounds up", Limits{CPUs: 1.5}, []string{"GOMAXPROCS=2", "OMP_NUM_THREADS=2", "MAKEFLAGS=-j2"}},
		{"no limit", Limits{}, nil},
	} {
		if got := LimitEnv(tc.l); !slices.Equal(got, tc.want) {
			t.Errorf("%s: LimitEnv(%+v) = %v, want %v", tc.name, tc.l, got, tc.want)
		}
	}
}

// agentArgs (through AgentRunArgs) and SidecarRunArgs pass the derived
// env by name, `--env NAME`, its value in podman's own environment
// (AgentProcessEnv), and keep an explicitly set value over the derived
// one.
func TestLimitEnvInRunArgs(t *testing.T) {
	pod := Pod{Name: "p"}
	spec := AgentSpec{Name: "a", Image: "img", Limits: Limits{CPUs: 2}}
	args := AgentRunArgs(pod, spec)
	for _, name := range []string{"GOMAXPROCS", "OMP_NUM_THREADS", "MAKEFLAGS"} {
		if !slices.Contains(flagValues(args, "--env"), name) {
			t.Fatalf("AgentRunArgs = %v, missing --env %s", args, name)
		}
	}
	if joined := strings.Join(args, " "); strings.Contains(joined, "=2") || strings.Contains(joined, "=-j2") {
		t.Fatalf("AgentRunArgs = %v: a limit value is on the command line", args)
	}
	penv := AgentProcessEnv(nil, spec)
	for _, want := range []string{"GOMAXPROCS=2", "OMP_NUM_THREADS=2", "MAKEFLAGS=-j2"} {
		if !slices.Contains(penv, want) {
			t.Fatalf("AgentProcessEnv = %v, missing %s", penv, want)
		}
	}

	sargs := SidecarRunArgs(pod, SidecarSpec{Name: "s", Image: "img", Limits: Limits{CPUs: 2}})
	if !slices.Contains(flagValues(sargs, "--env"), "GOMAXPROCS") || strings.Contains(strings.Join(sargs, " "), "GOMAXPROCS=") {
		t.Fatalf("SidecarRunArgs = %v, want a bare --env GOMAXPROCS", sargs)
	}

	// an explicit GOMAXPROCS in the spec's env wins over the derived one:
	// one bare `--env GOMAXPROCS`, podman's environment carrying 8.
	explicit := AgentSpec{Name: "a", Image: "img", Limits: Limits{CPUs: 2}, Env: []string{"GOMAXPROCS=8"}}
	eargs := AgentRunArgs(pod, explicit)
	n := 0
	for _, v := range flagValues(eargs, "--env") {
		if v == "GOMAXPROCS" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("args = %v: want GOMAXPROCS passed once", eargs)
	}
	epenv := AgentProcessEnv(nil, explicit)
	if slices.Contains(epenv, "GOMAXPROCS=2") || !slices.Contains(epenv, "GOMAXPROCS=8") {
		t.Fatalf("podman env = %v: the derived GOMAXPROCS=2 overrode the explicit one", epenv)
	}
}

// A session's agent is given every variable by name: for a job carrying
// CODEX_HOME, the Codex key, the account, a tool secret, the item token
// and a variable no list allows, each --env of AgentRunArgs is a bare
// name from BaseEnv, AllowedEnv, MESH_TOOL_* or LimitEnv — the limit
// variables included — the disallowed one absent, and the values in
// podman's own environment — HOME and PATH as much as any: the names
// are exactly the session's whole env (Session.Env, the limit variables
// added), with no --env-file. The job's CODEX_HOME is the session's
// CodexHome, under its HOME, never the host path.
func TestAgentEnvIsPassedByName(t *testing.T) {
	f := &Fake{}
	jobEnv := []string{
		"CODEX_HOME=/host/x", "OPENAI_API_KEY=sk-codex", "MESH_ACCOUNT=chatgpt-pro",
		"MESH_TOOL_FOO=foo-secret", "GH_TOKEN=ghs_item", "NODE_SECRET=node-only",
	}
	sess, spec, _ := runOnce(t, f, jobEnv)
	defer sess.Close()
	if spec.Limits.CPUs <= 0 {
		t.Fatalf("the test sandbox sets no --cpus: %+v", spec.Limits)
	}

	allowed := append(append([]string{}, BaseEnv...), AllowedEnv...)
	for _, kv := range LimitEnv(spec.Limits) {
		name, _, _ := strings.Cut(kv, "=")
		allowed = append(allowed, name)
	}
	args := AgentRunArgs(Pod{Name: "p"}, spec)
	names := flagValues(args, "--env")
	for _, name := range names {
		if strings.Contains(name, "=") {
			t.Errorf("--env %s: a value is on the command line", name)
		}
		if !slices.Contains(allowed, name) && !strings.HasPrefix(name, ToolEnvPrefix) {
			t.Errorf("--env %s is not in BaseEnv, AllowedEnv, MESH_TOOL_* or LimitEnv", name)
		}
	}
	for _, want := range []string{"CODEX_HOME", "OPENAI_API_KEY", "MESH_ACCOUNT", "MESH_TOOL_FOO", "GH_TOKEN", "GOMAXPROCS", "OMP_NUM_THREADS", "MAKEFLAGS", "MESH_ITEM", "MESH_SESSION_KIND"} {
		if !slices.Contains(names, want) {
			t.Errorf("--env %s missing: %v", want, names)
		}
	}
	if slices.Contains(names, "NODE_SECRET") || strings.Contains(strings.Join(args, " "), "node-only") {
		t.Errorf("NODE_SECRET reached the pod: %v", args)
	}
	for _, a := range args {
		for _, secret := range []string{"sk-codex", "foo-secret", "ghs_item", "/host/x"} {
			if strings.Contains(a, secret) {
				t.Errorf("%q is on podman's command line: %v", secret, args)
			}
		}
	}
	if slices.Contains(args, "--env-file") {
		t.Errorf("--env-file in %v: every variable is passed by name", args)
	}
	var whole []string
	for _, kv := range ContainerEnv(spec.Limits, spec.Env) {
		name, _, _ := strings.Cut(kv, "=")
		whole = append(whole, name)
	}
	if !slices.Equal(names, whole) || !slices.Contains(names, "HOME") || !slices.Contains(names, "PATH") {
		t.Errorf("--env names = %v, want the session's whole env by name %v", names, whole)
	}

	penv := AgentProcessEnv([]string{"HOME=/home/node", "PATH=/usr/bin"}, spec)
	for _, kv := range ContainerEnv(spec.Limits, spec.Env) {
		name, want, _ := strings.Cut(kv, "=")
		if got, _ := lastValue(penv, name); got != want {
			t.Errorf("podman env %s = %q, want the session's %q for --env %s", name, got, want, name)
		}
	}
	if v, _ := lastValue(penv, "XDG_DATA_HOME"); v != "/home/node/.local/share" {
		t.Errorf("podman env XDG_DATA_HOME = %q: podman's store must stay the node's", v)
	}
	for _, want := range []string{"OPENAI_API_KEY=sk-codex", "MESH_TOOL_FOO=foo-secret", "GH_TOKEN=ghs_item", "GOMAXPROCS=" + strconv.Itoa(int(math.Ceil(spec.Limits.CPUs)))} {
		if !slices.Contains(penv, want) {
			t.Errorf("podman env lacks %s: %v", want, penv)
		}
	}

	codexHome, ok := lastValue(spec.Env, "CODEX_HOME")
	if !ok || codexHome != sess.CodexHome() {
		t.Fatalf("CODEX_HOME = %q, want the session's %q", codexHome, sess.CodexHome())
	}
	if codexHome != filepath.Join(sess.Home, ".codex") || !strings.HasPrefix(codexHome, sess.Home+string(filepath.Separator)) {
		t.Fatalf("CodexHome %q is not under the session HOME %q", codexHome, sess.Home)
	}
	if v, _ := lastValue(penv, "CODEX_HOME"); v != sess.CodexHome() {
		t.Fatalf("podman env CODEX_HOME = %q, want %q", v, sess.CodexHome())
	}
}

// CreatePod wraps a `pod create` refused for the rootless store or the
// user namespace as a *RuntimeError, never a bare error — the
// classifier (validate.IsRuntimeBroken) reads the same words podman
// wrote to stderr that p.output already folded into err.
func TestCreatePodWrapsARuntimeBrokenFailure(t *testing.T) {
	p := &Podman{Exec: func(_ context.Context, args, _ []string, _ io.Reader, _, stderr io.Writer) error {
		if len(args) > 1 && args[0] == "pod" && args[1] == "create" {
			_, _ = io.WriteString(stderr, "Error: database configuration mismatch: expected mount_program=, found mount_program=/usr/bin/fuse-overlayfs")
			return errors.New("exit status 125")
		}
		return nil
	}}
	_, err := p.CreatePod(context.Background(), PodSpec{Name: "p", Image: "img"})
	var re *RuntimeError
	if !errors.As(err, &re) || re.Op != "pod create" {
		t.Fatalf("CreatePod err = %v, want a *RuntimeError naming pod create", err)
	}
}

// CreatePod's network-rules init container is the same: refused for the
// runtime, its failure is a *RuntimeError naming "network rules", not
// the plain-wrapped error an unrelated nft failure would still get.
func TestCreatePodWrapsARuntimeBrokenNetworkRulesFailure(t *testing.T) {
	p := &Podman{Exec: func(_ context.Context, args, _ []string, _ io.Reader, _, stderr io.Writer) error {
		switch {
		case len(args) > 1 && args[0] == "pod" && args[1] == "create":
			return nil
		case len(args) > 0 && args[0] == "run":
			_, _ = io.WriteString(stderr, "Error: creating temporary storage: permission denied")
			return errors.New("exit status 125")
		}
		return nil
	}}
	_, err := p.CreatePod(context.Background(), PodSpec{Name: "p", Image: "img"})
	var re *RuntimeError
	if !errors.As(err, &re) || re.Op != "network rules" {
		t.Fatalf("CreatePod err = %v, want a *RuntimeError naming network rules", err)
	}
}

// An ordinary nft failure in the network rules — no runtime words — is
// still the unwrapped "network rules: …" error, not a *RuntimeError:
// only podman's own storage/namespace refusals are.
func TestCreatePodLeavesAnOrdinaryNetworkFailureUnwrapped(t *testing.T) {
	p := &Podman{Exec: func(_ context.Context, args, _ []string, _ io.Reader, _, stderr io.Writer) error {
		switch {
		case len(args) > 1 && args[0] == "pod" && args[1] == "create":
			return nil
		case len(args) > 0 && args[0] == "run":
			_, _ = io.WriteString(stderr, "Error: nft: syntax error")
			return errors.New("exit status 1")
		}
		return nil
	}}
	_, err := p.CreatePod(context.Background(), PodSpec{Name: "p", Image: "img"})
	var re *RuntimeError
	if errors.As(err, &re) {
		t.Fatalf("CreatePod err = %v, want no *RuntimeError for an ordinary nft failure", err)
	}
	if err == nil || !strings.HasPrefix(err.Error(), "network rules: ") {
		t.Fatalf("CreatePod err = %v, want it to still open \"network rules: \"", err)
	}
}

// RunAgent wraps a container create refused for the runtime's storage
// or namespace as a *RuntimeError, not the agent's own exit — when the
// failure comes back as podman's own exit, 125 (podmanExitFailedToRun),
// the one podman ends with when it never ran the command at all.
func TestRunAgentWrapsARuntimeBrokenFailure(t *testing.T) {
	p := &Podman{Exec: func(_ context.Context, _, _ []string, _ io.Reader, _, stderr io.Writer) error {
		_, _ = io.WriteString(stderr, "Error: error opening storage: layer not known")
		return exec.Command("sh", "-c", "exit 125").Run()
	}}
	_, err := p.RunAgent(context.Background(), Pod{Name: "p"}, AgentSpec{Name: "a", Image: "img"})
	var re *RuntimeError
	if !errors.As(err, &re) || re.Op != "agent run" {
		t.Fatalf("RunAgent err = %v, want a *RuntimeError naming agent run", err)
	}
}

// A podman that does not run at all — no *exec.ExitError, just podman
// itself never starting — is a *RuntimeError too.
func TestRunAgentWrapsAPodmanThatDoesNotRunAtAll(t *testing.T) {
	p := &Podman{Exec: func(context.Context, []string, []string, io.Reader, io.Writer, io.Writer) error {
		return errors.New(`exec: "podman": executable file not found in $PATH`)
	}}
	_, err := p.RunAgent(context.Background(), Pod{Name: "p"}, AgentSpec{Name: "a", Image: "img"})
	var re *RuntimeError
	if !errors.As(err, &re) || re.Op != "agent run" {
		t.Fatalf("RunAgent err = %v, want a *RuntimeError naming agent run", err)
	}
}

// An agent that merely exits non-zero for its own reasons — no runtime
// words on stderr — is still an *exec.ExitError the caller classifies
// itself (classify, in sandbox.go), never a *RuntimeError.
func TestRunAgentLeavesAnOrdinaryExitUnwrapped(t *testing.T) {
	p := &Podman{Exec: func(_ context.Context, _, _ []string, _ io.Reader, _, stderr io.Writer) error {
		_, _ = io.WriteString(stderr, "--- FAIL: TestX")
		return exec.Command("false").Run()
	}}
	exit, err := p.RunAgent(context.Background(), Pod{Name: "p"}, AgentSpec{Name: "a", Image: "img"})
	var re *RuntimeError
	if errors.As(err, &re) {
		t.Fatalf("RunAgent err = %v, want no *RuntimeError for an ordinary exit", err)
	}
	if err != nil {
		t.Fatalf("RunAgent err = %v, want nil with the exit in Exit", err)
	}
	if exit.Code == 0 {
		t.Fatalf("exit = %+v, want a non-zero code", exit)
	}
}

// An agent's own command exiting 1 is never runtime_broken even when its
// stderr happens to contain podman's own runtime words — this is the
// agent's own failing test output, say, printing "error opening
// storage" as part of what it was testing, not podman refusing to run
// it. Only podman's own exit, 125, is read for those words at all; an
// agent's ordinary non-zero exit is read from its code alone and the
// error is nil, the exit code preserved, exactly as an unrelated
// failure would be (ADR-030's amendment: runtime_broken is never the
// agent's own command failing inside a pod that started fine).
func TestRunAgentExitOneWithRuntimeWordsIsStillAnOrdinaryExit(t *testing.T) {
	p := &Podman{Exec: func(_ context.Context, _, _ []string, _ io.Reader, _, stderr io.Writer) error {
		_, _ = io.WriteString(stderr, "--- FAIL: TestIsRuntimeBroken: error opening storage: layer not known")
		return exec.Command("false").Run()
	}}
	exit, err := p.RunAgent(context.Background(), Pod{Name: "p"}, AgentSpec{Name: "a", Image: "img"})
	var re *RuntimeError
	if errors.As(err, &re) {
		t.Fatalf("RunAgent err = %v, want no *RuntimeError for the agent's own exit 1", err)
	}
	if err != nil {
		t.Fatalf("RunAgent err = %v, want nil with the exit in Exit", err)
	}
	if exit.Code != 1 {
		t.Fatalf("exit = %+v, want Code 1", exit)
	}
}

// StartAgent wraps a refusal for the runtime's storage or namespace the
// same way RunAgent does.
func TestStartAgentWrapsARuntimeBrokenFailure(t *testing.T) {
	p := &Podman{Exec: func(_ context.Context, _, _ []string, _ io.Reader, _, stderr io.Writer) error {
		_, _ = io.WriteString(stderr, `Error: cannot set up namespace using "/usr/bin/newuidmap": exit status 1`)
		return errors.New("exit status 1")
	}}
	err := p.StartAgent(context.Background(), Pod{Name: "p"}, AgentSpec{Name: "a", Image: "img"})
	var re *RuntimeError
	if !errors.As(err, &re) || re.Op != "agent start" {
		t.Fatalf("StartAgent err = %v, want a *RuntimeError naming agent start", err)
	}
}
