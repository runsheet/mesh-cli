package node

import (
	"context"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/jobresult"
	"projectmesh.io/mesh-cli/internal/mcptool"
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

// A failing verdict handed in by tool, with no final message at all, is
// the verdict: passed false, its notes and its one failed clause
// recorded.
func TestVerdictCallFails(t *testing.T) {
	in, logs := claudeOnly(t, "", map[string]map[string]any{"mesh_verdict": {
		"item_id": item1, "passed": false, "notes": "The test is missing.",
		"failed_clauses": []any{map[string]any{"clause": "a test covers the call", "satisfy": "add TestVerdictCall"}},
	}})
	if in["passed"] != false || in["noVerdict"] == true {
		t.Fatalf("recorded %+v", in)
	}
	if notes, _ := in["notes"].(string); !strings.HasPrefix(notes, "The test is missing.\n\n") {
		t.Fatalf("notes %q", notes)
	}
	fc, _ := in["failedClauses"].([]any)
	if len(fc) != 1 {
		t.Fatalf("failedClauses %v, want one", in["failedClauses"])
	}
	if c := fc[0].(map[string]any); c["clause"] != "a test covers the call" || c["satisfy"] != "add TestVerdictCall" {
		t.Fatalf("failedClauses %v", fc)
	}
	_ = logs
}

// A passing call passes, whatever the final message says; its readings
// ride the notes as READING lines and are logged as recorded (ADR-020).
func TestVerdictCallPasses(t *testing.T) {
	in, logs := claudeOnly(t, "VERDICT: FAIL", map[string]map[string]any{"mesh_verdict": {
		"item_id": item1, "passed": true, "notes": "Holds.",
		"readings": []any{map[string]any{"clause": "the run takes under a minute", "reading": "48s at load 3"}},
	}})
	if in["passed"] != true {
		t.Fatalf("recorded %+v", in)
	}
	if notes, _ := in["notes"].(string); !strings.Contains(notes, "Holds.\n\nREADING: the run takes under a minute || 48s at load 3") {
		t.Fatalf("notes %q", notes)
	}
	if !strings.Contains(logs, "item ship-it/1: reading recorded: the run takes under a minute") {
		t.Fatalf("node.log:\n%s", logs)
	}
}

// A session that hands nothing in — whatever its final message says —
// is no verdict: the node reads no text (ADR-041).
func TestNoResultIsNoVerdict(t *testing.T) {
	in, logs := claudeOnly(t, "Holds, I think.", nil)
	if !sentNoVerdict(in) {
		t.Fatalf("recorded %+v, want no verdict", in)
	}
	if notes, _ := in["notes"].(string); !strings.Contains(notes, "no result: the session made no mesh_result call") {
		t.Fatalf("notes %q\n%s", notes, logs)
	}
}

// A session that gives the evaluation up by mesh_fail is no verdict,
// with its reason.
func TestMeshFailIsNoVerdictWithTheReason(t *testing.T) {
	f, ev, claude, codex, _, _ := familyFixture(t, "normal", "sonnet", "", "VERDICT: PASS")
	ev.CodexAccounts = (&Loop{Known: &KnownAccounts{}}).CodexCandidates
	claude.onRun = func(job harness.Job) {
		tool, _ := mcptool.Find(job.NodeTools, jobresult.FailTool)
		_, _ = tool.Call(context.Background(), map[string]any{"error": "the checkout has no go toolchain"})
	}
	in := evaluateItem1(t, f, ev)
	if codex.runs() != 0 || !sentNoVerdict(in) {
		t.Fatalf("recorded %+v", in)
	}
	if notes, _ := in["notes"].(string); !strings.Contains(notes, "the session gave the evaluation up: the checkout has no go toolchain") {
		t.Fatalf("notes %q", notes)
	}
}

// The evaluator's prompt is the brief's guidance first when the brief
// carries one, with none of the node's role text, and its session is
// served the node's mesh_result beside the runsheet server, the hosted
// mesh_verdict disallowed (ADR-041).
func TestEvaluationRunsOnTheBriefsGuidance(t *testing.T) {
	f, ev, claude, _, _, logs := familyFixture(t, "normal", "sonnet", "Holds.\nVERDICT: PASS", "")
	ev.CodexAccounts = (&Loop{Known: &KnownAccounts{}}).CodexCandidates
	f.SetKindGuidance(EvaluateKind, "You are the evaluator of one item of a goal.\n\n1. Judge literally.\n", "")
	in := evaluateItem1(t, f, ev)
	if in["passed"] != true {
		t.Fatalf("recorded %+v", in)
	}
	job := claude.jobs[0]
	if !strings.HasPrefix(job.Prompt, "You are the evaluator of one item of a goal.\n\n1. Judge literally.\n\n# Item 1 of goal") {
		t.Fatalf("the prompt does not open with the guidance:\n%.400s", job.Prompt)
	}
	for _, gone := range []string{"You are the evaluator of item 1", "# Rules\n", "mesh_verdict", "VERDICT: PASS"} {
		if strings.Contains(job.Prompt, gone) {
			t.Fatalf("the prompt carries the node's role text %q", gone)
		}
	}
	if !strings.Contains(job.Prompt, "# How this job finishes") || !strings.Contains(job.Prompt, "{verdict}") {
		t.Fatalf("no finish section:\n%s", job.Prompt)
	}
	if _, ok := job.MCPServers[harness.NodeWorkMCP]; !ok {
		t.Fatalf("no %s server: %v", harness.NodeWorkMCP, job.MCPServers)
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

// A Codex evaluation's call is its verdict too, and a Claude second
// opinion's call decides it (both families, the second-opinion path).
func TestVerdictCallOnCodexAndTheSecondOpinion(t *testing.T) {
	f, ev, claude, codex, _, logs := familyFixture(t, "high", "opus", "", "")
	codex.workCalls = map[string]map[string]any{"mesh_verdict": {"item_id": item1, "passed": true, "notes": "Codex: holds."}}
	claude.texts = []string{""}
	claude.workCalls = map[string]map[string]any{"mesh_verdict": {"item_id": item1, "passed": "false", "notes": "Claude: the doc is stale.",
		"failed_clauses": []any{map[string]any{"clause": "the ADR is amended", "satisfy": "amend ADR-023"}}}}
	in := evaluateItem1(t, f, ev)
	if codex.runs() != 1 || len(claude.jobs) != 1 {
		t.Fatalf("runs codex %d claude %d", codex.runs(), len(claude.jobs))
	}
	notes, _ := in["notes"].(string)
	if in["passed"] != false || !strings.Contains(notes, "Codex: holds.") || !strings.Contains(notes, "Claude: the doc is stale.") {
		t.Fatalf("recorded %+v", in)
	}
	if fc, _ := in["failedClauses"].([]any); len(fc) != 1 || fc[0].(map[string]any)["clause"] != "the ADR is amended" {
		t.Fatalf("failedClauses %v", in["failedClauses"])
	}
	_ = logs
}
