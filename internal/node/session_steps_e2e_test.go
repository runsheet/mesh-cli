//go:build e2e

package node

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

const stepsItemToken = "ghs_stepsE2EItemToken0123456789"

// A build runs as the session's ordered steps (ADR-038): against a
// stand-in GitHub that answers only the item's ghs_ token and a fake
// gateway that holds changesets, the node's own Git and the Claude Code
// harness in a pod on sandbox.Fake build item 1. The logged records are
// checkout, exchange, pod, run, validate, finish and release, each
// succeeded; the job container's env and every file it mounts hold
// neither ghs_ nor the session's bearer; the finish recorded a changeset
// whose tree is the agent's commit's and finished the item naming it;
// and nothing was pushed — the branch is the platform's to make.
func TestBuildRunsAsSessionStepsE2E(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, kv := range []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_TERMINAL_PROMPT=0"} {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}

	// GitHub: acme/widgets with main, reached on the item's token alone
	srvRoot := filepath.Join(root, "gh")
	origin := filepath.Join(srvRoot, "acme", "widgets.git")
	seedDir := filepath.Join(root, "seed")
	gitRun(t, root, "init", "--quiet", "--bare", "-b", "main", origin)
	gitRun(t, root, "init", "--quiet", "-b", "main", seedDir)
	writeFiles(t, seedDir, map[string]string{"api/api.go": "package api\n"})
	gitRun(t, seedDir, "add", "-A")
	gitRun(t, seedDir, "commit", "--quiet", "-m", "main")
	gitRun(t, seedDir, "push", "--quiet", origin, "HEAD:main")
	github := gatewaytest.GitHub(t, srvRoot, map[string]string{stepsItemToken: "acme/widgets"})

	f := seed(t)
	f.Changesets = true
	f.GitHubTokens[item1] = map[string]any{"token": stepsItemToken, "expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "repository": "acme/widgets"}

	sb := &sandbox.Sandbox{Runtime: &sandbox.Fake{}, Config: SandboxConfig{}.Settings("v1"), TempDir: t.TempDir(), Logf: func(string, ...any) {}}
	fake := sb.Runtime.(*sandbox.Fake)
	h := &harness.ClaudeCode{Sandbox: sb}
	r, item, logs := toolRunner(t, f, h)
	r.Sandbox = sb
	r.Log = log.New(io.MultiWriter(logs), "", 0)
	r.WorkRoot = filepath.Join(root, "work")
	r.WS = &Git{GitHubURL: github.URL, ReposRoot: filepath.Join(root, "repos"), Logf: r.Log.Printf, Log: io.Discard}
	tf := &TokenFiles{Token: func(context.Context) (string, error) { return proxyMachineBearer, nil }}
	unbind := tf.Bind(item.ID, item.Attempts, func(context.Context) (string, error) { return proxySessionBearer, nil }, nil)
	defer unbind()
	r.Proxies = &SessionProxies{Bearer: tf.Source, Root: shortProxyRoot(t), Logf: t.Logf}

	var env []string
	files := map[string]string{}
	var workspace string
	fake.Agent = func(_ context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		if spec.Stdin != nil {
			_, _ = io.Copy(io.Discard, spec.Stdin)
		}
		env = spec.Env
		for _, m := range spec.Mounts {
			_ = filepath.WalkDir(m.Source, func(p string, d fs.DirEntry, err error) error {
				if err != nil || !d.Type().IsRegular() {
					return nil
				}
				b, _ := os.ReadFile(p)
				files[p] = string(b)
				return nil
			})
		}
		// the agent's work: a commit in the workspace, as the builder
		// makes it inside the pod
		workspace = spec.Workdir
		if workspace == "" {
			workspace = filepath.Join(r.WorkRoot, "ship-it", "1")
		}
		writeFiles(t, workspace, map[string]string{"api/steps.go": "package api\n\nconst Steps = 7\n"})
		gitRun(t, workspace, "add", "api/steps.go")
		gitRun(t, workspace, "commit", "--quiet", "-m", "api: steps")
		fmt.Fprintln(spec.Stdout, `{"type":"system","subtype":"init","session_id":"s-1"}`)
		fmt.Fprintln(spec.Stdout, `{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s-1","total_cost_usd":0.5}`)
		return sandbox.Exit{}, nil
	}

	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatalf("RunItem: %v\n%s", err, logs.String())
	}
	out := logs.String()
	if env == nil {
		t.Fatalf("the agent never ran: %v\n%s", fake.Calls, out)
	}

	// the steps, in order, each succeeded
	for i, name := range []string{"checkout", "exchange", "pod", "run", "validate", "finish", "release"} {
		if want := fmt.Sprintf("item ship-it/1: step %d %s: succeeded", i+1, name); !strings.Contains(out, want) {
			t.Errorf("node.log lacks %q", want)
		}
	}

	// the job container holds no credential: not in its env, not in a
	// file it mounts (the workspace and its git dirs among them)
	if len(files) == 0 {
		t.Fatal("the agent's mounts held no files")
	}
	for _, s := range []string{"ghs_", stepsItemToken, proxySessionBearer, proxyMachineBearer} {
		for _, kv := range env {
			if strings.Contains(kv, s) {
				t.Errorf("job container env %q holds %q", kv, s)
			}
		}
		for p, body := range files {
			if strings.Contains(body, s) {
				t.Errorf("mounted file %s holds %q", p, s)
			}
		}
	}
	for _, kv := range env {
		if name, _, _ := strings.Cut(kv, "="); name == "GH_TOKEN" || name == "GH_CONFIG_DIR" {
			t.Errorf("job container env carries %s", name)
		}
	}

	// the finish recorded a changeset and finished the item naming it
	css := f.ChangesetsOf(item1)
	if len(css) != 1 {
		t.Fatalf("changesets recorded = %+v, want one\n%s", css, out)
	}
	cs := css[0]
	dir := filepath.Join(r.WorkRoot, "ship-it", "1")
	if tree := gitRun(t, dir, "rev-parse", "HEAD^{tree}"); cs.Tree != tree {
		t.Errorf("changeset tree %s, the agent's commit's %s", cs.Tree, tree)
	}
	if strings.Join(cs.PathsChanged, ",") != "api/steps.go" {
		t.Errorf("paths changed = %v", cs.PathsChanged)
	}
	finish := f.CallsNamed("finishWorkItem")
	if len(finish) != 1 || finish[0].Input[gateway.ChangesetIDField] != cs.ChangesetID || finish[0].Input["headSha"] != nil {
		t.Fatalf("finishes = %+v, want one naming changeset %s and no head\n%s", finish, cs.ChangesetID, out)
	}
	if got := f.Item(item1); got["latestChangesetId"] != cs.ChangesetID || got["status"] != "evaluating" {
		t.Fatalf("item after the finish = %v", got)
	}
	// nothing pushed: the platform makes the branch from the changeset
	if heads := gitRun(t, root, "ls-remote", "--heads", origin, BranchName("ship-it", 1)); heads != "" {
		t.Fatalf("the node pushed %s", heads)
	}
	if !strings.Contains(out, "changeset "+cs.ChangesetID+" recorded") {
		t.Errorf("node.log lacks the changeset line:\n%s", out)
	}
	t.Logf("node.log:\n%s", out)
}
