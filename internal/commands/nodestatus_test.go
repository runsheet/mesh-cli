package commands

import (
	"context"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/version"
)

const statusNode = "eeeeeeee-7777-4777-8777-eeeeeeeeeeee"

// `mesh node status` heartbeats like the loop: the tag as version and
// the commit beside it when the gateway's HeartbeatNodeInput takes it;
// a schema without the field gets no key, and the heartbeat still lands.
func TestStatusHeartbeatCarriesCommit(t *testing.T) {
	oldV, oldC := version.Version, version.Commit
	version.Version, version.Commit = "v0.2.188", "0895d40"
	t.Cleanup(func() { version.Version, version.Commit = oldV, oldC })

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
			if err := gw.RegisterNode(ctx, gateway.RegisterNodeInput{NodeID: statusNode, OrgID: "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa", Name: "mini", Slug: "mini", Capacity: 1, Version: "v1"}); err != nil {
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
