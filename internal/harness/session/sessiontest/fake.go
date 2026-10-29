// Package sessiontest is a fake claude for streaming-session tests: a
// test binary runs it as `claude` from its TestMain (Run).
package sessiontest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Env is the variable that makes a test binary the fake claude.
const Env = "FAKE_STREAM_CLAUDE"

// Main runs the fake and exits when the test binary was started as the
// fake claude (Env=1); else it returns, for TestMain to run the tests.
// With FAKE_CLAUDE_OUT set, the fake writes its args there (args, one a
// line), its whole environment (env, one variable a line) and every
// input line (stdin); with ANTHROPIC_BASE_URL set too, its first turn
// calls the model there as Claude Code does (modelCall), writing what
// the model answered (model).
func Main() {
	if os.Getenv(Env) != "1" {
		return
	}
	os.Exit(Fake(os.Args[1:], os.Stdin, os.Stdout))
}

// Fake speaks the stream-json protocol as claude 2.1.312 did in
// the PR 2.10 spike: every user turn opens with a system/init event
// naming the model and ends in its own result event (num_turns that
// turn's, total_cost_usd and modelUsage the session's); set_model and
// interrupt are answered with a control_response; the end of the input
// ends the session once the turn in flight is done. A turn "hang" runs
// until an interrupt; "slow" takes a moment; set_model "refuse" is
// refused; a turn "fail" ends in an error result.
func Fake(args []string, stdin io.Reader, stdout io.Writer) int {
	out := os.Getenv("FAKE_CLAUDE_OUT")
	var record io.Writer = io.Discard
	if out != "" {
		_ = os.WriteFile(out+".args", []byte(strings.Join(args, "\n")+"\n"), 0o600)
		_ = os.WriteFile(out+".env", []byte(strings.Join(os.Environ(), "\n")+"\n"), 0o600)
		if f, err := os.Create(out + ".stdin"); err == nil {
			defer f.Close()
			record = f
		}
	}
	model := "claude-haiku"
	for i, a := range args {
		if a == "--model" && i+1 < len(args) {
			model = "claude-" + args[i+1]
		}
	}
	emit := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintln(stdout, string(b))
	}
	lines := make(chan []byte)
	go func() {
		sc := bufio.NewScanner(stdin)
		for sc.Scan() {
			fmt.Fprintln(record, sc.Text())
			lines <- append([]byte(nil), sc.Bytes()...)
		}
		close(lines)
	}()
	type msg struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype string `json:"subtype"`
			Model   string `json:"model"`
		} `json:"request"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	called := false
	spend := map[string]map[string]any{}
	cost := 0.0
	control := func(m msg) {
		switch m.Request.Subtype {
		case "set_model":
			if m.Request.Model == "refuse" {
				emit(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": m.RequestID, "error": "no such model"}})
				return
			}
			model = "claude-" + m.Request.Model
			emit(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": m.RequestID, "response": map[string]any{"model": model}}})
		default:
			emit(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": m.RequestID, "response": map[string]any{}}})
		}
	}
	for line := range lines {
		var m msg
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		if m.Type == "control_request" {
			control(m)
			continue
		}
		text := m.Message.Content
		if base := os.Getenv("ANTHROPIC_BASE_URL"); base != "" && out != "" && !called {
			called = true
			_ = os.WriteFile(out+".model", []byte(modelCall(base)), 0o600)
		}
		emit(map[string]any{"type": "system", "subtype": "init", "session_id": "fake-session", "model": model})
		emit(map[string]any{"type": "assistant", "session_id": "fake-session", "message": map[string]any{"id": "msg-" + text, "role": "assistant", "model": model, "content": []map[string]any{{"type": "text", "text": "working on " + text}}}})
		subtype := "success"
		switch text {
		case "fail":
			subtype = "error_during_execution"
		case "slow":
			time.Sleep(300 * time.Millisecond)
		case "hang":
			for l := range lines {
				var c msg
				if json.Unmarshal(l, &c) == nil && c.Type == "control_request" {
					control(c)
					if c.Request.Subtype == "interrupt" {
						subtype = "error_during_execution"
						break
					}
				}
			}
		}
		cost += 0.01
		spend[model] = map[string]any{"inputTokens": 2, "outputTokens": 1, "costUSD": 0.01}
		emit(map[string]any{"type": "result", "subtype": subtype, "is_error": subtype != "success", "result": "got: " + text,
			"session_id": "fake-session", "num_turns": 1, "total_cost_usd": cost, "modelUsage": spend})
	}
	return 0
}

// modelCall is one Messages API call at base, signed in as Claude Code
// signs in from its environment: ANTHROPIC_AUTH_TOKEN or
// CLAUDE_CODE_OAUTH_TOKEN as a bearer (the latter with the OAuth beta),
// else ANTHROPIC_API_KEY as x-api-key. It answers "<status> <body>", or
// the error.
func modelCall(base string) string {
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(base, "/")+"/v1/messages", strings.NewReader(`{"model":"claude-haiku","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		return "error " + err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	switch {
	case os.Getenv("ANTHROPIC_AUTH_TOKEN") != "":
		req.Header.Set("Authorization", "Bearer "+os.Getenv("ANTHROPIC_AUTH_TOKEN"))
	case os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "":
		req.Header.Set("Authorization", "Bearer "+os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"))
		req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
	default:
		req.Header.Set("X-Api-Key", os.Getenv("ANTHROPIC_API_KEY"))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "error " + err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return fmt.Sprintf("%d %s", resp.StatusCode, b)
}
