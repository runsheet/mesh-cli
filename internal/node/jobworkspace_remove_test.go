package node

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The repos root defaults to the node's home, which holds .mesh/jobs:
// a job's own workspace under it is removable — a full clone, a plain
// dir, a cancelled run's whose job meta is gone — and a shared clone
// beside the jobs dir is still refused (dl360, 2026-10-28: "remove
// workspace …/.mesh/jobs/<job>/pr-1: under the repos root /home/mesh;
// a shared clone is never removed", 539 times).
func TestAJobWorkspaceUnderTheReposRootIsRemovable(t *testing.T) {
	home := t.TempDir()
	g := &Git{ReposRoot: home}
	jobs := filepath.Join(home, ".mesh", JobsDirName)

	clone := filepath.Join(jobs, "0c1f0f34-fd14-562b-80d1-7961612fe78f", "pr-1")
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", clone).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	plain := filepath.Join(jobs, "4263cf47-e493-5a20-b63b-c4f2b852e9e8", "checkout")
	if err := os.MkdirAll(filepath.Join(plain, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{clone, plain} {
		if err := g.RemoveWorkspace(context.Background(), dir); err != nil {
			t.Fatalf("remove %s: %v", dir, err)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("%s is still there: %v", dir, err)
		}
	}

	shared := filepath.Join(home, "github.com", "acme", "widgets")
	if err := os.MkdirAll(filepath.Join(shared, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := g.RemoveWorkspace(context.Background(), shared); err == nil {
		t.Fatal("removed a shared clone under the repos root")
	}
	for _, dir := range []string{"/home/mesh/.mesh/jobs", "/home/mesh/.mesh/jobs/0c1f0f34-fd14-562b-80d1-7961612fe78f", "/home/mesh/repos/acme/widgets"} {
		if inJobWorkspace(dir) {
			t.Fatalf("%s counts as a job's workspace", dir)
		}
	}
	if !inJobWorkspace("/home/mesh/.mesh/jobs/0c1f0f34-fd14-562b-80d1-7961612fe78f/pr-1") {
		t.Fatal("a job's pr-1 is not its workspace")
	}
}
