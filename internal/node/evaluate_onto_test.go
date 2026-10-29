package node

import (
	"context"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

// ontoEvalWS is fakeWS on a real checkout: Prepare clones origin onto
// the item's branch, and Diff and ChangedPaths are Git's, each base it
// was asked for noted.
type ontoEvalWS struct {
	*fakeWS
	t      *testing.T
	origin string
	diffs  []string
}

func (w *ontoEvalWS) Prepare(ctx context.Context, spec WorkspaceSpec) (string, error) {
	dir, err := w.fakeWS.Prepare(ctx, spec)
	if err != nil {
		return "", err
	}
	gitRun(w.t, dir, "clone", "--quiet", "--branch", spec.Branch, w.origin, ".")
	return dir, nil
}

func (w *ontoEvalWS) Diff(ctx context.Context, dir, base string) (string, error) {
	w.mu.Lock()
	w.diffs = append(w.diffs, base)
	w.mu.Unlock()
	return (&Git{}).Diff(ctx, dir, base)
}

func (w *ontoEvalWS) ChangedPaths(ctx context.Context, dir, base string) ([]string, error) {
	return (&Git{}).ChangedPaths(ctx, dir, base)
}

// ontoBriefGW is the evaluator's gateway with code added to the brief
// the kind's catalogue row makes (jobBriefOf's fallback).
type ontoBriefGW struct {
	*gateway.Client
	code gateway.JobCode
}

func (g ontoBriefGW) JobBriefOf(kind string, s gateway.JobSubject) (*gateway.JobBrief, bool) {
	b := jobBriefOf(context.Background(), g.Client, kind, s.ID)
	b.Code = g.code
	return &b, true
}

// ontoEvaluation evaluates item 1, branch mesh/ship-it-1 committing
// change on main, after main moved by mainMoves; code, set, is the
// brief's. It answers the fake, the evaluator's harness, the checkout,
// the new main and node.log.
func ontoEvaluation(t *testing.T, code *gateway.JobCode, change, mainMoves map[string]string) (f fakeAndHarness, ws *ontoEvalWS, newMain, out string) {
	t.Helper()
	fx, ev, h, _ := secondFixture(t, "normal", "Holds.\nVERDICT: PASS", &fakeCodexH{})
	origin := originRepo(t, nil)
	seedDir := filepath.Join(t.TempDir(), "seed")
	gitRun(t, filepath.Dir(seedDir), "clone", "--quiet", origin, seedDir)
	gitRun(t, seedDir, "checkout", "--quiet", "-b", "mesh/ship-it-1")
	writeFiles(t, seedDir, change)
	gitRun(t, seedDir, "add", "-A")
	gitRun(t, seedDir, "commit", "--quiet", "-m", "the item's change")
	gitRun(t, seedDir, "push", "--quiet", "origin", "HEAD:mesh/ship-it-1")
	gitRun(t, seedDir, "checkout", "--quiet", "main")
	writeFiles(t, seedDir, mainMoves)
	gitRun(t, seedDir, "add", "-A")
	gitRun(t, seedDir, "commit", "--quiet", "-m", "main moves on")
	gitRun(t, seedDir, "push", "--quiet", "origin", "HEAD:main")
	newMain = gitRun(t, seedDir, "rev-parse", "HEAD")

	ws = &ontoEvalWS{fakeWS: ev.WS.(*fakeWS), t: t, origin: origin}
	ev.WS = ws
	if code != nil {
		ev.GW = ontoBriefGW{Client: ev.GW.(*gateway.Client), code: *code}
	}
	logs := &syncBuf{}
	ev.Log = log.New(logs, "", 0)
	item, err := ev.GW.WorkItem(context.Background(), item1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.Evaluate(context.Background(), *item); err != nil {
		t.Fatalf("Evaluate: %v\n%s", err, logs.String())
	}
	return fakeAndHarness{f: fx, jobs: h.jobs}, ws, newMain, logs.String()
}

// The evaluator is handed main as it is now with the change on it: the
// checkout is detached at a commit whose one parent is the new main and
// which holds the change, and the diff is taken from main.
func TestEvaluateOntoMain(t *testing.T) {
	code := &gateway.JobCode{Form: "commit", Onto: &gateway.JobOnto{Ref: "main"}}
	fh, ws, newMain, out := ontoEvaluation(t, code,
		map[string]string{"api/item.go": "package api\n\nconst Item = 1\n"},
		map[string]string{"web/moved.go": "package web\n\nconst Moved = 1\n"})
	if len(fh.jobs) != 1 {
		t.Fatalf("evaluator sessions = %d, want one\n%s", len(fh.jobs), out)
	}
	dir := fh.jobs[0].Workspace
	if got := gitRun(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != "HEAD" {
		t.Fatalf("the checkout is on %s, want detached", got)
	}
	if parents := gitRun(t, dir, "rev-parse", "HEAD^@"); parents != newMain {
		t.Fatalf("HEAD's parents %q, want the new main %s alone\n%s", parents, newMain, out)
	}
	if !strings.Contains(gitRun(t, dir, "show", "HEAD:api/item.go"), "Item = 1") || !strings.Contains(gitRun(t, dir, "show", "HEAD:web/moved.go"), "Moved") {
		t.Fatal("HEAD lacks the change or main's move")
	}
	if got := strings.Join(ws.diffs, ","); got != "main" {
		t.Fatalf("Diff asked for %q, want main", got)
	}
	prompt := fh.jobs[0].Prompt
	if !strings.Contains(prompt, "api/item.go") || strings.Contains(prompt, "web/moved.go") {
		t.Fatalf("the prompt's diff is not the change's alone from the new main:\n%s", prompt)
	}
	if !strings.Contains(out, "onto: applied ") || !strings.Contains(out, "onto main "+short(newMain)) {
		t.Fatalf("node.log lacks the apply line:\n%s", out)
	}
	closed := evaluationClosed(t, fh.f)
	if res, _ := closed["finish"].(map[string]any)["result"].(map[string]any); strings.HasPrefix(stringOf(res["reason"]), OntoConflictPrefix) {
		t.Fatalf("a clean apply ended onto_conflict: %v", res)
	}
}

// A change that does not apply onto main starts no session: the job
// ends with the node's outcome, onto_conflict: <paths>.
func TestEvaluateOntoConflict(t *testing.T) {
	code := &gateway.JobCode{Form: "commit", Onto: &gateway.JobOnto{Ref: "main"}, ApplyMode: gateway.ApplyLinear}
	fh, ws, _, out := ontoEvaluation(t, code,
		map[string]string{"api/api.go": "package api\n\nconst X = 1\n"},
		map[string]string{"api/api.go": "package api\n\nconst X = 2\n"})
	if len(fh.jobs) != 0 {
		t.Fatalf("an evaluator session ran on a change that does not apply\n%s", out)
	}
	if len(ws.diffs) != 0 {
		t.Fatalf("the branch was diffed: %v", ws.diffs)
	}
	closed := evaluationClosed(t, fh.f)
	res, _ := closed["finish"].(map[string]any)["result"].(map[string]any)
	if res["outcome"] != "no_verdict" || res["reason"] != "onto_conflict: api/api.go" {
		t.Fatalf("job closed with %v, want no_verdict onto_conflict: api/api.go\n%s", res, out)
	}
}

// A brief without onto evaluates the branch as before: on its own head,
// diffed from the item's base, nothing applied.
func TestEvaluateWithoutOntoIsUnchanged(t *testing.T) {
	fh, ws, newMain, out := ontoEvaluation(t, nil,
		map[string]string{"api/item.go": "package api\n\nconst Item = 1\n"},
		map[string]string{"web/moved.go": "package web\n\nconst Moved = 1\n"})
	if len(fh.jobs) != 1 {
		t.Fatalf("evaluator sessions = %d, want one\n%s", len(fh.jobs), out)
	}
	dir := fh.jobs[0].Workspace
	if got := gitRun(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != "mesh/ship-it-1" {
		t.Fatalf("the checkout is on %s, want the branch", got)
	}
	if gitRun(t, dir, "rev-parse", "HEAD^") == newMain {
		t.Fatal("a brief without onto was put on the new main")
	}
	if got := strings.Join(ws.diffs, ","); got != "main" {
		t.Fatalf("Diff asked for %q, want main", got)
	}
	if strings.Contains(out, "onto:") {
		t.Fatalf("node.log speaks of onto:\n%s", out)
	}
}

func stringOf(v any) string { s, _ := v.(string); return s }

// fakeAndHarness is an evaluation's fake and the evaluator jobs its
// harness ran.
type fakeAndHarness struct {
	f    *gatewaytest.Fake
	jobs []harness.Job
}
