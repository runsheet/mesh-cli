package nodev1

import (
	"encoding/json"
	"sync"

	"projectmesh.io/mesh-cli/internal/mcptool"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/nodetools"
)

// The node tools that end a lease (projectmesh-io/api ADR 001
// amendment, node tools): result hands in the job's result for the
// finish, hand_back gives the job back with a cause a session may claim,
// and progress posts a note on the lease. result and hand_back each
// close the session's input (Mailbox.Finish), so a one-shot session ends
// once its turn does, and the lease's report acts on what they kept.

// sessionHandBack is the hand-back a lease's session asked for with
// hand_back, kept for the lease's report.
type sessionHandBack struct {
	mu            sync.Mutex
	cause, reason string
}

// keep records the session's hand-back; a later call replaces it.
func (h *sessionHandBack) keep(cause, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cause, h.reason = cause, reason
}

// asked is the hand-back the session asked for, nil when it asked none.
func (h *sessionHandBack) asked() *meshapi.HandBackRequest {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cause == "" {
		return nil
	}
	return &meshapi.HandBackRequest{Cause: h.cause, Reason: h.reason}
}

// keep records raw as the job's result, as the product's finish tool's
// answer is recorded, and closes the session's input.
func (f *finishCapture) keep(raw json.RawMessage) {
	f.mu.Lock()
	f.got = raw
	f.mu.Unlock()
	if f.onFinish != nil {
		f.onFinish()
	}
}

// isEndTool says whether name is one of result, hand_back and
// progress, which endTools serves rather than nodeTools.
func isEndTool(name string) bool {
	switch name {
	case nodetools.ResultTool, nodetools.HandBackTool, nodetools.ProgressTool:
		return true
	}
	return false
}

// endTools are the tools among result, hand_back and progress the kind
// names, in its order: result keeps its value in fin, hand_back its cause
// in hb, and progress posts on the lease.
func (r *Runtime) endTools(b *meshapi.Brief, fin *finishCapture, hb *sessionHandBack, mb *Mailbox) []mcptool.Tool {
	var out []mcptool.Tool
	for _, name := range b.NodeTools {
		switch name {
		case nodetools.ResultTool:
			out = append(out, (&nodetools.Result{Schema: b.FinishSchema, Accept: fin.keep}).Tool())
		case nodetools.HandBackTool:
			out = append(out, (&nodetools.HandBack{Accept: func(cause, reason string) {
				hb.keep(cause, reason)
				mb.Finish()
			}}).Tool())
		case nodetools.ProgressTool:
			var mesh nodetools.ProgressPoster
			if r.API != nil {
				mesh = r.API
			}
			out = append(out, (&nodetools.Progress{LeaseID: b.LeaseID, Mesh: mesh}).Tool())
		}
	}
	return out
}
