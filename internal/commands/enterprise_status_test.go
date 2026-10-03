package commands

import (
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/node"
)

// An enterprise answer's owner line reads "owned by anythink
// (enterprise); serving …", never the id or the person's shape.
func TestOwnedStatusLineEnterpriseAnswer(t *testing.T) {
	out := node.Outside{
		Owned: true,
		Answer: &gateway.NodeAnswer{
			NodeID: ownedNode,
			Owner:  gateway.NodeOwner{Kind: "enterprise", Name: "anythink"},
			Scopes: []gateway.ScopeAnswer{{OrgID: "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa", Login: "runsheet"}},
		},
	}
	line := ownedStatusLine(&node.Config{}, out)
	if !strings.Contains(line, "anythink (enterprise)") {
		t.Errorf("status line %q, want it to contain %q", line, "anythink (enterprise)")
	}
}

// A nil answer on an enterprise-owned node.yaml names the enterprise
// node.yaml itself records, since the answer carries none to fall
// back on.
func TestOwnedStatusLineNilAnswerNamesConfiguredEnterprise(t *testing.T) {
	out := node.Outside{Owned: true}
	cfg := &node.Config{Owner: "enterprise", Enterprise: "anythink"}
	line := ownedStatusLine(cfg, out)
	want := "owner: anythink (enterprise), the platform answered no orgs on this heartbeat"
	if line != want {
		t.Errorf("status line = %q, want %q", line, want)
	}
}
