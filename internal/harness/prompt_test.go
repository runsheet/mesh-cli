package harness

import (
	"strings"
	"testing"
)

func TestPlanPromptAcceptanceTheEvaluatorCanSee(t *testing.T) {
	p := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p"})
	for _, want := range []string{
		"3. Each item's brief says what to do and where, naming files and functions where it helps; its acceptance is what an evaluator can check — a test that passes, a command's output, a document with named sections — never a feeling.\n",
		"   An item's acceptance names only what the evaluator can see at evaluation time, on that item's branch, in that item's repository: tests, commands and their output, file contents. It never names a CI check on the item's own pull request (the pull request opens only after evaluation passes), a deploy, or a file in another repository. Work like that becomes its own item in the right repo, or goes in the goal's acceptance.\n",
		"4. depends_on names earlier seqs",
		"9. End your final message with the graph as a JSON object in a ```json fenced block, and nothing after it. Its `questions` is empty unless a clause of the goal is one no pull request can satisfy (rule 3); each is {\"clause\": \"…\", \"question\": \"…\"}, for the person to answer:\n",
		"```json\n{\"items\": [{\"seq\": 1, \"title\": \"…\", \"brief\": \"…\", \"acceptance\": \"…\", \"depends_on\": [], \"repo\": \"owner/name\", \"base_ref\": \"main\", \"model\": \"sonnet\", \"model_reason\": \"…\"}, {\"seq\": 2, \"title\": \"…\", \"brief\": \"…\", \"acceptance\": \"…\", \"depends_on\": [{\"seq\": 1, \"because\": \"uses the … item 1 adds\"}], \"repo\": \"owner/name\", \"base_ref\": \"main\", \"model\": \"sonnet\", \"model_reason\": \"…\"}], \"questions\": []}\n```\n",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plan prompt lacks %q", want)
		}
	}
}

// ADR-020's planner half: an item's acceptance names only what a PR can
// change, a host reading is a `Measure:` item, and a clause that cannot
// be phrased so is a question in the graph's `questions` — in rule 3 of
// every plan and in the re-ask.
func TestPlanPromptHostRule(t *testing.T) {
	first := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p"})
	reask := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p", Rejected: []string{"item 5 (Shard): a reading of the host"}})
	for name, p := range map[string]string{"first": first, "re-ask": reask} {
		for _, want := range []string{
			"   An item's acceptance names only what a pull request can change: code, tests, docs, a script that reproduces a reading, and the reading recorded honestly. A measurement of the host — load, wall time, shard durations, a p90 over a day — is never an item's merge gate: put it in the goal doc's After section, or in its own item titled `Measure: …` whose acceptance is that the script runs and its reading is recorded, whatever the number.",
			"A clause you cannot phrase that way is a question for the person, not an item: write it beside `items` as `\"questions\": [{\"clause\": \"…\", \"question\": \"…\"}]`.\n",
			"\"questions\": []}\n```\n",
		} {
			if !strings.Contains(p, want) {
				t.Errorf("%s plan prompt lacks %q", name, want)
			}
		}
	}
	if !strings.Contains(reask, "goes in the goal's acceptance. "+planHostRule+" "+planSplitRule+" Write the whole graph again") {
		t.Errorf("the re-ask does not restate the host rule:\n%s", reask)
	}
}

// ADR-021's planner half: a lineage's attempts shrink — rule 3 of every
// plan and the re-ask say to partition a failed item's acceptance into
// two or more items, never to recreate it at the same acceptance, and
// that `atomic` exempts once.
func TestPlanPromptSplitRule(t *testing.T) {
	first := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p"})
	reask := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p", Rejected: []string{"item 8 (Retry of item 1): the same acceptance as failed item 1 (lineage 1)"}})
	rule3 := func(p string) string {
		i, j := strings.Index(p, "\n3. "), strings.Index(p, "\n4. ")
		if i < 0 || j < i {
			t.Fatalf("no rule 3:\n%s", p)
		}
		return p[i : j+1]
	}
	reaskPara := func(p string) string {
		i := strings.Index(p, "## Your last answer was flagged")
		if i < 0 {
			t.Fatalf("no re-ask:\n%s", p)
		}
		j := strings.Index(p[i:], "this is the last ask.")
		if j < 0 {
			t.Fatalf("re-ask does not end:\n%s", p[i:])
		}
		return p[i : i+j]
	}
	for name, text := range map[string]string{"rule 3": rule3(first), "re-ask rule 3": rule3(reask), "re-ask": reaskPara(reask)} {
		for _, want := range []string{"partition", "two or more items", "atomic", "same acceptance", "because"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks %q:\n%s", name, want, text)
			}
		}
	}
	if !strings.Contains(rule3(first), "   "+planSplitRule+"\n") {
		t.Errorf("rule 3 does not carry planSplitRule on its own line:\n%s", rule3(first))
	}
}

// An item's `atomic` reason is kept; without one it is empty.
func TestParsePlanAtomic(t *testing.T) {
	items, err := ParsePlan("```json\n" + `{"items": [{"seq": 1, "title": "Schema", "repo": "o/r", "acceptance": "the migration runs", "atomic": "one migration, cannot be halved"}, {"seq": 2, "title": "Policy", "repo": "o/r", "depends_on": [{"seq": 1, "because": "uses the column item 1 adds"}]}]}` + "\n```")
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Atomic != "one migration, cannot be halved" {
		t.Errorf("atomic = %q, want the reason", items[0].Atomic)
	}
	if items[1].Atomic != "" {
		t.Errorf("an item without atomic has %q", items[1].Atomic)
	}
}

// A failed item of the plan before is rendered with its whole acceptance
// and its lineage, so a re-plan can partition it; a merged one's
// acceptance is not.
func TestPlanPromptExistingShowsFailedAcceptance(t *testing.T) {
	p := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p", NextSeq: 21, Existing: []ExistingItem{
		{Seq: 2, Title: "Console page", Status: "merged", PRURL: "https://github.com/o/r/pull/9", Acceptance: "the console lists the owner"},
		{Seq: 20, Title: "Re-land the owner model", Status: "failed", Note: "evaluation failed 3 times", Acceptance: "RegisterNode pins the id to NodeID(owner, slug);\ntransfersNode applies ownsAs to the source", LineageRootSeq: 1, LineageAttempt: 3},
	}})
	for _, want := range []string{
		"- 20. Re-land the owner model — failed: evaluation failed 3 times\n  lineage 1, attempt 3 of 3\n  Acceptance: RegisterNode pins the id to NodeID(owner, slug);\n  transfersNode applies ownsAs to the source\n",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plan prompt lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "the console lists the owner") {
		t.Errorf("a merged item's acceptance is rendered:\n%s", p)
	}
}

// A narrow re-plan names the item it replaces in its own section before
// the rules: title, acceptance and failed clauses, and asks for a split
// or one atomic item.
func TestPlanPromptReplacingItem(t *testing.T) {
	if p := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p"}); strings.Contains(p, "## Split this item") {
		t.Error("a plan replacing nothing has a split section")
	}
	item := ExistingItem{Seq: 1, Title: "Schema and policy: a Node has an owner", Status: "failed", Acceptance: "one row per node; TransferNode moves it", LineageRootSeq: 1, LineageAttempt: 2}
	p := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p", Existing: []ExistingItem{item}, NextSeq: 2, Replacing: &item, ReplacingClauses: []FailedClause{
		{Clause: "RegisterNode pins the aggregate id to NodeID(owner, slug)", Satisfy: "a test registering twice"},
		{Clause: "transfersNode applies ownsAs to the source"},
	}})
	i, j := strings.Index(p, "## Split this item"), strings.Index(p, "# Rules")
	if i < 0 || j < i {
		t.Fatalf("no split section before the rules:\n%s", p)
	}
	sec := p[i:j]
	for _, want := range []string{
		"\"Schema and policy: a Node has an owner\"",
		"Its acceptance:\n\none row per node; TransferNode moves it\n",
		"- RegisterNode pins the aggregate id to NodeID(owner, slug) — would be satisfied by: a test registering twice\n",
		"- transfersNode applies ownsAs to the source\n",
		"two or more items",
		"partitions",
		"one item carrying `\"atomic\": \"<reason>\"`",
		"lineage 1, attempt 2 of 3",
	} {
		if !strings.Contains(sec, want) {
			t.Errorf("split section lacks %q:\n%s", want, sec)
		}
	}
	if strings.Index(sec, "RegisterNode pins") > strings.Index(sec, "transfersNode applies") {
		t.Error("failed clauses are not oldest first")
	}
}

// The questions beside the items are read; one missing a half is dropped,
// and a graph with none has none.
func TestParsePlanQuestions(t *testing.T) {
	text := "Planned.\n\n```json\n{\"items\": [{\"seq\": 1, \"title\": \"T\", \"repo\": \"o/r\"}], \"questions\": [{\"clause\": \" the mini's load p90 under 1.5 × cores \", \"question\": \"Record it as not met?\"}, {\"clause\": \"half\"}]}\n```"
	got := ParsePlanQuestions(text)
	if len(got) != 1 || got[0] != (PlanQuestion{Clause: "the mini's load p90 under 1.5 × cores", Question: "Record it as not met?"}) {
		t.Fatalf("questions = %+v", got)
	}
	if q := ParsePlanQuestions("```json\n{\"items\": [{\"seq\": 1, \"title\": \"T\", \"repo\": \"o/r\"}]}\n```"); q != nil {
		t.Fatalf("no questions read as %+v", q)
	}
}

// The planner chooses each item's model by the rubric, and the graph is
// still the last thing it writes.
// A research session served Brave and Perplexity is told, beside
// mesh-context, when to reach for them — a source for a Scout note or a
// pitch's evidence — and cites what it found; with no tools served, no
// section at all (goal the-director-and-the-planner-get-their-tools).
func TestResearchPromptNamesItsTools(t *testing.T) {
	p := ResearchPrompt(ResearchInput{Org: "o", Project: "p", Direction: "d", Room: 1,
		Tools: []ToolHint{{Name: "brave"}, {Name: "perplexity"}}})
	for _, want := range []string{"# Your tools", "brave", "perplexity", "source", "evidence", "ref"} {
		if !strings.Contains(p, want) {
			t.Errorf("research prompt lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "served, never called") {
		t.Error("research prompt names the use-note's own phrase, which belongs on the item, not the prompt")
	}
	none := ResearchPrompt(ResearchInput{Org: "o", Project: "p", Direction: "d", Room: 1})
	if strings.Contains(none, "# Your tools") {
		t.Errorf("research prompt with no tools served shows the heading anyway:\n%s", none)
	}
}

// The planner gets the same section, shorter: a known search tool's use
// is search before naming a library or a version the brief relies on.
func TestPlanPromptNamesItsTools(t *testing.T) {
	p := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p",
		Tools: []ToolHint{{Name: "brave"}, {Name: "perplexity"}}})
	for _, want := range []string{"# Your tools", "brave", "perplexity", "library", "version"} {
		if !strings.Contains(p, want) {
			t.Errorf("plan prompt lacks %q:\n%s", want, p)
		}
	}
	none := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p"})
	if strings.Contains(none, "# Your tools") {
		t.Errorf("plan prompt with no tools served shows the heading anyway:\n%s", none)
	}
}

func TestPlanPromptModelRubric(t *testing.T) {
	p := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p"})
	for _, want := range []string{
		"8. Each item names the model its session runs on, as `model`, with a one-line `model_reason`:\n",
		"   - `haiku` for mechanical items: docs-only, a version bump, a rename.\n",
		"   - `sonnet` for a well-specified change in one package with tests to copy.\n",
		"   - `opus` for anything cross-cutting, concurrency, migrations, security, a failing item's retry, or when unsure.\n",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plan prompt lacks %q", want)
		}
	}
	if !strings.HasSuffix(p, "\"model\": \"sonnet\", \"model_reason\": \"…\"}], \"questions\": []}\n```\n") {
		t.Error("the plan prompt no longer ends with the JSON example")
	}
}

// The scorecard's per-repo, per-model evidence renders as a table under
// rule 8, with the under-0.5 rule; with none, no table (item 5).
func TestPlanPromptEvidence(t *testing.T) {
	p := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p", Evidence: []ModelEvidence{
		{Repo: "acme/console", Model: "sonnet", FirstPass: 0.3, Items: 12, CostPerItemCents: 450},
		{Repo: "acme/api", Model: "opus", FirstPass: 0.86, Items: 20, CostPerItemCents: 103},
	}})
	for _, want := range []string{
		"acme/console", "acme/api", "sonnet", "opus", "0.30", "0.86", "12", "20", "$4.50", "$1.03",
		"under 0.5",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plan prompt with evidence lacks %q:\n%s", want, p)
		}
	}
	if !strings.Contains(p, "|") {
		t.Error("plan prompt with evidence renders no table")
	}

	none := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p"})
	if strings.Contains(none, "|") {
		t.Error("plan prompt with no evidence renders a table")
	}
	if !strings.Contains(none, "under 0.5") {
		t.Error("plan prompt still states the under-0.5 rule with no evidence")
	}
}

// The dependency rubric is in the rules of a first plan, a replan and
// the re-ask alike (trd §5 phase 2).
func TestPlanPromptDependencyRubric(t *testing.T) {
	for name, in := range map[string]PlanInput{
		"first":  {GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p"},
		"replan": {GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p", NextSeq: 3, Existing: []ExistingItem{{Seq: 1, Title: "A", Status: "merged"}, {Seq: 2, Title: "B", Status: "failed"}}},
		"re-ask": {GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p", Rejected: []string{"item 1 (Ship): a deploy"}},
	} {
		p := PlanPrompt(in)
		for _, want := range []string{
			"4. depends_on names earlier seqs whose branches must be merged first; items with no dependency between them run in parallel.\n   " + planDependencyRule + "\n",
			"An item depends on another only when it uses something that item creates or changes — a type, command, field, function, file or endpoint — and could not be built against main without it.",
			"Sharing a repository or touching the same file is not a dependency",
			"Order by importance is priority, not dependency.",
			"Prefer a wide, shallow graph",
			"A final docs item depends only on the items whose results it records.",
			"Each dependency carries a one-line `because` naming what is used",
			"a dependency without one is dropped.",
			`"depends_on": [{"seq": 1, "because": `,
		} {
			if !strings.Contains(p, want) {
				t.Errorf("%s: plan prompt lacks %q", name, want)
			}
		}
	}
}

// A depends_on entry is {"seq", "because"}, or a bare seq with no because.
func TestParsePlanDependencyShapes(t *testing.T) {
	items, err := ParsePlan("```json\n{\"items\": [" +
		`{"seq": 1, "title": "A", "repo": "o/r"},` +
		`{"seq": 2, "title": "B", "repo": "o/r"},` +
		`{"seq": 3, "title": "C", "repo": "o/r", "depends_on": [{"seq": 1, "because": "uses the Foo type item 1 adds"}, 2]}` +
		"]}\n```")
	if err != nil {
		t.Fatal(err)
	}
	want := []PlannedDep{{Seq: 1, Because: "uses the Foo type item 1 adds"}, {Seq: 2}}
	if got := items[2].DependsOn; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("depends_on = %+v, want %+v", got, want)
	}
	if _, err := ParsePlan(`[{"seq": 1, "title": "A", "repo": "o/r", "depends_on": [{"seq": 4, "because": "x"}]}]`); err == nil {
		t.Error("a dependency on no item parsed")
	}
	if _, err := ParsePlan(`[{"seq": 2, "title": "B", "repo": "o/r", "depends_on": [{"seq": 1, "because": "x"}]}]`, 1); err != nil {
		t.Errorf("a dependency on a merged seq: %v", err)
	}
}

func TestEvaluatePromptUnverifiableClause(t *testing.T) {
	p := EvaluatePrompt(EvaluateInput{GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Repo: "o/r", Branch: "b", BaseRef: "main"})
	for _, want := range []string{
		"5. A clause you cannot check at evaluation time — a CI check on this item's pull request (it opens only after you pass), a deploy, a file in another repository — is reported in your review as unverifiable, not failed, and the item is not failed on that clause alone. Every clause you can check you still check.\n",
		"7. End your final message with a short review — what holds, what does not, what is unverifiable, and exactly what a retry must fix. When you fail it, write one line per failed acceptance clause, `FAILED: <the clause, as the acceptance words it> || SATISFY: <what would satisfy it>`, above the verdict; a clause that does not say what would satisfy it is dropped, so write both halves. Then one last line that is either `VERDICT: PASS` or `VERDICT: FAIL`. Pass only when every clause of the acceptance you can check holds.\n",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("evaluate prompt lacks %q", want)
		}
	}
	if strings.Contains(p, "Pass only when every clause of the acceptance holds") {
		t.Error("evaluate prompt still passes only when every clause holds, checkable or not")
	}
}

// A session with mesh-context's tools is told to use mesh_document and
// mesh_search, not the prologue_* names — in both shapes of the
// evaluator's context section (with and without a ContextDir).
func TestEvaluatePromptContextTools(t *testing.T) {
	withDir := EvaluatePrompt(EvaluateInput{GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Repo: "o/r", Branch: "b", BaseRef: "main",
		ContextDir: ".mesh/context", Project: "acme/mesh", MeshContext: true})
	noDir := EvaluatePrompt(EvaluateInput{GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Repo: "o/r", Branch: "b", BaseRef: "main",
		Project: "acme/mesh", MeshContext: true})
	for _, p := range []string{withDir, noDir} {
		// the slug from the index, never a file name: `adr-adr-….md`
		// taken as a slug is `not found` (goal
		// evaluation-reports-no-verdict)
		for _, want := range []string{"mesh_document", "mesh_search", "(project \"acme/mesh\", the document's slug — never a context file's name: a file name is not a slug, `instructions-acme-widgets-agents.md` is slug `agents`)"} {
			if !strings.Contains(p, want) {
				t.Errorf("evaluate prompt lacks %q:\n%s", want, p)
			}
		}
		if strings.Contains(p, "prologue_") {
			t.Errorf("evaluate prompt still names a prologue_ tool:\n%s", p)
		}
	}
}

// The planner is told to use mesh_document and mesh_search when the
// session has mesh-context's tools.
func TestPlanPromptContextTools(t *testing.T) {
	p := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p", ContextDir: ".mesh/context", MeshContext: true})
	for _, want := range []string{"mesh_document", "mesh_search"} {
		if !strings.Contains(p, want) {
			t.Errorf("plan prompt lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "prologue_") {
		t.Errorf("plan prompt still names a prologue_ tool:\n%s", p)
	}
}

func TestPlanPromptRejected(t *testing.T) {
	if p := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p"}); strings.Contains(p, "Your last answer") {
		t.Error("a first ask lists no rejected items")
	}
	p := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p", Rejected: []string{"item 1 (Ship): a check on its own pull request (`deploy / verify`)"}})
	for _, want := range []string{
		"## Your last answer was flagged\n\nThe node checked your graph before handing it in and flagged these items — an acceptance naming what no evaluator can see or a reading of the host as a merge gate, or a model the scorecard says does not work on its repo:\n\n- item 1 (Ship): a check on its own pull request (`deploy / verify`)\n\n",
		"Work like that becomes its own item in the right repo, or goes in the goal's acceptance. " + planHostRule + " " + planSplitRule + " Write the whole graph again with those acceptances and models rewritten; this is the last ask.\n",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plan prompt lacks %q", want)
		}
	}
}

// The services line is its own section before the rules, in the build,
// evaluate and rebase prompts; none is written when there are none, and
// no contract line moves.
func TestPromptsCarryTheServicesLine(t *testing.T) {
	const line = "The services this repository declares run beside you and are ready on 127.0.0.1 (also listed in `$HOME/.mesh-services`): postgres (postgres:16) at 127.0.0.1:5432."
	build := BuildPrompt(PromptInput{GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Repo: "o/r", Branch: "b", BaseRef: "main", GoalAcceptance: "tests pass", Services: line})
	if want := "## Acceptance (the goal)\n\ntests pass\n\n# Check main first\n\n"; !strings.Contains(build, want) {
		t.Errorf("build prompt lacks\n%s\n---\n%s", want, build)
	}
	if want := "If only part of it is there, build the rest.\n\n# Services\n\n" + line + "\n\n# Rules\n\n1. Read the repository's own instructions"; !strings.Contains(build, want) {
		t.Errorf("build prompt lacks\n%s\n---\n%s", want, build)
	}
	eval := EvaluatePrompt(EvaluateInput{GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Repo: "o/r", Branch: "b", BaseRef: "main", Services: line})
	if want := "The diff is empty: the branch changed nothing.\n\n# Services\n\n" + line + "\n\n# Rules\n\n1. Judge"; !strings.Contains(eval, want) {
		t.Errorf("evaluate prompt lacks\n%s\n---\n%s", want, eval)
	}
	if !strings.HasSuffix(strings.TrimSpace(eval), "Pass only when every clause of the acceptance you can check holds.") || !strings.Contains(eval, "`"+VerdictPass+"` or `"+VerdictFail+"`") {
		t.Error("the verdict contract line moved")
	}
	rebase := RebasePrompt(RebaseInput{GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Repo: "o/r", Branch: "b", BaseRef: "main", Services: line})
	if want := "# Services\n\n" + line + "\n\n# The runbook\n\n"; !strings.Contains(rebase, want) {
		t.Errorf("rebase prompt lacks\n%s\n---\n%s", want, rebase)
	}
	for _, p := range []string{
		BuildPrompt(PromptInput{Title: "x"}),
		EvaluatePrompt(EvaluateInput{Title: "x"}),
		RebasePrompt(RebaseInput{Title: "x"}),
	} {
		if strings.Contains(p, "# Services") {
			t.Errorf("no services, no section:\n%s", p)
		}
	}
}

// Every prompt that carries mesh-context's tools tells the session to
// search before it reads, and none claims the index lists every
// document — the inverted default (goal
// context-is-fetched-not-shipped): the context directory holds only
// the session's repos' instructions and its own goal, everything else
// comes through mesh_search and mesh_document.
func TestPromptsCarrySearchFirst(t *testing.T) {
	const phrase = "search before you read"
	build := BuildPrompt(PromptInput{GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Brief: "do it", Repo: "o/r", Branch: "b", BaseRef: "main", ContextDir: ".mesh/context", Project: "acme/mesh", MeshContext: true})
	eval := EvaluatePrompt(EvaluateInput{GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Repo: "o/r", Branch: "b", BaseRef: "main", ContextDir: ".mesh/context", Project: "acme/mesh", MeshContext: true})
	plan := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p", ContextDir: ".mesh/context", MeshContext: true})
	rebase := RebasePrompt(RebaseInput{GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Repo: "o/r", Branch: "b", BaseRef: "main", ContextDir: ".mesh/context", Project: "acme/mesh", MeshContext: true})
	research := ResearchPrompt(ResearchInput{Org: "acme", Project: "mesh", ContextDir: ".mesh/context", MeshContext: true})
	for name, p := range map[string]string{"build": build, "evaluate": eval, "plan": plan, "rebase": rebase, "research": research} {
		if !strings.Contains(p, phrase) {
			t.Errorf("%s prompt lacks %q:\n%s", name, phrase, p)
		}
		if !strings.Contains(p, "mesh_search") {
			t.Errorf("%s prompt lacks %q:\n%s", name, "mesh_search", p)
		}
		if strings.Contains(p, "lists every document") {
			t.Errorf("%s prompt still claims the index lists every document:\n%s", name, p)
		}
	}
}

// The plan prompt tells the planner to name, by slug, the two or
// three documents the builder must fetch with mesh_document — found
// with mesh_search while planning.
func TestPlanPromptFetchList(t *testing.T) {
	p := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p", ContextDir: ".mesh/context", MeshContext: true})
	for _, want := range []string{"mesh_document", "slug", "two or three documents"} {
		if !strings.Contains(p, want) {
			t.Errorf("plan prompt lacks %q:\n%s", want, p)
		}
	}
}

// Every prompt given the built-in server names it mesh-context and
// never sends the session to its old tool prefix (goal
// builtin-is-mesh-context).
func TestPromptsNameMeshContext(t *testing.T) {
	prompts := map[string]string{
		"build":    BuildPrompt(PromptInput{GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Brief: "do it", Repo: "o/r", Branch: "b", BaseRef: "main", ContextDir: ".mesh/context", Project: "acme/mesh", MeshContext: true}),
		"evaluate": EvaluatePrompt(EvaluateInput{GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Repo: "o/r", Branch: "b", BaseRef: "main", ContextDir: ".mesh/context", Project: "acme/mesh", MeshContext: true}),
		"plan":     PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p", ContextDir: ".mesh/context", MeshContext: true}),
		"research": ResearchPrompt(ResearchInput{Org: "acme", Project: "mesh", ContextDir: ".mesh/context", MeshContext: true}),
		"rebase":   RebasePrompt(RebaseInput{GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Repo: "o/r", Branch: "b", BaseRef: "main", ContextDir: ".mesh/context", Project: "acme/mesh", MeshContext: true}),
	}
	for name, p := range prompts {
		if !strings.Contains(p, "mesh-context") {
			t.Errorf("%s prompt does not name mesh-context:\n%s", name, p)
		}
		if strings.Contains(p, "mcp__prologue__") {
			t.Errorf("%s prompt still names mcp__prologue__:\n%s", name, p)
		}
	}
}
