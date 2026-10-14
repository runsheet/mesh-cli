package sandbox

import (
	"math"
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
