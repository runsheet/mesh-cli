package nodev1

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// releaseRecord is the lease's release AccessRecord, and how many.
func releaseRecords(t *testing.T, g *rig) []meshapi.AccessRecord {
	t.Helper()
	var out []meshapi.AccessRecord
	for _, c := range g.fake.Calls("access") {
		var req meshapi.AccessRequest
		json.Unmarshal(c.Body, &req)
		for _, r := range req.Records {
			if r.Vendor == "latchkey" && r.Service == "release" {
				out = append(out, r)
			}
		}
	}
	return out
}

// watchRelease has the fake Latchkey note, at each release, whether Mesh
// had already heard the lease end (it revokes the session then).
func watchRelease(g *rig, job string, endedFirst *bool) {
	g.latchkey.mu.Lock()
	defer g.latchkey.mu.Unlock()
	g.latchkey.onRelease = func() {
		if lease := g.fake.LeaseOf(job); lease != "" && g.fake.Ended(lease) != "" {
			*endedFirst = true
		}
	}
}

// A lease whose access minted children releases them at Latchkey as it
// ends, on every path — finished, handed back before its session ran,
// cancelled by Mesh, the node stopping — before Mesh hears the end, and
// records released on the lease (mesh-specs gap 28 item 6).
func TestTheLeasesEndReleasesItsMintedChildren(t *testing.T) {
	block := func(ctx context.Context, _ sandbox.AgentSpec) (sandbox.Exit, error) {
		<-ctx.Done()
		return sandbox.Exit{Code: 143}, nil
	}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, g *rig, sb *sandbox.Fake)
		end   func(t *testing.T, g *rig, sb *sandbox.Fake, job string) (context.Context, func())
		route string
	}{
		{"finished", nil, nil, "finish"},
		{"handed back before its session", func(t *testing.T, g *rig, sb *sandbox.Fake) {
			// a Secret, asked before the child, is refused: the release
			// still goes, and Latchkey has nothing to revoke
			g.latchkey.refuseCarried = map[string]carriedRefusal{"sec_query": {http.StatusForbidden, "purpose_not_granted"}}
		}, nil, "hand-back"},
		{"cancelled by Mesh", func(t *testing.T, g *rig, sb *sandbox.Fake) {
			sb.Agent = block
			g.rt.HeartbeatEvery = time.Hour
		}, func(t *testing.T, g *rig, sb *sandbox.Fake, job string) (context.Context, func()) {
			return context.Background(), func() {
				waitFor(t, "the session", func() bool { return len(sb.AgentSpecs()) > 0 && g.fake.Streams() > 0 })
				g.fake.StreamCancel(g.fake.LeaseOf(job), "superseded", "replaced")
			}
		}, "hand-back"},
		{"the node stopping", func(t *testing.T, g *rig, sb *sandbox.Fake) {
			sb.Agent = block
		}, func(t *testing.T, g *rig, sb *sandbox.Fake, job string) (context.Context, func()) {
			ctx, cancel := context.WithCancel(context.Background())
			return ctx, func() {
				waitFor(t, "the session", func() bool { return len(sb.AgentSpecs()) > 0 })
				cancel()
			}
		}, "hand-back"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, sb, _ := podRig(t)
			injectHostRig(t, g)
			carryAll(g.latchkey)
			sb.Agent = hostAgent(t, filepath.Join(t.TempDir(), "claude"), "")
			if tc.setup != nil {
				tc.setup(t, g, sb)
			}
			job := g.offer(t, injectBrief)
			var endedFirst bool
			watchRelease(g, job, &endedFirst)
			ctx := context.Background()
			if tc.end != nil {
				var act func()
				ctx, act = tc.end(t, g, sb, job)
				go act()
			}
			done := make(chan error, 1)
			go func() { done <- g.rt.Run(ctx) }()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("the runtime did not stop")
			}
			if n := len(g.fake.Calls(tc.route)); n != 1 {
				t.Fatalf("%d %s calls; hand-backs %+v fails %+v", n, tc.route, g.fake.Calls("hand-back"), g.fake.Calls("fail"))
			}
			g.latchkey.mu.Lock()
			releases := g.latchkey.releases
			g.latchkey.mu.Unlock()
			if releases != 1 || endedFirst {
				t.Fatalf("%d releases; Mesh heard the end first: %v", releases, endedFirst)
			}
			recs := releaseRecords(t, g)
			want := "released 1"
			if tc.name == "handed back before its session" {
				want = "released 0"
			}
			if len(recs) != 1 || recs[0].Outcome != "ok" || recs[0].Reason != want {
				t.Fatalf("the release recorded %+v", recs)
			}
		})
	}
}

// A release Latchkey fails is recorded release_failed and the lease ends
// as it would have; a lease whose access minted nothing calls no release.
func TestAFailedReleaseIsRecordedAndHoldsNothingOpen(t *testing.T) {
	g, sb, _ := podRig(t)
	injectHostRig(t, g)
	carryAll(g.latchkey)
	g.latchkey.refuseRelease = http.StatusServiceUnavailable
	sb.Agent = hostAgent(t, filepath.Join(t.TempDir(), "claude"), "")
	g.offer(t, injectBrief)
	run(t, g, context.Background())
	if n := len(g.fake.Calls("finish")); n != 1 {
		t.Fatalf("%d finishes", n)
	}
	recs := releaseRecords(t, g)
	if len(recs) != 1 || recs[0].Outcome != "failed" || !strings.HasPrefix(recs[0].Reason, "release_failed: ") {
		t.Fatalf("the release recorded %+v", recs)
	}

	g2, sb2, _ := podRig(t)
	injectHostRig(t, g2)
	carryAll(g2.latchkey)
	sb2.Agent = sb.Agent
	g2.offer(t, func(b *meshapi.Brief) { injectBrief(b); b.Access.Mint = nil })
	run(t, g2, context.Background())
	if g2.latchkey.releases != 0 || len(releaseRecords(t, g2)) != 0 {
		t.Fatalf("a lease that minted nothing released %d", g2.latchkey.releases)
	}
}
