package nodev1

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/nodetools"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/scheduler"
)

// shardRig is a sandboxed node's lease pod on the fake runtime, whose
// every run_suite container runs for shard (or until stopped), counting
// how many run at once, and the run_suite tool over it as work makes
// it: a validate lease, a Postgres among the brief's pods, its
// checkout's validate.yml declaring yml.
type shardRig struct {
	f       *sandbox.Fake
	rt      *Runtime
	lp      *leasePod
	mesh    *evidenceMesh
	suite   *nodetools.RunSuite
	mu      sync.Mutex
	running int
	peak    int
}

func newShardRig(t *testing.T, yml string, shard time.Duration, total scheduler.Capacity) *shardRig {
	t.Helper()
	g := &shardRig{f: &sandbox.Fake{}}
	g.f.Agent = func(ctx context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		if !strings.Contains(spec.Name, "-exec-") {
			return sandbox.Exit{}, nil
		}
		g.mu.Lock()
		g.running++
		g.peak = max(g.peak, g.running)
		g.mu.Unlock()
		defer func() { g.mu.Lock(); g.running--; g.mu.Unlock() }()
		select {
		case <-time.After(shard):
			return sandbox.Exit{}, nil
		case <-ctx.Done():
			return sandbox.Exit{Code: 137}, ctx.Err()
		}
	}
	g.rt = &Runtime{WorkRoot: t.TempDir(), Logf: t.Logf, Scheduler: scheduler.New(total)}
	g.rt.Pods = &sandbox.Sandbox{Runtime: g.f, Logf: t.Logf, Config: sandbox.Config{
		Image: "mesh-node:test", Limits: sandbox.Limits{CPUs: 2, Memory: "6g"},
		Registries: sandbox.DefaultRegistries, ServiceTimeout: 5 * time.Second,
	}}
	ws := t.TempDir()
	if out, err := exec.Command("git", "-C", ws, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".runsheet"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".runsheet", "validate.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &meshapi.Brief{JobID: "job-1", LeaseID: "lease-1", LeaseN: 1, Kind: "validate", Product: "runsheet", Tenant: "acme",
		NodeTools: []string{nodetools.RunSuiteTool}, EvidenceRequired: []string{nodetools.RunSuiteTool},
		Resources: meshapi.Resources{CPUs: 4, MemGB: 8},
		Pods:      []json.RawMessage{json.RawMessage(`{"name":"postgres","image":"docker.io/library/postgres:17-alpine"}`)}}
	ctx, cancel := context.WithCancel(context.Background())
	var closers []func() error
	t.Cleanup(func() {
		cancel()
		for _, c := range closers {
			c()
		}
	})
	lp, err := g.rt.openLeasePod(ctx, b, ws, nil, func(_ string, c func() error) { closers = append(closers, c) })
	if err != nil {
		t.Fatal(err)
	}
	g.lp = lp
	ctx, end := context.WithCancelCause(ctx)
	t.Cleanup(func() { end(nil) })
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	g.mesh = &evidenceMesh{}
	rec := &nodetools.Recorder{Lease: nodetools.Lease{JobID: b.JobID, LeaseID: b.LeaseID, LeaseN: 1, Product: b.Product, Tenant: b.Tenant},
		Signer: &nodetools.Signer{NodeKey: key}, Mesh: g.mesh}
	g.suite = &nodetools.RunSuite{Workspace: ws, Recorder: rec, Run: watchSuites(lp.suites, end), Wait: 100 * time.Millisecond,
		Context: ctx, Began: time.Now(), Budget: g.rt.suiteBudget(b, ws)}
	return g
}

// run calls run_suite with args and polls its handle until the run has
// finished, as a session would; it answers the first answer and the
// last.
func (g *shardRig) run(t *testing.T, args map[string]any) (first, last map[string]any) {
	t.Helper()
	tool := g.suite.Tool()
	decode := func(s string, err error) map[string]any {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		return m
	}
	first = decode(tool.Call(context.Background(), args))
	last = first
	for i := 0; last["status"] == "running" && i < 200; i++ {
		last = decode(tool.Call(context.Background(), map[string]any{"run_id": first["run_id"]}))
	}
	return first, last
}

// calls counts the fake runtime's calls that are call.
func (g *shardRig) calls(call string) int {
	n := 0
	for _, c := range slices.Clone(g.f.Calls) {
		if c == call {
			n++
		}
	}
	return n
}

// lanePods are the pods made beside the lease's for run_suite's lanes.
func (g *shardRig) lanePods() []string {
	var out []string
	for _, p := range g.f.PodSpecs() {
		if strings.Contains(p.Name, "-lane-") {
			out = append(out, p.Name)
		}
	}
	return out
}

const shardedYML = "budget: 80m\nsuites:\n  - name: integration\n    kind: integration\n    command: go test ./e2e\n    timeout: 60m\n    shards: 4\n"

// A sharded suite's shards run at once (mesh-specs gap 36): the first in
// the lease's own pod, each other in a pod of its own beside the
// lease's with its own Postgres, on the lease's resources held of the
// node's scheduler and given back when it ends, so the run takes about
// one shard's time, not four. A run longer than the call's wait (the
// MCP call's timeout, here 100ms) answers a handle the session polls.
func TestASuitesShardsRunAtOnceEachWithItsOwnServices(t *testing.T) {
	const shard = 600 * time.Millisecond
	g := newShardRig(t, shardedYML, shard, scheduler.Capacity{CPUs: 32, MemGB: 64, Slots: 3})
	start := time.Now()
	first, last := g.run(t, map[string]any{"suite": "integration"})
	wall := time.Since(start)
	if first["status"] != "running" || first["run_id"] == nil {
		t.Fatalf("a run past the call's wait answered %v, want its handle", first)
	}
	if last["status"] != "finished" || last["exit"] != float64(0) || last["evidence_id"] == nil {
		t.Fatalf("the run answered %v", last)
	}
	if wall > 3*shard {
		t.Fatalf("4 shards of %s took %s: they did not run at once", shard, wall)
	}
	if g.peak != 4 {
		t.Fatalf("%d shards ran at once, want 4", g.peak)
	}
	lanes := g.lanePods()
	if len(lanes) != 3 {
		t.Fatalf("lane pods %v, want 3 beside the lease's", lanes)
	}
	for _, p := range append(lanes, g.lp.name) {
		if n := g.calls("sidecar " + p + " postgres"); n != 1 {
			t.Fatalf("pod %s started %d Postgres, want its own one", p, n)
		}
	}
	for _, p := range g.f.PodSpecs() {
		if strings.Contains(p.Name, "-lane-") && (p.Labels[PodLeaseLabel] != "lease-1" || !p.Network.None) {
			t.Fatalf("lane pod %+v: not the lease's, or not isolated", p)
		}
	}
	for _, p := range lanes {
		if !slices.Contains(g.f.Removed, p) {
			t.Fatalf("lane pod %s was not removed: %v", p, g.f.Removed)
		}
	}
	if slices.Contains(g.f.Removed, g.lp.name) {
		t.Fatal("the lease's own pod was removed with the lanes")
	}
	if free := g.rt.Scheduler.Free(); free.CPUs != 32 || free.MemGB != 64 {
		t.Fatalf("the lanes' room was not given back: %+v", free)
	}
	shards := last["steps"].([]any)[0].(map[string]any)["shards"].([]any)
	if len(shards) != 4 {
		t.Fatalf("the answer's shards %v", shards)
	}
	if len(g.mesh.records) != 1 {
		t.Fatalf("%d records, want the run's one", len(g.mesh.records))
	}
}

// Shards are scheduled against the node's capacity like any exec: on a
// node with room for two lanes of the lease's resources beside the
// lease's own pod, the third waits for one to end.
func TestShardsWaitForTheNodesRoom(t *testing.T) {
	const shard = 400 * time.Millisecond
	g := newShardRig(t, shardedYML, shard, scheduler.Capacity{CPUs: 8, MemGB: 64, Slots: 3})
	start := time.Now()
	_, last := g.run(t, map[string]any{"suite": "integration"})
	wall := time.Since(start)
	if last["exit"] != float64(0) {
		t.Fatalf("the run answered %v", last)
	}
	if g.peak != 3 || wall < 2*shard {
		t.Fatalf("%d shards at once in %s on room for 3, want 3 and two shards' time", g.peak, wall)
	}
}

// A validation that would run past its repository's budget is stopped
// there and ends red, saying it ran out of time: its shards stopped, its
// lanes removed, a signed record of the red.
func TestAShardedRunPastItsBudgetEndsRedOutOfTime(t *testing.T) {
	g := newShardRig(t, shardedYML, time.Hour, scheduler.Capacity{CPUs: 32, MemGB: 64, Slots: 3})
	if g.suite.Budget != 80*time.Minute {
		t.Fatalf("the lease is held to %s, want validate.yml's 80m", g.suite.Budget)
	}
	g.suite.Budget = 500 * time.Millisecond
	start := time.Now()
	_, last := g.run(t, map[string]any{"suite": "integration"})
	if time.Since(start) > 10*time.Second {
		t.Fatal("the run was not stopped at its budget")
	}
	if last["exit"] != float64(1) || last["out_of_time"] != true || !strings.Contains(last["summary"].(string), "ran out of time") {
		t.Fatalf("the run answered %v, want red, out of time", last)
	}
	if len(g.mesh.records) != 1 {
		t.Fatalf("%d records of the red, want 1", len(g.mesh.records))
	}
	for _, p := range g.lanePods() {
		if !slices.Contains(g.f.Removed, p) {
			t.Fatalf("lane pod %s outlived the budget", p)
		}
	}
	if len(g.f.RemovedContainers) == 0 {
		t.Fatal("the shard stopped in the lease's own pod was not removed")
	}
}

// A kind other than validate is held to no budget.
func TestOnlyAValidationIsHeldToTheBudget(t *testing.T) {
	ws := t.TempDir()
	os.MkdirAll(filepath.Join(ws, ".runsheet"), 0o755)
	os.WriteFile(filepath.Join(ws, ".runsheet", "validate.yml"), []byte(shardedYML), 0o644)
	rt := &Runtime{}
	for kind, want := range map[string]time.Duration{"validate": 80 * time.Minute, "runsheet/validate": 80 * time.Minute, "build": 0, "steward": 0} {
		if got := rt.suiteBudget(&meshapi.Brief{Kind: kind}, ws); got != want {
			t.Fatalf("kind %s held to %s, want %s", kind, got, want)
		}
	}
}

// evidenceMesh takes a lease's evidence as Mesh does: each record's id
// its statement's digest.
type evidenceMesh struct {
	mu      sync.Mutex
	records []meshapi.EvidencePost
}

func (m *evidenceMesh) PutBlob(context.Context, string, []byte) error { return nil }

func (m *evidenceMesh) Evidence(_ context.Context, _ string, recs []meshapi.EvidencePost) (*meshapi.EvidenceResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for _, r := range recs {
		var env struct{ Payload string }
		_ = json.Unmarshal(r.Envelope, &env)
		p, _ := base64.StdEncoding.DecodeString(env.Payload)
		ids = append(ids, nodetools.Digest(p))
		m.records = append(m.records, r)
	}
	return &meshapi.EvidenceResponse{IDs: ids}, nil
}
