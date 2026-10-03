package meshmcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

const jItem2ID = "eeeeeeee-2222-4222-8222-eeeeeeeeeeee"

// tool calls the personal server's tool by name; a transport error
// fails the test.
func (w *joinWorld) tool(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	for _, tool := range w.persona.Tools() {
		if tool.Name == name {
			out, err := tool.Call(context.Background(), args)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			return out
		}
	}
	t.Fatalf("no %s tool", name)
	return ""
}

// secondItem adds the goal's item 2, ready, on the same repo.
func (w *joinWorld) secondItem() {
	w.fake.WorkItems[jItem2ID] = map[string]any{"id": jItem2ID, "goalId": jGoalID, "orgId": jOrgID, "seq": 2, "title": "Make it pass",
		"brief": "Make the test in api/ pass.", "acceptance": "go test ./api/ passes.",
		"repoId": jRepoID, "baseRef": "main", "status": "ready", "dependsOn": []string{}}
	w.fake.GitHubTokens[jItem2ID] = map[string]any{"token": "ghs_item", "expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "repository": "acme/widgets"}
}

func (w *joinWorld) state(t *testing.T) State {
	t.Helper()
	s, err := ReadState(filepath.Join(w.home, ".mesh", StateFile))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (w *joinWorld) statusOf(t *testing.T) []HeldStatus {
	t.Helper()
	out := w.tool(t, "mesh_status", nil)
	var s struct {
		Held []HeldStatus `json:"held"`
	}
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("mesh_status %q: %v", out, err)
	}
	return s.Held
}

// commit writes name in dir and commits it.
func commit(t *testing.T, dir, name, body string) string {
	t.Helper()
	os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", name)
	gitIn(t, dir, "commit", "--quiet", "-m", "add "+name)
	return gitIn(t, dir, "rev-parse", "HEAD")
}

// callsInOrder is the fake's mutations of those names, as received.
func callsInOrder(f *gatewaytest.Fake, names ...string) []gatewaytest.Call {
	var all []gatewaytest.Call
	for _, n := range names {
		all = append(all, f.CallsNamed(n)...)
	}
	sort.SliceStable(all, func(a, b int) bool { return all[a].At.Before(all[b].At) })
	return all
}

// join → commit a file in the checkout → mesh_finish: the origin's
// branch is the checkout's HEAD, finishWorkItem names that sha and
// branch, and the item leaves the state file.
func TestFlowJoinWorkFinish(t *testing.T) {
	w := newJoinWorld(t)
	a := decodeJoin(t, w.join(t, map[string]any{"goal": "ship-it"}))
	head := commit(t, a.Path, "api/api_test.go", "package api\n")

	out := w.tool(t, "mesh_finish", map[string]any{})
	if !strings.HasPrefix(out, "finished ship-it/1: pushed mesh/ship-it-1 at "+head) {
		t.Fatalf("mesh_finish = %q", out)
	}
	if got := gitIn(t, w.origin, "rev-parse", "refs/heads/mesh/ship-it-1"); got != head {
		t.Fatalf("origin's mesh/ship-it-1 at %s, checkout at %s", got, head)
	}
	finishes := w.fake.CallsNamed("finishWorkItem")
	if len(finishes) != 1 {
		t.Fatalf("finishWorkItem %d times", len(finishes))
	}
	if in := finishes[0].Input; in["aggregateId"] != jItemID || in["branch"] != "mesh/ship-it-1" || in["headSha"] != head {
		t.Fatalf("finishWorkItem %v", in)
	}
	if s := w.state(t); len(s.Held) != 0 {
		t.Fatalf("state still holds %+v", s.Held)
	}
	if got := w.tool(t, "mesh_status", nil); got != holdingNone {
		t.Fatalf("mesh_status after finish = %q", got)
	}
}

// join → mesh_switch → the second item: the first is pushed, noted and
// released `switched`, the second claimed in its own checkout, and
// mesh_status lists one then the other.
func TestFlowJoinSwitchJoin(t *testing.T) {
	w := newJoinWorld(t)
	w.secondItem()
	first := decodeJoin(t, w.join(t, map[string]any{"goal": "ship-it", "item": float64(1)}))
	if held := w.statusOf(t); len(held) != 1 || held[0].ItemID != jItemID || held[0].Path != first.Path {
		t.Fatalf("mesh_status before the switch: %+v", held)
	}
	head := commit(t, first.Path, "api/api_test.go", "package api\n")

	second := decodeJoin(t, w.tool(t, "mesh_switch", map[string]any{"goal": "ship-it", "item": float64(2), "note": "the test is written"}))
	if second.Item.ID != jItem2ID || second.Branch != "mesh/ship-it-2" {
		t.Fatalf("switch joined %+v", second)
	}

	var names []string
	calls := callsInOrder(w.fake, "claimWorkItem", "reportWorkItemProgress", "releaseWorkItem")
	for _, c := range calls {
		names = append(names, c.Name+":"+c.Input["aggregateId"].(string))
	}
	want := []string{"claimWorkItem:" + jItemID, "reportWorkItemProgress:" + jItemID, "releaseWorkItem:" + jItemID, "claimWorkItem:" + jItem2ID}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("calls %v, want %v", names, want)
	}
	if note, _ := calls[1].Input["note"].(string); !strings.HasPrefix(note, "the test is written") {
		t.Fatalf("progress note %q", note)
	}
	if reason := calls[2].Input["note"]; reason != SwitchReason {
		t.Fatalf("release reason %v", reason)
	}
	if got := gitIn(t, w.origin, "rev-parse", "refs/heads/mesh/ship-it-1"); got != head {
		t.Fatalf("the first item's commits were not pushed: origin at %s, checkout at %s", got, head)
	}
	for _, c := range []struct{ path, branch string }{{first.Path, "mesh/ship-it-1"}, {second.Path, "mesh/ship-it-2"}} {
		if got := gitIn(t, c.path, "rev-parse", "--abbrev-ref", "HEAD"); got != c.branch {
			t.Fatalf("%s is on %s, want %s", c.path, got, c.branch)
		}
	}
	if first.Path == second.Path {
		t.Fatalf("one checkout for both: %s", first.Path)
	}
	if held := w.statusOf(t); len(held) != 1 || held[0].ItemID != jItem2ID || held[0].Path != second.Path {
		t.Fatalf("mesh_status after the switch: %+v", held)
	}
}

// mesh_status names the held item's path and branch, whether its
// checkout has uncommitted changes and its commits past base.
func TestFlowStatusCountsCommitsPastBase(t *testing.T) {
	w := newJoinWorld(t)
	a := decodeJoin(t, w.join(t, map[string]any{"goal": "ship-it"}))
	if held := w.statusOf(t); len(held) != 1 || held[0].CommitsPastBase != 0 || held[0].Uncommitted {
		t.Fatalf("fresh checkout: %+v", held)
	}
	commit(t, a.Path, "api/one.go", "package api\n")
	commit(t, a.Path, "api/two.go", "package api\n")
	os.WriteFile(filepath.Join(a.Path, "scratch.txt"), []byte("wip\n"), 0o644)
	held := w.statusOf(t)
	if len(held) != 1 {
		t.Fatalf("held %+v", held)
	}
	h := held[0]
	if h.Path != a.Path || h.Branch != "mesh/ship-it-1" || h.CommitsPastBase != 2 || !h.Uncommitted || h.Error != "" {
		t.Fatalf("mesh_status: %+v", h)
	}
}

// mesh_progress and mesh_ask report on the held item — the question
// opening `question: ` — and mesh_release gives it back and drops it.
func TestFlowProgressAskRelease(t *testing.T) {
	w := newJoinWorld(t)
	w.join(t, map[string]any{"goal": "ship-it"})

	if out := w.tool(t, "mesh_progress", map[string]any{"note": "test written", "costCents": float64(12)}); !strings.HasPrefix(out, "noted on ship-it/1") {
		t.Fatalf("mesh_progress = %q", out)
	}
	if out := w.tool(t, "mesh_ask", map[string]any{"item": "ship-it/1", "question": "Should api/ export it?"}); !strings.HasPrefix(out, "asked on ship-it/1") {
		t.Fatalf("mesh_ask = %q", out)
	}
	reports := w.fake.CallsNamed("reportWorkItemProgress")
	if len(reports) != 2 {
		t.Fatalf("reportWorkItemProgress %d times", len(reports))
	}
	if in := reports[0].Input; in["aggregateId"] != jItemID || in["note"] != "test written" || in["costCents"] != float64(12) {
		t.Fatalf("progress %v", in)
	}
	if in := reports[1].Input; in["aggregateId"] != jItemID || in["note"] != "question: Should api/ export it?" {
		t.Fatalf("ask %v", in)
	}

	if out := w.tool(t, "mesh_release", map[string]any{"item": jItemID, "reason": "out of my depth"}); out != "released ship-it/1: out of my depth" {
		t.Fatalf("mesh_release = %q", out)
	}
	releases := w.fake.CallsNamed("releaseWorkItem")
	if len(releases) != 1 || releases[0].Input["aggregateId"] != jItemID || releases[0].Input["nodeId"] != jNodeID || releases[0].Input["note"] != "out of my depth" {
		t.Fatalf("releaseWorkItem %v", releases)
	}
	if s := w.state(t); len(s.Held) != 0 {
		t.Fatalf("state still holds %+v", s.Held)
	}
}

// A refusal from the gateway is the tool's answer, `<tool> refused:
// <reason>`, and the item stays held.
func TestFlowRefusalIsTheAnswer(t *testing.T) {
	w := newJoinWorld(t)
	w.join(t, map[string]any{"goal": "ship-it"})
	w.fake.Refuse["reportWorkItemProgress"] = "the item is not yours"
	w.fake.Refuse["releaseWorkItem"] = "the item is not yours"
	if out := w.tool(t, "mesh_progress", map[string]any{"note": "x"}); !strings.HasPrefix(out, "mesh_progress refused: ") || !strings.Contains(out, "the item is not yours") {
		t.Fatalf("mesh_progress = %q", out)
	}
	if out := w.tool(t, "mesh_release", map[string]any{"reason": "x"}); !strings.HasPrefix(out, "mesh_release refused: ") {
		t.Fatalf("mesh_release = %q", out)
	}
	if s := w.state(t); len(s.Held) != 1 {
		t.Fatalf("a refused release dropped the item: %+v", s.Held)
	}
}

// With several items held, a work tool needs `item`.
func TestFlowSeveralHeldNeedsItem(t *testing.T) {
	w := newJoinWorld(t)
	w.secondItem()
	w.join(t, map[string]any{"goal": "ship-it", "item": float64(1)})
	w.join(t, map[string]any{"goal": "ship-it", "item": float64(2)})
	for _, tl := range w.persona.Tools() {
		if tl.Name != "mesh_progress" {
			continue
		}
		if _, err := tl.Call(context.Background(), map[string]any{"note": "x"}); err == nil || !strings.Contains(err.Error(), "several items are held") {
			t.Fatalf("mesh_progress with two held: %v", err)
		}
	}
	if out := w.tool(t, "mesh_progress", map[string]any{"item": float64(2), "note": "x"}); !strings.HasPrefix(out, "noted on ship-it/2") {
		t.Fatalf("mesh_progress item 2 = %q", out)
	}
}
