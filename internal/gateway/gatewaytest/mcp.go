package gatewaytest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// The platform's /mcp, as `mesh mcp`'s hosted client speaks to it
// (mesh-cli ADR-023, Hosted path): JSON-RPC over HTTP POST, one reply
// per request. The context tools are always listed; the work tools only
// with HostedWorkTools, each applied as its mutation is and recorded
// under its own name (CallsNamed("mesh_claim")), never the mutation's.

// mcpSession is the Mcp-Session-Id the fake hands out on initialize.
const mcpSession = "fake-mcp-session"

var contextTools = []string{"mesh_document", "mesh_documents", "mesh_items", "mesh_projects", "mesh_search"}

var hostedWorkTools = []string{"mesh_claim", "mesh_progress", "mesh_ask", "mesh_finish", "mesh_release"}

func (f *Fake) mcp(w http.ResponseWriter, r *http.Request) {
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if bearer != f.Token {
		f.MCPRefused++
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	f.MCPHits++
	f.MCPRequests = append(f.MCPRequests, MCPRequest{Method: req.Method, Session: r.Header.Get("Mcp-Session-Id"), Bearer: bearer, At: time.Now()})
	if len(req.ID) == 0 {
		// a notification: accepted, no reply
		w.WriteHeader(http.StatusAccepted)
		return
	}
	reply := func(result any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}
	switch req.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", mcpSession)
		reply(map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "runsheet", "version": "fake"}})
	case "tools/list":
		names := append([]string(nil), contextTools...)
		if f.HostedWorkTools {
			names = append(names, hostedWorkTools...)
		}
		if err := publishedTools(names); err != nil {
			f.violate(err.Error())
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		tools := []map[string]any{}
		for _, n := range names {
			tools = append(tools, map[string]any{"name": n, "inputSchema": map[string]any{"type": "object"}})
		}
		reply(map[string]any{"tools": tools})
	case "tools/call":
		text, err := f.mcpCall(req.Params.Name, req.Params.Arguments, bearer)
		if err != nil {
			reply(map[string]any{"content": []map[string]any{{"type": "text", "text": err.Error()}}, "isError": true})
			return
		}
		reply(map[string]any{"content": []map[string]any{{"type": "text", "text": text}}})
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "method not found: " + req.Method}})
	}
}

// MCPRequest is one /mcp request the fake took (Fake.MCPRequests).
type MCPRequest struct {
	Method  string
	Session string
	Bearer  string
	At      time.Time
}

// MCPRequestsNamed returns the /mcp requests the fake took for method,
// in order.
func (f *Fake) MCPRequestsNamed(method string) []MCPRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []MCPRequest
	for _, r := range f.MCPRequests {
		if r.Method == method {
			out = append(out, r)
		}
	}
	return out
}

// SetToken rotates the bearer the fake takes, as the platform's
// rotation of a machine credential does: the old one is refused from
// then on, on /mcp as on GraphQL.
func (f *Fake) SetToken(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Token = token
}

// mcpCall is one tools/call under f.mu: a work tool applied as its
// mutation, or mesh_document from Bodies.
func (f *Fake) mcpCall(name string, args map[string]any, bearer string) (string, error) {
	if name == "mesh_document" {
		project, _ := args["project"].(string)
		slug, _ := args["slug"].(string)
		body, ok := f.Bodies[project+"/"+slug]
		if !ok {
			return "", fmt.Errorf("document %s not found in %s", slug, project)
		}
		return body, nil
	}
	work := false
	for _, n := range hostedWorkTools {
		work = work || n == name
	}
	if !work || !f.HostedWorkTools {
		return "", fmt.Errorf("unknown tool %s", name)
	}
	if msg := f.Refuse[name]; msg != "" {
		return "", fmt.Errorf("%s", msg)
	}
	item := func() (string, map[string]any, error) {
		id, _ := args["item"].(string)
		row := f.WorkItems[id]
		if row == nil {
			return "", nil, fmt.Errorf("no such item %q", id)
		}
		return id, row, nil
	}
	var text string
	switch name {
	case "mesh_claim":
		slug, _ := args["goal"].(string)
		seq := toInt(args["item"])
		var id string
		for gid, g := range f.Goals {
			if g["slug"] != slug {
				continue
			}
			for iid, it := range f.WorkItems {
				if it["goalId"] == gid && toInt(it["seq"]) == seq {
					id = iid
				}
			}
		}
		row := f.WorkItems[id]
		if row == nil {
			return "", fmt.Errorf("no item %d on goal %s", seq, slug)
		}
		nodeID, _ := args["node"].(string)
		if pair, _ := args["pair"].(bool); pair && row["nodeId"] != nil && row["nodeId"] != nodeID {
			pairs, _ := row["pairs"].([]any)
			row["pairs"] = append(pairs, nodeID)
			text = fmt.Sprintf("paired on %s/%d with node %v", slug, seq, row["nodeId"])
			break
		}
		if err := f.apply("claimWorkItem", map[string]any{"aggregateId": id, "nodeId": nodeID, "harness": "claude-code"}); err != nil {
			return "", err
		}
		text = fmt.Sprintf("claimed %s/%d for node %s", slug, seq, nodeID)
	case "mesh_progress", "mesh_ask":
		id, _, err := item()
		if err != nil {
			return "", err
		}
		note, _ := args["note"].(string)
		text = "noted"
		if name == "mesh_ask" {
			q, _ := args["question"].(string)
			note, text = "question: "+q, "asked; the answer comes back on the item"
		}
		if err := f.apply("reportWorkItemProgress", map[string]any{"aggregateId": id, "note": note}); err != nil {
			return "", err
		}
	case "mesh_finish":
		id, _, err := item()
		if err != nil {
			return "", err
		}
		in := map[string]any{"aggregateId": id}
		for _, k := range []string{"branch", "headSha", "outcome", "evidence"} {
			if v, ok := args[k]; ok {
				in[k] = v
			}
		}
		if err := f.apply("finishWorkItem", in); err != nil {
			return "", err
		}
		text = "finished: the item is evaluating"
	case "mesh_release":
		id, row, err := item()
		if err != nil {
			return "", err
		}
		if err := f.apply("releaseWorkItem", map[string]any{"aggregateId": id, "nodeId": row["nodeId"], "note": args["reason"]}); err != nil {
			return "", err
		}
		text = "released"
	}
	f.Calls = append(f.Calls, Call{Name: name, Input: copyRow(args), At: time.Now(), Bearer: bearer})
	return text, nil
}
