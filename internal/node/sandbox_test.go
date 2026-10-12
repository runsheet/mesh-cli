package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

func sandboxOf(t *testing.T, doc string) SandboxConfig {
	t.Helper()
	var c Config
	if err := yaml.Unmarshal([]byte(doc), &c); err != nil {
		t.Fatal(err)
	}
	return c.Sandbox
}

// flagValue is the value after flag in args, "" when absent.
func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// The agent's --cpus, --memory and --pids-limit come from node.yaml's
// sandbox block, and its defaults (2, 6g, 4096) when it names none.
func TestSandboxLimitsFromNodeYAML(t *testing.T) {
	for _, tc := range []struct {
		doc                string
		cpus, memory, pids string
		svcCPUs, svcMem    string
	}{
		{"slug: n\n", "2", "6g", "4096", "2", "4g"},
		{"slug: n\nsandbox:\n  mode: on\n", "2", "6g", "4096", "2", "4g"},
		{"slug: n\nsandbox:\n  cpus: 3.5\n  memory: 8g\n  pids: 2048\n  services_cpus: 1\n  services_memory: 2g\n", "3.5", "8g", "2048", "1", "2g"},
	} {
		s := sandboxOf(t, tc.doc).Settings("v1.2.3")
		args := sandbox.AgentRunArgs(sandbox.Pod{Name: "p"}, sandbox.AgentSpec{Name: "a", Image: s.Image, Limits: s.Limits})
		if got := [3]string{flagValue(args, "--cpus"), flagValue(args, "--memory"), flagValue(args, "--pids-limit")}; got != [3]string{tc.cpus, tc.memory, tc.pids} {
			t.Errorf("%q: agent limits = %v, want %s %s %s", tc.doc, got, tc.cpus, tc.memory, tc.pids)
		}
		if flagValue(args, "--memory-swap") != tc.memory {
			t.Errorf("%q: swap must equal memory: %v", tc.doc, args)
		}
		svc := sandbox.LimitArgs(s.ServicesLimits)
		if flagValue(svc, "--cpus") != tc.svcCPUs || flagValue(svc, "--memory") != tc.svcMem {
			t.Errorf("%q: services budget = %v", tc.doc, svc)
		}
		if s.Image != "us-central1-docker.pkg.dev/runsheet-prod/runsheet/mesh-node:v1.2.3" {
			t.Errorf("image = %s", s.Image)
		}
		if !reflect.DeepEqual(s.Registries, []string{"docker.io", "ghcr.io", "*.pkg.dev", "quay.io"}) {
			t.Errorf("registries = %v", s.Registries)
		}
	}
	s := sandboxOf(t, "slug: n\nsandbox:\n  image: localhost/mesh-node:mine\n  registries: [ghcr.io]\n").Settings("v1")
	if s.Image != "localhost/mesh-node:mine" || !reflect.DeepEqual(s.Registries, []string{"ghcr.io"}) {
		t.Fatalf("overrides: %+v", s)
	}
}

// `sandbox: off` and `sandbox: {mode: off}` read the same; a mode other
// than on or off is refused.
func TestSandboxModeParses(t *testing.T) {
	if m := sandboxOf(t, "sandbox: off\n").Mode; m != "off" {
		t.Fatalf("scalar: %q", m)
	}
	if m := sandboxOf(t, "sandbox:\n  mode: off\n").Mode; m != "off" {
		t.Fatalf("mapping: %q", m)
	}
	var c Config
	if err := yaml.Unmarshal([]byte("sandbox: maybe\n"), &c); err == nil {
		t.Fatal("sandbox: maybe must be refused")
	}
	out, err := yaml.Marshal(&Config{Slug: "n"})
	if err != nil || strings.Contains(string(out), "sandbox") {
		t.Fatalf("an unset sandbox is left out of node.yaml: %s %v", out, err)
	}
}

// The mode: off never asks the runtime; left out, it is on when the
// runtime answers and off (with a log line) when it does not; an
// explicit on with no runtime refuses to start rather than run
// unsandboxed.
func TestOpenSandboxModes(t *testing.T) {
	ctx := context.Background()
	gone := &sandbox.Fake{VersionErr: errors.New("exec: podman: not found")}
	var logged []string
	logf := func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }

	sb, info, err := OpenSandbox(ctx, SandboxConfig{Mode: "off"}, gone, "v1", logf)
	if err != nil || sb != nil || *info != (gateway.SandboxInfo{Mode: "off"}) || len(gone.Calls) != 0 {
		t.Fatalf("off: %v %+v %v calls=%v", sb, info, err, gone.Calls)
	}
	sb, info, err = OpenSandbox(ctx, SandboxConfig{}, gone, "v1", logf)
	if err != nil || sb != nil || info.Mode != "off" || len(logged) != 1 || !strings.Contains(logged[0], "sandbox: off") {
		t.Fatalf("unset without a runtime: %v %+v %v %q", sb, info, err, logged)
	}
	if _, _, err := OpenSandbox(ctx, SandboxConfig{Mode: "on"}, gone, "v1", logf); err == nil {
		t.Fatal("an explicit on without a runtime must refuse to start")
	}
	sb, info, err = OpenSandbox(ctx, SandboxConfig{}, &sandbox.Fake{}, "v1", logf)
	if err != nil || sb == nil || *info != (gateway.SandboxInfo{Mode: "on", Runtime: "fake 1.0", Image: "us-central1-docker.pkg.dev/runsheet-prod/runsheet/mesh-node:v1"}) {
		t.Fatalf("unset with a runtime: %v %+v %v", sb, info, err)
	}
}

// `sandbox: off` runs exactly today's exec path: the harness the node
// builds has no sandbox, and the claude it starts gets byte-for-byte
// the arguments it always did, in the workspace, on the node's env.
func TestSandboxOffRunsThePlainExecPath(t *testing.T) {
	sb, _, err := OpenSandbox(context.Background(), sandboxOf(t, "sandbox: off\n"), &sandbox.Fake{}, "v1", nil)
	if err != nil || sb != nil {
		t.Fatalf("off: %v %v", sb, err)
	}
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "claude")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > " + argsFile + "\npwd >> " + argsFile + "\n" +
		"echo '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"s\"}'\n" +
		"echo '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"ok\",\"session_id\":\"s\",\"total_cost_usd\":0.1}'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	h := &harness.ClaudeCode{Binary: bin, ExtraArgs: []string{"--dangerously-skip-permissions"}, Sandbox: sb, MeshBinary: "mesh"}
	ws := t.TempDir()
	job := harness.Job{Kind: harness.KindBuild, Workspace: ws, Prompt: "do it", Model: "sonnet", MaxBudgetUSD: 2.5,
		MCPServers: map[string]any{"mesh-context": map[string]any{"type": "http", "url": "https://gw/mcp"}}}
	res, err := h.Run(context.Background(), job, nil)
	if err != nil || !res.Success {
		t.Fatalf("run: %+v %v", res, err)
	}
	got, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"-p", "--output-format", "stream-json", "--verbose",
		"--model", "sonnet", "--max-budget-usd", "2.50",
		"--mcp-config", `{"mcpServers":{"mesh-context":{"type":"http","url":"https://gw/mcp"},"mesh-node":{"args":["session-tools"],"command":"mesh","type":"stdio"}}}`,
		"--allowedTools", "mcp__mesh-context__*,mcp__mesh-node__*",
		"--disallowedTools", "mcp__mesh-context__mesh_finish,mcp__mesh-context__mesh_fail",
		"--dangerously-skip-permissions",
	}, "\n") + "\n"
	wsReal, _ := filepath.EvalSymlinks(ws)
	if string(got) != want+wsReal+"\n" && string(got) != want+ws+"\n" {
		t.Fatalf("args =\n%s\nwant\n%s%s", got, want, ws)
	}
}

// A session its sandbox's limits killed is the node's condition: the
// item is released (not failed, the attempt not counted), the release
// and the heartbeat's reason name the limit and the item, and the node
// does not take the item again straight away.
func TestRunnerReleasesALimitKill(t *testing.T) {
	h := harnessFunc(func(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
		return harness.Result{}, fmt.Errorf("harness claude-code: %w", &sandbox.LimitError{Limit: "memory", Value: "6g"})
	})
	f := seed(t)
	r, _, item, row := runnerOnFake(t, f, h, nil)
	r.Declines = &Declines{}
	gw := gateway.New(f.URL(), "rs_test")
	_, _ = gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: r.Cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "n"})
	loop := &Loop{GW: gw, Cfg: r.Cfg, Scopes: scopesOf(r.Cfg), Interval: time.Hour, Declines: r.Declines,
		Sandbox: &gateway.SandboxInfo{Mode: "on", Runtime: "podman 5.2.1", Image: "us-central1-docker.pkg.dev/runsheet-prod/runsheet/mesh-node:v1"},
		Dispatch: func(context.Context, gateway.WorkItem) {
			t.Error("a released limit kill must not be taken again at once")
		}}
	r.Condition = loop.NoteCondition

	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatalf("a limit kill is not the item's error: %v", err)
	}
	if n := len(f.CallsNamed("failWorkItem")); n != 0 {
		t.Fatalf("failWorkItem called %d times", n)
	}
	rel := f.CallsNamed("releaseWorkItem")
	if len(rel) != 1 {
		t.Fatalf("releases: %+v", rel)
	}
	reason := fmt.Sprint(rel[0].Input["note"])
	for _, want := range []string{"memory", "6g", "ship-it/1"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("release reason %q must name %q", reason, want)
		}
	}
	if st := row(item1)["status"]; st == "failed" {
		t.Fatalf("row: %v", row(item1))
	}

	loop.Tick(context.Background())
	hb := f.CallsNamed("heartbeatNode")
	if len(hb) == 0 {
		t.Fatal("no heartbeat")
	}
	last := hb[len(hb)-1].Input
	if last["reason"] != reason {
		t.Fatalf("heartbeat reason = %#v, want %q", last["reason"], reason)
	}
	if sb, ok := last["sandbox"].(map[string]any); !ok || sb["mode"] != "on" || sb["runtime"] != "podman 5.2.1" {
		t.Fatalf("heartbeat sandbox = %#v", last["sandbox"])
	}
	if _, ok := last["status"]; ok {
		t.Fatalf("a limit kill leaves the node online: %#v", last)
	}
}

// A harness error that is not a limit still fails the item, as before.
func TestRunnerFailsAnOrdinaryExit(t *testing.T) {
	h := harnessFunc(func(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
		return harness.Result{}, fmt.Errorf("harness claude-code: %w", &sandbox.ExitError{Code: 1})
	})
	f := seed(t)
	r, _, item, row := runnerOnFake(t, f, h, nil)
	if err := r.RunItem(context.Background(), item); err == nil {
		t.Fatal("want the harness error")
	}
	if row(item1)["status"] != "failed" || len(f.CallsNamed("releaseWorkItem")) != 0 {
		t.Fatalf("row: %v", row(item1))
	}
}

// sandbox.service_timeout bounds the services' health checks: 120s
// unless node.yaml says otherwise, and it survives a save.
func TestSandboxServiceTimeout(t *testing.T) {
	if got := sandboxOf(t, "slug: n\n").Settings("v1").ServiceTimeout; got != 120*time.Second {
		t.Fatalf("default = %s", got)
	}
	c := sandboxOf(t, "slug: n\nsandbox:\n  service_timeout: 45s\n")
	if got := c.Settings("v1").ServiceTimeout; got != 45*time.Second {
		t.Fatalf("service_timeout = %s", got)
	}
	out, err := yaml.Marshal(&Config{Slug: "n", Sandbox: c})
	if err != nil || !strings.Contains(string(out), "service_timeout: 45s") {
		t.Fatalf("saved: %s %v", out, err)
	}
}

// imageRunner is the injected command runner of the image tests: podman
// answers or not, and every call is recorded; a build keeps the
// Containerfile it was given.
type imageRunner struct {
	calls    []string
	noPodman bool
	built    []byte
}

func (r *imageRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	if r.noPodman {
		return nil, errors.New(`exec: "podman": executable file not found in $PATH`)
	}
	if args[0] == "build" {
		for i, a := range args {
			if a == "--file" {
				r.built, _ = os.ReadFile(args[i+1])
			}
		}
	}
	return nil, nil
}

func (r *imageRunner) sub(s string) []string {
	var out []string
	for _, c := range r.calls {
		if strings.HasPrefix(c, "podman "+s+" ") {
			out = append(out, c)
		}
	}
	return out
}

// node.yaml's sandbox.image decides what an update ensures: empty pulls
// mesh-node:<the new version> and keeps the running one's; `build` builds
// that tag from images/node/Containerfile and pulls nothing; `sandbox:
// off` asks nothing; a mode left out without podman skips; `on` without
// podman is the error.
func TestSandboxEnsureImage(t *testing.T) {
	ctx := context.Background()

	r := &imageRunner{}
	img, err := sandboxOf(t, "sandbox: {}\n").EnsureImage(ctx, r.run, "v1.1.0", "v1.0.0")
	if err != nil || img.Ref != "us-central1-docker.pkg.dev/runsheet-prod/runsheet/mesh-node:v1.1.0" || !img.Pulled {
		t.Fatalf("default = %+v, %v", img, err)
	}
	if p := r.sub("pull"); len(p) != 1 || p[0] != "podman pull us-central1-docker.pkg.dev/runsheet-prod/runsheet/mesh-node:v1.1.0" {
		t.Fatalf("pulls = %q", p)
	}

	r = &imageRunner{}
	img, err = sandboxOf(t, "sandbox:\n  image: build\n").EnsureImage(ctx, r.run, "v1.1.0", "v1.0.0")
	if err != nil || img.Ref != "us-central1-docker.pkg.dev/runsheet-prod/runsheet/mesh-node:v1.1.0" || !img.Built {
		t.Fatalf("build = %+v, %v", img, err)
	}
	if len(r.sub("pull")) != 0 || len(r.sub("build")) != 1 {
		t.Fatalf("calls = %q; want one build, no pull", r.calls)
	}
	want, err := os.ReadFile("../../images/node/Containerfile")
	if err != nil {
		t.Fatal(err)
	}
	if string(r.built) != string(want) {
		t.Fatal("sandbox.image: build did not build images/node/Containerfile")
	}
	if got := sandboxOf(t, "sandbox:\n  image: build\n").Settings("v1.1.0").Image; got != "us-central1-docker.pkg.dev/runsheet-prod/runsheet/mesh-node:v1.1.0" {
		t.Fatalf("sessions of sandbox.image: build run %q", got)
	}

	r = &imageRunner{}
	img, err = sandboxOf(t, "sandbox:\n  image: registry.example.com/node:7\n").EnsureImage(ctx, r.run, "v1.1.0", "v1.0.0")
	if err != nil || img.Ref != "registry.example.com/node:7" {
		t.Fatalf("given = %+v, %v", img, err)
	}

	r = &imageRunner{}
	img, err = sandboxOf(t, "sandbox: off\n").EnsureImage(ctx, r.run, "v1.1.0", "v1.0.0")
	if err != nil || img.Skipped != "sandbox: off" || len(r.calls) != 0 {
		t.Fatalf("off = %+v, %v, calls %q; want nothing asked", img, err, r.calls)
	}

	r = &imageRunner{noPodman: true}
	img, err = sandboxOf(t, "sandbox: {}\n").EnsureImage(ctx, r.run, "v1.1.0", "v1.0.0")
	if err != nil || img.Skipped == "" || len(r.sub("pull")) != 0 {
		t.Fatalf("no podman = %+v, %v", img, err)
	}
	if _, err := sandboxOf(t, "sandbox: on\n").EnsureImage(ctx, r.run, "v1.1.0", "v1.0.0"); err == nil {
		t.Fatal("sandbox: on without podman ensured no image and no error")
	}
}

// The node fills its sandbox's stores: one pnpm store and one npm cache
// under ~/.mesh/store, beside ~/.mesh/work (mesh-specs ADR 017 §3).
func TestOpenSandboxNamesThePackageStores(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sb, _, err := OpenSandbox(context.Background(), SandboxConfig{}, &sandbox.Fake{}, "v1", nil)
	if err != nil || sb == nil {
		t.Fatalf("open: %v %v", sb, err)
	}
	want := sandbox.Stores{Pnpm: filepath.Join(home, ".mesh", "store", "pnpm"), Npm: filepath.Join(home, ".mesh", "store", "npm")}
	if sb.Config.Stores != want {
		t.Errorf("stores = %+v, want %+v", sb.Config.Stores, want)
	}
}
