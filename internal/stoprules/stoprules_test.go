package stoprules

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// The stream-json a session prints, as claude does.
func use(id, tool string, input map[string]any) harness.Event {
	return event(map[string]any{"type": "assistant", "message": map[string]any{"id": "msg_" + id, "role": "assistant",
		"content": []map[string]any{{"type": "tool_use", "id": id, "name": tool, "input": input}}}})
}

func result(id string, isError bool) harness.Event {
	return event(map[string]any{"type": "user", "message": map[string]any{"role": "user",
		"content": []map[string]any{{"type": "tool_result", "tool_use_id": id, "content": "out", "is_error": isError}}}})
}

func spend(id string, tokens int64) harness.Event {
	return event(map[string]any{"type": "assistant", "message": map[string]any{"id": id, "role": "assistant",
		"usage": map[string]any{"input_tokens": tokens / 2, "output_tokens": tokens / 2}, "content": []map[string]any{{"type": "text", "text": "x"}}}})
}

func event(v any) harness.Event {
	raw, _ := json.Marshal(v)
	var ev harness.Event
	if err := json.Unmarshal(raw, &ev); err != nil {
		panic(err)
	}
	return ev
}

var t0 = time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)

// step is one thing the session does at a moment after it started.
type step struct {
	at   time.Duration
	ev   *harness.Event
	tick bool
}

func at(d time.Duration, ev harness.Event) step { return step{at: d, ev: &ev} }
func tickAt(d time.Duration) step               { return step{at: d, tick: true} }

// run plays steps through a watch and answers the reasons fired.
func run(rules []meshapi.StopRule, workspace string, steps ...step) ([]string, *Watch) {
	w := New(rules, workspace, t0)
	var fired []string
	for _, s := range steps {
		var got []Fired
		if s.tick {
			got = w.Tick(t0.Add(s.at))
		} else {
			got = w.Observe(*s.ev, t0.Add(s.at))
		}
		for _, f := range got {
			fired = append(fired, f.Rule.Action+" "+f.Reason())
		}
	}
	return fired, w
}

// TestRules: each rule fires on what it names and nothing else.
func TestRules(t *testing.T) {
	loop := meshapi.StopRule{Name: "loop", Action: "end", ToolErrorLoop: &meshapi.ToolErrorLoop{Count: 3, WindowS: 60}}
	bashLoop := meshapi.StopRule{Name: "bash", Action: "end", ToolErrorLoop: &meshapi.ToolErrorLoop{Tool: "Bash", Count: 2}}
	submitLoop := meshapi.StopRule{Name: "submit", Action: "end", ToolErrorLoop: &meshapi.ToolErrorLoop{Tool: "submit", Count: 2}}
	protected := meshapi.StopRule{Name: "migrations", Action: "end", ProtectedPaths: &meshapi.ProtectedPaths{Globs: []string{"db/migrations/**", "**/*.lock"}}}
	tokens := meshapi.StopRule{Name: "tokens", Action: "end", Budget: &meshapi.BudgetRule{Tokens: 1000}}
	cost := meshapi.StopRule{Name: "cost", Action: "end", Budget: &meshapi.BudgetRule{CostCents: 50}}
	quiet := meshapi.StopRule{Name: "quiet", Action: "escalate", NoProgress: &meshapi.NoProgress{ForS: 600}}
	errs := func(tool string, n int, every time.Duration) []step {
		var out []step
		for i := range n {
			id := fmt.Sprintf("%s-%d", tool, i)
			out = append(out, at(time.Duration(i)*every, use(id, tool, nil)), at(time.Duration(i)*every+time.Second, result(id, true)))
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		rules []meshapi.StopRule
		steps []step
		want  []string
	}{
		{"three errors in a minute", []meshapi.StopRule{loop}, errs("Bash", 3, 10*time.Second),
			[]string{"end loop: 3 tool errors within 1m0s"}},
		{"errors spread past the window", []meshapi.StopRule{loop}, errs("Bash", 4, 40*time.Second), nil},
		{"an end rule fires once", []meshapi.StopRule{loop}, errs("Bash", 7, time.Second),
			[]string{"end loop: 3 tool errors within 1m0s"}},
		{"another tool's errors", []meshapi.StopRule{bashLoop}, errs("Read", 5, time.Second), nil},
		{"the named tool's errors", []meshapi.StopRule{bashLoop}, errs("Bash", 2, time.Second),
			[]string{"end bash: 2 errors of Bash in the session"}},
		{"an MCP tool by its own name", []meshapi.StopRule{submitLoop}, errs("mcp__conform__submit", 2, time.Second),
			[]string{"end submit: 2 errors of submit in the session"}},
		{"successes are no errors", []meshapi.StopRule{loop},
			[]step{at(0, use("a", "Bash", nil)), at(time.Second, result("a", false)), at(2*time.Second, use("b", "Bash", nil)), at(3*time.Second, result("b", false))}, nil},
		{"an edit of a protected path", []meshapi.StopRule{protected},
			[]step{at(0, use("a", "Edit", map[string]any{"file_path": "/work/db/migrations/001.sql"}))},
			[]string{"end migrations: Edit wrote to a protected path (db/migrations/**)"}},
		{"a write anywhere matching", []meshapi.StopRule{protected},
			[]step{at(0, use("a", "Write", map[string]any{"file_path": "/work/web/package.lock"}))},
			[]string{"end migrations: Write wrote to a protected path (**/*.lock)"}},
		{"a relative multi-edit", []meshapi.StopRule{protected},
			[]step{at(0, use("a", "MultiEdit", map[string]any{"file_path": "db/migrations/x/y.sql"}))},
			[]string{"end migrations: MultiEdit wrote to a protected path (db/migrations/**)"}},
		{"an edit elsewhere", []meshapi.StopRule{protected},
			[]step{at(0, use("a", "Edit", map[string]any{"file_path": "/work/db/schema.sql"})), at(0, use("b", "Edit", map[string]any{"file_path": "/elsewhere/db/migrations/1.sql"}))}, nil},
		{"a read of a protected path", []meshapi.StopRule{protected},
			[]step{at(0, use("a", "Read", map[string]any{"file_path": "/work/db/migrations/001.sql"}))}, nil},
		{"tokens reached", []meshapi.StopRule{tokens},
			[]step{at(0, spend("m1", 600)), at(time.Second, spend("m1", 600)), at(2*time.Second, spend("m2", 600)), at(3*time.Second, spend("m3", 600))},
			[]string{"end tokens: the session's tokens reached 1200 of 1000"}},
		{"cost reached", []meshapi.StopRule{cost},
			[]step{at(0, event(map[string]any{"type": "result", "total_cost_usd": 0.2})), at(time.Second, event(map[string]any{"type": "result", "total_cost_usd": 0.55}))},
			[]string{"end cost: the session's cost reached 55 of 50 cents"}},
		{"no progress, then again", []meshapi.StopRule{quiet},
			[]step{tickAt(5 * time.Minute), tickAt(10 * time.Minute), tickAt(15 * time.Minute), tickAt(20 * time.Minute)},
			[]string{"escalate quiet: no tool call succeeded for 10m0s", "escalate quiet: no tool call succeeded for 10m0s"}},
		{"progress holds it off", []meshapi.StopRule{quiet},
			[]step{at(8*time.Minute, use("a", "Bash", nil)), at(9*time.Minute, result("a", false)), tickAt(10 * time.Minute), tickAt(18 * time.Minute)}, nil},
		{"failures are no progress", []meshapi.StopRule{quiet},
			[]step{at(8*time.Minute, use("a", "Bash", nil)), at(9*time.Minute, result("a", true)), tickAt(10 * time.Minute)},
			[]string{"escalate quiet: no tool call succeeded for 10m0s"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := run(tc.rules, "/work", tc.steps...)
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Fatalf("fired\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}

// An escalate loop rule re-arms: its window starts again, so the session
// it moved is caught again on as many more errors.
func TestEscalateRearms(t *testing.T) {
	r := meshapi.StopRule{Name: "loop", Action: "escalate", ToolErrorLoop: &meshapi.ToolErrorLoop{Count: 2}}
	var steps []step
	for i := range 5 {
		id := fmt.Sprint(i)
		steps = append(steps, at(time.Duration(i)*time.Second, use(id, "Bash", nil)), at(time.Duration(i)*time.Second, result(id, true)))
	}
	got, w := run([]meshapi.StopRule{r}, "/work", steps...)
	if len(got) != 2 {
		t.Fatalf("fired %v", got)
	}
	if c := w.Counts(); len(c.StopRules) != 2 || c.StopRules[0] != "loop" {
		t.Fatalf("the rules fired %v", c.StopRules)
	}
}

// TestCounts: the content-free counts — calls and errors in all and by
// tool name, files edited (each once), the time to the first tool call
// and the rules fired — and nothing of any argument or output.
func TestCounts(t *testing.T) {
	loop := meshapi.StopRule{Name: "loop", Action: "end", ToolErrorLoop: &meshapi.ToolErrorLoop{Count: 2}}
	_, w := run([]meshapi.StopRule{loop}, "/work",
		at(1500*time.Millisecond, use("a", "Edit", map[string]any{"file_path": "/work/a.go", "old_string": "SECRET-ARG"})), at(2*time.Second, result("a", false)),
		at(3*time.Second, use("b", "Edit", map[string]any{"file_path": "a.go"})), at(3*time.Second, result("b", false)),
		at(4*time.Second, use("c", "Write", map[string]any{"file_path": "/work/b.go"})), at(4*time.Second, result("c", false)),
		at(5*time.Second, use("d", "Bash", nil)), at(5*time.Second, result("d", true)),
		at(6*time.Second, use("e", "Bash", nil)), at(6*time.Second, result("e", true)))
	c := w.Counts()
	want := meshapi.Counts{ToolCalls: 5, ToolErrors: 2, Edits: 2, FirstToolMS: 1500, StopRules: []string{"loop"},
		Tools: []meshapi.ToolCount{{Tool: "Bash", Calls: 2, Errors: 2}, {Tool: "Edit", Calls: 2}, {Tool: "Write", Calls: 1}}}
	gotRaw, _ := json.Marshal(c)
	wantRaw, _ := json.Marshal(want)
	if string(gotRaw) != string(wantRaw) {
		t.Fatalf("counts %s\nwant %s", gotRaw, wantRaw)
	}
	if strings.Contains(string(gotRaw), "SECRET-ARG") || strings.Contains(string(gotRaw), "a.go") {
		t.Fatalf("the counts carry content: %s", gotRaw)
	}
	if c := New(nil, "", t0).Counts(); c.FirstToolMS != 0 || c.ToolCalls != 0 {
		t.Fatalf("a session with no tool call %+v", c)
	}
}

// TestMatch: `**` crosses directories, none included; other segments
// match as path.Match.
func TestMatch(t *testing.T) {
	for _, tc := range []struct {
		glob, name string
		want       bool
	}{
		{"db/**", "db/x", true},
		{"db/**", "db/a/b.sql", true},
		{"db/**", "db", true},
		{"db/**", "dbx/a", false},
		{"**/*.lock", "a/b/c.lock", true},
		{"**/*.lock", "c.lock", true},
		{"*.go", "a/b.go", false},
		{"cmd/*/main.go", "cmd/x/main.go", true},
		{"cmd/*/main.go", "cmd/x/y/main.go", false},
		{"a/**/z", "a/z", true},
		{"a/**/z", "a/b/c/z", true},
		{"[", "x", false},
	} {
		if got := Match(tc.glob, tc.name); got != tc.want {
			t.Errorf("Match(%q, %q) = %v", tc.glob, tc.name, got)
		}
	}
}
