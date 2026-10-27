package node

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

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gitrelay"
)

// TestGitThroughTheRelay (runsheet ADR 076): with the node's git relay on,
// a git command the node runs on an item's token reaches GitHub through
// the relay — GitHub's URL rewritten by insteadOf — and carries the
// token in no environment variable; the relay adds it. A push to the
// node's own branch goes through and one to main is refused.
func TestGitThroughTheRelay(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	execPath, err := exec.Command(gitBin, "--exec-path").Output()
	if err != nil {
		t.Skip(err)
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("no git-http-backend")
	}
	root := t.TempDir()
	plain := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+root,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	served := filepath.Join(root, "served")
	os.MkdirAll(filepath.Join(served, "acme"), 0o755)
	bare := filepath.Join(served, "acme", "app.git")
	plain(root, "init", "-q", "--bare", "-b", "main", bare)
	plain(bare, "config", "http.receivepack", "true")
	seed := filepath.Join(root, "seed")
	plain(root, "init", "-q", "-b", "main", seed)
	os.WriteFile(filepath.Join(seed, "a.txt"), []byte("one\n"), 0o644)
	plain(seed, "add", ".")
	plain(seed, "commit", "-qm", "one")
	plain(seed, "push", "-q", bare, "main")

	const token = "ghs_relaytoken000000000000000000000000"
	var auths []string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		(&cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + served, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=x"}}).ServeHTTP(w, r)
	}))
	defer gh.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := &gitrelay.Server{Upstream: gh.URL}
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	SetGitRelay(srv, t.Logf)
	defer SetGitRelay(nil, nil)

	const base = "https://github.example"
	tctx := withGitToken(ctx, base, &gateway.GitHubToken{Token: token, Repository: "acme/app", Access: "write", ExpiresAt: time.Now().Add(time.Hour)})
	for _, kv := range hardenedGitEnv(tctx, base, "") {
		if strings.Contains(kv, token) {
			t.Fatalf("the token is in the command's environment: %s", kv)
		}
	}
	run := func(dir string, args ...string) (string, error) {
		cmd := hardenedGitCmdEnv(tctx, base, dir, []string{"HOME=" + root,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x"}, args...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	work := filepath.Join(root, "work")
	if out, err := run(root, "clone", "-q", base+"/acme/app.git", work); err != nil {
		t.Fatalf("clone through the relay: %v\n%s", err, out)
	}
	if len(auths) == 0 || auths[0] != "Basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)) {
		t.Fatalf("GitHub saw %q, want the relay's x-access-token", auths)
	}
	os.WriteFile(filepath.Join(work, "b.txt"), []byte("two\n"), 0o644)
	if out, err := run(work, "add", "."); err != nil {
		t.Fatal(out)
	}
	if out, err := run(work, "commit", "-qm", "two"); err != nil {
		t.Fatal(out)
	}
	if out, err := run(work, "push", "-q", "origin", "HEAD:refs/heads/mesh/g-1"); err != nil {
		t.Fatalf("push of the node's branch: %v\n%s", err, out)
	}
	if out, err := run(work, "push", "-q", "origin", "HEAD:main"); err == nil {
		t.Fatalf("a push to main went through:\n%s", out)
	}

	// the finish's write: the branch and its pinned changeset in one push
	if out, err := run(work, "checkout", "-q", "-b", "mesh/g-3"); err != nil {
		t.Fatal(out)
	}
	g := &Git{GitHubURL: base}
	pin := "refs/runsheet/changesets/7f4e0282-e04a-5639-ad32-d58275988f03"
	pushed, err := g.PushPinned(tctx, work, "mesh/g-3", pin)
	if err != nil {
		t.Fatalf("PushPinned through the relay: %v", err)
	}
	for _, ref := range []string{"refs/heads/mesh/g-3", pin} {
		out, err := exec.Command(gitBin, "-C", bare, "rev-parse", ref).Output()
		if err != nil || strings.TrimSpace(string(out)) != pushed {
			t.Fatalf("%s on the backend is %q, want %s (%v)", ref, out, pushed, err)
		}
	}

	// read token: fetch only
	rctx := withGitToken(ctx, base, &gateway.GitHubToken{Token: token + "r", Repository: "acme/app", Access: "read", ExpiresAt: time.Now().Add(time.Hour)})
	cmd := hardenedGitCmdEnv(rctx, base, work, []string{"HOME=" + root}, "push", "-q", "origin", "HEAD:refs/heads/mesh/g-2")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("a read token pushed:\n%s", out)
	}
}

// TestGitRelayOffKeepsTheHelper: with no relay, withGitToken is the
// token's helper by the environment, as before.
func TestGitRelayOffKeepsTheHelper(t *testing.T) {
	SetGitRelay(nil, nil)
	ctx := withGitToken(context.Background(), "https://github.com", &gateway.GitHubToken{Token: "ghs_x", Repository: "acme/app"})
	env := strings.Join(gitTokenEnv(ctx), "\n")
	if !strings.Contains(env, "password=ghs_x") || strings.Contains(env, "insteadOf") {
		t.Fatalf("relay off: %s", env)
	}
}
