package node

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/validate"
)

// The budget counts each pod's CPUs, a job's hold beside its pods, and
// a claimed session's expected pod until it opens; everything goes back.
func TestCPUBudgetAccounting(t *testing.T) {
	b := NewCPUBudget(32, 8)
	if !b.Fits(32) {
		t.Fatal("an empty budget refuses its own size")
	}
	rel := b.Reserve(8)
	if r := b.Read(); r.Reserved != 8 || r.Pods != 1 || r.Committed != 8 {
		t.Fatalf("after one pod: %+v", r)
	}
	b.Expect(8)
	if r := b.Read(); r.Reserved != 8 || r.Committed != 16 {
		t.Fatalf("an expected pod is committed, not reserved: %+v", r)
	}
	// the expected pod opens: it takes the expectation over
	rel2 := b.Reserve(8)
	if r := b.Read(); r.Reserved != 16 || r.Committed != 16 {
		t.Fatalf("an opened pod counted twice: %+v", r)
	}
	j := b.Job("pr 1", "runsheet/runsheet", 12)
	if r := b.Read(); r.Committed != 28 || r.Jobs != 1 {
		t.Fatalf("a job's hold is committed: %+v", r)
	}
	if b.Fits(8) {
		t.Fatal("8 more fit beside 28 of 32")
	}
	if !b.Fits(4) {
		t.Fatal("4 more do not fit beside 28 of 32")
	}
	release, err := j.Pod(context.Background(), 8)
	if err != nil {
		t.Fatal(err)
	}
	if r := b.Read(); r.Reserved != 24 || r.Committed != 28 {
		t.Fatalf("a pod within its job's hold commits nothing more: %+v", r)
	}
	release()
	release() // once
	rel()
	rel2()
	j.End()
	if r := b.Read(); r.Reserved != 0 || r.Committed != 0 || r.Pods != 0 || r.Jobs != 0 {
		t.Fatalf("everything given back, still %+v", r)
	}
	// an expectation lapses
	now := time.Now()
	b.Now = func() time.Time { return now }
	b.Expect(8)
	now = now.Add(ExpectFor + time.Second)
	if r := b.Read(); r.Committed != 0 {
		t.Fatalf("a lapsed expectation is still committed: %+v", r)
	}
	// a nil budget is no budget
	var none *CPUBudget
	if !none.Fits(1e9) || none.Job("x", "", 1) != nil {
		t.Fatal("a nil budget limits")
	}
	none.Reserve(4)()
}

// A validation's repo estimate is learnt from its validate.yml; before
// it, DefaultValidationPods pods. Estimate raises a hold only into what
// is free.
func TestCPUBudgetEstimates(t *testing.T) {
	b := NewCPUBudget(32, 8)
	if got := b.ValidationEstimate("acme/app"); got != DefaultValidationPods*8 {
		t.Fatalf("unknown repo estimate %g", got)
	}
	b.Learn("acme/app", 80)
	if got := b.ValidationEstimate("acme/app"); got != 80 {
		t.Fatalf("learnt estimate %g, want 80", got)
	}
	a := b.Job("a", "", 16)
	c := b.Job("c", "", 8)
	c.Estimate(80)
	if got := c.Hold(); got != 16 {
		t.Fatalf("raised hold %g, want what was free (16)", got)
	}
	c.Estimate(4)
	if got := c.Hold(); got != 4 {
		t.Fatalf("lowered hold %g, want 4", got)
	}
	a.End()
	c.Estimate(80)
	if got := c.Hold(); got != 32 {
		t.Fatalf("a job alone holds its estimate up to the budget, got %g", got)
	}
}

// node.yaml's capacity takes cpu_budget in its mapping and writes it
// back; a negative one is refused.
func TestCapacityCPUBudgetYAML(t *testing.T) {
	var c Capacity
	if err := yaml.Unmarshal([]byte("min: 4\nmax: 16\ncpu_budget: 120\n"), &c); err != nil {
		t.Fatal(err)
	}
	if c.CPUBudget != 120 || c.Max != 16 {
		t.Fatalf("read %+v", c)
	}
	out, err := yaml.Marshal(Capacity{Min: 16, Max: 16, CPUBudget: 96})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "cpu_budget: 96") {
		t.Fatalf("written %q", out)
	}
	if err := yaml.Unmarshal([]byte("min: 1\nmax: 2\ncpu_budget: -1\n"), &c); err == nil {
		t.Fatal("a negative cpu_budget read")
	}
	if got := CPUBudgetFor(0, 96); got != 120 {
		t.Fatalf("default budget on 96 cores %g, want 120", got)
	}
	if got := CPUBudgetFor(64, 96); got != 64 {
		t.Fatalf("configured budget %g, want 64", got)
	}
}

// A ten-pod validation — an eight-shard integration suite, a unit group
// and a checks group — on a 32-CPU budget with 8-CPU pods runs in waves:
// never more than four pods at once, each shard still in a pod of its
// own with its own Postgres, every lane run.
func TestATenPodValidationRunsInWaves(t *testing.T) {
	var mu sync.Mutex
	running, most, ran := 0, 0, 0
	rt := &sandbox.Fake{Agent: func(ctx context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		mu.Lock()
		running++
		ran++
		most = max(most, running)
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		running--
		mu.Unlock()
		return sandbox.Exit{}, nil
	}}
	sb := &sandbox.Sandbox{Runtime: rt, Config: SandboxConfig{CPUs: 6, ServicesCPUs: 2}.Settings("v1"), TempDir: t.TempDir()}
	sb.Config.ServiceTimeout = 2 * time.Second
	budget := NewCPUBudget(32, 8)
	sb.Reserve = budget.Reserve
	dir := t.TempDir()
	writeServices(t, dir, servicesYML)
	logs := &syncBuf{}
	logf := func(f string, a ...any) { fmt.Fprintf(logs, f+"\n", a...) }
	budget.SetLogf(logf)

	cj := budget.Job("pr acme/app#1", "acme/app", budget.ValidationEstimate("acme/app"))
	ctx := WithCPUJob(context.Background(), cj)
	first, err := OpenSessionPod(ctx, sb, dir, "pr-1", harness.Kind("validate"), sandbox.GitIdentity{}, logf)
	if err != nil {
		t.Fatal(err)
	}
	if r := budget.Read(); r.Reserved != 8 {
		t.Fatalf("the run's own pod reserves %g, want 8 (6 + 2 for its services)", r.Reserved)
	}
	lr := laneSandboxRunner{
		sandboxRunner: sandboxRunner{sess: first.Session, dir: dir},
		open: func(ctx context.Context, name string) (*SessionPod, error) {
			return OpenSessionPod(WithCPUJob(ctx, cj), sb, dir, "pr-1-"+name, harness.Kind("validate"), sandbox.GitIdentity{}, logf)
		},
		releaseFirst: first.Close,
	}
	steps := []validate.Step{
		{Name: "lint", Kind: validate.KindLint, Group: "checks", Command: "true", Timeout: time.Minute},
		{Name: "unit", Kind: validate.KindUnit, Group: "unit", Command: "go test ./...", Timeout: time.Minute},
		{Name: "integration", Kind: validate.KindIntegration, Command: "go test ./e2e", Timeout: time.Minute, Shards: 8},
	}
	if n := validate.Lanes(steps); n != 10 {
		t.Fatalf("lanes %d, want 10", n)
	}
	v := &buildValidation{runner: lr, cpu: cj, pod: PodCPUs(sb, dir)}
	validator{logf: logf, CPU: budget}.estimate(validationTarget{prefix: "pr acme/app#1"}, v, steps)
	if got := budget.ValidationEstimate("acme/app"); got != 80 {
		t.Fatalf("learnt estimate %g, want 10 pods of 8", got)
	}
	if got := cj.Hold(); got != 32 {
		t.Fatalf("hold %g, want its estimate capped at the budget", got)
	}

	// the budget is never past 32 while it runs
	stop := make(chan struct{})
	over := make(chan float64, 1)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if r := budget.Read(); r.Reserved > 32 {
				select {
				case over <- r.Reserved:
				default:
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()
	res, err := validate.Run(ctx, steps, lr, validate.Options{Dir: dir})
	close(stop)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-over:
		t.Fatalf("reserved %g cpus, past the budget of 32", got)
	default:
	}
	for _, s := range res {
		if s.ExitCode != 0 {
			t.Fatalf("step %s exited %d", s.Name, s.ExitCode)
		}
	}
	if ran != 10 {
		t.Fatalf("ran %d lanes, want 10", ran)
	}
	if most > 4 {
		t.Fatalf("%d pods ran at once on a 32-cpu budget of 8-cpu pods, want at most 4", most)
	}
	if most < 2 {
		t.Fatalf("at most %d pod at once: the lanes did not run beside each other", most)
	}
	var postgres int
	for _, sc := range rt.Sidecars {
		if strings.HasPrefix(sc.Image, "postgres") {
			postgres++
		}
	}
	if postgres != 10 {
		t.Fatalf("postgres sidecars %d, want one per pod (10)", postgres)
	}
	if !strings.Contains(logs.String(), "waits for room") {
		t.Fatalf("no wave said:\n%s", logs.String())
	}
	cj.End()
	if r := budget.Read(); r.Reserved != 0 || r.Committed != 0 {
		t.Fatalf("after the run: %+v", r)
	}
}

// The one claim stops when the budget is full and resumes once pods
// finish: two 4-cpu sessions fill an 8-cpu budget, the third waits for
// the next tick after their pods go.
func TestClaimingStopsWhenTheCPUBudgetIsFullAndResumesWhenPodsFinish(t *testing.T) {
	r := newQueueRig(t, 3)
	r.f.WorkItems[item2]["status"], r.f.WorkItems[item2]["dependsOn"] = "ready", []any{}
	third := map[string]any{}
	for k, v := range r.f.WorkItems[item2] {
		third[k] = v
	}
	third["id"], third["seq"] = queueItem3, 3
	r.f.WorkItems[queueItem3] = third
	r.f.Queue = []gatewaytest.QueuedJob{
		{Kind: "runsheet/build", SubjectType: "item", SubjectID: item1},
		{Kind: "runsheet/build", SubjectType: "item", SubjectID: item2},
		{Kind: "runsheet/build", SubjectType: "item", SubjectID: queueItem3},
	}
	r.loop.CPU = NewCPUBudget(8, 4)
	r.tick()
	if got := len(r.dispatched); got != 2 {
		t.Fatalf("dispatched %d with room for two pods, want 2\n%s", got, r.logs.String())
	}
	if !strings.Contains(r.logs.String(), "cpu budget full") {
		t.Fatalf("the full budget was not said:\n%s", r.logs.String())
	}
	r.tick()
	if got := len(r.dispatched); got != 2 {
		t.Fatalf("dispatched %d while the two sessions' pods are expected, want 2", got)
	}
	// the two sessions open their pods and finish
	a, b := r.loop.CPU.Reserve(4), r.loop.CPU.Reserve(4)
	a()
	b()
	r.tick()
	if got := len(r.dispatched); got != 3 {
		t.Fatalf("dispatched %d once the pods finished, want 3\n%s", got, r.logs.String())
	}
}

// A job bigger than the whole budget still runs, but alone: it fits only
// when nothing is committed, nothing else is claimed beside it, its pods
// run within the budget in waves, and a single pod bigger than the
// budget opens when nothing else is open.
func TestAnOversizedJobRunsAlone(t *testing.T) {
	b := NewCPUBudget(16, 8)
	rel := b.Reserve(8)
	if b.Fits(40) {
		t.Fatal("an oversized job fits beside another pod")
	}
	rel()
	if !b.Fits(40) {
		t.Fatal("an oversized job does not fit an idle node")
	}
	j := b.Job("big", "acme/big", 40)
	if j.Hold() != 16 {
		t.Fatalf("hold %g, want the whole budget, never more", j.Hold())
	}
	if b.Fits(1) {
		t.Fatal("a claim fits beside the oversized job")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r1, err := j.Pod(ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := j.Pod(ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		r3, err := j.Pod(ctx, 8)
		if err == nil {
			r3()
		}
		got <- err
	}()
	select {
	case <-got:
		t.Fatal("a third pod opened past the budget")
	case <-time.After(50 * time.Millisecond):
	}
	r1()
	if err := <-got; err != nil {
		t.Fatal(err)
	}
	r2()
	// one pod bigger than the whole budget opens on an idle node
	r4, err := j.Pod(ctx, 24)
	if err != nil {
		t.Fatal(err)
	}
	r4()
	j.End()
	if !b.Fits(8) {
		t.Fatal("the budget did not come back")
	}
}

// A pod past its job's hold waits in turn, gives up with its context,
// and — when every pod open waits on its validation — the first in turn
// opens past the budget after CPUValveAfter, one at a time.
func TestAPodPastItsHoldWaitsInTurn(t *testing.T) {
	was := CPUValveAfter
	CPUValveAfter = 50 * time.Millisecond
	defer func() { CPUValveAfter = was }()
	b := NewCPUBudget(8, 4)
	session := b.Reserve(8) // a build's pod, waiting on its validation
	j := b.Job("build", "", 0)
	ctx, cancel := context.WithCancel(context.Background())
	waiting := make(chan error, 1)
	go func() {
		_, err := j.Pod(ctx, 4)
		waiting <- err
	}()
	time.Sleep(10 * time.Millisecond)
	if r := b.Read(); r.Waiting != 1 {
		t.Fatalf("waiting %d, want 1", r.Waiting)
	}
	if b.Fits(1) {
		t.Fatal("a claim fits while a pod waits")
	}
	cancel()
	if err := <-waiting; !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want canceled", err)
	}
	if r := b.Read(); r.Waiting != 0 {
		t.Fatalf("a cancelled waiter still waits: %+v", r)
	}
	// the valve: nothing but the waiting session is open
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	start := time.Now()
	rel, err := j.Pod(ctx2, 4)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < CPUValveAfter {
		t.Fatal("the valve opened before CPUValveAfter")
	}
	// with the job's pod open, a second one waits again
	second := make(chan struct{})
	go func() {
		r, err := j.Pod(ctx2, 4)
		if err == nil {
			r()
		}
		close(second)
	}()
	select {
	case <-second:
		t.Fatal("a second pod passed the budget beside the first")
	case <-time.After(2 * CPUValveAfter):
	}
	session()
	<-second
	rel()
	j.End()
}

// dl360, v0.2.633: budget raised to 320, 64 reserved by 5 pods, 144
// committed, 3 shards waiting — the waiting pods start at once, and a
// 16-cpu claim fits.
func TestWaitingPodsStartWithRoomAndDoNotBlockClaims(t *testing.T) {
	// five pods reserving 64 — a session's of 16 and four of a
	// validation's of 12 — fill a budget of 64; three more shards wait
	b := NewCPUBudget(64, 16)
	sess := b.Reserve(16)
	defer sess()
	j := b.Job("pr runsheet/runsheet#5647@9f5e8d8b", "runsheet/runsheet", 96)
	var rels []func()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range 4 {
		r, err := j.Pod(ctx, 12)
		if err != nil {
			t.Fatal(err)
		}
		rels = append(rels, r)
	}
	started := make(chan func(), 3)
	for range 3 {
		go func() {
			r, err := j.Pod(ctx, 16)
			if err != nil {
				return
			}
			started <- r
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for b.Read().Waiting < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("the shards did not wait on a full budget: %s", b.Read().Line())
		}
		time.Sleep(5 * time.Millisecond)
	}
	// the operator raises the budget to 320
	b.SetTotal(320)
	for range 3 {
		select {
		case r := <-started:
			rels = append(rels, r)
		case <-time.After(2 * time.Second):
			t.Fatalf("a waiting pod did not start with room: %s", b.Read().Line())
		}
	}
	if r := b.Read(); r.Waiting != 0 || r.Reserved != 16+48+48 {
		t.Fatalf("after the raise: %+v", r)
	}
	// a waiting pod alone blocks no claim: the exact reading of the stall
	b2 := NewCPUBudget(320, 16)
	s2 := b2.Reserve(16)
	defer s2()
	j2 := b2.Job("pr", "runsheet/runsheet", 128)
	for range 4 {
		r, err := j2.Pod(ctx, 12)
		if err != nil {
			t.Fatal(err)
		}
		defer r()
	}
	b2.mu.Lock()
	b2.queue = append(b2.queue, &cpuWaiter{cpus: 16, since: time.Now()}, &cpuWaiter{cpus: 16, since: time.Now()}, &cpuWaiter{cpus: 16, since: time.Now()})
	b2.mu.Unlock()
	if r := b2.Read(); r.Reserved != 64 || r.Pods != 5 || r.Committed != 144 || r.Waiting != 3 {
		t.Fatalf("the stalled reading not reproduced: %+v", r)
	}
	if !b2.Fits(16) {
		t.Fatalf("a 16-cpu claim refused at %s", b2.Read().Line())
	}
	for _, r := range rels {
		r()
	}
}
