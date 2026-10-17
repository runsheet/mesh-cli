package node

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func writeTranscript(t *testing.T, name string, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResumeStoreKeepLookup(t *testing.T) {
	rs := RunState{Dir: t.TempDir()}
	body := `{"type":"system","subtype":"init","session_id":"sess-1"}` + "\n" + `{"type":"result","result":"done"}` + "\n"
	src := writeTranscript(t, "output.jsonl", body)
	rec := ResumeRecord{SessionID: "sess-1", Adapter: "claude-code", CredentialID: "cred-9", Worktree: "/home/n/.mesh/work/g/7"}
	if err := rs.Resumes().Keep("item-1", rec, src); err != nil {
		t.Fatal(err)
	}
	got, ok := rs.Resumes().Lookup("item-1")
	if !ok {
		t.Fatal("Lookup: no record")
	}
	if got.ItemID != "item-1" || got.SessionID != "sess-1" || got.Adapter != "claude-code" || got.CredentialID != "cred-9" || got.Worktree != "/home/n/.mesh/work/g/7" {
		t.Fatalf("Lookup = %+v", got)
	}
	if got.KeptAt.IsZero() {
		t.Fatal("KeptAt not set")
	}
	data, err := os.ReadFile(got.Transcript)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, []byte(body)) {
		t.Fatalf("transcript = %q, want %q", data, body)
	}
	if !strings.HasPrefix(got.Transcript, rs.Dir) {
		t.Fatalf("transcript %s not under the run state dir %s", got.Transcript, rs.Dir)
	}
	if _, ok := rs.Resumes().Lookup("item-2"); ok {
		t.Fatal("Lookup found a record never kept")
	}
}

func TestResumeStoreKeepsCodexRolloutName(t *testing.T) {
	rs := RunState{Dir: t.TempDir()}
	name := "rollout-2026-10-13T10-00-00-thread-1.jsonl"
	src := writeTranscript(t, name, "{}\n")
	if err := rs.Resumes().Keep("item-1", ResumeRecord{SessionID: "thread-1", Adapter: "codex"}, src); err != nil {
		t.Fatal(err)
	}
	got, _ := rs.Resumes().Lookup("item-1")
	if filepath.Base(got.Transcript) != name {
		t.Fatalf("transcript kept as %s, want %s", got.Transcript, name)
	}
}

func TestResumeStoreSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	src := writeTranscript(t, "output.jsonl", "line\n")
	if err := (RunState{Dir: dir}).Resumes().Keep("item-1", ResumeRecord{SessionID: "sess-1", Adapter: "claude-code", Worktree: "/w"}, src); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	// A new process: a new RunState over the same dir.
	again := RunState{Dir: dir}
	got, ok := again.Resumes().Lookup("item-1")
	if !ok || got.SessionID != "sess-1" || got.Worktree != "/w" {
		t.Fatalf("after restart Lookup = %+v, %v", got, ok)
	}
	if data, err := os.ReadFile(got.Transcript); err != nil || string(data) != "line\n" {
		t.Fatalf("after restart transcript = %q, %v", data, err)
	}
}

func TestResumeStoreSweep(t *testing.T) {
	rs := RunState{Dir: t.TempDir()}
	store := rs.Resumes()
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	src := writeTranscript(t, "output.jsonl", "x\n")
	status := map[string]string{
		"merged":    "merged",
		"cancelled": "cancelled",
		"failed":    "failed",
		"ready":     "ready",
		"old":       "ready",
		"unread":    "",
	}
	for id := range status {
		kept := now.Add(-time.Hour)
		if id == "old" {
			kept = now.Add(-ResumeKeepFor - time.Hour)
		}
		if err := store.Keep(id, ResumeRecord{SessionID: "s-" + id, Adapter: "claude-code", KeptAt: kept}, src); err != nil {
			t.Fatal(err)
		}
	}
	forgot, err := store.Sweep(context.Background(), func(id string) (string, error) {
		if id == "unread" {
			return "", errors.New("gateway down")
		}
		return status[id], nil
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(forgot)
	want := []string{"cancelled", "failed", "merged", "old"}
	if strings.Join(forgot, ",") != strings.Join(want, ",") {
		t.Fatalf("Sweep forgot %v, want %v", forgot, want)
	}
	for _, id := range want {
		if _, ok := store.Lookup(id); ok {
			t.Errorf("%s still kept after Sweep", id)
		}
		if _, err := os.Stat(filepath.Join(store.Dir, id)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: dir left behind: %v", id, err)
		}
	}
	for _, id := range []string{"ready", "unread"} {
		if _, ok := store.Lookup(id); !ok {
			t.Errorf("%s forgotten by Sweep, want kept", id)
		}
	}
}

// The resume sweep runs over every served org's own Resumes dir
// (ADR-034): a done item's record kept under each org is removed by one
// sweep of the OrgRootSet, and a record still live is kept, each org
// reading only its own dir.
func TestOrgRootSetSweepResumes(t *testing.T) {
	mesh := filepath.Join(t.TempDir(), ".mesh")
	set, err := NewOrgRootSet(mesh, []string{"acme", "grapevine"})
	if err != nil {
		t.Fatal(err)
	}
	src := writeTranscript(t, "output.jsonl", "x\n")
	for _, org := range []string{"acme", "grapevine"} {
		roots, err := set.For(org)
		if err != nil {
			t.Fatal(err)
		}
		store := ResumeStore{Dir: roots.Resumes}
		if err := store.Keep("done-item", ResumeRecord{SessionID: "s-done", Adapter: "claude-code"}, src); err != nil {
			t.Fatal(err)
		}
		if err := store.Keep("live-item", ResumeRecord{SessionID: "s-live", Adapter: "claude-code"}, src); err != nil {
			t.Fatal(err)
		}
	}
	statusOf := func(itemID string) (string, error) {
		if itemID == "done-item" {
			return "merged", nil
		}
		return "ready", nil
	}
	forgotten, err := set.SweepResumes(context.Background(), statusOf, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, org := range []string{"acme", "grapevine"} {
		if got := forgotten[org]; len(got) != 1 || got[0] != "done-item" {
			t.Fatalf("%s: forgotten = %v, want [done-item]", org, got)
		}
		roots, err := set.For(org)
		if err != nil {
			t.Fatal(err)
		}
		store := ResumeStore{Dir: roots.Resumes}
		if _, ok := store.Lookup("done-item"); ok {
			t.Fatalf("%s: done-item still kept after the sweep", org)
		}
		if _, ok := store.Lookup("live-item"); !ok {
			t.Fatalf("%s: live-item forgotten, want kept", org)
		}
	}
}

func TestResumeRecordHoldsNoSecret(t *testing.T) {
	const token = "ghs_FAKEtoken0123456789abcdef"
	const key = "sk-ant-FAKEkey9876543210"
	t.Setenv("GH_TOKEN", token)
	t.Setenv("ANTHROPIC_API_KEY", key)
	env := []string{"GH_TOKEN=" + os.Getenv("GH_TOKEN"), "ANTHROPIC_API_KEY=" + os.Getenv("ANTHROPIC_API_KEY"), "MESH_ACCOUNT=sub-1"}

	// Build the record the way a runner would: from a session's job env and ids.
	rec := ResumeRecord{SessionID: "sess-1", Adapter: "claude-code", CredentialID: "cred-1", Worktree: "/w"}
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "MESH_ACCOUNT="); ok {
			rec.CredentialID = v
		}
	}
	rs := RunState{Dir: t.TempDir()}
	src := writeTranscript(t, "output.jsonl", "{}\n")
	if err := rs.Resumes().Keep("item-1", rec, src); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(rs.Resumes().recordPath("item-1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{token, key} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("record.json holds a secret %q:\n%s", secret, data)
		}
	}
	for _, name := range []string{"GH_TOKEN", "ANTHROPIC_API_KEY"} {
		if bytes.Contains(data, []byte(name)) {
			t.Fatalf("record.json names env %s:\n%s", name, data)
		}
	}
}
