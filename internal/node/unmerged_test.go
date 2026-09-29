package node

import (
	"context"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// repoB is a second repo for the per-repo limit's tests; repoID is A.
const repoB = "cccccccc-3333-4333-8333-cccccccccccc"

// addReview adds an item of the goal whose pull request is open and
// waiting to merge (status review) in that repo; its id is title.
func (p *priorityFake) addReview(org, goal, title, repo string, seq int) {
	p.addItemRow(org, goal, title, seq, map[string]any{"status": "review", "repoId": repo, "prUrl": "https://github.com/acme/" + title + "/pull/1"})
}

// reviewReads counts the fake's work item listings in status review, by org.
func (p *priorityFake) reviewReads() map[any]int {
	out := map[any]int{}
	for _, r := range p.ReadsNamed("workItemIndexs") {
		if r.Input["status"] == "review" {
			out[r.Input["org_id"]]++
		}
	}
	return out
}

// Config parses `claim: unmerged_per_repo` and Reloaded carries it.
func TestClaimConfigParsesAndReloads(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeNodeYAML(t, "slug: mini\nname: mini\ncapacity: 1\nclaim:\n  unmerged_per_repo: 7\n")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Claim.UnmergedPerRepo != 7 {
		t.Fatalf("claim = %+v, want unmerged_per_repo 7", cfg.Claim)
	}
	writeNodeYAML(t, "slug: mini\nname: mini\ncapacity: 1\nclaim: {unmerged_per_repo: -1}\n")
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "claim.unmerged_per_repo") {
		t.Fatalf("a negative limit: err = %v, want one naming the key", err)
	}
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	base := &Config{OrgLogin: "acme", OrgID: orgID, Slug: "n", Capacity: Fixed(1)}
	reload := Reloader(gw, base, scopesOf(base), func() (*Config, error) { return cfg, nil }, func(context.Context, Scope) error { return nil })
	got, _ := reload(context.Background())
	if got.Claim == nil || got.Claim.UnmergedPerRepo != 7 {
		t.Fatalf("Reloaded.Claim = %+v, want unmerged_per_repo 7", got.Claim)
	}
}

// Limit 2 and repo A has two pull requests in review: the tick claims
// repo B's fresh item and repo A's started work — its rework, its rebase
// and a rework past the attempt bound (finishFirstAttempts), which loses
// its place in the order but not its exemption — leaves repo A's fresh
// item ready, and the holding report names the repo and the limit.
func TestLoopHoldsFreshClaimsForARepoAtItsUnmergedLimit(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "steady", "normal")
	p.addReview(orgID, "steady", "a-open-1", repoID, 1)
	p.addReview(orgID, "steady", "a-open-2", repoID, 2)
	p.addItemRow(orgID, "steady", "a-fresh", 3, nil)
	p.addItemRow(orgID, "steady", "b-fresh", 4, map[string]any{"repoId": repoB})
	p.addItemRow(orgID, "steady", "a-rework", 5, map[string]any{"prUrl": "https://github.com/acme/a/pull/5", "evaluation": "the test does not pass", "attempts": 1})
	p.addItemRow(orgID, "steady", "a-rebase", 6, map[string]any{"prUrl": "https://github.com/acme/a/pull/6", "rebaseState": "requested", "attempts": 1})
	p.addItemRow(orgID, "steady", "a-cycling", 7, map[string]any{"prUrl": "https://github.com/acme/a/pull/7", "evaluation": "still failing", "attempts": 2})
	p.register()
	loop, logs := p.loop(5)
	loop.Cfg.Claim = ClaimConfig{UnmergedPerRepo: 2}
	if got := p.claimedBy(loop); strings.Join(got, ",") != "a-rework,a-rebase,b-fresh,a-cycling" {
		t.Fatalf("claimed %v, want a-rework,a-rebase,b-fresh,a-cycling", got)
	}
	if s := p.Item("a-fresh")["status"]; s != "ready" {
		t.Fatalf("a-fresh is %v, want ready", s)
	}
	want := "repo " + repoID + " has 2 unmerged pull requests, at node.yaml's claim.unmerged_per_repo 2"
	if n := strings.Count(logs.String(), want); n != 1 {
		t.Fatalf("the hold is logged %d times, want once:\n%s", n, logs)
	}
	p.claimedBy(loop) // the next heartbeat carries this pass's holding
	u := unclaimedOf(t, p.lastHolding())["steady/item"]
	if u == nil || !strings.Contains(u["reason"].(string), want) || u["count"] != float64(1) {
		t.Fatalf("holding for steady = %#v, want one item held for %q", u, want)
	}
}

// claim.unmerged_per_repo is read from node.yaml every tick: absent
// claims everything, set to 1 the next tick holds, absent again claims.
func TestLoopUnmergedLimitIsReadEveryTick(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "steady", "normal")
	p.addReview(orgID, "steady", "a-open", repoID, 1)
	p.addItemRow(orgID, "steady", "a1", 2, nil)
	loop, _, logs := p.liveLoop(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: 3\n")
	if got := p.claimedBy(loop); strings.Join(got, ",") != "a1" {
		t.Fatalf("absent: claimed %v, want a1", got)
	}
	writeNodeYAML(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: 3\nclaim:\n  unmerged_per_repo: 1\n")
	p.addItemRow(orgID, "steady", "a2", 3, nil)
	if got := p.claimedBy(loop); len(got) != 0 {
		t.Fatalf("at the limit: claimed %v, want none", got)
	}
	if !strings.Contains(logs.String(), "claim: unmerged_per_repo 0 → 1") {
		t.Fatalf("the change is not logged:\n%s", logs)
	}
	writeNodeYAML(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: 3\n")
	if got := p.claimedBy(loop); strings.Join(got, ",") != "a2" {
		t.Fatalf("absent again: claimed %v, want a2", got)
	}
	if loop.UnmergedPerRepo() != 0 {
		t.Fatalf("limit in force = %d, want 0", loop.UnmergedPerRepo())
	}
}

// At the limit with only fresh items the tick reads each org's goals and
// review items once; with the key absent or 0 no review listing is read.
func TestLoopUnmergedLimitReadsReviewOncePerOrg(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "steady", "normal")
	p.addGoal(org2, "fire", "normal")
	p.addReview(orgID, "steady", "a-open", repoID, 1)
	p.addReview(org2, "fire", "g-open", repoB, 1)
	p.addItemRow(orgID, "steady", "a1", 2, nil)
	p.addItemRow(orgID, "steady", "a2", 3, nil)
	p.addItemRow(org2, "fire", "g1", 2, map[string]any{"repoId": repoB})
	loop, _ := p.loop(3)
	loop.Cfg.Claim = ClaimConfig{UnmergedPerRepo: 1}
	if got := p.claimedBy(loop); len(got) != 0 {
		t.Fatalf("claimed %v, want none", got)
	}
	if got := p.reviewReads(); got[orgID] != 1 || got[org2] != 1 || len(got) != 2 {
		t.Fatalf("review reads by org = %v, want one each", got)
	}
	goals := map[any]int{}
	for _, r := range p.ReadsNamed("goalIndexs") {
		goals[r.Input["org_id"]]++
	}
	if goals[orgID] != 1 || goals[org2] != 1 {
		t.Fatalf("goal reads by org = %v, want one each", goals)
	}

	q := newPriorityFake(t)
	q.addGoal(orgID, "steady", "normal")
	q.addReview(orgID, "steady", "a-open", repoID, 1)
	q.addItemRow(orgID, "steady", "a1", 2, nil)
	q.addItemRow(orgID, "steady", "a2", 3, nil)
	if got := q.tick(3); strings.Join(got, ",") != "a1,a2" {
		t.Fatalf("no limit: claimed %v, want a1,a2", got)
	}
	if got := q.reviewReads(); len(got) != 0 {
		t.Fatalf("no limit: review reads = %v, want none", got)
	}

	r := newPriorityFake(t)
	r.addGoal(orgID, "steady", "normal")
	r.addReview(orgID, "steady", "a-open", repoID, 1)
	r.addItemRow(orgID, "steady", "a1", 2, nil)
	r.addItemRow(orgID, "steady", "a2", 3, nil)
	r.register()
	zero, _ := r.loop(3)
	zero.Cfg.Claim = ClaimConfig{UnmergedPerRepo: 0}
	if got := r.claimedBy(zero); strings.Join(got, ",") != "a1,a2" {
		t.Fatalf("limit 0: claimed %v, want a1,a2", got)
	}
	if got := r.reviewReads(); len(got) != 0 {
		t.Fatalf("limit 0: review reads = %v, want none", got)
	}
}

// Two fresh items of one repo under limit 1 with capacity 2: the first
// claimed counts against the repo, so the tick claims only it.
func TestLoopUnmergedLimitCountsThisTicksClaims(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "steady", "normal")
	p.addItemRow(orgID, "steady", "a1", 1, nil)
	p.addItemRow(orgID, "steady", "a2", 2, nil)
	loop, _ := p.loop(2)
	loop.Cfg.Claim = ClaimConfig{UnmergedPerRepo: 1}
	if got := p.claimedBy(loop); strings.Join(got, ",") != "a1" {
		t.Fatalf("claimed %v, want a1 alone", got)
	}
}

// A failed review listing leaves the repo's count unknown: the tick
// holds its fresh items, as at the limit, and still claims its rework.
func TestLoopUnmergedLimitHoldsFreshClaimsWhenReviewIsUnread(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "steady", "normal")
	p.addItemRow(orgID, "steady", "a-fresh", 1, nil)
	p.addItemRow(orgID, "steady", "a-rework", 2, map[string]any{"prUrl": "https://github.com/acme/a/pull/2", "evaluation": "the test does not pass", "attempts": 1})
	p.FailReads["workItemIndexs/status=review"] = "gateway timeout"
	p.register()
	loop, logs := p.loop(3)
	loop.Cfg.Claim = ClaimConfig{UnmergedPerRepo: 5}
	if got := p.claimedBy(loop); strings.Join(got, ",") != "a-rework" {
		t.Fatalf("claimed %v, want a-rework alone", got)
	}
	if s := p.Item("a-fresh")["status"]; s != "ready" {
		t.Fatalf("a-fresh is %v, want ready", s)
	}
	want := "repo " + repoID + "'s unmerged pull requests could not be counted (acme, globex's review items unread) under node.yaml's claim.unmerged_per_repo 5"
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("the hold is not logged as %q:\n%s", want, logs)
	}
	p.claimedBy(loop)
	u := unclaimedOf(t, p.lastHolding())["steady/item"]
	if u == nil || !strings.Contains(u["reason"].(string), want) {
		t.Fatalf("holding for steady = %#v, want the reason %q", u, want)
	}
}

// Two repos of one goal at their limit: the goal's holding entry names
// both repos, not the first alone.
func TestLoopUnmergedLimitNamesEveryHeldRepo(t *testing.T) {
	const repoC = "dddddddd-4444-4444-8444-dddddddddddd"
	p := newPriorityFake(t)
	p.addGoal(orgID, "steady", "normal")
	p.addReview(orgID, "steady", "a-open", repoID, 1)
	p.addReview(orgID, "steady", "c-open", repoC, 2)
	p.addItemRow(orgID, "steady", "a-fresh", 3, nil)
	p.addItemRow(orgID, "steady", "c-fresh", 4, map[string]any{"repoId": repoC})
	p.register()
	loop, _ := p.loop(3)
	loop.Cfg.Claim = ClaimConfig{UnmergedPerRepo: 1}
	if got := p.claimedBy(loop); len(got) != 0 {
		t.Fatalf("claimed %v, want none", got)
	}
	p.claimedBy(loop)
	u := unclaimedOf(t, p.lastHolding())["steady/item"]
	if u == nil || u["count"] != float64(2) {
		t.Fatalf("holding for steady = %#v, want two items", u)
	}
	for _, repo := range []string{repoID, repoC} {
		want := "repo " + repo + " has 1 unmerged pull requests, at node.yaml's claim.unmerged_per_repo 1"
		if !strings.Contains(u["reason"].(string), want) {
			t.Fatalf("holding reason %q does not name %q", u["reason"], want)
		}
	}
}

// A repo's unmerged work is counted over every served org: org2 has
// nothing ready but a pull request in review in repo A, so under limit 1
// acme's fresh item of repo A is held and its repo B item claimed.
func TestLoopUnmergedLimitCountsReviewOfAnOrgWithNothingReady(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "steady", "normal")
	p.addGoal(org2, "fire", "normal")
	p.addReview(org2, "fire", "g-open", repoID, 1)
	p.addItemRow(orgID, "steady", "a-fresh", 2, nil)
	p.addItemRow(orgID, "steady", "b-fresh", 3, map[string]any{"repoId": repoB})
	p.register()
	loop, logs := p.loop(3)
	loop.Cfg.Claim = ClaimConfig{UnmergedPerRepo: 1}
	if got := p.claimedBy(loop); strings.Join(got, ",") != "b-fresh" {
		t.Fatalf("claimed %v, want b-fresh alone", got)
	}
	if got := p.reviewReads(); got[orgID] != 1 || got[org2] != 1 {
		t.Fatalf("review reads by org = %v, want one each", got)
	}
	want := "repo " + repoID + " has 1 unmerged pull requests, at node.yaml's claim.unmerged_per_repo 1"
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("the hold is not logged as %q:\n%s", want, logs)
	}
}

// An org whose reads fail — its ready listing and its review listing —
// leaves every repo's count unknown: its review listing is still tried,
// and the other org's fresh items are held while its rework is claimed.
func TestLoopUnmergedLimitHoldsFreshClaimsWhenAnotherOrgIsUnread(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "steady", "normal")
	p.addGoal(org2, "fire", "normal")
	p.addReview(org2, "fire", "g-open", repoID, 1)
	p.addItemRow(orgID, "steady", "a-fresh", 2, nil)
	p.addItemRow(orgID, "steady", "a-rework", 3, map[string]any{"prUrl": "https://github.com/acme/a/pull/3", "evaluation": "the test does not pass", "attempts": 1})
	p.FailReads["workItemIndexs/org_id="+org2] = "gateway timeout"
	p.register()
	loop, _ := p.loop(3)
	loop.Cfg.Claim = ClaimConfig{UnmergedPerRepo: 5}
	if got := p.claimedBy(loop); strings.Join(got, ",") != "a-rework" {
		t.Fatalf("claimed %v, want a-rework alone", got)
	}
	if got := p.reviewReads(); got[org2] != 1 {
		t.Fatalf("review reads by org = %v, want org2's tried despite its failed ready read", got)
	}
}
