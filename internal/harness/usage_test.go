package harness

import (
	"os"
	"testing"
)

// A result event's usage reaches Result.Usage beside CostUSD: the input
// counted with its cache reads and writes, each also on its own, the
// output, and the model the init event named.
func TestParseStreamUsage(t *testing.T) {
	f, err := os.Open("testdata/stream_usage.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	res := ParseStream(f, nil)
	if !res.Success || res.CostUSD != 0.4321 {
		t.Fatalf("result = %+v", res)
	}
	want := Usage{Model: "claude-sonnet-5", InputTokens: 1200 + 3400 + 56000, CachedInputTokens: 56000, CacheWriteInputTokens: 3400, OutputTokens: 789}
	if res.Usage == nil || *res.Usage != want {
		t.Fatalf("Usage = %+v, want %+v", res.Usage, want)
	}
}

// A result without usage leaves Result.Usage nil, not zero.
func TestParseStreamNoUsage(t *testing.T) {
	f, err := os.Open("testdata/stream_rate_limit.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if res := ParseStream(f, nil); res.Usage != nil {
		t.Fatalf("Usage = %+v, want nil", res.Usage)
	}
}
