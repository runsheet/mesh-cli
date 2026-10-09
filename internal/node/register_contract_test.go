package node

import (
	"context"
	"slices"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// registerRule is the fixture's registerNode rule for a bearer kind.
func registerRule(t *testing.T, kind string) gatewaytest.BearerRule {
	t.Helper()
	rules, err := gatewaytest.NodeMutationRules()
	if err != nil {
		t.Fatalf("node-mutations fixture: %v", err)
	}
	rule, ok := rules.Mutations["registerNode"]
	if !ok {
		t.Fatal("node-mutations fixture has no registerNode rule")
	}
	if kind == machineBearer {
		return rule.Machine
	}
	return rule.Person
}

// TestRegisterInputMatchesRuleFixture builds the registerNode input as
// the node sends it — RegisterInput, which `mesh node start`'s
// RegisterOwner and RegisterIn call, rendered by RegisterNodeFields on a
// current schema and wrapped as Mutate wraps it — on each bearer kind
// and each contract, and checks it against the fixture's rule for that
// kind directly, with no gateway (mesh-cli ADR-026).
func TestRegisterInputMatchesRuleFixture(t *testing.T) {
	cfg := &Config{Name: "mini", Slug: "mini", Capacity: Capacity{Min: 2, Max: 2}}
	const nodeID, orgID, ownerID = "5b0e2a52-0d7c-4a8e-9a1f-1d2c3b4a5f60", "0c4b1e8a-2a8b-4f43-9f0e-7a6b5c4d3e21", "11111111-1111-4111-8111-111111111111"
	for _, kind := range []string{personBearer, machineBearer} {
		for _, contract := range []struct{ name, owner string }{{ContractOwner, cfg.OwnerKind()}, {ContractPerOrg, ""}} {
			t.Run(kind+"/"+contract.name, func(t *testing.T) {
				in := RegisterInput(cfg, nodeID, orgID, ownerID, contract.owner, bearerCredentialID(kind), "v1.2.3")
				sent := gateway.MutationInput(in.NodeID, gateway.RegisterNodeFields(in, "ownerKind", true, true))
				if err := registerRule(t, kind).Check("registerNode", kind, sent); err != nil {
					t.Fatalf("the node's registerNode on a %s bearer breaks the fixture: %v\ninput: %v", kind, err, sent)
				}
				got, has := sent[gateway.NodeCredentialField]
				switch kind {
				case machineBearer:
					if got != bearerMachineID {
						t.Errorf("credentialId = %v, want the machine id %s", got, bearerMachineID)
					}
				case personBearer:
					if has {
						t.Errorf("a person bearer's registerNode carries credentialId %v", got)
					}
				}
				t.Logf("%s bearer, %s contract: registerNode input passes the fixture: %v", kind, contract.name, sent)
			})
		}
	}
}

// TestMachineRegisterWithoutCredentialIDFails registers through the fake
// on a machine bearer with the credentialId stripped: the fixture's
// machine rule requires the field, and the fake refuses the call naming
// it and the bearer kind. Its expectation is the fixture's — a
// registerNode.machine.required without credentialId fails this test —
// and the same input with the credentialId the node sends passes.
func TestMachineRegisterWithoutCredentialIDFails(t *testing.T) {
	if req := registerRule(t, machineBearer).Required; !slices.Contains(req, gateway.NodeCredentialField) {
		t.Fatalf("the fixture's registerNode.machine.required %v lacks %s: a machine register without it would be let through", req, gateway.NodeCredentialField)
	}
	_, gw := newBearerFake(t, machineBearer)
	ctx := context.Background()
	cfg := &Config{Name: "mini", Slug: "mini", Capacity: Capacity{Min: 1, Max: 1}}
	const nodeID = "5b0e2a52-0d7c-4a8e-9a1f-1d2c3b4a5f60"

	stripped := RegisterInput(cfg, nodeID, "", "", cfg.OwnerKind(), "", "v1.2.3")
	_, err := gw.RegisterNode(ctx, stripped)
	if err == nil {
		t.Fatal("a machine register without credentialId was accepted")
	}
	if !gateway.IsRefusal(err) {
		t.Fatalf("want a gateway refusal, got %v", err)
	}
	for _, want := range []string{gateway.NodeCredentialField, "machine bearer"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
	t.Logf("machine register without credentialId refused: %v", err)

	if _, err := gw.RegisterNode(ctx, RegisterInput(cfg, nodeID, "", "", cfg.OwnerKind(), bearerCredentialID(machineBearer), "v1.2.3")); err != nil {
		t.Fatalf("a machine register with credentialId was refused: %v", err)
	}
}
