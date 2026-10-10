package node

import (
	"context"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// TestNodeMutationsOnBothBearers runs the node's own registerNode,
// heartbeatNode and claimWorkItem — Register over the Registrar `mesh
// node start` builds (RegisterInput on RegisterOwner), the heartbeat
// the loop sends under the owned id, the claim the loop makes for a
// ready item — through gatewaytest's Fake on a person's console token
// and on a machine credential. The fake checks every input against the
// node-mutations.json rule for the bearer's kind, so a field the gateway
// starts to require, or to refuse, fails here (mesh-cli ADR-026).
func TestNodeMutationsOnBothBearers(t *testing.T) {
	const itemID = "9d1c2b3a-4e5f-4a6b-8c7d-0e1f2a3b4c5d"
	for _, kind := range []string{personBearer, machineBearer} {
		t.Run(kind, func(t *testing.T) {
			f, gw := newBearerFake(t, kind)
			f.WorkItems[itemID] = map[string]any{"id": itemID, "seq": 1, "title": "a ready item", "status": "ready"}
			ctx := context.Background()
			cfg := ownedCfg()
			credID := bearerCredentialID(kind)
			r := Registrar{
				CredentialID:  credID,
				Machine:       kind == machineBearer,
				TakesOwner:    func(context.Context) bool { return true },
				ConfiguredOrg: func(context.Context) (string, error) { return "33333333-3333-4333-8333-333333333333", nil },
				RegisterOwner: func(ctx context.Context, nodeID, orgID, ownerID string) (*gateway.NodeAnswer, error) {
					return gw.RegisterNode(ctx, RegisterInput(cfg, nodeID, orgID, ownerID, cfg.OwnerKind(), credID, "v1.2.3"))
				},
			}

			var nodeID string
			t.Run("register", func(t *testing.T) {
				reg, err := Register(ctx, r, cfg, nil)
				if err != nil {
					t.Fatalf("registerNode on a %s bearer: %v", kind, err)
				}
				if reg.Owned == nil || reg.Owned.NodeID == "" {
					t.Fatalf("registerNode on a %s bearer: no owned node id in %+v", kind, reg)
				}
				nodeID = reg.Owned.NodeID
				calls := f.CallsNamed("registerNode")
				if len(calls) == 0 {
					t.Fatal("the fake took no registerNode")
				}
				if got := f.BearerKind(calls[len(calls)-1].Bearer); got != kind {
					t.Errorf("registerNode went on a %s bearer, want %s", got, kind)
				}
			})
			if nodeID == "" {
				t.Fatal("no node registered; heartbeat and claim not run")
			}

			t.Run("heartbeat", func(t *testing.T) {
				if _, err := gw.HeartbeatNode(ctx, nodeID, "v1.2.3", 0, gateway.HeartbeatExtras{}); err != nil {
					t.Fatalf("heartbeatNode on a %s bearer: %v", kind, err)
				}
				if len(f.CallsNamed("heartbeatNode")) == 0 {
					t.Fatal("the fake took no heartbeatNode")
				}
			})

			t.Run("claim", func(t *testing.T) {
				if err := gw.ClaimWorkItem(actorContext(ctx, cfg, gateway.RoleBuilder), itemID, nodeID, "claude-code"); err != nil {
					t.Fatalf("claimWorkItem on a %s bearer: %v", kind, err)
				}
				if got := f.Item(itemID)["status"]; got != "claimed" {
					t.Errorf("item status after the claim = %v, want claimed", got)
				}
			})
		})
	}
}
