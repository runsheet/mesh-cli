package session

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/sandbox"
)

const (
	itemToken     = "ghs_itemTokenForTheJobContainer0001"
	sessionBearer = "rs_sessionBearerForTheJobContainer0001"
)

// gitWorktree makes a shared clone and one item worktree of it, as the
// node's workspace does (ADR-028), and returns the worktree.
func gitWorktree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	clone := filepath.Join(root, "repos", "acme", "widgets")
	wt := filepath.Join(root, "work", "goal", "4")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	git(clone, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(clone, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(clone, "add", ".")
	git(clone, "commit", "-q", "-m", "init")
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		t.Fatal(err)
	}
	git(clone, "worktree", "add", "-q", "-b", "mesh/goal-4", wt)
	return wt
}

// noSecretUnder fails when any regular file under dir holds the item's
// token, the session's bearer, or anything shaped like an installation
// token.
func noSecretUnder(t *testing.T, what, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, s := range []string{itemToken, sessionBearer, "ghs_"} {
			if strings.Contains(string(b), s) {
				t.Errorf("%s: %s holds %q", what, p, s)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("%s: walk %s: %v", what, dir, err)
	}
}

// The job container holds no credential: given a session whose node-side
// environment carries the item's ghs_ token and gh config dir, and whose
// MCP bearer sits in a token file on the node, the pod step's container
// — its env, every mount's files and the rendered HOME — holds neither
// the token, the bearer nor `ghs_`. The model account's key and the tool
// secrets still reach it (out of this goal's scope).
func TestJobContainerHoldsNoCredential(t *testing.T) {
	// the node's own side: a gh config dir with the token in it (as the
	// operator's would hold one) and the session's bearer in its token
	// file, both outside anything the pod mounts
	nodeSide := t.TempDir()
	ghDir := filepath.Join(nodeSide, "gh")
	if err := os.MkdirAll(ghDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ghDir, "hosts.yml"), []byte("github.com:\n    oauth_token: "+itemToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(nodeSide, "runsheet.token")
	if err := os.WriteFile(tokenFile, []byte(sessionBearer), 0o600); err != nil {
		t.Fatal(err)
	}

	stores := t.TempDir()
	f := &sandbox.Fake{}
	sb := &sandbox.Sandbox{
		Runtime: f,
		Config: sandbox.Config{
			Image:  "ghcr.io/runsheet/mesh-node:test",
			Stores: sandbox.Stores{Pnpm: filepath.Join(stores, "pnpm"), Npm: filepath.Join(stores, "npm")},
		},
		TempDir: t.TempDir(),
		Logf:    t.Logf,
	}
	ws := gitWorktree(t)

	env := []string{
		"GH_TOKEN=" + itemToken, "GH_CONFIG_DIR=" + ghDir, "GITHUB_TOKEN=" + itemToken,
		"MESH_TOKEN_FILE=" + tokenFile, "MESH_TOKEN=" + sessionBearer,
		"CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat-account", "MESH_ACCOUNT=team",
		"MESH_TOOL_LINEAR_API_KEY=lin-secret",
		"GIT_AUTHOR_NAME=Mesh builder (node)", "GIT_AUTHOR_EMAIL=mesh-builder+node@runsheet.dev",
	}
	job := Job{Steps: []Step{
		{Name: "checkout", Run: func(ctx context.Context, s *State) error { s.WorkspaceDir = ws; return nil }},
		PodStep(PodConfig{
			Sandbox: sb, Label: "goal/4", Kind: "build", Env: env,
			Git: sandbox.GitIdentity{Name: "Mesh builder (node)", Email: "mesh-builder+node@runsheet.dev"},
		}),
		{Name: "run", Run: func(ctx context.Context, s *State) error {
			c := s.Pod.(*Container)
			// while the pod stands: its HOME and every mount hold no secret
			noSecretUnder(t, "home", c.Session.Home)
			mounts, err := c.Session.CheckedMounts(c.Workspace)
			if err != nil {
				return err
			}
			for _, m := range mounts {
				if m.Source == tokenFile || m.Source == nodeSide || m.Source == ghDir {
					t.Errorf("the pod mounts the node's %s", m.Source)
				}
				noSecretUnder(t, "mount "+m.Target, m.Source)
			}
			return c.Exec(ctx, []string{"claude", "-p"}, 0, nil, nil)
		}},
		PodCloseStep(),
	}}
	state := &State{}
	for _, r := range Run(context.Background(), job, state) {
		if r.Outcome != Succeeded {
			t.Fatalf("step %s: %s: %v", r.Name, r.Outcome, r.Err)
		}
	}

	if len(f.Pods) != 1 {
		t.Fatalf("pods: %+v", f.Pods)
	}
	if len(f.Agents) != 1 {
		t.Fatalf("agents: %+v", f.Agents)
	}
	spec := f.Agents[0]
	got := map[string]string{}
	for _, kv := range spec.Env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
		for _, s := range []string{itemToken, sessionBearer, "ghs_", tokenFile, ghDir} {
			if strings.Contains(kv, s) {
				t.Errorf("the pod's env carries %q: %s", s, kv)
			}
		}
	}
	for _, name := range sandbox.HeldOutEnv {
		if _, ok := got[name]; ok {
			t.Errorf("the pod's env names %s: %v", name, spec.Env)
		}
	}
	for _, name := range []string{"MESH_TOKEN", "MESH_TOKEN_FILE"} {
		if _, ok := got[name]; ok {
			t.Errorf("the pod's env names %s: %v", name, spec.Env)
		}
	}
	// what this goal leaves in place: the account's key, the tool
	// secrets and the agent's identity
	if got["CLAUDE_CODE_OAUTH_TOKEN"] != "sk-ant-oat-account" || got["MESH_TOOL_LINEAR_API_KEY"] != "lin-secret" || got["GIT_AUTHOR_NAME"] == "" {
		t.Errorf("the pod's env lost what it keeps: %v", spec.Env)
	}
	for _, m := range spec.Mounts {
		if m.Source == tokenFile || m.Source == nodeSide || m.Source == ghDir {
			t.Errorf("the agent's container mounts the node's %s", m.Source)
		}
	}
	for _, s := range []string{itemToken, sessionBearer} {
		for _, a := range append(append([]string{}, spec.Command...), spec.Workdir) {
			if strings.Contains(a, s) {
				t.Errorf("the agent's command carries the secret: %v", spec.Command)
			}
		}
	}
	if state.Pod != nil {
		t.Errorf("pod-close left the container in state: %v", state.Pod)
	}
}

// JobEnv takes every GitHub credential out and keeps the rest in order.
func TestJobEnvHoldsOutGitHub(t *testing.T) {
	got := JobEnv([]string{"GH_TOKEN=ghs_x", "A=1", "GH_CONFIG_DIR=/x", "GITHUB_TOKEN=ghs_y", "B=2"})
	if strings.Join(got, " ") != "A=1 B=2" {
		t.Fatalf("JobEnv = %v", got)
	}
}

// A pod step with no sandbox fails; nothing is opened.
func TestPodStepNeedsASandbox(t *testing.T) {
	recs := Run(context.Background(), Job{Steps: []Step{PodStep(PodConfig{}), PodCloseStep()}}, &State{WorkspaceDir: t.TempDir()})
	if recs[0].Outcome != Failed || recs[0].Err != ErrNoSandbox || recs[1].Outcome != Succeeded {
		t.Fatalf("records: %+v", recs)
	}
}
