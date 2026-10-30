package nodev1

import (
	"time"

	"projectmesh.io/mesh-cli/internal/escalate"
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
// them, by the node's own key. escalate, and a red run_suite or a
// conflicted apply_onto, move the session up the lease's ladder through
// its mailbox (PR 2.11). A tool the node does not have never reaches
// here: work hands the lease back tool_unavailable first (causes.go).
// suites, set, runs run_suite's steps in the lease's pod; nil runs them
// on the host only on a node explicitly unsandboxed (hostSuites), and
// otherwise work hands the lease back tool_unavailable (podsuites.go).
//
// pusher, set, is how push pushes: through the lease's relay sidecar
// (relaypod.go), never the node's own grant.
func (r *Runtime) nodeTools(b *meshapi.Brief, grant *gitrelay.Grant, workspace string, tokens *sessionTokens, ladder *escalate.Ladder, mb *Mailbox, suites nodetools.SuiteRunner, pusher nodetools.Pusher) ([]mcptool.Tool, *nodetools.Recorder) {
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
			p := &nodetools.Push{Grant: g, Pusher: pusher, Repo: repo, Workspace: workspace, Recorder: rec,
				Refusal: func(since time.Time) string { return r.refusalSince(b.LeaseID, since) }}
			out = append(out, p.Tool())
		case nodetools.RunSuiteTool:
			out = append(out, (&nodetools.RunSuite{Workspace: workspace, Recorder: rec, Run: suites,
				OnHost: suites == nil && r.hostSuites(), Escalate: escalateOn(ladder, mb, escalate.OnRed)}).Tool())
		case nodetools.ApplyOntoTool:
			out = append(out, (&nodetools.ApplyOnto{Grant: g, Repo: repo, Workspace: workspace, Code: b.Code, Recorder: rec,
				Escalate: escalateOn(ladder, mb, escalate.OnConflict)}).Tool())
		case EscalateTool:
			out = append(out, escalateTool(ladder, mb))
		default:
			// result, hand_back and progress are endTools' (endtools.go)
			if isEndTool(name) {
				continue
			}
			r.logf("nodev1: lease %s: the kind names node tool %q, which this node does not have", b.LeaseID, name)
		}
	}
	return out, rec
}

// escalateOn is a node tool's escalation on trigger on: when the kind
// escalates on it and the ladder has a next model, it leaves the step in
// the lease's mailbox for the session's loop and answers that model;
// otherwise "". Nil when the lease has no ladder or mailbox.
func escalateOn(ladder *escalate.Ladder, mb *Mailbox, on string) func(string) string {
	if ladder == nil || mb == nil {
		return nil
	}
	return func(why string) string {
		to, err := ladder.Peek(on)
		if err != nil {
			return ""
		}
		if mb.Post(Message{Kind: MsgEscalate, On: on, Text: clip(why, 500)}) != nil {
			return ""
		}
		return to
	}
}
