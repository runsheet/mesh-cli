package harness

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// Event is one line of `claude -p --output-format stream-json --verbose`.
type Event struct {
	Type      string  `json:"type"`
	Subtype   string  `json:"subtype"`
	SessionID string  `json:"session_id"`
	IsError   bool    `json:"is_error"`
	Result    string  `json:"result"`
	CostUSD   float64 `json:"total_cost_usd"`
	NumTurns  int     `json:"num_turns"`
	// Model is the model a system/init event says the session runs on.
	Model string `json:"model"`
	// Usage is the session's token counts on the result event.
	Usage   *streamUsage `json:"usage"`
	Message *struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Name string `json:"name"`
			// ID is a tool_use block's id (toolu_…), kept on
			// Result.ToolCallIDs for recordToolUsage.
			ID string `json:"id"`
			// ToolUseID is a tool_result block's matching tool_use id
			// (a user event), used to close the call it opened.
			ToolUseID string `json:"tool_use_id"`
			// IsError is a tool_result block's own flag.
			IsError bool `json:"is_error"`
			// ResultContent is a tool_result block's content: a plain
			// string, or a list of {type:"text", text}. See
			// toolResultText.
			ResultContent json.RawMessage `json:"content"`
		} `json:"content"`
		// Usage is an assistant event's own turn's token counts (its
		// share, not the session's running total).
		Usage *streamUsage `json:"usage"`
	} `json:"message"`
	// RateLimitInfo is the account's plan standing on a
	// rate_limit_event (see RateLimit).
	RateLimitInfo *rateLimitInfo `json:"rate_limit_info"`
	// MCPServers is the system/init event's mcp_servers, each server's
	// name and connection status ("connected" or not), when the CLI
	// sends it.
	MCPServers []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"mcp_servers"`
}

// streamUsage is the token counts a result event carries. Its
// input_tokens are the uncached ones only.
type streamUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// Text returns the assistant text of an event, joined.
func (e Event) Text() string {
	if e.Message == nil {
		return ""
	}
	var parts []string
	for _, c := range e.Message.Content {
		if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
			parts = append(parts, strings.TrimSpace(c.Text))
		}
	}
	return strings.Join(parts, "\n")
}

// MCPServer is the server of an MCP tool name, mcp__<server>__<tool>
// ("" for a tool that is not an MCP server's).
func MCPServer(name string) string {
	rest, ok := strings.CutPrefix(name, "mcp__")
	if !ok {
		return ""
	}
	server, _, ok := strings.Cut(rest, "__")
	if !ok {
		return ""
	}
	return server
}

// countToolCall counts one call to server, keeping its id on
// ToolCallIDs; an empty id (an event that carried none) is synthesised
// from the session id, the server and the call's place in it, so the
// count and the id list always agree. Returns the id used.
func (r *Result) countToolCall(server, id string) string {
	if r.ToolCalls == nil {
		r.ToolCalls = map[string]int{}
	}
	r.ToolCalls[server]++
	if id == "" {
		id = fmt.Sprintf("%s:%s:%d", r.SessionID, server, r.ToolCalls[server])
	}
	if r.ToolCallIDs == nil {
		r.ToolCallIDs = map[string][]string{}
	}
	r.ToolCallIDs[server] = append(r.ToolCallIDs[server], id)
	return id
}

// now is the parser's clock, for Result.Tools' wall time: Claude Code's
// stream events carry no timestamps, so tests override it.
var now = time.Now

// toolResultText reads a tool_result block's content, a plain string or
// a list of {type:"text", text}, joined.
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// capText trims s and caps it at n runes.
func capText(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

// ParseStream reads stream-json lines, calling onEvent for each, and
// returns the Result carried by the final `result` event. Lines that are
// not JSON (claude's own diagnostics) are skipped. If the stream ends with
// no result event, Success is false and Text holds the last assistant
// message. A stream with no parseable event at all sets EmptyOutput.
// The latest rate_limit_event, if any, is kept on Result.RateLimit, and
// the result event's token counts on Result.Usage (its InputTokens the
// cached ones included, as Usage counts them), with the model the
// system/init event named. Each tool_use of an MCP server's tool is
// counted on Result.ToolCalls by its server, its id (empty ones
// synthesised) kept on Result.ToolCallIDs, and on Result.Tools: its wall
// time closes on the matching tool_result (a user event, by
// tool_use_id), left zero if none arrives before the stream ends; a
// tool_result with is_error true counts an error, its text (capped at
// 300 chars) kept as the server's FirstError. A system/init event's
// mcp_servers names each server whose status is not "connected" as an
// error too, that status as FirstError, even with zero calls.
//
// Every assistant event's own message.usage (input + cache_read +
// cache_creation) is that turn's input tokens; Result.InputPerTurnMean
// and InputPerTurnMax are computed across every turn that carried one.
// Every tool_result block's content (toolResultText) keeps its byte
// length among Result.LargestToolResults, by the matching tool_use
// block's own name (Bash, mcp__server__tool, …) — the five largest,
// largest first.
func ParseStream(r io.Reader, onEvent func(Event)) Result {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	var res Result
	var lastText, model string
	sawResult := false
	events := 0
	var turnInputSum, turnInputMax int64
	turnCount := 0
	toolUseNames := map[string]string{}
	type pendingCall struct {
		server string
		start  time.Time
	}
	pending := map[string]pendingCall{}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] != '{' {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		events++
		if ev.SessionID != "" {
			res.SessionID = ev.SessionID
		}
		switch ev.Type {
		case "system":
			if ev.Model != "" {
				model = ev.Model
			}
			for _, srv := range ev.MCPServers {
				if srv.Name == "" || srv.Status == "" || srv.Status == "connected" {
					continue
				}
				stat := res.toolStat(srv.Name)
				stat.Errors++
				if stat.FirstError == "" {
					stat.FirstError = capText(srv.Status, 300)
				}
			}
		case "assistant":
			if t := ev.Text(); t != "" {
				lastText = t
			}
			if ev.Message != nil {
				if u := ev.Message.Usage; u != nil {
					turnCount++
					turn := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
					turnInputSum += turn
					if turn > turnInputMax {
						turnInputMax = turn
					}
				}
				for _, c := range ev.Message.Content {
					if c.Type != "tool_use" {
						continue
					}
					if c.ID != "" {
						toolUseNames[c.ID] = c.Name
					}
					if server := MCPServer(c.Name); server != "" {
						id := res.countToolCall(server, c.ID)
						res.toolStat(server).Calls++
						pending[id] = pendingCall{server: server, start: now()}
					}
				}
			}
		case "user":
			if ev.Message == nil {
				break
			}
			for _, c := range ev.Message.Content {
				if c.Type != "tool_result" {
					continue
				}
				if name, ok := toolUseNames[c.ToolUseID]; ok {
					delete(toolUseNames, c.ToolUseID)
					res.addToolResultSize(name, len(toolResultText(c.ResultContent)))
				}
				pc, ok := pending[c.ToolUseID]
				if !ok {
					continue
				}
				delete(pending, c.ToolUseID)
				stat := res.toolStat(pc.server)
				stat.Wall += now().Sub(pc.start)
				if c.IsError {
					stat.Errors++
					if stat.FirstError == "" {
						stat.FirstError = capText(toolResultText(c.ResultContent), 300)
					}
				}
			}
		case "rate_limit_event":
			if rl := ev.RateLimit(time.Now()); rl != nil {
				res.RateLimit = rl
			}
		case "result":
			sawResult = true
			res.CostUSD = ev.CostUSD
			res.Turns = ev.NumTurns
			res.Success = !ev.IsError && ev.Subtype == "success"
			if u := ev.Usage; u != nil {
				res.Usage = &Usage{
					InputTokens:           u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
					CachedInputTokens:     u.CacheReadInputTokens,
					CacheWriteInputTokens: u.CacheCreationInputTokens,
					OutputTokens:          u.OutputTokens,
				}
			}
			if strings.TrimSpace(ev.Result) != "" {
				res.Text = strings.TrimSpace(ev.Result)
			}
		}
		if onEvent != nil {
			onEvent(ev)
		}
	}
	if res.Usage != nil {
		res.Usage.Model = model
	}
	if turnCount > 0 {
		res.InputPerTurnMean = turnInputSum / int64(turnCount)
		res.InputPerTurnMax = turnInputMax
	}
	if res.Text == "" {
		res.Text = lastText
	}
	if !sawResult {
		res.Success = false
	}
	res.EmptyOutput = events == 0
	return res
}
