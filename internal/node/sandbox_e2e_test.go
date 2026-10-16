package node

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// The e2e's two repositories (issue #152): runsheet's own test workflow,
// whose test job's services: is Postgres, and a fixture declaring Redis
// in .mesh/services.yml.
const (
	fixtureRunsheetPostgres = "testdata/runsheet-postgres"
	fixtureRedisServices    = "testdata/redis-services"
)

// Without podman: runsheet's workflow resolves to its Postgres, not
// required (a workflow's never are) and test-tuned; the Redis fixture's
// services.yml to its Redis, required. These are what the e2e starts.
func TestE2EFixturesResolve(t *testing.T) {
	pg, err := sandbox.Resolve(fixtureRunsheetPostgres, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []sandbox.Service{{
		Name: "postgres", Image: "postgres:17-alpine", Registry: "docker.io",
		Env: map[string]string{
			"POSTGRES_PASSWORD": "mysecret",
			"PGDATA":            sandbox.PostgresDefaultPGDATA, "POSTGRES_INITDB_ARGS": sandbox.PostgresDefaultInitdbArgs,
		},
		Ports:   []string{"5432:5432"},
		Command: sandbox.PostgresTuning, Tuned: true,
		Health:   &sandbox.HealthCheck{Cmd: "pg_isready -U postgres", Interval: 5 * time.Second, Timeout: 5 * time.Second, Retries: 10},
		Required: false, From: ".github/workflows/test.yml",
		Notes: []string{"defaulted PGDATA, POSTGRES_INITDB_ARGS so the data directory is writable under keep-id"},
	}}
	if pg.Source != sandbox.SourceWorkflow || !reflect.DeepEqual(pg.Services, want) {
		t.Errorf("runsheet-postgres: %s %+v\nwant %s %+v", pg.Source, pg.Services, sandbox.SourceWorkflow, want)
	}

	redis, err := sandbox.Resolve(fixtureRedisServices, nil)
	if err != nil {
		t.Fatal(err)
	}
	want = []sandbox.Service{{
		Name: "redis", Image: "redis:7-alpine", Registry: "docker.io",
		Env:      map[string]string{},
		Ports:    []string{"6379:6379"},
		Health:   &sandbox.HealthCheck{Cmd: "redis-cli ping", Interval: time.Second, Timeout: 5 * time.Second, Retries: 30},
		Required: true, From: sandbox.ServicesFile,
	}}
	if redis.Source != sandbox.SourceFile || !reflect.DeepEqual(redis.Services, want) {
		t.Errorf("redis-services: %s %+v\nwant %s %+v", redis.Source, redis.Services, sandbox.SourceFile, want)
	}
}

// stubAgent stands in for claude in the pod: it reads its prompt, probes
// its service on 127.0.0.1 from inside the agent's container — Postgres
// by its SSLRequest ('N' or 'S' back), Redis by PING (+PONG) — appends
// "<kind> OK|FAIL <addr>" to .mesh-e2e-probe in its workspace, and ends
// as claude's stream-json does, with the evaluator's verdict line.
func stubAgent(service string, port int) string {
	var probe string
	switch service {
	case "redis":
		probe = `printf 'PING\r\n' >&3; IFS= read -r -t 5 reply <&3; [ "${reply%$'\r'}" = "+PONG" ] && probe=OK`
	default:
		probe = `printf '\x00\x00\x00\x08\x04\xd2\x16\x2f' >&3; IFS= read -r -n 1 -t 5 reply <&3; case "$reply" in N|S) probe=OK;; esac`
	}
	return fmt.Sprintf(`#!/bin/bash
cat >/dev/null
probe=FAIL
if exec 3<>/dev/tcp/127.0.0.1/%[1]d; then
  %[2]s
fi
echo "$MESH_SESSION_KIND $probe 127.0.0.1:%[1]d" >> "$PWD/.mesh-e2e-probe"
verdict=FAIL; [ "$probe" = OK ] && verdict=PASS
services=$(tr '\t\n' '  ' 2>/dev/null < "$HOME/.mesh-services")
echo '{"type":"system","subtype":"init","session_id":"e2e"}'
printf '{"type":"result","subtype":"success","is_error":false,"session_id":"e2e","total_cost_usd":0,"result":"%%s %%s 127.0.0.1:%[1]d; services: %%s\\nVERDICT: %%s"}\n' "$MESH_SESSION_KIND" "$probe" "$services" "$verdict"
`, port, probe)
}

// TestSandboxedSessionsE2E drives the node's own sandboxed session path
// on podman — Runner.RunItem for a build, Evaluator.Evaluate for its
// evaluation, each through OpenSessionPod and the ClaudeCode harness
// with a stub agent: the checkout's services resolved, started as
// sidecars, health-checked, the agent run in the pod, the pod removed.
// Once for runsheet's Postgres (its test workflow's services:), once for
// the Redis fixture (.mesh/services.yml). The agent must reach
// 127.0.0.1:5432 and :6379 from its container, and no pod or container
// of the session is left after. It needs podman and the node image
// (MESH_SANDBOX_IMAGE, else mesh-node at dev), so it runs only with
// MESH_SANDBOX_E2E=1.
func TestSandboxedSessionsE2E(t *testing.T) {
	if os.Getenv("MESH_SANDBOX_E2E") != "1" {
		t.Skip("MESH_SANDBOX_E2E=1 runs real pods")
	}
	image := os.Getenv("MESH_SANDBOX_IMAGE")
	if image == "" {
		image = sandbox.DefaultImage("dev")
	}
	rt := &sandbox.Podman{}
	if _, err := rt.Version(context.Background()); err != nil {
		t.Fatalf("podman: %v", err)
	}
	for _, tc := range []struct {
		name, fixture, service string
		port                   int
	}{
		{"runsheet-postgres", fixtureRunsheetPostgres, "postgres", 5432},
		{"redis-services", fixtureRedisServices, "redis", 6379},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			slug := fmt.Sprintf("e2e-%s-%d", tc.service, time.Now().UnixNano()%1e6)
			sb := &sandbox.Sandbox{Runtime: rt, Config: SandboxConfig{Image: image}.Settings("dev"), TempDir: t.TempDir()}
			sb.Config.ServiceTimeout = 3 * time.Minute
			f := seed(t)
			f.Goals[goalID]["slug"] = slug

			// the build
			h := &harness.ClaudeCode{Sandbox: sb, Stderr: os.Stderr}
			r, ws, item, _ := runnerOnFake(t, f, h, nil)
			r.Sandbox, r.Declines = sb, &Declines{}
			dir := filepath.Join(ws.root, slug, "1")
			copyFixture(t, tc.fixture, dir)
			h.SandboxBinary = filepath.Join(dir, ".mesh-e2e-agent")
			if err := os.WriteFile(h.SandboxBinary, []byte(stubAgent(tc.service, tc.port)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := r.RunItem(ctx, item); err != nil {
				t.Fatalf("build: %v", err)
			}
			if rel := f.CallsNamed("releaseWorkItem"); len(rel) != 0 {
				t.Fatalf("build released: %+v", rel)
			}
			assertProbe(t, dir, "build", tc.port)
			assertNoPods(t, "mesh-"+slug+"-")

			// its evaluation, on the same checkout
			f.WorkItems[item1]["status"], f.WorkItems[item1]["branch"] = "evaluating", "mesh/"+slug+"-1"
			item.Branch = "mesh/" + slug + "-1"
			ev := &Evaluator{Credentials: testCreds(gateway.New(f.URL(), "rs_test")), GW: gateway.New(f.URL(), "rs_test"), Harness: h, WS: ws, Scopes: r.Scopes, WorkRoot: ws.root, Sandbox: sb, Declines: &Declines{}, Log: log.New(os.Stderr, "", 0)}
			if err := ev.Evaluate(ctx, item); err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			calls := f.CallsNamed("evaluateWorkItem")
			if len(calls) != 1 || calls[0].Input["passed"] != true {
				t.Fatalf("evaluation: %+v", calls)
			}
			assertProbe(t, dir, "evaluate", tc.port)
			assertNoPods(t, "mesh-"+slug+"-")
			if left, _ := os.ReadDir(sb.TempDir); len(left) != 0 {
				t.Fatalf("session HOMEs left: %v", left)
			}
		})
	}
}

// TestEnsureImageE2E makes the e2e's node image present the way a node
// does before its drain (SandboxConfig.EnsureImage, mesh-specs ADR 007):
// MESH_SANDBOX_ENSURE=build builds MESH_SANDBOX_IMAGE (else mesh-node at
// dev) from the embedded Containerfile — `sandbox.image: build` — and
// =pull pulls it. Like a node's update it then removes every other
// mesh-node tag but that of MESH_SANDBOX_KEEP_VERSION, the running
// node's version (`none` keeps no other), so it runs only when scripts/e2e-sandbox.sh asks.
func TestEnsureImageE2E(t *testing.T) {
	how := os.Getenv("MESH_SANDBOX_ENSURE")
	if os.Getenv("MESH_SANDBOX_E2E") != "1" || how == "" {
		t.Skip("MESH_SANDBOX_E2E=1 MESH_SANDBOX_ENSURE=build|pull builds or pulls the node image")
	}
	cfg := SandboxConfig{Mode: SandboxOn, Image: os.Getenv("MESH_SANDBOX_IMAGE")}
	switch how {
	case "build":
		if cfg.Image != "" && cfg.Image != sandbox.DefaultImage("dev") {
			t.Fatalf("MESH_SANDBOX_ENSURE=build builds %s; MESH_SANDBOX_IMAGE=%s is pulled", sandbox.DefaultImage("dev"), cfg.Image)
		}
		cfg.Image = "build"
	case "pull":
	default:
		t.Fatalf("MESH_SANDBOX_ENSURE=%s: want build or pull", how)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	keep := os.Getenv("MESH_SANDBOX_KEEP_VERSION")
	switch keep {
	case "":
		t.Fatal("MESH_SANDBOX_KEEP_VERSION: the running node's version (`mesh --version`), whose image is kept, or none")
	case "none":
		keep = ""
	}
	img, err := cfg.EnsureImage(ctx, sandbox.ExecRunner, "dev", keep, nil)
	if err != nil {
		t.Fatal(err)
	}
	if img.Skipped != "" {
		t.Fatalf("skipped: %s", img.Skipped)
	}
	t.Logf("node image %s (built %v); removed %v; kept %v", img.Ref, img.Built, img.Removed, img.Kept)
}

// copyFixture copies a testdata repository into the item's checkout.
func copyFixture(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(to, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(to, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// assertProbe: the stub agent of kind reached 127.0.0.1:port from its
// container.
func assertProbe(t *testing.T, dir, kind string, port int) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, ".mesh-e2e-probe"))
	if err != nil {
		t.Fatalf("%s: the agent did not run: %v", kind, err)
	}
	if want := fmt.Sprintf("%s OK 127.0.0.1:%d\n", kind, port); !strings.Contains(string(b), want) {
		t.Fatalf("%s: from the agent's container: want %q, got\n%s", kind, want, b)
	}
}

// assertNoPods: podman has no pod and no container, sidecars and the
// network's init container included, whose name starts with prefix.
func assertNoPods(t *testing.T, prefix string) {
	t.Helper()
	for _, args := range [][]string{
		{"pod", "ls", "--format", "{{.Name}}"},
		{"ps", "-a", "--format", "{{.Names}} {{.PodName}}"},
	} {
		out, err := exec.Command("podman", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("podman %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		for _, line := range strings.Split(string(out), "\n") {
			for _, name := range strings.Fields(line) {
				if strings.HasPrefix(name, prefix) {
					t.Errorf("left after the session (podman %s): %s", args[0], line)
				}
			}
		}
	}
}
