package sessionunit

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// noPID is a pid no process has (above the kernel's pid_max).
const noPID = 1 << 30

// fakeSystemctl is a user manager with one scope in it.
type fakeSystemctl struct {
	mu sync.Mutex
	// loaded is whether the scope is there; stop unloads it unless
	// stuck.
	loaded, stuck bool
	// procs are the processes in its cgroup; SIGTERM leaves those whose
	// pid is in ignoreTerm, SIGKILL and stop leave none.
	procs      []Process
	ignoreTerm map[int]bool
	calls      []string
}

func (f *fakeSystemctl) log(c string) { f.calls = append(f.calls, c) }

func (f *fakeSystemctl) Show(unit string, props ...string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("show " + strings.Join(props, ",") + " " + unit)
	if !f.loaded {
		return map[string]string{"LoadState": "not-found", "ActiveState": "inactive", "ControlGroup": ""}, nil
	}
	return map[string]string{"LoadState": "loaded", "ActiveState": "active", "ControlGroup": "/user.slice/" + unit}, nil
}

func (f *fakeSystemctl) Kill(unit, signal string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("kill " + signal + " " + unit)
	var left []Process
	for _, p := range f.procs {
		if signal == "SIGTERM" && f.ignoreTerm[p.PID] {
			left = append(left, p)
		}
	}
	f.procs = left
	return nil
}

func (f *fakeSystemctl) Stop(unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("stop " + unit)
	f.procs = nil
	if !f.stuck {
		f.loaded = false
	}
	return nil
}

func (f *fakeSystemctl) ListScopes() ([]Scope, error) { return nil, nil }

func (f *fakeSystemctl) Processes(cgroup string) ([]Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("procs " + cgroup)
	return append([]Process(nil), f.procs...), nil
}

// signals is the calls that act on or wait for the unit, in order: the
// kills, the stop and the state shows.
func (f *fakeSystemctl) signals() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "kill ") || strings.HasPrefix(c, "stop ") || strings.HasPrefix(c, "show LoadState,ActiveState ") {
			out = append(out, c)
		}
	}
	return out
}

func fastStop(t *testing.T) {
	g, s, p := StopGrace, goneSlack, pollEvery
	StopGrace, goneSlack, pollEvery = 100*time.Millisecond, 100*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { StopGrace, goneSlack, pollEvery = g, s, p })
}

// A scope holding the leader and two more is stopped as a unit: the two
// are reported, SIGTERM goes first, SIGKILL for the one that ignored
// it, then the stop, then the wait until the unit is gone.
func TestSystemdScopeKillReportsLeftovers(t *testing.T) {
	fastStop(t)
	f := &fakeSystemctl{loaded: true, procs: []Process{
		{PID: noPID, Comm: "claude"},
		{PID: noPID + 1, Comm: "go", Cmdline: "go run ./cmd/runsheet -addr :18211"},
		{PID: noPID + 2, Comm: "bash"},
	}, ignoreTerm: map[int]bool{noPID + 1: true}}
	u := "mesh-session-build-item_3-59082a54"
	killed, err := SystemdScope{Systemctl: f}.Kill(Handle{PID: noPID, UnitName: u})
	if err != nil {
		t.Fatal(err)
	}
	want := []Process{
		{PID: noPID + 1, Comm: "go", Cmdline: "go run ./cmd/runsheet -addr :18211"},
		{PID: noPID + 2, Comm: "bash"},
	}
	if !reflect.DeepEqual(killed, want) {
		t.Fatalf("killed %v, want %v", killed, want)
	}
	sc := u + ".scope"
	wantCalls := []string{
		"show LoadState,ActiveState " + sc,
		"kill SIGTERM " + sc,
		"kill SIGKILL " + sc,
		"stop " + sc,
		"show LoadState,ActiveState " + sc,
	}
	if got := f.signals(); !reflect.DeepEqual(got, wantCalls) {
		t.Fatalf("calls\n got %q\nwant %q", got, wantCalls)
	}
}

// Every process leaving on SIGTERM means no SIGKILL; the stop and the
// wait still follow.
func TestSystemdScopeKillTermIsEnough(t *testing.T) {
	fastStop(t)
	f := &fakeSystemctl{loaded: true, procs: []Process{{PID: noPID, Comm: "claude"}}}
	killed, err := SystemdScope{Systemctl: f}.Kill(Handle{PID: noPID, UnitName: "u"})
	if err != nil || killed != nil {
		t.Fatalf("killed %v, err %v", killed, err)
	}
	want := []string{"show LoadState,ActiveState u.scope", "kill SIGTERM u.scope", "stop u.scope", "show LoadState,ActiveState u.scope"}
	if got := f.signals(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls\n got %q\nwant %q", got, want)
	}
}

// A scope already gone — the ordinary end of a clean session — is
// nothing to do.
func TestSystemdScopeKillGone(t *testing.T) {
	fastStop(t)
	f := &fakeSystemctl{}
	for range 2 {
		killed, err := SystemdScope{Systemctl: f}.Kill(Handle{PID: noPID, UnitName: "u"})
		if err != nil || killed != nil {
			t.Fatalf("killed %v, err %v", killed, err)
		}
	}
	if got := f.signals(); !reflect.DeepEqual(got, []string{"show LoadState,ActiveState u.scope", "show LoadState,ActiveState u.scope"}) {
		t.Fatalf("calls %q", got)
	}
}

// A unit still loaded after the wait is an error naming it.
func TestSystemdScopeKillStuck(t *testing.T) {
	fastStop(t)
	f := &fakeSystemctl{loaded: true, stuck: true, procs: []Process{{PID: noPID, Comm: "claude"}}}
	u := "mesh-session-build-item_3-59082a54"
	_, err := SystemdScope{Systemctl: f}.Kill(Handle{PID: noPID, UnitName: u})
	if err == nil || !strings.Contains(err.Error(), u+".scope") {
		t.Fatalf("err %v", err)
	}
}

func TestParseUnitName(t *testing.T) {
	for _, c := range []struct{ kind, subject, clean string }{
		{"build", "a-finished-session-leaves-no-process:1", "a-finished-session-leaves-no-process_1"},
		{"plan", "goal:x/y", "goal_x_y"},
		{"research", "direction:mesh:nightly", "direction_mesh_nightly"},
	} {
		n := UnitName(c.kind, c.subject)
		for _, name := range []string{n, n + ".scope"} {
			k, s, ok := ParseUnitName(name)
			if !ok || k != c.kind || s != c.clean {
				t.Fatalf("ParseUnitName(%q) = %q, %q, %v", name, k, s, ok)
			}
		}
	}
	for _, bad := range []string{"foo.scope", "mesh-session-build", "mesh-session-build-item-xyz", "mesh-node.service", ""} {
		if _, _, ok := ParseUnitName(bad); ok {
			t.Fatalf("ParseUnitName(%q) ok", bad)
		}
	}
}

func TestParseListUnits(t *testing.T) {
	out := "mesh-session-build-item_1-0a1b2c3d.scope loaded active running /usr/bin/claude -p\n" +
		"mesh-session-plan-g-11223344.scope   loaded inactive dead  plan\n" +
		"mesh-session-x.service loaded active running other\n"
	want := []Scope{{Unit: "mesh-session-build-item_1-0a1b2c3d", Active: "active"}, {Unit: "mesh-session-plan-g-11223344", Active: "inactive"}}
	if got := parseListUnits([]byte(out)); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

// A detached session's group is stopped and its other members reported.
func TestDetachedKillReportsGroup(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	fastStop(t)
	h, cleanup := startSleeps(t, Detached{})
	defer cleanup()
	killed, err := Detached{}.Kill(h)
	if err != nil {
		t.Fatal(err)
	}
	assertOneSleep(t, killed, h)
}

// On a real user systemd: the scope's backgrounded sleep is killed with
// it, reported, and the unit is gone.
func TestSystemdScopeKillReal(t *testing.T) {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		t.Skip("no systemd-run")
	}
	if exec.Command("systemctl", "--user", "show-environment").Run() != nil {
		t.Skip("no user systemd")
	}
	u := SystemdScope{}
	h, cleanup := startSleeps(t, u)
	defer cleanup()
	defer exec.Command("systemctl", "--user", "stop", h.UnitName+".scope").Run()
	killed, err := u.Kill(h)
	if err != nil {
		t.Fatal(err)
	}
	assertOneSleep(t, killed, h)
	if !unitGone(HostSystemctl{}, h.UnitName+".scope") {
		t.Fatalf("%s still there", h.UnitName)
	}
	if err := h.Wait(t.Context()); err == nil {
		t.Fatal("the leader exited cleanly: it was not killed")
	}
}

// startSleeps starts `sh -c 'sleep 300 & exec sleep 300'` in u and waits
// for both sleeps.
func startSleeps(t *testing.T, u Unit) (Handle, func()) {
	t.Helper()
	dir := t.TempDir()
	out, _ := os.Create(filepath.Join(dir, "out"))
	errf, _ := os.Create(filepath.Join(dir, "err"))
	h, err := u.Start(Spec{Kind: "test", Subject: "kill", Binary: "sh", Args: []string{"-c", "sleep 300 & exec sleep 300"}, Dir: dir, Env: os.Environ(), Stdout: out, Stderr: errf})
	out.Close()
	errf.Close()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		ms := groupMembers(h.PID)
		if len(ms) == 2 && ms[0].Comm == "sleep" && ms[1].Comm == "sleep" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("group %v", ms)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return h, func() {
		for _, p := range groupMembers(h.PID) {
			if pr, err := os.FindProcess(p.PID); err == nil {
				pr.Kill()
			}
		}
	}
}

func assertOneSleep(t *testing.T, killed []Process, h Handle) {
	t.Helper()
	if len(killed) != 1 || killed[0].Comm != "sleep" || killed[0].PID == h.PID || killed[0].Cmdline != "sleep 300" {
		t.Fatalf("killed %v (leader %d)", killed, h.PID)
	}
	if pidAlive(killed[0].PID) {
		t.Fatalf("%v still alive", killed[0])
	}
}
