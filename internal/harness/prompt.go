package harness

import (
	"encoding/json"
	"fmt"
	"regexp"
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

// evaluateUnverifiableRule is the evaluator's side of adr-004: a clause
// it cannot see at evaluation time is unverifiable, not failed.
const evaluateUnverifiableRule = "A clause you cannot check at evaluation time — a CI check on this item's pull request (it opens only after you pass), a deploy, a file in another repository — is reported in your review as unverifiable, not failed, and the item is not failed on that clause alone. Every clause you can check you still check."

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
	b.WriteString("5. " + evaluateUnverifiableRule + "\n")
	fmt.Fprintf(&b, "6. End your final message with a short review — what holds, what does not, what is unverifiable, and exactly what a retry must fix — and then one last line that is either `%s` or `%s`. Pass only when every clause of the acceptance you can check holds.\n", VerdictPass, VerdictFail)
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
	// Rejected: on the re-ask, the items of the answer before whose
	// acceptance names what no evaluator can see, each with why (adr-004).
	Rejected []string
}

// ExistingItem is one item of the plan before, for the planner.
type ExistingItem struct {
	Seq    int
	Title  string
	Status string
	Note   string // the evaluation or error that decided it
	// Branch and PRURL: where its work went — for a merged item, the
	// pull request main took it in with.
	Branch string
	PRURL  string
}

// PlannedSpec is one item of the graph the planner writes: like
// gateway.PlannedItem but naming the repo (owner/name or name).
type PlannedSpec struct {
	Seq        int          `json:"seq"`
	Title      string       `json:"title"`
	Brief      string       `json:"brief"`
	Acceptance string       `json:"acceptance"`
	DependsOn  []PlannedDep `json:"depends_on"`
	Repo       string       `json:"repo"`
	BaseRef    string       `json:"base_ref"`
	// Model is the model the planner chose for the item (haiku, sonnet,
	// opus or a claude-* id), ModelReason its one line of why.
	Model       string `json:"model"`
	ModelReason string `json:"model_reason"`
}

// PlannedDep is one entry of an item's depends_on: the seq it waits on
// and the one line of what it uses from it (trd §1 WorkItem
// depends_on). A bare integer, the old shape, reads as a dependency
// with no because.
type PlannedDep struct {
	Seq     int    `json:"seq"`
	Because string `json:"because,omitempty"`
}

// UnmarshalJSON reads {"seq": 3, "because": "…"} or a bare 3.
func (d *PlannedDep) UnmarshalJSON(b []byte) error {
	var n int
	if err := json.Unmarshal(b, &n); err == nil {
		*d = PlannedDep{Seq: n}
		return nil
	}
	var obj struct {
		Seq     int    `json:"seq"`
		Because string `json:"because"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return fmt.Errorf("a depends_on entry is a seq or {\"seq\", \"because\"}: %s", b)
	}
	*d = PlannedDep(obj)
	return nil
}

// planDependencyRule is the planner's dependency rubric (trd §5 phase
// 2): an item waits for another only when it uses what that one builds.
const planDependencyRule = "An item depends on another only when it uses something that item creates or changes — a type, command, field, function, file or endpoint — and could not be built against main without it. Sharing a repository or touching the same file is not a dependency: the merge queue rebases, and the scheduler's overlap hold covers conflicts. Order by importance is priority, not dependency. Prefer a wide, shallow graph: the node runs up to 6 items at once and readies every item whose dependencies merged. A final docs or goal-doc item depends only on the items whose results it records. Each dependency carries a one-line `because` naming what is used, as {\"seq\": 3, \"because\": \"uses the Foo type item 3 adds\"}; a dependency without one is dropped."

// planAcceptanceRule is the planner's side of adr-004: an item's
// acceptance names only what its evaluator can see.
const planAcceptanceRule = "An item's acceptance names only what the evaluator can see at evaluation time, on that item's branch, in that item's repository: tests, commands and their output, file contents. It never names a CI check on the item's own pull request (the pull request opens only after evaluation passes), a deploy, or a file in another repository. Work like that becomes its own item in the right repo, or goes in the goal's acceptance."

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
	if len(in.Rejected) > 0 {
		b.WriteString("## Your last answer asked for what no evaluator can see\n\nThe node checked your graph before handing it in and flagged these items:\n\n")
		for _, r := range in.Rejected {
			fmt.Fprintf(&b, "- %s\n", strings.TrimSpace(r))
		}
		b.WriteString("\n" + planAcceptanceRule + " Write the whole graph again with those acceptances rewritten; this is the last ask.\n\n")
	}
	if len(in.Existing) > 0 {
		b.WriteString("## The items so far\n\nYour plan replaces only the live items; merged items stay. Merged items are done: your items may depend on their seqs. Failed and cancelled items stay in the record; do their work again differently if it is still needed. Anything still live is replaced by your plan.\n\n")
		for _, it := range in.Existing {
			fmt.Fprintf(&b, "- %d. %s — %s", it.Seq, it.Title, it.Status)
			if pr := strings.TrimSpace(it.PRURL); pr != "" {
				fmt.Fprintf(&b, " (%s)", pr)
			}
			if strings.TrimSpace(it.Note) != "" {
				fmt.Fprintf(&b, ": %s", strings.TrimSpace(it.Note))
			}
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "\nNumber your items from %d.\n\n", in.NextSeq)
		var merged []ExistingItem
		for _, it := range in.Existing {
			if it.Status == "merged" {
				merged = append(merged, it)
			}
		}
		if len(merged) > 0 {
			b.WriteString("## What main already holds\n\nThese items of the goal merged; their work is on main:\n\n")
			for _, it := range merged {
				fmt.Fprintf(&b, "- %d. %s", it.Seq, it.Title)
				if pr := strings.TrimSpace(it.PRURL); pr != "" {
					fmt.Fprintf(&b, " — %s", pr)
				} else if br := strings.TrimSpace(it.Branch); br != "" {
					fmt.Fprintf(&b, " — branch `%s`", br)
				}
				b.WriteString("\n")
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("# The project\n\n")
	if len(in.Repos) > 0 {
		b.WriteString("Its repos are checked out here, read-only, at their default branch, with its recent history:\n\n")
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
	b.WriteString("# Check main first\n\n")
	fmt.Fprintf(&b, "Before you write a graph, find out what main already holds of this goal's work. For each repo, search its recent merged pull requests and the commits on its default branch since the goal was set — `gh pr list --repo <owner/name> --state merged --limit 50` (add `--search` with the goal's words, and the `mesh/%s-` branches), and `git log --oneline` in its checkout, which carries the default branch's recent history — by title, by the brief's words, and by the files the acceptance names.\n\n", in.GoalSlug)
	b.WriteString("Leave out work main already has. When main may already hold an item's work but you cannot tell it satisfies the acceptance, keep the item and say in its brief that an already_done check is expected: the builder checks main first and, if the acceptance holds there, finishes it already_done with the merged PRs or commits as evidence instead of building it again.\n\n")
	b.WriteString("# Rules\n\n")
	b.WriteString("1. Read before you plan: what exists, what the design says, where the goal's work lands, and what main already holds (above).\n")
	b.WriteString("2. Each item is one fresh session's work in one repo on one branch: small enough to finish and evaluate in one sitting, large enough to be worth a pull request. Two to eight items.\n")
	b.WriteString("3. Each item's brief says what to do and where, naming files and functions where it helps; its acceptance is what an evaluator can check — a test that passes, a command's output, a document with named sections — never a feeling.\n")
	b.WriteString("   " + planAcceptanceRule + "\n")
	b.WriteString("4. depends_on names earlier seqs whose branches must be merged first; items with no dependency between them run in parallel.\n")
	b.WriteString("   " + planDependencyRule + "\n")
	b.WriteString("5. repo is one of the project's repos above, as owner/name. base_ref is its default branch unless the item builds on another item's branch.\n")
	b.WriteString("6. Change nothing in the repos: you plan, you do not build.\n")
	b.WriteString("7. Design keeps pace with code: an item that changes an aggregate, a policy, a process, or a public contract (a gateway field, an API, a console route) also amends the ADR or TRD section that describes it, in the same item and pull request; a decision no ADR covers gets a new ADR at the next free number in that repo. Say so in the item's brief and acceptance. Every new document carries frontmatter with a one-line `summary:`.\n")
	b.WriteString("8. Each item names the model its session runs on, as `model`, with a one-line `model_reason`:\n")
	b.WriteString("   - `haiku` for mechanical items: docs-only, a goal-doc section, a version bump, a rename.\n")
	b.WriteString("   - `sonnet` for a well-specified change in one package with tests to copy.\n")
	b.WriteString("   - `opus` for anything cross-cutting, concurrency, migrations, security, a failing item's retry, or when unsure.\n")
	b.WriteString("9. End your final message with the graph as a JSON object in a ```json fenced block, and nothing after it:\n\n")
	b.WriteString("```json\n{\"items\": [{\"seq\": 1, \"title\": \"…\", \"brief\": \"…\", \"acceptance\": \"…\", \"depends_on\": [], \"repo\": \"owner/name\", \"base_ref\": \"main\", \"model\": \"sonnet\", \"model_reason\": \"…\"}, {\"seq\": 2, \"title\": \"…\", \"brief\": \"…\", \"acceptance\": \"…\", \"depends_on\": [{\"seq\": 1, \"because\": \"uses the … item 1 adds\"}], \"repo\": \"owner/name\", \"base_ref\": \"main\", \"model\": \"sonnet\", \"model_reason\": \"…\"}]}\n```\n")
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

// planCandidates is every place the graph might be in the planner's
// message, best first: each fenced block from the last to the first,
// then the whole text. Trying them in turn rather than taking the last
// fence marker is what makes a graph survive an item whose own brief or
// acceptance mentions a fence — the marker inside the JSON is the last
// one in the message, and reading from it yields a fragment (seen
// 2026-09-22: three goals' plans lost that way, each after a five-minute
// session).
func planCandidates(text string) []string {
	raw := strings.TrimSpace(text)
	const fence = "```"
	var marks []int
	for i := 0; ; {
		j := strings.Index(raw[i:], fence)
		if j < 0 {
			break
		}
		marks = append(marks, i+j)
		i += j + len(fence)
	}
	trim := func(body string) string {
		// drop the language tag on the opening line
		if nl := strings.IndexByte(body, '\n'); nl >= 0 {
			if head := strings.TrimSpace(body[:nl]); head == "" || !strings.ContainsAny(head, "{[") {
				body = body[nl+1:]
			}
		}
		return strings.TrimSpace(body)
	}
	var out []string
	// every span between two markers, the latest close first and, for a
	// given close, the earliest open first — so the whole block wins over
	// a fragment starting at a fence the JSON itself mentions
	for j := len(marks) - 1; j >= 1; j-- {
		for i := 0; i < j; i++ {
			if body := trim(raw[marks[i]+len(fence) : marks[j]]); body != "" {
				out = append(out, body)
			}
		}
	}
	// an unclosed final block: everything after the last marker
	if n := len(marks); n > 0 {
		if body := trim(raw[marks[n-1]+len(fence):]); body != "" {
			out = append(out, body)
		}
	}
	return append(out, raw)
}

// ParsePlan reads the planner's final message: the graph in a fenced block
// (or the whole text when it is JSON) as {"items": [...]} or a bare
// array. It checks the graph's shape — seqs, titles, dependencies on
// other items or on the merged seqs given — but not the repos, which
// the caller knows.
func ParsePlan(text string, merged ...int) ([]PlannedSpec, error) {
	var items []PlannedSpec
	for _, raw := range planCandidates(text) {
		var wrapped struct {
			Items []PlannedSpec `json:"items"`
		}
		if err := json.Unmarshal([]byte(raw), &wrapped); err == nil && len(wrapped.Items) > 0 {
			items = wrapped.Items
			break
		}
		var bare []PlannedSpec
		if err := json.Unmarshal([]byte(raw), &bare); err == nil && len(bare) > 0 {
			items = bare
			break
		}
	}
	if len(items) == 0 {
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
		for _, dep := range it.DependsOn {
			if d := dep.Seq; (!seqs[d] && !done[d]) || d == it.Seq {
				return nil, fmt.Errorf("item %d depends on %d, which is neither another item nor merged", it.Seq, d)
			}
		}
	}
	return items, nil
}

// ResearchInput is what a research session is given: the project, the
// direction a person wrote, the goals it has already had, and the room
// there is for new ones (runsheet trd §6c).
type ResearchInput struct {
	Org       string
	Project   string
	Direction string
	// Repos are the project's repos, checked out read-only, by
	// owner/name → directory.
	Repos        map[string]string
	ContextDir   string
	ContextFiles []string
	Prologue     bool
	// Goals are what the project has already taken on, newest first —
	// what to build on and what not to propose again.
	Goals []ExistingGoal
	// Room is how many goals the platform has space for right now; 0
	// means the session should say so rather than propose.
	Room int
	// DefaultBudgetCents is what a goal here usually costs, for the
	// session to size its proposals against.
	DefaultBudgetCents int
}

// ExistingGoal is one goal the project has had.
type ExistingGoal struct {
	Slug   string
	Title  string
	Status string
	Note   string
}

// ProposedSpec is one goal a research session proposes.
type ProposedSpec struct {
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Statement   string `json:"statement"`
	Acceptance  string `json:"acceptance"`
	Why         string `json:"why"`
	BudgetCents int    `json:"budget_cents"`
}

// ResearchPrompt is the director's session (runsheet trd §6c): read the
// direction, the project and what it has already done, and propose the
// next goals worth doing. It writes nothing — the goals it proposes are
// built by other sessions, each of which plans and builds as usual.
//
// Its contract line is the JSON block at the end, like the planner's.
func ResearchPrompt(in ResearchInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the director of %s, project %s. Nobody is awake. Your job is to decide what this project should do next and to write the goals for it — other sessions plan each goal into items, build them, and open pull requests that are merged when they pass.\n\n", in.Org, in.Project)
	b.WriteString("# The direction\n\n")
	if strings.TrimSpace(in.Direction) != "" {
		fmt.Fprintf(&b, "%s\n\n", strings.TrimSpace(in.Direction))
	} else {
		b.WriteString("None was given. Propose nothing and say so.\n\n")
	}
	b.WriteString("This is what the project is for. Everything you propose serves it.\n\n")
	if len(in.Goals) > 0 {
		b.WriteString("# What the project has already taken on\n\n")
		for _, g := range in.Goals {
			fmt.Fprintf(&b, "- `%s` — %s — %s", g.Slug, g.Title, g.Status)
			if strings.TrimSpace(g.Note) != "" {
				fmt.Fprintf(&b, ": %s", strings.TrimSpace(g.Note))
			}
			b.WriteString("\n")
		}
		b.WriteString("\nBuild on what is done. Do not propose a goal that is live, and do not propose one that was done unless the work plainly remains. A goal that is blocked or paused is waiting for a person — leave it alone.\n\n")
	}
	b.WriteString("# The project\n\n")
	if len(in.Repos) > 0 {
		b.WriteString("Its repos are checked out here, read-only, at their default branch, with its recent history:\n\n")
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
		b.WriteString(": the instructions and the ADRs are binding, the PRD and TRD are the design. Read them first, then the repos' own AGENTS.md, CLAUDE.md and README, then the code.\n\n")
	} else {
		b.WriteString("There are no context documents; read each repo's AGENTS.md, CLAUDE.md and README, then the code.\n\n")
	}
	if in.Prologue {
		b.WriteString(prologueRule(in.ContextDir, in.Org+"/"+in.Project) + "\n\n")
	}
	b.WriteString("# Rules\n\n")
	fmt.Fprintf(&b, "1. Research first. Read the direction, the design documents, the code and the open issues before you propose anything. A goal that duplicates what is there, or that the design already forbids, is worse than none.\n")
	switch {
	case in.Room <= 0:
		b.WriteString("2. There is no room for another goal right now — the project is at its ceiling. Propose nothing, and say in your notes what you would have proposed.\n")
	case in.Room == 1:
		b.WriteString("2. Propose at most one goal — there is room for one. Make it the most valuable thing you found, or none if nothing is worth doing.\n")
	default:
		fmt.Fprintf(&b, "2. Propose at most %d goals, fewer if fewer are worth doing, none if none is. There is room for %d right now.\n", in.Room, in.Room)
	}
	b.WriteString("3. Each goal is a few days' work for one person: several items' worth, one coherent piece of value. Not a task (\"rename the button\") and not a project (\"rewrite the front end\").\n")
	b.WriteString("4. The statement says what is to be true when it is done, in the words the person who gave the direction would use. The acceptance is what someone can check — a test that passes, a command's output, a page that loads — never a feeling.\n")
	b.WriteString("5. The why is one sentence on why this is the next thing, given the direction and what is already done. A person reads it in the morning to decide whether you were right.\n")
	b.WriteString("6. Order them: the most valuable first. The platform may have room for fewer than you propose and takes them in your order.\n")
	b.WriteString("7. The slug is lowercase, hyphenated and stable — it is the goal's identity, so the same idea proposed twice must carry the same slug rather than making a second goal.\n")
	b.WriteString("8. Prefer work that can be finished and merged unattended: clear acceptance, tests that exist, no decision a person has to make first. A question for a person is not a goal — say it in your notes instead.\n")
	b.WriteString("9. Change nothing in the repos: you research and propose, you do not build.\n")
	if in.DefaultBudgetCents > 0 {
		fmt.Fprintf(&b, "10. A goal here is usually given $%.2f. Set budget_cents only when a goal plainly needs more or less; leave it out otherwise.\n", float64(in.DefaultBudgetCents)/100)
	}
	b.WriteString("\nEnd your final message with your proposals as a JSON object in a ```json fenced block, and nothing after it. An empty list is a valid answer:\n\n")
	b.WriteString("```json\n{\"goals\": [{\"slug\": \"…\", \"title\": \"…\", \"statement\": \"…\", \"acceptance\": \"…\", \"why\": \"…\"}]}\n```\n")
	return b.String()
}

// ParseProposals reads a research session's goals out of its final
// message. Like ParsePlan it tries every fenced span rather than the
// last fence marker, so a proposal whose own prose mentions a fence
// does not cost the session.
func ParseProposals(text string) ([]ProposedSpec, error) {
	var goals []ProposedSpec
	found := false
	for _, raw := range planCandidates(text) {
		var wrapped struct {
			Goals []ProposedSpec `json:"goals"`
		}
		if err := json.Unmarshal([]byte(raw), &wrapped); err == nil && len(wrapped.Goals) > 0 {
			goals, found = wrapped.Goals, true
			break
		}
		var bare []ProposedSpec
		if err := json.Unmarshal([]byte(raw), &bare); err == nil && len(bare) > 0 {
			goals, found = bare, true
			break
		}
		// an empty list is an answer: {"goals": []} parses to nothing,
		// and saying "nothing is worth doing" must not read as a failure
		if err := json.Unmarshal([]byte(raw), &wrapped); err == nil && wrapped.Goals != nil {
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("no proposals in the director's message")
	}
	slugs := map[string]bool{}
	out := make([]ProposedSpec, 0, len(goals))
	for _, g := range goals {
		g.Slug, g.Title = strings.TrimSpace(g.Slug), strings.TrimSpace(g.Title)
		if g.Slug == "" || g.Title == "" {
			return nil, fmt.Errorf("a proposal carries a slug and a title: %+v", g)
		}
		if strings.TrimSpace(g.Statement) == "" || strings.TrimSpace(g.Acceptance) == "" {
			return nil, fmt.Errorf("proposal %q carries a statement and an acceptance", g.Slug)
		}
		if slugs[g.Slug] {
			continue // the same goal twice in one session is one goal
		}
		slugs[g.Slug] = true
		out = append(out, g)
	}
	return out, nil
}

// RebaseInput is what a rebase prompt is made of: the item whose pull
// request conflicts with its base, the conflict as the integrator found
// it, and the item's acceptance to re-check on the rebased head.
type RebaseInput struct {
	GoalSlug   string
	GoalTitle  string
	Seq        int
	Title      string
	Brief      string
	Acceptance string
	Repo       string
	Branch     string
	BaseRef    string
	// Conflict is the conflicting regions in git's markers and Files the
	// files, as RequestWorkItemRebase gave them (either may be empty).
	Conflict string
	Files    []string
	// ContextDir, ContextFiles, Project and Prologue as on EvaluateInput.
	ContextDir   string
	ContextFiles []string
	Project      string
	Prologue     bool
}

// The contract lines a rebase session ends with; ParseRebase reads them.
// A rebase that landed ends with RebasedLine and its pushed head, then
// ResolvedLine and the files it resolved; one that did not, with
// RebaseFailedLine and why.
const (
	RebasedLine      = "REBASED:"
	ResolvedLine     = "RESOLVED:"
	RebaseFailedLine = "REBASE FAILED:"
)

// RebaseRunbook is the mesh-branch-rebase skill in runsheet/runsheet,
// the runbook a rebase session follows.
const (
	RebaseRunbookProject = "runsheet/runsheet"
	RebaseRunbookSlug    = "skill-mesh-branch-rebase"
)

// RebasePrompt renders the prompt of a session of kind rebase (runsheet
// ADR 008 step 2): its whole job is to rebase the item's branch onto its
// base keeping both sides' intent, re-run the acceptance, push with
// --force-with-lease and say what it did in the contract lines.
func RebasePrompt(in RebaseInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are a Mesh node session of kind rebase for item %d of goal %q (%s). The item's pull request passed evaluation, then its base moved and it conflicts; the integrator could not resolve it itself. Your whole job is to rebase the branch onto its base, keeping the intent of both sides, and push it. You do not change what the item does.\n\n", in.Seq, in.GoalTitle, in.GoalSlug)
	fmt.Fprintf(&b, "# Item %d: %s\n\n", in.Seq, in.Title)
	if strings.TrimSpace(in.Brief) != "" {
		fmt.Fprintf(&b, "## Brief\n\n%s\n\n", strings.TrimSpace(in.Brief))
	}
	if strings.TrimSpace(in.Acceptance) != "" {
		fmt.Fprintf(&b, "## Acceptance (this item)\n\n%s\n\n", strings.TrimSpace(in.Acceptance))
	}
	fmt.Fprintf(&b, "# The conflict\n\nYou are in %s with `%s` checked out at the head the builder pushed. It conflicts with `origin/%s`.", in.Repo, in.Branch, in.BaseRef)
	if len(in.Files) > 0 {
		fmt.Fprintf(&b, " The files: %s.", strings.Join(in.Files, ", "))
	}
	b.WriteString("\n\n")
	if strings.TrimSpace(in.Conflict) != "" {
		fmt.Fprintf(&b, "As the integrator found it:\n\n```\n%s\n```\n\n", strings.TrimSpace(in.Conflict))
	}
	if in.ContextDir != "" {
		fmt.Fprintf(&b, "# The context\n\nThe project's instructions and decisions are in `%s/`", in.ContextDir)
		if len(in.ContextFiles) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(in.ContextFiles, ", "))
		}
		b.WriteString(": the resolved branch must still keep to them.")
		if in.Prologue {
			b.WriteString(" " + prologueRule(in.ContextDir, in.Project))
		}
		b.WriteString("\n\n")
	} else if in.Prologue {
		b.WriteString("# The context\n\n" + prologueRule("", in.Project) + "\n\n")
	}
	b.WriteString("# The runbook\n\n")
	if in.Prologue {
		fmt.Fprintf(&b, "Read the mesh-branch-rebase skill first: `prologue_document` with project %q, slug %q (or the same slug in project %q, if it is indexed there). If neither has it, these steps are the runbook:\n\n", RebaseRunbookProject, RebaseRunbookSlug, in.Project)
	} else {
		b.WriteString("The mesh-branch-rebase skill's steps:\n\n")
	}
	fmt.Fprintf(&b, "1. Fetch: `git fetch origin && git checkout -B %s origin/%s`.\n", in.Branch, in.Branch)
	fmt.Fprintf(&b, "2. Rebase: `git rebase origin/%s`.\n", in.BaseRef)
	b.WriteString("3. Resolve each conflicting file on its own, reading both sides and why each changed (`git log` of the base since the branch forked, the brief above). Keep both sides' intent: never drop either side's change to make the conflict go away. Generated code is regenerated from the merged source, not merged by hand. Then `git add` it and `git rebase --continue`.\n")
	b.WriteString("4. Run the item's acceptance checks on the rebased head (the repository's AGENTS.md or CLAUDE.md says how), and fix only what the rebase broke.\n")
	fmt.Fprintf(&b, "5. Push: `git push --force-with-lease origin %s`. If the push is refused, the branch moved under you: say so, do not force it.\n\n", in.Branch)
	b.WriteString("# Rules\n\n")
	b.WriteString("1. Rebase, resolve, check, push: nothing else. No new features, no refactors, no pull requests, nothing outside this workspace.\n")
	b.WriteString("2. If a conflict cannot be resolved without dropping one side's intent, or the acceptance fails on the rebased head for a reason the rebase cannot fix, give up: `git rebase --abort`, push nothing, and say exactly why — which file, which change on each side. The builder is handed the item with your words.\n")
	fmt.Fprintf(&b, "3. End your final message with a short account of what you resolved and how, then the contract lines, exactly and last:\n\n")
	fmt.Fprintf(&b, "   On success, two lines — the head you pushed (`git rev-parse HEAD`) and the files you resolved by hand, comma-separated (`(none)` when the rebase needed none):\n\n   ```\n   %s <sha>\n   %s <file>, <file>\n   ```\n\n", RebasedLine, ResolvedLine)
	fmt.Fprintf(&b, "   Otherwise one line with the reason:\n\n   ```\n   %s <why>\n   ```\n", RebaseFailedLine)
	return b.String()
}

// RebaseResult is what a rebase session's contract lines say.
type RebaseResult struct {
	// Rebased: the session pushed Head, having resolved Files.
	Rebased bool
	Head    string
	Files   []string
	// Reason is why it did not, when Rebased is false.
	Reason string
}

var shaShape = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

// ParseRebase reads a rebase session's final message: the last contract
// line decides. ok is false when there is none, or a REBASED line with
// no sha — a session that cannot be read has not rebased.
func ParseRebase(text string) (RebaseResult, bool) {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	var files []string
	var sawResolved bool
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(strings.Trim(strings.TrimSpace(lines[i]), "`*_"))
		upper := strings.ToUpper(l)
		switch {
		case strings.HasPrefix(upper, ResolvedLine) && !sawResolved:
			sawResolved = true
			files = splitFiles(l[len(ResolvedLine):])
		case strings.HasPrefix(upper, RebaseFailedLine):
			reason := strings.TrimSpace(l[len(RebaseFailedLine):])
			if reason == "" {
				reason = strings.TrimSpace(strings.Join(lines[:i], "\n"))
			}
			if reason == "" {
				reason = "the rebase session gave up and did not say why"
			}
			return RebaseResult{Reason: reason}, true
		case strings.HasPrefix(upper, RebasedLine):
			head := strings.Trim(strings.TrimSpace(l[len(RebasedLine):]), "`*_ ")
			if !shaShape.MatchString(head) {
				return RebaseResult{}, false
			}
			if files == nil {
				files = []string{}
			}
			return RebaseResult{Rebased: true, Head: strings.ToLower(head), Files: files}, true
		}
	}
	return RebaseResult{}, false
}

// splitFiles reads a RESOLVED line's files: comma- or space-separated,
// "(none)" or "none" for no file.
func splitFiles(s string) []string {
	files := []string{}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		f = strings.Trim(f, "`*_")
		if f == "" || strings.EqualFold(f, "(none)") || strings.EqualFold(f, "none") {
			continue
		}
		files = append(files, f)
	}
	return files
}
