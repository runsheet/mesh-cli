package nodetools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"projectmesh.io/mesh-cli/internal/mcptool"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// A session's progress (projectmesh-io/api ADR 001 amendment, node
// tools): one line of what it has done, which the node posts on the
// lease to Mesh (POST /v1/leases/{id}/progress), where it is the
// lease's newest note and the product reads it with its job tool. It is
// for people: the lease's liveness stays the node's heartbeat.

// ProgressTool is the progress node tool's name, as a kind's node_tools
// names it.
const ProgressTool = "progress"

// maxProgressNote is the longest note Mesh keeps, in bytes.
const maxProgressNote = 500

// ProgressPoster is Mesh, as the progress tool posts there
// (meshapi.Client).
type ProgressPoster interface {
	Progress(ctx context.Context, leaseID string, req meshapi.ProgressRequest) (*meshapi.Progressed, error)
}

// Progress is one lease's progress tool.
type Progress struct {
	LeaseID string
	Mesh    ProgressPoster
}

// Tool is progress {note}.
func (p *Progress) Tool() mcptool.Tool {
	return mcptool.Tool{
		Name: ProgressTool,
		Description: "Say in one line what you have done so far, for the people following the job. " +
			"Each note replaces the last; keep it short and call it at real milestones.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"note": map[string]any{"type": "string", "description": "one line, at most 500 bytes"},
			},
			"required": []string{"note"},
		},
		Call: func(ctx context.Context, args map[string]any) (string, error) {
			note, _ := args["note"].(string)
			note = strings.TrimSpace(note)
			if i := strings.IndexAny(note, "\r\n"); i >= 0 {
				note = strings.TrimSpace(note[:i])
			}
			if note == "" {
				return "", fmt.Errorf("refused: note is required")
			}
			if len(note) > maxProgressNote {
				return "", fmt.Errorf("refused: note is %d bytes; at most %d", len(note), maxProgressNote)
			}
			if p.Mesh == nil {
				return "", fmt.Errorf("the node cannot reach Mesh for this lease")
			}
			got, err := p.Mesh.Progress(ctx, p.LeaseID, meshapi.ProgressRequest{Note: note})
			if err != nil {
				return "", fmt.Errorf("the note did not reach Mesh: %v", err)
			}
			out, _ := json.Marshal(got)
			return string(out), nil
		},
	}
}
