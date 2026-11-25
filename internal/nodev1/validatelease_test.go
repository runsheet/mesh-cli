package nodev1

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/nodetools"
	"projectmesh.io/mesh-cli/internal/validate"
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

// treeOf is commit's tree in the bare repository.
func treeOf(t *testing.T, bare, commit string) string {
	t.Helper()
	out, err := exec.Command("git", "--git-dir", bare, "rev-parse", commit+"^{tree}").CombinedOutput()
	if err != nil {
		t.Fatalf("the tree of %s: %v: %s", commit, err, out)
	}
	return strings.TrimSpace(string(out))
}

const twoGreenSuites = "suites:\n  - name: a\n    kind: lint\n    command: \"true\"\n  - name: b\n    kind: lint\n    command: \"true\"\n"

// oneRedSuite's suite b reports go test's events for one passing and
// one failing test and exits 1.
const oneRedSuite = "suites:\n  - name: a\n    kind: lint\n    command: \"true\"\n" +
	"  - name: b\n    kind: unit\n    results: stdout\n    command: \"cat events.json; exit 1\"\n"

const redEvents = `{"Action":"run","Package":"example.com/pkg","Test":"TestGreen"}
{"Action":"pass","Package":"example.com/pkg","Test":"TestGreen","Elapsed":0.01}
{"Action":"run","Package":"example.com/pkg","Test":"TestRed"}
{"Action":"fail","Package":"example.com/pkg","Test":"TestRed","Elapsed":0.02}
{"Action":"fail","Package":"example.com/pkg"}
`

// suiteThatMovesTheTree commits a file of its own in the checkout: the
// tree the finish would name is no longer the one the suites ran on.
const suiteThatMovesTheTree = "suites:\n  - name: a\n    kind: lint\n    command: \"echo moved > moved.txt && git add moved.txt && git -c user.name=t -c user.email=t@x commit -qm moved\"\n"

// finishOf unmarshals the lease's one Finish call's result.
func finishOf(t *testing.T, g *rig) map[string]any {
	t.Helper()
	calls := g.fake.Calls("finish")
	if len(calls) != 1 {
		t.Fatalf("finish calls: %d, want 1; hand-backs %+v; fails %+v", len(calls), g.fake.Calls("hand-back"), g.fake.Calls("fail"))
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

// chainValidate is a chain's validate lease as runsheet offers one
// today: a harness named (the kind is claude-code's), the three node
// tools, the candidate's onto and apply, and its push_to.
func chainValidate(g *rig, onto meshapi.Onto, apply meshapi.Apply) func(*meshapi.Brief) {
	return func(b *meshapi.Brief) {
		b.Kind = "conform/validate"
		b.Subject = meshapi.Subject{Type: "candidate", ID: "batch-1"}
		b.NodeTools = []string{nodetools.ApplyOntoTool, PushTool, nodetools.RunSuiteTool, "progress", "hand_back"}
		b.Git.Access = "write"
		b.Git.Push = []meshapi.PushRule{{Ref: "refs/heads/mesh/batch/candidate-1"}}
		b.Code.Onto = &onto
		b.Code.Apply = []meshapi.Apply{apply}
		b.Code.PushTo = "refs/heads/mesh/batch/candidate-1"
	}
}

// runChain offers a chain validate lease whose change adds files onto
// the fixture's head, runs the node until it ends, and answers the
// lease's id.
func runChain(t *testing.T, g *rig, files map[string]string) string {
	t.Helper()
	g.rt.HostSuites = true
	applySHA := seedOnBranch(t, g.github.bare, g.github.head, files, "refs/heads/feature/candidate")
	job := g.offer(t, chainValidate(g, meshapi.Onto{SHA: g.github.head},
		meshapi.Apply{SHA: applySHA, Ref: "refs/heads/feature/candidate", Base: g.github.head}))
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	return g.fake.LeaseOf(job)
}

// A chain's validation runs with no model session (ADR-044 amendment
// 2026-11-25): the node builds the candidate, pushes it to push_to, runs
// both suites and finishes with a result it wrote itself — green, the
// candidate's sha and tree, each suite's exit, suite_config_sha and
// environment. The kind still names its harness, as runsheet's validate
// kind does today; no session starts all the same.
func TestTheNodeValidatesAChainWithNoSession(t *testing.T) {
	sess := &stubSession{result: SessionResult{SessionID: "sess-unused"}}
	g := newRig(t, sess)
	lease := runChain(t, g, map[string]string{".runsheet/validate.yml": twoGreenSuites})
	if g.fake.Ended(lease) != "finish" {
		t.Fatalf("lease %s ended %q; hand-backs %+v; fails %+v", lease, g.fake.Ended(lease), g.fake.Calls("hand-back"), g.fake.Calls("fail"))
	}
	if len(sess.specs) != 0 {
		t.Fatalf("a session ran for a validate lease: %+v", sess.specs)
	}

	finish := finishOf(t, g)
	if finish["green"] != true {
		t.Fatalf("green %v: %+v", finish["green"], finish)
	}
	candidate, _ := finish["candidate"].(map[string]any)
	sha, _ := candidate["sha"].(string)
	tree, _ := candidate["tree"].(string)
	pushed := g.github.ref("refs/heads/mesh/batch/candidate-1")
	if sha == "" || sha != pushed {
		t.Fatalf("candidate %+v, push_to holds %q", candidate, pushed)
	}
	if want := treeOf(t, g.github.bare, sha); tree != want {
		t.Fatalf("candidate tree %q, the commit's tree is %q", tree, want)
	}
	suites, _ := finish["suites"].([]any)
	var names []string
	for _, s := range suites {
		st, _ := s.(map[string]any)
		names = append(names, st["name"].(string))
		if exit, _ := st["exit"].(float64); exit != 0 {
			t.Fatalf("suite %+v did not exit 0", st)
		}
	}
	if !slices.Equal(names, []string{"a", "b"}) {
		t.Fatalf("suites %v, want a and b", names)
	}
	want, err := validate.Parse([]byte(twoGreenSuites))
	if err != nil {
		t.Fatal(err)
	}
	if finish["suite_config_sha"] != want.Hash() {
		t.Fatalf("suite_config_sha %v, want validate.yml's hash %s", finish["suite_config_sha"], want.Hash())
	}
	env, _ := finish["environment"].(map[string]any)
	if env["release"] != "v0.0.0-test" || !strings.HasPrefix(env["image"].(string), "host:") {
		t.Fatalf("environment %+v", env)
	}
	evidence, _ := finish["evidence"].([]any)
	if len(evidence) < 3 {
		t.Fatalf("evidence %v: want apply_onto's, push's and run_suite's records", evidence)
	}
	if _, ok := finish["triage"]; ok {
		t.Fatalf("a green carries a triage: %+v", finish)
	}
}

// The same chain, but its change conflicts with the base it is applied
// onto: the finish carries the conflict, and nothing is pushed or run.
func TestTheNodeFinishesAConflictingChainWithTheConflict(t *testing.T) {
	sess := &stubSession{result: SessionResult{SessionID: "sess-unused"}}
	g := newRig(t, sess)
	g.rt.HostSuites = true
	applySHA := seedOnBranch(t, g.github.bare, g.github.head, map[string]string{"README.md": "mine\n"}, "refs/heads/feature/mine")
	ontoSHA := seedOnBranch(t, g.github.bare, g.github.head, map[string]string{"README.md": "theirs\n"}, "refs/heads/main-ahead")
	job := g.offer(t, chainValidate(g, meshapi.Onto{Ref: "refs/heads/main-ahead", SHA: ontoSHA},
		meshapi.Apply{SHA: applySHA, Ref: "refs/heads/feature/mine", Base: g.github.head}))
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	lease := g.fake.LeaseOf(job)
	if g.fake.Ended(lease) != "finish" {
		t.Fatalf("lease %s ended %q", lease, g.fake.Ended(lease))
	}
	if len(sess.specs) != 0 {
		t.Fatal("a session ran for a validate lease")
	}
	finish := finishOf(t, g)
	conflict, ok := finish["conflict"].(map[string]any)
	if !ok {
		t.Fatalf("no conflict in the finish: %+v", finish)
	}
	if idx, ok := conflict["index"].(float64); !ok || idx != 0 {
		t.Fatalf("conflict index %+v", conflict)
	}
	if paths, _ := conflict["paths"].([]any); len(paths) != 1 || paths[0] != "README.md" {
		t.Fatalf("conflict paths %+v, want README.md", conflict["paths"])
	}
	for _, k := range []string{"suites", "candidate", "tests"} {
		if _, ok := finish[k]; ok {
			t.Fatalf("a conflict's finish carries %s: %+v", k, finish)
		}
	}
	if got := g.github.ref("refs/heads/mesh/batch/candidate-1"); got != "" {
		t.Fatalf("push_to %s, want no push", got)
	}
}

// A red suite finishes red, with no model: the candidate as built and
// pushed, each suite as it ran, the failing test named in the triage and
// in tests.
func TestTheNodeFinishesARedSuiteRedWithItsFailingTests(t *testing.T) {
	sess := &stubSession{result: SessionResult{SessionID: "sess-unused"}}
	g := newRig(t, sess)
	g.rt.ValidateWithSession = false
	lease := runChain(t, g, map[string]string{".runsheet/validate.yml": oneRedSuite, "events.json": redEvents})
	if g.fake.Ended(lease) != "finish" {
		t.Fatalf("lease %s ended %q; hand-backs %+v; fails %+v", lease, g.fake.Ended(lease), g.fake.Calls("hand-back"), g.fake.Calls("fail"))
	}
	finish := finishOf(t, g)
	if finish["green"] != false {
		t.Fatalf("green %v, want false: %+v", finish["green"], finish)
	}
	candidate, _ := finish["candidate"].(map[string]any)
	if sha, _ := candidate["sha"].(string); sha == "" || sha != g.github.ref("refs/heads/mesh/batch/candidate-1") {
		t.Fatalf("candidate %+v", candidate)
	}
	exits := map[string]float64{}
	for _, s := range finish["suites"].([]any) {
		st := s.(map[string]any)
		exits[st["name"].(string)] = st["exit"].(float64)
	}
	if exits["a"] != 0 || exits["b"] == 0 {
		t.Fatalf("suite exits %v, want a 0 and b red", exits)
	}
	triage, _ := finish["triage"].(map[string]any)
	failing, _ := triage["failing_tests"].([]any)
	if triage["class"] != "code" || !slices.Contains(failing, any("example.com/pkg TestRed")) || slices.Contains(failing, any("example.com/pkg TestGreen")) {
		t.Fatalf("triage %+v", triage)
	}
	red := false
	for _, x := range finish["tests"].([]any) {
		tt := x.(map[string]any)
		if tt["name"] == "TestRed" && tt["outcome"] == "fail" {
			red = true
		}
	}
	if !red {
		t.Fatalf("tests %+v do not name TestRed failing", finish["tests"])
	}
}

// A suite that commits in the checkout moves its tree from the
// candidate's: the node refuses to finish with a result of a tree the
// suites did not prove, and hands the lease back saying so.
func TestTheNodeRefusesAResultWhoseTreeMovedBeforeTheFinish(t *testing.T) {
	sess := &stubSession{result: SessionResult{SessionID: "sess-unused"}}
	g := newRig(t, sess)
	lease := runChain(t, g, map[string]string{".runsheet/validate.yml": suiteThatMovesTheTree})
	if got := g.fake.Ended(lease); got == "finish" {
		t.Fatalf("lease %s finished with a moved tree: %+v", lease, finishOf(t, g))
	}
	var said []string
	for _, c := range append(g.fake.Calls("hand-back"), g.fake.Calls("fail")...) {
		said = append(said, string(c.Body))
	}
	if !strings.Contains(strings.Join(said, " "), "changed between build and finish") {
		t.Fatalf("the lease ended %q saying %v", g.fake.Ended(lease), said)
	}
	if len(sess.specs) != 0 {
		t.Fatal("a session ran for a validate lease")
	}
}

// --validate-with-session keeps a validate lease in its session, as
// before: the node runs none of its tools itself.
func TestValidateWithSessionKeepsTheSession(t *testing.T) {
	sess := &stubSession{result: SessionResult{SessionID: "sess-1"}}
	g := newRig(t, sess)
	g.rt.ValidateWithSession = true
	runChain(t, g, map[string]string{".runsheet/validate.yml": twoGreenSuites})
	if len(sess.specs) != 1 {
		t.Fatalf("sessions %d, want 1", len(sess.specs))
	}
	if got := g.github.ref("refs/heads/mesh/batch/candidate-1"); got != "" {
		t.Fatalf("the node pushed %s itself", got)
	}
}

// finishFor keeps only the keys a closed finish_schema names, so a
// product that has not yet named suite_config_sha or environment still
// takes the machine's result; an open schema takes every key.
func TestFinishForKeepsWhatTheSchemaTakes(t *testing.T) {
	finish := map[string]any{"green": true, "evidence": []string{"e"}, "suite_config_sha": "abc", "environment": map[string]any{"release": "v1"}, "learnings": []any{}}
	closed := json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"green":{},"evidence":{},"learnings":{}}}`)
	got, err := finishFor(closed, finish)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"evidence":["e"],"green":true,"learnings":[]}` {
		t.Fatalf("closed schema: %s", got)
	}
	got, _ = finishFor(json.RawMessage(`{"type":"object","properties":{"green":{}}}`), finish)
	var m map[string]any
	json.Unmarshal(got, &m)
	if len(m) != len(finish) {
		t.Fatalf("open schema: %s", got)
	}
}

// The brief's e2e selection runs the integration suite's selected tests
// alone, on the shards they need, beside every other suite; a selection
// of no test leaves the integration suite out; all, or no e2e block,
// runs every suite in one call.
func TestSuiteCallsFollowTheBriefsE2ESelection(t *testing.T) {
	cfg := validate.Config{Steps: []validate.Step{{Name: "lint"}, {Name: "unit"}, {Name: "integration"}}}
	sel := &validateBlock{}
	sel.E2E = &struct {
		All    bool     `json:"all"`
		Tests  []string `json:"tests"`
		Shards int      `json:"shards"`
	}{Tests: []string{"TestA", "TestB"}, Shards: 1}
	calls := suiteCalls(cfg, nil, sel)
	if len(calls) != 2 {
		t.Fatalf("calls %+v", calls)
	}
	if got, _ := json.Marshal(calls[0].args); string(got) != `{"suites":["lint","unit"]}` {
		t.Fatalf("first call %s", got)
	}
	if got, _ := json.Marshal(calls[1].args); string(got) != `{"shards":1,"suites":["integration"],"targets":["e2e:TestA","e2e:TestB"]}` || calls[1].suite != "integration" {
		t.Fatalf("second call %s", got)
	}
	sel.E2E.Tests = nil
	if calls := suiteCalls(cfg, nil, sel); len(calls) != 1 || calls[0].args["suites"] == nil {
		t.Fatalf("no e2e test selected: %+v", calls)
	}
	sel.E2E.All = true
	if calls := suiteCalls(cfg, nil, sel); len(calls) != 1 || len(calls[0].args) != 0 {
		t.Fatalf("all: %+v", calls)
	}
	if calls := suiteCalls(cfg, nil, &validateBlock{}); len(calls) != 1 || len(calls[0].args) != 0 {
		t.Fatalf("no e2e block: %+v", calls)
	}
}

// A validation the node runs itself has no agent in its pod to adopt
// across an upgrade: it is handed back, never handed off.
func TestAMachineValidationIsNotAdoptable(t *testing.T) {
	if ok, why := adoptability(&running{machine: true, sidecarDir: "/sidecar"}); ok || why == "" {
		t.Fatalf("adoptable %v (%q)", ok, why)
	}
	if ok, _ := adoptability(&running{sidecarDir: "/sidecar"}); !ok {
		t.Fatal("a session's lease with a sidecar is adoptable")
	}
}
