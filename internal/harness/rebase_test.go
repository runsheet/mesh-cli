package harness

import (
	"reflect"
	"strings"
	"testing"
)

// The rebase prompt carries the job's facts and the runbook, and ends on
// the finish: the rebased head handed to mesh_result (ADR-041).
func TestRebasePromptContract(t *testing.T) {
	p := RebasePrompt(RebaseInput{GoalSlug: "ship-it", GoalTitle: "Ship it", Seq: 2, Title: "Make it pass", Acceptance: "go test ./... passes",
		Repo: "acme/widgets", Branch: "mesh/ship-it-2", BaseRef: "main", Conflict: "<<<<<<< main.go\n=======\n>>>>>>>", Files: []string{"main.go"},
		ContextDir: ".mesh/context", ContextFiles: []string{"adr-001.md"}, Project: "acme/mesh", MeshContext: true, Finish: "# How this job finishes\n\nCall `mesh_result` once.\n"})
	for _, want := range []string{
		"# How this job finishes",
		"`rebasedHead.headSha` is the commit you pushed",
		"`mesh_fail` with exactly why",
		"git push --force-with-lease=mesh/ship-it-2:origin/mesh/ship-it-2 origin HEAD:refs/heads/mesh/ship-it-2",
		"a fresh checkout of acme/widgets made for this rebase alone",
		"git rebase origin/main",
		"git checkout --detach origin/mesh/ship-it-2",
		"never drop either side's change",
		"go test ./... passes",
		"<<<<<<< main.go",
		"The files: main.go.",
		`project "runsheet/runsheet", slug "skill-mesh-branch-rebase"`,
		"mesh_document",
		"mesh_search",
		"adr-001.md",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("rebase prompt lacks %q:\n%s", want, p)
		}
	}
	for _, gone := range []string{"REBASED:", "RESOLVED:", "REBASE FAILED:", "End your final message"} {
		if strings.Contains(p, gone) {
			t.Fatalf("the rebase prompt still carries the contract line %q", gone)
		}
	}
	// without mesh-context the steps are the runbook, and no tool is named
	if q := RebasePrompt(RebaseInput{Branch: "b", BaseRef: "main"}); strings.Contains(q, "mesh_document") || !strings.Contains(q, "mesh-branch-rebase skill's steps") {
		t.Fatalf("no-context prompt:\n%s", q)
	}
}

// A rebasedHead decodes to its lower-cased head and its files, trimmed,
// `(none)` and blanks dropped, never nil; one with no commit sha is an
// error. Both headSha and head_sha are read, and files as a list or a
// comma-separated string.
func TestDecodeRebasedHead(t *testing.T) {
	cases := []struct {
		in    map[string]any
		head  string
		files []string
		ok    bool
	}{
		{map[string]any{"headSha": "ABC1234def", "files": []any{"main.go", " specs/goal-x.md "}}, "abc1234def", []string{"main.go", "specs/goal-x.md"}, true},
		{map[string]any{"head_sha": "abc1234", "files": "(none)"}, "abc1234", []string{}, true},
		{map[string]any{"headSha": "abc1234"}, "abc1234", []string{}, true},
		{map[string]any{"headSha": "abc1234", "files": "a.go, b.go"}, "abc1234", []string{"a.go", "b.go"}, true},
		{map[string]any{"headSha": "not-a-sha", "files": []any{"a.go"}}, "", nil, false},
		{map[string]any{}, "", nil, false},
		{nil, "", nil, false},
	}
	for _, c := range cases {
		head, files, err := DecodeRebasedHead(c.in)
		if (err == nil) != c.ok || head != c.head || !reflect.DeepEqual(files, c.files) {
			t.Errorf("DecodeRebasedHead(%v) = %q %v %v; want %q %v ok=%v", c.in, head, files, err, c.head, c.files, c.ok)
		}
	}
}

// A rebase session on the brief's guidance opens with it and carries
// none of the node's role text or rules; without guidance the built-in
// text runs, ending on the finish (ADR-041).
func TestRebasePromptRunsOnTheBriefsGuidance(t *testing.T) {
	finish := "# How this job finishes\n\nCall `mesh_result` once.\n"
	p := RebasePrompt(RebaseInput{GoalSlug: "g", GoalTitle: "G", Seq: 2, Title: "T", Repo: "o/r", Branch: "b", BaseRef: "main", Guidance: "You rebase one branch.\n", Finish: finish})
	if !strings.HasPrefix(p, "You rebase one branch.\n\n# Item 2: T\n") {
		t.Fatalf("the prompt does not open with the guidance:\n%s", p)
	}
	for _, want := range []string{"# The conflict", "# The runbook", "git rebase origin/main", finish, "`rebasedHead.headSha`"} {
		if !strings.Contains(p, want) {
			t.Errorf("the guided rebase prompt lacks %q", want)
		}
	}
	for _, gone := range []string{"You are a Mesh node session of kind rebase", "# Rules", "1. Rebase, resolve, check, push"} {
		if strings.Contains(p, gone) {
			t.Errorf("the guided rebase prompt still carries %q", gone)
		}
	}
	fallback := RebasePrompt(RebaseInput{Branch: "b", BaseRef: "main", Finish: finish})
	if !strings.Contains(fallback, "You are a Mesh node session of kind rebase") || !strings.Contains(fallback, "# Rules\n\n1. Rebase, resolve, check, push") || !strings.HasSuffix(strings.TrimSpace(fallback), "the builder is handed the item with your words.") {
		t.Fatalf("the built-in rebase prompt:\n%s", fallback)
	}
}
