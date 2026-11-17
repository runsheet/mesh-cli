package nodev1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/actor"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

const (
	usedLease  = "a609904d-43e3-525d-9a41-456ed68bbcc3"
	otherLease = "e4b9d7de-e44a-5bb3-81fe-209e09d93fe3"
	// the node user's app.slice on dl360, where rootless podman on
	// cgroupfs puts every container's cgroup, named by its id
	appSlice = "/user.slice/user-1003.slice/user@1003.service/app.slice"
	gib      = 1 << 30
)

// fakeCgroups is a fixture /proc and /sys/fs/cgroup, laid out as dl360
// has them: the node in its service's cgroup, each container in a
// cgroup of its own beside it.
type fakeCgroups struct {
	t    *testing.T
	root string
}

func newFakeCgroups(t *testing.T) *fakeCgroups {
	f := &fakeCgroups{t: t, root: t.TempDir()}
	f.proc("self", "0::"+appSlice+"/_io.projectmesh.mesh-node-v1.service\n")
	return f
}

// proc writes /proc/<pid>/cgroup.
func (f *fakeCgroups) proc(pid, cgroup string) {
	f.write(filepath.Join("proc", pid, "cgroup"), cgroup)
}

// cgroup writes a cgroup's files, each name to its content.
func (f *fakeCgroups) cgroup(path string, files map[string]string) {
	for name, body := range files {
		f.write(filepath.Join("sys", "fs", "cgroup", filepath.FromSlash(path), name), body)
	}
}

// container gives pid a cgroup of its own named id, with cpu.stat's
// usage_usec and, when not negative, memory.peak and memory.current.
func (f *fakeCgroups) container(pid int, id string, usec, peak, now int64) {
	path := appSlice + "/" + id
	f.proc(strconv.Itoa(pid), "0::"+path+"\n")
	files := map[string]string{"cpu.stat": "usage_usec " + strconv.FormatInt(usec, 10) + "\nuser_usec 0\nsystem_usec 0\n"}
	if peak >= 0 {
		files["memory.peak"] = strconv.FormatInt(peak, 10) + "\n"
	}
	if now >= 0 {
		files["memory.current"] = strconv.FormatInt(now, 10) + "\n"
	}
	f.cgroup(path, files)
}

// gone removes a container's cgroup, as its exit does.
func (f *fakeCgroups) gone(id string) {
	if err := os.RemoveAll(filepath.Join(f.root, "sys", "fs", "cgroup", filepath.FromSlash(appSlice), id)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeCgroups) write(rel, body string) {
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// fakeLister is a `podman ps` that answers what a test sets.
type fakeLister struct {
	mu    sync.Mutex
	cs    []sandbox.RunningContainer
	err   error
	calls int
}

func (l *fakeLister) RunningContainers(context.Context) ([]sandbox.RunningContainer, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	return append([]sandbox.RunningContainer(nil), l.cs...), l.err
}

func (l *fakeLister) set(cs ...sandbox.RunningContainer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cs = cs
}

// usedRuntime is a node holding the leases in ids, reading its
// containers through list under fs.
func usedRuntime(t *testing.T, fs *fakeCgroups, list containerLister, ids ...string) *Runtime {
	r := &Runtime{WorkRoot: t.TempDir(), Logf: t.Logf, containers: list}
	if fs != nil {
		r.cgroupRoot = fs.root
	}
	for _, id := range ids {
		r.leases.Put(id, &running{jobID: "job-" + id})
	}
	return r
}

func ctr(id, name string, pid int) sandbox.RunningContainer {
	return sandbox.RunningContainer{ID: id, Name: name, PID: pid}
}

// A lease's use is every container named for its pods: the agent, a
// service, a lane's step. Its CPU is their usage_usec summed; its peak
// the larger of one container's memory.peak and their memory.current
// together. Another lease's containers and a pod's infra are not its.
func TestLeaseUsedReadsItsContainersCgroups(t *testing.T) {
	fs := newFakeCgroups(t)
	pod := PodName(usedLease)
	fs.container(101, "agent0", 10_000_000, 1*gib, gib/2)
	fs.container(102, "pg0", 5_000_000, gib/2, gib/4)
	fs.container(103, "lane0", 20_000_000, 2*gib, 2*gib)
	fs.container(201, "other0", 99_000_000, 8*gib, 8*gib)
	fs.container(301, "infra0", 7_000, 1<<20, 1<<20)
	list := &fakeLister{}
	list.set(
		ctr("agent0", sandbox.AgentName(sandbox.Pod{Name: pod}), 101),
		ctr("pg0", pod+"-postgres", 102),
		ctr("lane0", pod+"-lane-1-exec-1", 103),
		ctr("other0", PodName(otherLease)+"-agent", 201),
		ctr("infra0", "53c64adb78ae-infra", 301),
	)
	r := usedRuntime(t, fs, list, usedLease, otherLease)

	got := r.leaseUsed(usedLease)
	want := &meshapi.LeaseUsed{CPUSeconds: 35, MemPeakGB: 2.75}
	if got == nil || *got != *want {
		t.Fatalf("used %+v, want %+v", got, want)
	}
}

// A step's container that exits between readings keeps what it was last
// read at; one still running is read at its newest.
func TestLeaseUsedKeepsAContainerThatExited(t *testing.T) {
	fs := newFakeCgroups(t)
	pod := PodName(usedLease)
	fs.container(101, "agent0", 4_000_000, gib, gib)
	fs.container(102, "step1", 30_000_000, 3*gib, 3*gib)
	list := &fakeLister{}
	list.set(ctr("agent0", pod+"-agent", 101), ctr("step1", pod+"-exec-1", 102))
	r := usedRuntime(t, fs, list, usedLease)
	if err := r.readHeldUse(context.Background()); err != nil {
		t.Fatal(err)
	}

	// the step ended and its cgroup went with it; the agent ran on
	fs.gone("step1")
	fs.container(101, "agent0", 6_000_000, gib, gib)
	list.set(ctr("agent0", pod+"-agent", 101))
	got := r.leaseUsed(usedLease)
	want := &meshapi.LeaseUsed{CPUSeconds: 36, MemPeakGB: 4}
	if got == nil || *got != *want {
		t.Fatalf("used %+v, want %+v", got, want)
	}
}

// A cgroup file that is not there costs the lease nothing but that
// reading: no memory.peak (a kernel before 5.19) reads memory.current;
// no memory files reads the CPU alone; no cpu.stat, no /proc entry, no
// cgroup v2 line, a listing that fails or no listing at all reads
// nothing, and nothing read is no `used`.
func TestLeaseUsedMissingFiles(t *testing.T) {
	pod := PodName(usedLease)
	cases := []struct {
		name  string
		setup func(fs *fakeCgroups, list *fakeLister)
		want  *meshapi.LeaseUsed
	}{
		{"no memory.peak", func(fs *fakeCgroups, list *fakeLister) {
			fs.container(101, "a", 2_000_000, -1, gib)
			list.set(ctr("a", pod+"-agent", 101))
		}, &meshapi.LeaseUsed{CPUSeconds: 2, MemPeakGB: 1}},
		{"no memory files", func(fs *fakeCgroups, list *fakeLister) {
			fs.container(101, "a", 2_500_000, -1, -1)
			list.set(ctr("a", pod+"-agent", 101))
		}, &meshapi.LeaseUsed{CPUSeconds: 2.5}},
		{"no cpu.stat", func(fs *fakeCgroups, list *fakeLister) {
			fs.proc("101", "0::"+appSlice+"/a\n")
			fs.cgroup(appSlice+"/a", map[string]string{"memory.peak": "1024\n"})
			list.set(ctr("a", pod+"-agent", 101))
		}, nil},
		{"a cpu.stat with no usage_usec", func(fs *fakeCgroups, list *fakeLister) {
			fs.proc("101", "0::"+appSlice+"/a\n")
			fs.cgroup(appSlice+"/a", map[string]string{"cpu.stat": "user_usec 5\n"})
			list.set(ctr("a", pod+"-agent", 101))
		}, nil},
		{"the process has gone", func(fs *fakeCgroups, list *fakeLister) {
			list.set(ctr("a", pod+"-agent", 101))
		}, nil},
		{"no cgroup v2 line", func(fs *fakeCgroups, list *fakeLister) {
			fs.proc("101", "12:cpu,cpuacct:/a\n4:memory:/a\n")
			list.set(ctr("a", pod+"-agent", 101))
		}, nil},
		{"no cgroup filesystem", func(fs *fakeCgroups, list *fakeLister) {
			fs.proc("101", "0::"+appSlice+"/a\n")
			list.set(ctr("a", pod+"-agent", 101))
		}, nil},
		{"no pid", func(fs *fakeCgroups, list *fakeLister) {
			fs.container(101, "a", 2_000_000, gib, gib)
			list.set(ctr("a", pod+"-agent", 0))
		}, nil},
		{"the listing fails", func(fs *fakeCgroups, list *fakeLister) {
			fs.container(101, "a", 2_000_000, gib, gib)
			list.set(ctr("a", pod+"-agent", 101))
			list.err = errors.New("podman ps: signal: killed")
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeCgroups(t)
			list := &fakeLister{}
			tc.setup(fs, list)
			r := usedRuntime(t, fs, list, usedLease)
			got := r.leaseUsed(usedLease)
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("used %+v, want %+v", got, tc.want)
			}
		})
	}
	t.Run("no lister", func(t *testing.T) {
		r := usedRuntime(t, nil, nil, usedLease)
		if got := r.leaseUsed(usedLease); got != nil {
			t.Fatalf("used %+v with nothing to list containers", got)
		}
		if err := r.readHeldUse(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a lease the node does not hold", func(t *testing.T) {
		fs := newFakeCgroups(t)
		fs.container(101, "a", 2_000_000, gib, gib)
		list := &fakeLister{}
		list.set(ctr("a", pod+"-agent", 101))
		r := usedRuntime(t, fs, list)
		if got := r.leaseUsed(usedLease); got != nil {
			t.Fatalf("used %+v for a lease with no record", got)
		}
	})
}

// A cgroup that is not one container's alone is never read: the root,
// the node's own or an ancestor of it, and one two containers share.
func TestLeaseUsedSkipsCgroupsNotTheLeasesAlone(t *testing.T) {
	pod := PodName(usedLease)
	cases := map[string]func(fs *fakeCgroups, list *fakeLister){
		"the root": func(fs *fakeCgroups, list *fakeLister) {
			fs.proc("101", "0::/\n")
			fs.cgroup("/", map[string]string{"cpu.stat": "usage_usec 9000000000\n"})
			list.set(ctr("a", pod+"-agent", 101))
		},
		"the node's own": func(fs *fakeCgroups, list *fakeLister) {
			self := appSlice + "/_io.projectmesh.mesh-node-v1.service"
			fs.proc("101", "0::"+self+"\n")
			fs.cgroup(self, map[string]string{"cpu.stat": "usage_usec 9000000000\n"})
			list.set(ctr("a", pod+"-agent", 101))
		},
		"an ancestor of the node's": func(fs *fakeCgroups, list *fakeLister) {
			fs.proc("101", "0::"+appSlice+"\n")
			fs.cgroup(appSlice, map[string]string{"cpu.stat": "usage_usec 9000000000\n"})
			list.set(ctr("a", pod+"-agent", 101))
		},
		"one two containers share": func(fs *fakeCgroups, list *fakeLister) {
			fs.container(101, "shared", 9_000_000_000, gib, gib)
			fs.proc("102", "0::"+appSlice+"/shared\n")
			list.set(ctr("a", pod+"-agent", 101), ctr("b", PodName(otherLease)+"-agent", 102))
		},
		"a path that climbs": func(fs *fakeCgroups, list *fakeLister) {
			fs.proc("101", "0::/user.slice/../../etc\n")
			list.set(ctr("a", pod+"-agent", 101))
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			fs := newFakeCgroups(t)
			list := &fakeLister{}
			setup(fs, list)
			r := usedRuntime(t, fs, list, usedLease, otherLease)
			if got := r.leaseUsed(usedLease); got != nil {
				t.Fatalf("used %+v from a cgroup that is not the lease's alone", got)
			}
		})
	}
}

// A container is the lease's whose pods it is in: the pod itself, or a
// name that goes on from it with a "-". Another lease's pod, even one
// whose id starts the same, is not.
func TestLeaseOfContainer(t *testing.T) {
	ids := []string{"l1", "l10", usedLease}
	for name, want := range map[string]string{
		"mesh-v1-l1":                       "l1",
		"mesh-v1-l1-agent":                 "l1",
		"mesh-v1-l1-lane-2-run-3":          "l1",
		"mesh-v1-l10-agent":                "l10",
		"mesh-v1-l100-agent":               "",
		"mesh-v1-l1x":                      "",
		"53c64adb78ae-infra":               "",
		PodName(usedLease) + "-1-postgres": usedLease,
	} {
		if got := leaseOfContainer(name, ids); got != want {
			t.Errorf("leaseOfContainer(%q) = %q, want %q", name, got, want)
		}
	}
}

// meshRecorder answers every lease route 200, recording each body by
// route. strict, set, decodes finish and fail as a Mesh without
// projectmesh-io/api #219 does: a field it does not know is a 400.
type meshRecorder struct {
	mu     sync.Mutex
	strict bool
	bodies map[string][]json.RawMessage
}

func (m *meshRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	route := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	m.mu.Lock()
	if m.bodies == nil {
		m.bodies = map[string][]json.RawMessage{}
	}
	m.bodies[route] = append(m.bodies[route], raw)
	m.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if m.strict && (route == "finish" || route == "fail") {
		var old struct {
			Result     json.RawMessage `json:"result"`
			SessionRef string          `json:"session_ref"`
			Evidence   []string        `json:"evidence"`
			Cause      string          `json:"cause"`
			Reason     string          `json:"reason"`
			ExitCode   int             `json:"exit_code"`
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&old); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "bad_request", "message": "the body does not parse: " + err.Error()})
			return
		}
	}
	json.NewEncoder(w).Encode(meshapi.JobState{JobID: "job-1", Status: "ended", End: "finished"})
}

func (m *meshRecorder) sent(route string) []json.RawMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bodies[route]
}

// usedOf is the `used` a body carries, nil for none.
func usedOf(t *testing.T, body json.RawMessage) *meshapi.LeaseUsed {
	t.Helper()
	var b struct {
		Used *meshapi.LeaseUsed `json:"used"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatal(err)
	}
	return b.Used
}

// The finish and a fail carry what the lease used; a hand-back, which
// Mesh refuses it on, never does; a node with nothing read sends no
// `used` key at all; and a Mesh older than #219, which refuses the
// field, still has the lease finished and failed, sent the body again
// without it.
func TestFinishAndFailCarryWhatTheLeaseUsed(t *testing.T) {
	setup := func(t *testing.T, strict bool, list containerLister) (*Runtime, *meshRecorder) {
		fs := newFakeCgroups(t)
		fs.container(101, "agent0", 12_500_000, gib, gib/2)
		if fl, ok := list.(*fakeLister); ok {
			fl.set(ctr("agent0", PodName(usedLease)+"-agent", 101))
		}
		rec := &meshRecorder{strict: strict}
		srv := httptest.NewServer(rec)
		t.Cleanup(srv.Close)
		r := usedRuntime(t, fs, list, usedLease)
		r.API = &meshapi.Client{BaseURL: srv.URL, Token: func(context.Context) (string, error) { return "node", nil }}
		return r, rec
	}
	brief := &meshapi.Brief{JobID: "job-1", LeaseID: usedLease, Kind: "validate"}
	want := meshapi.LeaseUsed{CPUSeconds: 12.5, MemPeakGB: 1}

	t.Run("finish", func(t *testing.T) {
		r, rec := setup(t, false, &fakeLister{})
		end := r.report(usedLease, brief, outcome{finish: json.RawMessage(`{"verdict":"green"}`)})
		if end.Report != actor.ReportFinish || end.Reason != "" {
			t.Fatalf("end %+v", end)
		}
		bodies := rec.sent("finish")
		if len(bodies) != 1 {
			t.Fatalf("%d finishes", len(bodies))
		}
		if got := usedOf(t, bodies[0]); got == nil || *got != want {
			t.Fatalf("the finish's used %+v, want %+v", got, want)
		}
	})
	t.Run("fail", func(t *testing.T) {
		r, rec := setup(t, false, &fakeLister{})
		r.fail(usedLease, meshapi.CauseStoppedByRule, "a stop rule fired")
		bodies := rec.sent("fail")
		if len(bodies) != 1 {
			t.Fatalf("%d fails", len(bodies))
		}
		if got := usedOf(t, bodies[0]); got == nil || *got != want {
			t.Fatalf("the fail's used %+v, want %+v", got, want)
		}
	})
	t.Run("hand-back", func(t *testing.T) {
		r, rec := setup(t, false, &fakeLister{})
		r.handBack(usedLease, meshapi.CauseStalled, "quiet", "")
		bodies := rec.sent("hand-back")
		if len(bodies) != 1 || bytes.Contains(bodies[0], []byte(`"used"`)) {
			t.Fatalf("hand-back bodies %s", bodies)
		}
	})
	t.Run("nothing read", func(t *testing.T) {
		r, rec := setup(t, false, nil)
		r.report(usedLease, brief, outcome{finish: json.RawMessage(`{}`)})
		r.fail(usedLease, meshapi.CauseStoppedByRule, "a stop rule fired")
		for _, route := range []string{"finish", "fail"} {
			bodies := rec.sent(route)
			if len(bodies) != 1 || bytes.Contains(bodies[0], []byte(`"used"`)) {
				t.Fatalf("%s bodies %s, want one with no used", route, bodies)
			}
		}
	})
	t.Run("a Mesh without #219", func(t *testing.T) {
		r, rec := setup(t, true, &fakeLister{})
		end := r.report(usedLease, brief, outcome{finish: json.RawMessage(`{}`)})
		if end.Report != actor.ReportFinish || end.Reason != "" {
			t.Fatalf("end %+v: the finish was lost to used", end)
		}
		r.fail(usedLease, meshapi.CauseStoppedByRule, "a stop rule fired")
		for _, route := range []string{"finish", "fail"} {
			bodies := rec.sent(route)
			if len(bodies) != 2 || usedOf(t, bodies[0]) == nil || usedOf(t, bodies[1]) != nil {
				t.Fatalf("%s bodies %s, want one with used and then one without", route, bodies)
			}
		}
	})
}
