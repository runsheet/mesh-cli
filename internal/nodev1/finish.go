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
	// learningsTool is the kind's learnings tool (Brief.LearningsTool):
	// a call of it the product answered means the session handed its
	// learnings in itself.
	learningsTool string
	// onFinish hears the product answer the finish tool (Mailbox.Finish:
	// a one-shot job's input closes).
	onFinish func()

	mu  sync.Mutex
	got json.RawMessage
	// learnings are what the finish call carried beside the result, as
	// it carried them; learned is a learnings-tool call answered.
	learnings json.RawMessage
	learned   bool
}

type rpcCall struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Name      string `json:"name"`
		Arguments struct {
			Learnings json.RawMessage `json:"learnings"`
		} `json:"arguments"`
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
	if json.Unmarshal(request, &call) != nil || call.Method != "tools/call" {
		return
	}
	finish, learn := f.isFinish(call.Params.Name), f.learningsTool != "" && sameTool(f.learningsTool, call.Params.Name)
	if !finish && !learn {
		return
	}
	var ans rpcAnswer
	if json.Unmarshal(response, &ans) != nil || ans.Result == nil || ans.Result.IsError || !bytes.Equal(bytes.TrimSpace(ans.ID), bytes.TrimSpace(call.ID)) {
		return
	}
	if learn {
		f.mu.Lock()
		f.learned = true
		f.mu.Unlock()
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
	if l := bytes.TrimSpace(call.Params.Arguments.Learnings); len(l) > 0 && string(l) != "null" {
		f.learnings = json.RawMessage(l)
	}
	f.mu.Unlock()
	if f.onFinish != nil {
		f.onFinish()
	}
}

// isFinish says whether name is the finish tool: its own name, or the
// name a brief gives as <server>.<tool> or mcp__<server>__<tool>.
func (f *finishCapture) isFinish(name string) bool {
	return sameTool(f.tool, name)
}

// sameTool says whether name, a tools/call's, is tool as a brief names
// it: bare, <server>.<tool> or mcp__<server>__<tool>.
func sameTool(tool, name string) bool {
	bare := tool
	if i := strings.LastIndexAny(bare, "._"); i >= 0 && (strings.Contains(bare, ".") || strings.HasPrefix(bare, "mcp__")) {
		bare = bare[i+1:]
	}
	return name == tool || name == bare
}

// finished says whether the product has answered the finish tool.
func (f *finishCapture) finished() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got != nil
}

// learnedAlready says whether the session called the learnings tool
// itself, and the product answered it.
func (f *finishCapture) learnedAlready() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.learned
}

// carried is the learnings the finish call carried, nil when none.
func (f *finishCapture) carried() json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.learnings
}

// result is the last finish answer, nil when there was none.
func (f *finishCapture) result() json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got
}
