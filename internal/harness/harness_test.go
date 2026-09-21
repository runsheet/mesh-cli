package harness

import (
	"strings"
	"testing"
)

func TestBuildPrompt(t *testing.T) {
	p := BuildPrompt(PromptInput{
		GoalSlug: "ship-it", GoalTitle: "Ship it", GoalStatement: "Make the thing.", GoalAcceptance: "go test ./... passes",
		Seq: 2, Title: "Make it pass", Brief: "Implement.", Acceptance: "go test passes",
		Repo: "acme/widgets", Branch: "mesh/ship-it-2", BaseRef: "main",
		ContextDir: ".mesh/context", ContextFiles: []string{"instructions-agents.md", "adr-001.md"},
	})
	for _, want := range []string{
		"# Item 2: Make it pass", "Implement.", "go test passes",
		"# Goal: Ship it", "Make the thing.", "go test ./... passes",
		"`.mesh/context/` first", "instructions-agents.md, adr-001.md",
		"branch `mesh/ship-it-2` (from `main`)", "acme/widgets",
		"Do not push", "run the acceptance check",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(BuildPrompt(PromptInput{Title: "x"}), ".mesh/context") {
		t.Error("no context dir must not mention one")
	}
}

func TestParseStreamCost(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s-1","model":"claude"}`,
		`{"type":"assistant","session_id":"s-1","message":{"role":"assistant","content":[{"type":"text","text":"Reading the context."},{"type":"tool_use","name":"Read"}]}}`,
		`not json at all`,
		`{"type":"assistant","session_id":"s-1","message":{"role":"assistant","content":[{"type":"text","text":"Done: tests pass."}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"Implemented and tested.","total_cost_usd":0.4321,"num_turns":7,"session_id":"s-1"}`,
	}, "\n")
	var texts []string
	res := ParseStream(strings.NewReader(stream), func(ev Event) {
		if ev.Type == "assistant" {
			texts = append(texts, ev.Text())
		}
	})
	if !res.Success || res.CostUSD != 0.4321 || res.SessionID != "s-1" || res.Turns != 7 || res.Text != "Implemented and tested." {
		t.Fatalf("result = %+v", res)
	}
	if len(texts) != 2 || texts[0] != "Reading the context." {
		t.Fatalf("texts = %v", texts)
	}
}

func TestParseStreamNoResult(t *testing.T) {
	res := ParseStream(strings.NewReader(`{"type":"assistant","session_id":"s-2","message":{"content":[{"type":"text","text":"half way"}]}}`+"\n"), nil)
	if res.Success || res.Text != "half way" || res.SessionID != "s-2" {
		t.Fatalf("result = %+v", res)
	}
	errRes := ParseStream(strings.NewReader(`{"type":"result","subtype":"error_max_budget_usd","is_error":true,"result":"budget","total_cost_usd":2,"session_id":"s-3"}`), nil)
	if errRes.Success || errRes.CostUSD != 2 {
		t.Fatalf("error result = %+v", errRes)
	}
}

func TestClaudeCodeArgs(t *testing.T) {
	c := &ClaudeCode{ExtraArgs: []string{"--dangerously-skip-permissions"}}
	args := c.Args(Job{Prompt: "hi", MaxBudgetUSD: 1.5})
	want := "-p hi --output-format stream-json --verbose --max-budget-usd 1.50 --dangerously-skip-permissions"
	if got := strings.Join(args, " "); got != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
	// with an MCP server: the config inline and its tools allowed
	with := c.Args(Job{Prompt: "hi", MCPServers: map[string]any{"prologue": map[string]any{"type": "http", "url": "https://api.test/mcp"}}})
	joined := strings.Join(with, "\x00")
	if !strings.Contains(joined, "--mcp-config\x00{\"mcpServers\":{\"prologue\":{\"type\":\"http\",\"url\":\"https://api.test/mcp\"}}}\x00--allowedTools\x00mcp__prologue__*\x00--dangerously-skip-permissions") {
		t.Fatalf("mcp args: %q", with)
	}
}
