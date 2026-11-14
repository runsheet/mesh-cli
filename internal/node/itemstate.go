package node

import "projectmesh.io/mesh-cli/internal/gateway"

// SessionLive is whether an item is in a status a node session runs
// under: claimed or running (a build), evaluating (an evaluation), or a
// rebase claimed (runsheet ADR 008). Every other — ready, pending,
// review, merged, failed, cancelled, done, already_done — is terminal
// for a session: none should be running on it.
func SessionLive(item gateway.WorkItem) bool {
	switch item.Status {
	case "claimed", "running", "evaluating":
		return true
	}
	return item.RebaseState == "claimed"
}
