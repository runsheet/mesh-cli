package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

const issuedSecret = "sk-ant-oat01-ISSUED-by-the-platform"

// streamHarness plays testdata's stream-json through ParseStream, as
// `claude -p` would, and records each session's job.
type streamHarness struct {
	mu   sync.Mutex
	jobs []harness.Job
	file string
	text string
	// texts, when set, are the final messages of the next runs in turn,
	// before text.
	texts []string
}

func (h *streamHarness) Name() string { return "claude-code" }
func (h *streamHarness) Run(_ context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
	h.mu.Lock()
	h.jobs = append(h.jobs, job)
	text := h.text
	if len(h.texts) > 0 {
		text, h.texts = h.texts[0], h.texts[1:]
	}
	h.mu.Unlock()
	f, err := os.Open(h.file)
	if err != nil {
		return harness.Result{}, err
	}
	defer f.Close()
	res := harness.ParseStream(f, nil)
	if text != "" {
		res.Text = text
	}
	handInScript(job, nil, res.Text, res.WorkCalls)
	return res, nil
}

func (h *streamHarness) Jobs() []harness.Job {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]harness.Job(nil), h.jobs...)
}

func usageStream() string { return filepath.Join("..", "harness", "testdata", "stream_usage.jsonl") }

// issueCredential has the platform issue the subscription credential
// cred-1 (label max-cloud) for a session's subject: an item, a goal or
// a direction.
func issueCredential(f *gatewaytest.Fake, subject string) {
	f.SessionCredentials[subject] = []map[string]any{{"id": "cred-1", "label": "max-cloud", "vendor": "anthropic", "kind": "subscription", "serves": []any{"claude-code"}, "secret": issuedSecret}}
}

// usageCredentials is the node's Credentials with max-cloud's windows
// read before the session: five-hour at 20%, seven-day at 10%.
func usageCredentials(gw CredentialGateway, l *log.Logger) *Credentials {
	limits := &Limits{}
	limits.Observe("max-cloud", &harness.RateLimit{Status: "allowed",
		FiveHour: harness.RateLimitWindow{Utilization: 0.2, ResetsAt: time.Unix(1790330400, 0).UTC()},
		SevenDay: harness.RateLimitWindow{Utilization: 0.1, ResetsAt: time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)}})
	return &Credentials{GW: gw, Limits: limits, Log: l}
}

// checkSessionUsage is the one usage report the fake was handed, on
// mutation: the session of the stream fixture on cred-1 — its tokens,
// its cost in cents (list; none actual on a subscription) and the
// windows before (the node's reading) and after (the session's last
// rate_limit_event), each with its reset. Neither old usage mutation
// was called, and no recorded input carries the issued secret.
func checkSessionUsage(t *testing.T, f *gatewaytest.Fake, mutation string) map[string]any {
	t.Helper()
	if len(f.SessionUsage) != 1 || f.SessionUsage[0].Name != mutation {
		t.Fatalf("usage reports: %+v, want one on %s", f.SessionUsage, mutation)
	}
	in := f.SessionUsage[0].Input
	for k, want := range map[string]any{
		"sessionId": "s-usage", "adapter": "claude-code", "model": "claude-sonnet-5",
		"credentialId": "cred-1", "credentialKind": "subscription",
		"inputTokens": float64(60600), "outputTokens": float64(789), "cacheReadTokens": float64(56000), "cacheWriteTokens": float64(3400),
		"listCostCents": float64(43), "actualCostCents": float64(0), "toolCalls": float64(0),
		"fiveHourBefore": 0.2, "sevenDayBefore": 0.1, "fiveHourAfter": 0.52, "sevenDayAfter": 0.31,
		"fiveHourResetsAtBefore": "2026-09-25T10:00:00Z", "sevenDayResetsAtBefore": "2026-09-30T08:00:00Z",
		"fiveHourResetsAtAfter": "2026-09-25T10:00:00Z", "sevenDayResetsAtAfter": "2026-09-30T08:00:00Z",
	} {
		if in[k] != want {
			t.Errorf("usage %s = %v, want %v", k, in[k], want)
		}
	}
	if _, ok := in["wallMs"].(float64); !ok {
		t.Errorf("no wallMs: %v", in)
	}
	checkNoOldUsage(t, f)
	for _, c := range f.Calls {
		b, _ := json.Marshal(c)
		if strings.Contains(string(b), issuedSecret) {
			t.Errorf("the secret is in %s's payload: %s", c.Name, b)
		}
	}
	return in
}

// checkNoOldUsage: no code path calls recordSessionUsage or recordUsage.
func checkNoOldUsage(t *testing.T, f *gatewaytest.Fake) {
	t.Helper()
	for _, old := range []string{"recordSessionUsage", "recordUsage"} {
		if calls := f.CallsNamed(old); len(calls) != 0 {
			t.Errorf("%s called: %+v", old, calls)
		}
	}
}

// When the platform issues a subscription credential for the claimed
// item, the build runs on it — CLAUDE_CODE_OAUTH_TOKEN from the platform,
// node.yaml not consulted — and finishWorkItem carries its usage report:
// the credential's id, the tokens and cost the stream reported and the
// windows before and after; the secret is in the session's env and
// nowhere else.
func TestBuildRunsOnAnIssuedCredentialAndReportsUsage(t *testing.T) {
	f := seed(t)
	issueCredential(f, item1)
	h := &streamHarness{file: usageStream()}
	r, _, item, row := runnerOnFake(t, f, h, nil)
	var logs syncBuf
	r.Log = log.New(&logs, "", 0)
	r.Credentials = usageCredentials(r.GW.(CredentialGateway), r.Log)

	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	jobs := h.Jobs()
	if len(jobs) != 1 {
		t.Fatalf("%d sessions", len(jobs))
	}
	if v, _ := envOf(jobs[0].Env, "CLAUDE_CODE_OAUTH_TOKEN"); v != issuedSecret {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN = %q, want the issued secret", v)
	}
	if Account(jobs[0].Env) != "max-cloud" {
		t.Errorf("Account = %q, want max-cloud", Account(jobs[0].Env))
	}
	if reads := f.CallsNamed("itemCredentials"); len(reads) != 1 || reads[0].Input["itemId"] != item1 {
		t.Fatalf("itemCredentials calls: %+v", reads)
	}
	if row(item1)["status"] != "evaluating" {
		t.Fatalf("item: %v", row(item1))
	}
	checkSessionUsage(t, f, "finishWorkItem")
	// the record the finish carried is logged too, on the item's label
	if recs := loggedUsage(t, logs.String()); len(recs) != 1 || recs[0].Label != "item ship-it/1" || recs[0].Kind != "build" || recs[0].Report.CredentialID != "cred-1" {
		t.Errorf("usage records = %+v:\n%s", recs, logs.String())
	}
	if strings.Contains(logs.String(), "usage: {") {
		t.Errorf("an unlabelled record:\n%s", logs.String())
	}
	for what, s := range map[string]string{"log": logs.String(), "lastNote": toString(row(item1)["lastNote"]), "lastError": toString(row(item1)["lastError"])} {
		if strings.Contains(s, issuedSecret) {
			t.Errorf("the secret is in the %s: %s", what, s)
		}
	}
}

// An already-done build carries its report on the same finishWorkItem.
func TestAlreadyDoneBuildReportsUsage(t *testing.T) {
	f := seed(t)
	issueCredential(f, item1)
	h := &streamHarness{file: usageStream(), text: "Main has it.\n\nALREADY DONE: https://github.com/acme/widgets/pull/9 holds it"}
	r, _, item, _ := runnerOnFake(t, f, h, nil)
	r.Credentials = usageCredentials(r.GW.(CredentialGateway), r.Log)
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	in := checkSessionUsage(t, f, "finishWorkItem")
	if calls := f.CallsNamed("finishWorkItem"); len(calls) != 1 || calls[0].Input["outcome"] != gateway.OutcomeAlreadyDone {
		t.Fatalf("finish calls: %+v (usage %v)", calls, in)
	}
}

// An evaluation's verdict carries its report, on the issued credential.
func TestEvaluationReportsUsage(t *testing.T) {
	f := seed(t)
	issueCredential(f, item1)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: "22222222-2222-4222-8222-222222222222"}
	f.WorkItems[item1]["status"], f.WorkItems[item1]["nodeId"], f.WorkItems[item1]["branch"] = "evaluating", cfg.NodeID, "mesh/ship-it-1"
	var item gateway.WorkItem
	item.ID, item.OrgID, item.GoalID, item.RepoID, item.Seq, item.Branch = item1, orgID, goalID, repoID, 1, "mesh/ship-it-1"
	ws := &fakeWS{root: t.TempDir()}
	h := &streamHarness{file: usageStream(), text: "Fine.\nVERDICT: PASS"}
	ev := &Evaluator{GW: gw, Harness: h, WS: ws, Scopes: scopesOf(cfg), WorkRoot: ws.root, Credentials: usageCredentials(gw, log.New(io.Discard, "", 0))}
	if err := ev.Evaluate(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if calls := f.CallsNamed("evaluateWorkItem"); len(calls) != 1 || calls[0].Input["passed"] != true {
		t.Fatalf("verdicts: %+v", calls)
	}
	checkSessionUsage(t, f, "evaluateWorkItem")
}

// Against a platform whose finishing commands take no `usage`, a node
// running several sessions on issued credentials finishes every one —
// the builds reach evaluating, the verdict lands — says the fallback
// once for the run, and writes each session's usage report as one log
// line, its credential named; the secret is in no log line.
func TestCredentialsLogUsageWithoutTheLedgerOncePerRun(t *testing.T) {
	const local = issuedSecret
	f := seed(t)
	issueCredential(f, item1)
	f.UnknownFields["usage"] = true
	h := &streamHarness{file: usageStream()}
	r, ws, item, row := runnerOnFake(t, f, h, nil)
	var logs syncBuf
	r.Log = log.New(&logs, "", 0)
	r.Credentials = &Credentials{GW: r.GW.(CredentialGateway), Limits: &Limits{}, Log: r.Log}

	items := []gateway.WorkItem{item}
	for i, id := range []string{"99999999-1111-4111-8111-999999999991", "99999999-1111-4111-8111-999999999992"} {
		f.WorkItems[id] = map[string]any{"id": id, "goalId": goalID, "orgId": orgID, "seq": 3 + i, "title": "More",
			"brief": "More.", "acceptance": "go test passes", "repoId": repoID, "baseRef": "main", "status": "claimed", "nodeId": r.Cfg.NodeID, "dependsOn": []string{}}
		f.GitHubTokens[id] = f.GitHubTokens[item1]
		issueCredential(f, id)
		it := item
		it.ID, it.Seq = id, 3+i
		items = append(items, it)
	}
	for _, it := range items {
		if err := r.RunItem(context.Background(), it); err != nil {
			t.Fatalf("item %s: %v", it.ID, err)
		}
		if row(it.ID)["status"] != "evaluating" {
			t.Fatalf("item %s: %v", it.ID, row(it.ID))
		}
	}
	// and an evaluation: its verdict lands without the report
	h.text = "Fine.\nVERDICT: PASS"
	ev := &Evaluator{GW: r.GW, Harness: h, WS: ws, Scopes: r.Scopes, WorkRoot: ws.root, Credentials: r.Credentials, Log: r.Log}
	evItem := item
	evItem.Branch = "mesh/ship-it-1"
	if err := ev.Evaluate(context.Background(), evItem); err != nil {
		t.Fatal(err)
	}
	if calls := f.CallsNamed("evaluateWorkItem"); len(calls) != 1 || calls[0].Input["passed"] != true {
		t.Fatalf("verdicts: %+v", calls)
	}
	for _, job := range h.Jobs() {
		if v, _ := envOf(job.Env, "CLAUDE_CODE_OAUTH_TOKEN"); v != local || Account(job.Env) != "max-cloud" {
			t.Fatalf("session not on the issued credential: %v", job.Env)
		}
	}
	out := logs.String()
	if n := strings.Count(out, "usage: the platform's finishing command takes no usage report; each session's record is logged here instead (said once, for every session this run)"); n != 1 {
		t.Errorf("ledger fallback said %d times:\n%s", n, out)
	}
	var reports []gateway.UsageReport
	var kinds []string
	for _, rec := range loggedUsage(t, out) {
		reports = append(reports, rec.Report)
		kinds = append(kinds, rec.Kind)
	}
	if want := []string{"build", "build", "build", "evaluate"}; !slices.Equal(kinds, want) {
		t.Errorf("kinds = %v, want %v", kinds, want)
	}
	if len(reports) != len(items)+1 {
		t.Fatalf("%d usage lines for %d sessions:\n%s", len(reports), len(items)+1, out)
	}
	for i, rep := range reports {
		if rep.SessionID != "s-usage" || rep.CredentialID != "cred-1" || rep.CredentialKind != "subscription" ||
			rep.InputTokens != 60600 || rep.OutputTokens != 789 || rep.ListCostCents != 43 || rep.FiveHourAfter == nil || *rep.FiveHourAfter != 0.52 {
			t.Errorf("report %d = %+v", i, rep)
		}
	}
	if strings.Contains(out, local) {
		t.Errorf("the secret is in the log:\n%s", out)
	}
	if len(f.SessionUsage) != 0 {
		t.Errorf("a refused usage was recorded: %+v", f.SessionUsage)
	}
	checkNoOldUsage(t, f)
}

// A gateway whose usage input has no turns, inputPerTurnMean,
// inputPerTurnMax or largestToolResults (mesh-spends-less-per-item — a
// platform older than this node) still takes the rest of the report:
// Carried treats ErrNoUsageDetail as landed, so the record is not
// logged again, only the once-per-run notice that the detail did not
// land; every session still gets its own `usage: turns=…` summary line
// from the stream it actually saw, whatever became of the report.
func TestCredentialsFallBackFromUsageDetailOncePerRun(t *testing.T) {
	f := seed(t)
	issueCredential(f, item1)
	f.UnknownFields["turns"] = true
	f.UnknownFields["inputPerTurnMean"] = true
	f.UnknownFields["inputPerTurnMax"] = true
	f.UnknownFields["largestToolResults"] = true
	h := &streamHarness{file: usageStream()}
	r, _, item, row := runnerOnFake(t, f, h, nil)
	var logs syncBuf
	r.Log = log.New(&logs, "", 0)
	r.Credentials = usageCredentials(r.GW.(CredentialGateway), r.Log)

	items := []gateway.WorkItem{item}
	for i, id := range []string{"99999999-2222-4222-8222-999999999991", "99999999-2222-4222-8222-999999999992"} {
		f.WorkItems[id] = map[string]any{"id": id, "goalId": goalID, "orgId": orgID, "seq": 3 + i, "title": "More",
			"brief": "More.", "acceptance": "go test passes", "repoId": repoID, "baseRef": "main", "status": "claimed", "nodeId": r.Cfg.NodeID, "dependsOn": []string{}}
		f.GitHubTokens[id] = f.GitHubTokens[item1]
		issueCredential(f, id)
		it := item
		it.ID, it.Seq = id, 3+i
		items = append(items, it)
	}
	for _, it := range items {
		if err := r.RunItem(context.Background(), it); err != nil {
			t.Fatalf("item %s: %v", it.ID, err)
		}
		if row(it.ID)["status"] != "evaluating" {
			t.Fatalf("item %s: %v", it.ID, row(it.ID))
		}
	}
	out := logs.String()
	if n := strings.Count(out, "usage: the platform's usage report carries no turns or tool-result detail; sent without them (said once, for every session this run)"); n != 1 {
		t.Errorf("detail fallback said %d times:\n%s", n, out)
	}
	if n := strings.Count(out, "usage: turns=7 input/turn mean=3 max=3 largest="); n != len(items) {
		t.Errorf("turn summary line seen %d times, want %d:\n%s", n, len(items), out)
	}
	if strings.Contains(out, "the platform's finishing command takes no usage report") {
		t.Errorf("the wrong fallback fired:\n%s", out)
	}
	if len(f.SessionUsage) != len(items) {
		t.Fatalf("SessionUsage = %+v, want %d landed reports", f.SessionUsage, len(items))
	}
	for _, c := range f.SessionUsage {
		for _, field := range []string{"turns", "inputPerTurnMean", "inputPerTurnMax", "largestToolResults"} {
			if _, ok := c.Input[field]; ok {
				t.Errorf("usage still carried %q: %+v", field, c.Input)
			}
		}
		if _, ok := c.Input["inputTokens"]; !ok {
			t.Errorf("usage lost the rest of the report: %+v", c.Input)
		}
	}
	checkNoOldUsage(t, f)
}

// A build that ends in failWorkItem has nowhere to carry its report: the
// record is in the log, without the once-per-run line (the platform has
// the ledger; the fail command just takes no usage).
func TestFailedBuildLogsItsUsage(t *testing.T) {
	f := seed(t)
	issueCredential(f, item1)
	h := &fakeHarness{fail: true}
	r, _, item, row := runnerOnFake(t, f, h, nil)
	var logs syncBuf
	r.Log = log.New(&logs, "", 0)
	r.Credentials = usageCredentials(r.GW.(CredentialGateway), r.Log)
	if err := r.RunItem(context.Background(), item); err == nil {
		t.Fatal("a failed session is an error")
	}
	if len(f.CallsNamed("failWorkItem")) != 1 {
		t.Fatalf("item: %v", row(item1))
	}
	out := logs.String()
	recs := loggedUsage(t, out)
	if len(recs) != 1 || recs[0].Label != "item ship-it/1" || recs[0].Kind != "build" {
		t.Fatalf("usage records = %+v:\n%s", recs, out)
	}
	rep := recs[0].Report
	if rep.CredentialID != "cred-1" || rep.SessionID != "s-1" || rep.ListCostCents != 75 {
		t.Errorf("record = %+v", rep)
	}
	if strings.Contains(out, "takes no usage report") || strings.Contains(out, issuedSecret) {
		t.Errorf("log:\n%s", out)
	}
	if len(f.SessionUsage) != 0 {
		t.Errorf("usage = %+v", f.SessionUsage)
	}
	checkNoOldUsage(t, f)
}

// credsGW answers the claim-time read with a fixed list.
type credsGW struct {
	issued []gateway.Credential
}

func (g *credsGW) Session(context.Context, gateway.SessionRef, string) (*gateway.Session, error) {
	return &gateway.Session{Credentials: g.issued}, nil
}
func (g *credsGW) RecordToolUsage(context.Context, string, string, string, gateway.UsageReport) error {
	return nil
}

// An api credential whose daily or monthly cap is spent is never
// chosen — a hard stop — and one with room holds its session to what the
// cap leaves; with only spent keys the session has no account.
func TestSpentAPICapIsNotChosen(t *testing.T) {
	spentDay := gateway.Credential{ID: "k1", Label: "key-day", Kind: AccountAPI, Secret: "sk-ant-api-DAY", CapDailyUSD: 20, SpentDailyUSD: 20}
	spentMonth := gateway.Credential{ID: "k2", Label: "key-month", Kind: AccountAPI, Secret: "sk-ant-api-MONTH", SpentMonthlyUSD: DefaultCapMonthlyUSD}
	roomy := gateway.Credential{ID: "k3", Label: "key-room", Kind: AccountAPI, Secret: "sk-ant-api-ROOM", CapDailyUSD: 50, SpentDailyUSD: 48}
	var logs syncBuf
	ref := gateway.SessionRef{Kind: "build", ItemID: item1}

	gw := &credsGW{issued: []gateway.Credential{spentDay, spentMonth, roomy}}
	c := &Credentials{GW: gw, Log: log.New(&logs, "", 0)}
	s, err := c.Open(context.Background(), ref, "n", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Cleanup()
	if s.Label != "key-room" {
		t.Fatalf("chose %q, want key-room", s.Label)
	}
	job := harness.Job{MaxBudgetUSD: 5}
	s.Apply(&job)
	if v, _ := envOf(job.Env, "ANTHROPIC_API_KEY"); v != roomy.Secret {
		t.Errorf("ANTHROPIC_API_KEY = %q", v)
	}
	if job.MaxBudgetUSD != 2 {
		t.Errorf("MaxBudgetUSD = %v, want the $2 the daily cap leaves", job.MaxBudgetUSD)
	}

	gw.issued = []gateway.Credential{spentDay, spentMonth}
	if _, err := c.Open(context.Background(), ref, "n", ""); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("Open with every key at its cap = %v, want ErrNoAccount", err)
	}
	for _, sec := range []string{spentDay.Secret, spentMonth.Secret, roomy.Secret} {
		if strings.Contains(logs.String(), sec) {
			t.Errorf("a secret is in the log:\n%s", logs.String())
		}
	}
	if CapRoom(gateway.Credential{Kind: AccountAPI}) != DefaultCapDailyUSD {
		t.Errorf("a key with no caps has the $50 default day")
	}
}

// A build none of whose issued credentials has room is released, not
// failed: it waits for a later tick or another node. The platform leaves
// an api key at its cap out of the read (runsheet ADR 019), so room is
// the node's own reading: here the one issued subscription is stopped
// until its window resets.
func TestBuildWithNoIssuedHeadroomIsReleased(t *testing.T) {
	f := seed(t)
	f.SessionCredentials[item1] = []map[string]any{{"id": "c1", "label": "max-stopped", "vendor": "anthropic", "kind": "subscription", "serves": []any{"claude-code"}, "secret": "sk-ant-oat01-STOPPED"}}
	h := &streamHarness{file: usageStream()}
	r, _, item, row := runnerOnFake(t, f, h, nil)
	limits := &Limits{}
	limits.Observe("max-stopped", &harness.RateLimit{Status: "rejected",
		FiveHour: harness.RateLimitWindow{Utilization: 1, ResetsAt: time.Now().Add(time.Hour)},
		SevenDay: harness.RateLimitWindow{Utilization: 0.9, ResetsAt: time.Now().Add(48 * time.Hour)}})
	r.Credentials = &Credentials{GW: r.GW.(CredentialGateway), Limits: limits, Log: r.Log}
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if len(h.Jobs()) != 0 {
		t.Fatal("a session ran on a stopped account")
	}
	if row(item1)["status"] != "ready" || len(f.CallsNamed("failWorkItem")) != 0 {
		t.Fatalf("item: %v, fails %d", row(item1), len(f.CallsNamed("failWorkItem")))
	}
}

// Every anthropic/claude-code credential the claim-time read hands a
// session is remembered in Known — label, kind, id, first-issued first —
// before the api-cap filter, so a capped key is still known; Secret()
// on a known account always errors, since only the claim-time read ever
// holds the secret.
func TestOpenRemembersEveryIssuedCredential(t *testing.T) {
	gw := &credsGW{issued: []gateway.Credential{
		{ID: "cred-a", Label: "max20", Kind: AccountSubscription, Secret: "sk-ant-oat01-MAX20"},
		{ID: "cred-b", Label: "shhemail", Kind: AccountSubscription, Secret: "sk-ant-oat01-SHHEMAIL"},
		{ID: "cred-c", Label: "key-1", Kind: AccountAPI, Secret: "sk-ant-api-KEY1", CapDailyUSD: 20, SpentDailyUSD: 20},
	}}
	known, err := OpenKnownAccounts(filepath.Join(t.TempDir(), "accounts.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	c := &Credentials{GW: gw, Known: known, Now: func() time.Time { return now }, Log: log.New(io.Discard, "", 0)}
	ref := gateway.SessionRef{Kind: "build", ItemID: item1}

	// key-1 is at its cap, so Open still runs (on one of the two
	// subscriptions), but all three land in Known.
	s, err := c.Open(context.Background(), ref, "n", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Cleanup()

	accts := known.Accounts(now)
	want := []struct{ label, kind, id string }{
		{"max20", AccountSubscription, "cred-a"},
		{"shhemail", AccountSubscription, "cred-b"},
		{"key-1", AccountAPI, "cred-c"},
	}
	if len(accts) != len(want) {
		t.Fatalf("known accounts = %+v, want %d", accts, len(want))
	}
	for i, w := range want {
		if accts[i].Label != w.label || accts[i].Kind != w.kind || accts[i].ID != w.id {
			t.Errorf("account %d = %+v, want %+v", i, accts[i], w)
		}
		if _, err := accts[i].Secret(); err == nil {
			t.Errorf("account %d (%s): Secret() = nil error, want one", i, accts[i].Label)
		}
	}
}

// unsupportedGW answers every Session read as the platform not knowing
// the operation (gateway.IsUnsupported): Open falls back to
// ErrNoCredential but never reconciles on it.
type unsupportedGW struct{}

func (unsupportedGW) Session(context.Context, gateway.SessionRef, string) (*gateway.Session, error) {
	return nil, &gateway.Error{Unsupported: true}
}
func (unsupportedGW) RecordToolUsage(context.Context, string, string, string, gateway.UsageReport) error {
	return nil
}

// Open on a read issuing A, then a read for the same adapter and
// project issuing only B, leaves A withdrawn — omitted by Known.Accounts
// — and logs it (adr-005's amendment 2026-10-03, a withdrawn account
// leaves the report).
func TestOpenWithdrawsALabelAbsentFromTheNextRead(t *testing.T) {
	gw := &credsGW{issued: []gateway.Credential{{ID: "cred-a", Label: "A", Kind: AccountSubscription, Secret: "sk-ant-oat01-A"}}}
	known, err := OpenKnownAccounts(filepath.Join(t.TempDir(), "accounts.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var logs syncBuf
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	c := &Credentials{GW: gw, Known: known, Now: func() time.Time { return now }, Log: log.New(&logs, "", 0)}
	ctx := withSubject(context.Background(), Scope{Login: "acme", Slug: "acme"}, "proj-1", "mesh")
	ref := gateway.SessionRef{Kind: "build", ItemID: item1}

	s, err := c.Open(ctx, ref, "n", "")
	if err != nil {
		t.Fatal(err)
	}
	s.Cleanup()

	gw.issued = []gateway.Credential{{ID: "cred-b", Label: "B", Kind: AccountSubscription, Secret: "sk-ant-oat01-B"}}
	s, err = c.Open(ctx, ref, "n", "")
	if err != nil {
		t.Fatal(err)
	}
	s.Cleanup()

	accts := known.Accounts(now)
	if len(accts) != 1 || accts[0].Label != "B" {
		t.Fatalf("accounts = %+v, want only B", accts)
	}
	if !strings.Contains(logs.String(), "accounts: A withdrawn (not issued by the mesh read)") {
		t.Errorf("log missing the withdrawal line:\n%s", logs.String())
	}
}

// An unsupported read (gateway.IsUnsupported) withdraws nothing: the
// platform named nothing about what is still issued.
func TestOpenOnUnsupportedReadWithdrawsNothing(t *testing.T) {
	known, err := OpenKnownAccounts(filepath.Join(t.TempDir(), "accounts.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	if err := known.NoteFor(AccountConfig{Label: "A", Kind: AccountSubscription, ID: "cred-a"}, "proj-1", now); err != nil {
		t.Fatal(err)
	}
	c := &Credentials{GW: unsupportedGW{}, Known: known, Now: func() time.Time { return now }, Log: log.New(io.Discard, "", 0)}
	ctx := withSubject(context.Background(), Scope{Login: "acme", Slug: "acme"}, "proj-1", "mesh")
	ref := gateway.SessionRef{Kind: "build", ItemID: item1}

	if _, err := c.Open(ctx, ref, "n", ""); !isNoCredential(err) {
		t.Fatalf("Open = %v, want ErrNoCredential", err)
	}
	accts := known.Accounts(now)
	if len(accts) != 1 || accts[0].Label != "A" {
		t.Fatalf("accounts = %+v, want A untouched", accts)
	}
}

// The store's file on disk, after an Open that issued a secret, never
// carries that secret.
func TestKnownAccountsFileHasNoSecret(t *testing.T) {
	const secret = "sk-ant-oat01-SECRET-VALUE"
	gw := &credsGW{issued: []gateway.Credential{{ID: "cred-a", Label: "max20", Kind: AccountSubscription, Secret: secret}}}
	path := filepath.Join(t.TempDir(), "accounts.json")
	known, err := OpenKnownAccounts(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &Credentials{GW: gw, Known: known, Log: log.New(io.Discard, "", 0)}
	ref := gateway.SessionRef{Kind: "build", ItemID: item1}
	s, err := c.Open(context.Background(), ref, "n", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Cleanup()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) {
		t.Errorf("the secret is in the store's file:\n%s", data)
	}
	var file struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Accounts) != 1 || file.Accounts[0]["label"] != "max20" {
		t.Fatalf("file = %+v", file.Accounts)
	}
}

// A credential printed any way leaves its secret out.
func TestCredentialPrintsNoSecret(t *testing.T) {
	cr := gateway.Credential{ID: "c", Label: "max", Kind: AccountSubscription, Secret: issuedSecret}
	a := IssuedAccount(cr)
	for _, s := range []string{
		fmtAll(cr), fmtAll([]gateway.Credential{cr}), fmtAll(a), fmtAll([]AccountConfig{a}),
	} {
		if strings.Contains(s, issuedSecret) {
			t.Errorf("printed the secret: %s", s)
		}
	}
	if got, _ := a.Secret(); got != issuedSecret {
		t.Errorf("Secret = %q", got)
	}
}

func fmtAll(v any) string { return fmt.Sprintf("%v %+v %#v %s", v, v, v, v) }

// A credential's priorities and windowTargets become the account's
// Priorities and its own pace point (adr-005's amendment 2026-10-02): a
// five_hour windowTarget of 50% is PaceFiveHour 0.5, a seven_day one
// PaceSevenDay, and an unknown window is ignored. No windowTargets is
// the node-wide rule, 0.
func TestIssuedAccountCarriesPrioritiesAndPace(t *testing.T) {
	cr := gateway.Credential{ID: "c1", Label: "max20", Kind: AccountSubscription,
		Priorities:    []string{"urgent", "high"},
		WindowTargets: []gateway.WindowTarget{{Window: "five_hour", Percent: 50}, {Window: "unknown", Percent: 90}}}
	a := IssuedAccount(cr)
	if want := []string{"urgent", "high"}; len(a.Priorities) != 2 || a.Priorities[0] != want[0] || a.Priorities[1] != want[1] {
		t.Errorf("Priorities = %v, want %v", a.Priorities, want)
	}
	if a.PaceFiveHour != 0.5 {
		t.Errorf("PaceFiveHour = %v, want 0.5", a.PaceFiveHour)
	}
	if a.PaceSevenDay != 0 {
		t.Errorf("PaceSevenDay = %v, want 0", a.PaceSevenDay)
	}

	plain := IssuedAccount(gateway.Credential{ID: "c2", Label: "shhemail", Kind: AccountSubscription})
	if len(plain.Priorities) != 0 {
		t.Errorf("Priorities = %v, want none", plain.Priorities)
	}
	if plain.PaceFiveHour != 0 || plain.PaceSevenDay != 0 {
		t.Errorf("pace = %v/%v, want 0/0", plain.PaceFiveHour, plain.PaceSevenDay)
	}
}

// mesh node start logs each known issued account with its kind, what it
// serves and its own pace point, so a restart's node.log names a
// reserved subscription before its first claim.
func TestIssuedAccountLines(t *testing.T) {
	lines := IssuedAccountLines([]AccountConfig{
		{Label: "max20", Kind: AccountSubscription, Priorities: []string{"urgent", "high"}, PaceFiveHour: 0.5},
		{Label: "shhemail", Kind: AccountSubscription},
	})
	if len(lines) != 2 {
		t.Fatalf("lines = %v, want 2", lines)
	}
	if !strings.Contains(lines[0], "accounts: issued max20") || !strings.Contains(lines[0], "serves urgent, high") || !strings.Contains(lines[0], "50% five-hour") {
		t.Errorf("lines[0] = %q", lines[0])
	}
	if !strings.Contains(lines[1], "accounts: issued shhemail") || !strings.Contains(lines[1], "serves all") || !strings.Contains(lines[1], "node-wide") {
		t.Errorf("lines[1] = %q", lines[1])
	}
}

// A build's usage report carries what its context cost on disk
// (contextBytes, adr-016); a platform whose usage input has no such
// field takes the rest of every report, Carried treats
// ErrNoContextUsage as landed, and the node says so once per run.
func TestCredentialsCarryContextBytesAndFallBackOncePerRun(t *testing.T) {
	for _, old := range []bool{false, true} {
		f := seed(t)
		issueCredential(f, item1)
		f.NoContextUsage = old
		h := &streamHarness{file: usageStream()}
		r, _, item, row := runnerOnFake(t, f, h, nil)
		var logs syncBuf
		r.Log = log.New(&logs, "", 0)
		r.Credentials = usageCredentials(r.GW.(CredentialGateway), r.Log)

		items := []gateway.WorkItem{item}
		id := "99999999-2222-4222-8222-999999999991"
		f.WorkItems[id] = map[string]any{"id": id, "goalId": goalID, "orgId": orgID, "seq": 3, "title": "More",
			"brief": "More.", "acceptance": "go test passes", "repoId": repoID, "baseRef": "main", "status": "claimed", "nodeId": r.Cfg.NodeID, "dependsOn": []string{}}
		f.GitHubTokens[id] = f.GitHubTokens[item1]
		issueCredential(f, id)
		it := item
		it.ID, it.Seq = id, 3
		items = append(items, it)
		for _, it := range items {
			if err := r.RunItem(context.Background(), it); err != nil {
				t.Fatalf("old=%v item %s: %v", old, it.ID, err)
			}
			if row(it.ID)["status"] != "evaluating" {
				t.Fatalf("old=%v item %s: %v", old, it.ID, row(it.ID))
			}
		}
		out := logs.String()
		notice := "usage: the platform takes no context bytes on the usage report yet; sent without them (said once, for every session this run)"
		if n, want := strings.Count(out, notice), map[bool]int{false: 0, true: 1}[old]; n != want {
			t.Errorf("old=%v: notice said %d times, want %d:\n%s", old, n, want, out)
		}
		if strings.Contains(out, "the platform's finishing command takes no usage report") {
			t.Errorf("old=%v: the wrong fallback fired:\n%s", old, out)
		}
		if len(f.SessionUsage) != len(items) {
			t.Fatalf("old=%v: SessionUsage = %+v, want %d landed reports", old, f.SessionUsage, len(items))
		}
		for _, c := range f.SessionUsage {
			b, ok := c.Input["contextBytes"]
			if old && ok {
				t.Errorf("an old platform's usage carried contextBytes: %+v", c.Input)
			}
			if !old && (!ok || b.(float64) <= 0) {
				t.Errorf("usage.contextBytes = %v: %+v", b, c.Input)
			}
		}
	}
}

// loggedUsage is every `<label>: usage <kind> {…}` record in a node.log
// written without timestamps, in order; a record that is not one JSON
// report fails the test.
func loggedUsage(t *testing.T, out string) []UsageRecord {
	t.Helper()
	var recs []UsageRecord
	for _, line := range strings.Split(out, "\n") {
		m := usageRecordLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		rec := UsageRecord{Label: m[1], Kind: m[2], Priority: m[3]}
		if m[4] != "" {
			rec.Evaluators, _ = strconv.Atoi(m[4])
		}
		if err := json.Unmarshal([]byte(m[5]), &rec.Report); err != nil {
			t.Fatalf("usage line is not one JSON report: %q: %v", line, err)
		}
		recs = append(recs, rec)
	}
	return recs
}

// A report its finishing command carried (handedIn) is still logged,
// once, on the session's label with its kind: a build, an evaluation
// and a plan each give one `<label>: usage <kind> {…}` line whose JSON
// is the report as sent.
func TestFinishLogsTheCarriedRecordOnItsLabel(t *testing.T) {
	for _, tc := range []struct {
		label, kind, want string
	}{
		{"item ship-it/1", "build", "item ship-it/1: usage build "},
		{"item ship-it/1", "evaluate", "item ship-it/1: usage evaluate "},
		{"goal ship-it", "plan", "goal ship-it: usage plan "},
	} {
		var logs strings.Builder
		t0 := time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)
		c := &Credentials{Log: log.New(&logs, "", 0), Now: func() time.Time { return t0 }}
		s := &SessionAccount{creds: c, account: AccountConfig{Label: "max-a", Kind: AccountSubscription}, started: t0}
		s.SetContextBytes(4096)
		s.SetLabel(tc.label, tc.kind)
		s.Report(harness.Result{SessionID: "s-1", CostUSD: 1.25, Usage: &harness.Usage{Model: "claude-opus-5", InputTokens: 100000, CachedInputTokens: 90000, OutputTokens: 700}})
		if err := s.Carried(nil); err != nil {
			t.Fatal(err)
		}
		if !s.handedIn {
			t.Fatal("the report did not ride its finish")
		}
		s.Finish(context.Background(), harness.Result{})
		var records []string
		for _, l := range strings.Split(logs.String(), "\n") {
			if strings.Contains(l, ": usage "+tc.kind+" ") || strings.HasPrefix(l, "usage: {") {
				records = append(records, l)
			}
		}
		if len(records) != 1 || !strings.HasPrefix(records[0], tc.want) {
			t.Fatalf("%s: records %q, want one starting %q:\n%s", tc.kind, records, tc.want, logs.String())
		}
		want := `{"sessionId":"s-1","adapter":"claude-code","model":"claude-opus-5","credentialKind":"subscription","inputTokens":100000,"outputTokens":700,"cacheReadTokens":90000,"cacheWriteTokens":0,"toolCalls":0,"wallMs":0,"listCostCents":125,"actualCostCents":0,"contextBytes":4096}`
		if got := strings.TrimPrefix(records[0], tc.want); got != want {
			t.Errorf("%s: record\n%s\nwant\n%s", tc.kind, got, want)
		}
		if !strings.HasPrefix(logs.String(), "usage: turns=0 input/turn mean=0 max=0 largest=, 0 documents fetched\n") {
			t.Errorf("%s: the turns line changed:\n%s", tc.kind, logs.String())
		}
	}
}

// A platform refusing both `documentsFetched` and `toolNames`
// (ErrNoDocumentsFetched, ErrNoToolNames, possibly joined into one
// error as mutateWithUsage does) is not a finishing failure: Carried
// treats it as landed, and Finish logs each fallback once per run, not
// once per session (adr-016's amendment 2026-10-02).
func TestCarriedTreatsNoDocumentsFetchedAndNoToolNamesAsLandedAndLogsOncePerRun(t *testing.T) {
	var logs strings.Builder
	c := &Credentials{Log: log.New(&logs, "", 0)}
	joined := errors.Join(gateway.ErrNoDocumentsFetched, gateway.ErrNoToolNames)
	for i := range 2 {
		s := &SessionAccount{creds: c}
		s.Report(harness.Result{SessionID: fmt.Sprintf("s-%d", i), DocumentsFetched: 3, ToolNames: map[string]int{"runsheet__mesh_document": 3}})
		if err := s.Carried(joined); err != nil {
			t.Fatalf("session %d: Carried = %v, want nil", i, err)
		}
		if !s.handedIn || !s.noDocumentsFetched || !s.noToolNames {
			t.Fatalf("session %d: handedIn=%v noDocumentsFetched=%v noToolNames=%v", i, s.handedIn, s.noDocumentsFetched, s.noToolNames)
		}
		s.Finish(context.Background(), harness.Result{})
	}
	out := logs.String()
	for _, notice := range []string{
		"usage: the platform takes no documents-fetched count on the usage report yet; sent without it (said once, for every session this run)",
		"usage: the platform takes no per-tool call names on the usage report yet; sent without them (said once, for every session this run)",
	} {
		if n := strings.Count(out, notice); n != 1 {
			t.Errorf("notice %q said %d times, want 1:\n%s", notice, n, out)
		}
	}
}

// ErrNoDocumentsFetched or ErrNoToolNames joined with ErrNoContextUsage
// (a platform older than both context-budget-that-holds and
// context-is-fetched-not-shipped, mutateWithUsage joining every loss)
// is a landed command too: Carried returns nil and marks every loss,
// and the node's logger says each once per run.
func TestCarriedTreatsLossesJoinedWithNoContextUsageAsLanded(t *testing.T) {
	const (
		docsNotice    = "usage: the platform takes no documents-fetched count on the usage report yet; sent without it (said once, for every session this run)"
		namesNotice   = "usage: the platform takes no per-tool call names on the usage report yet; sent without them (said once, for every session this run)"
		contextNotice = "usage: the platform takes no context bytes on the usage report yet; sent without them (said once, for every session this run)"
	)
	for _, tc := range []struct {
		name       string
		err        error
		docs, tool bool
		notices    []string
	}{
		{"documentsFetched", gateway.ErrNoDocumentsFetched, true, false, []string{docsNotice}},
		{"toolNames", gateway.ErrNoToolNames, false, true, []string{namesNotice}},
		{"documentsFetched+contextBytes", errors.Join(gateway.ErrNoContextUsage, gateway.ErrNoDocumentsFetched), true, false, []string{docsNotice, contextNotice}},
		{"toolNames+contextBytes", errors.Join(gateway.ErrNoContextUsage, gateway.ErrNoToolNames), false, true, []string{namesNotice, contextNotice}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs strings.Builder
			c := &Credentials{Log: log.New(&logs, "", 0)}
			for i := range 3 {
				s := &SessionAccount{creds: c}
				s.Report(harness.Result{SessionID: fmt.Sprintf("s-%d", i), DocumentsFetched: 3, ToolNames: map[string]int{"runsheet__mesh_document": 2, "runsheet__mesh_search": 1}})
				if err := s.Carried(tc.err); err != nil {
					t.Fatalf("session %d: Carried = %v, want nil (landed)", i, err)
				}
				if !s.handedIn || s.noDocumentsFetched != tc.docs || s.noToolNames != tc.tool {
					t.Fatalf("session %d: handedIn=%v noDocumentsFetched=%v noToolNames=%v", i, s.handedIn, s.noDocumentsFetched, s.noToolNames)
				}
				s.Finish(context.Background(), harness.Result{})
			}
			out := logs.String()
			for _, notice := range tc.notices {
				if n := strings.Count(out, notice); n != 1 {
					t.Errorf("notice %q said %d times over 3 sessions, want 1:\n%s", notice, n, out)
				}
			}
			if !tc.docs && strings.Contains(out, docsNotice) || !tc.tool && strings.Contains(out, namesNotice) {
				t.Errorf("a loss not refused was logged:\n%s", out)
			}
		})
	}
}

// A session with no label (SetLabel never called) keeps the older
// `usage: {…}` line.
func TestFinishWithoutALabelLogsTheOlderLine(t *testing.T) {
	var logs strings.Builder
	c := &Credentials{Log: log.New(&logs, "", 0)}
	s := &SessionAccount{creds: c}
	s.Report(harness.Result{SessionID: "s-1"})
	s.Finish(context.Background(), harness.Result{})
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[1], `usage: {"sessionId":"s-1",`) {
		t.Fatalf("log:\n%s", logs.String())
	}
}

// Every kind of session — build, evaluate, rebase, plan, research — runs
// on a credential the claim-time read issued, so its usage report names
// that credential and its kind: never an empty CredentialID nor
// credentialKind unset, the 2,027 planner launches of 2026-09-29.
func TestEveryKindsUsageReportNamesItsCredential(t *testing.T) {
	const direction = "direction:mesh:night"
	f := seed(t)
	issueNothing(f)
	f.SessionCredentials[item1] = []map[string]any{{"id": "cred-item", "label": "max-item", "vendor": "anthropic", "kind": "subscription", "serves": []any{"claude-code"}, "secret": "sk-ant-oat01-ITEM"}}
	f.SessionCredentials[goalID] = []map[string]any{{"id": "cred-goal", "label": "key-goal", "vendor": "anthropic", "kind": "api_key", "serves": []any{"claude-code"}, "secret": "sk-ant-api03-GOAL"}}
	f.SessionCredentials[direction] = []map[string]any{{"id": "cred-dir", "label": "max-dir", "vendor": "anthropic", "kind": "subscription", "serves": []any{"claude-code"}, "secret": "sk-ant-oat01-DIR"}}
	c := &Credentials{GW: gateway.New(f.URL(), "rs_test"), Log: log.New(io.Discard, "", 0)}
	ctx := withSubject(context.Background(), Scope{Login: "acme", Slug: "acme", ID: orgID}, projID, "mesh")
	for _, tc := range []struct {
		ref      gateway.SessionRef
		id, kind string
	}{
		{gateway.SessionRef{Kind: string(harness.KindBuild), ItemID: item1, GoalID: goalID}, "cred-item", gateway.CredentialSubscription},
		{gateway.SessionRef{Kind: string(harness.KindEvaluate), ItemID: item1, GoalID: goalID}, "cred-item", gateway.CredentialSubscription},
		{gateway.SessionRef{Kind: string(harness.KindRebase), ItemID: item1, GoalID: goalID}, "cred-item", gateway.CredentialSubscription},
		{gateway.SessionRef{Kind: string(harness.KindPlan), GoalID: goalID}, "cred-goal", gateway.CredentialAPI},
		{gateway.SessionRef{Kind: string(harness.KindResearch), DirectionID: direction}, "cred-dir", gateway.CredentialSubscription},
	} {
		acct, err := c.Open(ctx, tc.ref, "n", AdapterClaudeCode)
		if err != nil {
			t.Fatalf("%s: %v", tc.ref.Kind, err)
		}
		rep := acct.Report(harness.Result{SessionID: "s-" + tc.ref.Kind})
		acct.Cleanup()
		if rep == nil || rep.CredentialID == "" || rep.CredentialKind == gateway.CredentialUnset {
			t.Fatalf("%s: report %+v names no credential", tc.ref.Kind, rep)
		}
		if rep.CredentialID != tc.id || rep.CredentialKind != tc.kind {
			t.Errorf("%s: credential %s/%s, want %s/%s", tc.ref.Kind, rep.CredentialID, rep.CredentialKind, tc.id, tc.kind)
		}
	}
}
