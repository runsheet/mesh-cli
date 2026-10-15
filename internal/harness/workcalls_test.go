package harness

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// finishInput is the mesh_finish input both fixtures carry.
var finishInput = map[string]any{
	"item_id":  "eeeeeeee-1111-4111-8111-eeeeeeeeeeee",
	"branch":   "mesh/ship-it-1",
	"head_sha": "3f2a9c41d0b7e65a8f1c2d3e4b5a69788c7d6e5f",
}

// A Claude session that ends by calling mcp__runsheet__mesh_finish
// keeps the call's input on WorkCalls by the tool's own name, beside
// mesh_progress's; mesh_document, not a work tool, is not kept
// (ADR-023, sessions finish by tool call).
func TestParseStreamKeepsWorkCalls(t *testing.T) {
	f, err := os.Open("testdata/stream_mesh_finish.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	res := ParseStream(f, nil)
	if !res.Success || res.Text != "" {
		t.Fatalf("success %v, text %q; want a success with no final text", res.Success, res.Text)
	}
	if got := res.WorkCalls["mesh_finish"]; !reflect.DeepEqual(got, finishInput) {
		t.Errorf("WorkCalls[mesh_finish] = %v, want %v", got, finishInput)
	}
	if got := res.WorkCalls["mesh_progress"]["note"]; got != "the failing test is written" {
		t.Errorf("WorkCalls[mesh_progress].note = %v", got)
	}
	if _, ok := res.WorkCalls["mesh_document"]; ok || len(res.WorkCalls) != 2 {
		t.Errorf("WorkCalls = %v, want mesh_finish and mesh_progress alone", res.WorkCalls)
	}
	if res.ToolCalls["runsheet"] != 3 {
		t.Errorf("ToolCalls[runsheet] = %d, want 3", res.ToolCalls["runsheet"])
	}
}

// A stream whose system/init names the built-in runsheet and one an
// older node started, naming it mesh-context, both yield the mesh_finish
// work call, and the old name's calls fold into runsheet on ToolNames and
// DocumentsFetched (tools.IsBuiltin; ADR-006's amendment 2026-10-15).
func TestParseStreamWorkCallsUnderEitherBuiltinName(t *testing.T) {
	raw, err := os.ReadFile("testdata/stream_mesh_finish.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"runsheet", "mesh-context"} {
		stream := strings.ReplaceAll(string(raw), `"runsheet"`, `"`+name+`"`)
		stream = strings.ReplaceAll(stream, "mcp__runsheet__", "mcp__"+name+"__")
		if !strings.Contains(stream, `{"name":"`+name+`","status":"connected"}`) {
			t.Fatalf("%s: system/init does not name it", name)
		}
		res := ParseStream(strings.NewReader(stream), nil)
		if got := res.WorkCalls["mesh_finish"]; !reflect.DeepEqual(got, finishInput) {
			t.Errorf("%s: WorkCalls[mesh_finish] = %v, want %v", name, got, finishInput)
		}
		if res.ToolNames["runsheet__mesh_finish"] != 1 || res.ToolNames["runsheet__mesh_document"] != 1 || res.DocumentsFetched != 1 {
			t.Errorf("%s: ToolNames %v, DocumentsFetched %d; want the built-in's calls under runsheet", name, res.ToolNames, res.DocumentsFetched)
		}
	}
}

// A Codex session's mcp_tool_call to mesh_finish keeps its arguments
// on WorkCalls the same way.
func TestParseCodexStreamKeepsWorkCalls(t *testing.T) {
	res, _ := parseCodexFixture(t, "codex_mesh_finish.jsonl", "gpt-6-sol")
	if got := res.WorkCalls["mesh_finish"]; !reflect.DeepEqual(got, finishInput) {
		t.Errorf("WorkCalls[mesh_finish] = %v, want %v", got, finishInput)
	}
	if len(res.WorkCalls) != 1 {
		t.Errorf("WorkCalls = %v, want mesh_finish alone", res.WorkCalls)
	}
}

// A session that called no work tool has no WorkCalls.
func TestParseStreamNoWorkCalls(t *testing.T) {
	f, err := os.Open("testdata/stream_tool_calls.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if res := ParseStream(f, nil); res.WorkCalls != nil {
		t.Errorf("WorkCalls = %v, want nil", res.WorkCalls)
	}
}

// The build prompt tells the session to end by calling mesh_finish on
// its item, built or already done, and keeps the ALREADY DONE: line as
// the fallback contract the node parses (ADR-023).
func TestBuildPromptFinishByToolCall(t *testing.T) {
	p := BuildPrompt(PromptInput{GoalSlug: "ship-it", GoalTitle: "Ship it", Seq: 2, ItemID: "item-2", Title: "Make it pass", Repo: "acme/widgets", Branch: "mesh/ship-it-2", BaseRef: "main"})
	for _, want := range []string{
		"# Finish by tool call",
		`mesh_finish(item_id="item-2", branch="mesh/ship-it-2", head_sha=`,
		`mesh_finish(item_id="item-2", outcome="already_done", evidence=`,
		"mesh_fail(item_id, error=",
		"end your final message with a line that starts `" + AlreadyDoneLine + "`",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("build prompt lacks %q:\n%s", want, p)
		}
	}
	if p := BuildPrompt(PromptInput{GoalSlug: "ship-it", Seq: 2, Branch: "b"}); !strings.Contains(p, `item_id="<the id of item 2 of ship-it>"`) {
		t.Errorf("build prompt with no item id lacks the placeholder:\n%s", p)
	}
}
