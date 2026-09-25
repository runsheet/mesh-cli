package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
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
}

func (h *streamHarness) Name() string { return "claude-code" }
func (h *streamHarness) Run(_ context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
	h.mu.Lock()
	h.jobs = append(h.jobs, job)
	h.mu.Unlock()
	f, err := os.Open(h.file)
	if err != nil {
		return harness.Result{}, err
	}
	defer f.Close()
	res := harness.ParseStream(f, nil)
	if h.text != "" {
		res.Text = h.text
	}
	return res, nil
}

func (h *streamHarness) Jobs() []harness.Job {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]harness.Job(nil), h.jobs...)
}

func usageStream() string { return filepath.Join("..", "harness", "testdata", "stream_usage.jsonl") }

// When the platform issues a subscription credential for the claimed
// item, the build runs on it — CLAUDE_CODE_OAUTH_TOKEN from the platform,
// node.yaml not consulted — and one recordSessionUsage carries its id and
// label, the tokens and cost the stream reported and the windows before
// and after; the secret is in the session's env and nowhere else.
func TestBuildRunsOnAnIssuedCredentialAndRecordsUsage(t *testing.T) {
	f := seed(t)
	f.SessionCredentials[item1] = []map[string]any{{"id": "cred-1", "label": "max-cloud", "kind": "subscription", "secret": issuedSecret}}
	h := &streamHarness{file: usageStream()}
	r, _, item, row := runnerOnFake(t, f, h, nil)
	var logs syncBuf
	r.Log = log.New(&logs, "", 0)
	// node.yaml names an account whose token file does not exist: if it
	// were consulted the session could not open
	r.Accounts = []AccountConfig{{Label: "max-local", Kind: AccountSubscription, TokenFile: "/nope/token"}}
	limits := &Limits{}
	limits.Observe("max-cloud", &harness.RateLimit{Status: "allowed",
		FiveHour: harness.RateLimitWindow{Utilization: 0.2, ResetsAt: time.Unix(1790330400, 0).UTC()},
		SevenDay: harness.RateLimitWindow{Utilization: 0.1, ResetsAt: time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)}})
	r.Credentials = &Credentials{GW: r.GW.(CredentialGateway), Limits: limits, Log: r.Log}

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
	if reads := f.CallsNamed("sessionCredentials"); len(reads) != 1 || reads[0].Input["workItemId"] != item1 || reads[0].Input["kind"] != "build" || reads[0].Input["nodeId"] != r.Cfg.NodeID {
		t.Fatalf("sessionCredentials calls: %+v", reads)
	}
	if row(item1)["status"] != "evaluating" {
		t.Fatalf("item: %v", row(item1))
	}

	usage := f.CallsNamed("recordSessionUsage")
	if len(usage) != 1 {
		t.Fatalf("recordSessionUsage calls: %+v", usage)
	}
	in := usage[0].Input
	for k, want := range map[string]any{
		"kind": "build", "workItemId": item1, "goalId": goalID, "nodeId": r.Cfg.NodeID, "sessionId": "s-usage",
		"credentialId": "cred-1", "credentialLabel": "max-cloud", "credentialKind": "subscription", "model": "claude-sonnet-5",
		"inputTokens": float64(60600), "outputTokens": float64(789), "cacheReadTokens": float64(56000), "cacheWriteTokens": float64(3400),
		"costUsd": 0.4321,
	} {
		if in[k] != want {
			t.Errorf("usage %s = %v, want %v", k, in[k], want)
		}
	}
	window := func(side, w string) any {
		m, _ := in[side].(map[string]any)
		win, _ := m[w].(map[string]any)
		return win["utilization"]
	}
	if window("before", "fiveHour") != 0.2 || window("before", "sevenDay") != 0.1 {
		t.Errorf("before = %v", in["before"])
	}
	if window("after", "fiveHour") != 0.52 || window("after", "sevenDay") != 0.31 {
		t.Errorf("after = %v", in["after"])
	}
	if in["startedAt"] == nil || in["endedAt"] == nil {
		t.Errorf("no times: %v", in)
	}

	// the secret is in the session's env and nowhere else
	for _, c := range f.Calls {
		b, _ := json.Marshal(c)
		if strings.Contains(string(b), issuedSecret) {
			t.Errorf("the secret is in %s's payload: %s", c.Name, b)
		}
	}
	for what, s := range map[string]string{"log": logs.String(), "lastNote": toString(row(item1)["lastNote"]), "lastError": toString(row(item1)["lastError"])} {
		if strings.Contains(s, issuedSecret) {
			t.Errorf("the secret is in the %s: %s", what, s)
		}
	}
}

// Against a platform without the claim-time read or the ledger, a node
// running several items uses node.yaml's accounts, says each fallback
// once for the run, and writes each session's usage record as one log
// line; the secret is in no log line.
func TestCredentialsFallBackToNodeYAMLOncePerRun(t *testing.T) {
	const local = "sk-ant-oat01-LOCAL-token-file"
	tokFile := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(tokFile, []byte(local+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := seed(t)
	f.Unsupported["sessionCredentials"] = true
	f.Unsupported["recordSessionUsage"] = true
	h := &streamHarness{file: usageStream()}
	r, _, item, row := runnerOnFake(t, f, h, nil)
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
	for _, job := range h.Jobs() {
		if v, _ := envOf(job.Env, "CLAUDE_CODE_OAUTH_TOKEN"); v != local || Account(job.Env) != "max-a" {
			t.Fatalf("session not on node.yaml's account: %v", job.Env)
		}
	}
	out := logs.String()
	if n := strings.Count(out, "the platform issues no session credentials"); n != 1 {
		t.Errorf("credential fallback said %d times:\n%s", n, out)
	}
	if n := strings.Count(out, "the platform keeps no usage ledger"); n != 1 {
		t.Errorf("ledger fallback said %d times:\n%s", n, out)
	}
	var records []gateway.UsageRecord
	for _, line := range strings.Split(out, "\n") {
		if js, ok := strings.CutPrefix(line, "usage: {"); ok {
			var rec gateway.UsageRecord
			if err := json.Unmarshal([]byte("{"+js), &rec); err != nil {
				t.Fatalf("usage line is not one JSON record: %q: %v", line, err)
			}
			records = append(records, rec)
		}
	}
	if len(records) != len(items) {
		t.Fatalf("%d usage lines for %d sessions:\n%s", len(records), len(items), out)
	}
	for i, rec := range records {
		if rec.Kind != "build" || rec.ItemID != items[i].ID || rec.CredentialLabel != "max-a" || rec.CredentialID != "" ||
			rec.InputTokens != 60600 || rec.OutputTokens != 789 || rec.CostUSD != 0.4321 || rec.After == nil || rec.After.FiveHour.Utilization != 0.52 {
			t.Errorf("record %d = %+v", i, rec)
		}
	}
	if strings.Contains(out, local) {
		t.Errorf("the secret is in the log:\n%s", out)
	}
	if len(f.CallsNamed("recordSessionUsage")) != 0 {
		t.Error("an unsupported mutation was recorded")
	}
}

// An evaluation reports its usage record too, as kind evaluate.
func TestEvaluationRecordsUsage(t *testing.T) {
	f := seed(t)
	h := &streamHarness{file: usageStream()}
	r, ws, item, _ := runnerOnFake(t, f, h, nil)
	creds := &Credentials{GW: r.GW.(CredentialGateway), Limits: &Limits{}, Log: r.Log}
	r.Credentials = creds
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	h.text = "Fine.\nVERDICT: PASS"
	ev := &Evaluator{GW: r.GW, Harness: h, WS: ws, Scopes: r.Scopes, WorkRoot: ws.root, Credentials: creds, Log: r.Log}
	item.Branch = "mesh/ship-it-1"
	if err := ev.Evaluate(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	usage := f.CallsNamed("recordSessionUsage")
	if len(usage) != 2 || usage[0].Input["kind"] != "build" || usage[1].Input["kind"] != "evaluate" || usage[1].Input["workItemId"] != item1 {
		t.Fatalf("usage records: %+v", usage)
	}
	if usage[1].Input["credentialLabel"] != DefaultAccount {
		t.Errorf("the operator's own login records as %v, want %s", usage[1].Input["credentialLabel"], DefaultAccount)
	}
}

// credsGW answers the claim-time read with a fixed list.
type credsGW struct {
	issued  []gateway.Credential
	records []gateway.UsageRecord
}

func (g *credsGW) SessionCredentials(context.Context, gateway.SessionRef, string) ([]gateway.Credential, error) {
	return g.issued, nil
}
func (g *credsGW) RecordSessionUsage(_ context.Context, rec gateway.UsageRecord) error {
	g.records = append(g.records, rec)
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
	s, err := c.Open(context.Background(), ref, "n", nil)
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
	if _, err := c.Open(context.Background(), ref, "n", nil); !errors.Is(err, ErrNoAccount) {
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
// failed: it waits for a later tick or another node.
func TestBuildWithNoIssuedHeadroomIsReleased(t *testing.T) {
	f := seed(t)
	f.SessionCredentials[item1] = []map[string]any{{"id": "k1", "label": "key", "kind": "api", "secret": "sk-ant-api-SPENT", "capDailyUsd": 10.0, "spentDailyUsd": 10.0}}
	h := &streamHarness{file: usageStream()}
	r, _, item, row := runnerOnFake(t, f, h, nil)
	r.Credentials = &Credentials{GW: r.GW.(CredentialGateway), Log: r.Log}
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if len(h.Jobs()) != 0 {
		t.Fatal("a session ran on a spent key")
	}
	if row(item1)["status"] != "ready" || len(f.CallsNamed("failWorkItem")) != 0 {
		t.Fatalf("item: %v, fails %d", row(item1), len(f.CallsNamed("failWorkItem")))
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
