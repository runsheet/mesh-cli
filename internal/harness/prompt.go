package harness

import (
	"fmt"
	"strings"
)

// PromptInput is what a work item's build prompt is made of.
type PromptInput struct {
	GoalSlug       string
	GoalTitle      string
	GoalStatement  string
	GoalAcceptance string
	Seq            int
	Title          string
	Brief          string
	Acceptance     string
	Repo           string // owner/name
	Branch         string
	BaseRef        string
	// ContextDir is where the goal's Prologue documents were written
	// (relative to the workspace), empty when there were none.
	ContextDir string
	// ContextFiles lists what is in ContextDir, for the prompt.
	ContextFiles []string
}

// BuildPrompt renders the session's whole prompt for a build job: the item
// (title, brief, acceptance), the goal it serves (statement, acceptance)
// and the rules of the road — read the context first, stay on the branch,
// commit clearly, run the acceptance check, never push.
func BuildPrompt(in PromptInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are a Mesh node session working on item %d of goal %q (%s).\n\n", in.Seq, in.GoalTitle, in.GoalSlug)
	fmt.Fprintf(&b, "# Item %d: %s\n\n", in.Seq, in.Title)
	if strings.TrimSpace(in.Brief) != "" {
		fmt.Fprintf(&b, "## Brief\n\n%s\n\n", strings.TrimSpace(in.Brief))
	}
	if strings.TrimSpace(in.Acceptance) != "" {
		fmt.Fprintf(&b, "## Acceptance (this item)\n\n%s\n\n", strings.TrimSpace(in.Acceptance))
	}
	fmt.Fprintf(&b, "# Goal: %s\n\n", in.GoalTitle)
	if strings.TrimSpace(in.GoalStatement) != "" {
		fmt.Fprintf(&b, "## Statement\n\n%s\n\n", strings.TrimSpace(in.GoalStatement))
	}
	if strings.TrimSpace(in.GoalAcceptance) != "" {
		fmt.Fprintf(&b, "## Acceptance (the goal)\n\n%s\n\n", strings.TrimSpace(in.GoalAcceptance))
	}
	b.WriteString("# Rules\n\n")
	if in.ContextDir != "" {
		fmt.Fprintf(&b, "1. Read the project's context in `%s/` first", in.ContextDir)
		if len(in.ContextFiles) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(in.ContextFiles, ", "))
		}
		b.WriteString(": the instructions and the ADRs are binding, the PRD and TRD are the design.\n")
	} else {
		b.WriteString("1. Read the repository's own instructions (AGENTS.md, CLAUDE.md, README) first.\n")
	}
	fmt.Fprintf(&b, "2. You are in %s on branch `%s` (from `%s`). Work on this branch only; do not switch or create branches.\n", in.Repo, in.Branch, in.BaseRef)
	b.WriteString("3. Commit as you go with clear, conventional messages that say what changed and why.\n")
	b.WriteString("4. Before you finish, run the acceptance check above and make it pass; if it cannot, say exactly why in your final message.\n")
	b.WriteString("5. Do not push, open pull requests, or touch anything outside this workspace. The node pushes the branch when you are done.\n")
	b.WriteString("6. End with a short summary of what you did and how the acceptance was checked.\n")
	return b.String()
}
