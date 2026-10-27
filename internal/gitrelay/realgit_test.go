package gitrelay

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRelayWithRealGit clones, fetches and pushes with the git binary
// through the relay to git http-backend, which plays GitHub: the token
// reaches the backend, the job's own ref is pushed, and main is refused.
func TestRelayWithRealGit(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	execPath, err := exec.Command(gitBin, "--exec-path").Output()
	if err != nil {
		t.Skip("git --exec-path: " + err.Error())
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("no git-http-backend")
	}
	root := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+root, "GIT_TERMINAL_PROMPT=0",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	// the "GitHub" side: a bare repo with one commit on main
	served := filepath.Join(root, "served")
	os.MkdirAll(filepath.Join(served, "acme"), 0o755)
	bare := filepath.Join(served, "acme", "app.git")
	run(root, "init", "-q", "--bare", "-b", "main", bare)
	run(bare, "config", "http.receivepack", "true")
	seed := filepath.Join(root, "seed")
	run(root, "init", "-q", "-b", "main", seed)
	os.WriteFile(filepath.Join(seed, "a.txt"), []byte("one\n"), 0o644)
	run(seed, "add", ".")
	run(seed, "commit", "-qm", "one")
	run(seed, "push", "-q", bare, "main")

	var seenAuth []string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = append(seenAuth, r.Header.Get("Authorization"))
		h := &cgi.Handler{
			Path: backend,
			Env:  []string{"GIT_PROJECT_ROOT=" + served, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=x-access-token"},
		}
		h.ServeHTTP(w, r)
	}))
	defer gh.Close()

	var events []Event
	relay := &Relay{
		Policy:   Policy{Repo: "acme/app", Access: Write, PushRef: "refs/heads/mesh/g-1"},
		Upstream: gh.URL,
		Mint: func(context.Context) (Token, error) {
			return Token{Value: "item-token", ExpiresAt: time.Now().Add(time.Hour)}, nil
		},
		Audit: func(e Event) { events = append(events, e) },
	}
	front := httptest.NewServer(relay)
	defer front.Close()

	work := filepath.Join(root, "work")
	run(root, "clone", "-q", front.URL+"/git/acme/app.git", work)
	if b, _ := os.ReadFile(filepath.Join(work, "a.txt")); string(b) != "one\n" {
		t.Fatalf("clone through the relay read %q", b)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:item-token"))
	for _, a := range seenAuth {
		if a != want {
			t.Fatalf("the backend saw %q, want the item token", a)
		}
	}
	if cfg := run(work, "config", "--list"); strings.Contains(cfg, "item-token") {
		t.Fatal("the token reached the checkout's git config")
	}

	run(work, "checkout", "-q", "-b", "mesh/g-1")
	os.WriteFile(filepath.Join(work, "b.txt"), []byte("two\n"), 0o644)
	run(work, "add", ".")
	run(work, "commit", "-qm", "two")
	run(work, "push", "-q", "origin", "mesh/g-1")
	if got := strings.TrimSpace(run(bare, "rev-parse", "refs/heads/mesh/g-1")); got != strings.TrimSpace(run(work, "rev-parse", "HEAD")) {
		t.Fatalf("the job's branch on the backend is %s", got)
	}

	// pushing main is refused by the relay, and main does not move
	before := strings.TrimSpace(run(bare, "rev-parse", "main"))
	cmd := exec.Command(gitBin, "push", "origin", "HEAD:main")
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+root, "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("a push to main went through:\n%s", out)
	}
	if after := strings.TrimSpace(run(bare, "rev-parse", "main")); after != before {
		t.Fatal("main moved")
	}
	refused := false
	for _, e := range events {
		if e.Service == "receive-pack" && e.Outcome == "refused" && len(e.Refs) == 1 && e.Refs[0] == "refs/heads/main" {
			refused = true
		}
	}
	if !refused {
		t.Fatalf("no refused audit event for main: %+v", events)
	}
}
