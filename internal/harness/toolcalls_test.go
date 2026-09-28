package harness

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeClock installs a monotonically increasing clock for the duration
// of the test, restoring now on cleanup.
func fakeClock(t *testing.T) {
	t.Helper()
	orig := now
	tick := int64(0)
	now = func() time.Time {
		tick++
		return time.Unix(tick, 0)
	}
	t.Cleanup(func() { now = orig })
}

// Each tool_use of an MCP server's tool counts once for its server, its
// id kept in stream order; a built-in tool (Bash) counts for none.
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
	if got := res.ToolCallIDs["search"]; len(got) != 2 || got[0] != "t1" || got[1] != "t2" {
		t.Fatalf("ToolCallIDs[search] = %v, want [t1 t2]", got)
	}
	if got := res.ToolCallIDs["prologue"]; len(got) != 1 || got[0] != "t3" {
		t.Fatalf("ToolCallIDs[prologue] = %v, want [t3]", got)
	}
	for server, calls := range res.ToolCalls {
		if got := len(res.ToolCallIDs[server]); got != calls {
			t.Errorf("len(ToolCallIDs[%s]) = %d, want %d", server, got, calls)
		}
	}
}

// A tool_use block with no id gets one synthesised, and the count and
// the id list still agree.
func TestParseStreamToolCallNoID(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s-noid","model":"claude-sonnet-5"}`,
		`{"type":"assistant","session_id":"s-noid","message":{"role":"assistant","content":[{"type":"tool_use","name":"mcp__search__query"}]}}`,
		`{"type":"result","subtype":"success","session_id":"s-noid","is_error":false,"result":"Done."}`,
	}, "\n")
	res := ParseStream(strings.NewReader(stream), nil)
	if res.ToolCalls["search"] != 1 {
		t.Fatalf("ToolCalls = %v, want search:1", res.ToolCalls)
	}
	ids := res.ToolCallIDs["search"]
	if len(ids) != 1 || ids[0] == "" {
		t.Fatalf("ToolCallIDs[search] = %v, want one non-empty synthesised id", ids)
	}
	if want := "s-noid:search:1"; ids[0] != want {
		t.Fatalf("ToolCallIDs[search][0] = %q, want %q", ids[0], want)
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
// its start, keeping its item id in stream order.
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
	if got := res.ToolCallIDs["search"]; len(got) != 2 || got[0] != "i1" || got[1] != "i2" {
		t.Fatalf("ToolCallIDs[search] = %v, want [i1 i2]", got)
	}
	for server, calls := range res.ToolCalls {
		if got := len(res.ToolCallIDs[server]); got != calls {
			t.Errorf("len(ToolCallIDs[%s]) = %d, want %d", server, got, calls)
		}
	}
}

// A completed mcp_tool_call with no item id gets one synthesised.
func TestParseCodexStreamToolCallNoID(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"thread.started","thread_id":"th-noid"}`,
		`{"type":"item.completed","item":{"type":"mcp_tool_call","server":"search","tool":"query","status":"completed"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`,
	}, "\n")
	res := ParseCodexStream(strings.NewReader(stream), "gpt-5-codex", nil)
	if res.ToolCalls["search"] != 1 {
		t.Fatalf("ToolCalls = %v, want search:1", res.ToolCalls)
	}
	ids := res.ToolCallIDs["search"]
	if want := "th-noid:search:1"; len(ids) != 1 || ids[0] != want {
		t.Fatalf("ToolCallIDs[search] = %v, want [%s]", ids, want)
	}
}

// Three calls to brave, one of whose tool_result errors, yield one
// ToolReport row {brave, Calls 3, Errors 1, Ms > 0}; the Bash call is
// not counted, and prologue is its own row.
func TestParseStreamToolReport(t *testing.T) {
	fakeClock(t)
	f, err := os.Open("testdata/stream_tool_errors.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	res := ParseStream(f, nil)
	if !res.Success {
		t.Fatalf("result = %+v", res)
	}
	report := res.ToolReport()
	if len(report) != 2 {
		t.Fatalf("ToolReport() = %+v, want 2 rows", report)
	}
	brave, prologue := report[0], report[1]
	if brave.Tool != "brave" || brave.Calls != 3 || brave.Errors != 1 || brave.Ms <= 0 {
		t.Errorf("brave row = %+v, want {brave 3 1 >0}", brave)
	}
	if prologue.Tool != "prologue" || prologue.Calls != 1 || prologue.Errors != 0 {
		t.Errorf("prologue row = %+v, want {prologue 1 0 _}", prologue)
	}
	if got := res.Tools["brave"].FirstError; got != "rate limited: too many requests" {
		t.Errorf("brave FirstError = %q", got)
	}
	if _, ok := res.Tools["Bash"]; ok {
		t.Errorf("Tools has a Bash row: %+v", res.Tools)
	}
}

// A system/init event's mcp_servers names a server whose status is not
// "connected" as an error, with that status as FirstError, even with
// zero calls.
func TestParseStreamMCPServerNotConnected(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s-mcp","model":"claude-sonnet-5","mcp_servers":[{"name":"badserver","status":"failed"},{"name":"goodserver","status":"connected"}]}`,
		`{"type":"result","subtype":"success","session_id":"s-mcp","is_error":false,"result":"Done."}`,
	}, "\n")
	res := ParseStream(strings.NewReader(stream), nil)
	report := res.ToolReport()
	if len(report) != 1 || report[0].Tool != "badserver" || report[0].Calls != 0 || report[0].Errors != 1 {
		t.Fatalf("ToolReport() = %+v, want one badserver row with 0 calls, 1 error", report)
	}
	if got := res.Tools["badserver"].FirstError; got != "failed" {
		t.Fatalf("FirstError = %q, want %q", got, "failed")
	}
}

// A Codex mcp_tool_call that completes with status failed counts as one
// call and one error for its server.
func TestParseCodexStreamToolReportError(t *testing.T) {
	fakeClock(t)
	f, err := os.Open("testdata/codex_tool_errors.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	res := ParseCodexStream(f, "gpt-5-codex", nil)
	report := res.ToolReport()
	if len(report) != 1 || report[0].Tool != "brave" || report[0].Calls != 1 || report[0].Errors != 1 {
		t.Fatalf("ToolReport() = %+v, want one brave row with 1 call, 1 error", report)
	}
	if got := res.Tools["brave"].FirstError; got != "failed" {
		t.Fatalf("FirstError = %q, want %q", got, "failed")
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

// A Codex mcp_tool_call that fails carrying an error keeps the error's
// text as FirstError, not the status, and the report's Error is its
// first 200 characters.
func TestParseCodexStreamKeepsTheCallsErrorText(t *testing.T) {
	fakeClock(t)
	f, err := os.Open("testdata/codex_mcp_call_error.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	res := ParseCodexStream(f, "gpt-6-sol", nil)
	want := "tool call failed for `prologue/mesh_document`: HTTP status client error (401 Unauthorized) for url (https://api.runsheet.dev/mcp)"
	stat := res.Tools["prologue"]
	if stat == nil || stat.FirstError != want || stat.Errors != 1 || stat.Calls != 1 {
		t.Fatalf("Tools[prologue] = %+v, want 1 call, 1 error, FirstError %q", stat, want)
	}
	report := res.ToolReport()
	if len(report) != 1 || report[0].Error != capText(want, 200) {
		t.Fatalf("ToolReport() = %+v, want Error %q", report, capText(want, 200))
	}
}

// The live cause, recorded on the mini (codex-cli 0.157.0, `codex exec
// --json --sandbox workspace-write`, a config.toml with no
// default_tools_approval_mode): exec refuses the call before it leaves
// the process, and the stream says so — the text the item's note names.
func TestParseCodexStreamKeepsTheApprovalRefusal(t *testing.T) {
	fakeClock(t)
	f, err := os.Open("testdata/codex_mcp_approval_error.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	res := ParseCodexStream(f, "gpt-6-luna", nil)
	const want = "MCP tool call requires approval, but approval policy is never"
	if stat := res.Tools["prologue"]; stat == nil || stat.Calls != 1 || stat.Errors != 1 || stat.FirstError != want {
		t.Fatalf("Tools[prologue] = %+v, want 1 call, 1 error, FirstError %q", stat, want)
	}
}

// A failed call with no error but a result falls back to the result's
// text; a long error is capped at 300 on FirstError and 200 on the
// report.
func TestParseCodexStreamCallErrorFallbacks(t *testing.T) {
	long := strings.Repeat("x", 400)
	stream := `{"type":"item.completed","item":{"id":"a","type":"mcp_tool_call","server":"s1","status":"failed","result":{"content":[{"type":"text","text":"no such document"}]}}}
{"type":"item.completed","item":{"id":"b","type":"mcp_tool_call","server":"s2","status":"failed","error":"` + long + `"}}
{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`
	res := ParseCodexStream(strings.NewReader(stream), "gpt-6-sol", nil)
	if got := res.Tools["s1"].FirstError; got != "no such document" {
		t.Fatalf("s1 FirstError = %q", got)
	}
	if got := res.Tools["s2"].FirstError; len(got) != 300 {
		t.Fatalf("s2 FirstError is %d chars, want 300", len(got))
	}
	if report := res.ToolReport(); len(report[1].Error) != 200 {
		t.Fatalf("s2 report Error is %d chars, want 200", len(report[1].Error))
	}
}

// A Codex error item saying the built-in server failed at startup gives
// that server an error with the message and zero calls.
func TestParseCodexStreamMCPStartupError(t *testing.T) {
	f, err := os.Open("testdata/codex_mcp_startup_error.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	res := ParseCodexStream(f, "gpt-6-sol", nil)
	want := "MCP client for `prologue` failed to start: Auth required during MCP startup"
	stat := res.Tools["prologue"]
	if stat == nil || stat.Calls != 0 || stat.Errors != 1 || stat.FirstError != want {
		t.Fatalf("Tools[prologue] = %+v, want 0 calls, 1 error, FirstError %q", stat, want)
	}
	if res.ToolCalls["prologue"] != 0 {
		t.Fatalf("ToolCalls = %+v", res.ToolCalls)
	}
	if report := res.ToolReport(); len(report) != 1 || report[0].Error != want {
		t.Fatalf("ToolReport() = %+v", report)
	}
}

func TestMCPStartupServers(t *testing.T) {
	for msg, want := range map[string][]string{
		"MCP client for `prologue` failed to start: Auth required during MCP startup": {"prologue"},
		"The prologue MCP server is not logged in. Run `codex mcp login prologue`.":   {"prologue"},
		"MCP startup incomplete (failed: prologue, codex_apps)":                       {"prologue", "codex_apps"},
		"Code Mode is unavailable: codex-code-mode-host not found":                    nil,
		"model rerouted: gpt-6-astra -> gpt-6-sol (capacity)":                         nil,
	} {
		if got := mcpStartupServers(msg); !slices.Equal(got, want) {
			t.Errorf("mcpStartupServers(%q) = %v, want %v", msg, got, want)
		}
	}
}
