package node

import (
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gitapply"
)

func TestApplyIdentity(t *testing.T) {
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	agent := Identity{Name: "mesh-builder", Email: "builder@example.com"}
	fixed := time.Date(2026, 10, 29, 1, 2, 3, 0, time.UTC)
	for _, c := range []struct {
		name string
		as   *gateway.JobApplyAs
		want gitapply.Identity
		bad  bool
	}{
		{"none: the session's own, now", nil, gitapply.Identity{Name: agent.Name, Email: agent.Email, When: now}, false},
		{"the brief's, its date", &gateway.JobApplyAs{Name: "Runsheet", Email: "r@example.com", Date: "2026-10-29T01:02:03Z"}, gitapply.Identity{Name: "Runsheet", Email: "r@example.com", When: fixed}, false},
		{"a date alone", &gateway.JobApplyAs{Date: "2026-10-29T01:02:03Z"}, gitapply.Identity{Name: agent.Name, Email: agent.Email, When: fixed}, false},
		{"a date that is not RFC 3339", &gateway.JobApplyAs{Date: "yesterday"}, gitapply.Identity{}, true},
	} {
		got, err := applyIdentity(c.as, agent, now)
		if (err != nil) != c.bad || !got.When.Equal(c.want.When) || got.Name != c.want.Name || got.Email != c.want.Email {
			t.Errorf("%s: %+v %v, want %+v", c.name, got, err, c.want)
		}
	}
}

func TestApplyMode(t *testing.T) {
	for mode, want := range map[string]gitapply.Mode{"": gitapply.Merge, "linear": gitapply.Linear, "merge": gitapply.Merge} {
		if got, err := applyMode(gateway.JobCode{ApplyMode: mode}, gitapply.Merge); err != nil || got != want {
			t.Errorf("%q: %v %v, want %v", mode, got, err, want)
		}
	}
	if _, err := applyMode(gateway.JobCode{ApplyMode: "octopus"}, gitapply.Linear); err == nil {
		t.Error("an unknown mode was taken")
	}
}
