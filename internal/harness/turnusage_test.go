package harness

import (
	"os"
	"strings"
	"testing"
)

// Three assistant turns, each carrying its own message.usage, give
// Result.Turns from the result event's num_turns, and
// InputPerTurnMean/Max computed across the turns' own input (input +
// cache_read + cache_creation); the two tool_result blocks' content
// lengths land on LargestToolResults, largest first, named by their
// tool_use block's own name.
func TestParseStreamTurnUsage(t *testing.T) {
	f, err := os.Open("testdata/stream_turn_usage.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	res := ParseStream(f, nil)
	if !res.Success {
		t.Fatalf("result = %+v", res)
	}
	if res.Turns != 3 {
		t.Fatalf("Turns = %d, want 3", res.Turns)
	}
	wantMean := int64((150 + 300 + 50) / 3)
	if res.InputPerTurnMean != wantMean {
		t.Errorf("InputPerTurnMean = %d, want %d", res.InputPerTurnMean, wantMean)
	}
	if res.InputPerTurnMax != 300 {
		t.Errorf("InputPerTurnMax = %d, want 300", res.InputPerTurnMax)
	}
	if len(res.LargestToolResults) != 2 {
		t.Fatalf("LargestToolResults = %+v, want 2 rows", res.LargestToolResults)
	}
	if got := res.LargestToolResults[0]; got.Tool != "mcp__search__query" || got.Bytes != 500 {
		t.Errorf("LargestToolResults[0] = %+v, want {mcp__search__query 500}", got)
	}
	if got := res.LargestToolResults[1]; got.Tool != "Bash" || got.Bytes != 100 {
		t.Errorf("LargestToolResults[1] = %+v, want {Bash 100}", got)
	}
}

// Two assistant events that share one message.id — a content block
// each, as a CLI streaming partial messages could send — count as one
// turn, not two: the second event's usage does not add to the mean or
// the max. A third event with no id, or a different one, still counts.
func TestParseStreamCountsATurnOncePerMessageID(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s-dup","model":"claude-sonnet-5"}`,
		`{"type":"assistant","session_id":"s-dup","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"Looking."}],"usage":{"input_tokens":100,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":5}}}`,
		`{"type":"assistant","session_id":"s-dup","message":{"id":"msg_1","role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}],"usage":{"input_tokens":100,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":5}}}`,
		`{"type":"user","session_id":"s-dup","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"x"}]}}`,
		`{"type":"assistant","session_id":"s-dup","message":{"id":"msg_2","role":"assistant","content":[{"type":"text","text":"Done."}],"usage":{"input_tokens":300,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":5}}}`,
		`{"type":"result","subtype":"success","session_id":"s-dup","is_error":false,"result":"Done.","total_cost_usd":0.1,"num_turns":2}`,
	}, "\n")
	res := ParseStream(strings.NewReader(stream), nil)
	if res.InputPerTurnMean != 200 {
		t.Errorf("InputPerTurnMean = %d, want 200 ((100+300)/2, msg_1's second event not double-counted)", res.InputPerTurnMean)
	}
	if res.InputPerTurnMax != 300 {
		t.Errorf("InputPerTurnMax = %d, want 300", res.InputPerTurnMax)
	}
}

// turn.completed's usage is the thread's running total: two turns'
// input_tokens of 1000 then 1500 give shares of 1000 and 500, so
// InputPerTurnMean is 750 and InputPerTurnMax 1000. The mcp_tool_call's
// own result keeps its byte length on LargestToolResults, named
// mcp__<server>__<tool>.
func TestParseCodexStreamTurnUsage(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"thread.started","thread_id":"th-turns"}`,
		`{"type":"item.started","item":{"id":"i1","type":"mcp_tool_call","server":"search","tool":"query","status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"i1","type":"mcp_tool_call","server":"search","tool":"query","status":"completed","result":{"text":"0123456789"}}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1000,"output_tokens":10}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1500,"output_tokens":10}}`,
	}, "\n")
	res := ParseCodexStream(strings.NewReader(stream), "gpt-6-astra", nil)
	if res.Turns != 2 {
		t.Fatalf("Turns = %d, want 2", res.Turns)
	}
	if res.InputPerTurnMean != 750 {
		t.Errorf("InputPerTurnMean = %d, want 750", res.InputPerTurnMean)
	}
	if res.InputPerTurnMax != 1000 {
		t.Errorf("InputPerTurnMax = %d, want 1000", res.InputPerTurnMax)
	}
	if len(res.LargestToolResults) != 1 {
		t.Fatalf("LargestToolResults = %+v, want 1 row", res.LargestToolResults)
	}
	if got := res.LargestToolResults[0]; got.Tool != "mcp__search__query" || got.Bytes != len(`{"text":"0123456789"}`) {
		t.Errorf("LargestToolResults[0] = %+v", got)
	}
}
