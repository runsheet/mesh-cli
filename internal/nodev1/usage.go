package nodev1

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/crash"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// What each kind's leases use (goal
// a-v1-node-reserves-room-for-validations-and-sizes-its-reserves-from-use
// P3): the node reads its leases' containers while they run, keeps each
// lease's CPU and memory peaks in its record, and at the lease's end
// folds them into its kind's: how many ran, the most that ran at once,
// and the peaks' median, 95th percentile and highest. `mesh node
// status` (Status.Usage) reports them and node.log has a line per
// lease, so every reserve can be sized from a day's data (P4).
//
// Where it is read. A sandboxed lease runs in pods named for it
// (PodName, its later pods and its lanes PodName-…), and every container
// in them is named for its pod. Rootless podman on cgroupfs (dl360)
// gives each container a cgroup of its own and removes it as the
// container exits, so the node reads them while they run: every
// peakEvery one `podman ps` lists the running containers and their
// pids, and each named for a held lease has its cgroup read from
// /proc/<pid>/cgroup — cpu.stat's usage_usec, memory.current and
// memory.peak.
//
// A lease's CPU peak is the most cores its containers used together
// over one reading's window: the usage_usec they gained since the last
// reading (a container first seen, all it used) over the time between.
// Its memory peak is the larger of its containers' memory.current
// summed at one reading and any one container's memory.peak. Both are
// a little low: a container that lived less than peakEvery, and a
// burst shorter than one window, go unseen.
//
// None of it costs a lease anything: no meter (a node without the
// sandbox), a `podman ps` that fails or a reading that panics leaves
// nothing read, and concurrency is counted whatever is read.

// peakEvery is how often the node reads its leases' containers.
const peakEvery = 15 * time.Second

// peaksKept bounds the lease peaks a kind keeps for its percentiles.
const peaksKept = 500

// ContainerUse is one running container's cgroup counters.
type ContainerUse struct {
	Name    string
	CPUUsec uint64
	MemNow  uint64
	MemPeak uint64
}

// UsageMeter reads the node's running containers' cgroups.
type UsageMeter interface {
	Read(ctx context.Context) ([]ContainerUse, error)
}

// leasePeaks is what a lease's containers were read to have used, kept
// in its record (running.peaks), under Runtime.mu.
type leasePeaks struct {
	read bool
	at   time.Time
	// usec is each container's usage_usec at the last reading
	usec map[string]uint64
	// cpu is the most cores used at once; mem the most bytes held
	cpu float64
	mem uint64
	// nowCPU and nowMem are the latest reading alone, not the peak:
	// nowCPU the cores used over the window up to it, nowMem the
	// containers' summed memory.current at it (`mesh node status`'s
	// "now", beside "peak").
	nowCPU float64
	nowMem uint64
}

// add keeps one reading of the lease's containers at now; started is
// when the lease began, the window of its first reading.
func (p *leasePeaks) add(now, started time.Time, cs []ContainerUse) {
	if len(cs) == 0 {
		return
	}
	since := started
	if p.read {
		since = p.at
	}
	var gained, together, one uint64
	usec := make(map[string]uint64, len(cs))
	for _, c := range cs {
		usec[c.Name] = c.CPUUsec
		if prev, ok := p.usec[c.Name]; ok {
			if c.CPUUsec > prev {
				gained += c.CPUUsec - prev
			}
		} else {
			// started since the last reading: all it used is the window's
			gained += c.CPUUsec
		}
		together += c.MemNow
		one = max(one, c.MemPeak)
	}
	if w := now.Sub(since).Seconds(); w > 0 {
		cores := float64(gained) / 1e6 / w
		p.nowCPU = math.Min(cores, float64(runtime.NumCPU()))
		p.cpu = max(p.cpu, p.nowCPU)
	}
	p.nowMem = together
	p.mem = max(p.mem, together, one)
	p.usec, p.at, p.read = usec, now, true
}

// kindUse is one kind's leases as the node has run them.
type kindUse struct {
	ended, running, atOnce int
	// cpu and mem are the measured leases' peaks, the newest peaksKept
	cpu, mem []float64
}

// usageBook is the node's leases by kind since it started: what each
// kind used, outliving every lease (no lease's in-flight state).
type usageBook struct {
	mu    sync.Mutex
	since time.Time
	kinds map[string]*kindUse
}

func (b *usageBook) of(kind string) *kindUse {
	if b.kinds == nil {
		b.kinds = map[string]*kindUse{}
		if b.since.IsZero() {
			b.since = time.Now()
		}
	}
	k := b.kinds[kind]
	if k == nil {
		k = &kindUse{}
		b.kinds[kind] = k
	}
	return k
}

func (b *usageBook) began(kind string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	k := b.of(kind)
	k.running++
	k.atOnce = max(k.atOnce, k.running)
}

func (b *usageBook) ended(kind string, p leasePeaks) {
	b.mu.Lock()
	defer b.mu.Unlock()
	k := b.of(kind)
	k.running = max(k.running-1, 0)
	k.ended++
	if !p.read {
		return
	}
	k.cpu = appendKept(k.cpu, p.cpu)
	k.mem = appendKept(k.mem, float64(p.mem)/(1<<30))
}

func appendKept(xs []float64, x float64) []float64 {
	xs = append(xs, x)
	if len(xs) > peaksKept {
		xs = xs[len(xs)-peaksKept:]
	}
	return xs
}

// KindUsage is one kind's leases as `mesh node status` reports them:
// how many ended, run now and ran at most at once since the node
// started, how many of the ended were measured, and their CPU (cores)
// and memory (GiB) peaks' median, 95th percentile and highest.
type KindUsage struct {
	Kind      string    `json:"kind"`
	Ended     int       `json:"ended"`
	Running   int       `json:"running"`
	MaxAtOnce int       `json:"max_at_once"`
	Measured  int       `json:"measured"`
	CPUP50    float64   `json:"cpu_p50"`
	CPUP95    float64   `json:"cpu_p95"`
	CPUMax    float64   `json:"cpu_max"`
	MemGBP50  float64   `json:"mem_gb_p50"`
	MemGBP95  float64   `json:"mem_gb_p95"`
	MemGBMax  float64   `json:"mem_gb_max"`
	Since     time.Time `json:"since"`
}

func (b *usageBook) report() []KindUsage {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]KindUsage, 0, len(b.kinds))
	for kind, k := range b.kinds {
		u := KindUsage{Kind: kind, Ended: k.ended, Running: k.running, MaxAtOnce: k.atOnce, Measured: len(k.cpu), Since: b.since.UTC()}
		u.CPUP50, u.CPUP95, u.CPUMax = quantiles(k.cpu)
		u.MemGBP50, u.MemGBP95, u.MemGBMax = quantiles(k.mem)
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// quantiles is xs's median, 95th percentile (nearest rank) and highest,
// each to two places.
func quantiles(xs []float64) (p50, p95, top float64) {
	return percentile(xs, 0.5), percentile(xs, 0.95), percentile(xs, 1)
}

// percentile is xs's qth percentile (nearest rank), to two places; 0
// for an empty xs.
func percentile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	i := int(math.Ceil(q*float64(len(s)))) - 1
	return math.Round(s[max(min(i, len(s)-1), 0)]*100) / 100
}

// kindPercentiles is kind's measured CPU (cores) and memory (GiB) peaks'
// 75th and 95th percentiles, and how many leases of it are measured;
// measured 0, with both percentiles 0, for a kind the node has run none
// of, or measured none of, yet. Used to learn a kind's request
// (learnedrequest.go) without handing out usageBook's own slices, which
// append grows and replaces under its lock.
func (b *usageBook) kindPercentiles(kind string) (cpuP75, memP95 float64, measured int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	k := b.kinds[kind]
	if k == nil {
		return 0, 0, 0
	}
	return percentile(k.cpu, 0.75), percentile(k.mem, 0.95), len(k.cpu)
}

// usageBegan counts a lease of kind as running.
func (r *Runtime) usageBegan(kind string) {
	r.usage.began(path.Base(kind))
}

// usageEnded reads lease id's containers once more and folds its peaks
// into its kind's, with a line in node.log; called as the lease leaves
// the registry.
func (r *Runtime) usageEnded(id string) {
	r.readPeaks(context.Background(), id)
	r.mu.Lock()
	l := r.lease(id)
	if l == nil {
		r.mu.Unlock()
		return
	}
	kind, p, began := path.Base(l.kind), l.peaks, l.started
	r.mu.Unlock()
	r.usage.ended(kind, p)
	if p.read {
		r.logf("nodev1: lease %s (%s) peaks: %.2f cpus, %.2f GB over %s", id, kind, p.cpu, float64(p.mem)/(1<<30), time.Since(began).Round(time.Second))
	}
}

// readPeaks reads the containers of the leases in only (every held
// lease when none) once, keeping each lease's reading in its record. It
// never fails or panics its caller: an error or a panic is logged.
func (r *Runtime) readPeaks(ctx context.Context, only ...string) {
	if r.Meter == nil {
		return
	}
	defer func() {
		if v := recover(); v != nil {
			r.logf("nodev1: reading the leases' containers: panic: %v", v)
		}
	}()
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cs, err := r.Meter.Read(rctx)
	if err != nil {
		if ctx.Err() == nil {
			r.logf("nodev1: reading the leases' containers: %v", err)
		}
		return
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.leases.All() {
		if len(only) > 0 && !containsStr(only, e.ID) {
			continue
		}
		pod := PodName(e.ID)
		var mine []ContainerUse
		for _, c := range cs {
			if c.Name == pod || strings.HasPrefix(c.Name, pod+"-") {
				mine = append(mine, c)
			}
		}
		e.Value.peaks.add(now, e.Value.started, mine)
	}
}

func containsStr(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// watchPeaks reads the held leases' containers every peakEvery until
// ctx ends.
func (r *Runtime) watchPeaks(ctx context.Context) {
	defer crash.Guard()
	if r.Meter == nil {
		return
	}
	t := time.NewTicker(peakEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if r.leases.Len() > 0 {
			r.readPeaks(ctx)
		}
	}
}

// PodmanMeter is the UsageMeter of a node's rootless podman: `podman
// ps` for the running containers' names and pids, and each one's cgroup
// v2 files under CgroupRoot (/sys/fs/cgroup by default).
type PodmanMeter struct {
	Podman     *sandbox.Podman
	CgroupRoot string
	// Proc is /proc by default.
	Proc string
}

// Read implements UsageMeter. A container whose cgroup cannot be read
// (it exited between the listing and the read) is left out.
func (m PodmanMeter) Read(ctx context.Context) ([]ContainerUse, error) {
	out, err := m.Podman.Output(ctx, []string{"ps", "--format", "json"})
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Names []string `json:"Names"`
		Pid   int      `json:"Pid"`
	}
	if strings.TrimSpace(out) != "" {
		if err := json.Unmarshal([]byte(out), &rows); err != nil {
			return nil, fmt.Errorf("podman ps: %w", err)
		}
	}
	root, proc := m.CgroupRoot, m.Proc
	if root == "" {
		root = "/sys/fs/cgroup"
	}
	if proc == "" {
		proc = "/proc"
	}
	var cs []ContainerUse
	for _, row := range rows {
		if len(row.Names) == 0 || row.Pid <= 0 {
			continue
		}
		cg, err := cgroupOf(filepath.Join(proc, strconv.Itoa(row.Pid), "cgroup"))
		if err != nil || cg == "/" || cg == "" {
			continue
		}
		dir := filepath.Join(root, cg)
		usec, err := cpuUsec(filepath.Join(dir, "cpu.stat"))
		if err != nil {
			continue
		}
		now := readUint(filepath.Join(dir, "memory.current"))
		peak := readUint(filepath.Join(dir, "memory.peak"))
		cs = append(cs, ContainerUse{Name: row.Names[0], CPUUsec: usec, MemNow: now, MemPeak: max(peak, now)})
	}
	return cs, nil
}

// cgroupOf is a /proc/<pid>/cgroup file's cgroup v2 path (its "0::"
// line).
func cgroupOf(file string) (string, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if p, ok := strings.CutPrefix(sc.Text(), "0::"); ok {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: no cgroup v2 line", file)
}

func cpuUsec(file string) (uint64, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "usage_usec "); ok {
			return strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		}
	}
	return 0, fmt.Errorf("%s: no usage_usec", file)
}

func readUint(file string) uint64 {
	b, err := os.ReadFile(file)
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return v
}
