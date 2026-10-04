package commands

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

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

// `mesh node status` names `mesh mcp`'s personal node when
// ~/.mesh/personal-node.json holds one (ADR-023, mesh mcp).
func TestNodeStatusNamesPersonalNode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if line := personalNodeStatusLine(); line != "" {
		t.Fatalf("personalNodeStatusLine with no file = %q, want empty", line)
	}
	pn := node.PersonalNode{ID: "node-123", Name: "mini (Claude Code)"}
	if err := node.SavePersonalNode(pn); err != nil {
		t.Fatal(err)
	}
	line := personalNodeStatusLine()
	if !strings.Contains(line, "personal node:") || !strings.Contains(line, pn.Name) || !strings.Contains(line, pn.ID) {
		t.Fatalf("personalNodeStatusLine = %q", line)
	}
}

const statusOwnerID = "18a8b766-18f3-5e73-b4cf-efb91c01946a"

// `mesh node status` names node.yaml's node_id and how it was arrived
// at — derived, adopted from the platform, or recorded (a node_id from
// before node_id_source existed) — after the owner line, whatever the
// heartbeat answered.
func TestOwnedStatusLineNodeID(t *testing.T) {
	answer := &gateway.NodeAnswer{
		NodeID: statusOwnerID,
		Owner:  gateway.NodeOwner{Kind: "person", Name: "Chris"},
		Scopes: []gateway.ScopeAnswer{{OrgID: "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa", Login: "runsheet"}},
	}
	for _, tc := range []struct {
		name   string
		source string
		want   string
	}{
		{"derived", node.IDDerived, "node id: " + statusOwnerID + " (derived)"},
		{"adopted", node.IDAdopted, "node id: " + statusOwnerID + " (adopted from the platform)"},
		{"recorded", "", "node id: " + statusOwnerID + " (recorded)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &node.Config{NodeID: statusOwnerID, NodeIDSource: tc.source}
			out := node.Outside{Owned: true, Answer: answer}
			line := ownedStatusLine(cfg, out)
			lines := strings.Split(line, "\n")
			if lines[len(lines)-1] != tc.want {
				t.Errorf("status line %q, want it to end with %q", line, tc.want)
			}
		})
	}
}

// A per-org node (Outside.Owned false) prints nothing, including no
// node id line, whatever node.yaml records — the per-org path has no
// single node_id to show.
func TestOwnedStatusLineNodeIDPerOrg(t *testing.T) {
	cfg := &node.Config{NodeID: statusOwnerID, NodeIDSource: node.IDDerived}
	line := ownedStatusLine(cfg, node.Outside{Owned: false})
	if line != "" {
		t.Errorf("status line %q, want empty on the per-org path", line)
	}
}

// `mesh node status` prints the kinds node.yaml's `takes:` claims right
// after "This machine:" (ADR-025, item 6): every kind when left out, the
// list when it names some, the cap beside a kind that has one.
func TestTakesStatusLine(t *testing.T) {
	for _, tc := range []struct {
		name  string
		takes node.Takes
		want  string
	}{
		{"a list", node.Takes{{Kind: "evaluate"}}, "Takes: evaluate"},
		{"a capped mapping", node.Takes{{Kind: "evaluate", Cap: 2}}, "Takes: evaluate (at most 2)"},
		{"left out", nil, "Takes: every kind"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &node.Config{Takes: tc.takes}
			if got := takesStatusLine(cfg); got != tc.want {
				t.Errorf("takesStatusLine = %q, want %q", got, tc.want)
			}
		})
	}
}

// Against a real client and the fake platform: a heartbeat under the
// owner contract answers normally, and the status line still carries
// the node id line node.yaml records, derived or adopted.
func TestStatusLineNodeIDAgainstFake(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		want   string
	}{
		{"derived", node.IDDerived, "node id: " + statusOwnerID + " (derived)"},
		{"adopted", node.IDAdopted, "node id: " + statusOwnerID + " (adopted from the platform)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := gatewaytest.New("rs_test")
			t.Cleanup(f.Close)
			f.Answer = &gateway.NodeAnswer{
				NodeID: statusOwnerID,
				Owner:  gateway.NodeOwner{Kind: "person", Name: "Chris"},
				Scopes: []gateway.ScopeAnswer{{OrgID: "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa", Login: "runsheet"}},
			}
			gw := gateway.New(f.URL(), "rs_test")
			ctx := context.Background()
			if _, err := gw.RegisterNode(ctx, gateway.RegisterNodeInput{NodeID: statusOwnerID, OrgID: "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa", OwnerID: statusOwnerID, Name: "mini", Slug: "runsheet-mini", Owner: "person", Capacity: 1, Version: "v1"}); err != nil {
				t.Fatal(err)
			}
			cfg := &node.Config{NodeID: statusOwnerID, NodeIDSource: tc.source, Contract: node.ContractOwner, Slug: "runsheet-mini"}
			out, err := node.HeartbeatOutside(ctx, gw, cfg, "v1", gateway.HeartbeatExtras{},
				func(node.Scope, error) error { t.Fatal("a per-org heartbeat on the owner contract"); return nil })
			if err != nil {
				t.Fatal(err)
			}
			line := ownedStatusLine(cfg, out)
			if !strings.Contains(line, tc.want) {
				t.Errorf("status line %q, want it to contain %q", line, tc.want)
			}
		})
	}
}

// `mesh node status` prints a Held goals table — ORG, GOAL, KIND,
// SINCE, UNTIL and WHY — for the goals this node holds off claiming
// for (goal a-refused-plan-is-released-not-replanned, item 2).
func TestHeldGoalsSectionTable(t *testing.T) {
	since := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	until := since.Add(5 * time.Hour)
	held := []node.HeldGoal{
		{GoalID: "g1", Slug: "loom", Org: "acme", Kind: "plan", Reason: "refused: same clause twice", Since: since},
		{GoalID: "g2", Slug: "steady", Org: "acme", Kind: "item", Reason: "budget paused", Since: since, Until: until},
	}
	var out bytes.Buffer
	heldGoalsSection(&out, held)
	got := out.String()
	for _, want := range []string{"Held goals", "ORG", "GOAL", "KIND", "SINCE", "UNTIL", "WHY",
		"acme", "loom", "plan", "refused: same clause twice",
		"steady", "item", "budget paused", since.Format(time.RFC3339), until.Format(time.RFC3339)} {
		if !strings.Contains(got, want) {
			t.Fatalf("Held goals section missing %q:\n%s", want, got)
		}
	}
}

// With nothing held, the section is just "Held goals: none."
func TestHeldGoalsSectionNone(t *testing.T) {
	var out bytes.Buffer
	heldGoalsSection(&out, nil)
	if got := strings.TrimSpace(out.String()); got != "Held goals: none." {
		t.Fatalf("Held goals section = %q, want %q", got, "Held goals: none.")
	}
}

// localHeld only reports held goals, and only true, while a node runs
// here — the same pid rule as localScaling.
func TestLocalHeldPidRule(t *testing.T) {
	state := node.RunState{Dir: t.TempDir()}
	if _, ok := localHeld(state); ok {
		t.Fatal("localHeld with no node running")
	}
	pid := os.Getpid()
	if err := state.Started(pid); err != nil {
		t.Fatal(err)
	}
	if held, ok := localHeld(state); !ok || len(held) != 0 {
		t.Fatalf("localHeld with a node running but nothing held = %+v, %v, want [], true", held, ok)
	}
	if err := state.WriteHeld([]node.HeldGoal{{GoalID: "g1", Kind: "plan"}}); err != nil {
		t.Fatal(err)
	}
	if held, ok := localHeld(state); !ok || len(held) != 1 {
		t.Fatalf("localHeld = %+v, %v, want one entry", held, ok)
	}
	state.Stopped(pid)
	if _, ok := localHeld(state); ok {
		t.Fatal("localHeld after the node stopped")
	}
}

// `mesh node status`'s shares table: the header and one row per org as
// node.shares left them, a failed ready read as "-", and the line for
// no node.shares at all (ADR-023, Reporting).
func TestSharesLines(t *testing.T) {
	got := sharesLines([]node.HoldingOrg{
		{Org: "runsheet", OrgID: "org-r", Floor: 2, Held: 1, Ready: 3},
		{Org: "wardroomid", OrgID: "org-w", Floor: 2, Held: 2, Ready: 0},
		{Org: "quiet", OrgID: "org-q", Floor: 0, Held: 0, Ready: -1},
	})
	want := []string{
		"ORG         FLOOR  HELD  READY",
		"runsheet    2      1     3",
		"wardroomid  2      2     0",
		"quiet       0      0     -",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("sharesLines =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	state := node.RunState{Dir: t.TempDir()}
	rows, _ := state.Shares()
	if got := sharesLines(rows); len(got) != 1 || got[0] != "shares: none recorded" {
		t.Fatalf("absent node.shares = %q", got)
	}
}

// `mesh node status` prints each account's clock standing — week use,
// target line, distance and push — in label order, from node.standing;
// nothing with no node.standing, or with no node running at all (goal
// every-subscription-lands-full-at-its-reset, item 6).
func TestNodeStatusShowsStanding(t *testing.T) {
	state := node.RunState{Dir: t.TempDir()}
	if _, ok := localStanding(state); ok {
		t.Fatal("a node that has not ticked shows standing")
	}
	pid := os.Getpid()
	if err := state.Started(pid); err != nil {
		t.Fatal(err)
	}
	if rows, ok := localStanding(state); !ok || len(rows) != 0 {
		t.Fatalf("localStanding with a node running but no node.standing = %+v, %v, want [], true", rows, ok)
	}
	rows := []node.AccountStanding{
		{Label: "max-a", Kind: node.AccountSubscription,
			SevenDay: node.WindowStanding{Utilization: 0.10, Target: 0.45, Distance: -0.35}, Distance: -0.35},
		{Label: "max-b", Kind: node.AccountSubscription,
			SevenDay: node.WindowStanding{Utilization: 0.70, Target: 0.45, Distance: 0.25}, Distance: 0.25, Push: true},
	}
	if err := state.WriteStanding(rows); err != nil {
		t.Fatal(err)
	}
	got, ok := localStanding(state)
	if !ok {
		t.Fatal("localStanding: not ok with a node running and node.standing written")
	}
	lines := strings.Join(standingLines(got), "\n")
	for _, want := range []string{
		"account max-a: week 10% used, target 45% (35% behind)",
		"account max-b: week 70% used, target 45% (25% ahead)",
	} {
		if !strings.Contains(lines, want) {
			t.Fatalf("standing lines missing %q:\n%s", want, lines)
		}
	}
	for _, line := range strings.Split(lines, "\n") {
		if strings.HasPrefix(line, "account max-b:") && !strings.Contains(line, "push") {
			t.Fatalf("max-b's line has no push: %q", line)
		}
		if strings.HasPrefix(line, "account max-a:") && strings.Contains(line, "push") {
			t.Fatalf("max-a's line should have no push: %q", line)
		}
	}

	if err := state.WriteStanding(nil); err != nil {
		t.Fatal(err)
	}
	if rows, ok := localStanding(state); !ok || len(rows) != 0 {
		t.Fatalf("localStanding after an empty WriteStanding = %+v, %v, want [], true", rows, ok)
	}

	state.Stopped(pid)
	if _, ok := localStanding(state); ok {
		t.Fatal("localStanding after the node stopped")
	}
}
