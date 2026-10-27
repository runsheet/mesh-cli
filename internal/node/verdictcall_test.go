package node

import (
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/harness"
)

// claudeOnly is familyFixture's evaluator with no Codex account, so the
// evaluation runs on Claude alone, its session ending with says and
// calls — a mesh_verdict call the scripted session hands the node as its
// mesh_result verdict (ADR-041).
func claudeOnly(t *testing.T, says string, calls map[string]map[string]any) (map[string]any, string) {
	t.Helper()
	f, ev, claude, codex, _, logs := familyFixture(t, "normal", "sonnet", "", "VERDICT: PASS")
	ev.CodexAccounts = (&Loop{Known: &KnownAccounts{}}).CodexCandidates
	claude.texts = []string{says}
	claude.workCalls = calls
	in := evaluateItem1(t, f, ev)
	if len(claude.jobs) != 1 || codex.runs() != 0 {
		t.Fatalf("runs claude %d codex %d", len(claude.jobs), codex.runs())
	}
	return in, logs.String()
}

// The evaluator's prompt is the brief's guidance first when the brief
// carries one, with none of the node's role text; its finish section
// names the product's mesh_verdict, the tool the session hands in with
// (runsheet ADR 077).
func TestEvaluationRunsOnTheBriefsGuidance(t *testing.T) {
	f, ev, claude, _, _, logs := familyFixture(t, "normal", "sonnet", "Holds.\nVERDICT: PASS", "")
	ev.CodexAccounts = (&Loop{Known: &KnownAccounts{}}).CodexCandidates
	f.SetKindGuidance(EvaluateKind, "You are the evaluator of one item of a goal.\n\n1. Judge literally.\n", "")
	evaluateItem1(t, f, ev)
	job := claude.jobs[0]
	if !strings.HasPrefix(job.Prompt, "You are the evaluator of one item of a goal.\n\n1. Judge literally.\n\n# Item 1 of goal") {
		t.Fatalf("the prompt does not open with the guidance:\n%.400s", job.Prompt)
	}
	for _, gone := range []string{"You are the evaluator of item 1", "# Rules\n", "VERDICT: PASS"} {
		if strings.Contains(job.Prompt, gone) {
			t.Fatalf("the prompt carries the node's role text %q", gone)
		}
	}
	if !strings.Contains(job.Prompt, "# How this job finishes") || !strings.Contains(job.Prompt, "mesh_verdict") {
		t.Fatalf("no finish section:\n%s", job.Prompt)
	}
	// the verdict goes to the product's tool, not the node's own
	if _, ok := job.MCPServers[harness.NodeWorkMCP]; ok {
		t.Fatalf("the node's own %s server is served: %v", harness.NodeWorkMCP, job.MCPServers)
	}
	if strings.Contains(logs.String(), noGuidanceLine) {
		t.Fatalf("a brief with guidance logged %q", noGuidanceLine)
	}
}

// Without guidance the built-in prompt runs, logged once per kind.
func TestEvaluationWithoutGuidanceRunsTheBuiltInPrompt(t *testing.T) {
	f, ev, claude, _, _, logs := familyFixture(t, "normal", "sonnet", "Holds.\nVERDICT: PASS", "")
	ev.CodexAccounts = (&Loop{Known: &KnownAccounts{}}).CodexCandidates
	evaluateItem1(t, f, ev)
	if !strings.Contains(claude.jobs[0].Prompt, "You are the evaluator of item 1 of goal") || !strings.Contains(claude.jobs[0].Prompt, "# How this job finishes") {
		t.Fatalf("the built-in prompt:\n%.300s", claude.jobs[0].Prompt)
	}
	if n := strings.Count(logs.String(), noGuidanceLine); n != 1 {
		t.Fatalf("%q logged %d times", noGuidanceLine, n)
	}
}
