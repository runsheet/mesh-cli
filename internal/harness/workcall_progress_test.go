package harness

import (
	"bytes"
	"os"
	"testing"
)

// TestProgressCarriesEveryWorkCall: each mesh-work tool call reaches
// onProgress as the stream carries it, one Progress per call and in
// order, its input byte for byte as the stream had it (the fixtures'
// odd spacing and key order would not survive a re-marshal), and a
// work-call Progress carries no text, cost, session id or rate limit.
// Any other tool call (mesh_document, Bash) yields none.
func TestProgressCarriesEveryWorkCall(t *testing.T) {
	want := []WorkCall{
		{Tool: "mesh_progress", Input: []byte(`{"note": "the failing test is written",  "item_id":"eeeeeeee-1111-4111-8111-eeeeeeeeeeee"}`)},
		{Tool: "mesh_progress", Input: []byte(`{"item_id":"eeeeeeee-1111-4111-8111-eeeeeeeeeeee","note":"parser emits é calls\nsecond line"}`)},
		{Tool: "mesh_ask", Input: []byte(`{ "item_id" : "eeeeeeee-1111-4111-8111-eeeeeeeeeeee", "question":"which fixture?" }`)},
		{Tool: "mesh_progress", Input: []byte(`{"item_id":"eeeeeeee-1111-4111-8111-eeeeeeeeeeee","note":"state: done the parser; next the node; open none","extra":[1, 2]}`)},
	}
	runs := map[string]func(t *testing.T, path string, onProgress func(Progress)) Result{
		"claude": func(t *testing.T, path string, onProgress func(Progress)) Result {
			f := openFixture(t, path)
			return parseClaude(f, onProgress, nil)
		},
		"codex": func(t *testing.T, path string, onProgress func(Progress)) Result {
			f := openFixture(t, path)
			res, _ := (&Codex{}).readStream(f, Job{Model: "gpt-6-sol"}, onProgress)
			return res
		},
	}
	fixtures := map[string]string{
		"claude": "testdata/stream_work_calls.jsonl",
		"codex":  "testdata/codex_work_calls.jsonl",
	}
	for name, run := range runs {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(fixtures[name])
			if err != nil {
				t.Fatal(err)
			}
			var got []WorkCall
			var texts []string
			res := run(t, fixtures[name], func(p Progress) {
				if p.WorkCall == nil {
					if p.Text != "" {
						texts = append(texts, p.Text)
					}
					return
				}
				if p.Text != "" || p.CostUSD != 0 || p.SessionID != "" || p.RateLimit != nil {
					t.Errorf("work-call progress carries more than the call: %+v", p)
				}
				got = append(got, *p.WorkCall)
			})
			if len(got) != len(want) {
				t.Fatalf("got %d work-call progress values, want %d: %+v", len(got), len(want), got)
			}
			for i, w := range want {
				if got[i].Tool != w.Tool {
					t.Errorf("call %d: tool %q, want %q", i, got[i].Tool, w.Tool)
				}
				if !bytes.Equal(got[i].Input, w.Input) {
					t.Errorf("call %d: input\n %s\nwant\n %s", i, got[i].Input, w.Input)
				}
				if !bytes.Contains(raw, got[i].Input) {
					t.Errorf("call %d: input not byte-equal to the stream's: %s", i, got[i].Input)
				}
			}
			for _, txt := range texts {
				if bytes.Contains([]byte(txt), []byte("item_id")) {
					t.Errorf("a work call leaked into the spoken text: %q", txt)
				}
			}
			// Result.WorkCalls still keeps the last input per tool
			if note := res.WorkCalls["mesh_progress"]["note"]; note != "state: done the parser; next the node; open none" {
				t.Errorf("WorkCalls[mesh_progress].note = %v", note)
			}
			if q := res.WorkCalls["mesh_ask"]["question"]; q != "which fixture?" {
				t.Errorf("WorkCalls[mesh_ask].question = %v", q)
			}
		})
	}
}

func openFixture(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}
