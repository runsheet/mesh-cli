package nodetools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

type dirGrant string

func (g dirGrant) Base() string { return string(g) }

// apply_onto fetches the base and the changes through the grant, applies
// them under the brief's identity, checks the result out and records it;
// a change that does not merge is answered as a conflict and recorded
// with exit 1. The same brief writes the same commit.
func TestApplyOnto(t *testing.T) {
	root := t.TempDir()
	up := filepath.Join(root, "up", "acme", "fixture")
	os.MkdirAll(up, 0o755)
	gitIn(t, up, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(up, "a.txt"), []byte("a\n"), 0o644)
	gitIn(t, up, "add", "-A")
	gitIn(t, up, "commit", "-qm", "base")
	base := gitIn(t, up, "rev-parse", "HEAD")
	gitIn(t, up, "checkout", "-qb", "change")
	os.WriteFile(filepath.Join(up, "b.txt"), []byte("b\n"), 0o644)
	gitIn(t, up, "add", "-A")
	gitIn(t, up, "commit", "-qm", "add b")
	change := gitIn(t, up, "rev-parse", "HEAD")
	gitIn(t, up, "checkout", "-qb", "clash", base)
	os.WriteFile(filepath.Join(up, "a.txt"), []byte("clash\n"), 0o644)
	gitIn(t, up, "commit", "-qam", "clash a")
	clash := gitIn(t, up, "rev-parse", "HEAD")
	gitIn(t, up, "checkout", "-q", "main")
	os.WriteFile(filepath.Join(up, "a.txt"), []byte("main\n"), 0o644)
	gitIn(t, up, "commit", "-qam", "main moves a")
	onto := gitIn(t, up, "rev-parse", "HEAD")
	// the repository is reached at <base>acme/fixture.git, as through a grant
	os.Rename(up, up+".git")
	grant := dirGrant(filepath.Join(root, "up") + "/")

	work := func() string {
		w := t.TempDir()
		gitIn(t, w, "init", "-q")
		return w
	}
	as := &meshapi.ApplyAs{Name: "Mesh Queue", Email: "queue@projectmesh.io", Date: "2026-10-29T10:00:00Z"}
	ok := &meshapi.Code{Onto: &meshapi.Onto{Ref: "refs/heads/main", SHA: onto}, ApplyMode: "merge", ApplyAs: as,
		Apply: []meshapi.Apply{{SHA: change, Ref: "refs/heads/change"}}}
	bad := &meshapi.Code{Onto: &meshapi.Onto{SHA: onto}, ApplyMode: "linear", ApplyAs: as, Apply: []meshapi.Apply{{SHA: clash}}}

	var shas []string
	for range 2 {
		m := &fakeMesh{}
		rec, _ := recorder(t, m)
		w := work()
		text, err := (&ApplyOnto{Grant: grant, Repo: "acme/fixture", Workspace: w, Code: ok, Recorder: rec}).Tool().Call(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		var ans struct{ SHA, Tree, EvidenceID string }
		json.Unmarshal([]byte(text), &struct {
			SHA        *string `json:"sha"`
			Tree       *string `json:"tree"`
			EvidenceID *string `json:"evidence_id"`
		}{&ans.SHA, &ans.Tree, &ans.EvidenceID})
		if ans.SHA == "" || ans.Tree == "" || ans.EvidenceID == "" {
			t.Fatalf("answered %s", text)
		}
		if head := gitIn(t, w, "rev-parse", "HEAD"); head != ans.SHA {
			t.Fatalf("the checkout is at %s, not the applied %s", head, ans.SHA)
		}
		if parents := gitIn(t, w, "rev-list", "--parents", "-n1", ans.SHA); parents != ans.SHA+" "+onto+" "+change {
			t.Fatalf("a merge apply's parents: %s", parents)
		}
		st, _ := statementOf(t, m.records[0])
		if st.Predicate.Tool != ApplyOntoTool || st.Predicate.Exit != 0 {
			t.Fatalf("statement %+v", st)
		}
		shas = append(shas, ans.SHA)
	}
	if shas[0] != shas[1] {
		t.Fatalf("two applies of one brief wrote %v", shas)
	}

	m := &fakeMesh{}
	rec, _ := recorder(t, m)
	text, err := (&ApplyOnto{Grant: grant, Repo: "acme/fixture", Workspace: work(), Code: bad, Recorder: rec}).Tool().Call(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var ans struct {
		Conflict *struct {
			Index int      `json:"index"`
			Paths []string `json:"paths"`
		} `json:"conflict"`
		EvidenceID string `json:"evidence_id"`
	}
	json.Unmarshal([]byte(text), &ans)
	if ans.Conflict == nil || ans.Conflict.Index != 0 || len(ans.Conflict.Paths) != 1 || ans.Conflict.Paths[0] != "a.txt" || ans.EvidenceID == "" {
		t.Fatalf("a clash answered %s", text)
	}
	if st, _ := statementOf(t, m.records[0]); st.Predicate.Exit != 1 {
		t.Fatalf("a conflict is recorded with exit %d", st.Predicate.Exit)
	}

	if _, err := (&ApplyOnto{Grant: grant, Repo: "acme/fixture", Workspace: work(), Code: &meshapi.Code{}, Recorder: rec}).Tool().Call(context.Background(), nil); err == nil {
		t.Fatal("a brief with nothing to apply applied")
	}
}
