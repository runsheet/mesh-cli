package commands

import (
	"context"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/node"
	"projectmesh.io/mesh-cli/internal/version"
)

const statusNode = "eeeeeeee-7777-4777-8777-eeeeeeeeeeee"

// `mesh node status` heartbeats like the loop: the tag as version and
// the commit beside it when the gateway's HeartbeatNodeInput takes it;
// a schema without the field gets no key, and the heartbeat still lands.
func TestStatusHeartbeatCarriesCommit(t *testing.T) {
	oldV, oldC := version.Version, version.Sha
	version.Version, version.Sha = "v0.2.188", "0895d40"
	t.Cleanup(func() { version.Version, version.Sha = oldV, oldC })

	for _, tc := range []struct {
		name    string
		unknown bool
		want    any
	}{
		{"schema takes commit", false, "0895d40"},
		{"schema lacks commit", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := gatewaytest.New("rs_test")
			t.Cleanup(f.Close)
			if tc.unknown {
				f.UnknownFields["commit"] = true
			}
			gw := gateway.New(f.URL(), "rs_test")
			ctx := context.Background()
			if _, err := gw.RegisterNode(ctx, gateway.RegisterNodeInput{NodeID: statusNode, OrgID: "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa", Name: "mini", Slug: "mini", Capacity: 1, Version: "v1"}); err != nil {
				t.Fatal(err)
			}
			if err := statusHeartbeat(ctx, gw, statusNode); err != nil {
				t.Fatal(err)
			}
			hb := f.CallsNamed("heartbeatNode")
			if len(hb) != 1 {
				t.Fatalf("heartbeats: %+v", hb)
			}
			if hb[0].Input["version"] != "v0.2.188" {
				t.Fatalf("version = %#v", hb[0].Input["version"])
			}
			got, ok := hb[0].Input["commit"]
			if tc.want == nil && ok {
				t.Fatalf("sent commit the schema lacks: %#v", got)
			}
			if tc.want != nil && got != tc.want {
				t.Fatalf("commit = %#v, want %#v", got, tc.want)
			}
		})
	}
}

const statusOwnerID = "18a8b766-18f3-5e73-b4cf-efb91c01946a"

// `mesh node status` names node.yaml's node_id and how it was arrived
// at — derived, adopted from the platform, or recorded (a node_id from
// before node_id_source existed) — after the owner line, whatever the
// heartbeat answered.
func TestOwnedStatusLineNodeID(t *testing.T) {
	answer := &gateway.NodeAnswer{
		NodeID: statusOwnerID,
		Owner:  gateway.NodeOwner{Kind: "person", Name: "Chris"},
		Scopes: []gateway.ScopeAnswer{{OrgID: "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa", Login: "runsheet"}},
	}
	for _, tc := range []struct {
		name   string
		source string
		want   string
	}{
		{"derived", node.IDDerived, "node id: " + statusOwnerID + " (derived)"},
		{"adopted", node.IDAdopted, "node id: " + statusOwnerID + " (adopted from the platform)"},
		{"recorded", "", "node id: " + statusOwnerID + " (recorded)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &node.Config{NodeID: statusOwnerID, NodeIDSource: tc.source}
			out := node.Outside{Owned: true, Answer: answer}
			line := ownedStatusLine(cfg, out)
			lines := strings.Split(line, "\n")
			if lines[len(lines)-1] != tc.want {
				t.Errorf("status line %q, want it to end with %q", line, tc.want)
			}
		})
	}
}

// A per-org node (Outside.Owned false) prints nothing, including no
// node id line, whatever node.yaml records — the per-org path has no
// single node_id to show.
func TestOwnedStatusLineNodeIDPerOrg(t *testing.T) {
	cfg := &node.Config{NodeID: statusOwnerID, NodeIDSource: node.IDDerived}
	line := ownedStatusLine(cfg, node.Outside{Owned: false})
	if line != "" {
		t.Errorf("status line %q, want empty on the per-org path", line)
	}
}

// Against a real client and the fake platform: a heartbeat under the
// owner contract answers normally, and the status line still carries
// the node id line node.yaml records, derived or adopted.
func TestStatusLineNodeIDAgainstFake(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		want   string
	}{
		{"derived", node.IDDerived, "node id: " + statusOwnerID + " (derived)"},
		{"adopted", node.IDAdopted, "node id: " + statusOwnerID + " (adopted from the platform)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := gatewaytest.New("rs_test")
			t.Cleanup(f.Close)
			f.Answer = &gateway.NodeAnswer{
				NodeID: statusOwnerID,
				Owner:  gateway.NodeOwner{Kind: "person", Name: "Chris"},
				Scopes: []gateway.ScopeAnswer{{OrgID: "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa", Login: "runsheet"}},
			}
			gw := gateway.New(f.URL(), "rs_test")
			ctx := context.Background()
			if _, err := gw.RegisterNode(ctx, gateway.RegisterNodeInput{NodeID: statusOwnerID, OrgID: "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa", OwnerID: statusOwnerID, Name: "mini", Slug: "runsheet-mini", Owner: "person", Capacity: 1, Version: "v1"}); err != nil {
				t.Fatal(err)
			}
			cfg := &node.Config{NodeID: statusOwnerID, NodeIDSource: tc.source, Contract: node.ContractOwner, Slug: "runsheet-mini"}
			out, err := node.HeartbeatOutside(ctx, gw, cfg, "v1", gateway.HeartbeatExtras{},
				func(node.Scope, error) error { t.Fatal("a per-org heartbeat on the owner contract"); return nil })
			if err != nil {
				t.Fatal(err)
			}
			line := ownedStatusLine(cfg, out)
			if !strings.Contains(line, tc.want) {
				t.Errorf("status line %q, want it to contain %q", line, tc.want)
			}
		})
	}
}
