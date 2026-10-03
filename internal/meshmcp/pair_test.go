package meshmcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/node"
)

const jPairNodeID = "33333333-3333-4333-8333-333333333333"

// pairWorld is joinWorld with a second person's `mesh mcp` beside the
// holder's: its own personal node, work root and state file.
type pairWorld struct {
	*joinWorld
	pair *Personal
}

func newPairWorld(t *testing.T) *pairWorld {
	t.Helper()
	w := newJoinWorld(t)
	gw := gateway.New(w.fake.URL(), "rs_test")
	pair := &Personal{GW: gw, Join: &Join{
		GW: gw, WS: &node.Git{GitHubURL: w.github}, Cwd: w.persona.Join.Cwd,
		NodeID:    func(context.Context) (string, error) { return jPairNodeID, nil },
		WorkRoot:  filepath.Join(w.root, "pair-work"),
		StatePath: filepath.Join(w.root, "pair-state.json"),
		Logf:      t.Logf,
	}}
	return &pairWorld{joinWorld: w, pair: pair}
}

// pairTool calls the pair's tool by name.
func (w *pairWorld) pairTool(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	for _, tool := range w.pair.Tools() {
		if tool.Name == name {
			out, err := tool.Call(context.Background(), args)
			if err != nil {
				t.Fatalf("pair %s: %v", name, err)
			}
			return out
		}
	}
	t.Fatalf("no %s tool", name)
	return ""
}

// The holder joins and pushes a commit; the pair joins with pair:true:
// no second claimWorkItem, its checkout on the holder's branch at the
// origin's head, and its mesh_status marks it paired.
func TestPairJoinsTheHoldersBranchWithoutAClaim(t *testing.T) {
	w := newPairWorld(t)
	holder := decodeJoin(t, w.join(t, map[string]any{"goal": "ship-it"}))
	commit(t, holder.Path, "api/api_test.go", "package api\n")
	gitIn(t, holder.Path, "push", "--quiet", "-u", "origin", holder.Branch)
	originHead := gitIn(t, w.origin, "rev-parse", "refs/heads/mesh/ship-it-1")

	pair := decodeJoin(t, w.pairTool(t, "mesh_join", map[string]any{"goal": "ship-it", "item": float64(1), "pair": true}))
	if n := len(w.fake.Claims); n != 1 {
		t.Fatalf("claimWorkItem sent %d times, want the holder's alone", n)
	}
	if c := w.fake.Claims[0]; c.Input["nodeId"] != jNodeID {
		t.Fatalf("the claim was %v", c.Input)
	}
	if !pair.Paired || pair.Branch != holder.Branch || pair.Path == holder.Path {
		t.Fatalf("pair joined %+v; holder %+v", pair, holder)
	}
	if got := gitIn(t, pair.Path, "rev-parse", "--abbrev-ref", "HEAD"); got != "mesh/ship-it-1" {
		t.Fatalf("pair's HEAD is %s", got)
	}
	if got := gitIn(t, pair.Path, "rev-parse", "HEAD"); got != originHead {
		t.Fatalf("pair at %s, origin's branch at %s", got, originHead)
	}
	if helpers := gitIn(t, pair.Path, "config", "--local", "--get-all", "credential."+w.github+".helper"); !strings.Contains(helpers, "ghs_item") {
		t.Fatalf("pair's helpers: %q", helpers)
	}

	out := w.pairTool(t, "mesh_status", nil)
	var st struct {
		Held []HeldStatus `json:"held"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("mesh_status %q: %v", out, err)
	}
	if len(st.Held) != 1 || !st.Held[0].Paired || st.Held[0].ItemID != jItemID || !strings.Contains(out, `"paired": true`) {
		t.Fatalf("pair's mesh_status: %s", out)
	}
	if held := w.statusOf(t); len(held) != 1 || held[0].Paired {
		t.Fatalf("holder's mesh_status: %+v", held)
	}
}

// The holder finishes first; the pair's finish on its stale base is
// `push refused:`, no finishWorkItem, origin still at the holder's
// head; after `git pull --rebase` the pair's finish goes through.
func TestPairFinishOnAStaleBaseIsRefusedThenRebased(t *testing.T) {
	w := newPairWorld(t)
	w.fake.PairedFinish = true
	holder := decodeJoin(t, w.join(t, map[string]any{"goal": "ship-it"}))
	commit(t, holder.Path, "api/one.go", "package api\n")
	gitIn(t, holder.Path, "push", "--quiet", "-u", "origin", holder.Branch)
	pair := decodeJoin(t, w.pairTool(t, "mesh_join", map[string]any{"goal": "ship-it", "pair": true, "item": "1"}))

	holderHead := commit(t, holder.Path, "api/two.go", "package api\n")
	if out := w.tool(t, "mesh_finish", nil); !strings.HasPrefix(out, "finished ship-it/1: pushed mesh/ship-it-1 at "+holderHead) {
		t.Fatalf("holder's mesh_finish = %q", out)
	}

	commit(t, pair.Path, "api/three.go", "package api\n")
	out := w.pairTool(t, "mesh_finish", nil)
	want := "push refused: the branch moved on origin; run git pull --rebase in " + pair.Path + " and finish again"
	if out != want {
		t.Fatalf("pair's mesh_finish = %q, want %q", out, want)
	}
	if n := len(w.fake.CallsNamed("finishWorkItem")); n != 1 {
		t.Fatalf("finishWorkItem %d times after a refused push", n)
	}
	if got := gitIn(t, w.origin, "rev-parse", "refs/heads/mesh/ship-it-1"); got != holderHead {
		t.Fatalf("origin moved to %s, holder's head %s", got, holderHead)
	}
	if held, _ := ReadState(w.pair.Join.StatePath); len(held.Held) != 1 {
		t.Fatalf("a refused push dropped the item: %+v", held)
	}

	gitIn(t, pair.Path, "pull", "--quiet", "--rebase")
	pairHead := gitIn(t, pair.Path, "rev-parse", "HEAD")
	if out := w.pairTool(t, "mesh_finish", nil); !strings.HasPrefix(out, "finished ship-it/1: pushed mesh/ship-it-1 at "+pairHead) {
		t.Fatalf("pair's mesh_finish after the rebase = %q", out)
	}
	finishes := w.fake.CallsNamed("finishWorkItem")
	if len(finishes) != 2 || finishes[1].Input["headSha"] != pairHead {
		t.Fatalf("finishWorkItem %v", finishes)
	}
	if got := gitIn(t, w.origin, "rev-parse", "refs/heads/mesh/ship-it-1"); got != pairHead {
		t.Fatalf("origin at %s, pair's head %s", got, pairHead)
	}
	if got := gitIn(t, w.origin, "merge-base", "--is-ancestor", holderHead, pairHead); got != "" {
		t.Fatalf("merge-base: %s", got)
	}
}

// A pair whose token the platform refuses — the item held by another
// node — is answered `pair refused: <reason>`, and nothing is claimed
// or released.
func TestPairRefusedTokenIsTheAnswer(t *testing.T) {
	w := newPairWorld(t)
	w.fake.HeldBy(jItemID, jNodeID)
	w.fake.TokenNodeID = jPairNodeID // the caller is the pair, not the holder: refused
	out := w.pairTool(t, "mesh_join", map[string]any{"goal": "ship-it", "item": float64(1), "pair": true})
	if !strings.HasPrefix(out, "pair refused: ") {
		t.Fatalf("answer %q", out)
	}
	if n := len(w.fake.Claims) + len(w.fake.CallsNamed("releaseWorkItem")); n != 0 {
		t.Fatalf("claims %d, releases %d", len(w.fake.Claims), len(w.fake.CallsNamed("releaseWorkItem")))
	}
	if s, _ := ReadState(w.pair.Join.StatePath); len(s.Held) != 0 {
		t.Fatalf("state holds %+v", s.Held)
	}
}

// pair:true on an item nobody holds claims it as ever.
func TestPairOnAReadyItemClaims(t *testing.T) {
	w := newPairWorld(t)
	a := decodeJoin(t, w.pairTool(t, "mesh_join", map[string]any{"goal": "ship-it", "pair": true}))
	if a.Paired || len(w.fake.Claims) != 1 || w.fake.Claims[0].Input["nodeId"] != jPairNodeID {
		t.Fatalf("answer %+v, claims %v", a, w.fake.Claims)
	}
}

// Leaving a paired item drops it here and sends no release: the claim
// is the holder's.
func TestPairReleaseLeavesTheHoldersClaim(t *testing.T) {
	w := newPairWorld(t)
	w.join(t, map[string]any{"goal": "ship-it"})
	w.pairTool(t, "mesh_join", map[string]any{"goal": "ship-it", "item": float64(1), "pair": true})
	if out := w.pairTool(t, "mesh_release", map[string]any{"reason": "done pairing"}); !strings.HasPrefix(out, "left ship-it/1: done pairing") {
		t.Fatalf("mesh_release = %q", out)
	}
	if n := len(w.fake.CallsNamed("releaseWorkItem")); n != 0 {
		t.Fatalf("releaseWorkItem %d times", n)
	}
	if got := w.fake.WorkItems[jItemID]["nodeId"]; got != jNodeID {
		t.Fatalf("the item is now %v's", got)
	}
}
