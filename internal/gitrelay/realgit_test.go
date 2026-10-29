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
		Policy:   Policy{Repo: "acme/app", Access: Write, Push: []RefRule{{Ref: "refs/heads/mesh/g-1"}, {Ref: "refs/pins/*", CreateOnly: true}}},
		Upstream: gh.URL,
		Mint: func(context.Context) (Token, error) {
			return Token{Value: "job-token", ExpiresAt: time.Now().Add(time.Hour)}, nil
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
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:job-token"))
	for _, a := range seenAuth {
		if a != want {
			t.Fatalf("the backend saw %q, want the job token", a)
		}
	}
	if cfg := run(work, "config", "--list"); strings.Contains(cfg, "job-token") {
		t.Fatal("the token reached the checkout's git config")
	}

	run(work, "checkout", "-q", "-b", "mesh/g-1")
	os.WriteFile(filepath.Join(work, "b.txt"), []byte("two\n"), 0o644)
	run(work, "add", ".")
	run(work, "commit", "-qm", "two")
	pin := "refs/pins/7f4e0282-e04a-5639-ad32-d58275988f03"
	run(work, "push", "-q", "origin", "mesh/g-1", "HEAD:"+pin)
	if got := strings.TrimSpace(run(bare, "rev-parse", pin)); got != strings.TrimSpace(run(work, "rev-parse", "HEAD")) {
		t.Fatalf("the create-only ref on the backend is %s", got)
	}
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

// realBackend is git http-backend over a bare acme/app with one commit
// on main, playing GitHub, and a git runner in a HOME of the test's own.
func realBackend(t *testing.T) (run func(dir string, args ...string) (string, error), root, bare, url string) {
	t.Helper()
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
	root = t.TempDir()
	run = func(dir string, args ...string) (string, error) {
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+root, "GIT_TERMINAL_PROMPT=0",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	must := func(dir string, args ...string) {
		if out, err := run(dir, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	served := filepath.Join(root, "served")
	os.MkdirAll(filepath.Join(served, "acme"), 0o755)
	bare = filepath.Join(served, "acme", "app.git")
	must(root, "init", "-q", "--bare", "-b", "main", bare)
	must(bare, "config", "http.receivepack", "true")
	seed := filepath.Join(root, "seed")
	must(root, "init", "-q", "-b", "main", seed)
	os.WriteFile(filepath.Join(seed, "a.txt"), []byte("one\n"), 0o644)
	must(seed, "add", ".")
	must(seed, "commit", "-qm", "one")
	must(seed, "push", "-q", bare, "main", "main:refs/heads/mesh/g-1")
	gh := httptest.NewServer(&cgi.Handler{
		Path: backend,
		Env:  []string{"GIT_PROJECT_ROOT=" + served, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=x-access-token"},
	})
	t.Cleanup(gh.Close)
	return run, root, bare, gh.URL
}

// A lease's grant proves a fast-forward in its checkout with git
// merge-base --is-ancestor: real git's fast-forward passes, its forced
// push of a rewritten commit is refused by the relay before anything
// reaches the vendor, and so is moving a create-only ref and a delete.
func TestGrantRefusesForcePushesWithRealGit(t *testing.T) {
	run, root, bare, url := realBackend(t)
	s := &Server{Upstream: url}
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	m := &minter{ttl: time.Hour}
	g, err := s.Open("lease-1", Policy{Repo: "acme/app", Access: Write, Push: []RefRule{
		{Ref: "refs/heads/mesh/*", FastForwardOnly: true},
		{Ref: "refs/pins/*", CreateOnly: true, FastForwardOnly: true},
	}}, m.mint, nil)
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(root, "work")
	if out, err := run(root, "clone", "-q", g.Base()+"acme/app.git", work); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	g.SetCheckout(work)
	ref := func(name string) string {
		out, _ := run(bare, "rev-parse", "-q", "--verify", name)
		return strings.TrimSpace(out)
	}
	commit := func(msg string) {
		os.WriteFile(filepath.Join(work, "a.txt"), []byte(msg+"\n"), 0o644)
		if out, err := run(work, "commit", "-qam", msg); err != nil {
			t.Fatalf("commit: %v\n%s", err, out)
		}
	}

	run(work, "checkout", "-q", "-b", "mesh/g-1", "origin/mesh/g-1")
	commit("two")
	if out, err := run(work, "push", "-q", "origin", "HEAD:refs/heads/mesh/g-1", "HEAD:refs/pins/p1"); err != nil {
		t.Fatalf("a fast-forward and a new pin: %v\n%s", err, out)
	}
	two := ref("refs/heads/mesh/g-1")

	run(work, "reset", "-q", "--hard", "HEAD~1")
	commit("rewritten")
	for name, args := range map[string][]string{
		"a forced push":           {"push", "-q", "--force", "origin", "HEAD:refs/heads/mesh/g-1"},
		"a create-only ref moved": {"push", "-q", "--force", "origin", "HEAD:refs/pins/p1"},
		"a delete":                {"push", "-q", "origin", ":refs/heads/mesh/g-1"},
	} {
		if out, err := run(work, args...); err == nil {
			t.Fatalf("%s went through:\n%s", name, out)
		}
	}
	if ref("refs/heads/mesh/g-1") != two || ref("refs/pins/p1") != two {
		t.Fatal("a refused push moved a ref")
	}
}
