package harness

import (
	"encoding/json"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/tools"
)

func TestPlanPromptAcceptanceTheEvaluatorCanSee(t *testing.T) {
	p := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p"})
	for _, want := range []string{
		"3. Each item's brief says what to do and where, naming files and functions where it helps; its acceptance is what an evaluator can check — a test that passes, a command's output, a document with named sections — never a feeling.\n",
		"   An item's acceptance names only what the evaluator can see at evaluation time, on that item's branch, in that item's repository: tests, commands and their output, file contents. It never names a CI check on the item's own pull request (the pull request opens only after evaluation passes), a deploy, or a file in another repository. Work like that becomes its own item in the right repo, or goes in the goal's acceptance.\n",
		"4. depends_on names earlier seqs",
		"`plan.items` is the graph: each item `{\"seq\": 1, \"title\": \"…\", \"brief\": \"…\", \"acceptance\": \"…\", \"acceptance_checks\": [{\"kind\": \"test\", \"check\": \"go test ./internal/<pkg>/\", \"expected\": \"passes\"}], \"depends_on\": [{\"seq\": 1, \"because\": \"uses the … item 1 adds\"}], \"repo\": \"owner/name\", \"base_ref\": \"main\", \"model\": \"sonnet\", \"model_reason\": \"…\", \"files\": [\"…\"]}`",
		"`plan.questions`, one `{\"clause\", \"question\"}` per clause no pull request can satisfy",
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
			"`plan.questions`, one `{\"clause\", \"question\"}` per clause no pull request can satisfy",
		} {
			if !strings.Contains(p, want) {
				t.Errorf("%s plan prompt lacks %q", name, want)
			}
		}
	}
	if !strings.Contains(reask, "goes in the goal's acceptance. "+planChecksRule+" "+planHostRule+" "+planSplitRule+" "+planSizeRule+" "+planCloseOutRule+" "+planCutOverRule+" "+planRepoRule+" "+planFilesRule+" "+planSharedDocsRule+" Write the whole graph again") {
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
	items, err := DecodePlan(graphOf(t, `{"items": [{"seq": 1, "title": "Schema", "repo": "o/r", "acceptance": "the migration runs", "atomic": "one migration, cannot be halved"}, {"seq": 2, "title": "Policy", "repo": "o/r", "depends_on": [{"seq": 1, "because": "uses the column item 1 adds"}]}]}`))
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

// ParsePlan keeps an item's `files` and `writes_shared_docs`; an item
// naming neither has an empty Files and a false WritesSharedDocs
// (goal items-that-share-a-file-are-planned-in-a-line, item 1).
func TestParsePlanFilesAndWritesSharedDocs(t *testing.T) {
	items, err := DecodePlan(graphOf(t, `{"items": [
		{"seq": 1, "title": "Build the thing", "repo": "o/r", "files": ["internal/node/plan_check.go", "internal/node/plan_check_test.go"]},
		{"seq": 2, "title": "Write the docs", "repo": "o/r", "writes_shared_docs": true, "depends_on": [{"seq": 1, "because": "folds in item 1's notes"}]}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"internal/node/plan_check.go", "internal/node/plan_check_test.go"}; !slicesEqual(items[0].Files, want) {
		t.Errorf("files = %v, want %v", items[0].Files, want)
	}
	if items[0].WritesSharedDocs {
		t.Error("item 1 does not write the shared docs")
	}
	if len(items[1].Files) != 0 {
		t.Errorf("item 2 names no files, got %v", items[1].Files)
	}
	if !items[1].WritesSharedDocs {
		t.Error("item 2 writes the shared docs")
	}
}

// graphOf is a plan's `plan` object as mesh_result takes it, from its
// JSON text.
func graphOf(t *testing.T, text string) map[string]any {
	t.Helper()
	var graph map[string]any
	if err := json.Unmarshal([]byte(text), &graph); err != nil {
		t.Fatal(err)
	}
	return graph
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ExpectedFiles falls back to the paths named in the item's brief when
// it names no files of its own, deduplicated and sorted; a bare
// AGENTS.md is found too, with no path.
func TestExpectedFilesFallsBackToTheBrief(t *testing.T) {
	sp := PlannedSpec{Brief: "Change `internal/node/plan_check.go` and its test `internal/node/plan_check_test.go`, and mention `internal/node/plan_check.go` again; also update AGENTS.md."}
	want := []string{"AGENTS.md", "internal/node/plan_check.go", "internal/node/plan_check_test.go"}
	if got := ExpectedFiles(sp); !slicesEqual(got, want) {
		t.Errorf("ExpectedFiles = %v, want %v", got, want)
	}
	withFiles := PlannedSpec{Files: []string{"b.go", "a.go"}, Brief: "mentions `c.go` too"}
	if got := ExpectedFiles(withFiles); !slicesEqual(got, []string{"a.go", "b.go"}) {
		t.Errorf("ExpectedFiles with Files set = %v, want Files alone, sorted", got)
	}
}

// IsDocumentPath is true for a document — markdown, a docs/ or specs/
// path, the bare AGENTS.md/README.md — and false for code, generated or
// not.
func TestIsDocumentPath(t *testing.T) {
	for path, want := range map[string]bool{
		"AGENTS.md":                        true,
		"README.md":                        true,
		"docs/adr/adr-023.md":              true,
		"specs/trd/6a.md":                  true,
		"internal/node/plan_check.go":      false,
		"platform/loomgen/registry_gen.go": false,
	} {
		if got := IsDocumentPath(path); got != want {
			t.Errorf("IsDocumentPath(%q) = %v, want %v", path, got, want)
		}
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

// An item's `precondition` is kept, like `atomic`; without one it is
// empty (ADR-023, planCutOverRule).
func TestParsePlanPrecondition(t *testing.T) {
	items, err := DecodePlan(graphOf(t, `{"items": [{"seq": 1, "title": "Ship", "repo": "o/r", "acceptance": "the release is cut", "precondition": "a person confirms the release in production"}, {"seq": 2, "title": "Other", "repo": "o/r"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Precondition != "a person confirms the release in production" {
		t.Errorf("precondition = %q, want the clause", items[0].Precondition)
	}
	if items[1].Precondition != "" {
		t.Errorf("an item without precondition has %q", items[1].Precondition)
	}
}

// ParsePlanSplit reads a `split` answer of goal drafts, each with its
// slug, title, statement, acceptance and repo (ADR-023, planSizeRule);
// an `items` answer with no `split` is ok=false, not an empty split.
func TestParsePlanSplit(t *testing.T) {
	drafts, ok := DecodePlanSplit(graphOf(t, `{"split": [{"slug": "a-goal", "title": "A goal", "statement": "A is true", "acceptance": "a test proves it", "repo": "o/r"}, {"slug": "b-goal", "title": "B goal", "statement": "B is true", "acceptance": "a test proves it", "repo": "o/r2"}]}`))
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if len(drafts) != 2 {
		t.Fatalf("drafts = %+v, want 2", drafts)
	}
	if drafts[0] != (GoalDraft{Slug: "a-goal", Title: "A goal", Statement: "A is true", Acceptance: "a test proves it", Repo: "o/r"}) {
		t.Errorf("draft 0 = %+v", drafts[0])
	}
	if drafts[1].Slug != "b-goal" || drafts[1].Repo != "o/r2" {
		t.Errorf("draft 1 = %+v", drafts[1])
	}
	if got, ok := DecodePlanSplit(graphOf(t, `{"items": [{"seq": 1, "title": "T", "repo": "o/r"}]}`)); ok {
		t.Errorf("an items answer read as a split: %+v", got)
	}
}

// ParsePlanCheckpoints reads `checkpoints` beside `items`, like
// ParsePlanQuestions; a graph with none has none (ADR-023,
// planCutOverRule).
func TestParsePlanCheckpoints(t *testing.T) {
	got := DecodePlanCheckpoints(graphOf(t, `{"items": [{"seq": 1, "title": "T", "repo": "o/r"}], "checkpoints": ["pr"]}`))
	if len(got) != 1 || got[0] != "pr" {
		t.Fatalf("checkpoints = %+v, want [pr]", got)
	}
	none := DecodePlanCheckpoints(graphOf(t, `{"items": [{"seq": 1, "title": "T", "repo": "o/r"}]}`))
	if none != nil {
		t.Fatalf("no checkpoints read as %+v", none)
	}
}

// The questions beside the items are read; one missing a half is dropped,
// and a graph with none has none.
func TestParsePlanQuestions(t *testing.T) {
	got := DecodePlanQuestions(graphOf(t, `{"items": [{"seq": 1, "title": "T", "repo": "o/r"}], "questions": [{"clause": " the mini's load p90 under 1.5 × cores ", "question": "Record it as not met?"}, {"clause": "half"}]}`))
	if len(got) != 1 || got[0] != (PlanQuestion{Clause: "the mini's load p90 under 1.5 × cores", Question: "Record it as not met?"}) {
		t.Fatalf("questions = %+v", got)
	}
	if q := DecodePlanQuestions(graphOf(t, `{"items": [{"seq": 1, "title": "T", "repo": "o/r"}]}`)); q != nil {
		t.Fatalf("no questions read as %+v", q)
	}
}

// The planner chooses each item's model by the rubric, and the graph is
// still the last thing it writes.
// A research session served Brave and Perplexity is told, beside
// runsheet, when to reach for them — a source for a Scout note or a
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
	// the result's fields follow the finish (ADR-041)
	if !strings.HasSuffix(p, "each at most 8 items and one repo.\n") || !strings.Contains(p, "\n`plan.items` is the graph") {
		t.Errorf("the plan prompt does not end with the plan's fields:\n%s", p)
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
			"Sharing a repository is not a dependency; sharing a code file is, and the node adds it when the planner did not; sharing a document is not.",
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
	items, err := DecodePlan(graphOf(t, `{"items": [`+
		`{"seq": 1, "title": "A", "repo": "o/r"},`+
		`{"seq": 2, "title": "B", "repo": "o/r"},`+
		`{"seq": 3, "title": "C", "repo": "o/r", "depends_on": [{"seq": 1, "because": "uses the Foo type item 1 adds"}, 2]}`+
		`]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []PlannedDep{{Seq: 1, Because: "uses the Foo type item 1 adds"}, {Seq: 2}}
	if got := items[2].DependsOn; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("depends_on = %+v, want %+v", got, want)
	}
	if _, err := DecodePlan(graphOf(t, `{"items": [{"seq": 1, "title": "A", "repo": "o/r", "depends_on": [{"seq": 4, "because": "x"}]}]}`)); err == nil {
		t.Error("a dependency on no item parsed")
	}
	if _, err := DecodePlan(graphOf(t, `{"items": [{"seq": 2, "title": "B", "repo": "o/r", "depends_on": [{"seq": 1, "because": "x"}]}]}`), 1); err != nil {
		t.Errorf("a dependency on a merged seq: %v", err)
	}
	// a client that sent the items as a JSON string
	if items, err := DecodePlan(map[string]any{"items": `[{"seq": 1, "title": "A", "repo": "o/r"}]`}); err != nil || len(items) != 1 {
		t.Errorf("items as a string: %v %v", items, err)
	}
}

func TestEvaluatePromptUnverifiableClause(t *testing.T) {
	p := EvaluatePrompt(EvaluateInput{GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Repo: "o/r", Branch: "b", BaseRef: "main"})
	for _, want := range []string{
		"5. A clause you cannot check at evaluation time — a CI check on this item's pull request (it opens only after you pass), a deploy, a file in another repository — is reported in your review as unverifiable, not failed, and the item is not failed on that clause alone. Every clause you can check you still check.\n",
		"`verdict.notes` is your review: what holds, what does not, what is unverifiable, and exactly what a retry must fix. `verdict.failedClauses` is one `{clause, satisfy}` per acceptance clause you fail",
		"Pass only when every clause of the acceptance you can check holds.\n",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("evaluate prompt lacks %q", want)
		}
	}
	if strings.Contains(p, "Pass only when every clause of the acceptance holds") {
		t.Error("evaluate prompt still passes only when every clause holds, checkable or not")
	}
}

// A session with runsheet's tools is told to use mesh_document and
// mesh_search, not the alias's <alias>_* names (tools.BuiltinAlias) — in both shapes of the
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
		if strings.Contains(p, tools.BuiltinAlias()+"_") {
			t.Errorf("evaluate prompt still names a %s_ tool:\n%s", tools.BuiltinAlias(), p)
		}
	}
}

// The planner is told to use mesh_document and mesh_search when the
// session has runsheet's tools.
func TestPlanPromptContextTools(t *testing.T) {
	p := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p", ContextDir: ".mesh/context", MeshContext: true})
	for _, want := range []string{"mesh_document", "mesh_search"} {
		if !strings.Contains(p, want) {
			t.Errorf("plan prompt lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, tools.BuiltinAlias()+"_") {
		t.Errorf("plan prompt still names a %s_ tool:\n%s", tools.BuiltinAlias(), p)
	}
}

func TestPlanPromptRejected(t *testing.T) {
	if p := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p"}); strings.Contains(p, "Your last answer") {
		t.Error("a first ask lists no rejected items")
	}
	p := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p", Rejected: []string{"item 1 (Ship): a check on its own pull request (`deploy / verify`)"}})
	for _, want := range []string{
		"## Your last answer was flagged\n\nThe node checked your graph before handing it in and flagged these items — an acceptance naming what no evaluator can see or a reading of the host as a merge gate, an item with no `acceptance_checks`, or a model the scorecard says does not work on its repo:\n\n- item 1 (Ship): a check on its own pull request (`deploy / verify`)\n\n",
		"Work like that becomes its own item in the right repo, or goes in the goal's acceptance. " + planChecksRule + " " + planHostRule + " " + planSplitRule + " " + planSizeRule + " " + planCloseOutRule + " " + planCutOverRule + " " + planRepoRule + " " + planFilesRule + " " + planSharedDocsRule + " Write the whole graph again with those acceptances and models rewritten; this is the last ask.\n",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plan prompt lacks %q", want)
		}
	}
}

// ADR-023's planner half: rule 3 and the re-ask carry the four rules —
// size, close-out, cut-over, repo — each on its own line after
// planSplitRule; a PlanInput naming MaxItems renders it in rule 2, and
// an over-cap re-ask asks for `split`, not `items`.
func TestPlanPromptFourRules(t *testing.T) {
	first := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p"})
	reask := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p", Rejected: []string{"the plan has 12 items; the project's cap is 8"}})
	rule3 := func(p string) string {
		i, j := strings.Index(p, "\n3. "), strings.Index(p, "\n4. ")
		if i < 0 || j < i {
			t.Fatalf("no rule 3:\n%s", p)
		}
		return p[i : j+1]
	}
	for name, text := range map[string]string{"rule 3": rule3(first), "re-ask rule 3": rule3(reask)} {
		for _, rule := range []string{planSplitRule, planSizeRule, planCloseOutRule, planCutOverRule, planRepoRule} {
			if !strings.Contains(text, rule) {
				t.Errorf("%s lacks a rule:\n%s", name, text)
			}
		}
		i := strings.Index(text, planSplitRule)
		j := strings.Index(text, planSizeRule)
		if i < 0 || j < i {
			t.Errorf("%s: planSizeRule does not follow planSplitRule:\n%s", name, text)
		}
	}
	if !strings.Contains(reask, planSplitRule+" "+planSizeRule+" "+planCloseOutRule+" "+planCutOverRule+" "+planRepoRule) {
		t.Errorf("the re-ask does not carry the four rules after planSplitRule:\n%s", reask)
	}
	capped := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p", MaxItems: 8})
	if !strings.Contains(rule3Rule2(t, capped), "Two to 8 items") {
		t.Errorf("rule 2 does not name the cap:\n%s", capped)
	}
	overCap := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p", MaxItems: 8, OverCap: true,
		Rejected: []string{"the plan has 12 items; the project's cap is 8"}})
	i := strings.Index(overCap, "## Your last answer was flagged")
	if i < 0 {
		t.Fatalf("no re-ask:\n%s", overCap)
	}
	if reaskText := overCap[i:]; !strings.Contains(reaskText, "answer `split`, not `items`") {
		t.Errorf("an over-cap re-ask does not ask for split, not items:\n%s", reaskText)
	} else if strings.Contains(reaskText, "Write the whole graph again") {
		t.Errorf("an over-cap re-ask still asks to rewrite the items graph:\n%s", reaskText)
	}
}

// The goal items-that-share-a-file-are-planned-in-a-line's two rules —
// files, and one writer of the shared documents — are in rule 3 of
// every plan and again in the re-ask, like the four rules above.
func TestPlanPromptFilesAndSharedDocsRules(t *testing.T) {
	first := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p"})
	reask := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p", Rejected: []string{"the plan has 12 items; the project's cap is 8"}})
	rule3 := func(p string) string {
		i, j := strings.Index(p, "\n3. "), strings.Index(p, "\n4. ")
		if i < 0 || j < i {
			t.Fatalf("no rule 3:\n%s", p)
		}
		return p[i : j+1]
	}
	for name, text := range map[string]string{"rule 3": rule3(first), "re-ask rule 3": rule3(reask)} {
		for _, rule := range []string{planFilesRule, planSharedDocsRule} {
			if !strings.Contains(text, rule) {
				t.Errorf("%s lacks a rule:\n%s", name, text)
			}
		}
		if i, j := strings.Index(text, planRepoRule), strings.Index(text, planFilesRule); i < 0 || j < i {
			t.Errorf("%s: planFilesRule does not follow planRepoRule:\n%s", name, text)
		}
	}
	if !strings.Contains(reask, planRepoRule+" "+planFilesRule+" "+planSharedDocsRule) {
		t.Errorf("the re-ask does not carry the two rules after planRepoRule:\n%s", reask)
	}
}

// rule3Rule2 returns rule 2's line of the prompt.
func rule3Rule2(t *testing.T, p string) string {
	i, j := strings.Index(p, "\n2. "), strings.Index(p, "\n3. ")
	if i < 0 || j < i {
		t.Fatalf("no rule 2:\n%s", p)
	}
	return p[i : j+1]
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
	if want := "If only part of it is there, build the rest.\n\n# Test what you touched\n\n"; !strings.Contains(build, want) {
		t.Errorf("build prompt lacks\n%s\n---\n%s", want, build)
	}
	if want := "test guard refuses a whole-suite or backgrounded test run.\n\n# Services\n\n" + line + "\n\n# Rules\n\n1. Read the repository's own instructions"; !strings.Contains(build, want) {
		t.Errorf("build prompt lacks\n%s\n---\n%s", want, build)
	}
	eval := EvaluatePrompt(EvaluateInput{GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Repo: "o/r", Branch: "b", BaseRef: "main", Services: line})
	if want := "The diff is empty: the branch changed nothing.\n\n# Services\n\n" + line + "\n\n# Rules\n\n1. Judge"; !strings.Contains(eval, want) {
		t.Errorf("evaluate prompt lacks\n%s\n---\n%s", want, eval)
	}
	if !strings.Contains(eval, "6. "+evaluateHostRule+"\n\n\n`verdict.notes` is your review") || strings.Contains(eval, VerdictPass) {
		t.Errorf("the rules do not end on the finish:\n%s", eval)
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

// Every prompt that carries runsheet's tools tells the session to
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

// Every prompt given the built-in server names it runsheet and
// never sends the session to its old tool prefix (goal
// builtin-is-runsheet).
func TestPromptsNameMeshContext(t *testing.T) {
	prompts := map[string]string{
		"build":    BuildPrompt(PromptInput{GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Brief: "do it", Repo: "o/r", Branch: "b", BaseRef: "main", ContextDir: ".mesh/context", Project: "acme/mesh", MeshContext: true}),
		"evaluate": EvaluatePrompt(EvaluateInput{GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Repo: "o/r", Branch: "b", BaseRef: "main", ContextDir: ".mesh/context", Project: "acme/mesh", MeshContext: true}),
		"plan":     PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p", ContextDir: ".mesh/context", MeshContext: true}),
		"research": ResearchPrompt(ResearchInput{Org: "acme", Project: "mesh", ContextDir: ".mesh/context", MeshContext: true}),
		"rebase":   RebasePrompt(RebaseInput{GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Repo: "o/r", Branch: "b", BaseRef: "main", ContextDir: ".mesh/context", Project: "acme/mesh", MeshContext: true}),
	}
	for name, p := range prompts {
		if !strings.Contains(p, "runsheet") {
			t.Errorf("%s prompt does not name runsheet:\n%s", name, p)
		}
		if strings.Contains(p, "mcp__"+tools.BuiltinAlias()+"__") {
			t.Errorf("%s prompt still names mcp__%s__:\n%s", name, tools.BuiltinAlias(), p)
		}
	}
}

// The build prompt asks for a last mesh_progress note opening `state:`
// naming what is done, what is next and what is open, so a fresh
// fallback session has somewhere to start from (goal
// a-fresh-session-starts-from-the-items-record).
func TestBuildPromptAsksForAStateNote(t *testing.T) {
	p := BuildPrompt(PromptInput{GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Repo: "o/r", Branch: "b", BaseRef: "main"})
	if !strings.Contains(p, "mesh_progress note") || !strings.Contains(p, "`state:`") {
		t.Errorf("build prompt lacks a line asking for a final mesh_progress note opening `state:`:\n%s", p)
	}
}

func TestBuildPromptNotesToFoldIn(t *testing.T) {
	in := PromptInput{GoalSlug: "g", GoalTitle: "G", Seq: 3, Title: "Docs", Brief: "write the ADR", Repo: "o/r", Branch: "b", BaseRef: "main"}
	if p := BuildPrompt(in); strings.Contains(p, "## Notes to fold in") {
		t.Fatalf("no notes, yet the section:\n%s", p)
	}
	in.FoldNotes = []ItemNote{
		{Seq: 1, Title: "Harness rules", Note: "for the docs writer: name planFilesRule", PRURL: "https://github.com/o/r/pull/1"},
		{Seq: 2, Title: "Plan check", Note: "for the docs writer: name sharedCodeFiles"},
	}
	p := BuildPrompt(in)
	want := "## Notes to fold in\n\nThis item writes the goal's shared documents. The other items left these notes for it; fold each into the documents:\n\n" +
		"- item 1 (Harness rules): for the docs writer: name planFilesRule (https://github.com/o/r/pull/1)\n" +
		"- item 2 (Plan check): for the docs writer: name sharedCodeFiles\n" +
		"\nEach sibling's merged pull request description is the other source: read it for what the note leaves out.\n\n"
	if !strings.Contains(p, want) {
		t.Fatalf("want the section\n%s\nin\n%s", want, p)
	}
	if strings.Index(p, "## Brief") > strings.Index(p, "## Notes to fold in") {
		t.Fatal("the notes come after the brief")
	}
}

// The evaluator and the planner run on the brief's guidance when it
// carries one (ADR-041): the guidance first, verbatim, then the job's
// facts, then the node's checks and the finish — none of the node's
// role text, no contract line, no hosted tool named.
func TestPromptsRunOnTheBriefsGuidance(t *testing.T) {
	finish := "# How this job finishes\n\nCall the mesh-node server's `mesh_result` tool once.\n"
	eval := EvaluatePrompt(EvaluateInput{ItemID: "item-7", GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Repo: "o/r", Branch: "b", BaseRef: "main", Diff: "+x",
		PriorAttempts: []PriorAttempt{{Attempt: 1, Clauses: []FailedClause{{"a", "b"}}}}, Guidance: "You are the evaluator.\n\n1. Judge literally.\n", Finish: finish})
	if !strings.HasPrefix(eval, "You are the evaluator.\n\n1. Judge literally.\n\n# Item 1 of goal \"G\" (g): T") {
		t.Fatalf("the evaluate prompt does not open with the guidance:\n%s", eval)
	}
	for _, want := range []string{"# The branch", "```diff\n+x\n```", "# What earlier attempts failed on", "- a — satisfied by: b", finish, "`verdict.failedClauses`", "`verdict.readings`"} {
		if !strings.Contains(eval, want) {
			t.Errorf("the guided evaluate prompt lacks %q", want)
		}
	}
	for _, gone := range []string{"You are the evaluator of item", "# Rules", evaluateUnverifiableRule, evaluateHostRule, evaluateRetryRule, evaluateSameClauseRule, "mesh_verdict", VerdictPass, ClauseFailed} {
		if strings.Contains(eval, gone) {
			t.Errorf("the guided evaluate prompt still carries %q", gone)
		}
	}
	plan := PlanPrompt(PlanInput{GoalID: "goal-3", GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p", Repos: map[string]string{"o/r": "repos/r"}, MeshContext: true,
		Tools: []ToolHint{{Name: "brave"}}, Guidance: "You are the planner.\n", Finish: finish})
	if !strings.HasPrefix(plan, "You are the planner.\n\n# Goal: G (g) for o, project p\n") {
		t.Fatalf("the plan prompt does not open with the guidance:\n%s", plan)
	}
	for _, want := range []string{"- `o/r` in `repos/r/`", "mesh_search", "# Your tools", "- `brave`:", "# What the node checks before it hands the plan in", "- " + planHostRule, "- " + planSharedDocsRule, "Each item names the model its session runs on", finish, "`plan.items` is the graph", "`plan.already_done`", "`plan.split`"} {
		if !strings.Contains(plan, want) {
			t.Errorf("the guided plan prompt lacks %q", want)
		}
	}
	for _, gone := range []string{"You are the planner of goal", "# Rules", "# Check main first", "1. Read before you plan", "mesh_plan", "```json"} {
		if strings.Contains(plan, gone) {
			t.Errorf("the guided plan prompt still carries %q", gone)
		}
	}
	// without guidance the built-in text runs, ending on the finish
	fallback := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p", Finish: finish})
	if !strings.Contains(fallback, "You are the planner of goal") || !strings.Contains(fallback, "# Rules\n\n1. Read before you plan") || !strings.Contains(fallback, finish) || strings.Contains(fallback, "```json") {
		t.Errorf("the built-in plan prompt:\n%s", fallback)
	}
}

// A validation failure's retry turn names the step, its command, exit
// code and output, where it was judged, and the item's acceptance.
func TestRetryTurnPromptValidation(t *testing.T) {
	p := RetryTurnPrompt(RetryTurn{
		Step: "unit", Command: "go test ./...", ExitCode: 1, Output: "FAIL ./api TestThing\n",
		Branch: "mesh/ship-it-1", HeadSHA: "abc123", ChangedPaths: []string{"internal/api/handler.go"},
		Acceptance: "go test ./... passes",
	})
	for _, want := range []string{
		"step unit (`go test ./...`) exited 1",
		"FAIL ./api TestThing",
		"mesh/ship-it-1",
		"abc123",
		"internal/api/handler.go",
		"go test ./... passes",
		"Fix it and commit",
		"hand back",
		"Never push",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("validation retry turn prompt lacks %q:\n%s", want, p)
		}
	}
}

// An evaluation failure's retry turn names each failed clause with what
// would satisfy it, the evaluator's own review, where it was judged, and
// the item's acceptance.
func TestRetryTurnPromptEvaluation(t *testing.T) {
	p := RetryTurnPrompt(RetryTurn{
		Clauses: []FailedClause{
			{Clause: "go vet ./... passes", Satisfy: "no vet error on the branch"},
			{Clause: "the endpoint returns 404 for a missing id", Satisfy: "a test proving it"},
		},
		Notes:      "The handler panics on a missing id instead of returning 404.",
		Branch:     "mesh/ship-it-1",
		HeadSHA:    "def456",
		Acceptance: "The service builds with `go build ./...` and ships a CHANGELOG entry.",
	})
	for _, want := range []string{
		"go vet ./... passes",
		"no vet error on the branch",
		"the endpoint returns 404 for a missing id",
		"a test proving it",
		"panics on a missing id",
		"def456",
		"## Acceptance\n\nThe service builds with `go build ./...` and ships a CHANGELOG entry.",
		"Fix it and commit",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("evaluation retry turn prompt lacks %q:\n%s", want, p)
		}
	}
}

// A research session on the brief's guidance opens with it, is told the
// room as a fact, and carries none of the node's role text or rules;
// without guidance the built-in text runs, ending on the finish
// (ADR-041).
func TestResearchPromptRunsOnTheBriefsGuidance(t *testing.T) {
	finish := "# How this job finishes\n\nCall `mesh_result` once.\n"
	p := ResearchPrompt(ResearchInput{Org: "o", Project: "p", Direction: "d", Room: 2, DefaultBudgetCents: 4000, Tools: []ToolHint{{Name: "brave"}}, Guidance: "You direct the project.\n", Finish: finish})
	if !strings.HasPrefix(p, "You direct the project.\n\n# The direction\n\nd\n") {
		t.Fatalf("the prompt does not open with the guidance:\n%s", p)
	}
	for _, want := range []string{"# The project", "# Your tools", "# The room\n\nThere is room for 2 goals right now", "$40.00", finish, "`research.goals` is your proposals"} {
		if !strings.Contains(p, want) {
			t.Errorf("the guided research prompt lacks %q", want)
		}
	}
	for _, gone := range []string{"You are the director of", "# Rules", "1. Research first", "```json"} {
		if strings.Contains(p, gone) {
			t.Errorf("the guided research prompt still carries %q", gone)
		}
	}
	pitches := ResearchPrompt(ResearchInput{Org: "o", Project: "p", Direction: "d", Room: 0, Proposes: true, Guidance: "You direct.", Finish: finish})
	if !strings.Contains(pitches, "# The room\n\nThis direction proposes rather than sets") || !strings.Contains(pitches, "`research.pitches` is your pitches") || strings.Contains(pitches, "mesh_projects") {
		t.Fatalf("the guided propose-mode prompt:\n%s", pitches)
	}
	fallback := ResearchPrompt(ResearchInput{Org: "o", Project: "p", Direction: "d", Room: 1, Finish: finish})
	if !strings.Contains(fallback, "You are the director of o, project p") || !strings.Contains(fallback, "# Rules\n\n1. Research first") || !strings.Contains(fallback, finish) || strings.Contains(fallback, "```json") {
		t.Fatalf("the built-in research prompt:\n%s", fallback)
	}
}

// A build session on the brief's guidance opens with it, keeps the
// node's own sections — check main first, test what you touched, the
// workspace, the finish by mesh_finish — and carries none of the node's
// role line or rules (ADR-041: the build's finish stays the node's).
func TestBuildPromptRunsOnTheBriefsGuidance(t *testing.T) {
	p := BuildPrompt(PromptInput{ItemID: "item-1", GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Repo: "o/r", Branch: "mesh/g-1", BaseRef: "main", ContextDir: ".mesh/context", MeshContext: true, Project: "o/p", Guidance: "You build one item.\n"})
	if !strings.HasPrefix(p, "You build one item.\n\n# Item 1: T\n") {
		t.Fatalf("the prompt does not open with the guidance:\n%s", p)
	}
	for _, want := range []string{"# Check main first", "# Test what you touched", "# The workspace\n\nYou are in o/r on branch `mesh/g-1` (from `main`)", "The project's context is in `.mesh/context/`", "mesh_search", "# Finish by tool call", "`mesh_finish` tool"} {
		if !strings.Contains(p, want) {
			t.Errorf("the guided build prompt lacks %q", want)
		}
	}
	for _, gone := range []string{"You are a Mesh node session working on item", "# Rules", "3. Commit as you go", "8. Before you stop"} {
		if strings.Contains(p, gone) {
			t.Errorf("the guided build prompt still carries %q", gone)
		}
	}
	fallback := BuildPrompt(PromptInput{Seq: 1, Title: "T", Repo: "o/r", Branch: "b", BaseRef: "main"})
	if !strings.Contains(fallback, "You are a Mesh node session working on item 1") || !strings.Contains(fallback, "# Rules\n\n1. Read the repository") || strings.Contains(fallback, "# The workspace") {
		t.Fatalf("the built-in build prompt:\n%s", fallback)
	}
}
