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
	// node.yaml names an account whose token file does not exist: if it
	// were consulted the session could not open
	r.Accounts = []AccountConfig{{Label: "max-local", Kind: AccountSubscription, TokenFile: "/nope/token"}}
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
	if strings.Contains(logs.String(), "usage: {") {
		t.Errorf("a report the finish carried is logged too:\n%s", logs.String())
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

// Against a platform without the claim-time read, and whose finishing
// commands take no `usage`, a node running several sessions uses
// node.yaml's accounts, finishes every one — the builds reach
// evaluating, the verdict lands — says each fallback once for the run,
// and writes each session's usage report as one log line; the secret
// is in no log line.
func TestCredentialsFallBackToNodeYAMLOncePerRun(t *testing.T) {
	const local = "sk-ant-oat01-LOCAL-token-file"
	tokFile := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(tokFile, []byte(local+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := seed(t)
	f.Unsupported["itemCredentials"] = true
	f.UnknownFields["usage"] = true
	h := &streamHarness{file: usageStream()}
	r, ws, item, row := runnerOnFake(t, f, h, nil)
	var logs syncBuf
	r.Log = log.New(&logs, "", 0)
	r.Accounts = []AccountConfig{{Label: "max-a", Kind: AccountSubscription, TokenFile: tokFile}}
	r.Credentials = &Credentials{GW: r.GW.(CredentialGateway), Limits: &Limits{}, Log: r.Log}

	items := []gateway.WorkItem{item}
	for i, id := range []string{"99999999-1111-4111-8111-999999999991", "99999999-1111-4111-8111-999999999992"} {
		f.WorkItems[id] = map[string]any{"id": id, "goalId": goalID, "orgId": orgID, "seq": 3 + i, "title": "More",
			"brief": "More.", "acceptance": "go test passes", "repoId": repoID, "baseRef": "main", "status": "claimed", "nodeId": r.Cfg.NodeID, "dependsOn": []string{}}
		f.GitHubTokens[id] = f.GitHubTokens[item1]
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
	ev := &Evaluator{GW: r.GW, Harness: h, WS: ws, Scopes: r.Scopes, WorkRoot: ws.root, Accounts: r.Accounts, Credentials: r.Credentials, Log: r.Log}
	evItem := item
	evItem.Branch = "mesh/ship-it-1"
	if err := ev.Evaluate(context.Background(), evItem); err != nil {
		t.Fatal(err)
	}
	if calls := f.CallsNamed("evaluateWorkItem"); len(calls) != 1 || calls[0].Input["passed"] != true {
		t.Fatalf("verdicts: %+v", calls)
	}
	for _, job := range h.Jobs() {
		if v, _ := envOf(job.Env, "CLAUDE_CODE_OAUTH_TOKEN"); v != local || Account(job.Env) != "max-a" {
			t.Fatalf("session not on node.yaml's account: %v", job.Env)
		}
	}
	out := logs.String()
	if n := strings.Count(out, "the platform issues no session credentials"); n != 1 {
		t.Errorf("credential fallback said %d times:\n%s", n, out)
	}
	if n := strings.Count(out, "usage: the platform's finishing command takes no usage report; each session's record is logged here instead (said once, for every session this run)"); n != 1 {
		t.Errorf("ledger fallback said %d times:\n%s", n, out)
	}
	var reports []gateway.UsageReport
	for _, line := range strings.Split(out, "\n") {
		if js, ok := strings.CutPrefix(line, "usage: {"); ok {
			var rep gateway.UsageReport
			if err := json.Unmarshal([]byte("{"+js), &rep); err != nil {
				t.Fatalf("usage line is not one JSON report: %q: %v", line, err)
			}
			reports = append(reports, rep)
		}
	}
	if len(reports) != len(items)+1 {
		t.Fatalf("%d usage lines for %d sessions:\n%s", len(reports), len(items)+1, out)
	}
	for i, rep := range reports {
		if rep.SessionID != "s-usage" || rep.CredentialID != "" || rep.CredentialKind != "subscription" ||
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
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "usage: {") {
			if line != "" {
				t.Fatalf("two usage lines:\n%s", out)
			}
			line = l
		}
	}
	var rep gateway.UsageReport
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "usage: ")), &rep); err != nil {
		t.Fatalf("no usage record in the log (%v):\n%s", err, out)
	}
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
	s, err := c.Open(context.Background(), ref, "n", nil, "")
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
	if _, err := c.Open(context.Background(), ref, "n", nil, ""); !errors.Is(err, ErrNoAccount) {
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
	s, err := c.Open(context.Background(), ref, "n", nil, "")
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
	s, err := c.Open(context.Background(), ref, "n", nil, "")
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

// A chosen account the platform issued, which node.yaml does not have,
// is the loop's prediction only: OpenAccount never asks it for a secret
// and opens node.yaml's first instead.
func TestOpenAccountPassesOverAChosenIssuedAccount(t *testing.T) {
	tok := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(tok, []byte("tok-own"), 0o600); err != nil {
		t.Fatal(err)
	}
	fallback := []AccountConfig{{Label: "own", Kind: AccountSubscription, TokenFile: tok}}
	ctx := WithAccount(context.Background(), AccountConfig{Label: "max20", Kind: AccountSubscription, ID: "cred-1"})
	acct, err := OpenAccount(ctx, fallback)
	if err != nil {
		t.Fatal(err)
	}
	defer acct.Cleanup()
	if acct.Label != "own" {
		t.Fatalf("opened %q, want own", acct.Label)
	}
	if a := openedAccount(ctx, fallback); a.Label != "own" {
		t.Fatalf("openedAccount = %q, want own", a.Label)
	}
	if acct, err := OpenAccount(ctx, nil); err != nil || acct != nil {
		t.Fatalf("with no node.yaml accounts: %v, %v; want the operator's own login", acct, err)
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
