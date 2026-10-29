package nodev1

import (
	"fmt"
	"strings"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// landedWrites are the irreversible external writes among a lease's
// AccessRecords, one line each: a push the relay forwarded and the
// vendor accepted, and any vendor API call that is not a read. A lease
// that has made one has changed the world outside it, and a run of the
// job from scratch would meet those writes (a retried lease's pushes
// refused as non-fast-forward; TestWritePath, 2026-10-30).
func landedWrites(records []meshapi.AccessRecord) []string {
	var out []string
	for _, a := range records {
		if a.Outcome != "ok" {
			continue
		}
		switch {
		case a.Kind == "git" && a.Service == "receive-pack" && len(a.Refs) > 0:
			out = append(out, fmt.Sprintf("pushed %s to %s", strings.Join(a.Refs, ", "), a.Repo))
		case a.Kind == "api" && !readMethod(a.Method):
			out = append(out, fmt.Sprintf("called %s %s %s", a.Vendor, a.Method, a.Path))
		}
	}
	return out
}

func readMethod(m string) bool {
	switch strings.ToUpper(m) {
	case "GET", "HEAD", "OPTIONS":
		return true
	}
	return false
}

// afterWrites is a hand-back's or failure's reason with the lease's
// landed writes named, so neither Mesh nor the product can miss that
// the job's next run starts from a world those writes changed.
func afterWrites(reason string, writes []string) string {
	if len(writes) == 0 {
		return reason
	}
	// the writes are the part that must reach Mesh: the reason gives
	// way to them under maxReason
	return fmt.Sprintf("%s; before this the lease had already %s, which cannot be undone: a retry must build on them, not run from scratch", clip(reason, maxReason/2), strings.Join(writes, "; "))
}
