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
