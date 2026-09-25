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
		"9. End your final message with the graph as a JSON object in a ```json fenced block, and nothing after it:\n",
		"```json\n{\"items\": [{\"seq\": 1, \"title\": \"…\", \"brief\": \"…\", \"acceptance\": \"…\", \"depends_on\": [], \"repo\": \"owner/name\", \"base_ref\": \"main\", \"model\": \"sonnet\", \"model_reason\": \"…\"}]}\n```\n",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plan prompt lacks %q", want)
		}
	}
}

// The planner chooses each item's model by the rubric, and the graph is
// still the last thing it writes.
func TestPlanPromptModelRubric(t *testing.T) {
	p := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p"})
	for _, want := range []string{
		"8. Each item names the model its session runs on, as `model`, with a one-line `model_reason`:\n",
		"   - `haiku` for mechanical items: docs-only, a goal-doc section, a version bump, a rename.\n",
		"   - `sonnet` for a well-specified change in one package with tests to copy.\n",
		"   - `opus` for anything cross-cutting, concurrency, migrations, security, a failing item's retry, or when unsure.\n",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plan prompt lacks %q", want)
		}
	}
	if !strings.HasSuffix(p, "\"model\": \"sonnet\", \"model_reason\": \"…\"}]}\n```\n") {
		t.Error("the plan prompt no longer ends with the JSON example")
	}
}

func TestEvaluatePromptUnverifiableClause(t *testing.T) {
	p := EvaluatePrompt(EvaluateInput{GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Repo: "o/r", Branch: "b", BaseRef: "main"})
	for _, want := range []string{
		"5. A clause you cannot check at evaluation time — a CI check on this item's pull request (it opens only after you pass), a deploy, a file in another repository — is reported in your review as unverifiable, not failed, and the item is not failed on that clause alone. Every clause you can check you still check.\n",
		"6. End your final message with a short review — what holds, what does not, what is unverifiable, and exactly what a retry must fix — and then one last line that is either `VERDICT: PASS` or `VERDICT: FAIL`. Pass only when every clause of the acceptance you can check holds.\n",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("evaluate prompt lacks %q", want)
		}
	}
	if strings.Contains(p, "Pass only when every clause of the acceptance holds") {
		t.Error("evaluate prompt still passes only when every clause holds, checkable or not")
	}
}

func TestPlanPromptRejected(t *testing.T) {
	if p := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p"}); strings.Contains(p, "Your last answer") {
		t.Error("a first ask lists no rejected items")
	}
	p := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Org: "o", Project: "p", Rejected: []string{"item 1 (Ship): a check on its own pull request (`deploy / verify`)"}})
	for _, want := range []string{
		"## Your last answer asked for what no evaluator can see\n\nThe node checked your graph before handing it in and flagged these items:\n\n- item 1 (Ship): a check on its own pull request (`deploy / verify`)\n\n",
		"Work like that becomes its own item in the right repo, or goes in the goal's acceptance. Write the whole graph again with those acceptances rewritten; this is the last ask.\n",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plan prompt lacks %q", want)
		}
	}
}
