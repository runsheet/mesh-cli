package sessionproxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"projectmesh.io/mesh-cli/internal/mcprelay"
)

// Slot tools (projectmesh-io/api ADR 001 amendment, tool slots): when a
// kind names its tools, a session sees exactly those of its product's
// MCP server, each under the kind's name and description, and nothing
// else the server has. The proxy holds the line on the product route
// itself, so a session that knows a hidden tool's name still cannot call
// it: tools/list answers only the named tools, their descriptions the
// kind's, and a tools/call of any other name is refused here and never
// reaches the product.

// ToolFilter is the tools of the proxy's Product upstreams a session is
// served: name to the description it is presented with. A nil filter
// serves every tool as the server describes it.
type ToolFilter map[string]string

// toolNotServed is the JSON-RPC error a refused tools/call answers:
// invalid params, as an MCP server answers a tool it does not have.
const toolNotServed = -32602

// rpc is the part of a JSON-RPC message the filter reads.
type rpc struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params struct {
		Name string `json:"name"`
	} `json:"params"`
}

// filtered says the request on name's upstream u is held to the filter.
func (p *Proxy) filtered(u Upstream) bool {
	_, product := u.(Product)
	return product && p.Tools != nil
}

// screen reads a request to a filtered upstream: a tools/call of a tool
// the filter does not name is answered here (true), a tools/list is
// marked for its answer to be filtered (list). A batch holding either is
// refused whole: MCP sends single messages, and a batch is not read
// member by member.
func (p *Proxy) screen(w http.ResponseWriter, name string, body []byte) (handled, list bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return false, false
	}
	if trimmed[0] == '[' {
		var batch []rpc
		if json.Unmarshal(trimmed, &batch) != nil {
			return false, false
		}
		for _, m := range batch {
			if m.Method == "tools/call" || m.Method == "tools/list" {
				writeError(w, http.StatusOK, -32600, "sessionproxy: "+name+": tools/list and tools/call are served one message at a time")
				return true, false
			}
		}
		return false, false
	}
	var m rpc
	if json.Unmarshal(trimmed, &m) != nil {
		return false, false
	}
	switch m.Method {
	case "tools/list":
		return false, true
	case "tools/call":
		if _, ok := p.Tools[m.Params.Name]; ok {
			return false, false
		}
		p.logf("sessionproxy: %s: refused a call of tool %q, which this session is not served", name, m.Params.Name)
		id := m.ID
		if len(id) == 0 {
			id = json.RawMessage("null")
		}
		b, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"error":   map[string]any{"code": toolNotServed, "message": fmt.Sprintf("tool %q is not served to this session", m.Params.Name)},
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
		return true, false
	}
	return false, false
}

// filterList rewrites one JSON-RPC answer to tools/list: only the tools
// the filter names, each with the filter's description; anything that
// is not such an answer comes back as it was. It answers the names it
// kept.
func (f ToolFilter) filterList(msg []byte) ([]byte, []string) {
	var env map[string]json.RawMessage
	if json.Unmarshal(msg, &env) != nil {
		return msg, nil
	}
	raw, ok := env["result"]
	if !ok {
		return msg, nil
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(raw, &result) != nil {
		return msg, nil
	}
	var tools []map[string]json.RawMessage
	if t, ok := result["tools"]; !ok || json.Unmarshal(t, &tools) != nil {
		return msg, nil
	}
	kept := []map[string]json.RawMessage{}
	var names []string
	for _, t := range tools {
		var name string
		if json.Unmarshal(t["name"], &name) != nil {
			continue
		}
		desc, ok := f[name]
		if !ok {
			continue
		}
		d, _ := json.Marshal(desc)
		t["description"] = d
		kept = append(kept, t)
		names = append(names, name)
	}
	result["tools"], _ = json.Marshal(kept)
	env["result"], _ = json.Marshal(result)
	out, err := json.Marshal(env)
	if err != nil {
		return msg, nil
	}
	return out, names
}

// replyList relays a tools/list answer filtered: read whole (a list is
// one message, whether as JSON or one event of a stream), each message
// rewritten by filterList, masked, and the tools it kept recorded as
// served on name.
func (p *Proxy) replyList(w http.ResponseWriter, resp *http.Response, name string, request []byte) {
	for _, k := range returned {
		if v := resp.Header.Values(k); len(v) > 0 {
			w.Header()[http.CanonicalHeaderKey(k)] = append([]string(nil), v...)
		}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, mcprelay.MaxMessage))
	if err != nil {
		p.logf("sessionproxy: reading the tools list: %v", err)
	}
	var out []byte
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		out = p.filterStream(b, name, request)
	} else {
		msg, names := p.Tools.filterList(b)
		p.served(name, names)
		out = []byte(p.mask(string(msg)))
		if p.Observe != nil && resp.StatusCode/100 == 2 {
			p.Observe(name, request, out)
		}
	}
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
}

// filterStream rewrites each event of a stream: its data lines joined
// into one message, filtered, and sent as one data line.
func (p *Proxy) filterStream(b []byte, name string, request []byte) []byte {
	var out bytes.Buffer
	var data []string
	var other []string
	flush := func() {
		if len(data) == 0 && len(other) == 0 {
			return
		}
		for _, l := range other {
			out.WriteString(l + "\n")
		}
		if len(data) > 0 {
			msg, names := p.Tools.filterList([]byte(strings.Join(data, "\n")))
			p.served(name, names)
			masked := p.mask(string(msg))
			if p.Observe != nil {
				p.Observe(name, request, []byte(masked))
			}
			out.WriteString("data: " + strings.ReplaceAll(masked, "\n", "") + "\n")
		}
		out.WriteString("\n")
		data, other = nil, nil
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), mcprelay.MaxMessage+1)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		default:
			other = append(other, p.mask(line))
		}
	}
	flush()
	return out.Bytes()
}

// served records names as the tools server name presented.
func (p *Proxy) served(server string, names []string) {
	if len(names) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.servedTools == nil {
		p.servedTools = map[string]map[string]bool{}
	}
	if p.servedTools[server] == nil {
		p.servedTools[server] = map[string]bool{}
	}
	for _, n := range names {
		p.servedTools[server][n] = true
	}
}

// ServedTools are the product tools each filtered server presented to
// the session, by server, each list sorted: what the session was served,
// by name alone.
func (p *Proxy) ServedTools() map[string][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string][]string, len(p.servedTools))
	for server, set := range p.servedTools {
		names := make([]string, 0, len(set))
		for n := range set {
			names = append(names, n)
		}
		sort.Strings(names)
		out[server] = names
	}
	return out
}
