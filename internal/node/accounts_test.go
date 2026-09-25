package node

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/harness"
)

// Two subscriptions and an api key load as listed; the token file's ~
// is the operator's home.
func TestAccountsLoad(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeNodeYAML(t, `slug: mini
name: mini
accounts:
  - label: max-a
    kind: subscription
    token_file: ~/.mesh/max-a.token
  - label: max-b
    kind: subscription
    token_file: /etc/mesh/max-b.token
  - label: api
    kind: api
    api_key_env: MESH_TEST_API_KEY
`)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	want := []AccountConfig{
		{Label: "max-a", Kind: AccountSubscription, TokenFile: "~/.mesh/max-a.token"},
		{Label: "max-b", Kind: AccountSubscription, TokenFile: "/etc/mesh/max-b.token"},
		{Label: "api", Kind: AccountAPI, APIKeyEnv: "MESH_TEST_API_KEY"},
	}
	if len(cfg.Accounts) != len(want) {
		t.Fatalf("accounts %+v, want %+v", cfg.Accounts, want)
	}
	for i := range want {
		if cfg.Accounts[i] != want[i] {
			t.Errorf("accounts[%d] = %+v, want %+v", i, cfg.Accounts[i], want[i])
		}
	}
	if p, _ := cfg.Accounts[0].TokenPath(); p != filepath.Join(home, ".mesh", "max-a.token") {
		t.Errorf("token path %q: ~ not expanded", p)
	}
}

// A node.yaml with none is the node as it was: no accounts, sessions on
// the operator's own login.
func TestAccountsNone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeNodeYAML(t, "slug: mini\nname: mini\n")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Accounts) != 0 {
		t.Fatalf("accounts %+v", cfg.Accounts)
	}
	acct, err := OpenAccount(context.Background(), cfg.Accounts)
	if err != nil || acct != nil {
		t.Fatalf("OpenAccount with none = %v, %v; want nil, nil", acct, err)
	}
	job := harness.Job{Env: []string{"GH_TOKEN=x"}}
	acct.Apply(&job)
	acct.Cleanup()
	if len(job.Env) != 1 || len(job.Unset) != 0 || len(job.Secrets) != 0 {
		t.Fatalf("a nil account changed the job: %+v", job)
	}
}

func TestAccountsRejected(t *testing.T) {
	for name, entries := range map[string]string{
		"duplicate label": "  - {label: a, kind: subscription, token_file: /t}\n  - {label: a, kind: api, api_key_env: K}\n",
		"no label":        "  - {kind: subscription, token_file: /t}\n",
		"unknown kind":    "  - {label: a, kind: team, token_file: /t}\n",
		"no token_file":   "  - {label: a, kind: subscription}\n",
		"no api_key_env":  "  - {label: a, kind: api}\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			writeNodeYAML(t, "slug: mini\nname: mini\naccounts:\n"+entries)
			if cfg, err := LoadConfig(); err == nil {
				t.Fatalf("loaded %+v, want an error", cfg.Accounts)
			}
		})
	}
}

func TestAccountSecret(t *testing.T) {
	dir := t.TempDir()
	tokFile := filepath.Join(dir, "tok")
	if err := os.WriteFile(tokFile, []byte("  sk-ant-oat-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := (AccountConfig{Label: "a", Kind: AccountSubscription, TokenFile: tokFile}).Secret(); err != nil || s != "sk-ant-oat-123" {
		t.Fatalf("subscription secret %q, %v", s, err)
	}
	t.Setenv("MESH_TEST_KEY", "sk-ant-api-456")
	if s, err := (AccountConfig{Label: "b", Kind: AccountAPI, APIKeyEnv: "MESH_TEST_KEY"}).Secret(); err != nil || s != "sk-ant-api-456" {
		t.Fatalf("api secret %q, %v", s, err)
	}
	if _, err := (AccountConfig{Label: "c", Kind: AccountAPI, APIKeyEnv: "MESH_TEST_UNSET_KEY"}).Secret(); err == nil {
		t.Fatal("an unset key variable read as a secret")
	}
	if _, err := (AccountConfig{Label: "d", Kind: AccountSubscription, TokenFile: filepath.Join(dir, "missing")}).Secret(); err == nil {
		t.Fatal("a missing token file read as a secret")
	}
}

// Each kind signs the CLI in with its own variable, in its own config
// dir that lives until Cleanup; two sessions never share a dir.
func TestPrepareAccount(t *testing.T) {
	for _, c := range []struct {
		acct AccountConfig
		want string
	}{
		{AccountConfig{Label: "max-a", Kind: AccountSubscription, TokenFile: "/t"}, "CLAUDE_CODE_OAUTH_TOKEN"},
		{AccountConfig{Label: "api", Kind: AccountAPI, APIKeyEnv: "K"}, "ANTHROPIC_API_KEY"},
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

// A build on a configured account runs with its env beside the GH
// variables, the auth variables unset and the config dir alive; the dir
// is gone after, and a failure that carries the secret is logged and
// handed in with it redacted.
func TestRunItemOnAccount(t *testing.T) {
	const secret = "sk-ant-oat-SECRET-token"
	tokFile := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(tokFile, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &accountHarness{fail: "claude: bad credential " + secret}
	r, _, item, row := runnerOn(t, h, nil)
	var logs bytes.Buffer
	r.Log = log.New(&logs, "", 0)
	r.Accounts = []AccountConfig{{Label: "max-a", Kind: AccountSubscription, TokenFile: tokFile}, {Label: "max-b", Kind: AccountSubscription, TokenFile: "/nope"}}
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
	dir := t.TempDir()
	var accounts []AccountConfig
	for _, l := range []string{"max-a", "max-b"} {
		p := filepath.Join(dir, l)
		if err := os.WriteFile(p, []byte("tok-"+l), 0o600); err != nil {
			t.Fatal(err)
		}
		accounts = append(accounts, AccountConfig{Label: l, Kind: AccountSubscription, TokenFile: p})
	}
	a, err := OpenAccount(context.Background(), accounts[:1])
	if err != nil {
		t.Fatal(err)
	}
	defer a.Cleanup()
	b, err := OpenAccount(context.Background(), accounts[1:])
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
