package harness

import (
	"os"
	"strings"
	"testing"
)

// Each tool_use of an MCP server's tool counts once for its server; a
// built-in tool (Bash) counts for none.
func TestParseStreamToolCalls(t *testing.T) {
	f, err := os.Open("testdata/stream_tool_calls.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	res := ParseStream(f, nil)
	if !res.Success {
		t.Fatalf("result = %+v", res)
	}
	if res.ToolCalls["search"] != 2 || res.ToolCalls["prologue"] != 1 || len(res.ToolCalls) != 2 {
		t.Fatalf("ToolCalls = %v, want search:2 prologue:1", res.ToolCalls)
	}
}

// A session that calls no MCP tool leaves ToolCalls nil.
func TestParseStreamNoToolCalls(t *testing.T) {
	f, err := os.Open("testdata/stream_usage.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if res := ParseStream(f, nil); res.ToolCalls != nil {
		t.Fatalf("ToolCalls = %v, want nil", res.ToolCalls)
	}
}

// A Codex session counts each completed mcp_tool_call by its server, not
// its start.
func TestParseCodexStreamToolCalls(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"thread.started","thread_id":"th"}`,
		`{"type":"item.started","item":{"id":"i1","type":"mcp_tool_call","server":"search","tool":"query","status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"i1","type":"mcp_tool_call","server":"search","tool":"query","status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"i2","type":"mcp_tool_call","server":"search","tool":"query","status":"failed"}}`,
		`{"type":"item.completed","item":{"id":"i3","type":"command_execution","command":"ls","status":"completed"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`,
	}, "\n")
	res := ParseCodexStream(strings.NewReader(stream), "gpt-5-codex", nil)
	if res.ToolCalls["search"] != 2 || len(res.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %v, want search:2", res.ToolCalls)
	}
}

func TestMCPServer(t *testing.T) {
	for name, want := range map[string]string{
		"mcp__search__query": "search", "mcp__my-tool__a__b": "my-tool", "Bash": "", "mcp__bare": "",
	} {
		if got := MCPServer(name); got != want {
			t.Errorf("MCPServer(%q) = %q, want %q", name, got, want)
		}
	}
}
