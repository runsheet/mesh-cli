package nodev1

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/nodetools"
)

// seedOnBranch commits files on top of the bare repository's from
// (a ref or a commit) and pushes the result to ref, answering the new
// commit's sha. Used to build a validate lease's apply change (and, for
// a conflict, its onto base) directly in the fixture's bare repository,
// never through a session.
func seedOnBranch(t *testing.T, bare, from string, files map[string]string, ref string) string {
	t.Helper()
	tmp := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = tmp
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+tmp,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "work")
	git("fetch", "-q", bare, from)
	git("checkout", "-q", "FETCH_HEAD")
	for path, content := range files {
		full := filepath.Join(tmp, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("commit", "-qm", "seed")
	sha := git("rev-parse", "HEAD")
	git("push", "-q", bare, "HEAD:"+ref)
	return sha
}

const twoLintSuites = "suites:\n  - name: a\n    kind: lint\n    command: \"true\"\n  - name: b\n    kind: lint\n    command: \"true\"\n"

// finishOf unmarshals the lease's one Finish call's result.
func finishOf(t *testing.T, g *rig) map[string]any {
	t.Helper()
	calls := g.fake.Calls("finish")
	if len(calls) != 1 {
		t.Fatalf("finish calls: %d, want 1", len(calls))
	}
	var fr meshapi.FinishRequest
	if err := json.Unmarshal(calls[0].Body, &fr); err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(fr.Result, &out); err != nil {
		t.Fatalf("the finish result: %v: %s", err, fr.Result)
	}
	return out
}

// A validate lease with no harness builds its candidate, pushes it and
// runs its suites itself, with no session at all (Mesh v1 spec, "A
// validate job builds and pushes its chain candidate"): the finish
// carries the candidate apply_onto wrote, both suites' results and the
// checkout's suite_config_sha and environment, and the brief's push_to
// on the fake remote is the candidate's sha.
func TestANoHarnessValidateLeaseBuildsPushesAndRunsItsSuitesWithNoSession(t *testing.T) {
	sess := &stubSession{result: SessionResult{SessionID: "sess-unused"}}
	g := newRig(t, sess)
	g.rt.HostSuites = true
	applySHA := seedOnBranch(t, g.github.bare, g.github.head, map[string]string{".runsheet/validate.yml": twoLintSuites}, "refs/heads/feature/candidate")
	job := g.offer(t, func(b *meshapi.Brief) {
		b.Kind = "conform/validate"
		b.Harness.Name = ""
		b.NodeTools = []string{nodetools.ApplyOntoTool, PushTool, nodetools.RunSuiteTool}
		b.Git.Access = "write"
		b.Git.Push = []meshapi.PushRule{{Ref: "refs/heads/mesh/candidate-1"}}
		b.Code.Onto = &meshapi.Onto{SHA: g.github.head}
		b.Code.Apply = []meshapi.Apply{{SHA: applySHA, Ref: "refs/heads/feature/candidate", Base: g.github.head}}
		b.Code.PushTo = "refs/heads/mesh/candidate-1"
	})
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	lease := g.fake.LeaseOf(job)
	if g.fake.Ended(lease) != "finish" {
		t.Fatalf("lease %s ended %q; hand-backs %+v", lease, g.fake.Ended(lease), g.fake.Calls("hand-back"))
	}
	if len(sess.specs) != 0 {
		t.Fatal("a session ran for a no-harness validate lease")
	}

	finish := finishOf(t, g)
	if finish["verdict"] != "pass" {
		t.Fatalf("verdict %v, want pass: %+v", finish["verdict"], finish)
	}
	candidate, _ := finish["candidate"].(map[string]any)
	sha, _ := candidate["sha"].(string)
	tree, _ := candidate["tree"].(string)
	if sha == "" || tree == "" {
		t.Fatalf("candidate %+v", finish)
	}
	validation, _ := finish["validation"].(map[string]any)
	steps, _ := validation["steps"].([]any)
	if len(steps) != 2 {
		t.Fatalf("validation steps %+v", finish)
	}
	for _, s := range steps {
		step, _ := s.(map[string]any)
		if exit, _ := step["exit"].(float64); exit != 0 {
			t.Fatalf("step %+v did not exit 0", step)
		}
	}
	if finish["suite_config_sha"] == "" || finish["suite_config_sha"] == nil {
		t.Fatalf("no suite_config_sha: %+v", finish)
	}
	if env, _ := finish["environment"].(string); !strings.HasPrefix(env, "host:") {
		t.Fatalf("environment %q, want host:<hostname>", env)
	}
	if got := g.github.ref("refs/heads/mesh/candidate-1"); got != sha {
		t.Fatalf("push_to is %s, want the candidate %s", got, sha)
	}
}

// Acceptance check 2: the same fixture, but the brief's apply conflicts
// with its onto base — both the apply change and the onto commit touch
// README.md differently from their common ancestor. The finish carries
// the conflict apply_onto found; no suite runs and no push lands, since
// there is no candidate to validate or push.
func TestANoHarnessValidateLeaseWithAConflictingApplyFinishesWithTheConflict(t *testing.T) {
	sess := &stubSession{result: SessionResult{SessionID: "sess-unused"}}
	g := newRig(t, sess)
	g.rt.HostSuites = true
	applySHA := seedOnBranch(t, g.github.bare, g.github.head, map[string]string{"README.md": "mine\n"}, "refs/heads/feature/mine")
	ontoSHA := seedOnBranch(t, g.github.bare, g.github.head, map[string]string{"README.md": "theirs\n"}, "refs/heads/main-ahead")
	job := g.offer(t, func(b *meshapi.Brief) {
		b.Kind = "conform/validate"
		b.Harness.Name = ""
		b.NodeTools = []string{nodetools.ApplyOntoTool, PushTool, nodetools.RunSuiteTool}
		b.Git.Access = "write"
		b.Git.Push = []meshapi.PushRule{{Ref: "refs/heads/mesh/candidate-1"}}
		b.Code.Onto = &meshapi.Onto{Ref: "refs/heads/main-ahead", SHA: ontoSHA}
		b.Code.Apply = []meshapi.Apply{{SHA: applySHA, Ref: "refs/heads/feature/mine", Base: g.github.head}}
		b.Code.PushTo = "refs/heads/mesh/candidate-1"
	})
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	lease := g.fake.LeaseOf(job)
	if g.fake.Ended(lease) != "finish" {
		t.Fatalf("lease %s ended %q; hand-backs %+v", lease, g.fake.Ended(lease), g.fake.Calls("hand-back"))
	}
	if len(sess.specs) != 0 {
		t.Fatal("a session ran for a no-harness validate lease")
	}

	finish := finishOf(t, g)
	conflict, ok := finish["conflict"].(map[string]any)
	if !ok {
		t.Fatalf("no conflict in the finish: %+v", finish)
	}
	if _, ok := conflict["index"]; !ok {
		t.Fatalf("conflict has no index: %+v", conflict)
	}
	paths, _ := conflict["paths"].([]any)
	if len(paths) == 0 || paths[0] != "README.md" {
		t.Fatalf("conflict paths %+v, want README.md", conflict["paths"])
	}
	if len(finish) != 1 {
		t.Fatalf("the finish carries more than the conflict: %+v", finish)
	}
	if got := g.github.ref("refs/heads/mesh/candidate-1"); got != "" {
		t.Fatalf("push_to %s, want no push", got)
	}
}
