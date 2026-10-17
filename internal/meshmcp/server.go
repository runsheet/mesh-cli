// Package meshmcp is Mesh's stdio MCP server, shared by the node's
// channel mode (internal/channel, which pushes the platform's events as
// `notifications/claude/channel`) and by `mesh mcp`, the client half of
// Mesh inside Claude Code on the person's own credential (ADR-023, mesh
// mcp). Only a Server with Channel set advertises the channel capability.
package meshmcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	"projectmesh.io/mesh-cli/internal/version"
)

// ServerName is the MCP server's name, in its initialize response's
// serverInfo.name: `--dangerously-load-development-channels
// server:mesh` for channel mode (that flag's own name is unrelated and
// unchanged), `claude mcp add runsheet -- mesh mcp` for the personal
// one.
//
// Amended 2026-10-17 (ADR-023): renamed from "mesh" to "runsheet",
// alongside mcpinstall.ServerName — the entry a client's config keeps
// it under (ADR-025).
const ServerName = "runsheet"

// Tool is one MCP tool: its schema and its handler.
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any
	Call        func(ctx context.Context, args map[string]any) (string, error)
}

// Server is a stdio MCP server. With Channel set it advertises Claude
// Code's experimental `claude/channel` capability and Push reaches the
// session; without it, it is a plain tools server.
type Server struct {
	Instructions string
	Tools        []Tool
	Channel      bool
	// Register, when set, runs once before Serve reads its first
	// request: `mesh mcp`'s personal node registration (ADR-015's owner
	// form, ADR-023 mesh mcp). A failure is logged to stderr, never
	// fatal — the tools still serve. A channel-mode server leaves it nil.
	Register func(ctx context.Context) error

	mu  sync.Mutex
	out io.Writer
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

func (s *Server) write(m rpcMessage) error {
	m.JSONRPC = "2.0"
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.out == nil {
		return fmt.Errorf("mcp: not connected")
	}
	_, err = s.out.Write(append(b, '\n'))
	return err
}

// Push sends one channel event into the session: content is the body,
// meta's entries become attributes of the <channel> tag (identifier keys).
func (s *Server) Push(content string, meta map[string]string) error {
	params := map[string]any{"content": content}
	if len(meta) > 0 {
		params["meta"] = meta
	}
	return s.write(rpcMessage{Method: "notifications/claude/channel", Params: mustJSON(params)})
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// Serve speaks newline-delimited JSON-RPC on in/out until in ends or ctx
// is done.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.mu.Lock()
	s.out = out
	s.mu.Unlock()
	if s.Register != nil {
		if err := s.Register(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "mesh mcp: register: %v\n", err)
		}
	}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	lines := make(chan []byte)
	errc := make(chan error, 1)
	go func() {
		defer close(lines)
		for sc.Scan() {
			line := append([]byte(nil), sc.Bytes()...)
			select {
			case lines <- line:
			case <-ctx.Done():
				return
			}
		}
		errc <- sc.Err()
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case line, ok := <-lines:
			if !ok {
				select {
				case err := <-errc:
					return err
				default:
					return nil
				}
			}
			if err := s.handle(ctx, line); err != nil {
				return err
			}
		}
	}
}

func (s *Server) handle(ctx context.Context, raw []byte) error {
	var msg rpcMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil // not ours to fail on
	}
	if len(msg.ID) == 0 {
		return nil // a notification (initialized, cancelled): nothing to answer
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
		capabilities := map[string]any{"tools": map[string]any{}}
		if s.Channel {
			capabilities["experimental"] = map[string]any{"claude/channel": map[string]any{}}
		}
		return s.write(rpcMessage{ID: msg.ID, Result: map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    capabilities,
			"serverInfo":      map[string]any{"name": ServerName, "version": version.Resolved()},
			"instructions":    s.Instructions,
		}})
	case "ping":
		return s.write(rpcMessage{ID: msg.ID, Result: map[string]any{}})
	case "tools/list":
		tools := make([]map[string]any, 0, len(s.Tools))
		for _, t := range s.Tools {
			tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.Schema})
		}
		return s.write(rpcMessage{ID: msg.ID, Result: map[string]any{"tools": tools}})
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return s.write(rpcMessage{ID: msg.ID, Error: &rpcError{Code: -32602, Message: "invalid params"}})
		}
		for _, t := range s.Tools {
			if t.Name != p.Name {
				continue
			}
			text, err := t.Call(ctx, p.Arguments)
			if err != nil {
				return s.write(rpcMessage{ID: msg.ID, Result: map[string]any{
					"content": []map[string]any{{"type": "text", "text": err.Error()}}, "isError": true,
				}})
			}
			return s.write(rpcMessage{ID: msg.ID, Result: map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
			}})
		}
		return s.write(rpcMessage{ID: msg.ID, Error: &rpcError{Code: -32601, Message: "unknown tool " + p.Name}})
	default:
		return s.write(rpcMessage{ID: msg.ID, Error: &rpcError{Code: -32601, Message: "method not found"}})
	}
}

// ObjectSchema builds a JSON schema for an object of string/int/bool
// properties, with the required names.
func ObjectSchema(props map[string]string, required ...string) map[string]any {
	p := map[string]any{}
	for name, desc := range props {
		typ := "string"
		switch {
		case name == "cost_cents" || name == "seq":
			typ = "integer"
		case name == "passed" || name == "pair":
			typ = "boolean"
		}
		p[name] = map[string]any{"type": typ, "description": desc}
	}
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": p, "required": required}
}
