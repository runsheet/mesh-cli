package nodev1

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/crash"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// What a lease used (projectmesh-io/api #219, ADR 001 amendment, placed
// by learned size; ADR-044 amendment, 2026-11-18): a lease's finish and
// fail carry `used`, the CPU time its containers spent and the most
// memory they held, so Mesh can place the next job of its kind by the
// room it really takes rather than the room it declared.
//
// Where it is read. A sandboxed lease runs in pods named for it
// (PodName, its session's later pods PodName-<n>, its lanes
// PodName-lane-<n>), and every container in them is named for its pod:
// the agent, each run_suite step's exec or run container, the services
// and the relay. Rootless podman on cgroupfs (dl360, podman 4.9) gives
// each container a cgroup of its own, flat under the node user's
// app.slice and named by its id. There is no pod or lease cgroup to read
// once, and a container's cgroup goes when the container exits. So the
// node reads them while they run:
//
//   - every useEvery while it holds a lease, one `podman ps` lists the
//     running containers and their pids. Each container named for a held
//     lease has its cgroup read from /proc/<pid>/cgroup's cgroup v2 line,
//     and from it cpu.stat's usage_usec, memory.peak (memory.current on
//     a kernel that keeps no peak) and memory.current;
//   - each container's readings are kept in the lease's record by
//     container, the highest read, so a step's container that has
//     exited keeps what it was last read at;
//   - at the finish or fail the lease's containers are read once more.
//
// cpu_seconds is the sum of each container's usage_usec. mem_peak_gb is
// the larger of the highest single container's peak and the highest sum
// of the lease's containers' memory.current at one reading. Both are what
// the lease held at least, and neither counts two steps that ran one
// after the other as held at once. Both are a little under the truth: a
// container's last seconds after its last reading, and a container that
// lived less than useEvery, go unseen.
//
// None of it may cost the lease its end. No container lister (a node
// without the sandbox), no cgroup v2 line, an unreadable cpu.stat, a
// `podman ps` that fails or a reading that panics leaves nothing read,
// and nothing read sends no `used` at all. A Mesh older than #219 refuses
// the field and is sent the body again without it (meshapi.UsedRefused).
// A cgroup that is the root, the node's own or an ancestor of it, or one
// two containers share is never read: it is not one lease's alone.

// useEvery is how often the node reads its leases' containers' cgroups.
const useEvery = 15 * time.Second

// useReadTimeout bounds one reading: its `podman ps` and its files.
const useReadTimeout = 10 * time.Second

// containerLister lists the running containers with their host pids
// (sandbox.Podman's RunningContainers).
type containerLister interface {
	RunningContainers(ctx context.Context) ([]sandbox.RunningContainer, error)
}

// leaseUse is what a lease's containers were read to have used. It
// lives in the lease's record (running.use), under Runtime.mu, and goes
// with it.
type leaseUse struct {
	// cpuUsec and peak are, by container, the highest usage_usec and
	// memory peak read
	cpuUsec map[string]uint64
	peak    map[string]uint64
	// together is the highest sum of the lease's containers'
	// memory.current at one reading
	together uint64
}

// containerReading is one container's cgroup, read once.
type containerReading struct {
	container string
	cgroup    cgroupReading
}

// cgroupReading is a cgroup's counters: cpu.stat's usage_usec, its
// memory peak and its memory now, in bytes.
type cgroupReading struct {
	cpuUsec, memPeak, memNow uint64
}

// add keeps one reading of the lease's containers.
func (u *leaseUse) add(rs []containerReading) {
	if len(rs) == 0 {
		return
	}
	if u.cpuUsec == nil {
		u.cpuUsec, u.peak = map[string]uint64{}, map[string]uint64{}
	}
	var now uint64
	for _, c := range rs {
		u.cpuUsec[c.container] = max(u.cpuUsec[c.container], c.cgroup.cpuUsec)
		u.peak[c.container] = max(u.peak[c.container], c.cgroup.memPeak)
		now += c.cgroup.memNow
	}
	u.together = max(u.together, now)
}

// used is the lease's LeaseUsed, nil when nothing was read.
func (u *leaseUse) used() *meshapi.LeaseUsed {
	if len(u.cpuUsec) == 0 {
		return nil
	}
	var usec, peak uint64
	for _, v := range u.cpuUsec {
		usec += v
	}
	for _, v := range u.peak {
		peak = max(peak, v)
	}
	peak = max(peak, u.together)
	return &meshapi.LeaseUsed{
		CPUSeconds: math.Round(float64(usec)/1e3) / 1e3,
		MemPeakGB:  math.Round(float64(peak)/(1<<30)*1e3) / 1e3,
	}
}

// lister is the runtime that lists the node's running containers: the
// lease pods' (Pods), else the actor's (Sandbox); nil when neither can.
func (r *Runtime) lister() containerLister {
	if r.containers != nil {
		return r.containers
	}
	if r.Pods != nil {
		if l, ok := r.Pods.Runtime.(containerLister); ok {
			return l
		}
	}
	if l, ok := r.Sandbox.(containerLister); ok {
		return l
	}
	return nil
}

// readUse reads the containers of the leases in ids and keeps each
// lease's reading in its record. A lease no longer held is skipped.
func (r *Runtime) readUse(ctx context.Context, ids []string) error {
	list := r.lister()
	if list == nil || len(ids) == 0 {
		return nil
	}
	got, err := readContainers(ctx, list, cmp.Or(r.cgroupRoot, "/"), ids)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, rs := range got {
		if l := r.lease(id); l != nil {
			l.use.add(rs)
		}
	}
	return nil
}

// watchUse reads the held leases' containers every useEvery until ctx
// ends. A reading that fails is logged once, when readings start
// failing, and one that panics is logged and dropped: what a lease used
// never stops the node.
func (r *Runtime) watchUse(ctx context.Context) {
	defer crash.Guard()
	if r.lister() == nil {
		return
	}
	t := time.NewTicker(useEvery)
	defer t.Stop()
	failing := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := r.readHeldUse(ctx)
		if err != nil && !failing && ctx.Err() == nil {
			r.logf("nodev1: reading the leases' cgroups: %v", err)
		}
		failing = err != nil
	}
}

// readHeldUse is one of watchUse's readings, of every held lease.
func (r *Runtime) readHeldUse(ctx context.Context) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("panic: %v", v)
		}
	}()
	r.mu.Lock()
	var ids []string
	for _, e := range r.leases.All() {
		ids = append(ids, e.ID)
	}
	r.mu.Unlock()
	rctx, cancel := context.WithTimeout(ctx, useReadTimeout)
	defer cancel()
	return r.readUse(rctx, ids)
}

// leaseUsed is what lease id used, its containers read once more now:
// the finish's and fail's `used`, nil when nothing was ever read. It
// never fails the end it is for: an error or a panic is logged and
// sends what was read before, or nothing.
func (r *Runtime) leaseUsed(id string) (used *meshapi.LeaseUsed) {
	defer func() {
		if v := recover(); v != nil {
			r.logf("nodev1: lease %s: reading what it used: panic: %v", id, v)
			used = nil
		}
	}()
	if r.lister() == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), useReadTimeout)
	defer cancel()
	if err := r.readUse(ctx, []string{id}); err != nil {
		r.logf("nodev1: lease %s: reading what it used: %v", id, err)
	}
	if used = r.usedOf(id); used != nil {
		r.logf("nodev1: lease %s used %.1f CPU-seconds, peak %.2f GiB", id, used.CPUSeconds, used.MemPeakGB)
	}
	return used
}

// usedOf is lease id's LeaseUsed as read so far, nil for none.
func (r *Runtime) usedOf(id string) *meshapi.LeaseUsed {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l := r.lease(id); l != nil {
		return l.use.used()
	}
	return nil
}

// readContainers lists the running containers once and reads the cgroup
// of each one named for a lease in ids, under root ("/" on a node), by
// lease. A container with no cgroup v2 line, an unreadable cpu.stat, or
// a cgroup that is not its own alone (leaseCgroup, or shared with
// another container) is left out.
func readContainers(ctx context.Context, list containerLister, root string, ids []string) (map[string][]containerReading, error) {
	cs, err := list.RunningContainers(ctx)
	if err != nil {
		return nil, err
	}
	self := cgroupOf(root, "self")
	type found struct {
		lease, container, cgroup string
	}
	var hits []found
	shared := map[string]int{}
	for _, c := range cs {
		if c.PID <= 0 {
			continue
		}
		// every container's cgroup is counted, any lease's or none, so
		// one shared with a container of another lease is seen as shared
		cg := cgroupOf(root, strconv.Itoa(c.PID))
		if cg == "" {
			continue
		}
		shared[cg]++
		lease := leaseOfContainer(c.Name, ids)
		if lease == "" || !leaseCgroup(cg, self) {
			continue
		}
		hits = append(hits, found{lease: lease, container: cmp.Or(c.ID, c.Name), cgroup: cg})
	}
	out := map[string][]containerReading{}
	for _, h := range hits {
		if shared[h.cgroup] > 1 {
			continue
		}
		reading, ok := readCgroup(filepath.Join(root, "sys", "fs", "cgroup", filepath.FromSlash(h.cgroup)))
		if !ok {
			continue
		}
		out[h.lease] = append(out[h.lease], containerReading{container: h.container, cgroup: reading})
	}
	return out, nil
}

// leaseOfContainer is the lease in ids whose pods a container of that
// name is in: its name is the lease's PodName or starts with it and a
// "-" (the agent, a step's exec or run, a service, a later pod's or a
// lane's); the longest match wins. "" for none.
func leaseOfContainer(name string, ids []string) string {
	best, bestLen := "", 0
	for _, id := range ids {
		p := PodName(id)
		if (name == p || strings.HasPrefix(name, p+"-")) && len(p) > bestLen {
			best, bestLen = id, len(p)
		}
	}
	return best
}

// cgroupOf is the cgroup v2 path of process pid ("self" for this one),
// from root's /proc/<pid>/cgroup "0::" line: "" when there is none (a
// host without cgroup v2, or a process that has gone).
func cgroupOf(root, pid string) string {
	b, err := os.ReadFile(filepath.Join(root, "proc", pid, "cgroup"))
	if err != nil {
		return ""
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if path, ok := strings.CutPrefix(sc.Text(), "0::"); ok {
			return strings.TrimSpace(path)
		}
	}
	return ""
}

// leaseCgroup says whether cg can be one container's alone: an absolute
// path below the root, with no ".." in it, and neither the node's own
// cgroup (self) nor one of its ancestors, which hold more than any one
// lease (a container run with no cgroup of its own).
func leaseCgroup(cg, self string) bool {
	if !strings.HasPrefix(cg, "/") || cg == "/" || strings.Contains(cg, "..") {
		return false
	}
	return self == "" || (self != cg && !strings.HasPrefix(self, cg+"/"))
}

// readCgroup reads one cgroup directory: cpu.stat's usage_usec, which
// must read, and memory.peak and memory.current, each zero when missing
// (no memory controller); a kernel that keeps no memory.peak (before
// 5.19) has memory.current stand in for it.
func readCgroup(dir string) (cgroupReading, bool) {
	var r cgroupReading
	usec, ok := cpuUsage(filepath.Join(dir, "cpu.stat"))
	if !ok {
		return r, false
	}
	r.cpuUsec = usec
	r.memNow, _ = readCount(filepath.Join(dir, "memory.current"))
	peak, _ := readCount(filepath.Join(dir, "memory.peak"))
	r.memPeak = max(peak, r.memNow)
	return r, true
}

// cpuUsage is cpu.stat's usage_usec.
func cpuUsage(path string) (uint64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && f[0] == "usage_usec" {
			n, err := strconv.ParseUint(f[1], 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

// readCount reads a cgroup file holding one count.
func readCount(path string) (uint64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return n, err == nil
}
