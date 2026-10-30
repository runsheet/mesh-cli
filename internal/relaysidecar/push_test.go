package relaysidecar

import (
	"context"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gitrelay"
)

// One enforcement point for every push: a push the job's push rules
// refuse is refused in the same words whether the session's own git
// pushes it through the sidecar's /git/ route or the node's push tool
// asks the sidecar for it on the control socket; a push the rules allow
// reaches the vendor either way. The vendor is git http-backend, reached
// only through the node's tunnel.
func TestAPushIsRefusedTheSameWhoeverAsks(t *testing.T) {
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
	env := append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+root, "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
	run := func(dir string, args ...string) (string, error) {
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	must := func(dir string, args ...string) string {
		t.Helper()
		out, err := run(dir, args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(out)
	}
	served := filepath.Join(root, "served")
	bare := filepath.Join(served, "acme", "app.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	must(root, "init", "-q", "--bare", "-b", "main", bare)
	must(bare, "config", "http.receivepack", "true")
	checkout := filepath.Join(root, "work")
	must(root, "init", "-q", "-b", "main", checkout)
	if err := os.WriteFile(filepath.Join(checkout, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	must(checkout, "add", ".")
	must(checkout, "commit", "-qm", "one")
	must(checkout, "push", "-q", bare, "main")
	if err := os.WriteFile(filepath.Join(checkout, "a.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	must(checkout, "commit", "-qam", "two")
	sha := must(checkout, "rev-parse", "HEAD")

	vendor := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(&cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + served, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=x-access-token"}}).ServeHTTP(w, r)
	})
	c := testCreds(t)
	c.Git = &Git{
		Policy:   gitrelay.Policy{Repo: "acme/app", Access: gitrelay.Write, Push: []gitrelay.RefRule{{Ref: "refs/heads/mesh/g-1", FastForwardOnly: true}}},
		Upstream: "https://git.example.com",
		Token:    Token{Value: gitSecret, ExpiresAt: time.Now().Add(time.Hour)},
		Checkout: checkout,
	}
	g := newRig(t, c, map[string]http.Handler{"git.example.com:443": vendor})
	tool := PushClient{Socket: filepath.Join(g.dir, "r")}
	ctx := context.Background()

	// the session's own git push of main, which no rule names
	if out, err := run(checkout, "push", g.base+"/git/acme/app.git", "HEAD:refs/heads/main"); err == nil {
		t.Fatalf("the session pushed main: %s", out)
	}
	// the push tool's push of main
	res, err := tool.Push(ctx, "refs/heads/main", sha)
	if err != nil {
		t.Fatal(err)
	}
	if res.Pushed || res.Refused == "" {
		t.Fatalf("the push tool's push of main: %+v, want refused", res)
	}
	var refusals []string
	for _, rec := range g.records() {
		if rec.Kind == "git" && rec.Service == "receive-pack" && rec.Outcome == "refused" {
			refusals = append(refusals, rec.Reason)
		}
	}
	if len(refusals) != 2 || refusals[0] != refusals[1] || refusals[1] != res.Refused {
		t.Fatalf("refusals %q and the tool's %q: want two, the same words", refusals, res.Refused)
	}
	if strings.Contains(res.Refused, gitSecret) {
		t.Fatal("a refusal named the git token")
	}

	// the job's own ref goes through either way
	res, err = tool.Push(ctx, "refs/heads/mesh/g-1", sha)
	if err != nil || !res.Pushed {
		t.Fatalf("the push tool's push of the job's ref: %+v, %v", res, err)
	}
	if got := must(bare, "rev-parse", "refs/heads/mesh/g-1"); got != sha {
		t.Fatalf("the vendor has mesh/g-1 at %s, want %s", got, sha)
	}
	// and a forced non-fast-forward of it is refused by the sidecar's
	// proof in the checkout; the tool never forces, so git refuses it
	// before the relay is asked
	first := must(checkout, "rev-parse", "HEAD~1")
	if out, err := run(checkout, "push", "--force", g.base+"/git/acme/app.git", first+":refs/heads/mesh/g-1"); err == nil {
		t.Fatalf("the session force-pushed: %s", out)
	}
	recs := g.records()
	last := recs[len(recs)-1]
	if last.Outcome != "refused" || !strings.Contains(last.Reason, "force push") {
		t.Fatalf("the session's force push was recorded %+v, want refused as a force push", last)
	}
	res, err = tool.Push(ctx, "refs/heads/mesh/g-1", first)
	if err != nil {
		t.Fatal(err)
	}
	if res.Pushed || res.Refused != "" && !strings.Contains(res.Refused, "force push") {
		t.Fatalf("the push tool's non-fast-forward: %+v", res)
	}
	if got := must(bare, "rev-parse", "refs/heads/mesh/g-1"); got != sha {
		t.Fatalf("the vendor's mesh/g-1 moved to %s", got)
	}
}
