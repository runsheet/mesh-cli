package node

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// The memory floor (memfloor.go, ADR-037): with node.yaml's
// host.min_free_memory set, a node whose memory available is at or under
// it claims nothing of any kind, said once as it enters the state and
// once as it leaves it; left out, nothing changes from ADR-004.

// movingMachine is a MachineProbe whose free memory the test moves; its
// load and temperature are quiet, so only memory is judged.
type movingMachine struct {
	mu    sync.Mutex
	free  uint64
	total uint64
}

func (m *movingMachine) set(free uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.free = free
}

func (m *movingMachine) Read() (MachineReading, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return MachineReading{Load1: 1, CPUs: 96, MemAvailable: m.free, MemTotal: m.total, PackageTempC: 50, HasTemp: true}, nil
}

// host.min_free_memory reads as a size or a share of the machine's
// memory; left out, or a share on a machine whose total is unknown, it
// is MinFreeMemory.
func TestMemoryFloorReadsSizesAndShares(t *testing.T) {
	const total = 100 * gibBytes
	for _, tc := range []struct {
		in    string
		total uint64
		want  uint64
	}{
		{"", total, MinFreeMemory},
		{"32g", total, 32 * gibBytes},
		{"32G", total, 32 * gibBytes},
		{"32gb", total, 32 * gibBytes},
		{"1.5g", total, 3 * gibBytes / 2},
		{"512m", total, 512 << 20},
		{"1024k", total, 1 << 20},
		{"4096", total, 4096},
		{"15%", total, 15 * gibBytes},
		{" 15 % ", total, 15 * gibBytes},
		{"15%", 0, MinFreeMemory},
	} {
		if err := (HostConfig{MinFreeMemory: tc.in}).Validate(); err != nil {
			t.Errorf("%q: Validate = %v", tc.in, err)
		}
		if got := (HostConfig{MinFreeMemory: tc.in}).MemoryFloor(tc.total); got != tc.want {
			t.Errorf("%q on %d: floor = %d, want %d", tc.in, tc.total, got, tc.want)
		}
	}
	for _, bad := range []string{"lots", "0", "0g", "-1g", "0%", "100%", "150%", "g"} {
		err := (HostConfig{MinFreeMemory: bad}).Validate()
		if err == nil || !strings.HasPrefix(err.Error(), "host.min_free_memory: ") {
			t.Errorf("%q: Validate = %v, want a host.min_free_memory refusal", bad, err)
		}
	}
}

// node.yaml's host: block carries the key.
func TestMemoryFloorFromNodeYAML(t *testing.T) {
	var c struct {
		Host HostConfig `yaml:"host"`
	}
	if err := yaml.Unmarshal([]byte("host:\n  min_free_memory: 32g\n"), &c); err != nil {
		t.Fatal(err)
	}
	if got := c.Host.MemoryFloor(251 * gibBytes); got != 32*gibBytes {
		t.Fatalf("floor = %d, want 32 GiB", got)
	}
}

// ProcMachine reads MemTotal beside MemAvailable.
func TestProcMachineReadsMemTotal(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root+"/proc/loadavg", "3.47 2.10 1.90 2/812 12345\n")
	writeFile(t, root+"/proc/meminfo", "MemTotal:       16318548 kB\nMemFree:          512000 kB\nMemAvailable:    1048576 kB\n")
	m, err := (&ProcMachine{Root: root}).Read()
	if err != nil {
		t.Fatal(err)
	}
	if m.MemTotal != 16318548*1024 || m.MemAvailable != 1<<30 {
		t.Fatalf("read %+v", m)
	}
	// 1 GiB free of 15.6 GiB: under a 10% floor, over a 5% one
	if !m.ShortOfMemoryAt(HostConfig{MinFreeMemory: "10%"}) || m.ShortOfMemoryAt(HostConfig{MinFreeMemory: "5%"}) {
		t.Fatalf("10%%/5%% floors misjudged %+v", m)
	}
}

// The scalers judge free memory by the floor in force: 20 GiB free is
// plenty at the default floor and short of a 32g one.
func TestScalerMemoryCapTakesTheConfiguredFloor(t *testing.T) {
	m := fakeMachine{m: MachineReading{Load1: 1, CPUs: 96, MemAvailable: 20 * gibBytes, MemTotal: 251 * gibBytes}}
	for _, tc := range []struct {
		host HostConfig
		want int
		why  Reason
	}{
		{HostConfig{}, 4, ReasonMax},
		{HostConfig{MinFreeMemory: "32g"}, 2, ReasonLowMemory},
		{HostConfig{MinFreeMemory: "5%"}, 4, ReasonMax},
	} {
		s, c := newTestScaler(1, 4, m)
		feed(s, c, 2,
			&rise{0.10, 0.101, headroomT0.Add(4 * time.Hour)},
			&rise{0.30, 0.301, headroomT0.Add(5 * 24 * time.Hour)})
		s.Host = tc.host
		if n, why := s.Target(2); n != tc.want || why != tc.why {
			t.Errorf("%q: target = %d (%s), want %d (%s)", tc.host.MinFreeMemory, n, why, tc.want, tc.why)
		}
	}
}

// With host.min_free_memory set and memory available at or under it,
// three ticks over a build, an evaluation, a plan, a rebase and a
// direction claim nothing and log one memory_low line; once memory is
// freed the next tick claims and logs memory ok, once.
func TestMemoryLowClaimsNothingThenClaimsOnceFreed(t *testing.T) {
	p := takesFake(t)
	loop, logs := p.loop(2)
	loop.Cfg.Host.MinFreeMemory = "32g"
	machine := &movingMachine{free: 20 * gibBytes, total: 251 * gibBytes}
	loop.Machine = machine
	var evaluated atomic.Bool
	loop.Evaluate = func(context.Context, gateway.WorkItem) { evaluated.Store(true) }
	loop.Rebase = func(context.Context, gateway.WorkItem) {}

	for range 3 {
		tick(loop)
	}
	if got := claimCalls(p); len(got) != 0 {
		t.Fatalf("claims under the floor = %v, want none\n%s", got, logs.String())
	}
	if evaluated.Load() {
		t.Error("an evaluation started under the floor")
	}
	want := "memory_low: 20.0GiB free <= floor 32.0GiB; claiming nothing"
	if n := strings.Count(logs.String(), want); n != 1 {
		t.Fatalf("%d %q lines over three ticks, want 1:\n%s", n, want, logs.String())
	}
	if f := loop.MemoryFindings(); len(f) != 1 || f[0].Kind != "memory_low" || f[0].Severity != "critical" || f[0].Bytes != int64(20*gibBytes) {
		t.Errorf("MemoryFindings() = %+v, want one critical memory_low", f)
	}

	machine.set(100 * gibBytes)
	tick(loop)
	for _, name := range []string{"claimWorkItem", "claimPlan", "claimWorkItemRebase", "claimDirectionResearch", "claimWorkItemEvaluation"} {
		if len(p.CallsNamed(name)) == 0 {
			t.Errorf("no %s once memory was freed\n%s", name, logs.String())
		}
	}
	if !evaluated.Load() {
		t.Error("no evaluation started once memory was freed")
	}
	if n := strings.Count(logs.String(), "memory ok: 100.0GiB free > floor 32.0GiB; claiming again"); n != 1 {
		t.Fatalf("%d memory ok lines, want 1:\n%s", n, logs.String())
	}
	if f := loop.MemoryFindings(); len(f) != 0 {
		t.Errorf("MemoryFindings() = %+v above the floor, want none", f)
	}
}

// A merge batch's validation — the memory-hungriest claim — is not
// claimed under the floor.
func TestMemoryLowClaimsNoBatchValidation(t *testing.T) {
	loop, f, _, logs := batchLoop(t, oneStepYML("true"), 1)
	loop.Cfg.Host.MinFreeMemory = "10%"
	machine := &movingMachine{free: 10 * gibBytes, total: 251 * gibBytes}
	loop.Machine = machine
	tick(loop)
	if n := len(f.CallsNamed("claimMergeBatchValidation")); n != 0 {
		t.Fatalf("%d batch claims under the floor, want 0\n%s", n, logs.String())
	}
	machine.set(200 * gibBytes)
	tick(loop)
	if n := len(f.CallsNamed("claimMergeBatchValidation")); n != 1 {
		t.Fatalf("%d batch claims once freed, want 1\n%s", n, logs.String())
	}
}

// Left out, the key stops nothing: a fixed node with 1 GiB free claims
// as it did before ADR-037 and says nothing of memory.
func TestMemoryFloorUnsetClaimsAsBefore(t *testing.T) {
	p := takesFake(t)
	loop, logs := p.loop(2)
	loop.Machine = &movingMachine{free: 1 * gibBytes, total: 16 * gibBytes}
	tick(loop)
	if len(p.CallsNamed("claimWorkItem")) == 0 {
		t.Fatalf("no build claimed with the key left out\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "memory") {
		t.Errorf("a node without host.min_free_memory said something of memory:\n%s", logs.String())
	}
}

// The key is reloaded every tick with the rest of host:, a change said
// once, and a floor set by a reload judges the next tick.
func TestMemoryFloorReloads(t *testing.T) {
	loop, _, logs := newPriorityFake(t).liveLoop(t, reloadYAML)
	wantReloadLines(t, "first tick", tickLines(t, loop, logs))
	loop.Machine = &movingMachine{free: 20 * gibBytes, total: 251 * gibBytes}
	writeNodeYAML(t, reloadYAML+"host: {min_free_memory: 32g}\n")
	lines := tickLines(t, loop, logs)
	wantReloadLines(t, "min_free_memory set", lines, "host: min_free_memory (none) → 32g")
	if got := loop.HostThresholds().MinFreeMemory; got != "32g" {
		t.Fatalf("min_free_memory in force %q, want 32g", got)
	}
	if !loop.memLow() {
		t.Fatalf("20 GiB free under a reloaded 32g floor is not low:\n%s", logs.String())
	}
	writeNodeYAML(t, reloadYAML)
	tickLines(t, loop, logs)
	if loop.memLow() || !strings.Contains(logs.String(), "memory ok: host.min_free_memory is not set; claiming again") {
		t.Fatalf("removing the key did not lift the floor:\n%s", logs.String())
	}
}
