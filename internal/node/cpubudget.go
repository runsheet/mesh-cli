package node

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"
)

// The CPU budget (ADR-043): the node's capacity counts the CPUs its pods
// ask for, not only its jobs. Every running pod — a build, evaluation,
// plan, research or rebase session's, a validation's own and each of its
// groups' and shards' — reserves its --cpus against capacity.cpu_budget
// (its sidecars their services_cpus). A claimed validation holds its
// estimate (every group and shard it will run, from its repo's
// validate.yml once known) from its claim to its end, its pods drawing
// on that hold first; past it they wait for room, so a validation's
// shards start in waves, never above the budget. The node claims a new
// job only when its estimate fits. The job-count capacity stays, an outer
// limit.

const (
	// DefaultCPUBudgetFactor is capacity.cpu_budget left out: this many
	// CPUs per core of the host. A pod's --cpus is a ceiling, rarely all
	// used at once; a little over the cores keeps the host busy.
	DefaultCPUBudgetFactor = 1.25
	// DefaultValidationPods is a validation's estimate in pods before its
	// repo's validate.yml has been read on this node: conservative, as a
	// repo with groups and shards runs several.
	DefaultValidationPods = 4
	// ExpectFor is how long a claimed session's expected pod is held
	// against the budget before its pod opens (Expect): the checkout
	// comes first.
	ExpectFor = 5 * time.Minute
	// cpuSayEvery is how often the budget is logged while near its limit.
	cpuSayEvery = time.Minute
	// cpuNear is the share of the budget that is near its limit.
	cpuNear = 0.9
	// cpuEps absorbs float sums of quarter CPUs.
	cpuEps = 1e-6
)

// CPUBudgetFor is the budget in CPUs: node.yaml's capacity.cpu_budget
// when set, else DefaultCPUBudgetFactor × cores.
func CPUBudgetFor(configured float64, cores int) float64 {
	if configured > 0 {
		return configured
	}
	return DefaultCPUBudgetFactor * float64(max(cores, 1))
}

// CPUBudget is the node's CPUs reserved against capacity.cpu_budget. A
// nil *CPUBudget is no budget: everything fits and nothing waits.
type CPUBudget struct {
	// Pod is one session pod's CPUs, the sandbox's cpus plus its
	// services_cpus: what a claimed session is expected to reserve, and a
	// validation's estimate per group or shard.
	Pod float64
	// Now is the clock; time.Now when nil.
	Now func() time.Time

	mu    sync.Mutex
	total float64
	// pods is what the pods no job holds reserve (Reserve), npods their
	// count
	pods  float64
	npods int
	jobs  map[*CPUJob]struct{}
	// pending are the pods claimed sessions are expected to open
	// (Expect), until one opens or ExpectFor passes
	pending []pendingCPU
	// known is each repo's validation estimate, learnt from its
	// validate.yml (Learn)
	known map[string]float64
	// queue is the pod requests past their job's hold, first come first
	// served (CPUJob.Pod)
	queue []*cpuWaiter
	// changed is closed and replaced whenever a reservation goes back
	changed chan struct{}
	saidAt  time.Time
	logfn   func(string, ...any)
}

type pendingCPU struct {
	cpus  float64
	until time.Time
}

type cpuWaiter struct {
	cpus  float64
	since time.Time
}

// CPUValveAfter is how long the first pod in turn waits with no job's pod
// open before it opens past the budget (CPUJob.Pod): the sessions that
// fill the budget may be waiting on their own validations' shards.
var CPUValveAfter = 2 * time.Minute

// NewCPUBudget is a budget of total CPUs.
func NewCPUBudget(total, pod float64) *CPUBudget {
	return &CPUBudget{total: total, Pod: pod}
}

func (b *CPUBudget) now() time.Time {
	if b.Now == nil {
		return time.Now()
	}
	return b.Now()
}

// SetTotal changes the budget (node.yaml reloaded); waiters look again.
func (b *CPUBudget) SetTotal(total float64) {
	if b == nil || total <= 0 {
		return
	}
	b.mu.Lock()
	b.total = total
	b.broadcastLocked()
	b.mu.Unlock()
}

// Total is the budget in CPUs; 0 for no budget.
func (b *CPUBudget) Total() float64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

func (b *CPUBudget) broadcastLocked() {
	if b.changed != nil {
		close(b.changed)
	}
	b.changed = make(chan struct{})
}

func (b *CPUBudget) waitChanLocked() chan struct{} {
	if b.changed == nil {
		b.changed = make(chan struct{})
	}
	return b.changed
}

// inUseLocked is the CPUs every open pod reserves.
func (b *CPUBudget) inUseLocked() float64 {
	n := b.pods
	for j := range b.jobs {
		n += j.pods
	}
	return n
}

// jobPodsLocked is the CPUs the jobs' own pods reserve.
func (b *CPUBudget) jobPodsLocked() float64 {
	var n float64
	for j := range b.jobs {
		n += j.pods
	}
	return n
}

func (b *CPUBudget) pendingLocked() float64 {
	now := b.now()
	keep := b.pending[:0]
	var n float64
	for _, p := range b.pending {
		if now.Before(p.until) && p.cpus > cpuEps {
			keep = append(keep, p)
			n += p.cpus
		}
	}
	b.pending = keep
	return n
}

// committedLocked is what the budget has promised: the pods no job
// holds, each job's hold or its pods if more, and the pods claimed
// sessions are expected to open.
func (b *CPUBudget) committedLocked() float64 {
	n := b.pods + b.pendingLocked()
	for j := range b.jobs {
		n += j.committed()
	}
	return n
}

// Fits says whether a new job estimated at est CPUs may be claimed: no
// pod waits for room, and est fits beside what is committed — or nothing
// is committed at all, so a job bigger than the whole budget still
// runs, alone.
func (b *CPUBudget) Fits(est float64) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fitsLocked(est)
}

func (b *CPUBudget) fitsLocked(est float64) bool {
	if b.total <= 0 {
		return true
	}
	if len(b.queue) > 0 {
		return false
	}
	c := b.committedLocked()
	return c <= cpuEps || c+est <= b.total+cpuEps
}

// Expect holds cpus for a claimed session's pod until it opens (the
// next Reserve takes it over) or ExpectFor passes: a claim made before
// its checkout is counted on the next tick too.
func (b *CPUBudget) Expect(cpus float64) {
	if b == nil || cpus <= 0 {
		return
	}
	b.mu.Lock()
	b.pending = append(b.pending, pendingCPU{cpus: cpus, until: b.now().Add(ExpectFor)})
	b.mu.Unlock()
}

// Reserve reserves cpus for a pod no job holds (a session's, through
// the sandbox's Reserve): never refused, since its claim already fitted;
// the oldest expected pods it takes over. The func gives them back, once.
func (b *CPUBudget) Reserve(cpus float64) func() {
	if b == nil || cpus <= 0 {
		return func() {}
	}
	b.mu.Lock()
	b.pods += cpus
	b.npods++
	left := cpus
	b.pendingLocked()
	for i := range b.pending {
		if left <= cpuEps {
			break
		}
		take := math.Min(left, b.pending[i].cpus)
		b.pending[i].cpus -= take
		left -= take
	}
	b.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			b.pods = math.Max(b.pods-cpus, 0)
			b.npods--
			b.broadcastLocked()
			b.mu.Unlock()
		})
	}
}

// Learn keeps repo's validation estimate, all its groups and shards, for
// the next claim of one of its validations (ValidationEstimate).
func (b *CPUBudget) Learn(repo string, cpus float64) {
	if b == nil || repo == "" || cpus <= 0 {
		return
	}
	b.mu.Lock()
	if b.known == nil {
		b.known = map[string]float64{}
	}
	b.known[repo] = cpus
	b.mu.Unlock()
}

// ValidationEstimate is a validation of repo's estimate: what its
// validate.yml asked for when last read here, else DefaultValidationPods
// pods.
func (b *CPUBudget) ValidationEstimate(repo string) float64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if n, ok := b.known[repo]; ok && repo != "" {
		return n
	}
	return DefaultValidationPods * b.Pod
}

// Job registers a job holding est CPUs from now until End: its pods
// (CPUJob.Pod) draw on the hold first. The hold is what is free when est
// does not fit, unless nothing else is committed (a job bigger than the
// budget runs alone).
func (b *CPUBudget) Job(name, repo string, est float64) *CPUJob {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	j := &CPUJob{b: b, name: name, repo: repo}
	c := b.committedLocked()
	switch {
	case c <= cpuEps:
		j.hold = est
	default:
		j.hold = math.Max(math.Min(est, b.total-c), 0)
	}
	if b.jobs == nil {
		b.jobs = map[*CPUJob]struct{}{}
	}
	b.jobs[j] = struct{}{}
	return j
}

// CPUJob is one job's share of the budget: its hold and its pods.
type CPUJob struct {
	b          *CPUBudget
	name, repo string
	hold, pods float64
	npods      int
	// ended is a job past End whose pods have not all gone
	ended bool
}

func (j *CPUJob) committed() float64 { return math.Max(j.hold, j.pods) }

// Reserved is the CPUs the job's open pods reserve.
func (j *CPUJob) Reserved() float64 {
	if j == nil {
		return 0
	}
	j.b.mu.Lock()
	defer j.b.mu.Unlock()
	return j.pods
}

// Hold is the CPUs held for the job.
func (j *CPUJob) Hold() float64 {
	if j == nil {
		return 0
	}
	j.b.mu.Lock()
	defer j.b.mu.Unlock()
	return j.hold
}

// Repo is the repo the job validates, "" when unknown.
func (j *CPUJob) Repo() string {
	if j == nil {
		return ""
	}
	return j.repo
}

// Estimate sets the job's hold to cpus, now that its validate.yml says
// what it runs: lowered at once, raised only into what is free (or
// wholly, when nothing else is committed), so a raise never holds back a
// pod another job was promised.
func (j *CPUJob) Estimate(cpus float64) {
	if j == nil {
		return
	}
	b := j.b
	b.mu.Lock()
	defer b.mu.Unlock()
	if cpus <= j.hold {
		j.hold = math.Max(cpus, 0)
		b.broadcastLocked()
		return
	}
	others := b.committedLocked() - j.committed()
	if others <= cpuEps {
		j.hold = cpus
		return
	}
	j.hold = math.Max(j.hold, math.Min(cpus, b.total-others))
}

// Pod reserves cpus for one of the job's pods, waiting while they do not
// fit: within the job's hold, they fit while every pod's CPUs stay within
// the budget; past it, in turn with the other requests past their holds,
// once what is committed leaves room. A pod fits whatever its size when
// no pod is open at all, and — so sessions waiting on their validations'
// shards cannot hold the node forever — the first request in turn fits
// once it has waited CPUValveAfter with no job's pod open. The func
// gives the CPUs back, once; the error is ctx's.
func (j *CPUJob) Pod(ctx context.Context, cpus float64) (func(), error) {
	if j == nil || cpus <= 0 {
		return func() {}, nil
	}
	b := j.b
	var w *cpuWaiter
	said := false
	b.mu.Lock()
	for {
		ok, valve := j.fitsLocked(cpus, w)
		if ok {
			if w != nil {
				b.dequeueLocked(w)
			}
			j.pods += cpus
			j.npods++
			b.mu.Unlock()
			if valve {
				b.logf("cpu budget: %s: one pod of %g cpus past the budget — every pod open waits on its validation", j.name, cpus)
			}
			var once sync.Once
			return func() {
				once.Do(func() {
					b.mu.Lock()
					j.pods = math.Max(j.pods-cpus, 0)
					j.npods--
					if j.ended && j.npods <= 0 {
						delete(b.jobs, j)
					}
					b.broadcastLocked()
					b.mu.Unlock()
				})
			}, nil
		}
		if w == nil && j.pods+cpus > j.hold+cpuEps {
			w = &cpuWaiter{cpus: cpus, since: b.now()}
			b.queue = append(b.queue, w)
		}
		var valveAt <-chan time.Time
		if w != nil && len(b.queue) > 0 && b.queue[0] == w {
			valveAt = time.After(max(CPUValveAfter-b.now().Sub(w.since), 0) + 10*time.Millisecond)
		}
		line := ""
		if !said {
			said = true
			line = b.lineLocked()
		}
		ch := b.waitChanLocked()
		logfn := b.logfn
		b.mu.Unlock()
		if line != "" && logfn != nil {
			logfn("cpu budget: %s: a pod of %g cpus waits for room (%s)", j.name, cpus, line)
		}
		select {
		case <-ctx.Done():
			b.mu.Lock()
			if w != nil {
				b.dequeueLocked(w)
				b.broadcastLocked()
			}
			b.mu.Unlock()
			return nil, ctx.Err()
		case <-ch:
		case <-valveAt:
		}
		b.mu.Lock()
	}
}

// fitsLocked says whether cpus more of j's pods fit now, and whether
// only by the valve (w first in turn, no job's pod open).
func (j *CPUJob) fitsLocked(cpus float64, w *cpuWaiter) (ok, valve bool) {
	b := j.b
	inUse := b.inUseLocked()
	if b.total <= 0 || inUse <= cpuEps {
		return true, false
	}
	if j.pods+cpus <= j.hold+cpuEps {
		return inUse+cpus <= b.total+cpuEps, false
	}
	first := len(b.queue) == 0 || (w != nil && b.queue[0] == w)
	if !first {
		return false, false
	}
	after := b.committedLocked() - j.committed() + math.Max(j.hold, j.pods+cpus)
	if after <= b.total+cpuEps && inUse+cpus <= b.total+cpuEps {
		return true, false
	}
	if w != nil && b.now().Sub(w.since) >= CPUValveAfter && b.jobPodsLocked() <= cpuEps {
		return true, true
	}
	return false, false
}

func (b *CPUBudget) dequeueLocked(w *cpuWaiter) {
	for i, q := range b.queue {
		if q == w {
			b.queue = append(b.queue[:i], b.queue[i+1:]...)
			return
		}
	}
}

// End gives the job's hold back; its pods still open give theirs back
// as they go.
func (j *CPUJob) End() {
	if j == nil {
		return
	}
	b := j.b
	b.mu.Lock()
	j.ended, j.hold = true, 0
	if j.npods <= 0 {
		delete(b.jobs, j)
	}
	b.broadcastLocked()
	b.mu.Unlock()
}

// CPUReading is the budget as read now: the heartbeat's `cpuBudget`, the
// admin status and the near-limit line.
type CPUReading struct {
	// Budget is the CPUs pods may reserve.
	Budget float64 `json:"budget"`
	// Reserved is what the open pods reserve, Pods how many they are.
	Reserved float64 `json:"reserved"`
	Pods     int     `json:"pods"`
	// Committed is Reserved plus what the running jobs hold beyond their
	// pods and the claimed sessions' pods not open yet: what a new claim
	// is weighed against.
	Committed float64 `json:"committed"`
	// Jobs is the jobs holding CPUs, Waiting the pods waiting for room.
	Jobs    int `json:"jobs"`
	Waiting int `json:"waiting"`
}

// Read is the budget now; zero for no budget.
func (b *CPUBudget) Read() CPUReading {
	if b == nil {
		return CPUReading{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.readLocked()
}

func (b *CPUBudget) readLocked() CPUReading {
	n := b.npods
	for j := range b.jobs {
		n += j.npods
	}
	return CPUReading{Budget: b.total, Reserved: b.inUseLocked(), Pods: n, Committed: b.committedLocked(), Jobs: len(b.jobs), Waiting: len(b.queue)}
}

// Line is the reading for node.log: `48 of 120 cpus reserved by 6 pods,
// 96 committed, 1 waiting`.
func (r CPUReading) Line() string {
	s := fmt.Sprintf("%g of %g cpus reserved by %d pod(s), %g committed", round2(r.Reserved), round2(r.Budget), r.Pods, round2(r.Committed))
	if r.Waiting > 0 {
		s += fmt.Sprintf(", %d waiting", r.Waiting)
	}
	return s
}

func (b *CPUBudget) lineLocked() string { return b.readLocked().Line() }

func round2(x float64) float64 { return math.Round(x*100) / 100 }

// Near says whether the budget is near its limit: a pod waiting, or
// cpuNear of it reserved or committed.
func (r CPUReading) Near() bool {
	if r.Budget <= 0 {
		return false
	}
	return r.Waiting > 0 || math.Max(r.Reserved, r.Committed) >= cpuNear*r.Budget
}

// SayNear logs the budget at most once a minute while it is near its
// limit.
func (b *CPUBudget) SayNear(logf func(string, ...any)) {
	if b == nil || logf == nil {
		return
	}
	b.mu.Lock()
	r := b.readLocked()
	now := b.now()
	if !r.Near() || (!b.saidAt.IsZero() && now.Sub(b.saidAt) < cpuSayEvery) {
		b.mu.Unlock()
		return
	}
	b.saidAt = now
	b.mu.Unlock()
	logf("cpu budget: near its limit: %s", r.Line())
}

// SetLogf is where the budget says a pod waits or passes it; nil says
// nothing.
func (b *CPUBudget) SetLogf(logf func(string, ...any)) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.logfn = logf
	b.mu.Unlock()
}

func (b *CPUBudget) logf(format string, args ...any) {
	b.mu.Lock()
	f := b.logfn
	b.mu.Unlock()
	if f != nil {
		f(format, args...)
	}
}

// cpuJobKey carries a validation's CPUJob on its context, for the pods
// it opens (OpenSessionPod).
type cpuJobKey struct{}

// WithCPUJob is ctx carrying j.
func WithCPUJob(ctx context.Context, j *CPUJob) context.Context {
	if j == nil {
		return ctx
	}
	return context.WithValue(ctx, cpuJobKey{}, j)
}

// CPUJobOf is the CPUJob ctx carries, nil when none.
func CPUJobOf(ctx context.Context) *CPUJob {
	j, _ := ctx.Value(cpuJobKey{}).(*CPUJob)
	return j
}
