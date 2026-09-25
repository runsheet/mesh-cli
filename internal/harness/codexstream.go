package harness

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
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
// stays nil. Lines that are not JSON and unknown types are skipped.
func ParseCodexStream(r io.Reader, model string, onEvent func(CodexEvent)) Result {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	var res Result
	var usage *CodexUsage
	completed, failed := false, false
	events := 0
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
		case "item.completed":
			if ev.Item == nil {
				break
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
			}
		case "turn.failed":
			failed = true
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
	res.Success = completed && !failed
	res.EmptyOutput = events == 0
	return res
}
