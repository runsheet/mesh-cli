package nodev1

import (
	"encoding/json"
	"sync"
)

// taintState is the lease's own taint lockdown (ADR-046's 2026-11
// amendment, taint lockdown): a session that reads untrusted content —
// a tool's own answer marked result._meta["mesh/untrusted"]: true —
// loses its git push and its egress for the rest of the lease. A plain
// MCP call and the lease's model route (sessionproxy.Model, served
// apart from Observe) are both unaffected: only nodetools.Push and
// egress.Tunnel ever ask Tainted.
type taintState struct {
	lease string
	logf  func(string, ...any)

	mu      sync.Mutex
	tainted bool
	reason  string
}

// newTaintState is a lease's taint tracker, scoped to its one session.
func newTaintState(lease string, logf func(string, ...any)) *taintState {
	return &taintState{lease: lease, logf: logf}
}

// Tainted says whether the lease has read untrusted content; nil-safe,
// so it composes directly into a Push's or a Tunnel's Tainted field.
func (t *taintState) Tainted() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tainted
}

// reasonFor is why the lease was tainted, "" when it was not.
func (t *taintState) reasonFor() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reason
}

// taint marks the lease tainted, logging once the first time; a later
// call changes nothing but is not an error.
func (t *taintState) taint(reason string) {
	t.mu.Lock()
	already := t.tainted
	t.tainted, t.reason = true, reason
	t.mu.Unlock()
	if !already && t.logf != nil {
		t.logf("nodev1: lease %s: tainted: %s; its push and egress are refused for the rest of the lease", t.lease, reason)
	}
}

// taintUntrustedAnswer is the part of a tools/call response that marks
// its own content untrusted.
type taintUntrustedAnswer struct {
	Result *struct {
		Meta struct {
			Untrusted bool `json:"mesh/untrusted"`
		} `json:"_meta"`
	} `json:"result"`
}

// observe is the sessionproxy.Proxy's Observe (composed with the
// lease's other observers by observeBoth): a tools/call response
// carrying result._meta["mesh/untrusted"]: true taints the lease,
// naming the server and the tool. Every other message — a tools/list,
// a notification, a plain tools/call answer, an error answer — taints
// nothing, so an untainted or already-tainted lease reads on exactly as
// before.
func (t *taintState) observe(server string, request, response []byte) {
	var call rpcCall
	if json.Unmarshal(request, &call) != nil || call.Method != "tools/call" {
		return
	}
	var ans taintUntrustedAnswer
	if json.Unmarshal(response, &ans) != nil || ans.Result == nil || !ans.Result.Meta.Untrusted {
		return
	}
	name := call.Params.Name
	if name == "" {
		name = "a tool"
	}
	t.taint(server + " " + name + " answered content marked untrusted")
}

// observeBoth answers an Observe that calls a then b with the same
// request and response, either of which may be nil: the lease's finish
// capture and its taint tracker both hear every message.
func observeBoth(a, b func(server string, request, response []byte)) func(string, []byte, []byte) {
	return func(server string, request, response []byte) {
		if a != nil {
			a(server, request, response)
		}
		if b != nil {
			b(server, request, response)
		}
	}
}
