package node

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

// The replays of runsheet goal ci-fits-the-mini's e2e-sharding item
// (ADR-020, Context): failed five times on "the three shards' durations
// within 20% of each other", measured while the mini's load ran 30–65 on
// 18 cores, its mechanism passing every time. Replayed on today's node,
// item 5's host reading is recorded with a pass, and item 17's failure
// on the clause item 14 already failed on opens by saying so.

const (
	// item 5's title, as mesh_items reads it on runsheet/runsheet
	cifitsItem5Title = "e2e shards are cut by measured test duration, not by name hash: a checked-in timings file, a generator, and a test that the three shards are within 20%"
	cifitsItem5Brief = "Cut the e2e suite into three shards by measured test duration instead of by name hash: a checked-in timings file from a whole-package `go test -json ./e2e` run, a generator that writes the shard lists from it, and a test that the shards are disjoint and cover every test."
	// item 5's acceptance, written as it was: the mechanism, and a
	// reading of the host as one of its clauses
	cifitsItem5Acceptance = "shards are cut by measured duration; a checked-in timings file (e2e/shard_times.json); a generator that writes the shard lists from it; the three lists disjoint and covering every test; the -check tools pass; the three shards' durations within 20% of each other"
	cifitsHostClause      = "the three shards' durations within 20% of each other"
	// the branch's README, recording a round measured under load
	cifitsItem5README = "# e2e\n\nShards are cut by `go run ./e2e/shardplan` from shard_times.json.\n\n" +
		"Round of 2026-09-29, sequential on the mini (load 30–65 on 18 cores): 65.0 s, 63.5 s, 54.4 s — max/min 1.19.\n"
	cifitsItem5Verdict = "The planner cuts by shard_times.json, the generator writes three disjoint lists covering all 309 tests, and `go run ./e2e/shardplan -check` passes. " +
		"e2e/README.md records the 2026-09-29 round truthfully against the file it cites.\n\n" +
		"READING: the three shards' durations within 20% of each other || 65.0/63.5/54.4 s, max/min 1.19, 2026-09-29, load 30–65 on 18 cores\n" +
		"VERDICT: PASS"
)

// readmeWS is the item's checkout with its branch's e2e/README.md in it.
type readmeWS struct{ *fakeWS }

func (w readmeWS) Prepare(ctx context.Context, spec WorkspaceSpec) (string, error) {
	dir, err := w.fakeWS.Prepare(ctx, spec)
	if err != nil {
		return dir, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "e2e"), 0o755); err != nil {
		return dir, err
	}
	return dir, os.WriteFile(filepath.Join(dir, "e2e", "README.md"), []byte(cifitsItem5README), 0o644)
}

// readmeEvaluator is the scripted evaluator of item 5: it reads the
// branch's README in its workspace, as the new prompt asks it to judge
// the reading recorded truthfully, and answers with its READING line.
type readmeEvaluator struct{ fakeHarness }

func (h *readmeEvaluator) Run(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
	b, err := os.ReadFile(filepath.Join(job.Workspace, "e2e", "README.md"))
	if err != nil || !strings.Contains(string(b), "max/min 1.19") {
		h.text = "e2e/README.md records no round.\nVERDICT: FAIL"
	}
	return h.fakeHarness.Run(ctx, job, on)
}

// cifitsItem5 makes item 1 the replay of item 5, in evaluating on its branch.
func cifitsItem5(f *gatewaytest.Fake) {
	f.WorkItems[item1]["title"] = cifitsItem5Title
	f.WorkItems[item1]["brief"] = cifitsItem5Brief
	f.WorkItems[item1]["acceptance"] = cifitsItem5Acceptance
	f.WorkItems[item1]["branch"] = "mesh/ci-fits-the-mini-5"
}

// lastEvaluation is the verdict the evaluation under test recorded.
func lastEvaluation(t *testing.T, f *gatewaytest.Fake) map[string]any {
	t.Helper()
	calls := f.CallsNamed("evaluateWorkItem")
	if len(calls) == 0 {
		t.Fatal("no verdict recorded")
	}
	return calls[len(calls)-1].Input
}

func evaluateIt(t *testing.T, ev *Evaluator) {
	t.Helper()
	item, err := ev.GW.WorkItem(context.Background(), item1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.Evaluate(context.Background(), *item); err != nil {
		t.Fatal(err)
	}
}

// assertItem5Recorded: passed, the READING line in the notes, the item in
// review, and node.log saying the reading was recorded.
func assertItem5Recorded(t *testing.T, f *gatewaytest.Fake, logs string) {
	t.Helper()
	in := lastEvaluation(t, f)
	if in["passed"] != true {
		t.Fatalf("recorded %+v", in)
	}
	notes, _ := in["notes"].(string)
	if !strings.Contains(notes, "READING: "+cifitsHostClause+" || 65.0/63.5/54.4 s, max/min 1.19, 2026-09-29, load 30–65 on 18 cores") {
		t.Fatalf("notes lack the reading:\n%s", notes)
	}
	if _, ok := in[gateway.FailedClausesField]; ok && in[gateway.FailedClausesField] != nil {
		t.Fatalf("a pass carried failed clauses: %+v", in[gateway.FailedClausesField])
	}
	if st := f.WorkItems[item1]["status"]; st != "review" {
		t.Fatalf("item status %v, want review", st)
	}
	if want := "item ship-it/1: reading recorded: " + cifitsHostClause; !strings.Contains(logs, want) {
		t.Fatalf("node.log lacks %q:\n%s", want, logs)
	}
}

// Item 5 replayed on Claude: the evaluator checks the mechanism, takes
// its reading of the host's ratio and passes; the fake records the pass
// with the reading in its notes and the item goes to review.
func TestReplayCIFitsItem5HostReadingPasses(t *testing.T) {
	f, ev, _, logs := priorAttemptsFixture(t)
	// item 5 is a first attempt: no earlier verdicts
	f.WorkItems[item1]["attempts"] = 0
	f.WorkItems[item1]["evaluations"] = nil
	cifitsItem5(f)
	h := &readmeEvaluator{fakeHarness{text: cifitsItem5Verdict}}
	ev.Harness = h
	ev.WS = readmeWS{ev.WS.(*fakeWS)}
	evaluateIt(t, ev)
	if len(h.jobs) != 1 || !strings.Contains(h.jobs[0].Prompt, "READING:") {
		t.Fatalf("the evaluator was not asked for its reading: %d jobs", len(h.jobs))
	}
	assertItem5Recorded(t, f, logs.String())
}

// Item 5 replayed on Codex: the same reading, recorded the same way.
func TestReplayCIFitsItem5HostReadingPassesOnCodex(t *testing.T) {
	f, ev, claude, codex, _, logs := familyFixture(t, "normal", "sonnet", "VERDICT: FAIL", cifitsItem5Verdict)
	cifitsItem5(f)
	ev.WS = readmeWS{ev.WS.(*fakeWS)}
	evaluateIt(t, ev)
	if len(claude.jobs) != 0 || codex.runs() != 1 {
		t.Fatalf("runs: claude %d, codex %d", len(claude.jobs), codex.runs())
	}
	assertItem5Recorded(t, f, logs.String())
}

// cifitsItem5LongVerdict is item 5's verdict with a review long enough
// that the notes are cut to fit: the READING line sits near the verdict,
// below the cut, and must still be recorded.
func cifitsItem5LongVerdict() string {
	review := strings.Repeat("The shard planner reads e2e/shard_times.json and the generator's lists were checked test by test against `go test -list . ./e2e`. ", 60)
	return review + "\n\n" + cifitsItem5Verdict
}

// Item 5 replayed on Claude with a long review: the notes are cut, the
// reading is not.
func TestReplayCIFitsItem5LongReviewKeepsReading(t *testing.T) {
	f, ev, _, logs := priorAttemptsFixture(t)
	f.WorkItems[item1]["attempts"] = 0
	f.WorkItems[item1]["evaluations"] = nil
	cifitsItem5(f)
	verdict := cifitsItem5LongVerdict()
	if len(verdict) <= 4000 {
		t.Fatalf("the review is %d bytes, not long enough to be cut", len(verdict))
	}
	ev.Harness = &readmeEvaluator{fakeHarness{text: verdict}}
	ev.WS = readmeWS{ev.WS.(*fakeWS)}
	evaluateIt(t, ev)
	assertItem5Cut(t, f)
	assertItem5Recorded(t, f, logs.String())
}

// Item 5 replayed on Codex with a long review: the same.
func TestReplayCIFitsItem5LongReviewKeepsReadingOnCodex(t *testing.T) {
	f, ev, _, codex, _, logs := familyFixture(t, "normal", "sonnet", "VERDICT: FAIL", cifitsItem5LongVerdict())
	cifitsItem5(f)
	ev.WS = readmeWS{ev.WS.(*fakeWS)}
	evaluateIt(t, ev)
	if codex.runs() != 1 {
		t.Fatalf("codex runs %d", codex.runs())
	}
	assertItem5Cut(t, f)
	assertItem5Recorded(t, f, logs.String())
}

// assertItem5Cut: the recorded notes were cut to fit.
func assertItem5Cut(t *testing.T, f *gatewaytest.Fake) {
	t.Helper()
	notes, _ := lastEvaluation(t, f)["notes"].(string)
	if len(notes) > 4000 || !strings.Contains(notes, "…") {
		t.Fatalf("notes were not cut (%d bytes):\n%s", len(notes), notes)
	}
}

// cifitsItem14 records item 14's verdict in the fake as attempt 1, its
// FAILED clause with it, and puts the retry back in evaluating.
func cifitsItem14(t *testing.T, f *gatewaytest.Fake, gw *gateway.Client) {
	t.Helper()
	text := readTestFile(t, filepath.Join("..", "harness", "testdata", "cifits_item14_verdict.txt"))
	passed, notes, ok := harness.ParseVerdict(text)
	if !ok || passed {
		t.Fatalf("item 14's verdict reads passed=%v ok=%v", passed, ok)
	}
	var clauses []gateway.FailedClause
	for _, c := range harness.ParseFailedClauses(text) {
		clauses = append(clauses, gateway.FailedClause{Clause: c.Clause, Satisfy: c.Satisfy})
	}
	if len(clauses) != 1 {
		t.Fatalf("item 14's clauses %+v", clauses)
	}
	if err := gw.EvaluateWorkItem(context.Background(), item1, false, strings.TrimSpace(notes), clauses, 10, nil, gateway.EvaluateChoice{}); err != nil {
		t.Fatal(err)
	}
	f.WorkItems[item1]["status"] = "evaluating"
	f.WorkItems[item1]["branch"] = "mesh/ci-fits-the-mini-17"
}

const (
	cifitsItem14Clause = "the three shards within 20% of each other, proven by a sequential run on the mini"
	cifitsItem17Clause = "the three shards within 20%"
)

// assertSameClause: the notes open with the same-clause line, the clause
// is carried, the prompt carried attempt 1's clause, node.log says it.
func assertSameClause(t *testing.T, f *gatewaytest.Fake, prompt, logs string) {
	t.Helper()
	in := lastEvaluation(t, f)
	if in["passed"] != false {
		t.Fatalf("recorded %+v", in)
	}
	notes, _ := in["notes"].(string)
	first, _, _ := strings.Cut(notes, "\n")
	if want := "fails on the same clause as attempt 1: " + cifitsItem17Clause; first != want {
		t.Fatalf("notes' first line %q, want %q:\n%s", first, want, notes)
	}
	cl, _ := in[gateway.FailedClausesField].([]any)
	if len(cl) != 1 || cl[0].(map[string]any)["clause"] != cifitsItem17Clause {
		t.Fatalf("failed clauses %+v", in[gateway.FailedClausesField])
	}
	if !strings.Contains(prompt, cifitsItem14Clause) || !strings.Contains(prompt, "## Attempt 1") {
		t.Fatalf("the prompt lacks attempt 1's clause:\n%s", prompt)
	}
	if want := "item ship-it/1: fails on the same clause as attempt 1"; !strings.Contains(logs, want) {
		t.Fatalf("node.log lacks %q:\n%s", want, logs)
	}
}

// cifitsFixture is an evaluator on item 1, in evaluating on this node,
// with no verdict recorded yet; Claude answers says.
func cifitsFixture(t *testing.T, says string) (*gatewaytest.Fake, *gateway.Client, *Evaluator, *fakeHarness, *bytes.Buffer) {
	t.Helper()
	f := seed(t)
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(1)}
	gw := gateway.New(f.URL(), "rs_test")
	f.WorkItems[item1]["status"] = "evaluating"
	f.WorkItems[item1]["nodeId"] = cfg.NodeID
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: says}
	var logs bytes.Buffer
	ev := &Evaluator{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Scopes: scopesOf(cfg), WorkRoot: ws.root, MaxBudgetUSD: 2, Log: log.New(&logs, "", 0)}
	return f, gw, ev, h, &logs
}

// Item 14 against item 17 on Claude: with item 14's verdict recorded as
// attempt 1, item 17's verdict — the same clause failed again, without
// the line — is recorded opening with "fails on the same clause as
// attempt 1", so the narrow re-plan changes the clause or escalates.
func TestReplayCIFitsItem14Then17SameClause(t *testing.T) {
	item17 := readTestFile(t, filepath.Join("..", "harness", "testdata", "cifits_item17_verdict.txt"))
	if strings.HasPrefix(strings.TrimSpace(item17), harness.SameClauseLine) {
		t.Fatal("item 17's verdict already carries the line")
	}
	f, gw, ev, h, logs := cifitsFixture(t, item17)
	cifitsItem14(t, f, gw)
	evaluateIt(t, ev)
	if len(h.jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(h.jobs))
	}
	assertSameClause(t, f, h.jobs[0].Prompt, logs.String())
}

// Item 14 against item 17 on Codex: the same first line.
func TestReplayCIFitsItem14Then17SameClauseOnCodex(t *testing.T) {
	item17 := readTestFile(t, filepath.Join("..", "harness", "testdata", "cifits_item17_verdict.txt"))
	f, ev, claude, codex, _, logs := familyFixture(t, "normal", "sonnet", "VERDICT: PASS", item17)
	cifitsItem14(t, f, ev.GW.(*gateway.Client))
	evaluateIt(t, ev)
	if len(claude.jobs) != 0 || codex.runs() != 1 {
		t.Fatalf("runs: claude %d, codex %d", len(claude.jobs), codex.runs())
	}
	assertSameClause(t, f, codex.jobs[0].Prompt, logs.String())
}

// truncateNotes cuts the review and keeps every READING line, to n.
func TestTruncateNotesKeepsReadings(t *testing.T) {
	reading := "READING: " + cifitsHostClause + " || max/min 1.19"
	s := strings.Repeat("review ", 200) + "\n" + reading + "\nmore review after it"
	for _, n := range []int{100, 400, 1000} {
		got := truncateNotes(s, n)
		if len(got) > n || !strings.Contains(got, reading) {
			t.Fatalf("n=%d: %d bytes:\n%s", n, len(got), got)
		}
	}
	if got := truncateNotes("short\n"+reading, 4000); got != "short\n"+reading {
		t.Fatalf("an uncut note changed: %q", got)
	}
	if got, want := truncateNotes(strings.Repeat("x", 50), 10), truncate(strings.Repeat("x", 50), 10); got != want {
		t.Fatalf("no reading: %q, want %q", got, want)
	}
}

// Both families' notes, cut together, keep each one's reading.
func TestBothNotesKeepReadings(t *testing.T) {
	long := cifitsItem5LongVerdict()
	_, notes, _ := harness.ParseVerdict(long)
	got := bothNotes(family{}, notes, true, family{codex: true}, notes, true, 3000)
	if len(got) > 3000 || strings.Count(got, "READING: "+cifitsHostClause) != 2 {
		t.Fatalf("%d bytes:\n%s", len(got), got)
	}
}
