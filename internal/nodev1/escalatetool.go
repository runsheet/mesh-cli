package nodev1

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"projectmesh.io/mesh-cli/internal/escalate"
	"projectmesh.io/mesh-cli/internal/mcptool"
)

// EscalateTool is the escalate node tool's name, as a kind's node_tools
// names it: the session asking for a stronger model (Mesh v1 spec, node
// tools; PR 2.11, trigger "asked"). The node, not the model, decides:
// the kind's ladder names the next model and whether asking escalates at
// all, and the step is reported with the lease's usage.
const EscalateTool = "escalate"

// escalateTool is the lease's escalate tool: escalate {reason}. codex
// says the lease's session is a Codex one (internal/harness/codexsession;
// app-server has no set_model, codexsession.go's own doc comment), which
// has no way to move mid-session: the call refuses up front, naming why,
// rather than answering "escalating: true" and then silently dropping the
// step once posted, as CodexSession.loop's own MsgEscalate case used to
// leave it. A Claude session's call answers the model the session moves
// to and leaves the step in the lease's mailbox for the session's loop,
// which moves the live session with set_model.
func escalateTool(ladder *escalate.Ladder, mb *Mailbox, codex bool) mcptool.Tool {
	return mcptool.Tool{
		Name: EscalateTool,
		Description: "Ask for a stronger model when the job is beyond the one you run on: the node moves this session up the job's model ladder. " +
			"Say why. Answers the model you move to, or why there is none; carry on with the job either way.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"reason": map[string]any{"type": "string", "description": "why the job needs a stronger model"},
			},
			"required": []string{"reason"},
		},
		Call: func(_ context.Context, args map[string]any) (string, error) {
			reason, _ := args["reason"].(string)
			reason = strings.TrimSpace(reason)
			if reason == "" {
				return "", fmt.Errorf("reason is required")
			}
			if codex {
				return "", fmt.Errorf("this session has no capability to change model mid-session: codex's app-server has no set_model")
			}
			to, err := ladder.Peek(escalate.OnAsked)
			if err != nil {
				return "", err
			}
			if err := mb.Post(Message{Kind: MsgEscalate, On: escalate.OnAsked, Text: clip(reason, 500)}); err != nil {
				return "", err
			}
			out, _ := json.Marshal(map[string]any{"escalating": true, "to": to,
				"note": "the node moves this session to " + to + "; carry on with the job"})
			return string(out), nil
		},
	}
}
