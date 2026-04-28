package shim

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// RunEchoChannelServer runs the echo MCP channel server on stdin/stdout.
func RunEchoChannelServer(agent string, index int) error {
	if agent == "" {
		return fmt.Errorf("--agent is required")
	}

	logFile, err := OpenChannelLogFile(agent, "echo")
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

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		logf("received signal %s, shutting down", sig)
		cancel()
	}()

	var writeMu sync.Mutex
	write := func(msg JSONRPCMessage) error {
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
				_ = write(JSONRPCMessage{
					Method: "notifications/message",
					Params: params,
				})
			}
		}
	}()

	reader := bufio.NewReader(os.Stdin)
	for {
		if ctx.Err() != nil {
			return nil
		}
		line, err := reader.ReadBytes('\n')
		trimmed := BytesTrimSpace(line)
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
			return err
		}
	}
}

// handleEchoMessage parses a single inbound JSON-RPC frame and dispatches it.
func handleEchoMessage(raw []byte, write func(JSONRPCMessage) error, logf func(string, ...any)) error {
	var msg JSONRPCMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return fmt.Errorf("parse frame: %w", err)
	}
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
		return write(JSONRPCMessage{ID: msg.ID, Result: result})

	case "tools/list":
		tools := []map[string]any{{
			"name":        "echo_send",
			"description": "Logs the provided content to the echo channel's stderr.",
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
		return write(JSONRPCMessage{ID: msg.ID, Result: map[string]any{"tools": tools}})

	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			return write(JSONRPCMessage{
				ID:    msg.ID,
				Error: &JSONRPCError{Code: -32602, Message: "invalid params"},
			})
		}
		if params.Name != "echo_send" {
			return write(JSONRPCMessage{
				ID:    msg.ID,
				Error: &JSONRPCError{Code: -32601, Message: "unknown tool"},
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
		return write(JSONRPCMessage{ID: msg.ID, Result: result})

	default:
		return write(JSONRPCMessage{
			ID:    msg.ID,
			Error: &JSONRPCError{Code: -32601, Message: "method not found: " + msg.Method},
		})
	}
}
