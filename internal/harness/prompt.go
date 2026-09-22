package harness

import (
	"encoding/json"
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
	// Project is the Prologue project (org/slug) the goal belongs to;
	// Prologue says the session has Prologue's tools for context on demand.
	Project  string
	Prologue bool
}

// prologueRule is the line every prompt carries when the session has
// Prologue's tools: the index in the context directory, the tools that
// fetch the rest, and the other projects within reach.
func prologueRule(contextDir, project string) string {
	where := "the project's index"
	if contextDir != "" {
		where = "`" + contextDir + "/INDEX.md`"
	}
	return fmt.Sprintf("%s lists every document of project %q — specs, decisions, instructions, skills — with what is on disk and what is not; fetch any of the rest with the `prologue_document` tool (project %q, the document's slug), search with `prologue_search`, and read another project's decisions through `prologue_projects` and `prologue_documents` when the work touches it. Prefer reading a document to guessing what it says.", where, project, project)
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
	if in.Prologue {
		b.WriteString("   " + prologueRule(in.ContextDir, in.Project) + "\n")
	}
	fmt.Fprintf(&b, "2. You are in %s on branch `%s` (from `%s`). Work on this branch only; do not switch or create branches.\n", in.Repo, in.Branch, in.BaseRef)
	b.WriteString("3. Commit as you go with clear, conventional messages that say what changed and why.\n")
	b.WriteString("4. Before you finish, run the acceptance check above and make it pass; if it cannot, say exactly why in your final message.\n")
	b.WriteString("5. Do not push, open pull requests, or touch anything outside this workspace. The node pushes the branch when you are done.\n")
	b.WriteString("6. Design keeps pace with code: if your change alters an aggregate, a policy, a process, or a public contract (a gateway field, an API, a console route), amend the ADR or TRD section that describes it on this branch; a decision no ADR covers gets a new ADR at the next free number in this repo. A new document carries frontmatter with a one-line `summary:`.\n")
	b.WriteString("7. End with a short summary of what you did and how the acceptance was checked.\n")
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
	// ContextDir and ContextFiles: the goal's Prologue documents, the
	// ADRs and instructions the builder was bound by. Project and
	// Prologue as on PromptInput.
	ContextDir   string
	ContextFiles []string
	Project      string
	Prologue     bool
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
	if in.ContextDir != "" {
		fmt.Fprintf(&b, "# The context\n\nThe project's instructions and decisions are in `%s/`", in.ContextDir)
		if len(in.ContextFiles) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(in.ContextFiles, ", "))
		}
		b.WriteString(": the instructions and the ADRs bound the builder; the PRD and TRD are the design.")
		if in.Prologue {
			b.WriteString(" " + prologueRule(in.ContextDir, in.Project))
		}
		b.WriteString("\n\n")
	} else if in.Prologue {
		b.WriteString("# The context\n\n" + prologueRule("", in.Project) + "\n\n")
	}
	b.WriteString("# Rules\n\n")
	b.WriteString("1. Judge the item's acceptance, literally: every clause, against what the diff does — not what its messages claim. A change that goes against an ADR or the instructions fails, whatever the acceptance says.\n")
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

// PlanInput is what a planner prompt is made of: the goal, its project's
// context and repos, and the budget the graph has to fit.
type PlanInput struct {
	GoalSlug    string
	GoalTitle   string
	Statement   string
	Acceptance  string
	BudgetCents int
	Org         string
	Project     string
	// Repos lists the project's repos as owner/name → the directory they
	// are checked out in, relative to the workspace.
	Repos map[string]string
	// ContextDir and ContextFiles: the goal's Prologue documents.
	ContextDir   string
	ContextFiles []string
	// Blocked: on a re-plan, what went wrong the last time.
	Blocked string
	// Prologue says the session has Prologue's tools for context on demand.
	Prologue bool
	// Existing are the goal's items so far, on a re-plan: merged ones
	// stay and may be depended on, failed and cancelled ones are the
	// record, live ones are replaced by this plan. NextSeq is where the
	// plan's numbering starts.
	Existing []ExistingItem
	NextSeq  int
}

// ExistingItem is one item of the plan before, for the planner.
type ExistingItem struct {
	Seq    int
	Title  string
	Status string
	Note   string // the evaluation or error that decided it
}

// PlannedSpec is one item of the graph the planner writes: like
// gateway.PlannedItem but naming the repo (owner/name or name).
type PlannedSpec struct {
	Seq        int    `json:"seq"`
	Title      string `json:"title"`
	Brief      string `json:"brief"`
	Acceptance string `json:"acceptance"`
	DependsOn  []int  `json:"depends_on"`
	Repo       string `json:"repo"`
	BaseRef    string `json:"base_ref"`
}

// PlanPrompt renders the planner's prompt (mesh-specs prd, the planner;
// trd §5 phase 2): read the context and the repos, then compile the goal
// into a graph of items — each one fresh session's work with a checkable
// acceptance — and end with the graph as JSON.
func PlanPrompt(in PlanInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the planner of goal %q (%s) for %s, project %s. You compile the goal into a graph of work items; other sessions, each fresh, do the items one at a time, and an evaluator judges each against its acceptance before a pull request opens.\n\n", in.GoalTitle, in.GoalSlug, in.Org, in.Project)
	fmt.Fprintf(&b, "# Goal: %s\n\n", in.GoalTitle)
	if strings.TrimSpace(in.Statement) != "" {
		fmt.Fprintf(&b, "## Statement\n\n%s\n\n", strings.TrimSpace(in.Statement))
	}
	if strings.TrimSpace(in.Acceptance) != "" {
		fmt.Fprintf(&b, "## Acceptance (the goal)\n\n%s\n\n", strings.TrimSpace(in.Acceptance))
	}
	if in.BudgetCents > 0 {
		fmt.Fprintf(&b, "## Budget\n\n$%.2f for the whole goal, every session included. A build session costs about $3–8, an evaluation $1–3.\n\n", float64(in.BudgetCents)/100)
	}
	if strings.TrimSpace(in.Blocked) != "" {
		fmt.Fprintf(&b, "## The last plan blocked\n\n%s\n\nPlan around it.\n\n", strings.TrimSpace(in.Blocked))
	}
	if len(in.Existing) > 0 {
		b.WriteString("## The items so far\n\nMerged items are done and stay: your items may depend on their seqs. Failed and cancelled items stay in the record; do their work again differently if it is still needed. Anything still live is replaced by your plan.\n\n")
		for _, it := range in.Existing {
			fmt.Fprintf(&b, "- %d. %s — %s", it.Seq, it.Title, it.Status)
			if strings.TrimSpace(it.Note) != "" {
				fmt.Fprintf(&b, ": %s", strings.TrimSpace(it.Note))
			}
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "\nNumber your items from %d.\n\n", in.NextSeq)
	}
	b.WriteString("# The project\n\n")
	if len(in.Repos) > 0 {
		b.WriteString("Its repos are checked out here, read-only, at their default branch:\n\n")
		for _, name := range sortedKeys(in.Repos) {
			fmt.Fprintf(&b, "- `%s` in `%s/`\n", name, in.Repos[name])
		}
		b.WriteString("\n")
	}
	if in.ContextDir != "" {
		fmt.Fprintf(&b, "The project's context is in `%s/`", in.ContextDir)
		if len(in.ContextFiles) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(in.ContextFiles, ", "))
		}
		b.WriteString(": the instructions and the ADRs are binding, the PRD and TRD are the design. Read them first, then the repos' own AGENTS.md, CLAUDE.md and README.\n\n")
	} else {
		b.WriteString("There are no context documents; read each repo's AGENTS.md, CLAUDE.md and README first.\n\n")
	}
	if in.Prologue {
		b.WriteString(prologueRule(in.ContextDir, in.Org+"/"+in.Project) + "\n\n")
	}
	b.WriteString("# Rules\n\n")
	b.WriteString("1. Read before you plan: what exists, what the design says, where the goal's work lands.\n")
	b.WriteString("2. Each item is one fresh session's work in one repo on one branch: small enough to finish and evaluate in one sitting, large enough to be worth a pull request. Two to eight items.\n")
	b.WriteString("3. Each item's brief says what to do and where, naming files and functions where it helps; its acceptance is what an evaluator can check — a test that passes, a command's output, a document with named sections — never a feeling.\n")
	b.WriteString("4. depends_on names earlier seqs whose branches must be merged first; items with no dependency between them run in parallel. Keep the graph shallow.\n")
	b.WriteString("5. repo is one of the project's repos above, as owner/name. base_ref is its default branch unless the item builds on another item's branch.\n")
	b.WriteString("6. Change nothing in the repos: you plan, you do not build.\n")
	b.WriteString("7. Design keeps pace with code: an item that changes an aggregate, a policy, a process, or a public contract (a gateway field, an API, a console route) also amends the ADR or TRD section that describes it, in the same item and pull request; a decision no ADR covers gets a new ADR at the next free number in that repo. Say so in the item's brief and acceptance. Every new document carries frontmatter with a one-line `summary:`.\n")
	b.WriteString("8. End your final message with the graph as a JSON object in a ```json fenced block, and nothing after it:\n\n")
	b.WriteString("```json\n{\"items\": [{\"seq\": 1, \"title\": \"…\", \"brief\": \"…\", \"acceptance\": \"…\", \"depends_on\": [], \"repo\": \"owner/name\", \"base_ref\": \"main\"}]}\n```\n")
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// ParsePlan reads the planner's final message: the last ```json block
// (or the whole text when it is JSON) as {"items": [...]} or a bare
// array. It checks the graph's shape — seqs, titles, dependencies on
// other items or on the merged seqs given — but not the repos, which
// the caller knows.
func ParsePlan(text string, merged ...int) ([]PlannedSpec, error) {
	raw := strings.TrimSpace(text)
	if i := strings.LastIndex(raw, "```json"); i >= 0 {
		rest := raw[i+len("```json"):]
		if j := strings.Index(rest, "```"); j >= 0 {
			raw = strings.TrimSpace(rest[:j])
		} else {
			raw = strings.TrimSpace(rest)
		}
	} else if i := strings.LastIndex(raw, "```"); i >= 0 {
		// a fence without a language: take the last block
		head := raw[:i]
		if k := strings.LastIndex(head, "```"); k >= 0 {
			raw = strings.TrimSpace(head[k+3:])
			if nl := strings.IndexByte(raw, '\n'); nl >= 0 && !strings.HasPrefix(raw, "{") && !strings.HasPrefix(raw, "[") {
				raw = strings.TrimSpace(raw[nl+1:])
			}
		}
	}
	var wrapped struct {
		Items []PlannedSpec `json:"items"`
	}
	var items []PlannedSpec
	if err := json.Unmarshal([]byte(raw), &wrapped); err == nil && len(wrapped.Items) > 0 {
		items = wrapped.Items
	} else if err := json.Unmarshal([]byte(raw), &items); err != nil || len(items) == 0 {
		return nil, fmt.Errorf("no graph in the planner's message")
	}
	seqs := map[int]bool{}
	for _, it := range items {
		if it.Seq <= 0 || strings.TrimSpace(it.Title) == "" || strings.TrimSpace(it.Repo) == "" {
			return nil, fmt.Errorf("an item has a positive seq, a title and a repo: %+v", it)
		}
		if seqs[it.Seq] {
			return nil, fmt.Errorf("item seq %d twice", it.Seq)
		}
		seqs[it.Seq] = true
	}
	done := map[int]bool{}
	for _, m := range merged {
		done[m] = true
	}
	for _, it := range items {
		for _, d := range it.DependsOn {
			if (!seqs[d] && !done[d]) || d == it.Seq {
				return nil, fmt.Errorf("item %d depends on %d, which is neither another item nor merged", it.Seq, d)
			}
		}
	}
	return items, nil
}
