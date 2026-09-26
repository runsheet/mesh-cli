package channel

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// mesh_record reports to the platform and nothing else: run from inside
// an item's workspace, it leaves every file and the branch's head as
// they were, and the gateway hears one progress note with the summary
// (ADR 003, decision 4).
func TestRecordReportsAndWritesNothing(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	f, h := seed(t)
	ws := filepath.Join(t.TempDir(), "ship-it", "1")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", ws}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	git("init", "-q", "-b", "mesh/ship-it-1")
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("# widgets\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "init")
	head, before := git("rev-parse", "HEAD"), snapshot(t, ws)
	t.Chdir(ws)

	if out := call(t, h, "mesh_record", map[string]any{"item_id": item1, "summary": "test written"}); out != "recorded on the platform" {
		t.Fatalf("record: %q", out)
	}

	if after := snapshot(t, ws); after != before {
		t.Fatalf("the workspace changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if got := git("rev-parse", "HEAD"); got != head {
		t.Fatalf("a commit was made: %s → %s", head, got)
	}
	if st := git("status", "--porcelain", "--untracked-files=all"); st != "" {
		t.Fatalf("the worktree is dirty:\n%s", st)
	}
	notes := f.CallsNamed("reportWorkItemProgress")
	if len(notes) != 1 || notes[0].Input["aggregateId"] != item1 || notes[0].Input["note"] != "test written" {
		t.Fatalf("progress notes: %v", notes)
	}
}

// snapshot lists every file under dir outside .git with its mode,
// modification time and contents.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		b.WriteString(rel + " " + info.Mode().String() + " " + info.ModTime().String())
		if !d.IsDir() {
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			b.WriteString(" " + strconv.Quote(string(body)))
		}
		b.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}
