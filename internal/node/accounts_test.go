package node

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/harness"
)

// issuedWith is a an issued account carrying secret, as the claim-time
// read hands one over (IssuedAccount): the only kind a session opens.
func issuedWith(a AccountConfig, secret string) AccountConfig {
	a.issued = &secret
	return a
}

// A nil account — a test's job with none — changes nothing on a job.
func TestNilAccountSessionChangesNothing(t *testing.T) {
	var acct *AccountSession
	job := harness.Job{Env: []string{"GH_TOKEN=x"}}
	acct.Apply(&job)
	acct.Cleanup()
	if len(job.Env) != 1 || len(job.Unset) != 0 || len(job.Secrets) != 0 {
		t.Fatalf("a nil account changed the job: %+v", job)
	}
}

// An api key's caps default to $50 a day and $1,000 a month; set, they
// are its own. A project's budget is shared by the keys naming it.
func TestAccountsCapsAndBudgets(t *testing.T) {
	budgets := map[string]float64{"mesh-cli": 120}
	accounts := []AccountConfig{
		{Label: "max-a", Kind: AccountSubscription},
		{Label: "key", Kind: AccountAPI},
		{Label: "big", Kind: AccountAPI, DailyCapUSD: 200, MonthlyCapUSD: 3000, Project: "mesh-cli"},
	}
	for _, c := range []struct {
		i          int
		day, month float64
		project    string
		budget     float64
	}{{1, 50, 1000, "", 0}, {2, 200, 3000, "mesh-cli", 120}} {
		a := accounts[c.i]
		if d, m := a.Caps(); d != c.day || m != c.month {
			t.Errorf("%s caps %v/%v, want %v/%v", a.Label, d, m, c.day, c.month)
		}
		got := a.capacity(budgets)
		want := CapacityAccount{Name: a.Label, Kind: AccountAPI, DailyCapUSD: c.day, MonthlyCapUSD: c.month, Project: c.project, ProjectDailyBudgetUSD: c.budget}
		if got != want {
			t.Errorf("%s as the scaler sees it: %+v, want %+v", a.Label, got, want)
		}
	}
	if got := accounts[0].capacity(budgets); got != (CapacityAccount{Name: "max-a", Kind: AccountSubscription}) {
		t.Errorf("subscription as the scaler sees it: %+v", got)
	}
}

// An account's secret is only what the claim-time read issued: one Known
// remembers (no secret) or an empty one is no secret.
func TestAccountSecret(t *testing.T) {
	if s, err := issuedWith(AccountConfig{Label: "a", Kind: AccountSubscription, ID: "c1"}, "sk-ant-oat-123").Secret(); err != nil || s != "sk-ant-oat-123" {
		t.Fatalf("issued secret %q, %v", s, err)
	}
	if _, err := (AccountConfig{Label: "b", Kind: AccountAPI, ID: "c2"}).Secret(); err == nil {
		t.Fatal("a remembered account read as a secret")
	}
	if _, err := issuedWith(AccountConfig{Label: "c", Kind: AccountAPI}, "").Secret(); err == nil {
		t.Fatal("an empty issued secret read as a secret")
	}
}

// Each kind signs the CLI in with its own variable, in its own config
// dir that lives until Cleanup; two sessions never share a dir.
func TestPrepareAccount(t *testing.T) {
	for _, c := range []struct {
		acct AccountConfig
		want string
	}{
		{AccountConfig{Label: "max-a", Kind: AccountSubscription}, "CLAUDE_CODE_OAUTH_TOKEN"},
		{AccountConfig{Label: "api", Kind: AccountAPI}, "ANTHROPIC_API_KEY"},
	} {
		t.Run(c.acct.Kind, func(t *testing.T) {
			s, err := PrepareAccount(c.acct, "secret-"+c.acct.Label)
			if err != nil {
				t.Fatal(err)
			}
			if v, ok := envOf(s.Env, c.want); !ok || v != "secret-"+c.acct.Label {
				t.Errorf("%s = %q in %v", c.want, v, s.Env)
			}
			other := map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY": "CLAUDE_CODE_OAUTH_TOKEN"}[c.want]
			if _, ok := envOf(s.Env, other); ok {
				t.Errorf("%s set too: %v", other, s.Env)
			}
			dir, ok := envOf(s.Env, "CLAUDE_CONFIG_DIR")
			if !ok || dir != s.ConfigDir {
				t.Fatalf("CLAUDE_CONFIG_DIR %q, config dir %q", dir, s.ConfigDir)
			}
			fi, err := os.Stat(dir)
			if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
				t.Fatalf("config dir %s: %v %v", dir, fi, err)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("config dir holds %v: nothing is written to disk", entries)
			}
			if got := Account(s.Env); got != c.acct.Label {
				t.Errorf("Account = %q, want the label %q", got, c.acct.Label)
			}
			second, err := PrepareAccount(c.acct, "secret-"+c.acct.Label)
			if err != nil {
				t.Fatal(err)
			}
			defer second.Cleanup()
			if second.ConfigDir == s.ConfigDir {
				t.Errorf("two sessions share %s", s.ConfigDir)
			}
			s.Cleanup()
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("config dir %s after Cleanup: %v", dir, err)
			}
		})
	}
	if _, err := PrepareAccount(AccountConfig{Label: "x", Kind: AccountSubscription}, ""); err == nil {
		t.Error("an empty secret prepared")
	}
}

func TestRedact(t *testing.T) {
	if got := redact("token sk-1 and sk-2, sk-1 again", "sk-1", "", "sk-2"); got != "token [redacted] and [redacted], [redacted] again" {
		t.Fatalf("redact = %q", got)
	}
}

// accountHarness records each session's env and whether its config dir
// existed while it ran, then says what fails.
type accountHarness struct {
	mu      sync.Mutex
	jobs    []harness.Job
	dirSeen []bool
	fail    string
}

func (h *accountHarness) Name() string { return "claude-code" }
func (h *accountHarness) Run(_ context.Context, job harness.Job, _ func(harness.Progress)) (harness.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.jobs = append(h.jobs, job)
	dir, _ := envOf(job.Env, "CLAUDE_CONFIG_DIR")
	_, err := os.Stat(dir)
	h.dirSeen = append(h.dirSeen, dir != "" && err == nil)
	if h.fail != "" {
		return harness.Result{}, errors.New(h.fail)
	}
	return harness.Result{Success: true, Text: "done", SessionID: "s"}, nil
}

// A build on an issued account runs with its env beside the GH
// variables, the auth variables unset and the config dir alive; the dir
// is gone after, and a failure that carries the secret is logged and
// handed in with it redacted.
func TestRunItemOnAccount(t *testing.T) {
	const secret = "sk-ant-oat-SECRET-token"
	f := seed(t)
	f.SessionCredentials[item1] = []map[string]any{{"id": "cred-a", "label": "max-a", "vendor": "anthropic", "kind": "subscription", "serves": []any{"claude-code"}, "secret": secret}}
	h := &accountHarness{fail: "claude: bad credential " + secret}
	r, _, item, row := runnerOnFake(t, f, h, nil)
	var logs bytes.Buffer
	r.Log = log.New(&logs, "", 0)
	r.Credentials.Log = r.Log
	err := r.RunItem(context.Background(), item)
	if err == nil {
		t.Fatal("RunItem succeeded on a failing session")
	}
	if len(h.jobs) != 1 {
		t.Fatalf("%d sessions", len(h.jobs))
	}
	job := h.jobs[0]
	if v, _ := envOf(job.Env, "CLAUDE_CODE_OAUTH_TOKEN"); v != secret {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN = %q", v)
	}
	if Account(job.Env) != "max-a" {
		t.Errorf("Account = %q, want max-a", Account(job.Env))
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR"} {
		found := false
		for _, u := range job.Unset {
			found = found || u == k
		}
		if !found {
			t.Errorf("%s not unset: %v", k, job.Unset)
		}
	}
	if !h.dirSeen[0] {
		t.Error("the config dir did not exist during the session")
	}
	dir, _ := envOf(job.Env, "CLAUDE_CONFIG_DIR")
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("config dir %s after the session: %v", dir, err)
	}
	for what, s := range map[string]string{"error": err.Error(), "log": logs.String(), "lastError": toString(row(item1)["lastError"]), "lastNote": toString(row(item1)["lastNote"])} {
		if strings.Contains(s, secret) {
			t.Errorf("the secret is in the %s: %s", what, s)
		}
	}
	if !strings.Contains(toString(row(item1)["lastError"]), "[redacted]") {
		t.Errorf("lastError %q: want the secret redacted", row(item1)["lastError"])
	}
}

// Two sessions on different accounts run in different config dirs.
func TestSessionsOnDifferentAccounts(t *testing.T) {
	var accounts []AccountConfig
	for _, l := range []string{"max-a", "max-b"} {
		accounts = append(accounts, issuedWith(AccountConfig{Label: l, Kind: AccountSubscription, ID: "cred-" + l}, "tok-"+l))
	}
	a, err := OpenAccount(accounts[0])
	if err != nil {
		t.Fatal(err)
	}
	defer a.Cleanup()
	b, err := OpenAccount(accounts[1])
	if err != nil {
		t.Fatal(err)
	}
	defer b.Cleanup()
	if a.ConfigDir == b.ConfigDir || a.Label == b.Label {
		t.Fatalf("sessions share %s / %s", a.ConfigDir, a.Label)
	}
	var ja, jb harness.Job
	a.Apply(&ja)
	b.Apply(&jb)
	if va, _ := envOf(ja.Env, "CLAUDE_CODE_OAUTH_TOKEN"); va != "tok-max-a" {
		t.Errorf("a's token %q", va)
	}
	if vb, _ := envOf(jb.Env, "CLAUDE_CODE_OAUTH_TOKEN"); vb != "tok-max-b" {
		t.Errorf("b's token %q", vb)
	}
}

func toString(v any) string {
	s, _ := v.(string)
	return s
}
