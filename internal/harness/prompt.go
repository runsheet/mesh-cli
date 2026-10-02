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
	// ContextDir is where the goal's context documents were written
	// (relative to the workspace), empty when there were none.
	ContextDir string
	// ContextFiles lists what is in ContextDir, for the prompt.
	ContextFiles []string
	// Attempts and Evaluation: on a retry, how many builds failed
	// evaluation before and what the evaluator said of the last one.
	Attempts   int
	Evaluation string
	// FailedClauses are the acceptance clauses the last evaluation failed,
	// each with what would satisfy it (evaluateWorkItem's failedClauses).
	// Empty on a first attempt, and from an evaluator that named none.
	FailedClauses []FailedClause
	// LastSession is what the last build session on this item spent its
	// budget on when it was handed back (Result.SpentOn, ADR 012
	// Hand-back), recorded by the node; empty when none was.
	LastSession string
	// Project is the mesh-context project (org/slug) the goal belongs to.
	Project string
	// MeshContext says the session has the built-in mesh-context server
	// (tools.Builtin) and its mesh_* tools for context on demand.
	MeshContext bool
	// Services is what the session's pod runs beside it
	// (sandbox.ServicesLine): the ready ones on 127.0.0.1 and the
	// missing ones; empty when there are none.
	Services string
}

// servicesSection is the prompt's Services section, when there are any.
func servicesSection(b *strings.Builder, services string) {
	if s := strings.TrimSpace(services); s != "" {
		fmt.Fprintf(b, "# Services\n\n%s\n\n", s)
	}
}

// contextRule is the line every prompt carries when the session has
// mesh-context's tools: the context directory holds only this
// session's repos' instructions and its own goal — everything else is
// fetched on demand, search before you read.
func contextRule(contextDir, project string) string {
	where := "The project's index"
	if contextDir != "" {
		where = "`" + contextDir + "/INDEX.md`"
	}
	return fmt.Sprintf("%s says what is on disk — just this session's repos' instructions and its own goal — and how to find the rest: search before you read, with the mesh-context server's tools: `mesh_search` (query → slug, title, kind, summary); read a document only when the work touches it, with `mesh_document` (project %q, the document's slug — never a context file's name: a file name is not a slug, `instructions-acme-widgets-agents.md` is slug `agents`). Reach another project with `mesh_projects` and `mesh_documents`, live state with `mesh_items`. The ADRs and the TRD are binding and are searched before any design decision. A build fetches two or three documents, not forty.", where, project)
}

// BuildPrompt renders the session's whole prompt for a build job: the item
// (title, brief, acceptance), the goal it serves (statement, acceptance)
// the check of main before any building (and the already_done hand-back)
// and the rules of the road — read the context first, stay on the branch,
// commit clearly, run the acceptance check, never push.
func BuildPrompt(in PromptInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are a Mesh node session working on item %d of goal %q (%s).\n\n", in.Seq, in.GoalTitle, in.GoalSlug)
	fmt.Fprintf(&b, "# Item %d: %s\n\n", in.Seq, in.Title)
	if strings.TrimSpace(in.Brief) != "" {
		fmt.Fprintf(&b, "## Brief\n\n%s\n\n", strings.TrimSpace(in.Brief))
		if in.MeshContext {
			b.WriteString("Fetch the documents the brief names before you build; search for anything else.\n\n")
		}
	}
	if strings.TrimSpace(in.Acceptance) != "" {
		fmt.Fprintf(&b, "## Acceptance (this item)\n\n%s\n\n", strings.TrimSpace(in.Acceptance))
	}
	if in.Attempts > 0 && (strings.TrimSpace(in.Evaluation) != "" || len(in.FailedClauses) > 0) {
		fmt.Fprintf(&b, "## Previous attempt\n\nThis is attempt %d. The branch carries the last attempt's commits; an independent evaluator rejected it. Fix what it found before anything else:\n\n", in.Attempts+1)
		if ev := strings.TrimSpace(in.Evaluation); ev != "" {
			fmt.Fprintf(&b, "%s\n\n", ev)
		}
		// the clauses it failed, as a list: the prose above says it too, but a
		// retry that works through a list finishes what the last one missed
		if len(in.FailedClauses) > 0 {
			b.WriteString("### Every clause it failed\n\nOne line per acceptance clause the evaluator failed, with what it said would satisfy it. Fix every one; the next evaluation judges this attempt on these clauses and on whether anything that held before has regressed.\n\n")
			for _, c := range in.FailedClauses {
				fmt.Fprintf(&b, "- **%s** — satisfied by: %s\n", c.Clause, c.Satisfy)
			}
			b.WriteString("\n")
		}
	}
	if s := strings.TrimSpace(in.LastSession); s != "" {
		fmt.Fprintf(&b, "## The last session on this item\n\nIt ran past its budget's hand-back mark with nothing committed and was told to commit what worked and finish. %s Do not repeat that: read less, commit earlier — commit each piece that works as soon as it works.\n\n", s)
	}
	fmt.Fprintf(&b, "# Goal: %s\n\n", in.GoalTitle)
	if strings.TrimSpace(in.GoalStatement) != "" {
		fmt.Fprintf(&b, "## Statement\n\n%s\n\n", strings.TrimSpace(in.GoalStatement))
	}
	if strings.TrimSpace(in.GoalAcceptance) != "" {
		fmt.Fprintf(&b, "## Acceptance (the goal)\n\n%s\n\n", strings.TrimSpace(in.GoalAcceptance))
	}
	checkMainFirst(&b, in)
	servicesSection(&b, in.Services)
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
	if in.MeshContext {
		b.WriteString("   " + contextRule(in.ContextDir, in.Project) + "\n")
	}
	fmt.Fprintf(&b, "2. You are in %s on branch `%s` (from `%s`). Work on this branch only; do not switch or create branches.\n", in.Repo, in.Branch, in.BaseRef)
	b.WriteString("3. Commit as you go with clear, conventional messages that say what changed and why.\n")
	b.WriteString("4. Before you finish, run the acceptance check above and make it pass; if it cannot, say exactly why in your final message.\n")
	b.WriteString("5. Do not push, open pull requests, or touch anything outside this workspace. The node pushes the branch when you are done.\n")
	b.WriteString("6. Design keeps pace with code: if your change alters an aggregate, a policy, a process, or a public contract (a gateway field, an API, a console route), amend the ADR or TRD section that describes it on this branch; a decision no ADR covers gets a new ADR at the next free number in this repo. A new document carries frontmatter with a one-line `summary:`.\n")
	b.WriteString("7. End with a short summary of what you did and how the acceptance was checked.\n")
	return b.String()
}

// AlreadyDoneLine starts the line a build session ends with when main
// already holds the item's work (adr-003 decisions 1 and 2): the rest of
// the message from it is the evidence, which the node hands in as
// mesh_finish outcome=already_done. ParseAlreadyDone reads it.
const AlreadyDoneLine = "ALREADY DONE:"

// checkMainFirst is the builder's first step (adr-003 decision 2): look
// on the base ref for the item's work before writing any, and hand the
// item back already done, with evidence, when the acceptance holds there.
func checkMainFirst(b *strings.Builder, in PromptInput) {
	base := in.BaseRef
	if base == "" {
		base = "main"
	}
	b.WriteString("# Check main first\n\n")
	fmt.Fprintf(b, "Before you write any code, look for this item's work on `origin/%s` — it may have merged already, under this goal or another:\n\n", base)
	fmt.Fprintf(b, "- `git log origin/%s` since the goal was set (the checkout carries the base ref's history; `git fetch origin %s` if it looks short), by the item's title and the brief's keywords;\n", base, base)
	fmt.Fprintf(b, "- `gh pr list --repo %s --state merged --search \"<the title's or the brief's keywords>\"`;\n", in.Repo)
	fmt.Fprintf(b, "- the files and tests the acceptance names, as they are on `origin/%s`.\n\n", base)
	fmt.Fprintf(b, "If the acceptance already holds on `origin/%s`, do not build it again: finish the item as mesh_finish outcome=already_done with the evidence instead. Commit nothing; end your final message with a line that starts `%s` followed by the evidence — the merged PR URLs and/or commit SHAs on `%s` that satisfy the acceptance, and one line on why. The node pushes nothing and hands it in; an evaluator checks the evidence against the acceptance on `%s`. If only part of it is there, build the rest.\n\n", base, AlreadyDoneLine, base, base)
}

// ParseAlreadyDone reads a build session's final message for its
// already_done hand-back: the text from the last line that starts
// AlreadyDoneLine (markdown emphasis allowed) to the end. ok is false
// when there is none or it names no evidence.
func ParseAlreadyDone(text string) (evidence string, ok bool) {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimLeft(strings.TrimSpace(lines[i]), "*_`#> ")
		rest, found := strings.CutPrefix(line, AlreadyDoneLine)
		if !found {
			continue
		}
		rest = strings.TrimLeft(rest, "*_` ")
		ev := strings.TrimSpace(strings.Join(append([]string{rest}, lines[i+1:]...), "\n"))
		return ev, ev != ""
	}
	return "", false
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
	// ContextDir and ContextFiles: the goal's context documents, the
	// ADRs and instructions the builder was bound by. Project and
	// MeshContext as on PromptInput.
	ContextDir   string
	ContextFiles []string
	Project      string
	// MeshContext says the session has the built-in mesh-context server
	// (tools.Builtin) and its mesh_* tools for context on demand.
	MeshContext bool
	// Services as on PromptInput.
	Services string
	// AlreadyDone is the second shape (mesh ADR 003 decision 1): the
	// builder finished the item already_done, with no branch, and
	// Evidence says where main holds the work — merged PR URLs and/or
	// commit SHAs. The session is on BaseRef, not a branch, and has no
	// Diff: it checks the evidence and the acceptance there.
	AlreadyDone bool
	Evidence    string
	// PriorAttempts are the evaluations this item already failed, oldest
	// first and empty on a first attempt. They pin the retry's verdict
	// (evaluateRetryRule).
	PriorAttempts []PriorAttempt
}

// Verdict lines the evaluator ends with; ParseVerdict reads them.
const (
	VerdictPass = "VERDICT: PASS"
	VerdictFail = "VERDICT: FAIL"
)

// Clause lines a failing evaluator writes above its verdict, one per
// acceptance clause it failed: `FAILED: <clause> || SATISFY: <what would
// satisfy it>`. ParseFailedClauses reads them and the node records them
// with the verdict, so a retry is told what to fix in a shape the
// platform stores rather than only in prose a later session must
// re-read. Load-bearing, as the verdict line is.
const (
	ClauseFailed  = "FAILED:"
	ClauseSatisfy = "SATISFY:"
	// ClauseSep divides a clause from what would satisfy it.
	ClauseSep = "||"
	// ClauseMax is how many clauses one verdict carries; beyond it the
	// evaluator is listing, not judging.
	ClauseMax = 20
)

// FailedClause is one acceptance clause an evaluation failed, with what
// would satisfy it. Both halves are required: the platform refuses a
// clause that does not say what would satisfy it.
type FailedClause struct {
	Clause  string
	Satisfy string
}

// PriorAttempt is what an earlier evaluation of the same item failed on:
// its attempt number, the clauses it named and its review.
type PriorAttempt struct {
	Attempt int
	Clauses []FailedClause
	Notes   string
}

// evaluateUnverifiableRule is the evaluator's side of adr-004: a clause
// it cannot see at evaluation time is unverifiable, not failed.
// evaluateRetryRule is the evaluator's side of a retry: an item that has
// satisfied every objection already named must not be failed on a fresh
// one, or three attempts can never converge on anything. A gap nobody
// named before is work still to do, reported, not failed.
const evaluateRetryRule = "Judge this attempt on the clauses earlier attempts failed on, listed above, and on whether anything that held before has regressed. A clause of the acceptance no earlier attempt named, and that is not a regression, belongs in your review as work still to do — not in your verdict: do not fail the item on it. Fail only a clause already named, or a regression."

const evaluateUnverifiableRule = "A clause you cannot check at evaluation time — a CI check on this item's pull request (it opens only after you pass), a deploy, a file in another repository — is reported in your review as unverifiable, not failed, and the item is not failed on that clause alone. Every clause you can check you still check."

// EvaluatePrompt renders the evaluator's prompt (mesh-specs trd §3.4): a
// fresh session, the diff and the acceptance, an adversarial review that
// runs the acceptance check itself and ends with a verdict line. An
// already_done item (AlreadyDone) gets the evidence and the acceptance
// to check on the base ref instead of a diff, under the same ADRs and
// with the same verdict line (mesh ADR 003 decision 1).
func EvaluatePrompt(in EvaluateInput) string {
	var b strings.Builder
	if in.AlreadyDone {
		fmt.Fprintf(&b, "You are the evaluator of item %d of goal %q (%s), checking another session's claim that the item is already done: that %s already holds its work, so nothing was built. You did not make the claim; assume it is wrong until the evidence says otherwise.\n\n", in.Seq, in.GoalTitle, in.GoalSlug, in.BaseRef)
	} else {
		fmt.Fprintf(&b, "You are the evaluator of item %d of goal %q (%s), reviewing a branch another session built. You did not write it; assume it is wrong until the evidence says otherwise.\n\n", in.Seq, in.GoalTitle, in.GoalSlug)
	}
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
	if in.AlreadyDone {
		fmt.Fprintf(&b, "# The claim\n\nThe builder pushed no branch: it finished the item already_done, saying %s already satisfies the acceptance. Its evidence:\n\n", in.BaseRef)
		if ev := strings.TrimSpace(in.Evidence); ev != "" {
			fmt.Fprintf(&b, "```\n%s\n```\n\n", ev)
		} else {
			b.WriteString("(none given — a claim without evidence cannot be confirmed)\n\n")
		}
		fmt.Fprintf(&b, "You are in %s with `origin/%s` checked out (detached) and its full history. There is no diff: check the claim on %s.\n\n", in.Repo, in.BaseRef, in.BaseRef)
	} else {
		fmt.Fprintf(&b, "# The branch\n\nYou are in %s with `%s` checked out at the commit the builder pushed; the diff below is `git diff origin/%s...HEAD`.\n\n", in.Repo, in.Branch, in.BaseRef)
		if strings.TrimSpace(in.Diff) == "" {
			b.WriteString("The diff is empty: the branch changed nothing.\n\n")
		} else {
			fmt.Fprintf(&b, "```diff\n%s\n```\n\n", strings.TrimSpace(in.Diff))
		}
	}
	if in.ContextDir != "" {
		fmt.Fprintf(&b, "# The context\n\nThe project's instructions and decisions are in `%s/`", in.ContextDir)
		if len(in.ContextFiles) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(in.ContextFiles, ", "))
		}
		b.WriteString(": the instructions and the ADRs bound the builder; the PRD and TRD are the design.")
		if in.MeshContext {
			b.WriteString(" " + contextRule(in.ContextDir, in.Project))
		}
		b.WriteString("\n\n")
	} else if in.MeshContext {
		b.WriteString("# The context\n\n" + contextRule("", in.Project) + "\n\n")
	}
	if len(in.PriorAttempts) > 0 {
		b.WriteString("# What earlier attempts failed on\n\nThis item was built and failed before. Each attempt below is an earlier evaluation of it — the clauses it failed, and its review.\n\n")
		for _, p := range in.PriorAttempts {
			fmt.Fprintf(&b, "## Attempt %d\n\n", p.Attempt)
			for _, c := range p.Clauses {
				fmt.Fprintf(&b, "- %s — satisfied by: %s\n", c.Clause, c.Satisfy)
			}
			if len(p.Clauses) == 0 {
				b.WriteString("It named no clauses; its review is all there is.\n")
			}
			if n := strings.TrimSpace(p.Notes); n != "" {
				fmt.Fprintf(&b, "\n%s\n", n)
			}
			b.WriteString("\n")
		}
	}
	servicesSection(&b, in.Services)
	b.WriteString("# Rules\n\n")
	if in.AlreadyDone {
		fmt.Fprintf(&b, "1. Check the evidence on %[1]s: every cited pull request is merged into %[1]s (`gh pr view <url> --json state,mergeCommit,baseRefName`) and every cited SHA is on it (`git merge-base --is-ancestor <sha> origin/%[1]s`). A citation that is not on %[1]s, or evidence that cites nothing, fails the claim.\n", in.BaseRef)
		fmt.Fprintf(&b, "2. Judge the item's acceptance, literally, every clause, against what %[1]s holds now — not what the evidence or a commit message claims — and run the acceptance check yourself on %[1]s when it names one (tests, a build, a command). Read the repository's own instructions (AGENTS.md, CLAUDE.md) for how. Work on %[1]s that goes against an ADR or the instructions does not satisfy the item, whatever the acceptance says.\n", in.BaseRef)
		b.WriteString("3. Look for what the claim skips: a clause of the brief or the acceptance no cited change covers, work that was later reverted, a check that passes only because it tests nothing. When you fail it, say exactly what is missing on " + in.BaseRef + ", so the next builder builds that.\n")
	} else {
		b.WriteString("1. Judge the item's acceptance, literally: every clause, against what the diff does — not what its messages claim. A change that goes against an ADR or the instructions fails, whatever the acceptance says.\n")
		b.WriteString("2. Run the acceptance check yourself when it names one (tests, a build, a command). Read the repository's own instructions (AGENTS.md, CLAUDE.md) for how.\n")
		b.WriteString("3. Look for what the builder skipped: untested paths, a check made to pass by weakening it, changes outside the brief, secrets or destructive commands.\n")
	}
	b.WriteString("4. Change nothing: no edits, no commits, no pushes, nothing outside this workspace. You are a reviewer.\n")
	b.WriteString("5. " + evaluateUnverifiableRule + "\n")
	n := 6
	if len(in.PriorAttempts) > 0 {
		fmt.Fprintf(&b, "%d. %s\n", n, evaluateRetryRule)
		n++
	}
	fmt.Fprintf(&b, "%d. End your final message with a short review — what holds, what does not, what is unverifiable, and exactly what a retry must fix. When you fail it, write one line per failed acceptance clause, `%s <the clause, as the acceptance words it> %s %s <what would satisfy it>`, above the verdict; a clause that does not say what would satisfy it is dropped, so write both halves. Then one last line that is either `%s` or `%s`. Pass only when every clause of the acceptance you can check holds.\n", n, ClauseFailed, ClauseSep, ClauseSatisfy, VerdictPass, VerdictFail)
	return b.String()
}

// ParseFailedClauses reads the FAILED/SATISFY lines of an evaluator's
// final message, in the order written and at most ClauseMax of them. A
// line missing either half is dropped rather than sent: the platform
// refuses a clause without what would satisfy it, and one malformed line
// must not cost the whole verdict. Duplicates are dropped too.
func ParseFailedClauses(text string) []FailedClause {
	var out []FailedClause
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimLeft(strings.TrimSpace(line), "-*\u2022># \t")
		l = strings.TrimSpace(strings.Trim(l, "`*_"))
		if !strings.HasPrefix(strings.ToUpper(l), ClauseFailed) {
			continue
		}
		clause, satisfy, ok := strings.Cut(strings.TrimSpace(l[len(ClauseFailed):]), ClauseSep)
		if !ok {
			continue
		}
		satisfy = strings.TrimSpace(satisfy)
		if strings.HasPrefix(strings.ToUpper(satisfy), ClauseSatisfy) {
			satisfy = strings.TrimSpace(satisfy[len(ClauseSatisfy):])
		}
		clause = strings.TrimSpace(strings.Trim(strings.TrimSpace(clause), "`*_"))
		satisfy = strings.TrimSpace(strings.Trim(satisfy, "`*_"))
		if clause == "" || satisfy == "" || seen[clause] {
			continue
		}
		seen[clause] = true
		if out = append(out, FailedClause{Clause: clause, Satisfy: satisfy}); len(out) == ClauseMax {
			break
		}
	}
	return out
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
	// ContextDir and ContextFiles: the goal's context documents.
	ContextDir   string
	ContextFiles []string
	// Blocked: on a re-plan, what went wrong the last time.
	Blocked string
	// MeshContext says the session has the built-in mesh-context server
	// (tools.Builtin) and its mesh_* tools for context on demand.
	MeshContext bool
	// Existing are the goal's items so far, on a re-plan: merged ones
	// stay and may be depended on, failed and cancelled ones are the
	// record, live ones are replaced by this plan. NextSeq is where the
	// plan's numbering starts.
	Existing []ExistingItem
	NextSeq  int
	// Rejected: on the re-ask, the items of the answer before whose
	// acceptance names what no evaluator can see, each with why (adr-004),
	// or whose model the scorecard says does not work on its repo.
	Rejected []string
	// Evidence is the scorecard's per-repo, per-model first-pass and cost
	// per item (trd §6a): what worked here, for the planner's model choice.
	// Empty when the platform's scorecard carries nothing usable.
	Evidence []ModelEvidence
}

// ModelEvidence is one repo/model row of the scorecard: how often that
// model's first pass evaluated clean on that repo, over how many items,
// and what a merged item cost on average.
type ModelEvidence struct {
	Repo             string
	Model            string
	FirstPass        float64
	Items            int
	CostPerItemCents int
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
// planGoalDocRule keeps the planner out of the goal's own document. The
// node writes it itself when the goal ends (RecordEndedGoal, ADR 003
// decision 4) - straight to the default branch, regenerated from the
// platform's own rows, no item and no pull request. An item that writes it
// as well is work nobody needs, and two such items in one plan conflict
// with each other over the one file. The path is named in goaldoc.go
// alone, which TestOnlyGoalDocWritesTheGoalDoc holds to.
const planGoalDocRule = "Plan no item for the goal's own document: when the goal ends the node writes it itself, under the project's specs, from the platform's record of every item and its pull request. An item that records the goal's outcome is work nobody needs. The documents an item changes are the ones its own change makes wrong - an ADR, a TRD section, a README."

const planDependencyRule = "An item depends on another only when it uses something that item creates or changes — a type, command, field, function, file or endpoint — and could not be built against main without it. Sharing a repository or touching the same file is not a dependency: the merge queue rebases, and the scheduler's overlap hold covers conflicts. Order by importance is priority, not dependency. Prefer a wide, shallow graph: the node runs up to 6 items at once and readies every item whose dependencies merged. A final docs item depends only on the items whose results it records. Each dependency carries a one-line `because` naming what is used, as {\"seq\": 3, \"because\": \"uses the Foo type item 3 adds\"}; a dependency without one is dropped."

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
		b.WriteString("## Your last answer was flagged\n\nThe node checked your graph before handing it in and flagged these items — an acceptance naming what no evaluator can see, or a model the scorecard says does not work on its repo:\n\n")
		for _, r := range in.Rejected {
			fmt.Fprintf(&b, "- %s\n", strings.TrimSpace(r))
		}
		b.WriteString("\n" + planAcceptanceRule + " Write the whole graph again with those acceptances and models rewritten; this is the last ask.\n\n")
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
	if in.MeshContext {
		b.WriteString(contextRule(in.ContextDir, in.Org+"/"+in.Project) + "\n\n")
	}
	b.WriteString("# Check main first\n\n")
	fmt.Fprintf(&b, "Before you write a graph, find out what main already holds of this goal's work. For each repo, search its recent merged pull requests and the commits on its default branch since the goal was set — `gh pr list --repo <owner/name> --state merged --limit 50` (add `--search` with the goal's words, and the `mesh/%s-` branches), and `git log --oneline` in its checkout, which carries the default branch's recent history — by title, by the brief's words, and by the files the acceptance names.\n\n", in.GoalSlug)
	b.WriteString("Leave out work main already has. When main may already hold an item's work but you cannot tell it satisfies the acceptance, keep the item and say in its brief that an already_done check is expected: the builder checks main first and, if the acceptance holds there, finishes it already_done with the merged PRs or commits as evidence instead of building it again.\n\n")
	b.WriteString("# Rules\n\n")
	b.WriteString("1. Read before you plan: what exists, what the design says, where the goal's work lands, and what main already holds (above).\n")
	b.WriteString("2. Each item is one fresh session's work in one repo on one branch: small enough to finish and evaluate in one sitting, large enough to be worth a pull request. Two to eight items.\n")
	b.WriteString("3. Each item's brief says what to do and where, naming files and functions where it helps; its acceptance is what an evaluator can check — a test that passes, a command's output, a document with named sections — never a feeling.\n")
	b.WriteString("   " + planAcceptanceRule + "\n")
	if in.MeshContext {
		b.WriteString("   Each item's brief names, by slug, the two or three documents the builder must fetch with `mesh_document` — found with `mesh_search` while you plan. You search before you read, same as the builder will.\n")
	}
	b.WriteString("4. depends_on names earlier seqs whose branches must be merged first; items with no dependency between them run in parallel.\n")
	b.WriteString("   " + planDependencyRule + "\n")
	b.WriteString("5. repo is one of the project's repos above, as owner/name. base_ref is its default branch unless the item builds on another item's branch.\n")
	b.WriteString("6. Change nothing in the repos: you plan, you do not build.\n")
	b.WriteString("   " + planGoalDocRule + "\n")
	b.WriteString("7. Design keeps pace with code: an item that changes an aggregate, a policy, a process, or a public contract (a gateway field, an API, a console route) also amends the ADR or TRD section that describes it, in the same item and pull request; a decision no ADR covers gets a new ADR at the next free number in that repo. Say so in the item's brief and acceptance. Every new document carries frontmatter with a one-line `summary:`.\n")
	b.WriteString("8. Each item names the model its session runs on, as `model`, with a one-line `model_reason`:\n")
	b.WriteString("   - `haiku` for mechanical items: docs-only, a version bump, a rename.\n")
	b.WriteString("   - `sonnet` for a well-specified change in one package with tests to copy.\n")
	b.WriteString("   - `opus` for anything cross-cutting, concurrency, migrations, security, a failing item's retry, or when unsure.\n")
	if len(in.Evidence) > 0 {
		b.WriteString("\n   Evidence: which models worked here\n\n")
		b.WriteString("   | Repo | Model | First-pass | Items | Cost/item |\n")
		b.WriteString("   | --- | --- | --- | --- | --- |\n")
		for _, ev := range in.Evidence {
			fmt.Fprintf(&b, "   | %s | %s | %.2f | %d | $%.2f |\n", ev.Repo, ev.Model, ev.FirstPass, ev.Items, float64(ev.CostPerItemCents)/100)
		}
		b.WriteString("\n")
	}
	b.WriteString("   A model whose first-pass on a repo is under 0.5 over five or more items is not chosen for that repo; prefer the model with the best first-pass at the lowest cost per item; the goal docs in the context name models that worked.\n")
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
	// MeshContext says the session has the built-in mesh-context server
	// (tools.Builtin) and its mesh_* tools for context on demand.
	MeshContext bool
	// Goals are what the project has already taken on, newest first —
	// what to build on and what not to propose again.
	Goals []ExistingGoal
	// Room is how many goals the platform has space for right now; 0
	// means the session should say so rather than propose — unless it
	// Proposes, when the room is information and not a cap.
	Room int
	// Proposes is a direction in mode propose (runsheet ADR 022): what
	// the session proposes waits for a person and sets no goal, so it
	// takes no live-goal slot and the room does not limit it.
	Proposes bool
	// DefaultBudgetCents is what a goal here usually costs, for the
	// session to size its proposals against.
	DefaultBudgetCents int
	// Bets are the project's strategy bets — the slug H2 headings of
	// the home repo's specs/strategy.md, in order; empty when the file
	// has none. A propose-mode pitch names one of these in its bet.
	Bets []string
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

// PitchSpec is one pitch a propose-mode research session hands in
// (runsheet ADR 026, trd §6c Pitches): the same idea as a proposal, but
// a record — problem and evidence, the strategy bet it serves, the
// goal or initiative it would set, an estimate, risks, alternatives
// and the signal it expects to move.
type PitchSpec struct {
	Slug         string         `json:"slug"`
	Title        string         `json:"title"`
	Problem      string         `json:"problem"`
	Evidence     []EvidenceSpec `json:"evidence"`
	Bet          string         `json:"bet"`
	Scope        ScopeSpec      `json:"scope"`
	Estimate     EstimateSpec   `json:"estimate"`
	Risks        []string       `json:"risks"`
	Alternatives []string       `json:"alternatives"`
	Signal       SignalSpec     `json:"signal"`
}

// EvidenceSpec is one piece of evidence behind a pitch: ref is a URL or
// a platform reference (a research note's path, a scorecard metric, a
// goal or an incident), note is what it shows.
type EvidenceSpec struct {
	Kind string `json:"kind"` // research_note | metric | incident | user_signal
	Ref  string `json:"ref"`
	Note string `json:"note"`
}

// ScopeSpec is what a pitch would set: kind "goal" is exactly one draft
// goal, kind "initiative" is one or more.
type ScopeSpec struct {
	Kind  string          `json:"kind"` // goal | initiative
	Goals []ScopeGoalSpec `json:"goals"`
}

// ScopeGoalSpec is one draft goal of a pitch's scope — a ProposedSpec
// without the why, which is the pitch's problem.
type ScopeGoalSpec struct {
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Statement   string `json:"statement"`
	Acceptance  string `json:"acceptance"`
	BudgetCents int    `json:"budget_cents,omitempty"`
}

// EstimateSpec sizes a pitch: basis is what the figures were drawn
// from — the project's goal_history, for one worth trusting.
type EstimateSpec struct {
	CostCents int     `json:"cost_cents"`
	Days      float64 `json:"days"`
	Basis     string  `json:"basis"`
}

// SignalSpec is the one scorecard metric a pitch expects to move: a
// direction of "up" or "down", over window_days after the goal ships.
type SignalSpec struct {
	Metric      string `json:"metric"`
	HowMeasured string `json:"how_measured"`
	Direction   string `json:"direction"`
	WindowDays  int    `json:"window_days"`
}

// PitchAnswerSpec answers an open question the platform's
// pitch_feedback asked of an earlier pitch of this direction's.
type PitchAnswerSpec struct {
	Pitch string `json:"pitch"`
	Text  string `json:"text"`
}

// PitchOutput is what ParsePitches reads from a propose-mode research
// session's final message.
type PitchOutput struct {
	Pitches []PitchSpec       `json:"pitches"`
	Answers []PitchAnswerSpec `json:"answers"`
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
	if in.MeshContext {
		b.WriteString(contextRule(in.ContextDir, in.Org+"/"+in.Project) + "\n\n")
	}
	b.WriteString("# Rules\n\n")
	fmt.Fprintf(&b, "1. Research first. Read the direction, the design documents, the code and the open issues before you propose anything. A goal that duplicates what is there, or that the design already forbids, is worse than none.\n")
	if in.Proposes {
		researchPitchRules(&b, in)
	} else {
		researchGoalRules(&b, in)
	}
	return b.String()
}

// researchGoalRules writes a set-mode direction's rules and its JSON
// contract (runsheet trd §6c): the goals shape, unchanged so a
// platform and a session both read it exactly as before.
func researchGoalRules(b *strings.Builder, in ResearchInput) {
	switch {
	case in.Room <= 0:
		b.WriteString("2. There is no room for another goal right now — the project is at its ceiling. Propose nothing, and say in your notes what you would have proposed.\n")
	case in.Room == 1:
		b.WriteString("2. Propose at most one goal — there is room for one. Make it the most valuable thing you found, or none if nothing is worth doing.\n")
	default:
		fmt.Fprintf(b, "2. Propose at most %d goals, fewer if fewer are worth doing, none if none is. There is room for %d right now.\n", in.Room, in.Room)
	}
	b.WriteString("3. Each goal is a few days' work for one person: several items' worth, one coherent piece of value. Not a task (\"rename the button\") and not a project (\"rewrite the front end\").\n")
	b.WriteString("4. The statement says what is to be true when it is done, in the words the person who gave the direction would use. The acceptance is what someone can check — a test that passes, a command's output, a page that loads — never a feeling.\n")
	b.WriteString("5. The why is one sentence on why this is the next thing, given the direction and what is already done. A person reads it in the morning to decide whether you were right.\n")
	b.WriteString("6. Order them: the most valuable first. The platform may have room for fewer than you propose and takes them in your order.\n")
	b.WriteString("7. The slug is lowercase, hyphenated and stable — it is the goal's identity, so the same idea proposed twice must carry the same slug rather than making a second goal.\n")
	b.WriteString("8. Prefer work that can be finished and merged unattended: clear acceptance, tests that exist, no decision a person has to make first. A question for a person is not a goal — say it in your notes instead.\n")
	b.WriteString("9. Change nothing in the repos: you research and propose, you do not build.\n")
	if in.DefaultBudgetCents > 0 {
		fmt.Fprintf(b, "10. A goal here is usually given $%.2f. Set budget_cents only when a goal plainly needs more or less; leave it out otherwise.\n", float64(in.DefaultBudgetCents)/100)
	}
	b.WriteString("\nEnd your final message with your proposals as a JSON object in a ```json fenced block, and nothing after it. An empty list is a valid answer:\n\n")
	b.WriteString("```json\n{\"goals\": [{\"slug\": \"…\", \"title\": \"…\", \"statement\": \"…\", \"acceptance\": \"…\", \"why\": \"…\"}]}\n```\n")
}

// researchPitchRules writes a propose-mode direction's rules and its
// JSON contract (runsheet ADR 026, trd §6c Pitches): a pitch argues a
// strategy bet with evidence, scopes it to a goal or an initiative,
// sizes and risks it, and names the signal it should move — not a bare
// goal proposal. The platform hands the direction's earlier pitches'
// feedback back through mesh-context's mesh_projects read, so the session
// answers it before pitching again, never re-pitches a declined idea,
// and grounds its bet and its estimate in what the project already
// knows.
func researchPitchRules(b *strings.Builder, in ResearchInput) {
	room := in.Room
	if room < 0 {
		room = 0
	}
	fmt.Fprintf(b, "2. This direction proposes rather than sets: nothing you propose becomes a goal until a person accepts it, so the room is information, not a cap. The project has room for %d right now. Propose what is worth doing, fewer if fewer are, none if none is; a proposal that would wait for room should say so in its why.\n", room)
	fmt.Fprintf(b, "3. Before you pitch, call `mesh_projects` for this project (`mesh_projects { project: %q }`) and read your own direction's `pitch_feedback`, `bets` and `goal_history`: answer every open question of `open_questions` in your `answers`, never pitch a slug `declined` carries again, name each pitch's `bet` from `bets`, and size each `estimate` against `goal_history`.\n", in.Org+"/"+in.Project)
	bet := "a strategy bet slug (the project's `bets`)"
	if len(in.Bets) > 0 {
		bet = fmt.Sprintf("a strategy bet slug — one of %s", strings.Join(quoteAll(in.Bets), ", "))
	}
	fmt.Fprintf(b, "4. `bet` is %s, or \"\" when the project has none.\n", bet)
	b.WriteString("5. `problem` is what is broken or missing, in the direction's own words. `evidence` is a list of at least one entry — `{\"kind\": \"research_note\"|\"metric\"|\"incident\"|\"user_signal\", \"ref\", \"note\"}` — `ref` a URL or a platform reference to what you read, `note` what it shows. A pitch with no evidence is not ready to pitch.\n")
	b.WriteString("6. `scope` is `{\"kind\": \"goal\"|\"initiative\", \"goals\": [...]}`: kind `goal` is exactly one goal — a single piece of value — kind `initiative` is one or more. Each goal names `slug`, `title`, `statement` and `acceptance`, and `budget_cents` only when it plainly needs more or less than usual.\n")
	b.WriteString("7. `estimate` is `{\"cost_cents\", \"days\", \"basis\"}`: size it against `goal_history`'s median cost and days for goals like it, and say so in `basis`.\n")
	b.WriteString("8. `risks` and `alternatives` are lists: what could go wrong, and what else you considered instead of this pitch.\n")
	b.WriteString("9. `signal` is `{\"metric\", \"how_measured\", \"direction\", \"window_days\"}`: the one scorecard metric this should move (`merged`, `mergedWithoutRewrite`, `costCents`, `conflictReworksPer100Merges`, `shareSettledWithoutSession`, or one like them), how you would measure it, `\"up\"` or `\"down\"`, and the window in days after the goal ships.\n")
	b.WriteString("10. The slug is lowercase, hyphenated and stable — it is the pitch's identity: the same idea pitched twice must carry the same slug rather than making a second pitch.\n")
	b.WriteString("11. Change nothing in the repos: you research and pitch, you do not build.\n")
	if in.DefaultBudgetCents > 0 {
		fmt.Fprintf(b, "12. A goal here is usually given $%.2f. Set a scope goal's budget_cents only when it plainly needs more or less; leave it out otherwise.\n", float64(in.DefaultBudgetCents)/100)
	}
	b.WriteString("\nEnd your final message with your pitches as a JSON object in a ```json fenced block, and nothing after it. An empty list is a valid answer, and so is an empty `answers` list:\n\n")
	b.WriteString("```json\n{\"pitches\": [{\"slug\": \"…\", \"title\": \"…\", \"problem\": \"…\", \"evidence\": [{\"kind\": \"research_note\", \"ref\": \"…\", \"note\": \"…\"}], \"bet\": \"…\", \"scope\": {\"kind\": \"goal\", \"goals\": [{\"slug\": \"…\", \"title\": \"…\", \"statement\": \"…\", \"acceptance\": \"…\"}]}, \"estimate\": {\"cost_cents\": 400000, \"days\": 3, \"basis\": \"…\"}, \"risks\": [\"…\"], \"alternatives\": [\"…\"], \"signal\": {\"metric\": \"merged\", \"how_measured\": \"…\", \"direction\": \"up\", \"window_days\": 30}}], \"answers\": [{\"pitch\": \"<slug>\", \"text\": \"…\"}]}\n```\n")
}

// quoteAll backtick-quotes each string, for a list read in prose.
func quoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = "`" + s + "`"
	}
	return out
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

// ParsePitches reads a propose-mode research session's pitches and
// answers out of its final message, in ParseProposals's own style
// (planCandidates, a nested fence, a bare list, an empty list an
// answer): a pitch without a slug, a title, a problem or a scope
// naming a goal is refused, so is a goal scope of two goals or an
// evidence entry with no ref, the same slug twice is one pitch (the
// first wins), and an answer naming no pitch or no text is dropped.
func ParsePitches(text string) (PitchOutput, error) {
	var pitches []PitchSpec
	var answers []PitchAnswerSpec
	found := false
	for _, raw := range planCandidates(text) {
		var wrapped struct {
			Pitches []PitchSpec       `json:"pitches"`
			Answers []PitchAnswerSpec `json:"answers"`
		}
		if err := json.Unmarshal([]byte(raw), &wrapped); err == nil && wrapped.Pitches != nil {
			pitches, answers, found = wrapped.Pitches, wrapped.Answers, true
			break
		}
		var bare []PitchSpec
		if err := json.Unmarshal([]byte(raw), &bare); err == nil && len(bare) > 0 {
			pitches, found = bare, true
			break
		}
	}
	if !found {
		return PitchOutput{}, fmt.Errorf("no pitches in the director's message")
	}
	slugs := map[string]bool{}
	out := make([]PitchSpec, 0, len(pitches))
	for _, p := range pitches {
		p.Slug, p.Title, p.Problem = strings.TrimSpace(p.Slug), strings.TrimSpace(p.Title), strings.TrimSpace(p.Problem)
		if p.Slug == "" || p.Title == "" || p.Problem == "" {
			return PitchOutput{}, fmt.Errorf("a pitch carries a slug, a title and a problem: %+v", p)
		}
		if p.Scope.Kind == "goal" && len(p.Scope.Goals) != 1 {
			return PitchOutput{}, fmt.Errorf("pitch %q has a goal scope with %d goals, not one", p.Slug, len(p.Scope.Goals))
		}
		if len(p.Scope.Goals) == 0 {
			return PitchOutput{}, fmt.Errorf("pitch %q carries no goal in its scope", p.Slug)
		}
		for _, ev := range p.Evidence {
			if strings.TrimSpace(ev.Ref) == "" {
				return PitchOutput{}, fmt.Errorf("pitch %q has an evidence entry with no ref", p.Slug)
			}
		}
		if slugs[p.Slug] {
			continue // the same idea pitched twice in one session is one pitch
		}
		slugs[p.Slug] = true
		out = append(out, p)
	}
	answered := make([]PitchAnswerSpec, 0, len(answers))
	for _, a := range answers {
		a.Pitch, a.Text = strings.TrimSpace(a.Pitch), strings.TrimSpace(a.Text)
		if a.Pitch == "" || a.Text == "" {
			continue
		}
		answered = append(answered, a)
	}
	return PitchOutput{Pitches: out, Answers: answered}, nil
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
	// ContextDir, ContextFiles and Project as on EvaluateInput.
	ContextDir   string
	ContextFiles []string
	Project      string
	// MeshContext says the session has the built-in mesh-context server
	// (tools.Builtin) and its mesh_* tools for context on demand.
	MeshContext bool
	// Services as on PromptInput.
	Services string
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
		if in.MeshContext {
			b.WriteString(" " + contextRule(in.ContextDir, in.Project))
		}
		b.WriteString("\n\n")
	} else if in.MeshContext {
		b.WriteString("# The context\n\n" + contextRule("", in.Project) + "\n\n")
	}
	servicesSection(&b, in.Services)
	b.WriteString("# The runbook\n\n")
	if in.MeshContext {
		fmt.Fprintf(&b, "Read the mesh-branch-rebase skill first: `mesh_document` with project %q, slug %q (or the same slug in project %q, if it is indexed there). If neither has it, these steps are the runbook:\n\n", RebaseRunbookProject, RebaseRunbookSlug, in.Project)
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
