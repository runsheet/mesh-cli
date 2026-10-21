package node

import (
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// The stop list (ADR 066, decision 8): a registered session is reported
// on the heartbeat; the answer's stop ends it with the platform's
// reason, which Is harness.ErrStopSession so its unit is killed, not
// detached; a stop for a session the node no longer runs is nothing;
// done unregisters, and none left reports an empty list, never nil.
func TestStoppableSessions(t *testing.T) {
	var logs strings.Builder
	l := &Loop{Log: log.New(&logs, "", 0)}
	if got := l.Sessions(); got == nil || len(got) != 0 {
		t.Fatalf("no sessions: %#v, want an empty list", got)
	}
	bctx, buildDone := l.stoppableCtx(context.Background(), "build", "item-b")
	ectx, evalDone := l.stoppableCtx(context.Background(), "evaluate", "item-a")
	defer buildDone()
	defer evalDone()
	got := l.Sessions()
	if len(got) != 2 || got[0].ItemID != "item-a" || got[0].Kind != "evaluate" || got[1].ItemID != "item-b" || got[1].Since == "" {
		t.Fatalf("sessions: %+v", got)
	}

	l.applyStops([]gateway.SessionStop{{ItemID: "item-b", Kind: "build", Reason: "connection revoked"}, {ItemID: "item-gone", Kind: "build", Reason: "item merged"}})
	why, stopped := StoppedByPlatform(bctx)
	if !stopped || why != "connection revoked" || !harness.StopRequested(bctx) {
		t.Fatalf("the build: stopped %v %q, stop requested %v", stopped, why, harness.StopRequested(bctx))
	}
	if !errors.Is(context.Cause(bctx), harness.ErrStopSession) {
		t.Fatalf("cause %v is not ErrStopSession", context.Cause(bctx))
	}
	if ectx.Err() != nil {
		t.Fatal("the evaluation was stopped too")
	}
	if !strings.Contains(logs.String(), "item item-b: build session stopped by the platform (connection revoked)") || strings.Contains(logs.String(), "item-gone") {
		t.Fatalf("log:\n%s", logs.String())
	}
	if got := l.Sessions(); len(got) != 1 || got[0].ItemID != "item-a" {
		t.Fatalf("after the stop: %+v", got)
	}
	evalDone()
	if got := l.Sessions(); len(got) != 0 {
		t.Fatalf("after done: %+v", got)
	}
	if _, stopped := StoppedByPlatform(ectx); stopped {
		t.Fatal("a session ended by done reads as stopped by the platform")
	}
}
