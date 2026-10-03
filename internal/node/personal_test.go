package node

import (
	"context"
	"fmt"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// Two RegisterPersonal calls register once under the same id: a
// person, kind personal, capacity 1, and the id is saved to
// ~/.mesh/personal-node.json for the second call to reuse (ADR-015's
// owner form, ADR-023 mesh mcp).
func TestPersonalRegistersOnceAndReuses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.TakesNodeKind = true
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()

	first, err := RegisterPersonal(ctx, gw, "v0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := RegisterPersonal(ctx, gw, "v0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.ID == "" {
		t.Fatalf("ids = %q, %q; want the same, non-empty id", first.ID, second.ID)
	}

	calls := f.CallsNamed("registerNode")
	if len(calls) != 2 {
		t.Fatalf("registerNode calls = %d, want 2: %+v", len(calls), calls)
	}
	for i, call := range calls {
		in := call.Input
		if in["aggregateId"] != first.ID {
			t.Fatalf("call %d aggregateId = %v, want %s", i, in["aggregateId"], first.ID)
		}
		if in["ownerKind"] != "person" {
			t.Fatalf("call %d ownerKind = %v, want person", i, in["ownerKind"])
		}
		if in[gateway.NodeKindField] != "personal" {
			t.Fatalf("call %d kind = %v, want personal", i, in[gateway.NodeKindField])
		}
		if fmt.Sprint(in["capacity"]) != "1" {
			t.Fatalf("call %d capacity = %v, want 1", i, in["capacity"])
		}
	}

	saved, err := LoadPersonalNode()
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID != first.ID {
		t.Fatalf("saved personal-node.json id = %q, want %q", saved.ID, first.ID)
	}
}

// A platform whose RegisterNodeInput has no `kind` field gets a
// registerNode input that carries none (TakesNodeKind false, the
// default).
func TestPersonalRegisterWithNoNodeKindFieldSendsNone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	f := gatewaytest.New("rs_test")
	defer f.Close()
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()

	if _, err := RegisterPersonal(ctx, gw, "v0.0.1"); err != nil {
		t.Fatal(err)
	}
	calls := f.CallsNamed("registerNode")
	if len(calls) != 1 {
		t.Fatalf("registerNode calls = %d, want 1: %+v", len(calls), calls)
	}
	if _, ok := calls[0].Input[gateway.NodeKindField]; ok {
		t.Fatalf("registerNode input carried kind on a platform without it: %+v", calls[0].Input)
	}
}
