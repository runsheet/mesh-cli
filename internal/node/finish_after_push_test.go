package node

import (
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

// pushOrderWS is the fake workspace whose Push notes how many
// finishWorkItem calls the platform's fake had seen when it ran, and
// fails with err when set.
type pushOrderWS struct {
	*fakeWS
	f              *gatewaytest.Fake
	err            error
	finishesAtPush []int
}

func (w *pushOrderWS) Push(ctx context.Context, dir, branch string) (string, error) {
	w.finishesAtPush = append(w.finishesAtPush, len(w.f.CallsNamed("finishWorkItem")))
	if w.err != nil {
		return "", w.err
	}
	return w.fakeWS.Push(ctx, dir, branch)
}

// runOrdered runs item1 to its end on a session that called mesh_finish,
// pushing through a pushOrderWS that fails with pushErr when set.
func runOrdered(t *testing.T, pushErr error) (*gatewaytest.Fake, *pushOrderWS, error, string) {
	t.Helper()
	f := seed(t)
	r, ws, item, _ := runnerOnFake(t, f, &resultHarness{result: harness.Result{Success: true, Text: "Built it.", WorkCalls: map[string]map[string]any{
		"mesh_finish": {"item_id": item1, "branch": "mesh/g-1", "head_sha": "0000000"},
	}}}, nil)
	ow := &pushOrderWS{fakeWS: ws, f: f, err: pushErr}
	r.WS = ow
	logs := &syncBuf{}
	r.Log = log.New(logs, "", 0)
	err := r.RunItem(context.Background(), item)
	return f, ow, err, logs.String()
}

// The session's mesh_finish records nothing: the node pushes first, and
// only then hands in exactly one finishWorkItem, on the head it pushed
// (ADR-023 Decision 2; goal a-build-pushes-before-it-finishes).
func TestBuildFinishIsRecordedOnlyAfterThePush(t *testing.T) {
	f, ws, err, logs := runOrdered(t, nil)
	if err != nil {
		t.Fatalf("RunItem: %v\n%s", err, logs)
	}
	if len(ws.finishesAtPush) != 1 || ws.finishesAtPush[0] != 0 {
		t.Fatalf("finishWorkItem calls seen at the push = %v, want one push with none before it", ws.finishesAtPush)
	}
	finishes := f.CallsNamed("finishWorkItem")
	if len(finishes) != 1 {
		t.Fatalf("finishWorkItem called %d times, want exactly one", len(finishes))
	}
	if got := finishes[0].Input["headSha"]; got != "abc123def456" {
		t.Fatalf("finishWorkItem headSha = %v, want the pushed abc123def456 (not the session's 0000000)", got)
	}
	if n := len(f.CallsNamed("failWorkItem")); n != 0 {
		t.Fatalf("failWorkItem called %d times", n)
	}
}

// A push that fails is the build's failure, named: no finish of either
// kind is sent and the branch is left for a retry.
func TestBuildPushRefusedFailsTheItemWithNoFinish(t *testing.T) {
	refused := errors.New("git push -u --force-with-lease origin mesh/g-1: remote: Write access to repository not granted")
	f, ws, err, logs := runOrdered(t, refused)
	if err == nil {
		t.Fatalf("RunItem: want the push's failure\n%s", logs)
	}
	if len(ws.finishesAtPush) != 1 {
		t.Fatalf("pushes = %d, want 1", len(ws.finishesAtPush))
	}
	// FinishWorkItem and FinishWorkItemAlreadyDone are both finishWorkItem
	if c := f.CallsNamed("finishWorkItem"); len(c) != 0 {
		t.Fatalf("finishWorkItem sent after a refused push: %v", c[0].Input)
	}
	fails := f.CallsNamed("failWorkItem")
	if len(fails) != 1 {
		t.Fatalf("failWorkItem called %d times, want 1", len(fails))
	}
	msg, _ := fails[0].Input["error"].(string)
	if !strings.HasPrefix(msg, "push mesh/") || !strings.Contains(msg, "Write access to repository not granted") {
		t.Fatalf("failWorkItem error = %q, want it to name the push", msg)
	}
}
