package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
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

// heartbeatAnswerFixture reads specs/contracts/heartbeatNode.json and
// decodes its "answer" into a gateway.NodeAnswer, the way
// internal/node/owner_test.go's answerFixture does for its own package.
func heartbeatAnswerFixture(t *testing.T) *gateway.NodeAnswer {
	t.Helper()
	raw, err := os.ReadFile("../../specs/contracts/heartbeatNode.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Answer gateway.NodeAnswer `json:"answer"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("heartbeatNode.json: %v", err)
	}
	return &fx.Answer
}

// `mesh node status` on a saved owner contract renders the `owned by …`
// line and one table row per org the answer names — decoding
// heartbeatNode.json's answer directly, no httptest, no socket:
// renderOwnedStatus driven with a stub nodesOf. An org without a GitHub
// login (grapevine) is named by its slug, and the row for the node's
// one id carries the `*` mark.
func TestStatusOwnedRendersOwnerAndOneRowPerOrg(t *testing.T) {
	answer := heartbeatAnswerFixture(t)
	var buf bytes.Buffer
	nodesOf := func(orgID string) ([]gateway.Node, error) {
		return []gateway.Node{{ID: answer.NodeID, Name: "mini", Slug: "mini", Status: "ok", Running: 1, Capacity: 4, Version: "v0.2.200", LastSeenAt: "now"}}, nil
	}
	scopes, err := renderOwnedStatus(&buf, answer, nil, nodesOf)
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != len(answer.Scopes) {
		t.Fatalf("scopes = %d, want %d (one per org the answer names)", len(scopes), len(answer.Scopes))
	}
	out := buf.String()
	wantLine := "owned by anythink (enterprise); serving " + strings.Join(scopes.Names(), ", ")
	if !strings.Contains(out, wantLine) {
		t.Fatalf("output %q, want a line containing %q", out, wantLine)
	}
	var rows []string
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.HasPrefix(line, "ORG") || !strings.Contains(line, "mini") {
			continue
		}
		rows = append(rows, line)
	}
	if len(rows) != len(answer.Scopes) {
		t.Fatalf("table rows = %d, want %d (one per scope the fixture answers): %q", len(rows), len(answer.Scopes), out)
	}
	var sawGrapevine, sawMark bool
	for _, r := range rows {
		if strings.HasPrefix(r, "grapevine") {
			sawGrapevine = true
		}
		if strings.Contains(r, "*") {
			sawMark = true
		}
	}
	if !sawGrapevine {
		t.Fatalf("no row named grapevine: %q", out)
	}
	if !sawMark {
		t.Fatalf("no row marked * for the node's one id: %q", out)
	}
}

// An empty `contract` (a node.yaml from before owners) takes the legacy
// path: ownedHeartbeat — what `mesh node status` and `reportRollback`
// call first — never heartbeats under cfg.NodeID, no socket opened.
func TestStatusPerOrgUnchanged(t *testing.T) {
	gw := &outsideStub{}
	cfg := &node.Config{NodeID: "55555555-5555-4555-8555-555555555555", Contract: ""}
	answer, owned, err := ownedHeartbeat(context.Background(), gw, cfg, "v1", gateway.HeartbeatExtras{})
	if err != nil {
		t.Fatal(err)
	}
	if owned || answer != nil {
		t.Fatalf("ownedHeartbeat = (%v, %v), want (nil, false) on an empty contract", answer, owned)
	}
	if len(gw.beats) != 0 {
		t.Fatalf("heartbeats = %v, want none under cfg.NodeID on the per-org path", gw.beats)
	}
}

// outsideStub is a gateway that records every heartbeat it is asked for.
type outsideStub struct{ beats []string }

func (g *outsideStub) HeartbeatNode(_ context.Context, nodeID, _ string, _ int, _ ...gateway.HeartbeatExtras) (*gateway.NodeAnswer, error) {
	g.beats = append(g.beats, nodeID)
	return nil, nil
}
