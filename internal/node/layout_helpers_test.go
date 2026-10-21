package node

import (
	"path/filepath"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// testLayout is a Layout under a temp ~/.mesh.
func testLayout(t *testing.T) *Layout {
	t.Helper()
	return &Layout{Dir: filepath.Join(t.TempDir(), ".mesh")}
}

// connected is scopes with each scope given a connection, "conn-<name>",
// and the runsheet product, as the node's answer names them (ADR 066).
func connected(scopes Scopes) Scopes {
	out := make(Scopes, len(scopes))
	for i, sc := range scopes {
		if sc.ConnectionID == "" {
			sc.ConnectionID = "conn-" + sc.Name()
		}
		if sc.Product == "" {
			sc.Product = "runsheet"
		}
		out[i] = sc
	}
	return out
}

// testItem is an item of goal-1 in org.
func testItem(id, org string) gateway.WorkItem {
	return gateway.WorkItem{ID: id, OrgID: org, GoalID: "goal-1", Seq: 1}
}

// testGoal is a goal by id and slug.
func testGoal(id, slug string) gateway.Goal {
	return gateway.Goal{ID: id, Slug: slug, Status: "planning"}
}
