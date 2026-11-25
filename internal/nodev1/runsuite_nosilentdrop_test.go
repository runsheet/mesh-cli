package nodev1

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/nodetools"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/scheduler"
)

// A call naming 13 suites either runs every one of them or refuses
// naming every suite it would not run: it is never the case that only
// a subset (here 11 of 13) runs silently with no error. This exercises
// the real lane-opening path (leasePod.suites, validate.Run, the
// node's lane sizing and services), not a stand-in runner: 11 of the
// 13 suites need no services and open trivially; the other two (b and
// c) ask for the repository's declared service, which is made to
// refuse only after the lease's own pod is already up, so exactly
// those two lanes fail to open while the rest succeed — the prior bug
// (validate.Run's cmp.Or kept only the first of several simultaneous
// lane-open failures, and run_suite's execute only refused when
// results was entirely empty) silently finished with b and c missing
// and no error at all.
func TestRunSuiteRunsEveryNamedSuiteOrRefuses(t *testing.T) {
	var yml strings.Builder
	yml.WriteString("suites:\n")
	var names []string
	for i := 0; i <= 10; i++ {
		name := fmt.Sprintf("s%d", i)
		names = append(names, name)
		fmt.Fprintf(&yml, "  - name: %s\n    kind: lint\n    group: %s\n    command: \"true\"\n", name, name)
	}
	for _, name := range []string{"b", "c"} {
		names = append(names, name)
		fmt.Fprintf(&yml, "  - name: %s\n    kind: lint\n    group: %s\n    services: true\n    command: \"true\"\n", name, name)
	}
	if len(names) != 13 {
		t.Fatalf("test setup names %d suites, want 13", len(names))
	}

	f := &sandbox.Fake{}
	rt := &Runtime{WorkRoot: t.TempDir(), Logf: t.Logf, Scheduler: scheduler.New(scheduler.Capacity{CPUs: 64, MemGB: 128, Slots: 30})}
	rt.Pods = &sandbox.Sandbox{Runtime: f, Logf: t.Logf, Config: sandbox.Config{
		Image: "mesh-node:test", Limits: sandbox.Limits{CPUs: 1, Memory: "1g"},
		Registries: sandbox.DefaultRegistries, ServiceTimeout: 5 * time.Second,
	}}
	ws := t.TempDir()
	if out, err := exec.Command("git", "-C", ws, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".runsheet"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".runsheet", "validate.yml"), []byte(yml.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".mesh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".mesh", "services.yml"), []byte("redis:\n  image: redis:7\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := &meshapi.Brief{JobID: "job-1", LeaseID: "lease-1", LeaseN: 1, Kind: "validate", Product: "runsheet", Tenant: "acme",
		NodeTools: []string{nodetools.RunSuiteTool}, EvidenceRequired: []string{nodetools.RunSuiteTool},
		Resources: meshapi.Resources{CPUs: 4, MemGB: 8}}

	ctx, cancel := context.WithCancel(context.Background())
	var closers []func() error
	t.Cleanup(func() {
		cancel()
		for _, c := range closers {
			c()
		}
	})
	lp, err := rt.openLeasePod(ctx, b, ws, nil, func(_ string, c func() error) { closers = append(closers, c) })
	if err != nil {
		t.Fatalf("opening the lease's own pod: %v", err)
	}

	// Only now, after the lease's own pod (and its own copy of redis)
	// is already up, does redis start refusing: b and c's lanes are
	// the only ones whose suites run services (services: true), so
	// only their lane-open calls hit this; the other 11 never ask for
	// it and open trivially.
	f.SidecarErr = map[string]error{"redis": errors.New("fake: redis refused")}

	laneCtx, end := context.WithCancelCause(ctx)
	t.Cleanup(func() { end(nil) })
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	mesh := &evidenceMesh{}
	rec := &nodetools.Recorder{Lease: nodetools.Lease{JobID: b.JobID, LeaseID: b.LeaseID, LeaseN: 1, Product: b.Product, Tenant: b.Tenant},
		Signer: &nodetools.Signer{NodeKey: key}, Mesh: mesh}
	suite := &nodetools.RunSuite{Workspace: ws, Recorder: rec, Run: watchSuites(lp.suites, end), Wait: 100 * time.Millisecond,
		Context: laneCtx, Began: time.Now(), Budget: rt.suiteBudget(b, ws)}

	tool := suite.Tool()
	decode := func(args map[string]any) (map[string]any, error) {
		t.Helper()
		text, err := tool.Call(context.Background(), args)
		if err != nil {
			return nil, err
		}
		var m map[string]any
		if jerr := json.Unmarshal([]byte(text), &m); jerr != nil {
			t.Fatalf("%q: %v", text, jerr)
		}
		return m, nil
	}

	nameArgs := make([]any, len(names))
	for i, n := range names {
		nameArgs[i] = n
	}
	checkRefusal := func(err error) {
		t.Helper()
		if !strings.Contains(err.Error(), "b") || !strings.Contains(err.Error(), "c") {
			t.Fatalf("refusal %q does not name both dropped suites b and c", err)
		}
	}

	ans, err := decode(map[string]any{"suites": nameArgs})
	if err != nil {
		checkRefusal(err)
		return // a hard refusal naming both b and c: nothing ran silently
	}
	for i := 0; ans["status"] == "running" && i < 200; i++ {
		ans, err = decode(map[string]any{"run_id": ans["run_id"]})
		if err != nil {
			checkRefusal(err)
			return
		}
	}
	if ans["status"] != "finished" {
		t.Fatalf("the run did not finish in time: %v", ans)
	}
	steps, _ := ans["steps"].([]any)
	got := map[string]bool{}
	for _, st := range steps {
		got[st.(map[string]any)["name"].(string)] = true
	}
	var missing []string
	for _, n := range names {
		if !got[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("the run answered %v with %v silently missing, and no error", ans, missing)
	}
}
