package gateway

import (
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
