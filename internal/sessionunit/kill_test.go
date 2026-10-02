package sessionunit

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
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
	// collect unloads the scope once no process is left in it, as
	// --collect does; a kill or stop after that answers ErrNotLoaded.
	collect bool
	// showErr, killErr, stopErr and procsErr are what those calls
	// return instead of answering: a manager that cannot be reached.
	// showErrAfterStop is showErr from the stop on.
	showErr, killErr, stopErr, procsErr, showErrAfterStop error
	// removing is a cgroup systemd tears down once a signal empties it:
	// from then on its cgroup.procs answers ENODEV, as the kernel's does.
	removing bool
	gone     bool
	calls    []string
}

func (f *fakeSystemctl) log(c string) { f.calls = append(f.calls, c) }

func (f *fakeSystemctl) Show(unit string, props ...string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("show " + strings.Join(props, ",") + " " + unit)
	if f.showErr != nil {
		return nil, f.showErr
	}
	if !f.loaded {
		return map[string]string{"LoadState": "not-found", "ActiveState": "inactive", "ControlGroup": ""}, nil
	}
	return map[string]string{"LoadState": "loaded", "ActiveState": "active", "ControlGroup": "/user.slice/" + unit}, nil
}

func (f *fakeSystemctl) Kill(unit, signal string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("kill " + signal + " " + unit)
	if f.killErr != nil {
		return f.killErr
	}
	if !f.loaded {
		return fmt.Errorf("Failed to kill unit %s: %w", unit, ErrNotLoaded)
	}
	var left []Process
	for _, p := range f.procs {
		if signal == "SIGTERM" && f.ignoreTerm[p.PID] {
			left = append(left, p)
		}
	}
	f.procs = left
	if f.removing && len(left) == 0 {
		f.gone = true
	}
	if f.collect && len(left) == 0 {
		f.loaded = false
	}
	return nil
}

func (f *fakeSystemctl) Stop(unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("stop " + unit)
	if f.showErrAfterStop != nil {
		f.showErr = f.showErrAfterStop
	}
	if f.stopErr != nil {
		return f.stopErr
	}
	if !f.loaded {
		return fmt.Errorf("Failed to stop %s: %w", unit, ErrNotLoaded)
	}
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
	if f.procsErr != nil {
		return nil, f.procsErr
	}
	if f.gone {
		return nil, &os.PathError{Op: "read", Path: cgroup + "/cgroup.procs", Err: syscall.ENODEV}
	}
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

// A scope whose cgroup systemd is removing once SIGTERM emptied it
// answers ENODEV to a read of cgroup.procs: nothing is left in it, not
// an error (the race TestSystemdScopeKillReal hit on a real host).
func TestSystemdScopeKillCgroupRemoved(t *testing.T) {
	fastStop(t)
	f := &fakeSystemctl{loaded: true, removing: true, procs: []Process{{PID: noPID, Comm: "claude"}, {PID: noPID + 1, Comm: "sleep"}}}
	killed, err := SystemdScope{Systemctl: f}.Kill(Handle{PID: noPID, UnitName: "u"})
	if err != nil {
		t.Fatalf("err %v", err)
	}
	if len(killed) != 1 || killed[0].Comm != "sleep" {
		t.Fatalf("killed %v", killed)
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
	f := &fakeSystemctl{loaded: true, stuck: true, procs: []Process{{PID: noPID, Comm: "claude"}, {PID: noPID + 1, Comm: "sleep"}}}
	u := "mesh-session-build-item_3-59082a54"
	killed, err := SystemdScope{Systemctl: f}.Kill(Handle{PID: noPID, UnitName: u})
	if err == nil || !strings.Contains(err.Error(), u+".scope") || len(killed) != 0 {
		t.Fatalf("killed %v, err %v", killed, err)
	}
}

// A manager that cannot be asked is not a unit that is gone: Show's
// error is Kill's, nothing is signalled and nothing is reported.
func TestSystemdScopeKillShowError(t *testing.T) {
	fastStop(t)
	boom := errors.New("Failed to connect to bus: No such file or directory")
	f := &fakeSystemctl{loaded: true, showErr: boom, procs: []Process{{PID: noPID, Comm: "claude"}, {PID: noPID + 1, Comm: "sleep"}}}
	killed, err := SystemdScope{Systemctl: f}.Kill(Handle{PID: noPID, UnitName: "u"})
	if !errors.Is(err, boom) || len(killed) != 0 {
		t.Fatalf("killed %v, err %v", killed, err)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "kill ") || strings.HasPrefix(c, "stop ") {
			t.Fatalf("acted on the unit: %q", f.calls)
		}
	}
}

// A stop that fails with the unit still there is returned, with the
// unit named, and nothing is reported as killed.
func TestSystemdScopeKillStopError(t *testing.T) {
	fastStop(t)
	boom := errors.New("Access denied")
	f := &fakeSystemctl{loaded: true, stopErr: boom, procs: []Process{{PID: noPID, Comm: "claude"}, {PID: noPID + 1, Comm: "sleep"}}}
	killed, err := SystemdScope{Systemctl: f}.Kill(Handle{PID: noPID, UnitName: "u"})
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "u.scope") || len(killed) != 0 {
		t.Fatalf("killed %v, err %v", killed, err)
	}
}

// A stop that fails is returned even when the unit is gone by the time
// Show is asked (collected after SIGTERM): the error is never swallowed.
func TestSystemdScopeKillStopErrorUnitGone(t *testing.T) {
	fastStop(t)
	boom := errors.New("Access denied")
	f := &fakeSystemctl{loaded: true, collect: true, stopErr: boom, procs: []Process{{PID: noPID, Comm: "claude"}, {PID: noPID + 1, Comm: "sleep"}}}
	_, err := SystemdScope{Systemctl: f}.Kill(Handle{PID: noPID, UnitName: "u"})
	if !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
}

// A stop that fails and a Show after it that fails are both returned,
// and with the unit not confirmed gone nothing is reported.
func TestSystemdScopeKillStopErrorShowError(t *testing.T) {
	fastStop(t)
	boom, down := errors.New("Access denied"), errors.New("Failed to connect to bus")
	f := &fakeSystemctl{loaded: true, stopErr: boom, showErrAfterStop: down, procs: []Process{{PID: noPID, Comm: "claude"}, {PID: noPID + 1, Comm: "sleep"}}}
	killed, err := SystemdScope{Systemctl: f}.Kill(Handle{PID: noPID, UnitName: "u"})
	if !errors.Is(err, boom) || !errors.Is(err, down) || len(killed) != 0 {
		t.Fatalf("killed %v, err %v", killed, err)
	}
}

// A Show failing after the stop is Kill's error and no report, though
// the stop itself went through.
func TestSystemdScopeKillShowErrorAfterStop(t *testing.T) {
	fastStop(t)
	down := errors.New("Failed to connect to bus")
	f := &fakeSystemctl{loaded: true, showErrAfterStop: down, procs: []Process{{PID: noPID, Comm: "claude"}, {PID: noPID + 1, Comm: "sleep"}}}
	killed, err := SystemdScope{Systemctl: f}.Kill(Handle{PID: noPID, UnitName: "u"})
	if !errors.Is(err, down) || len(killed) != 0 {
		t.Fatalf("killed %v, err %v", killed, err)
	}
}

// A signal that fails with the manager answering that the unit is not
// loaded — collected once SIGTERM emptied it — is the unit gone, no
// error: the stop after it answers the same.
func TestSystemdScopeKillCollected(t *testing.T) {
	fastStop(t)
	f := &fakeSystemctl{loaded: true, collect: true, procs: []Process{{PID: noPID, Comm: "claude"}, {PID: noPID + 1, Comm: "sleep"}}}
	killed, err := SystemdScope{Systemctl: f}.Kill(Handle{PID: noPID, UnitName: "u"})
	if err != nil || len(killed) != 1 || killed[0].PID != noPID+1 {
		t.Fatalf("killed %v, err %v", killed, err)
	}
}

// A signal that fails is returned too, though the stop then clears the
// unit: the report stands, the unit being confirmed gone.
func TestSystemdScopeKillSignalError(t *testing.T) {
	fastStop(t)
	boom := errors.New("Access denied")
	f := &fakeSystemctl{loaded: true, killErr: boom, procs: []Process{{PID: noPID, Comm: "claude"}, {PID: noPID + 1, Comm: "sleep"}}}
	killed, err := SystemdScope{Systemctl: f}.Kill(Handle{PID: noPID, UnitName: "u"})
	if !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
	if len(killed) != 1 || killed[0].PID != noPID+1 {
		t.Fatalf("killed %v", killed)
	}
}

// Reading the cgroup's processes failing is Kill's error before any
// signal: what was there cannot be reported.
func TestSystemdScopeKillProcessesError(t *testing.T) {
	fastStop(t)
	boom := errors.New("permission denied")
	f := &fakeSystemctl{loaded: true, procsErr: boom}
	killed, err := SystemdScope{Systemctl: f}.Kill(Handle{PID: noPID, UnitName: "u"})
	if !errors.Is(err, boom) || len(killed) != 0 {
		t.Fatalf("killed %v, err %v", killed, err)
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
	skipWithoutCgroup(t, h.UnitName+".scope")
	killed, err := u.Kill(h)
	if err != nil {
		t.Fatal(err)
	}
	assertOneSleep(t, killed, h)
	if gone, err := unitGone(HostSystemctl{}, h.UnitName+".scope"); err != nil || !gone {
		t.Fatalf("%s still there", h.UnitName)
	}
	if err := h.Wait(t.Context()); err == nil {
		t.Fatal("the leader exited cleanly: it was not killed")
	}
}

// skipWithoutCgroup skips the test when the started scope's cgroup.procs
// cannot be read: a GitHub Actions runner has a user systemd but no
// cgroup delegation, and reading the file answers ENODEV ("no such
// device"). The test proves the kill on a delegated cgroup; a host
// without one cannot run it.
func skipWithoutCgroup(t *testing.T, unit string) {
	t.Helper()
	sc := HostSystemctl{}
	props, err := sc.Show(unit, "ControlGroup")
	if err != nil {
		t.Fatalf("show %s: %v", unit, err)
	}
	if _, err := sc.Processes(props["ControlGroup"]); err != nil {
		if errors.Is(err, syscall.ENODEV) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
			t.Skipf("the runner has no cgroup delegation: %s's cgroup.procs cannot be read: %v", unit, err)
		}
		t.Fatalf("cgroup.procs of %s: %v", unit, err)
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
