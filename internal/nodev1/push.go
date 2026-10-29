package nodev1

import (
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/nodetools"
)

// Every write goes through the relay (Mesh v1 spec, "Many git vendors,
// and every write through the relay"). The push node tool
// (nodetools.Push) pushes a commit of the lease's checkout through the
// lease's grant — the relay's loopback capability, never a vendor URL —
// and the relay checks it against the brief's push rules and records it
// as the lease's AccessRecord. There is no other path from nodev1 to a
// vendor: no credential helper, no write token outside the relay and no
// fallback (guard_test.go).

// NodeToolsServer is the in-process MCP server the node's own tools are
// served at on a lease's sessionproxy, beside the product's servers.
const NodeToolsServer = "mesh-node"

// PushTool is the push node tool's name, as a kind's node_tools names it.
const PushTool = nodetools.PushTool

// refusalSince is the reason of the relay's latest refused push of the
// lease since since, "" when there is none.
func (r *Runtime) refusalSince(lease string, since time.Time) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var recs []meshapi.AccessRecord
	if l := r.lease(lease); l != nil {
		recs = l.access
	}
	for i := len(recs) - 1; i >= 0; i-- {
		rec := recs[i]
		if rec.At.Before(since.Add(-time.Second)) {
			break
		}
		if rec.Kind == "git" && rec.Service == "receive-pack" && rec.Outcome == "refused" {
			return rec.Reason
		}
	}
	return ""
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
