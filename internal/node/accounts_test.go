package node

import (
	"errors"
	"os"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/harness"
)

func envOf(env []string, key string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

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
