package nodev1

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
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
	b       *meshapi.Brief
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
	g.b = b
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
	var leaseNet sandbox.NetworkPolicy
	for _, p := range g.f.PodSpecs() {
		if p.Name == g.lp.name {
			leaseNet = p.Network
		}
	}
	for _, p := range g.f.PodSpecs() {
		if strings.Contains(p.Name, "-lane-") && (p.Labels[PodLeaseLabel] != "lease-1" || !reflect.DeepEqual(p.Network, leaseNet)) {
			t.Fatalf("lane pod %+v: not the lease's, or not on the lease's network %+v", p, leaseNet)
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
// node with room for three lanes of the lease's resources, the fourth
// waits for one to end. 12 CPUs, not 8: since the lease's lanes reserve
// their room as soon as the lane count is known (reserveLanes), the
// first lane — the lease's own pod — now holds Resources-worth of the
// scheduler too, where it once ran free of it; three concurrent lanes
// need three holds' worth of room, 12 CPUs at this suite's 4 each.
func TestShardsWaitForTheNodesRoom(t *testing.T) {
	const shard = 400 * time.Millisecond
	g := newShardRig(t, shardedYML, shard, scheduler.Capacity{CPUs: 12, MemGB: 64, Slots: 3})
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

// A lease's lanes reserve their room as soon as the lane count is known
// (reserveLanes, ADR-049 amendment): a validate lease whose suite shards
// into 4 lanes holds all 4 of the scheduler's lane holds the moment its
// pod opens, before run_suite ever opens one — so a later lease already
// sees no room left for lanes that have not run yet. 16 CPUs and 32
// MemGB fill exactly on 4 holds of the suite's Resources{4, 8} each: a
// scheduler close to its cap, as the acceptance asks.
func TestALeasesLanesHoldRoomAsSoonAsTheLaneCountIsKnown(t *testing.T) {
	g := newShardRig(t, shardedYML, 400*time.Millisecond, scheduler.Capacity{CPUs: 16, MemGB: 32, Slots: 3})

	var owners []string
	for _, h := range g.rt.Scheduler.Leases() {
		owners = append(owners, h.Owner)
	}
	slices.Sort(owners)
	want := []string{"lease-1/lane-1", "lease-1/lane-2", "lease-1/lane-3", "lease-1/lane-4"}
	if !slices.Equal(owners, want) {
		t.Fatalf("lane holds taken before any lane ran = %v, want %v", owners, want)
	}

	if _, err := g.rt.Scheduler.TryTake(scheduler.Ask{Owner: "lease-2", CPUs: 1, MemGB: 1}); err == nil {
		t.Fatal("a later lease was offered room the lease's unopened lanes already hold")
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

// goTestInPod is the fake runtime's `go test -json` of a package whose
// modules are only in the lease's Go module cache, as runsheet's e2e
// is on a node: a step whose HOME is not mounted, or holds no modules,
// fails as go does on a pod that cannot download them — at once, 0
// tests, [setup failed] (dl360, 2026-11-02, every shard but the first);
// one that sees them runs its two tests.
func goTestInPod(spec sandbox.AgentSpec) sandbox.Exit {
	var home string
	for _, kv := range spec.Env {
		if v, ok := strings.CutPrefix(kv, "HOME="); ok {
			home = v
		}
	}
	mounted := slices.ContainsFunc(spec.Mounts, func(m sandbox.Mount) bool { return m.Source == home && !m.ReadOnly })
	if _, err := os.Stat(filepath.Join(home, "go", "pkg", "mod", "cache", "download", "example.com", "dep", "@v", "v1.0.0.zip")); err != nil || !mounted {
		fmt.Fprintln(spec.Stderr, `e2e/helpers_test.go:36:2: example.com/dep@v1.0.0: Get "https://proxy.golang.org/example.com/dep/@v/v1.0.0.zip": dial tcp: lookup proxy.golang.org: connect: network is unreachable`)
		fmt.Fprintln(spec.Stdout, `{"Action":"output","Package":"example.com/e2e","Output":"FAIL\texample.com/e2e [setup failed]\n"}`)
		fmt.Fprintln(spec.Stdout, `{"Action":"fail","Package":"example.com/e2e"}`)
		return sandbox.Exit{Code: 1}
	}
	for _, test := range []string{"TestA", "TestB"} {
		fmt.Fprintf(spec.Stdout, `{"Action":"run","Package":"example.com/e2e","Test":%q}`+"\n", test)
		fmt.Fprintf(spec.Stdout, `{"Action":"pass","Package":"example.com/e2e","Test":%q}`+"\n", test)
	}
	fmt.Fprintln(spec.Stdout, `{"Action":"pass","Package":"example.com/e2e"}`)
	return sandbox.Exit{}
}

// A lane sees what a step in the lease's pod sees (ADR-049 amendment;
// dl360, 2026-11-02): a sharded Go suite whose modules are only in the
// lease's Go module cache — what its session and its earlier steps
// downloaded through the lease's relay — runs every shard's tests, not
// only the first's. Each lane runs on the lease's HOME, mounted
// read-write, and the HOME outlives the lanes.
func TestEveryShardSeesTheLeasesGoModuleCache(t *testing.T) {
	g := newShardRig(t, shardedYML, 0, scheduler.Capacity{CPUs: 32, MemGB: 64, Slots: 3})
	home := g.lp.sess.Home
	zip := filepath.Join(home, "go", "pkg", "mod", "cache", "download", "example.com", "dep", "@v", "v1.0.0.zip")
	if err := os.MkdirAll(filepath.Dir(zip), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zip, []byte("module"), 0o644); err != nil {
		t.Fatal(err)
	}
	g.f.Agent = func(_ context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		if !strings.Contains(spec.Name, "-exec-") {
			return sandbox.Exit{}, nil
		}
		return goTestInPod(spec), nil
	}
	_, last := g.run(t, map[string]any{"suite": "integration"})
	shards := last["steps"].([]any)[0].(map[string]any)["shards"].([]any)
	if len(shards) != 4 {
		t.Fatalf("the answer's shards %v", shards)
	}
	for _, sh := range shards {
		sh := sh.(map[string]any)
		if sh["exit"] != float64(0) || sh["tests_run"] != float64(2) {
			t.Fatalf("shard %v: want exit 0 and its 2 tests, the lease's modules seen", sh)
		}
	}
	if last["exit"] != float64(0) {
		t.Fatalf("the run answered %v", last)
	}
	if len(g.lanePods()) != 3 {
		t.Fatalf("lane pods %v, want 3", g.lanePods())
	}
	if _, err := os.Stat(zip); err != nil {
		t.Fatalf("a lane's end removed the lease's HOME: %v", err)
	}
}

// A step that fails in a lane is in the node's log: its lane, its
// shard, its exit and the last lines of its stderr.
func TestALanesFailedStepIsInTheNodesLog(t *testing.T) {
	g := newShardRig(t, shardedYML, 0, scheduler.Capacity{CPUs: 32, MemGB: 64, Slots: 3})
	var mu sync.Mutex
	var lines []string
	g.rt.Logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	g.f.Agent = func(_ context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		if !strings.Contains(spec.Name, "-exec-") {
			return sandbox.Exit{}, nil
		}
		return goTestInPod(spec), nil // no modules anywhere: every shard fails
	}
	_, last := g.run(t, map[string]any{"suite": "integration"})
	if last["exit"] == float64(0) {
		t.Fatalf("the run answered %v, want red", last)
	}
	mu.Lock()
	defer mu.Unlock()
	var lane, own bool
	for _, l := range lines {
		if !strings.Contains(l, "nodev1: lease lease-1: run_suite's step MESH_SHARD_INDEX=") || !strings.Contains(l, "exited 1 after") ||
			!strings.Contains(l, "network is unreachable") {
			continue
		}
		if strings.Contains(l, " in lane integration-") && strings.Contains(l, "-lane-") {
			lane = true
		}
		if strings.Contains(l, " in the lease's pod "+g.lp.name+" ") {
			own = true
		}
	}
	if !lane || !own {
		t.Fatalf("a lane's failed step (%v) or the lease pod's (%v) is not in the log:\n%s", lane, own, strings.Join(lines, "\n"))
	}
}

// A lane reaches out as the lease's pod does, never on a raw network:
// with the lease's relay as its only exit, the lane is on the lease's
// network, the relay's socket directory is mounted into it, and each
// step runs behind the bridge to it on the lane's loopback, with the
// lease's proxy environment.
func TestALaneReachesTheLeasesRelay(t *testing.T) {
	g := newShardRig(t, shardedYML, 0, scheduler.Capacity{CPUs: 32, MemGB: 64, Slots: 3})
	sockDir := t.TempDir()
	sock := filepath.Join(sockDir, "s")
	g.lp.proxyEnv = proxyEnv(g.lp.bridgeBase())
	g.lp.setLaneSocket(sock)
	var mu sync.Mutex
	var lanes []sandbox.AgentSpec
	g.f.Agent = func(_ context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		if strings.Contains(spec.Name, "-lane-") && strings.Contains(spec.Name, "-exec-") {
			mu.Lock()
			lanes = append(lanes, spec)
			mu.Unlock()
		}
		return sandbox.Exit{}, nil
	}
	_, last := g.run(t, map[string]any{"suite": "integration"})
	if last["exit"] != float64(0) {
		t.Fatalf("the run answered %v", last)
	}
	if len(lanes) != 3 {
		t.Fatalf("%d steps ran in lanes, want 3", len(lanes))
	}
	for _, spec := range lanes {
		if len(spec.Command) < 6 || spec.Command[0] != "node" || spec.Command[1] != filepath.Join(g.lp.sess.Home, laneBridgeName) ||
			spec.Command[2] != sock || spec.Command[3] != strconv.Itoa(g.lp.gitPort) || spec.Command[4] != "--" || spec.Command[5] != "sh" {
			t.Fatalf("a lane's step ran %q, want it behind the bridge to %s", spec.Command, sock)
		}
		if !slices.ContainsFunc(spec.Mounts, func(m sandbox.Mount) bool { return m.Source == sockDir }) {
			t.Fatalf("the relay's socket directory is not mounted in the lane: %+v", spec.Mounts)
		}
		if !slices.Contains(spec.Env, "HTTPS_PROXY="+g.lp.bridgeBase()) {
			t.Fatalf("the lane's step has not the lease's proxy env: %v", spec.Env)
		}
	}
	if _, err := os.Stat(filepath.Join(g.lp.sess.Home, laneBridgeName)); err != nil {
		t.Fatalf("the lanes' bridge is not in the lease's HOME: %v", err)
	}
}
