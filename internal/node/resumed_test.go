package node

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// A noted retry turn rides every heartbeat's `resumed` — its item, kind,
// session id and fresh flag — until its done func runs; done is safe to
// call twice.
func TestRetryTurnIsOnTheHeartbeatUntilDone(t *testing.T) {
	f, cfg := claimsFake(t)
	gw := gateway.New(f.URL(), "rs_test")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := gw.RegisterNode(ctx, gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "n", Harnesses: []string{"claude-code"}, Capacity: 1, Version: "test"}); err != nil {
		t.Fatal(err)
	}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Version: "test", Interval: time.Hour, Log: log.New(os.Stderr, "", 0),
		Dispatch: func(context.Context, gateway.WorkItem) {}}

	lastResumed := func() []any {
		hb := f.CallsNamed("heartbeatNode")
		if len(hb) == 0 {
			t.Fatal("no heartbeat")
		}
		resumed, _ := hb[len(hb)-1].Input["resumed"].([]any)
		return resumed
	}

	done := loop.NoteRetryTurn(item1, "build", "sess-build-1", true)
	loop.Tick(ctx)
	resumed := lastResumed()
	if len(resumed) != 1 {
		t.Fatalf("heartbeat resumed = %#v, want one entry", resumed)
	}
	r, ok := resumed[0].(map[string]any)
	if !ok || r["itemId"] != item1 || r["kind"] != "build" || r["sessionId"] != "sess-build-1" || r["fresh"] != true {
		t.Fatalf("entry = %#v", resumed[0])
	}

	done()
	done()
	loop.Tick(ctx)
	hb := f.CallsNamed("heartbeatNode")
	if len(hb) != 2 {
		t.Fatalf("heartbeats = %d, want 2", len(hb))
	}
	if v, has := hb[1].Input["resumed"]; has {
		t.Fatalf("heartbeat after done carries resumed = %#v", v)
	}
	if got := loop.Resumed(); got != nil {
		t.Fatalf("Resumed after done = %#v", got)
	}
}
