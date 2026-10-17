//go:build e2e

package node

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/tripline"
	"projectmesh.io/mesh-cli/internal/tripline/triplinetest"
)

// plantedToken is a secret the node's error carries: the item token in
// the remote URL git's refusal quotes. No event may hold it.
const plantedToken = "ghs_PlantedTripline0123456789abcdefXYZ"

// leakyLockedWS is a Workspacer whose every Authorize answers configLock
// with the item's token in the remote git names, as a stale
// .git/config.lock in the shared clone does for every item of a repo.
type leakyLockedWS struct{ *fakeWS }

func (w *leakyLockedWS) Authorize(context.Context, WorkspaceSpec, *gateway.GitHubToken) error {
	return errors.New(strings.Replace(configLock, "https://github.com/", "https://x-access-token:"+plantedToken+"@github.com/", 1))
}

// A node reports its failure classes to Tripline (ADR-034): a node loop
// on the gatewaytest fake claims three ready items of one goal, each on
// its own item id, and every one's token cannot be written into the repo
// cache — released as an infrastructure hold. The triplinetest ingest
// sees three events grouped into exactly one Issue, Count 3, tagged with
// the node and the item; no event carries the planted token or the
// node's bearer; and `mesh node status`'s reporting line reads on.
func TestNodeReportsInfrastructureToTripline(t *testing.T) {
	const item3 = "abababab-3333-4333-8333-abababababab"
	f := seed(t)
	f.SetItem(item2, map[string]any{"status": "ready", "dependsOn": []string{}})
	f.WorkItems[item3] = map[string]any{"id": item3, "goalId": goalID, "orgId": orgID, "seq": 3, "title": "And document it",
		"brief": "Docs.", "acceptance": "docs exist", "repoId": repoID, "baseRef": "main", "status": "ready", "dependsOn": []string{}}
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	f.GitHubTokens[item3] = map[string]any{"token": plantedToken, "expiresAt": exp, "repo": "acme/widgets"}
	for _, id := range []string{item1, item2, item3} {
		f.GitHubTokens[id]["token"] = plantedToken
		issueClaude(f, id)
	}

	ingest := triplinetest.NewServer(t)
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "tripnode", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(3),
		Tripline: tripline.Config{DSN: ingest.DSN(), Secrets: []string{"rs_test"}}}
	if line := ReportingStatusLine(cfg); !strings.HasPrefix(line, "reporting: on") {
		t.Fatalf("status line %q, want reporting: on", line)
	}
	rep, err := tripline.New(cfg.Tripline, cfg.Slug, "v0.0.0-e2e")
	if err != nil {
		t.Fatal(err)
	}
	if got := ReportingStartLine(rep); !strings.HasPrefix(got, "tripline reporting on (") {
		t.Fatalf("start line %q", got)
	}

	logs := &syncBuf{}
	lg := log.New(logs, "", 0)
	gw := gateway.New(f.URL(), "rs_test")
	ws := &leakyLockedWS{fakeWS: &fakeWS{root: t.TempDir()}}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Declines: &Declines{}, Log: lg, Tripline: rep}
	r := &Runner{GW: gw, Harness: &fakeHarness{}, WS: ws, Cfg: cfg, Scopes: loop.Scopes, WorkRoot: ws.root, ProgressEvery: time.Hour,
		Credentials: &Credentials{GW: gw, Log: lg}, Declines: loop.Declines, Tripline: loop.Tripline, Log: lg}
	loop.Dispatch = func(ctx context.Context, item gateway.WorkItem) { _ = r.RunItem(ctx, item) }

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	loop.Tick(ctx)
	loop.wg.Wait()

	if n := strings.Count(logs.String(), "released as an infrastructure hold, no attempt charged"); n != 3 {
		t.Fatalf("%d infrastructure releases, want 3:\n%s", n, logs.String())
	}
	if !rep.Flush(10 * time.Second) {
		t.Fatal("the reporter did not flush")
	}
	if !ingest.Wait(3, 10*time.Second) {
		t.Fatalf("ingest has %d events, want 3", len(ingest.Events()))
	}

	issues := ingest.Issues()
	if len(issues) != 1 || issues[0].Count != 3 {
		t.Fatalf("issues = %+v, want one with Count 3", issues)
	}
	is := issues[0]
	if is.Tags["node"] != "tripnode" || is.Tags["item"] == "" || is.Tags["class"] != string(tripline.Infrastructure) || is.Tags["goal"] != "ship-it" {
		t.Fatalf("issue tags = %v", is.Tags)
	}

	items := map[string]bool{}
	for _, ev := range ingest.Events() {
		items[ev.Tags["item"]] = true
		if ev.Tags["node"] != "tripnode" {
			t.Errorf("event %s tagged node %q", ev.EventID, ev.Tags["node"])
		}
		if !strings.Contains(ev.ExceptionValue, "could not lock config file") {
			t.Errorf("event %s exception %q lost git's words", ev.EventID, ev.ExceptionValue)
		}
		// the parsed event and every byte of the payload as it arrived
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		b = append(b, ev.Payload...)
		for _, secret := range []string{plantedToken, "rs_test", "sk-ant-oat01-ISSUED"} {
			if strings.Contains(string(b), secret) {
				t.Errorf("event %s carries the planted secret %q: %s", ev.EventID, secret, b)
			}
		}
		if !strings.Contains(string(b), "could not lock config file") {
			t.Errorf("event %s lost git's words: %s", ev.EventID, b)
		}
	}
	if len(items) != 3 || !items[item1] || !items[item2] || !items[item3] {
		t.Fatalf("events tagged items %v, want the three", items)
	}
}
