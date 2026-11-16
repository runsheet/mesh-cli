package nodev1

import (
	"context"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/escalate"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// A Codex session has no set_model (codexsession.go): the escalate tool
// built for one refuses the call outright, naming the capability it
// lacks, rather than posting a step the session's loop can only log and
// drop in silence.
func TestEscalateTool_RefusesOnCodex(t *testing.T) {
	mb := NewMailbox()
	tool := escalateTool(escalate.New(meshapi.ModelPolicy{Default: "haiku", Escalate: []string{"sonnet"}, On: []string{escalate.OnAsked}}), mb, true)
	out, err := tool.Call(context.Background(), map[string]any{"reason": "the refactor is too big"})
	if err == nil {
		t.Fatalf("a codex session escalating: %s", out)
	}
	if !strings.Contains(err.Error(), "change model mid-session") {
		t.Fatalf("the refusal does not name the capability: %v", err)
	}
	select {
	case m := <-mb.C():
		t.Fatalf("a message reached the mailbox: %+v", m)
	default:
	}
}
