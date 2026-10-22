package jobresult

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/mcptool"
)

const cardSchema = `{"type": "object", "required": ["slug", "title"], "properties": {"slug": {"type": "string"}, "title": {"type": "string", "maxLength": 60}}, "additionalProperties": false}`

// An answer-or-proposal result is recorded as finishJob takes it: the
// answer as text, the proposal as the JSON text of the object the
// session sent, and a second call replaces the first.
func TestAnswerOrProposalIsRecordedAsFinishJobTakesIt(t *testing.T) {
	h, err := New("runsheet/goal-chat", FinishAnswerOrProposal, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Call(map[string]any{"answer": "Here is a draft.", "proposal": map[string]any{"slug": "pdf-export", "title": "Reports export as PDF"}}); err != nil {
		t.Fatal(err)
	}
	got, ok := h.Result()
	if !ok || got["answer"] != "Here is a draft." {
		t.Fatalf("result %v %v", got, ok)
	}
	var proposal map[string]any
	if err := json.Unmarshal([]byte(got["proposal"].(string)), &proposal); err != nil || proposal["slug"] != "pdf-export" {
		t.Fatalf("proposal %v %v", got["proposal"], err)
	}
	reply, err := h.Call(map[string]any{"answer": "Changed my mind."})
	if err != nil || !strings.Contains(reply, "replacing") {
		t.Fatalf("second call: %q %v", reply, err)
	}
	if got, _ := h.Result(); got["answer"] != "Changed my mind." || got["proposal"] != nil {
		t.Fatalf("the second call did not replace the first: %v", got)
	}
	if h.Calls() != 2 {
		t.Fatalf("calls %d", h.Calls())
	}
}

// A call whose keys are not the finish's, or with neither of a one-of
// pair, is refused naming the shape; nothing is recorded.
func TestWrongKeysAreRefused(t *testing.T) {
	h, _ := New("runsheet/goal-chat", FinishAnswerOrProposal, "")
	for _, args := range []map[string]any{
		{"verdict": map[string]any{"passed": true}},
		{"answer": ""},
		{},
		{"answer": 7},
	} {
		if _, err := h.Call(args); err == nil || !strings.Contains(err.Error(), "{answer and/or proposal}") {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if _, ok := h.Result(); ok {
		t.Fatal("a refused call was recorded")
	}
	v, _ := New("runsheet/evaluate", FinishVerdict, "")
	if _, err := v.Call(map[string]any{"verdict": "PASS"}); err == nil || !strings.Contains(err.Error(), "`verdict` is required, an object") {
		t.Fatalf("a verdict that is no object: %v", err)
	}
	if _, err := v.Call(map[string]any{"verdict": map[string]any{"passed": true}, "notes": "x"}); err == nil || !strings.Contains(err.Error(), `"notes" is not one of its keys`) {
		t.Fatalf("a stray key: %v", err)
	}
}

// A proposal is validated against the kind's finish_schema: one that
// does not fit is refused naming the field, as the gateway would refuse
// it, so the session corrects it before the node ever sends it.
func TestProposalIsValidatedAgainstTheFinishSchema(t *testing.T) {
	h, err := New("wardroom/thread", FinishAnswerOrProposal, cardSchema)
	if err != nil {
		t.Fatal(err)
	}
	if !h.HasSchema() {
		t.Fatal("no schema")
	}
	_, err = h.Call(map[string]any{"proposal": map[string]any{"slug": "x", "colour": "red"}})
	if err == nil || !strings.Contains(err.Error(), "does not fit the kind's finish_schema") || !strings.Contains(err.Error(), "colour") {
		t.Fatalf("a proposal missing title and carrying colour: %v", err)
	}
	if _, err := h.Call(map[string]any{"proposal": "not json"}); err == nil || !strings.Contains(err.Error(), "is not JSON") {
		t.Fatalf("a proposal that is no JSON: %v", err)
	}
	if _, err := h.Call(map[string]any{"proposal": `{"slug": "x", "title": "Card"}`}); err != nil {
		t.Fatalf("a fitting proposal as text: %v", err)
	}
	if _, err := h.Call(map[string]any{"answer": "no draft"}); err != nil {
		t.Fatalf("an answer alone needs no proposal: %v", err)
	}
	// the tool's input schema carries the product's schema as the
	// proposal's, so the session sees the shape before it calls
	tool, _ := mcptool.Find(h.Tools(), ResultTool)
	props := tool.Schema["properties"].(map[string]any)
	if props["proposal"].(map[string]any)["required"] == nil {
		t.Fatalf("the proposal's schema is not the product's: %v", props["proposal"])
	}
	if _, err := New("wardroom/thread", FinishAnswerOrProposal, "[1]"); err == nil || !strings.Contains(err.Error(), "finish_schema is not a JSON Schema object") {
		t.Fatalf("a schema that is no object: %v", err)
	}
	if _, err := New("runsheet/evaluate", FinishVerdict, cardSchema); err == nil || !strings.Contains(err.Error(), "takes no product schema") {
		t.Fatalf("a schema on a verdict: %v", err)
	}
	if _, err := New("x/y", "verdict-ish", ""); err == nil {
		t.Fatal("an unknown finish made a holder")
	}
}

// mesh_fail records why the job cannot be done and replaces a result;
// the tools answer through the MCP handler as a session reaches them.
func TestFailAndTheToolsThroughTheHandler(t *testing.T) {
	h, _ := New("runsheet/rebase", FinishRebasedHead, "")
	srv := &mcptool.Handler{Name: NodeServer, Tools: h.Tools()}
	call := func(name string, args map[string]any) (map[string]any, bool) {
		req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
		reply, ok := srv.Answer(context.Background(), req)
		if !ok {
			t.Fatalf("%s: no reply", name)
		}
		var out struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(reply, &out); err != nil {
			t.Fatal(err)
		}
		return map[string]any{"text": out.Result.Content[0].Text}, out.Result.IsError
	}
	if r, isErr := call(ResultTool, map[string]any{"rebasedHead": map[string]any{"headSha": "abc"}}); isErr || !strings.HasPrefix(r["text"].(string), "recorded") {
		t.Fatalf("result: %v %v", r, isErr)
	}
	if r, isErr := call(FailTool, map[string]any{}); !isErr || !strings.Contains(r["text"].(string), "takes `error`") {
		t.Fatalf("a fail with no error: %v %v", r, isErr)
	}
	if _, isErr := call(FailTool, map[string]any{"error": "the branch moved under me"}); isErr {
		t.Fatal("fail refused")
	}
	if _, ok := h.Result(); ok {
		t.Fatal("the failure did not replace the result")
	}
	if why, ok := h.Failed(); !ok || why != "the branch moved under me" {
		t.Fatalf("failed %q %v", why, ok)
	}
	if _, err := h.Call(map[string]any{"rebasedHead": map[string]any{"headSha": "abc"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.Failed(); ok {
		t.Fatal("the result did not replace the failure")
	}
	// the list a client reads
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	reply, _ := srv.Answer(context.Background(), req)
	if s := string(reply); !strings.Contains(s, `"name":"mesh_result"`) || !strings.Contains(s, `"name":"mesh_fail"`) || !strings.Contains(s, "rebasedHead") {
		t.Fatalf("tools/list: %s", s)
	}
}

// The finish section names the tool, the shape and its keys, and the
// schema when the product gave one.
func TestFinishSection(t *testing.T) {
	h, _ := New("wardroom/thread", FinishAnswerOrProposal, cardSchema)
	s := h.FinishSection()
	for _, want := range []string{"# How this job finishes", "`mesh_result` tool once", "{answer and/or proposal}", "`answer`:", "`proposal`:", "input schema", "`mesh_fail(error=<why>)`", "nothing in your final message is read"} {
		if !strings.Contains(s, want) {
			t.Fatalf("finish section lacks %q:\n%s", want, s)
		}
	}
	v, _ := New("runsheet/evaluate", FinishVerdict, "")
	if s := v.FinishSection(); !strings.Contains(s, "{verdict}") || !strings.Contains(s, "failedClauses") || strings.Contains(s, "input schema") {
		t.Fatalf("verdict section:\n%s", s)
	}
	if sh, _ := ShapeOf(FinishChangeset); sh.String() != "{changeset, attestation?}" {
		t.Fatalf("shape %s", sh)
	}
}
