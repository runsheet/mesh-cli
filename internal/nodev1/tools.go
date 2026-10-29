package nodev1

import (
	"time"

	"projectmesh.io/mesh-cli/internal/gitrelay"
	"projectmesh.io/mesh-cli/internal/mcptool"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/nodetools"
)

// nodeTools are the node tools the brief's kind names (Mesh v1 spec,
// "Evidence signing"): run_suite, apply_onto and push, each recording
// its signed ToolRecord through the lease's recorder, which keeps the
// evidence ids for the finish. Each is signed keylessly as the lease's
// session — tokens, held here alone, give Fulcio the mesh-session
// token — or, when the node has no Fulcio and Rekor or cannot reach
// them, by the node's own key. A tool the node does not have is left
// out and logged.
func (r *Runtime) nodeTools(b *meshapi.Brief, grant *gitrelay.Grant, workspace string, tokens *sessionTokens) ([]mcptool.Tool, *nodetools.Recorder) {
	if len(b.NodeTools) == 0 {
		return nil, nil
	}
	signer := &nodetools.Signer{Sigstore: r.Sigstore, NodeKey: r.SigningKey}
	if tokens != nil {
		signer.Token = tokens.Token
	}
	rec := &nodetools.Recorder{
		Lease: nodetools.Lease{JobID: b.JobID, LeaseID: b.LeaseID, LeaseN: b.LeaseN, Product: b.Product, Tenant: b.Tenant,
			NodeID: r.NodeID(), EvidenceRequired: b.EvidenceRequired},
		Signer: signer, Mesh: r.API,
	}
	var g nodetools.Grant
	repo := ""
	if grant != nil {
		g = grant
	}
	if b.Git != nil {
		repo = b.Git.Repo
	}
	var out []mcptool.Tool
	for _, name := range b.NodeTools {
		switch name {
		case nodetools.PushTool:
			p := &nodetools.Push{Grant: g, Repo: repo, Workspace: workspace, Recorder: rec,
				Refusal: func(since time.Time) string { return r.refusalSince(b.LeaseID, since) }}
			out = append(out, p.Tool())
		case nodetools.RunSuiteTool:
			out = append(out, (&nodetools.RunSuite{Workspace: workspace, Recorder: rec}).Tool())
		case nodetools.ApplyOntoTool:
			out = append(out, (&nodetools.ApplyOnto{Grant: g, Repo: repo, Workspace: workspace, Code: b.Code, Recorder: rec}).Tool())
		default:
			r.logf("nodev1: lease %s: the kind names node tool %q, which this node does not have", b.LeaseID, name)
		}
	}
	return out, rec
}
