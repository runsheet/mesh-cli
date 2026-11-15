package nodev1

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"

	"projectmesh.io/mesh-cli/internal/harness"
)

// fixtureInteraction is one abstract interaction a session can have —
// the assistant answers with text, runs one command and sees its
// output, and the turn ends — independent of which harness runs it:
// claudeFixtureRun and codexFixtureRun each stand in for the vendor's
// own process, replaying it as that vendor's own wire protocol.
type fixtureInteraction struct {
	text      string
	toolID    string
	command   string
	output    string
	toolError bool
}

// claudeFixtureRun is a fake `claude -p --output-format stream-json`
// that answers fx on its first turn: an assistant text event, an
// assistant tool_use event (named command_execution, so its shape
// matches what a Codex session's own command_execution item becomes —
// codexsession.go's toolUseLine), a user tool_result event, and a
// result event — then drains the input until it closes, as a real
// `claude` does once its turn is sent to its end.
func claudeFixtureRun(fx fixtureInteraction) PodRun {
	return func(_ context.Context, _, _ []string, stdin io.Reader, stdout, _ io.Writer) error {
		emit := func(v any) {
			b, _ := json.Marshal(v)
			fmt.Fprintln(stdout, string(b))
		}
		sc := bufio.NewScanner(stdin)
		sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
		sent := false
		for sc.Scan() {
			if sent {
				continue
			}
			sent = true
			emit(map[string]any{"type": "assistant", "message": map[string]any{
				"role": "assistant", "content": []map[string]any{{"type": "text", "text": fx.text}},
			}})
			emit(map[string]any{"type": "assistant", "message": map[string]any{
				"role": "assistant", "content": []map[string]any{{
					"type": "tool_use", "id": fx.toolID, "name": "command_execution",
					"input": map[string]any{"command": fx.command},
				}},
			}})
			emit(map[string]any{"type": "user", "message": map[string]any{
				"role": "user", "content": []map[string]any{{
					"type": "tool_result", "tool_use_id": fx.toolID, "is_error": fx.toolError, "content": fx.output,
				}},
			}})
			emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": fx.text})
		}
		return nil
	}
}

// codexFixtureRun is a fake `codex app-server` that answers fx the same
// way the real one would: an agentMessage delta carrying fx.text, a
// command_execution item's item.started and item.completed for fx's
// command and output, and a turn.completed — each rewritten by
// CodexSession's own codexTranslator (codexsession.go) into the same
// stream-json shape claudeFixtureRun writes directly.
func codexFixtureRun(fx fixtureInteraction) PodRun {
	return func(_ context.Context, _, _ []string, stdin io.Reader, stdout, _ io.Writer) error {
		emit := func(v any) {
			b, _ := json.Marshal(v)
			fmt.Fprintln(stdout, string(b))
		}
		sc := bufio.NewScanner(stdin)
		sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
		for sc.Scan() {
			var m struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			var id int
			_ = json.Unmarshal(m.ID, &id)
			switch m.Method {
			case "thread/start":
				emit(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"threadId": "fake-thread"}})
			case "turn/start":
				emit(map[string]any{"jsonrpc": "2.0", "method": "agentMessage", "params": map[string]any{"text": fx.text}})
				status := "completed"
				if fx.toolError {
					status = "failed"
				}
				emit(map[string]any{"jsonrpc": "2.0", "method": "item.started", "params": map[string]any{"item": map[string]any{
					"id": fx.toolID, "type": "command_execution", "command": fx.command, "status": "in_progress",
				}}})
				emit(map[string]any{"jsonrpc": "2.0", "method": "item.completed", "params": map[string]any{"item": map[string]any{
					"id": fx.toolID, "type": "command_execution", "command": fx.command,
					"aggregated_output": fx.output, "status": status,
				}}})
				emit(map[string]any{"jsonrpc": "2.0", "method": "turn.completed", "params": map[string]any{}})
			}
		}
		return nil
	}
}

// recordEvents runs s to its end on run, answering every harness.Event
// its OnEvent heard, in order.
func recordEvents(t *testing.T, s Session, run PodRun) []harness.Event {
	t.Helper()
	var events []harness.Event
	spec := SessionSpec{
		Workspace: t.TempDir(), Prompt: "do the job",
		InPod:   run,
		OnEvent: func(ev harness.Event) { events = append(events, ev) },
	}
	if _, err := s.Run(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	return events
}

// contentShape is the fields of an event's content block the fixture
// cares about: a block's own Text, Name, ID, ToolUseID and IsError, as
// the acceptance names them.
type contentShape struct {
	Type      string
	Text      string
	Name      string
	ID        string
	ToolUseID string
	IsError   bool
}

// eventShape is the fields of one harness.Event two translations of the
// same interaction must agree on: its Type, its message's Role, each of
// its content blocks, and — on the final result event alone — its
// Subtype and IsError.
type eventShape struct {
	Type    string
	Role    string
	Content []contentShape
	Subtype string
	IsError bool
}

func shapesOf(evs []harness.Event) []eventShape {
	shapes := make([]eventShape, 0, len(evs))
	for _, ev := range evs {
		s := eventShape{Type: ev.Type}
		if ev.Type == "result" {
			s.Subtype, s.IsError = ev.Subtype, ev.IsError
		}
		if ev.Message != nil {
			s.Role = ev.Message.Role
			for _, c := range ev.Message.Content {
				s.Content = append(s.Content, contentShape{
					Type: c.Type, Text: c.Text, Name: c.Name, ID: c.ID, ToolUseID: c.ToolUseID, IsError: c.IsError,
				})
			}
		}
		shapes = append(shapes, s)
	}
	return shapes
}

// TestConformance_ClaudeAndCodexTranslateTheSameInteraction runs one
// fixture interaction — assistant text, a command run and its output,
// the turn's own end — through ClaudeSession (whose stream is already
// in the target shape) and through CodexSession (whose codexTranslator
// rewrites app-server's own notifications into it), and checks the two
// event sequences agree field-for-field on what stoprules' Watch and
// the lease's liveness and evidence hooks (lease.go) read off either
// one: Type, Message.Role, each content block's Type/Text/Name/ID/
// ToolUseID/IsError, and the final result event's Subtype/IsError. A
// change to codexTranslator's shape that drifts from what a real
// `claude` emits — a renamed field, a different role, a tool_use with
// no matching id — fails here before it reaches a lease.
func TestConformance_ClaudeAndCodexTranslateTheSameInteraction(t *testing.T) {
	fx := fixtureInteraction{
		text: "building the thing", toolID: "call-1", command: "echo hi", output: "hi\n", toolError: false,
	}

	claudeEvents := recordEvents(t, &ClaudeSession{Binary: "claude", Logf: t.Logf}, claudeFixtureRun(fx))
	codexEvents := recordEvents(t, &CodexSession{Binary: "codex", Logf: t.Logf}, codexFixtureRun(fx))

	claudeShapes, codexShapes := shapesOf(claudeEvents), shapesOf(codexEvents)
	if len(claudeShapes) != len(codexShapes) {
		t.Fatalf("claude produced %d events, codex %d:\nclaude: %+v\ncodex:  %+v",
			len(claudeShapes), len(codexShapes), claudeShapes, codexShapes)
	}
	for i := range claudeShapes {
		if claudeShapes[i].Type != codexShapes[i].Type {
			t.Fatalf("event %d: claude Type %q, codex %q", i, claudeShapes[i].Type, codexShapes[i].Type)
		}
		if claudeShapes[i].Role != codexShapes[i].Role {
			t.Fatalf("event %d (%s): claude Role %q, codex %q", i, claudeShapes[i].Type, claudeShapes[i].Role, codexShapes[i].Role)
		}
		if claudeShapes[i].Type == "result" {
			if claudeShapes[i].Subtype != codexShapes[i].Subtype {
				t.Fatalf("result event: claude Subtype %q, codex %q", claudeShapes[i].Subtype, codexShapes[i].Subtype)
			}
			if claudeShapes[i].IsError != codexShapes[i].IsError {
				t.Fatalf("result event: claude IsError %v, codex %v", claudeShapes[i].IsError, codexShapes[i].IsError)
			}
		}
		if len(claudeShapes[i].Content) != len(codexShapes[i].Content) {
			t.Fatalf("event %d (%s): claude has %d content blocks, codex %d: %+v vs %+v",
				i, claudeShapes[i].Type, len(claudeShapes[i].Content), len(codexShapes[i].Content), claudeShapes[i].Content, codexShapes[i].Content)
		}
		for j := range claudeShapes[i].Content {
			if claudeShapes[i].Content[j] != codexShapes[i].Content[j] {
				t.Fatalf("event %d (%s) content block %d: claude %+v, codex %+v",
					i, claudeShapes[i].Type, j, claudeShapes[i].Content[j], codexShapes[i].Content[j])
			}
		}
	}
}
