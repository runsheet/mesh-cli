package node

import (
	"context"
	"testing"

	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// A build session's mesh session is minted for audience mesh-session,
// DPoP-bound to the node's own in-memory key, as mesh node start builds
// SessionBearers (internal/commands/nodecmd.go; runsheet ADR 064 §5).
func TestRelaySignsEachRequest(t *testing.T) {
	t.Run("mint_asks_mesh_session", func(t *testing.T) {
		key, err := dpop.NewKey()
		if err != nil {
			t.Fatal(err)
		}
		f := gatewaytest.New(machine)
		t.Cleanup(f.Close)
		f.WorkItems[item1] = map[string]any{"id": item1, "orgId": orgID, "status": "claimed", "nodeId": nodeA, "attempts": 1}
		gw := gateway.New(f.URL(), machine)
		b := &SessionBearers{GW: gw, Tokens: &TokenFiles{}, Key: key, Orgs: func() int { return 1 }}

		unbind, err := b.Open(context.Background(), gateway.WorkItem{ID: item1, OrgID: orgID, Attempts: 1}, nodeA)
		if err != nil {
			t.Fatal(err)
		}
		defer unbind()

		ms := f.MeshSessionFor(item1)
		if ms == nil || len(ms.Minted) == 0 {
			t.Fatalf("no token minted (%+v)", ms)
		}
		if len(ms.Audiences) != 1 || ms.Audiences[0] != MeshSessionAudience {
			t.Fatalf("audiences = %v, want [%s]", ms.Audiences, MeshSessionAudience)
		}
		tok := ms.Minted[0]
		if got := ms.JKT[tok]; got != key.Thumbprint() {
			t.Fatalf("token %q bound to %q, want %s", tok, got, key.Thumbprint())
		}
	})
}
