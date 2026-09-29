package node

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// sweepSystemctl is a user manager with some scopes: each its
// ActiveState and its cgroup's processes. A SIGTERM empties a scope and
// a stop unloads it; every call is logged.
type sweepSystemctl struct {
	scopes []sessionunit.Scope
	procs  map[string][]sessionunit.Process // by unit, without .scope
	calls  []string
}

func (f *sweepSystemctl) state(unit string) (sessionunit.Scope, bool) {
	for _, s := range f.scopes {
		if s.Unit+".scope" == unit {
			return s, true
		}
	}
	return sessionunit.Scope{}, false
}

func (f *sweepSystemctl) Show(unit string, props ...string) (map[string]string, error) {
	f.calls = append(f.calls, "show "+strings.Join(props, ",")+" "+unit)
	s, ok := f.state(unit)
	if !ok {
		return map[string]string{"LoadState": "not-found"}, nil
	}
	return map[string]string{"LoadState": "loaded", "ActiveState": s.Active, "ControlGroup": "/cg/" + unit}, nil
}

func (f *sweepSystemctl) Kill(unit, signal string) error {
	f.calls = append(f.calls, "kill "+signal+" "+unit)
	delete(f.procs, strings.TrimSuffix(unit, ".scope"))
	return nil
}

func (f *sweepSystemctl) Stop(unit string) error {
	f.calls = append(f.calls, "stop "+unit)
	f.scopes = slices.DeleteFunc(f.scopes, func(s sessionunit.Scope) bool { return s.Unit+".scope" == unit })
	return nil
}

func (f *sweepSystemctl) ListScopes() ([]sessionunit.Scope, error) {
	f.calls = append(f.calls, "list")
	return slices.Clone(f.scopes), nil
}

func (f *sweepSystemctl) Processes(cgroup string) ([]sessionunit.Process, error) {
	return f.procs[strings.TrimSuffix(strings.TrimPrefix(cgroup, "/cg/"), ".scope")], nil
}

func (f *sweepSystemctl) stopped() bool {
	return slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "stop ") || strings.HasPrefix(c, "kill ") })
}

// sweepFake is the seeded platform with item1 in status and a scope of
// kind build for it holding go and bash.
func sweepFake(t *testing.T, status string) (*gatewaytest.Fake, Gateway, *sweepSystemctl, string) {
	t.Helper()
	f := seed(t)
	f.WorkItems[item1]["status"] = status
	unit := sessionunit.UnitName("build", item1)
	sc := &sweepSystemctl{
		scopes: []sessionunit.Scope{{Unit: unit, Active: "active"}},
		procs: map[string][]sessionunit.Process{unit: {
			{PID: 4242, Comm: "go", Cmdline: "go run ./cmd/runsheet -addr :18211"},
			{PID: 4243, Comm: "bash"},
		}},
	}
	return f, gateway.New(f.URL(), "rs_test"), sc, unit
}

func sweepLog() (*[]string, func(string, ...any)) {
	var lines []string
	return &lines, func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
}

// The leak on the mini: a build scope of a merged item still holding a
// platform and its bash is stopped, and both are named.
func TestSweepStopsTerminalScope(t *testing.T) {
	_, gw, sc, unit := sweepFake(t, "merged")
	lines, logf := sweepLog()
	if n := SweepScopes(context.Background(), gw, sc, nil, logf); n != 1 {
		t.Fatalf("stopped %d, log %q", n, *lines)
	}
	if !slices.Contains(sc.calls, "stop "+unit+".scope") {
		t.Fatalf("no stop: %q", sc.calls)
	}
	want := fmt.Sprintf("sweep: stopped %s (item %s, merged on the platform): killed go[4242] go run ./cmd/runsheet -addr :18211, bash[4243]", unit, item1)
	if !slices.Contains(*lines, want) {
		t.Fatalf("log %q\nwant %q", *lines, want)
	}
}

// A running item whose session is about to be adopted keeps its scope.
func TestSweepLeavesLiveScope(t *testing.T) {
	_, gw, sc, unit := sweepFake(t, "running")
	found := []FoundSession{{State: sessionunit.State{Kind: "build", ItemID: item1, Unit: unit}}}
	lines, logf := sweepLog()
	if n := SweepScopes(context.Background(), gw, sc, found, logf); n != 0 || sc.stopped() {
		t.Fatalf("stopped %d, calls %q, log %q", n, sc.calls, *lines)
	}
}

// A running item with no state file here is still live on the
// platform: left, and said.
func TestSweepLeavesLiveItemWithoutState(t *testing.T) {
	_, gw, sc, unit := sweepFake(t, "running")
	lines, logf := sweepLog()
	if n := SweepScopes(context.Background(), gw, sc, nil, logf); n != 0 || sc.stopped() {
		t.Fatalf("stopped %d, calls %q", n, sc.calls)
	}
	want := fmt.Sprintf("sweep: %s left, item %s is running on the platform", unit, item1)
	if !slices.Contains(*lines, want) {
		t.Fatalf("log %q\nwant %q", *lines, want)
	}
}

// An inactive or failed scope is systemd's to collect: not looked up,
// not stopped.
func TestSweepSkipsInactive(t *testing.T) {
	f, gw, sc, _ := sweepFake(t, "merged")
	sc.scopes[0].Active = "inactive"
	sc.scopes = append(sc.scopes, sessionunit.Scope{Unit: sessionunit.UnitName("build", item2), Active: "failed"})
	lines, logf := sweepLog()
	if n := SweepScopes(context.Background(), gw, sc, nil, logf); n != 0 || sc.stopped() {
		t.Fatalf("stopped %d, calls %q, log %q", n, sc.calls, *lines)
	}
	if len(f.Reads) != 0 {
		t.Fatalf("reads %v", f.Reads)
	}
}

// An item the platform does not know is terminal: its scope is stopped.
func TestSweepStopsUnknownItem(t *testing.T) {
	f, gw, sc, unit := sweepFake(t, "running")
	delete(f.WorkItems, item1)
	lines, logf := sweepLog()
	if n := SweepScopes(context.Background(), gw, sc, nil, logf); n != 1 {
		t.Fatalf("stopped %d, log %q", n, *lines)
	}
	if !slices.Contains(sc.calls, "stop "+unit+".scope") {
		t.Fatalf("no stop: %q", sc.calls)
	}
	if !strings.Contains(strings.Join(*lines, "\n"), "unknown on the platform") {
		t.Fatalf("log %q", *lines)
	}
}

// A platform that cannot answer is an outage, not a leak: the scope is
// left and the error logged.
func TestSweepLeavesOnGatewayError(t *testing.T) {
	f, gw, sc, unit := sweepFake(t, "merged")
	f.FailReads["workItemIndex"] = "upstream unavailable"
	lines, logf := sweepLog()
	if n := SweepScopes(context.Background(), gw, sc, nil, logf); n != 0 || sc.stopped() {
		t.Fatalf("stopped %d, calls %q", n, sc.calls)
	}
	if len(*lines) != 1 || !strings.Contains((*lines)[0], unit+" left") || !strings.Contains((*lines)[0], "upstream unavailable") {
		t.Fatalf("log %q", *lines)
	}
}

// A plan scope follows its goal, a research one and a name that is not
// a session's are left alone.
func TestSweepPlanResearchAndStrangers(t *testing.T) {
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	planning, done := sessionunit.UnitName("plan", goalID), sessionunit.UnitName("plan", "0f0f0f0f-0000-4000-8000-000000000000")
	research := sessionunit.UnitName("research", "direction:mesh:nightly")
	f.Goals[goalID]["status"] = "planning"
	sc := &sweepSystemctl{scopes: []sessionunit.Scope{
		{Unit: planning, Active: "active"}, {Unit: done, Active: "active"},
		{Unit: research, Active: "active"}, {Unit: "mesh-session-odd", Active: "active"},
	}}
	lines, logf := sweepLog()
	if n := SweepScopes(context.Background(), gw, sc, nil, logf); n != 1 {
		t.Fatalf("stopped %d, log %q", n, *lines)
	}
	if !slices.Contains(sc.calls, "stop "+done+".scope") || slices.Contains(sc.calls, "stop "+planning+".scope") {
		t.Fatalf("calls %q", sc.calls)
	}
	if len(sc.scopes) != 3 {
		t.Fatalf("scopes left %v", sc.scopes)
	}
}

func TestSessionLive(t *testing.T) {
	for _, c := range []struct {
		status, rebase string
		live           bool
	}{
		{"pending", "", false},
		{"ready", "", false},
		{"claimed", "", true},
		{"running", "", true},
		{"evaluating", "", true},
		{"review", "", false},
		{"review", "requested", false},
		{"review", "claimed", true},
		{"review", "rebased", false},
		{"review", "unresolved", false},
		{"merged", "", false},
		{"done", "", false},
		{"already_done", "", false},
		{"failed", "", false},
		{"cancelled", "", false},
		{"", "", false},
	} {
		if got := SessionLive(gateway.WorkItem{Status: c.status, RebaseState: c.rebase}); got != c.live {
			t.Errorf("SessionLive(%s, rebase %q) = %v, want %v", c.status, c.rebase, got, c.live)
		}
	}
	for status, live := range map[string]bool{"planning": true, "running": false, "paused": false, "awaiting_approval": false, "cancelled": false, "done": false} {
		if got := GoalLive(gateway.Goal{Status: status}); got != live {
			t.Errorf("GoalLive(%s) = %v", status, got)
		}
	}
}
