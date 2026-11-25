package nodetools

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/validate"
)

// run_suite's description states the cap on how many suites one call
// may name and that a call over it is refused, naming the ones it will
// not run (the node says so, not just enforces it silently).
func TestRunSuiteToolDescribesItsSuiteCap(t *testing.T) {
	desc := (&RunSuite{}).Tool().Description
	cap := fmt.Sprintf("%d", maxRunSuites)
	if !strings.Contains(desc, cap) {
		t.Fatalf("description does not state the cap (%s): %s", cap, desc)
	}
	if !strings.Contains(desc, "refused") || !strings.Contains(desc, "will not run") {
		t.Fatalf("description does not say a call over the cap is refused, naming what it will not run: %s", desc)
	}
}

// A call naming more suites than the cap is refused outright, naming
// the suites beyond it, rather than running only the first maxRunSuites
// and dropping the rest unnamed.
func TestRunSuiteRefusesACallNamingMoreSuitesThanItsCap(t *testing.T) {
	var yml strings.Builder
	yml.WriteString("suites:\n")
	var declared []string
	for i := 0; i < maxRunSuites+3; i++ {
		name := fmt.Sprintf("s%d", i)
		declared = append(declared, name)
		fmt.Fprintf(&yml, "  - name: %s\n    kind: unit\n    command: \"true\"\n", name)
	}
	dir := suiteCheckout(t, yml.String())
	m := &fakeMesh{}
	rec, _ := recorder(t, m, RunSuiteTool)
	run := func(_ context.Context, _ string, _ []validate.Step, _ []string) ([]validate.StepResult, error) {
		t.Fatal("the runner ran: the call should have been refused before it started")
		return nil, nil
	}
	names := make([]any, len(declared))
	for i, n := range declared {
		names[i] = n
	}
	_, err := (&RunSuite{Workspace: dir, Recorder: rec, Run: run}).Tool().Call(context.Background(), map[string]any{"suites": names})
	if err == nil {
		t.Fatal("want a refusal naming the suites over the cap, got none")
	}
	for _, n := range declared[maxRunSuites:] {
		if !strings.Contains(err.Error(), n) {
			t.Fatalf("refusal %q does not name %s", err, n)
		}
	}
}

// When a run's runner drops suites for a real reason — not fail_fast's
// deliberate skip, which answers no error at all — run_suite refuses
// the call outright, naming every suite that did not run, rather than
// answering a "finished" result with the dropped suites silently
// missing (a prior bug: cmp.Or inside validate.Run kept only the first
// of several simultaneous lane-open failures, and run_suite's execute
// only refused when results was entirely empty, so a partial drop
// alongside at least one real result went out as success).
func TestRunSuiteRefusesRatherThanSilentlyDropSuites(t *testing.T) {
	dir := suiteCheckout(t, "suites:\n"+
		"  - name: a\n    kind: unit\n    command: \"true\"\n"+
		"  - name: b\n    kind: unit\n    command: \"true\"\n"+
		"  - name: c\n    kind: unit\n    command: \"true\"\n")
	m := &fakeMesh{}
	rec, _ := recorder(t, m, RunSuiteTool)
	run := func(_ context.Context, _ string, steps []validate.Step, _ []string) ([]validate.StepResult, error) {
		// simulates two lanes (b and c) failing to open while a's ran:
		// the exact shape validate.Run answers for a genuine lane-open
		// failure, not a fail-fast skip (which answers no error).
		var out []validate.StepResult
		for _, st := range steps {
			if st.Name == "a" {
				out = append(out, validate.StepResult{Name: st.Name, Command: st.Command})
			}
		}
		return out, fmt.Errorf("lane b: pod create refused\nlane c: pod create refused")
	}
	text, err := (&RunSuite{Workspace: dir, Recorder: rec, Run: run}).Tool().Call(context.Background(), map[string]any{})
	if err == nil {
		t.Fatalf("want a refusal naming b and c, got a finished answer: %s", text)
	}
	if !strings.Contains(err.Error(), "b") || !strings.Contains(err.Error(), "c") {
		t.Fatalf("error %q does not name both dropped suites", err)
	}
}
