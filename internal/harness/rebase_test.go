package harness

import (
	"reflect"
	"strings"
	"testing"
)

// The rebase prompt's contract lines are load-bearing: ParseRebase reads
// them, so the prompt must name them exactly as they are parsed.
func TestRebasePromptContract(t *testing.T) {
	p := RebasePrompt(RebaseInput{GoalSlug: "ship-it", GoalTitle: "Ship it", Seq: 2, Title: "Make it pass", Acceptance: "go test ./... passes",
		Repo: "acme/widgets", Branch: "mesh/ship-it-2", BaseRef: "main", Conflict: "<<<<<<< main.go\n=======\n>>>>>>>", Files: []string{"main.go"},
		ContextDir: ".mesh/context", ContextFiles: []string{"adr-001.md"}, Project: "acme/mesh", MeshContext: true})
	for _, want := range []string{
		"REBASED: <sha>\n   RESOLVED: <file>, <file>",
		"REBASE FAILED: <why>",
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
	if RebasedLine != "REBASED:" || ResolvedLine != "RESOLVED:" || RebaseFailedLine != "REBASE FAILED:" {
		t.Fatal("the contract lines changed")
	}
	// without mesh-context the steps are the runbook, and no tool is named
	if q := RebasePrompt(RebaseInput{Branch: "b", BaseRef: "main"}); strings.Contains(q, "mesh_document") || !strings.Contains(q, "mesh-branch-rebase skill's steps") {
		t.Fatalf("no-context prompt:\n%s", q)
	}
}

func TestParseRebase(t *testing.T) {
	cases := []struct {
		in   string
		want RebaseResult
		ok   bool
	}{
		{"Kept both enum members.\n\nREBASED: ABC1234def\nRESOLVED: main.go, specs/goal-x.md", RebaseResult{Rebased: true, Head: "abc1234def", Files: []string{"main.go", "specs/goal-x.md"}}, true},
		{"**REBASED: abc1234**\n`RESOLVED: (none)`", RebaseResult{Rebased: true, Head: "abc1234", Files: []string{}}, true},
		{"REBASED: abc1234", RebaseResult{Rebased: true, Head: "abc1234", Files: []string{}}, true},
		{"main.go: both sides rewrote Run differently.\nREBASE FAILED: main.go Run: the base removed the retry the branch depends on", RebaseResult{Reason: "main.go Run: the base removed the retry the branch depends on"}, true},
		{"main.go cannot be merged.\nREBASE FAILED:", RebaseResult{Reason: "main.go cannot be merged."}, true},
		{"REBASED: abc1234\nthen the push was refused\nREBASE FAILED: push refused", RebaseResult{Reason: "push refused"}, true},
		{"REBASED: not-a-sha\nRESOLVED: a.go", RebaseResult{}, false},
		{"I rebased it.", RebaseResult{}, false},
		{"", RebaseResult{}, false},
	}
	for _, c := range cases {
		got, ok := ParseRebase(c.in)
		if ok != c.ok || !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseRebase(%q) = %+v %v; want %+v %v", c.in, got, ok, c.want, c.ok)
		}
	}
}
