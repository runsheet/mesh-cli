package gateway

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/google/uuid"
)

func TestNodeIDMatchesPlatformDerivation(t *testing.T) {
	org := uuid.MustParse("0f3b8a1e-2c4d-4e6f-8a9b-0c1d2e3f4a5b")
	ns := uuid.MustParse("7b3c1d2e-4f5a-4b6c-8d7e-9f0a1b2c3d4e")
	want := uuid.NewSHA1(ns, []byte("node:"+org.String()+":chris-mbp"))
	if got := NodeID(org, "chris-mbp"); got != want {
		t.Fatalf("NodeID = %s, want %s", got, want)
	}
	if NodeID(org, "chris-mbp") != NodeID(org, "chris-mbp") {
		t.Fatal("NodeID must be deterministic")
	}
	if NodeID(org, "other") == want {
		t.Fatal("a different slug must derive a different id")
	}
	if GoalID(org, "chris-mbp") == want {
		t.Fatal("goal and node prefixes must not collide")
	}
	if got, want := GoalID(org, "ship-it"), uuid.NewSHA1(ns, []byte("goal:"+org.String()+":ship-it")); got != want {
		t.Fatalf("GoalID = %s, want %s", got, want)
	}
	if got, want := ProjectID(org, "mesh"), uuid.NewSHA1(ns, []byte("project:"+org.String()+":mesh")); got != want {
		t.Fatalf("ProjectID = %s, want %s", got, want)
	}
}

// NodeOwnerID is the platform's own formula (platform/ids/node.go,
// NodeOwnerID): specs/contracts/registerNode.json's ownerNodeId object
// carries a worked example — never invented here — and this is the
// contract the fixture and the platform are tested against (ADR-015,
// 2026-10-03).
func TestNodeOwnerIDMatchesFixtureExample(t *testing.T) {
	raw, err := os.ReadFile("../../specs/contracts/registerNode.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		OwnerNodeID struct {
			Namespace string `json:"namespace"`
			Kind      string `json:"kind"`
			Owner     string `json:"owner"`
			Slug      string `json:"slug"`
			ID        string `json:"id"`
		} `json:"ownerNodeId"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if fx.OwnerNodeID.ID == "" {
		t.Fatal("registerNode.json: no ownerNodeId")
	}
	if fx.OwnerNodeID.Namespace != idNamespace.String() {
		t.Fatalf("ownerNodeId.namespace = %q, want the client's own %s", fx.OwnerNodeID.Namespace, idNamespace)
	}
	owner := uuid.MustParse(fx.OwnerNodeID.Owner)
	got := NodeOwnerID(fx.OwnerNodeID.Kind, owner, fx.OwnerNodeID.Slug)
	if got.String() != fx.OwnerNodeID.ID {
		t.Fatalf("NodeOwnerID(%q, %s, %q) = %s, want the fixture's %s", fx.OwnerNodeID.Kind, owner, fx.OwnerNodeID.Slug, got, fx.OwnerNodeID.ID)
	}
	// an empty kind reads as person
	if NodeOwnerID("", owner, fx.OwnerNodeID.Slug) != NodeOwnerID("person", owner, fx.OwnerNodeID.Slug) {
		t.Fatal("NodeOwnerID(\"\", ...) must read as person")
	}
}

func TestPriorityRank(t *testing.T) {
	for p, want := range map[string]int{"urgent": 0, "high": 1, "normal": 2, "": 2, "bogus": 2, "low": 3} {
		if got := PriorityRank(p); got != want {
			t.Errorf("PriorityRank(%q) = %d, want %d", p, got, want)
		}
	}
}
