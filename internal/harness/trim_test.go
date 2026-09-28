package harness

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// A 100 KB result under a 16 KB cap keeps its first and last lines and
// says what went, in under 17 KB.
func TestTrimToolResult(t *testing.T) {
	var b strings.Builder
	for i := 0; b.Len() < 100*1024; i++ {
		fmt.Fprintf(&b, "line %05d of the test log, with some padding to make it longer\n", i)
	}
	in := b.String()
	lines := strings.Split(strings.TrimSuffix(in, "\n"), "\n")
	got := TrimToolResult(in, 16*1024)
	if len(got) >= 17*1024 {
		t.Errorf("trimmed to %d bytes, want under 17 KB", len(got))
	}
	if !strings.HasPrefix(got, lines[0]+"\n") {
		t.Errorf("does not start with the first line: %q", got[:80])
	}
	if !strings.HasSuffix(got, lines[len(lines)-1]+"\n") {
		t.Errorf("does not end with the last line: %q", got[len(got)-80:])
	}
	if !strings.Contains(got, "\n[mesh: trimmed 84 KB of 100 KB; run the command again with a narrower filter if the middle matters]\n") {
		t.Errorf("no trim note: %s", got[9000:10500])
	}
	// every line kept is whole
	for _, l := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if !strings.HasPrefix(l, "line ") && !strings.HasPrefix(l, "[mesh: trimmed") {
			t.Errorf("a cut line: %q", l)
		}
	}
}

func TestTrimToolResultUnderTheCap(t *testing.T) {
	in := strings.Repeat("ok\n", 1000)
	if got := TrimToolResult(in, 16*1024); got != in {
		t.Error("a result under the cap was changed")
	}
	if got := TrimToolResult(strings.Repeat("x", 100000), 0); len(got) != 100000 {
		t.Error("a cap of 0 trimmed")
	}
}

// One long line has no boundary to cut at: it is cut at the byte, never
// inside a rune.
func TestTrimToolResultOneLine(t *testing.T) {
	in := strings.Repeat("é", 20000)
	got := TrimToolResult(in, 1000)
	if len(got) > 1200 || !strings.Contains(got, "[mesh: trimmed") {
		t.Fatalf("got %d bytes: %q", len(got), got)
	}
	if !strings.HasPrefix(got, "é") || !strings.HasSuffix(got, "é") {
		t.Errorf("a split rune: %q … %q", got[:4], got[len(got)-4:])
	}
	for _, r := range got {
		if r == '�' {
			t.Fatal("invalid UTF-8 in the result")
		}
	}
}

// The hook wraps a Bash command, keeps the rest of its input, and leaves
// alone what it must not touch.
func TestTrimHook(t *testing.T) {
	out, ok, err := TrimHook([]byte(`{"tool_name":"Bash","tool_input":{"command":"go test ./... # all","timeout":600000}}`), "/home/me/bin/mesh", 16)
	if err != nil || !ok {
		t.Fatalf("ok %v, err %v", ok, err)
	}
	var got struct {
		HookSpecificOutput struct {
			HookEventName string         `json:"hookEventName"`
			UpdatedInput  map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	h := got.HookSpecificOutput
	if h.HookEventName != "PreToolUse" {
		t.Errorf("hookEventName %q", h.HookEventName)
	}
	if want := "( go test ./... # all\n) 2>&1 | /home/me/bin/mesh tool-trim --max-kb 16"; h.UpdatedInput["command"] != want {
		t.Errorf("command %q, want %q", h.UpdatedInput["command"], want)
	}
	if h.UpdatedInput["timeout"] != float64(600000) {
		t.Errorf("timeout lost: %v", h.UpdatedInput)
	}
	for _, in := range []string{
		`{"tool_name":"Read","tool_input":{"file_path":"x"}}`,
		`{"tool_name":"Bash","tool_input":{"command":"make | mesh tool-trim --max-kb 8"}}`,
		`{"tool_name":"Bash","tool_input":{"command":"npm run dev","run_in_background":true}}`,
	} {
		if _, ok, err := TrimHook([]byte(in), "mesh", 16); ok || err != nil {
			t.Errorf("%s: rewritten (err %v)", in, err)
		}
	}
	if got := ToolTrimCommand("/my bin/mesh", 32); got != `'/my bin/mesh' tool-trim --max-kb 32` {
		t.Errorf("quoted %q", got)
	}
}
