package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// A job carrying its org's KeptRoot (ADR-034) keeps its note,
// output.jsonl and stderr.log there, and nothing under another org's
// Kept root or the harness's WorkRoot/kept-sessions.
func TestOpenKeptSessionWritesUnderTheJobsOrgKeptRoot(t *testing.T) {
	mesh := t.TempDir()
	keptA := filepath.Join(mesh, "orgs", "acme", "work", "kept-sessions")
	keptB := filepath.Join(mesh, "orgs", "grapevine", "work", "kept-sessions")
	if err := os.MkdirAll(keptB, 0o700); err != nil {
		t.Fatal(err)
	}
	workRoot := filepath.Join(mesh, "work")
	job := Job{Kind: KindEvaluate, Label: "ship-it/2 evaluate", KeptRoot: keptA, Meta: SessionMeta{ItemID: "item-a"}}
	dir, out, errf, err := openKeptSession(workRoot, job, time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	out.Close()
	errf.Close()
	if want := filepath.Join(keptA, "ship-it-2-1700000000"); dir != want {
		t.Fatalf("kept dir %s, want %s", dir, want)
	}
	for _, f := range []string{sessionunit.StateFile, sessionunit.OutputFile, sessionunit.StderrFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("%s not under org A's Kept root: %v", f, err)
		}
	}
	entries, err := os.ReadDir(keptB)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("org B's Kept root holds %d entries, want none", len(entries))
	}
	if _, err := os.Stat(KeptSessionsDir(workRoot)); !os.IsNotExist(err) {
		t.Fatalf("WorkRoot/kept-sessions made for a job with a KeptRoot (err %v)", err)
	}
	if strings.HasPrefix(dir, keptB) {
		t.Fatalf("kept dir %s under org B's root", dir)
	}
}

// A job with no KeptRoot keeps today's WorkRoot/kept-sessions.
func TestOpenKeptSessionWithoutKeptRootUsesWorkRoot(t *testing.T) {
	root := t.TempDir()
	dir, out, errf, err := openKeptSession(root, Job{Kind: KindEvaluate, Label: "g/1"}, time.Unix(5, 0))
	if err != nil {
		t.Fatal(err)
	}
	out.Close()
	errf.Close()
	if want := filepath.Join(root, "kept-sessions", "g-1-5"); dir != want {
		t.Fatalf("kept dir %s, want %s", dir, want)
	}
	if _, _, _, err := openKeptSession("", Job{}, time.Unix(5, 0)); err == nil {
		t.Fatal("no work root and no KeptRoot: want a refusal")
	}
}
