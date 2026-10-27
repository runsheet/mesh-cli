package node

import (
	"context"
	"log"
	"strings"
	"sync"
	"testing"
)

// goneBaseWS is fakeWS whose Diff and ChangedPaths are Git's on dir, the
// item's real checkout, noting each base it was asked for.
type goneBaseWS struct {
	*fakeWS
	dir   string
	mu    sync.Mutex
	diffs []string
	paths []string
}

func (w *goneBaseWS) Diff(ctx context.Context, _, base string) (string, error) {
	w.mu.Lock()
	w.diffs = append(w.diffs, base)
	w.mu.Unlock()
	return (&Git{}).Diff(ctx, w.dir, base)
}

func (w *goneBaseWS) ChangedPaths(ctx context.Context, _, base string) ([]string, error) {
	w.mu.Lock()
	w.paths = append(w.paths, base)
	w.mu.Unlock()
	return (&Git{}).ChangedPaths(ctx, w.dir, base)
}

// An item built on a dependency's branch that merged into main and was
// deleted on origin is evaluated against origin/main: no failDiff
// verdict, the diff and the changed paths taken from main, the prompt
// naming it, and node.log saying so.
func TestEvaluateBaseGoneDiffsFromTheDefaultBranch(t *testing.T) {
	f, ev, h, _ := secondFixture(t, "normal", "Holds.\nVERDICT: PASS", &fakeCodexH{})
	_, dir := mergedDependencyRepo(t)
	f.WorkItems[item1]["baseRef"] = "mesh/g-1"
	f.WorkItems[item1]["branch"] = "mesh/g-2"
	ws := &goneBaseWS{fakeWS: ev.WS.(*fakeWS), dir: dir}
	ev.WS = ws
	logs := &syncBuf{}
	ev.Log = log.New(logs, "", 0)

	in := evaluateItem1(t, f, ev)
	notes, _ := in["notes"].(string)
	if strings.Contains(notes, "could not diff") {
		t.Fatalf("verdict: %+v\n%s", in, logs.String())
	}
	if len(h.jobs) != 1 {
		t.Fatalf("evaluator sessions = %d, want one", len(h.jobs))
	}
	if got := strings.Join(ws.diffs, ","); got != "mesh/g-1,main" {
		t.Fatalf("Diff asked for %q, want mesh/g-1 then main", got)
	}
	// no protected-path rule reads the changed paths: the evaluation
	// policy is the product's (runsheet ADR 077)
	if got := strings.Join(ws.paths, ","); got != "" {
		t.Fatalf("ChangedPaths asked for %q, want none", got)
	}
	prompt := h.jobs[0].Prompt
	if !strings.Contains(prompt, "origin/main") || strings.Contains(prompt, "origin/mesh/g-1") || !strings.Contains(prompt, "item.txt") {
		t.Fatalf("the prompt is not on origin/main's diff:\n%s", prompt)
	}
	line := "evaluate: base mesh/g-1 is gone (merged); diff from origin/main"
	if !strings.Contains(logs.String(), "item ship-it/1: "+line) {
		t.Fatalf("log lacks %q:\n%s", line, logs.String())
	}
	if strings.Contains(logs.String(), "not evaluated, failed") {
		t.Fatalf("failDiff ran:\n%s", logs.String())
	}
}

// The retry turn's changed paths on such an item are read against
// origin/main, not left empty.
func TestRetryTurnBaseGoneChangedPathsFromTheDefaultBranch(t *testing.T) {
	_, dir := mergedDependencyRepo(t)
	logs := &syncBuf{}
	r := &Runner{WS: &Git{}, Log: log.New(logs, "", 0)}
	got := r.retryChangedPaths(context.Background(), "g/2", dir, "mesh/g-1", "main")
	if strings.Join(got, ",") != "dep.txt,item.txt" {
		t.Fatalf("retry turn's changed paths = %q, want dep.txt,item.txt\n%s", got, logs.String())
	}
	line := "item g/2: retry turn: base mesh/g-1 is gone (merged); changed paths from origin/main"
	if !strings.Contains(logs.String(), line) {
		t.Fatalf("log lacks %q:\n%s", line, logs.String())
	}
	// a base origin still has is read as before, and says nothing
	before := logs.String()
	if got := r.retryChangedPaths(context.Background(), "g/2", dir, "main", "main"); len(got) == 0 {
		t.Fatal("changed paths against main are empty")
	}
	if logs.String() != before {
		t.Fatalf("log: %s", logs.String())
	}
}

// goneBaseFallback takes the default branch only for a gone base.
func TestGoneBaseFallbackOnlyForBaseGone(t *testing.T) {
	gone := &BaseGoneError{Base: "mesh/g-1"}
	if def, ok := goneBaseFallback(gone, "mesh/g-1", "main"); !ok || def != "main" {
		t.Fatalf("gone base: %q %v", def, ok)
	}
	for _, c := range []struct {
		err       error
		base, def string
	}{
		{nil, "mesh/g-1", "main"},
		{&NoMergeBaseError{Base: "main", Branch: "b"}, "mesh/g-1", "main"},
		{gone, "main", "main"},
		{gone, "mesh/g-1", ""},
	} {
		if _, ok := goneBaseFallback(c.err, c.base, c.def); ok {
			t.Fatalf("fallback taken for %v %q %q", c.err, c.base, c.def)
		}
	}
}
