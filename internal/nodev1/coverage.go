package nodev1

import (
	"encoding/json"
	"sync"
)

// coverageNote is the newest e2e coverage map a lease's run_suite runs
// wrote (nodetools.RunSuite.OnCoverage): runsheet's e2ecoverage suite
// writes e2e-coverage.json on main's validation, and the node carries
// it on the validate result's "coverage" key, which the platform
// attests as the e2e-coverage report (runsheet ADR 032, amended
// 2026-11-20). A nil note keeps nothing.
type coverageNote struct {
	mu      sync.Mutex
	content string
}

func (c *coverageNote) set(content string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.content = content
	c.mu.Unlock()
}

func (c *coverageNote) get() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.content
}

// withCoverage adds content as finish's "coverage" key, a JSON string,
// the way withPin adds "pin". The key is the node's alone: one a session
// wrote is dropped, so with no map written the result carries none.
func withCoverage(finish json.RawMessage, content string) json.RawMessage {
	var obj map[string]json.RawMessage
	if len(finish) == 0 || json.Unmarshal(finish, &obj) != nil || obj == nil {
		if content == "" {
			return finish
		}
		obj = map[string]json.RawMessage{}
	}
	_, had := obj["coverage"]
	if content == "" && !had {
		return finish
	}
	delete(obj, "coverage")
	if content != "" {
		raw, err := json.Marshal(content)
		if err != nil {
			return finish
		}
		obj["coverage"] = raw
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return finish
	}
	return out
}
