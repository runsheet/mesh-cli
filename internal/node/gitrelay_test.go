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

// TestGitThroughTheRelay: with the node's git relay on, a git command the
// node runs on a read token reaches GitHub through the relay — GitHub's
// URL rewritten by insteadOf — and carries the token in no environment
// variable; the relay adds it, and refuses a push. A write token is not
// relayed until a claim names its refs.
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
	tctx := withGitToken(ctx, base, &gateway.GitHubToken{Token: token, Repository: "acme/app", Access: "read", ExpiresAt: time.Now().Add(time.Hour)})
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
	if out, err := run(work, "fetch", "-q", "origin"); err != nil {
		t.Fatalf("fetch through the relay: %v\n%s", err, out)
	}
	os.WriteFile(filepath.Join(work, "b.txt"), []byte("two\n"), 0o644)
	if out, err := run(work, "add", "."); err != nil {
		t.Fatal(out)
	}
	if out, err := run(work, "commit", "-qm", "two"); err != nil {
		t.Fatal(out)
	}
	if out, err := run(work, "push", "-q", "origin", "HEAD:refs/heads/mesh/g-1"); err == nil {
		t.Fatalf("a read token pushed:\n%s", out)
	}

	// a write token is not relayed until a claim names the refs it may
	// push: its command carries its helper, as before
	wctx := withGitToken(ctx, base, &gateway.GitHubToken{Token: token + "w", Repository: "acme/app", Access: "write", ExpiresAt: time.Now().Add(time.Hour)})
	env := strings.Join(gitTokenEnv(wctx), "\n")
	if strings.Contains(env, "insteadOf") || !strings.Contains(env, "password="+token+"w") {
		t.Fatalf("a write token was relayed: %s", env)
	}

	// with the claim's push rules it is relayed, held to them: the
	// branch and the create-only pin push, a pin moved or any other ref
	// is refused (runsheet ADR 076, option (c))
	const pin = "refs/runsheet/changesets/0f0e0d0c-0b0a-4908-8706-050403020100"
	rctx := WithPushRules(ctx, []gateway.JobPushRef{{Ref: "refs/heads/mesh/g-1"}, {Ref: pin, CreateOnly: true}})
	wtok := &gateway.GitHubToken{Token: token + "w", Repository: "acme/app", Access: "write", ExpiresAt: time.Now().Add(time.Hour)}
	pctx := withGitToken(rctx, base, wtok)
	if env := strings.Join(gitTokenEnv(pctx), "\n"); !strings.Contains(env, "insteadOf") || strings.Contains(env, token) {
		t.Fatalf("a write token with push rules is not relayed: %s", env)
	}
	push := func(args ...string) (string, error) {
		cmd := hardenedGitCmdEnv(pctx, base, work, []string{"HOME=" + root}, args...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := push("push", "-q", "origin", "HEAD:refs/heads/mesh/g-1"); err != nil {
		t.Fatalf("the branch through the relay: %v\n%s", err, out)
	}
	if out, err := push("push", "-q", "origin", "HEAD:"+pin); err != nil {
		t.Fatalf("the pin through the relay: %v\n%s", err, out)
	}
	if out, err := push("push", "-q", "origin", "HEAD:refs/heads/main"); err == nil {
		t.Fatalf("a ref the claim did not grant was pushed:\n%s", out)
	}
	os.WriteFile(filepath.Join(work, "c.txt"), []byte("three\n"), 0o644)
	run(work, "add", ".")
	run(work, "commit", "-qm", "three")
	if out, err := push("push", "-q", "-f", "origin", "HEAD:"+pin); err == nil {
		t.Fatalf("a pin was moved:\n%s", out)
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
