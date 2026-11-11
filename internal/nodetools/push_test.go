package nodetools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A tainted lease's push is refused at once, naming the taint, and never
// reaches Grant or Pusher; Tainted answering false or left nil changes
// nothing — the same push goes through as before.
func TestPush_Tainted(t *testing.T) {
	w := t.TempDir()
	gitIn(t, w, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(w, "a.txt"), []byte("a\n"), 0o644)
	gitIn(t, w, "add", "-A")
	gitIn(t, w, "commit", "-qm", "base")

	m := &fakeMesh{}
	rec, _ := recorder(t, m)

	fatalPusher := func(ctx context.Context, ref, sha, lease string) (PushOutcome, error) {
		t.Fatal("Pusher called while the lease is tainted")
		return PushOutcome{}, nil
	}
	p := &Push{Pusher: fatalPusher, Grant: fatalGrant{t}, Repo: "acme/fixture", Workspace: w, Recorder: rec,
		PushTo: "main", Tainted: func() bool { return true }}
	if _, err := p.Tool().Call(context.Background(), map[string]any{"ref": "main"}); err == nil || !strings.Contains(err.Error(), "tainted") {
		t.Fatalf("a tainted push answered %v, want a refusal naming the taint", err)
	}

	for _, tc := range []struct {
		name    string
		tainted func() bool
	}{
		{"false", func() bool { return false }},
		{"nil", nil},
	} {
		var called bool
		pusher := func(ctx context.Context, ref, sha, lease string) (PushOutcome, error) {
			called = true
			return PushOutcome{Pushed: true}, nil
		}
		p := &Push{Pusher: pusher, Repo: "acme/fixture", Workspace: w, Recorder: rec, PushTo: "main", Tainted: tc.tainted}
		text, err := p.Tool().Call(context.Background(), map[string]any{"ref": "main"})
		if err != nil {
			t.Fatalf("Tainted=%s: %v", tc.name, err)
		}
		if !called {
			t.Fatalf("Tainted=%s: the pusher was never called", tc.name)
		}
		var ans struct {
			Pushed bool `json:"pushed"`
		}
		if err := json.Unmarshal([]byte(text), &ans); err != nil || !ans.Pushed {
			t.Fatalf("Tainted=%s: answered %s (%v)", tc.name, text, err)
		}
	}
}

// fatalGrant fails the test if it is ever asked for its base: a tainted
// push must never touch Grant.
type fatalGrant struct{ t *testing.T }

func (g fatalGrant) Base() string {
	g.t.Fatal("Grant.Base called while the lease is tainted")
	return ""
}

// fileGrant is a Grant whose Base is a file:// URL, so NodePush's
// grant-fallback path (no Pusher) pushes through a real git transport,
// as a lease's loopback relay does, and is as reachable by a
// url.*.insteadOf rewrite as the relay's http base is.
type fileGrant struct{ base string }

func (g fileGrant) Base() string { return g.base }

// NodePush's grant-fallback path (no Pusher, used when a lease has no
// relay sidecar) is the node's own push of a lease's final pin
// (nodev1.pushPin, adr-050). A session could point a url.*.insteadOf
// rewrite at the grant's own base, in the checkout it controls, and
// have the node's push silently land somewhere else while answering
// success — the same redirection the push tool itself already refuses
// (TestPushToolIgnoresWhatTheSessionDidToItsCheckout, in nodev1). This
// proves NodePush refuses it too, before git ever runs, and neither the
// real target nor the rewrite's decoy ever receives the push.
func TestNodePushRefusesAURLRewriteInTheGrantFallback(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real", "acme", "fixture.git")
	decoyDir := filepath.Join(root, "decoy", "acme", "fixture.git")
	for _, d := range []string{realDir, decoyDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		gitIn(t, d, "init", "-q", "--bare", "-b", "main")
	}
	realBase := "file://" + filepath.Join(root, "real") + "/"
	decoyBase := "file://" + filepath.Join(root, "decoy") + "/"

	w := t.TempDir()
	gitIn(t, w, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(w, "a.txt"), []byte("one\n"), 0o644)
	gitIn(t, w, "add", "-A")
	gitIn(t, w, "commit", "-qm", "one")
	sha1 := gitIn(t, w, "rev-parse", "HEAD")

	grant := fileGrant{base: realBase}
	if err := NodePush(context.Background(), grant, nil, w, "acme/fixture", "refs/heads/main", sha1); err != nil {
		t.Fatalf("the honest push: %v", err)
	}
	if got := gitIn(t, realDir, "rev-parse", "refs/heads/main"); got != sha1 {
		t.Fatalf("the real repo's main is %q, want %s", got, sha1)
	}

	// the session plants a rewrite redirecting the grant's own base to
	// the decoy, in the checkout's own local config, as it could the
	// vendor's URL before any push tool call
	os.WriteFile(filepath.Join(w, "b.txt"), []byte("two\n"), 0o644)
	gitIn(t, w, "add", "-A")
	gitIn(t, w, "commit", "-qm", "two")
	sha2 := gitIn(t, w, "rev-parse", "HEAD")
	gitIn(t, w, "config", "url."+decoyBase+".insteadOf", realBase)

	err := NodePush(context.Background(), grant, nil, w, "acme/fixture", "refs/heads/main", sha2)
	if err == nil || !strings.Contains(err.Error(), "rewrites URLs") {
		t.Fatalf("NodePush with a rewritten origin answered %v, want a refusal naming the rewrite", err)
	}
	if got := gitIn(t, realDir, "rev-parse", "refs/heads/main"); got != sha1 {
		t.Fatalf("the real repo's main moved to %q, want it left at %s", got, sha1)
	}
	if out, decoyErr := exec.Command("git", "-C", decoyDir, "rev-parse", "--verify", "-q", "refs/heads/main").CombinedOutput(); decoyErr == nil {
		t.Fatalf("the decoy repo received the push: %s", out)
	}
}
