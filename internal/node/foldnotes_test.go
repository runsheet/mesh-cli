package node

import (
	"context"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// The goal's shared-docs writer is built with the notes its siblings
// left for it (ADR-025): each sibling the brief names whose last note
// opens "for the docs writer:" is a line of the prompt's "Notes to fold
// in", with its pull request; a sibling with no such note is left out.
func TestWriterBuildPromptCarriesSiblingsNotes(t *testing.T) {
	const item3 = "abababab-1111-4111-8111-abababababab"
	var prompts []string
	h := harnessFunc(func(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
		prompts = append(prompts, job.Prompt)
		return harness.Result{Success: true, Text: "done", CostUSD: 1, SessionID: "s-1"}, nil
	})
	f := seed(t)
	f.WorkItems[item2]["lastNote"] = "for the docs writer: AGENTS.md names sharedCodeFiles"
	f.WorkItems[item2]["prUrl"] = "https://github.com/acme/widgets/pull/2"
	f.WorkItems[item3] = map[string]any{"id": item3, "goalId": goalID, "orgId": orgID, "seq": 3, "title": "Name the files",
		"brief": "x", "acceptance": "y", "repoId": repoID, "baseRef": "main", "status": "merged", "dependsOn": []string{},
		"lastNote": "for the docs writer: the ADR names planFilesRule", "prUrl": "https://github.com/acme/widgets/pull/3"}
	r, _, item, _ := runnerOnFake(t, f, h, nil)
	item.Brief = gateway.BriefWithSharedDocsWriter("Write the docs.", []gateway.SharedDocsSibling{{Seq: 2, Title: "Make it pass"}, {Seq: 3, Title: "Name the files"}})
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 1 {
		t.Fatalf("%d sessions", len(prompts))
	}
	p := prompts[0]
	for _, want := range []string{
		"## Notes to fold in\n",
		"- item 2 (Make it pass): for the docs writer: AGENTS.md names sharedCodeFiles (https://github.com/acme/widgets/pull/2)\n",
		"- item 3 (Name the files): for the docs writer: the ADR names planFilesRule (https://github.com/acme/widgets/pull/3)\n",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("the writer's prompt lacks %q:\n%s", want, p)
		}
	}
}

// An item that is not the writer gets no section, whatever its
// siblings noted.
func TestNonWriterBuildPromptHasNoNotes(t *testing.T) {
	var prompts []string
	h := harnessFunc(func(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
		prompts = append(prompts, job.Prompt)
		return harness.Result{Success: true, Text: "done", CostUSD: 1, SessionID: "s-1"}, nil
	})
	f := seed(t)
	f.WorkItems[item2]["lastNote"] = "for the docs writer: something"
	r, _, item, _ := runnerOnFake(t, f, h, nil)
	item.Brief = gateway.BriefWithoutSharedDocs("Write the test.", 2)
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 1 || strings.Contains(prompts[0], "## Notes to fold in") {
		t.Fatalf("a non-writer's prompt carries the notes:\n%v", prompts)
	}
}
