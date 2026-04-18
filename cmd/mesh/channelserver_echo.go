package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// bytesTrimSpace is a local wrapper so we don't sprinkle bytes.TrimSpace
// everywhere in the hot read loop — keeps intent clearer at call-sites.
func bytesTrimSpace(b []byte) []byte { return bytes.TrimSpace(b) }

var (
	echoAgentFlag string
	echoIndexFlag int
)

// channelServerEchoCmd implements a minimal MCP-over-stdio server for the
// `echo` channel type. It exists to exercise the slice-1 plumbing
// (spawn / settings.json / lifecycle) — slice 2 will swap it for discord.
//
// The server speaks just enough of MCP to satisfy Claude Code:
//
//   - initialize → returns a tools capability
//   - tools/list → returns one tool, echo_send(content)
//   - tools/call echo_send → logs "ECHO: <content>" to stderr and succeeds
//
// Every 10 s it emits a notifications/message heartbeat so slice 2 has a
// working example of the "server initiates a turn" flow.
//
// Protocol: newline-delimited JSON-RPC 2.0 on stdin / stdout (not the
// Content-Length framing style — Claude Code's stdio transport uses
// line-delimited JSON per MCP's stdio recommendation). No external MCP lib:
// the surface is small and slice 2's discord work may want a different
// dependency, so we hand-roll.
var channelServerEchoCmd = &cobra.Command{
	Use:   "echo",
	Short: "Echo MCP server (debug / slice-1 plumbing exerciser)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runEchoChannelServer(echoAgentFlag, echoIndexFlag)
	},
}

func init() {
	channelServerEchoCmd.Flags().StringVar(&echoAgentFlag, "agent", "", "Agent name this channel backs (required)")
	channelServerEchoCmd.Flags().IntVar(&echoIndexFlag, "index", 0, "Channel index within the agent's channel list")
}

// jsonrpcMessage is the loose on-wire envelope. Presence of each field
// disambiguates request / response / notification per JSON-RPC 2.0.
type jsonrpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func runEchoChannelServer(agent string, index int) error {
	if agent == "" {
		return fmt.Errorf("--agent is required")
	}

	logFile, err := openChannelLogFile(agent, "echo")
	if err != nil {
		return err
	}
	defer logFile.Close()
	logf := func(format string, args ...any) {
		fmt.Fprintf(logFile, "["+time.Now().UTC().Format(time.RFC3339Nano)+"] "+format+"\n", args...)
	}
	logf("echo channel start agent=%s index=%d pid=%d", agent, index, os.Getpid())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// SIGTERM → clean exit. ChannelManager sends this on shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		logf("received signal %s, shutting down", sig)
		cancel()
	}()

	// Serialize writes so the heartbeat goroutine and the handler loop don't
	// interleave on stdout.
	var writeMu sync.Mutex
	write := func(msg jsonrpcMessage) error {
		msg.JSONRPC = "2.0"
		writeMu.Lock()
		defer writeMu.Unlock()
		data, err := json.Marshal(msg)
		if err != nil {
			return err
		}
		logf("send: %s", string(data))
		_, err = os.Stdout.Write(append(data, '\n'))
		return err
	}

	// Heartbeat every 10 s so slice 2 has a reference for the server-push case.
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				params, _ := json.Marshal(map[string]any{
					"level": "info",
					"data":  "echo channel alive",
				})
				_ = write(jsonrpcMessage{
					Method: "notifications/message",
					Params: params,
				})
			}
		}
	}()

	// Main read loop. Line-delimited JSON per MCP stdio convention.
	reader := bufio.NewReader(os.Stdin)
	for {
		if ctx.Err() != nil {
			return nil
		}
		line, err := reader.ReadBytes('\n')
		trimmed := bytesTrimSpace(line)
		if len(trimmed) > 0 {
			logf("recv: %s", string(trimmed))
			if handleErr := handleEchoMessage(trimmed, write, logf); handleErr != nil {
				logf("handle error: %v", handleErr)
			}
		}
		if err != nil {
			if err == io.EOF {
				logf("stdin EOF, exiting")
				return nil
			}
			// Real I/O error — surface it so the daemon sees a non-zero exit.
			return err
		}
	}
}

// handleEchoMessage parses a single inbound JSON-RPC frame and dispatches it.
// Parse failures are logged but don't kill the server — a malformed frame
// from a confused client shouldn't take the channel down.
func handleEchoMessage(raw []byte, write func(jsonrpcMessage) error, logf func(string, ...any)) error {
	var msg jsonrpcMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return fmt.Errorf("parse frame: %w", err)
	}
	// Notifications (no id) are silently accepted; nothing to respond to.
	if len(msg.ID) == 0 {
		return nil
	}

	switch msg.Method {
	case "initialize":
		result := map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "mesh-channel-echo",
				"version": "0.1.0",
			},
		}
		return write(jsonrpcMessage{ID: msg.ID, Result: result})

	case "tools/list":
		tools := []map[string]any{{
			"name":        "echo_send",
			"description": "Logs the provided content to the echo channel's stderr. Slice-1 placeholder for real channel outbound.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"content": map[string]any{
						"type":        "string",
						"description": "Text to echo.",
					},
				},
				"required": []string{"content"},
			},
		}}
		return write(jsonrpcMessage{ID: msg.ID, Result: map[string]any{"tools": tools}})

	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			return write(jsonrpcMessage{
				ID:    msg.ID,
				Error: &jsonrpcError{Code: -32602, Message: "invalid params"},
			})
		}
		if params.Name != "echo_send" {
			return write(jsonrpcMessage{
				ID:    msg.ID,
				Error: &jsonrpcError{Code: -32601, Message: "unknown tool"},
			})
		}
		content, _ := params.Arguments["content"].(string)
		fmt.Fprintf(os.Stderr, "ECHO: %s\n", content)
		logf("echo_send content=%q", content)
		result := map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": "echoed",
			}},
		}
		return write(jsonrpcMessage{ID: msg.ID, Result: result})

	default:
		return write(jsonrpcMessage{
			ID:    msg.ID,
			Error: &jsonrpcError{Code: -32601, Message: "method not found: " + msg.Method},
		})
	}
}

// openChannelLogFile returns a per-pid log file under the agent's channels
// dir. Writing to a pid-scoped file avoids clobbering logs from prior runs.
func openChannelLogFile(agent, chanType string) (*os.File, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".mesh", "agents", agent, "channels")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%d.log", chanType, os.Getpid()))
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}
