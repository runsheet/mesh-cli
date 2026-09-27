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
		} `json:"content"`
	} `json:"message"`
	// RateLimitInfo is the account's plan standing on a
	// rate_limit_event (see RateLimit).
	RateLimitInfo *rateLimitInfo `json:"rate_limit_info"`
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
// count and the id list always agree.
func (r *Result) countToolCall(server, id string) {
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
// synthesised) kept on Result.ToolCallIDs.
func ParseStream(r io.Reader, onEvent func(Event)) Result {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	var res Result
	var lastText, model string
	sawResult := false
	events := 0
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
		case "assistant":
			if t := ev.Text(); t != "" {
				lastText = t
			}
			if ev.Message != nil {
				for _, c := range ev.Message.Content {
					if c.Type != "tool_use" {
						continue
					}
					if server := MCPServer(c.Name); server != "" {
						res.countToolCall(server, c.ID)
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
	if res.Text == "" {
		res.Text = lastText
	}
	if !sawResult {
		res.Success = false
	}
	res.EmptyOutput = events == 0
	return res
}
