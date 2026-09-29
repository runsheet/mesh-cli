package harness

import (
	"bufio"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"time"
)

// CodexEvent is one line of `codex exec --json` (codex-cli 0.157.0,
// verified 2026-09-25; docs/codex-events.md). The stream is
// thread.started, turn.started, item.started|updated|completed…,
// then turn.completed or turn.failed; top-level `error` lines are
// retry notices along the way.
type CodexEvent struct {
	Type string `json:"type"`
	// ThreadID is on thread.started: the session id (`codex exec
	// resume <id>`).
	ThreadID string `json:"thread_id"`
	// Usage is on turn.completed: the thread's running total so far,
	// not the turn's own share.
	Usage *CodexUsage `json:"usage"`
	// Item is on item.started, item.updated and item.completed.
	Item *CodexItem `json:"item"`
	// Error is on turn.failed.
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	// Message is on a top-level error event.
	Message string `json:"message"`
}

// CodexItem is one thing a turn did. Type is agent_message, reasoning,
// command_execution, file_change, mcp_tool_call, collab_tool_call,
// web_search, todo_list or error; only the fields the node reads are
// kept here.
type CodexItem struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	// Text is an agent_message's or a reasoning item's text.
	Text string `json:"text"`
	// Message is an error item's text. Error items are warnings: the
	// turn goes on.
	Message string `json:"message"`
	// Command, AggregatedOutput and ExitCode are a command_execution's.
	Command          string `json:"command"`
	AggregatedOutput string `json:"aggregated_output"`
	ExitCode         *int   `json:"exit_code"`
	// Server and ToolName are an mcp_tool_call's MCP server and tool.
	Server   string `json:"server"`
	ToolName string `json:"tool"`
	// Error and Result are an mcp_tool_call's: error is
	// {"message": "…"} on a failed call (a bare string accepted too),
	// result {"content": [{"type": "text", "text": "…"}], …}; only
	// Result's byte length is read for LargestToolResults.
	Error  json.RawMessage `json:"error"`
	Result json.RawMessage `json:"result"`
	// Status is in_progress, completed or failed (declined, for a
	// command).
	Status string `json:"status"`
}

// CodexUsage is turn.completed's usage. InputTokens includes both
// CachedInputTokens (read from the cache) and CacheWriteInputTokens
// (written to it); OutputTokens includes ReasoningOutputTokens.
type CodexUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
}

// CodexPrice is a model's list price in USD per million tokens.
type CodexPrice struct {
	Input       float64
	CachedInput float64
	CacheWrite  float64
	Output      float64
}

// CodexPrices are OpenAI's standard (short-context) API list prices,
// from https://developers.openai.com/api/docs/pricing as read on
// 2026-09-25. Codex reports tokens, not dollars, so a session's cost
// is its usage at these. Long-context rates (a request over 272K input
// tokens) never apply: Codex caps the context at 258,400. A model with
// no cache-write price bills a cache write as input.
var CodexPrices = map[string]CodexPrice{
	"gpt-6-astra":   {Input: 10.00, CachedInput: 1.00, CacheWrite: 12.50, Output: 50.00},
	"gpt-6-sol":     {Input: 2.00, CachedInput: 0.20, CacheWrite: 2.50, Output: 10.00},
	"gpt-6-luna":    {Input: 0.10, CachedInput: 0.01, CacheWrite: 0.125, Output: 0.50},
	"gpt-5.3-codex": {Input: 1.75, CachedInput: 0.175, CacheWrite: 1.75, Output: 14.00},
}

// Cost is what usage costs at price p.
func (p CodexPrice) Cost(u CodexUsage) float64 {
	plain := u.InputTokens - u.CachedInputTokens - u.CacheWriteInputTokens
	if plain < 0 {
		plain = 0
	}
	return (float64(plain)*p.Input +
		float64(u.CachedInputTokens)*p.CachedInput +
		float64(u.CacheWriteInputTokens)*p.CacheWrite +
		float64(u.OutputTokens)*p.Output) / 1e6
}

// ParseCodexStream reads `codex exec --json` lines, calling onEvent for
// each, and returns the session's Result. The stream never names its
// model, so model is the one the session was started on (`-m`). Text is the last agent message; SessionID
// the thread id; Usage the last turn.completed's totals, and CostUSD
// those at CodexPrices[model] — an unknown model costs 0 with
// Usage.Unpriced set. Success is a turn.completed with no turn.failed.
// A stream with no parseable event sets EmptyOutput. Codex's exec
// stream carries no plan limits (docs/codex-events.md), so RateLimit
// stays nil. Each completed mcp_tool_call is counted on ToolCalls by
// its server, its item id (empty ones synthesised) kept on
// ToolCallIDs, and on Result.Tools: its wall time is item.started to
// item.completed, and a completed status of "failed" counts an error,
// the item's error text kept as the server's FirstError (else its
// result's text, else the status), and one with no error but a result
// counted Answered too (codexToolAnswered: the tool replied) — the call still counts on ToolCalls
// and Tools whatever its status. An error item or top-level error
// naming an MCP server that failed to start (mcpStartupServers) counts
// an error for that server too, the message as FirstError, even with
// zero calls — once per server. Its own result, when carried, keeps its
// byte length among Result.LargestToolResults, named
// mcp__<server>__<tool>. turn.completed's usage is the thread's running
// total, not the turn's own share: each turn's share is the delta from
// the previous turn.completed, and Result.InputPerTurnMean and
// InputPerTurnMax are computed across those shares. Lines that are not
// JSON and unknown types are skipped.
func ParseCodexStream(r io.Reader, model string, onEvent func(CodexEvent)) Result {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	var res Result
	var usage *CodexUsage
	completed, failed := false, false
	events := 0
	var prevInput, turnInputSum, turnInputMax int64
	turnCount := 0
	pending := map[string]time.Time{}
	startupFailed := map[string]bool{}
	startupError := func(msg string) {
		for _, server := range mcpStartupServers(msg) {
			if startupFailed[server] {
				continue
			}
			startupFailed[server] = true
			stat := res.toolStat(server)
			stat.Errors++
			if stat.FirstError == "" {
				stat.FirstError = capText(msg, 300)
			}
		}
	}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] != '{' {
			continue
		}
		var ev CodexEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		events++
		switch ev.Type {
		case "thread.started":
			if ev.ThreadID != "" {
				res.SessionID = ev.ThreadID
			}
		case "item.started":
			if ev.Item != nil && ev.Item.Type == "mcp_tool_call" && ev.Item.Server != "" {
				key := ev.Item.ID
				if key == "" {
					key = ev.Item.Server
				}
				pending[key] = now()
			}
		case "item.completed":
			if ev.Item == nil {
				break
			}
			if ev.Item.Type == "mcp_tool_call" && ev.Item.Server != "" {
				server := ev.Item.Server
				res.countToolCall(server, ev.Item.ID)
				stat := res.toolStat(server)
				stat.Calls++
				key := ev.Item.ID
				if key == "" {
					key = server
				}
				if start, ok := pending[key]; ok {
					delete(pending, key)
					stat.Wall += now().Sub(start)
				}
				if ev.Item.Status == "failed" {
					stat.Errors++
					if codexToolAnswered(ev.Item) {
						stat.Answered++
					}
					if stat.FirstError == "" {
						stat.FirstError = capText(codexCallError(ev.Item), 300)
					}
				}
				if len(ev.Item.Result) > 0 {
					name := "mcp__" + server
					if ev.Item.ToolName != "" {
						name += "__" + ev.Item.ToolName
					}
					res.addToolResultSize(name, len(ev.Item.Result))
				}
			}
			if ev.Item.Type == "error" {
				startupError(ev.Item.Message)
			}
			if ev.Item.Type == "agent_message" {
				if t := strings.TrimSpace(ev.Item.Text); t != "" {
					res.Text = t
				}
			}
		case "turn.completed":
			completed = true
			res.Turns++
			if ev.Usage != nil {
				u := *ev.Usage
				usage = &u
				turnCount++
				share := u.InputTokens - prevInput
				if share < 0 {
					share = 0
				}
				turnInputSum += share
				if share > turnInputMax {
					turnInputMax = share
				}
				prevInput = u.InputTokens
			}
		case "turn.failed":
			failed = true
		case "error":
			startupError(ev.Message)
		}
		if onEvent != nil {
			onEvent(ev)
		}
	}
	if usage != nil {
		price, ok := CodexPrices[model]
		res.Usage = &Usage{
			Model:                 model,
			InputTokens:           usage.InputTokens,
			CachedInputTokens:     usage.CachedInputTokens,
			CacheWriteInputTokens: usage.CacheWriteInputTokens,
			OutputTokens:          usage.OutputTokens,
			ReasoningOutputTokens: usage.ReasoningOutputTokens,
			Unpriced:              !ok,
		}
		if ok {
			res.CostUSD = price.Cost(*usage)
		}
	}
	if turnCount > 0 {
		res.InputPerTurnMean = turnInputSum / int64(turnCount)
		res.InputPerTurnMax = turnInputMax
	}
	res.Success = completed && !failed
	res.EmptyOutput = events == 0
	return res
}

// codexToolAnswered says whether a failed mcp_tool_call was the tool's
// own answer: no `error` (Codex sets that when the call never reached
// the server — an approval refusal, a transport or auth failure) and a
// result the server returned, its isError content (docs/codex-events.md,
// `prologue: not found`).
func codexToolAnswered(it *CodexItem) bool {
	return strings.TrimSpace(rawText(it.Error)) == "" && strings.TrimSpace(rawText(it.Result)) != ""
}

// codexCallError is a failed mcp_tool_call's error text: its error's
// message (an object's `message`, or a bare string), else its result's
// text content, else its status.
func codexCallError(it *CodexItem) string {
	if t := strings.TrimSpace(rawText(it.Error)); t != "" {
		return t
	}
	if t := strings.TrimSpace(rawText(it.Result)); t != "" {
		return t
	}
	return it.Status
}

// rawText is the text a JSON value carries: a string itself, an
// object's `message`, else its `content` blocks' texts joined by
// newlines (an MCP result), else a string `text`; "" for null or
// anything else.
func rawText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		Message string `json:"message"`
		Text    string `json:"text"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	if obj.Message != "" {
		return obj.Message
	}
	var parts []string
	for _, c := range obj.Content {
		if t := strings.TrimSpace(c.Text); t != "" {
			parts = append(parts, t)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n")
	}
	return obj.Text
}

// mcpStartupPatterns find the server a Codex MCP startup failure names:
// codex-cli's "MCP client for `<server>` failed to start: …", "The
// <server> MCP server is not logged in…", "MCP server `<server>` …",
// and "MCP startup incomplete (failed: <a>, <b>)".
var mcpStartupPatterns = []*regexp.Regexp{
	regexp.MustCompile("MCP client for [`'\"]([^`'\"]+)[`'\"]"),
	regexp.MustCompile(`(?i)\bThe ([\w.-]+) MCP server\b`),
	regexp.MustCompile("(?i)MCP server [`'\"]([^`'\"]+)[`'\"]"),
}

var mcpStartupFailedList = regexp.MustCompile(`(?i)MCP startup[^()]*\(failed: ([^)]*)\)`)

// mcpStartupServers is the servers an error message says failed to
// start (Codex's `error` item or top-level error), nil when it names
// none or is not about MCP.
func mcpStartupServers(msg string) []string {
	if !strings.Contains(strings.ToUpper(msg), "MCP") {
		return nil
	}
	if m := mcpStartupFailedList.FindStringSubmatch(msg); m != nil {
		var out []string
		for _, s := range strings.Split(m[1], ",") {
			if s = strings.Trim(strings.TrimSpace(s), "`'\""); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	for _, re := range mcpStartupPatterns {
		if m := re.FindStringSubmatch(msg); m != nil {
			return []string{m[1]}
		}
	}
	return nil
}
