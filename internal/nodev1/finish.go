package nodev1

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
)

// finishCapture reads the product's answer to its own finish tool off
// the lease's sessionproxy (sessionproxy.Proxy.Observe): the job's
// result is what the product said when the session finished, in the
// product's words, never the session's transcript. A JSON answer is the
// result as it is; any other text is {"text": …}.
type finishCapture struct {
	tool string

	mu  sync.Mutex
	got json.RawMessage
}

type rpcCall struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Name string `json:"name"`
	} `json:"params"`
}

type rpcAnswer struct {
	ID     json.RawMessage `json:"id"`
	Result *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	} `json:"result"`
}

// observe is the proxy's Observe.
func (f *finishCapture) observe(_ string, request, response []byte) {
	if f.tool == "" {
		return
	}
	var call rpcCall
	if json.Unmarshal(request, &call) != nil || call.Method != "tools/call" || !f.isFinish(call.Params.Name) {
		return
	}
	var ans rpcAnswer
	if json.Unmarshal(response, &ans) != nil || ans.Result == nil || ans.Result.IsError || !bytes.Equal(bytes.TrimSpace(ans.ID), bytes.TrimSpace(call.ID)) {
		return
	}
	var text strings.Builder
	for _, c := range ans.Result.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	t := strings.TrimSpace(text.String())
	var raw json.RawMessage
	if json.Valid([]byte(t)) && strings.HasPrefix(t, "{") {
		raw = json.RawMessage(t)
	} else {
		raw, _ = json.Marshal(map[string]string{"text": t})
	}
	f.mu.Lock()
	f.got = raw
	f.mu.Unlock()
}

// isFinish says whether name is the finish tool: its own name, or the
// name a brief gives as <server>.<tool> or mcp__<server>__<tool>.
func (f *finishCapture) isFinish(name string) bool {
	tool := f.tool
	if i := strings.LastIndexAny(tool, "._"); i >= 0 && (strings.Contains(tool, ".") || strings.HasPrefix(tool, "mcp__")) {
		tool = tool[i+1:]
	}
	return name == f.tool || name == tool
}

// result is the last finish answer, nil when there was none.
func (f *finishCapture) result() json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got
}
