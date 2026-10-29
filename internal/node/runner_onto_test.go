package node

import (
	"context"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

// mainMovesMidBuild is a build session that commits its turn and then,
// before the node validates, has main move on origin: move(), once.
type mainMovesMidBuild struct {
	*commitHarness
	move func()
}

func (h *mainMovesMidBuild) Run(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
	res, err := h.commitHarness.Run(ctx, job, on)
	if h.move != nil {
		h.move()
		h.move = nil
	}
	return res, err
}

// ontoBuild is a build of item 1 on a real checkout of an origin whose
// main declares validateYML, the session committing turn and main then
// moving by mainMoves (files) before the validation; code, set, is what
// the claim's brief adds to its code. It answers the runner's fake, the
// checkout and node.log once RunItem has returned, and main as it moved.
func ontoBuild(t *testing.T, changesets bool, code map[string]any, turn, mainMoves map[string]string) (f *gatewaytest.Fake, dir, newMain string, out string) {
	t.Helper()
	f = seed(t)
	f.Changesets = changesets
	if code != nil {
		f.KindCode = map[string]map[string]any{BuildKind: code}
	}
	h := &mainMovesMidBuild{commitHarness: &commitHarness{t: t, turns: []map[string]string{turn}}}
	r, ws, item, _ := runnerOnFake(t, f, h, nil)
	origin := originRepo(t, withValidation())
	gws := &gitWS{fakeWS: ws, t: t, origin: origin}
	r.WS = gws
	logs := &syncBuf{}
	r.Log = log.New(logs, "", 0)
	h.move = func() {
		other := filepath.Join(t.TempDir(), "other")
		gitRun(t, filepath.Dir(other), "clone", "--quiet", origin, other)
		writeFiles(t, other, mainMoves)
		gitRun(t, other, "add", "-A")
		gitRun(t, other, "commit", "--quiet", "-m", "main moves on")
		gitRun(t, other, "push", "--quiet", "origin", "HEAD:main")
		newMain = gitRun(t, other, "rev-parse", "HEAD")
	}
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatalf("RunItem: %v\n%s", err, logs.String())
	}
	if newMain == "" {
		t.Fatal("main never moved mid-build")
	}
	return f, filepath.Join(ws.root, "ship-it", "1"), newMain, logs.String()
}

// oneFinish is the one finishWorkItem call's input.
func oneFinish(t *testing.T, f *gatewaytest.Fake, out string) map[string]any {
	t.Helper()
	fin := f.CallsNamed("finishWorkItem")
	if len(fin) != 1 {
		t.Fatalf("finishWorkItem calls = %d, want 1\n%s", len(fin), out)
	}
	return fin[0].Input
}

// A brief naming onto has the build's change validated on main as it is
// now (runsheet ADR 077): main moved on web/ while the session built on
// api/, and the node applied the change onto the new main before its
// validation. The pushed head's one parent is the new main, the finish
// names the new main as its base, the attestation is of the pushed head,
// and the suites are the change's alone (vet, api — not main's web).
func TestBuildValidatesOntoMain(t *testing.T) {
	onto := map[string]any{"onto": map[string]any{"ref": "main"}, "applyMode": "linear"}
	f, dir, newMain, out := ontoBuild(t, false, onto,
		map[string]string{"api/api.go": "package api\n\nconst X = 1\n"},
		map[string]string{"web/web.go": "package web\n\nconst Moved = 1\n"})
	in := oneFinish(t, f, out)
	head, _ := in["headSha"].(string)
	if head == "" || head != gitRun(t, dir, "rev-parse", "HEAD") {
		t.Fatalf("finished on %v, the checkout's HEAD is %s\n%s", in["headSha"], gitRun(t, dir, "rev-parse", "HEAD"), out)
	}
	if parents := gitRun(t, dir, "rev-parse", head+"^@"); parents != newMain {
		t.Fatalf("pushed head's parents %q, want the new main %s alone\n%s", parents, newMain, out)
	}
	if in[gateway.BaseShaAtFinishField] != newMain {
		t.Fatalf("finish names base %v, want the new main %s", in[gateway.BaseShaAtFinishField], newMain)
	}
	if _, ok := in[gateway.RebaseNeededField]; ok {
		t.Fatalf("a clean apply's finish says rebaseNeeded: %v", in)
	}
	if got := gitRun(t, dir, "show", head+":web/web.go"); !strings.Contains(got, "Moved") {
		t.Fatalf("the head lacks main's move: %q", got)
	}
	if got := gitRun(t, dir, "show", head+":api/api.go"); !strings.Contains(got, "X = 1") {
		t.Fatalf("the head lacks the change: %q", got)
	}
	a := uploadedAttestation(t, f)
	if a["commit"] != head {
		t.Fatalf("attested %v, want the pushed head %s", a["commit"], head)
	}
	if got := strings.Join(attestationSteps(t, f.CallsNamed("recordValidationAttestation")[0]), ","); got != "vet,api" {
		t.Fatalf("suites run %s, want vet,api: selected by the diff from onto", got)
	}
	if !strings.Contains(out, "onto: applied ") || !strings.Contains(out, "onto main "+short(newMain)) {
		t.Fatalf("node.log lacks the apply line:\n%s", out)
	}
}

// On a platform that holds changesets the changeset is the applied
// head's tree on the new main's: the attestation's tree is the
// changeset's, and the finish names the new main.
func TestBuildValidatesOntoMainChangeset(t *testing.T) {
	onto := map[string]any{"onto": map[string]any{"ref": "main"}}
	f, dir, newMain, out := ontoBuild(t, true, onto,
		map[string]string{"api/api.go": "package api\n\nconst X = 1\n"},
		map[string]string{"web/web.go": "package web\n\nconst Moved = 1\n"})
	css := f.ChangesetsOf(item1)
	if len(css) != 1 {
		t.Fatalf("changesets = %+v, want one\n%s", css, out)
	}
	cs := css[0]
	if tree := gitRun(t, dir, "rev-parse", "HEAD^{tree}"); cs.Tree != tree {
		t.Fatalf("changeset tree %s, HEAD's %s", cs.Tree, tree)
	}
	if base := gitRun(t, dir, "rev-parse", newMain+"^{tree}"); cs.BaseTree != base {
		t.Fatalf("changeset base tree %s, the new main's %s", cs.BaseTree, base)
	}
	if strings.Join(cs.PathsChanged, ",") != "api/api.go" {
		t.Fatalf("changeset paths %v, want the change's alone", cs.PathsChanged)
	}
	if in := oneFinish(t, f, out); in[gateway.BaseShaAtFinishField] != newMain {
		t.Fatalf("finish names base %v, want the new main %s", in[gateway.BaseShaAtFinishField], newMain)
	}
	a := uploadedAttestation(t, f)
	if a["treeHash"] != cs.Tree {
		t.Fatalf("attested tree %v, the changeset's %s", a["treeHash"], cs.Tree)
	}
}

// A change that conflicts with the new main is validated on its own base
// as before, its head untouched, and its finish says rebaseNeeded.
func TestBuildOntoConflictAsksForARebase(t *testing.T) {
	onto := map[string]any{"onto": map[string]any{"ref": "main"}, "applyMode": "linear"}
	f, dir, newMain, out := ontoBuild(t, false, onto,
		map[string]string{"api/api.go": "package api\n\nconst X = 1\n"},
		map[string]string{"api/api.go": "package api\n\nconst X = 2\n"})
	in := oneFinish(t, f, out)
	if in[gateway.RebaseNeededField] != true {
		t.Fatalf("finish %v, want rebaseNeeded\n%s", in, out)
	}
	head := gitRun(t, dir, "rev-parse", "HEAD")
	if in["headSha"] != head || gitRun(t, dir, "log", "-1", "--format=%s") != "turn 1" {
		t.Fatalf("the head moved: finished %v, HEAD %s (%s)", in["headSha"], head, gitRun(t, dir, "log", "-1", "--format=%s"))
	}
	if gitRun(t, dir, "rev-parse", head+"^") == newMain {
		t.Fatal("the head was put on the new main")
	}
	if in[gateway.BaseShaAtFinishField] == newMain {
		t.Fatalf("finish names the new main as its base")
	}
	if !strings.Contains(out, "the change conflicts with main "+short(newMain)+" in api/api.go") {
		t.Fatalf("node.log lacks the conflict's paths:\n%s", out)
	}
	if uploadedAttestation(t, f)["commit"] != head {
		t.Fatal("the attestation is not of the head")
	}
}

// A brief without onto is validated and finished as before: on the
// session's head, its base the checkout's, nothing applied and no
// rebaseNeeded — the same finish, field for field, as a brief whose
// onto never came.
func TestBuildWithoutOntoIsUnchanged(t *testing.T) {
	turn := map[string]string{"api/api.go": "package api\n\nconst X = 1\n"}
	moves := map[string]string{"web/web.go": "package web\n\nconst Moved = 1\n"}
	f, dir, newMain, out := ontoBuild(t, false, nil, turn, moves)
	in := oneFinish(t, f, out)
	head := gitRun(t, dir, "rev-parse", "HEAD")
	if in["headSha"] != head || gitRun(t, dir, "log", "-1", "--format=%s") != "turn 1" {
		t.Fatalf("finished %v, HEAD %s", in["headSha"], head)
	}
	if gitRun(t, dir, "rev-parse", head+"^") == newMain || in[gateway.BaseShaAtFinishField] == newMain {
		t.Fatal("a brief without onto was put on the new main")
	}
	if _, ok := in[gateway.RebaseNeededField]; ok {
		t.Fatalf("finish says rebaseNeeded: %v", in)
	}
	if strings.Contains(out, "onto:") {
		t.Fatalf("node.log speaks of onto:\n%s", out)
	}
	// the same inputs, field for field, as an empty code's brief: the
	// fields that differ run to run (the head, the base, the cost) aside
	f2, _, _, out2 := ontoBuild(t, false, map[string]any{}, turn, moves)
	in2 := oneFinish(t, f2, out2)
	keys := func(m map[string]any) string {
		var ks []string
		for k := range m {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		return strings.Join(ks, ",")
	}
	if keys(in) != keys(in2) {
		t.Fatalf("finish fields %s, with an empty code %s", keys(in), keys(in2))
	}
}
