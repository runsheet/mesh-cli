package nodetools

import (
	"context"
	"encoding/json"
	"os"
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
