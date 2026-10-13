package node

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// A build run to its end leaves its session in the resume store before
// its session dir goes: once RemoveSession has run, Lookup has a record
// for the item naming the session, the adapter and the worktree, and the
// transcript it copied out is still on disk (goal
// a-failed-check-returns-to-the-session-that-built-it).
func TestAFinishedBuildLeavesItsResumeRecord(t *testing.T) {
	var workspace string
	h := harnessFunc(func(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
		workspace = job.Workspace
		dir := sessionunit.Dir(job.Workspace)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return harness.Result{}, err
		}
		if err := os.WriteFile(filepath.Join(dir, sessionunit.OutputFile), []byte(`{"type":"result","session_id":"s-kept"}`+"\n"), 0o600); err != nil {
			return harness.Result{}, err
		}
		return harness.Result{Success: true, Text: "done", CostUSD: 1, SessionID: "s-kept"}, nil
	})
	r, _, item, _ := runnerOnFake(t, seed(t), h, nil)
	store := ResumeStore{Dir: t.TempDir()}
	r.Resumes = &store
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sessionunit.Dir(workspace)); !os.IsNotExist(err) {
		t.Fatalf("session dir %s still there after the build's end: %v", sessionunit.Dir(workspace), err)
	}
	rec, ok := store.Lookup(item.ID)
	if !ok {
		t.Fatal("no resume record for the finished build's item")
	}
	if rec.SessionID != "s-kept" || rec.Adapter != "claude-code" || rec.Worktree != workspace {
		t.Fatalf("record = %+v, want session s-kept, claude-code, worktree %s", rec, workspace)
	}
	data, err := os.ReadFile(rec.Transcript)
	if err != nil {
		t.Fatalf("kept transcript: %v", err)
	}
	if string(data) != `{"type":"result","session_id":"s-kept"}`+"\n" {
		t.Fatalf("kept transcript = %q", data)
	}
}

// A runner with no store keeps nothing, and a build still finishes.
func TestNoResumeStoreKeepsNothing(t *testing.T) {
	h := harnessFunc(func(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
		return harness.Result{Success: true, Text: "done", CostUSD: 1, SessionID: "s-1"}, nil
	})
	r, ws, item, _ := runnerOnFake(t, seed(t), h, nil)
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if len(ws.pushed) != 1 {
		t.Fatalf("pushed %v", ws.pushed)
	}
}
