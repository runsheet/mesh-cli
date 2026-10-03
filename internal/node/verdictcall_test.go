package node

import (
	"strings"
	"testing"
)

// claudeOnly is familyFixture's evaluator with no Codex account, so the
// evaluation runs on Claude alone, its session ending with says and
// calls (ADR-023, finish by tool call).
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

// A failing mesh_verdict call, with no final message at all, is the
// verdict: passed false, its notes and its one failed clause recorded.
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
	if strings.Contains(logs, noVerdictCallLine) {
		t.Fatalf("a tool-call verdict logged the fallback:\n%s", logs)
	}
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
	if !strings.Contains(logs, "item ship-it/1: reading recorded: the run takes under a minute") || strings.Contains(logs, noVerdictCallLine) {
		t.Fatalf("node.log:\n%s", logs)
	}
}

// With no call the final message is parsed as before, and that is
// logged once for the session.
func TestVerdictTextIsTheLoggedFallback(t *testing.T) {
	in, logs := claudeOnly(t, "Holds.\nVERDICT: PASS", nil)
	if in["passed"] != true {
		t.Fatalf("recorded %+v", in)
	}
	if n := strings.Count(logs, "no mesh_verdict call; parsed the final message"); n != 1 {
		t.Fatalf("fallback logged %d times:\n%s", n, logs)
	}
}

// Neither a call nor a VERDICT line is no verdict, as before.
func TestNeitherCallNorVerdictIsNoVerdict(t *testing.T) {
	in, _ := claudeOnly(t, "I ran out of ideas.", nil)
	if !sentNoVerdict(in) {
		t.Fatalf("recorded %+v, want no verdict", in)
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
	if strings.Contains(logs.String(), noVerdictCallLine) {
		t.Fatalf("node.log:\n%s", logs.String())
	}
}
