package harness

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
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
	Message   *struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Name string `json:"name"`
		} `json:"content"`
	} `json:"message"`
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

// ParseStream reads stream-json lines, calling onEvent for each, and
// returns the Result carried by the final `result` event. Lines that are
// not JSON (claude's own diagnostics) are skipped. If the stream ends with
// no result event, Success is false and Text holds the last assistant
// message.
func ParseStream(r io.Reader, onEvent func(Event)) Result {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	var res Result
	var lastText string
	sawResult := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] != '{' {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.SessionID != "" {
			res.SessionID = ev.SessionID
		}
		switch ev.Type {
		case "assistant":
			if t := ev.Text(); t != "" {
				lastText = t
			}
		case "result":
			sawResult = true
			res.CostUSD = ev.CostUSD
			res.Turns = ev.NumTurns
			res.Success = !ev.IsError && ev.Subtype == "success"
			if strings.TrimSpace(ev.Result) != "" {
				res.Text = strings.TrimSpace(ev.Result)
			}
		}
		if onEvent != nil {
			onEvent(ev)
		}
	}
	if res.Text == "" {
		res.Text = lastText
	}
	if !sawResult {
		res.Success = false
	}
	return res
}
