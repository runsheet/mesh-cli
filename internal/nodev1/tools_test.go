package nodev1

import (
	"testing"

	"projectmesh.io/mesh-cli/internal/escalate"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// A node tool's escalation (a red run_suite, a conflicted apply_onto)
// leaves the step in the lease's mailbox when the kind escalates on its
// trigger and answers the model the session moves to; a trigger the
// kind does not name posts nothing (PR 2.11).
func TestEscalateOnPostsToTheMailbox(t *testing.T) {
	ladder := escalate.New(meshapi.ModelPolicy{Default: "haiku", Escalate: []string{"sonnet"}, On: []string{escalate.OnRed}})
	mb := NewMailbox()
	if to := escalateOn(ladder, mb, escalate.OnRed)("run_suite went red: 1 of 2 steps passed"); to != "sonnet" {
		t.Fatalf("red escalated to %q", to)
	}
	select {
	case m := <-mb.C():
		if m.Kind != MsgEscalate || m.On != escalate.OnRed || m.Text != "run_suite went red: 1 of 2 steps passed" {
			t.Fatalf("the mailbox holds %+v", m)
		}
	default:
		t.Fatal("red left nothing in the mailbox")
	}
	if to := escalateOn(ladder, mb, escalate.OnConflict)("apply_onto conflicted"); to != "" {
		t.Fatalf("a trigger the kind does not name escalated to %q", to)
	}
	select {
	case m := <-mb.C():
		t.Fatalf("the mailbox holds %+v", m)
	default:
	}
	if escalateOn(nil, mb, escalate.OnRed) != nil || escalateOn(ladder, nil, escalate.OnRed) != nil {
		t.Fatal("a lease with no ladder or mailbox escalates")
	}
}
