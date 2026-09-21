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
	// Attempts and Evaluation: on a retry, how many builds failed
	// evaluation before and what the evaluator said of the last one.
	Attempts   int
	Evaluation string
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
	if in.Attempts > 0 && strings.TrimSpace(in.Evaluation) != "" {
		fmt.Fprintf(&b, "## Previous attempt\n\nThis is attempt %d. The branch carries the last attempt's commits; an independent evaluator rejected it. Fix what it found before anything else:\n\n%s\n\n", in.Attempts+1, strings.TrimSpace(in.Evaluation))
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

// EvaluateInput is what an evaluation prompt is made of: the item, the
// goal's acceptance, and the branch's diff from its base.
type EvaluateInput struct {
	GoalSlug       string
	GoalTitle      string
	GoalAcceptance string
	Seq            int
	Title          string
	Brief          string
	Acceptance     string
	Repo           string
	Branch         string
	BaseRef        string
	Diff           string
}

// Verdict lines the evaluator ends with; ParseVerdict reads them.
const (
	VerdictPass = "VERDICT: PASS"
	VerdictFail = "VERDICT: FAIL"
)

// EvaluatePrompt renders the evaluator's prompt (mesh-specs trd §3.4): a
// fresh session, the diff and the acceptance, an adversarial review that
// runs the acceptance check itself and ends with a verdict line.
func EvaluatePrompt(in EvaluateInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the evaluator of item %d of goal %q (%s), reviewing a branch another session built. You did not write it; assume it is wrong until the evidence says otherwise.\n\n", in.Seq, in.GoalTitle, in.GoalSlug)
	fmt.Fprintf(&b, "# Item %d: %s\n\n", in.Seq, in.Title)
	if strings.TrimSpace(in.Brief) != "" {
		fmt.Fprintf(&b, "## Brief\n\n%s\n\n", strings.TrimSpace(in.Brief))
	}
	if strings.TrimSpace(in.Acceptance) != "" {
		fmt.Fprintf(&b, "## Acceptance (this item)\n\n%s\n\n", strings.TrimSpace(in.Acceptance))
	}
	if strings.TrimSpace(in.GoalAcceptance) != "" {
		fmt.Fprintf(&b, "## Acceptance (the goal)\n\n%s\n\n", strings.TrimSpace(in.GoalAcceptance))
	}
	fmt.Fprintf(&b, "# The branch\n\nYou are in %s with `%s` checked out at the commit the builder pushed; the diff below is `git diff origin/%s...HEAD`.\n\n", in.Repo, in.Branch, in.BaseRef)
	if strings.TrimSpace(in.Diff) == "" {
		b.WriteString("The diff is empty: the branch changed nothing.\n\n")
	} else {
		fmt.Fprintf(&b, "```diff\n%s\n```\n\n", strings.TrimSpace(in.Diff))
	}
	b.WriteString("# Rules\n\n")
	b.WriteString("1. Judge the item's acceptance, literally: every clause, against what the diff does — not what its messages claim.\n")
	b.WriteString("2. Run the acceptance check yourself when it names one (tests, a build, a command). Read the repository's own instructions (AGENTS.md, CLAUDE.md) for how.\n")
	b.WriteString("3. Look for what the builder skipped: untested paths, a check made to pass by weakening it, changes outside the brief, secrets or destructive commands.\n")
	b.WriteString("4. Change nothing: no edits, no commits, no pushes, nothing outside this workspace. You are a reviewer.\n")
	fmt.Fprintf(&b, "5. End your final message with a short review — what holds, what does not, and exactly what a retry must fix — and then one last line that is either `%s` or `%s`. Pass only when every clause of the acceptance holds.\n", VerdictPass, VerdictFail)
	return b.String()
}

// ParseVerdict reads the evaluator's final message: the last verdict line
// decides; the text before it is the notes. ok is false when no verdict
// line is there — an evaluation that reached no verdict is not one.
func ParseVerdict(text string) (passed bool, notes string, ok bool) {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(strings.Trim(strings.TrimSpace(lines[i]), "`*_"))
		switch {
		case strings.EqualFold(l, VerdictPass):
			return true, strings.TrimSpace(strings.Join(lines[:i], "\n")), true
		case strings.EqualFold(l, VerdictFail):
			return false, strings.TrimSpace(strings.Join(lines[:i], "\n")), true
		}
	}
	return false, "", false
}
