package nodev1

import (
	"context"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/escalate"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// A Codex session has no set_model (codexsession.go's own doc comment),
// so the escalate tool refuses up front rather than answering
// "escalating: true" and leaving CodexSession.loop to drop the step on
// the floor with only a log line.
func TestEscalateTool_RefusesOnCodex(t *testing.T) {
	mb := NewMailbox()
	tool := escalateTool(escalate.New(meshapi.ModelPolicy{Default: "haiku", Escalate: []string{"sonnet"}, On: []string{escalate.OnAsked}}), mb, true)
	if _, err := tool.Call(context.Background(), map[string]any{"reason": "the refactor is too big"}); err == nil || !strings.Contains(err.Error(), "change model mid-session") {
		t.Fatalf("%v", err)
	}
	select {
	case m := <-mb.C():
		t.Fatalf("a message reached the mailbox: %+v", m)
	default:
	}
}
