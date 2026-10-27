package node

import (
	"fmt"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// lineageOf is an item's lineage: its root's seq and its attempt.
func lineageOf(it gateway.WorkItem) (root, attempt int) {
	if it.LineageRootSeq > 0 {
		return it.LineageRootSeq, it.LineageAttempt
	}
	return it.Seq, it.LineageAttempt
}

// lineageWords says a lineage for a log line.
func lineageWords(root, attempt int) string {
	if attempt > 0 {
		return fmt.Sprintf("lineage %d, attempt %d of 3", root, attempt)
	}
	return fmt.Sprintf("lineage %d", root)
}
