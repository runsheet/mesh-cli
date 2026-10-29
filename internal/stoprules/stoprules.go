// Package stoprules checks a kind's stop rules over a session's own
// stream-json, on the node, and counts what Mesh learns from without
// content (projectmesh-io/api docs/adr/adr-001-amendment-learnings-and-
// transcripts.md, "Live rules run on the node" and "Content-free counts
// Mesh adds per lease"). The stream never leaves the node for this.
//
// A Watch hears every event of the session (Observe) and the passing of
// time (Tick), and answers the rules that fire:
//
//   - tool_error_loop: count tool errors (a tool_result with is_error) —
//     of the rule's tool alone, when it names one — within its window;
//   - protected_paths: an Edit, Write, MultiEdit or NotebookEdit tool
//     call whose file, relative to the session's directory, matches a
//     glob (`**` crosses directories);
//   - budget: the session's tokens (input, output and cache, summed over
//     its assistant messages, or the result's modelUsage when more) or
//     its cost (the result's total_cost_usd) reaching the figure;
//   - no_progress: no tool call that succeeded for the rule's time.
//
// A rule that fires is reported once for each time it fires. An end
// rule fires once; an escalate rule re-arms — its window starts again —
// so a session it moved can be caught again. What the node does about
// a firing is the caller's.
package stoprules

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// EditTools are the tools that write a file the session names, and the
// input field each names it in.
var EditTools = map[string]string{"Edit": "file_path", "Write": "file_path", "MultiEdit": "file_path", "NotebookEdit": "notebook_path"}

// Fired is one rule firing: the rule and what caught it, in words that
// name no path, argument or output — the reason Mesh keeps.
type Fired struct {
	Rule   meshapi.StopRule
	Detail string
}

// Reason is the job's reason for a rule's end: the rule's name first.
func (f Fired) Reason() string { return f.Rule.Name + ": " + f.Detail }

// Watch is one lease's session under its kind's rules. It is safe for
// concurrent use: Observe runs on the session's reading goroutine, Tick
// on a timer's.
type Watch struct {
	rules     []meshapi.StopRule
	workspace []string

	mu      sync.Mutex
	start   time.Time
	uses    map[string]string // tool_use id -> tool name
	errs    map[string][]time.Time
	quietAt map[string]time.Time // no_progress: when each rule's quiet began
	spent   map[string]bool      // end rules that fired, budget rules that fired
	msgs    map[string]bool      // assistant message ids whose usage is counted
	tokens  int64
	result  int64 // the newest result's modelUsage tokens
	cents   int64

	calls, errors int64
	tools         map[string]*meshapi.ToolCount
	edited        map[string]bool
	firstTool     time.Duration
	fired         []string
}

// New watches a session started at start in workspace under rules.
func New(rules []meshapi.StopRule, workspace string, start time.Time) *Watch {
	w := &Watch{rules: rules, start: start, uses: map[string]string{}, errs: map[string][]time.Time{},
		quietAt: map[string]time.Time{}, spent: map[string]bool{}, msgs: map[string]bool{},
		tools: map[string]*meshapi.ToolCount{}, edited: map[string]bool{}, firstTool: -1}
	if workspace != "" {
		w.workspace = append(w.workspace, filepath.Clean(workspace))
		if real, err := filepath.EvalSymlinks(workspace); err == nil && real != filepath.Clean(workspace) {
			w.workspace = append(w.workspace, real)
		}
	}
	for _, r := range rules {
		if r.NoProgress != nil {
			w.quietAt[r.Name] = start
		}
	}
	return w
}

// Observe hears one event of the session at now and answers the rules
// it fires.
func (w *Watch) Observe(ev harness.Event, now time.Time) []Fired {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []Fired
	if ev.Message != nil {
		if ev.Type == "assistant" && ev.Message.Usage != nil && ev.Message.ID != "" && !w.msgs[ev.Message.ID] {
			w.msgs[ev.Message.ID] = true
			u := ev.Message.Usage
			w.tokens += u.InputTokens + u.OutputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
		}
		for _, c := range ev.Message.Content {
			switch c.Type {
			case "tool_use":
				out = append(out, w.toolUse(c.ID, c.Name, c.Input, now)...)
			case "tool_result":
				out = append(out, w.toolResult(c.ToolUseID, c.IsError, now)...)
			}
		}
	}
	if ev.Type == "result" {
		var tokens int64
		for _, m := range ev.ModelUsage {
			tokens += m.InputTokens + m.OutputTokens + m.CacheReadInputTokens + m.CacheCreationInputTokens
		}
		w.result = max(w.result, tokens)
		w.cents = max(w.cents, int64(math.Round(ev.CostUSD*100)))
	}
	return append(out, w.budget()...)
}

// Tick answers the rules that fired by the passing of time alone: a
// session quiet past a no_progress rule's time.
func (w *Watch) Tick(now time.Time) []Fired {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []Fired
	for _, r := range w.rules {
		c := r.NoProgress
		if c == nil || w.spent[r.Name] || c.ForS <= 0 {
			continue
		}
		for_ := time.Duration(c.ForS) * time.Second
		if now.Sub(w.quietAt[r.Name]) < for_ {
			continue
		}
		w.quietAt[r.Name] = now
		out = append(out, w.fire(r, fmt.Sprintf("no tool call succeeded for %s", for_)))
	}
	return out
}

// toolUse counts a tool call and checks the protected paths.
func (w *Watch) toolUse(id, name string, input json.RawMessage, now time.Time) []Fired {
	if name == "" {
		return nil
	}
	if id != "" {
		w.uses[id] = name
	}
	w.calls++
	w.tool(name).Calls++
	if w.firstTool < 0 {
		w.firstTool = max(now.Sub(w.start), 0)
	}
	field, edits := EditTools[name]
	if !edits {
		return nil
	}
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	file, _ := in[field].(string)
	if file == "" {
		return nil
	}
	rel, inside := w.relative(file)
	if !inside {
		w.edited[file] = true
		return nil
	}
	w.edited[rel] = true
	var out []Fired
	for _, r := range w.rules {
		c := r.ProtectedPaths
		if c == nil || w.spent[r.Name] {
			continue
		}
		for _, g := range c.Globs {
			if Match(g, rel) {
				out = append(out, w.fire(r, fmt.Sprintf("%s wrote to a protected path (%s)", name, g)))
				break
			}
		}
	}
	return out
}

// toolResult counts an error against its call's tool, and a success as
// progress.
func (w *Watch) toolResult(id string, isError bool, now time.Time) []Fired {
	name := w.uses[id]
	if name == "" {
		name = "unknown"
	}
	if !isError {
		for _, r := range w.rules {
			if r.NoProgress != nil {
				w.quietAt[r.Name] = now
			}
		}
		return nil
	}
	w.errors++
	w.tool(name).Errors++
	var out []Fired
	for _, r := range w.rules {
		c := r.ToolErrorLoop
		if c == nil || w.spent[r.Name] || c.Count < 1 || !sameTool(c.Tool, name) {
			continue
		}
		errs := append(w.errs[r.Name], now)
		if c.WindowS > 0 {
			from := now.Add(-time.Duration(c.WindowS) * time.Second)
			errs = slices.DeleteFunc(errs, func(t time.Time) bool { return t.Before(from) })
		}
		w.errs[r.Name] = errs
		if len(errs) < c.Count {
			continue
		}
		w.errs[r.Name] = nil
		of := "tool errors"
		if c.Tool != "" {
			of = "errors of " + c.Tool
		}
		within := "in the session"
		if c.WindowS > 0 {
			within = "within " + (time.Duration(c.WindowS) * time.Second).String()
		}
		out = append(out, w.fire(r, fmt.Sprintf("%d %s %s", len(errs), of, within)))
	}
	return out
}

// budget answers the budget rules the session's spend has reached.
func (w *Watch) budget() []Fired {
	var out []Fired
	tokens := max(w.tokens, w.result)
	for _, r := range w.rules {
		c := r.Budget
		if c == nil || w.spent[r.Name] {
			continue
		}
		switch {
		case c.Tokens > 0 && tokens >= c.Tokens:
			w.spent[r.Name] = true
			out = append(out, w.fire(r, fmt.Sprintf("the session's tokens reached %d of %d", tokens, c.Tokens)))
		case c.CostCents > 0 && w.cents >= c.CostCents:
			w.spent[r.Name] = true
			out = append(out, w.fire(r, fmt.Sprintf("the session's cost reached %d of %d cents", w.cents, c.CostCents)))
		}
	}
	return out
}

// fire records r firing: an end rule is spent.
func (w *Watch) fire(r meshapi.StopRule, detail string) Fired {
	if r.Action != meshapi.StopActionEscalate {
		w.spent[r.Name] = true
	}
	w.fired = append(w.fired, r.Name)
	return Fired{Rule: r, Detail: detail}
}

func (w *Watch) tool(name string) *meshapi.ToolCount {
	t := w.tools[name]
	if t == nil {
		t = &meshapi.ToolCount{Tool: name}
		w.tools[name] = t
	}
	return t
}

// relative is file relative to the session's directory, and whether it
// is inside it.
func (w *Watch) relative(file string) (string, bool) {
	if !filepath.IsAbs(file) {
		rel := filepath.ToSlash(filepath.Clean(file))
		return rel, rel != ".." && !strings.HasPrefix(rel, "../")
	}
	for _, ws := range w.workspace {
		if rel, err := filepath.Rel(ws, filepath.Clean(file)); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return filepath.ToSlash(rel), true
		}
	}
	return "", false
}

// sameTool is whether a rule's tool names the tool called: the same
// name, or an MCP tool's own name (mcp__<server>__<tool>) as the rule
// names it bare.
func sameTool(rule, called string) bool {
	return rule == "" || rule == called || strings.HasSuffix(called, "__"+rule) && strings.HasPrefix(called, "mcp__")
}

// Counts are the session's content-free counts so far: tool calls and
// errors, in all and by tool name, files edited (never their paths),
// the time to the first tool call and the rules that fired.
func (w *Watch) Counts() meshapi.Counts {
	w.mu.Lock()
	defer w.mu.Unlock()
	c := meshapi.Counts{ToolCalls: w.calls, ToolErrors: w.errors, Edits: int64(len(w.edited)), StopRules: slices.Clone(w.fired)}
	if w.firstTool >= 0 {
		c.FirstToolMS = max(w.firstTool.Milliseconds(), 1)
	}
	for _, t := range w.tools {
		c.Tools = append(c.Tools, *t)
	}
	sort.Slice(c.Tools, func(i, j int) bool { return c.Tools[i].Tool < c.Tools[j].Tool })
	return c
}
