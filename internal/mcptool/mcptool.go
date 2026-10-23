// Package mcptool is the smallest MCP tools server the node serves a
// session itself, in process: a Tool is a name, a schema and a Go
// handler, and Handler answers the streamable-HTTP MCP methods a
// client needs of a tools server — initialize, ping, tools/list,
// tools/call — over one POST each, so a sessionproxy can front it at
// /mcp/<name> beside the servers it reaches over the network. A leaf
// package: the node's own tools (mesh_result, mesh_fail) are defined
// here so harness, node and sessionproxy share one shape without a
// cycle.
package mcptool

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
)

// MaxBody bounds one request's body.
const MaxBody = 16 << 20

// Tool is one tool: its name, what it is for, its input schema and
// the handler that answers a call. A handler's error is the call's
// refusal, answered to the session as the tool's isError result with
// the error's text, so the session reads why and can call again.
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any
	Call        func(ctx context.Context, args map[string]any) (string, error)
}

// Handler serves Tools as a streamable-HTTP MCP server: a JSON-RPC
// request's answer comes back as the POST's JSON body; a notification
// is answered 202 with no body; GET is 405, since the server has
// nothing to say unasked.
type Handler struct {
	// Name is the server's name in its initialize answer.
	Name string
	// Instructions, when set, ride the initialize answer.
	Instructions string
	Tools        []Tool

	mu sync.Mutex
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete:
		w.WriteHeader(http.StatusOK)
		return
	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "mcptool: nothing to stream", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if err != nil || len(body) > MaxBody {
		http.Error(w, "mcptool: reading the request", http.StatusBadRequest)
		return
	}
	reply, ok := h.Answer(r.Context(), body)
	if !ok {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(reply)
}

// Answer answers one JSON-RPC message: the reply to write, or ok false
// for a notification or a message that is not JSON-RPC, which gets none.
func (h *Handler) Answer(ctx context.Context, raw []byte) (reply []byte, ok bool) {
	var msg rpcMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return h.encode(rpcMessage{Error: &rpcError{Code: -32700, Message: "parse error"}}), true
	}
	if len(msg.ID) == 0 || string(msg.ID) == "null" {
		return nil, false
	}
	switch msg.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		result := map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": h.Name, "version": "1"},
		}
		if h.Instructions != "" {
			result["instructions"] = h.Instructions
		}
		return h.encode(rpcMessage{ID: msg.ID, Result: result}), true
	case "ping":
		return h.encode(rpcMessage{ID: msg.ID, Result: map[string]any{}}), true
	case "tools/list":
		list := make([]map[string]any, 0, len(h.Tools))
		for _, t := range h.Tools {
			schema := t.Schema
			if schema == nil {
				schema = map[string]any{"type": "object"}
			}
			list = append(list, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": schema})
		}
		return h.encode(rpcMessage{ID: msg.ID, Result: map[string]any{"tools": list}}), true
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return h.encode(rpcMessage{ID: msg.ID, Error: &rpcError{Code: -32602, Message: "invalid params"}}), true
		}
		t, found := h.tool(p.Name)
		if !found {
			return h.encode(rpcMessage{ID: msg.ID, Error: &rpcError{Code: -32601, Message: "unknown tool " + p.Name}}), true
		}
		text, err := t.Call(ctx, p.Arguments)
		if err != nil {
			return h.encode(rpcMessage{ID: msg.ID, Result: map[string]any{
				"content": []map[string]any{{"type": "text", "text": err.Error()}}, "isError": true,
			}}), true
		}
		return h.encode(rpcMessage{ID: msg.ID, Result: map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
		}}), true
	default:
		return h.encode(rpcMessage{ID: msg.ID, Error: &rpcError{Code: -32601, Message: "method not found"}}), true
	}
}

// tool is the tool named, if served.
func (h *Handler) tool(name string) (Tool, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, t := range h.Tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

func (h *Handler) encode(m rpcMessage) []byte {
	m.JSONRPC = "2.0"
	b, _ := json.Marshal(m)
	return b
}

// Find is the tool named name among tools, for a caller that reaches a
// tool directly (a test double harness standing in for a session).
func Find(tools []Tool, name string) (Tool, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}
