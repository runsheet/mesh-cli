package nodetools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"projectmesh.io/mesh-cli/internal/mcptool"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// A session's hand-back (projectmesh-io/api ADR 001 amendment, node
// tools): the session says the job cannot run here, and the node hands
// the lease back with the cause once the session ends. The job is
// offered again, keeps its place in the queue and spends no attempt, and
// the cause feeds the kind's breaker. A session may claim only the
// causes it can know; the rest — a stall, a killed pod, a restart — are
// the node's or Mesh's to say.

// HandBackTool is the hand_back node tool's name, as a kind's node_tools
// names it.
const HandBackTool = "hand_back"

// SessionCauses are the hand-back causes a session may claim:
// cannot_run (the job cannot run on this node or in this session) and
// product (the product's tools or brief are broken).
var SessionCauses = []string{meshapi.CauseCannotRun, meshapi.CauseProduct}

// maxHandBackReason bounds a session's reason, as the node bounds its
// own.
const maxHandBackReason = 2000

// HandBack is one lease's hand_back tool.
type HandBack struct {
	// Accept keeps the hand-back for the lease's report and closes the
	// session's input.
	Accept func(cause, reason string)
}

// Tool is hand_back {cause, reason}.
func (h *HandBack) Tool() mcptool.Tool {
	return mcptool.Tool{
		Name: HandBackTool,
		Description: "Give the job back when it cannot be done here, for a reason that is not the work's own: " +
			"cause cannot_run when this node or session cannot run it (a tool or service it needs is missing), " +
			"cause product when the product's tools or brief are broken. The job is offered again, to this node or another, " +
			"and costs no attempt. Say why in reason. Once accepted the session should end: call it last.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"cause":  map[string]any{"type": "string", "enum": SessionCauses, "description": "why the job goes back"},
				"reason": map[string]any{"type": "string", "description": "what stopped the job, in one or two lines"},
			},
			"required": []string{"cause", "reason"},
		},
		Call: func(_ context.Context, args map[string]any) (string, error) {
			cause, _ := args["cause"].(string)
			reason, _ := args["reason"].(string)
			cause, reason = strings.TrimSpace(cause), strings.TrimSpace(reason)
			if !slices.Contains(SessionCauses, cause) {
				return "", fmt.Errorf("refused: cause is one of %s; any other is the node's to say", strings.Join(SessionCauses, ", "))
			}
			if reason == "" {
				return "", fmt.Errorf("refused: reason is required")
			}
			if len(reason) > maxHandBackReason {
				reason = reason[:maxHandBackReason]
			}
			if h.Accept != nil {
				h.Accept(cause, reason)
			}
			out, _ := json.Marshal(map[string]any{"handing_back": true, "cause": cause,
				"note": "the node hands the job back when your session ends; end it now"})
			return string(out), nil
		},
	}
}
