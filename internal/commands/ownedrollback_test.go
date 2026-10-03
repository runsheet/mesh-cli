package commands

import (
	"bytes"
	"context"
	"log"
	"slices"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/node"
)

// ownedPlatform answers the probe, the heartbeat (nil: the platform
// named no orgs) and the person's orgs, recording each heartbeat.
type ownedPlatform struct {
	beats  []string
	errs   []string
	myOrgs int
}

func (p *ownedPlatform) TakesOwner(context.Context) bool { return true }
func (p *ownedPlatform) HeartbeatNode(_ context.Context, nodeID, _ string, _ int, extras ...gateway.HeartbeatExtras) (*gateway.NodeAnswer, error) {
	p.beats = append(p.beats, nodeID)
	for _, e := range extras {
		p.errs = append(p.errs, e.UpdateError)
	}
	return nil, nil
}
func (p *ownedPlatform) MyOrgs(context.Context) ([]gateway.Org, error) {
	p.myOrgs++
	return []gateway.Org{{ID: "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa", Login: "runsheet"}}, nil
}

const ownedNode = "55555555-5555-4555-8555-555555555555"

// The rollback reporter on a saved owner contract reports once, under
// the one id; a nil answer ends it, never a heartbeat per org.
func TestOwnedRollbackNilAnswerStaysOnTheOneID(t *testing.T) {
	p := &ownedPlatform{}
	var buf bytes.Buffer
	cfg := &node.Config{NodeID: ownedNode, Contract: node.ContractOwner, Slug: "runsheet-mini"}
	reportRollbackVia(context.Background(), p, cfg, node.StartCheck{From: "v0.2.1", Reason: "v0.2.2 did not start"}, log.New(&buf, "", 0))
	if !slices.Equal(p.beats, []string{ownedNode}) || p.myOrgs != 0 {
		t.Errorf("beats %v, MyOrgs %d; want one heartbeat under %s and no orgs read", p.beats, p.myOrgs, ownedNode)
	}
	if !slices.Equal(p.errs, []string{"v0.2.2 did not start"}) {
		t.Errorf("updateError = %v", p.errs)
	}
	if buf.Len() != 0 {
		t.Errorf("logged %q", buf.String())
	}
}

// `mesh node status` on a saved owner contract with a nil answer: one
// heartbeat under the one id, no per-org resolution, and a line that
// says the platform named no orgs rather than a blank.
func TestOwnedStatusNilAnswerStaysOnTheOneID(t *testing.T) {
	p := &ownedPlatform{}
	cfg := &node.Config{NodeID: ownedNode, Contract: node.ContractOwner, Slug: "runsheet-mini"}
	out, err := node.HeartbeatOutside(context.Background(), p, cfg, "v1", gateway.HeartbeatExtras{},
		func(node.Scope, error) error { t.Fatal("a per-org heartbeat on the owner contract"); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.beats, []string{ownedNode}) || p.myOrgs != 0 || len(out.Scopes) != 0 {
		t.Errorf("beats %v, MyOrgs %d, scopes %v; want one heartbeat under %s, no orgs", p.beats, p.myOrgs, out.Scopes.Names(), ownedNode)
	}
	if line := ownedStatusLine(cfg, out); !strings.Contains(line, "answered no orgs") {
		t.Errorf("status line %q", line)
	}
}
