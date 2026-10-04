package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// codexLogin is a fixture ChatGPT login as `codex login` writes it; its
// refresh token is the fixture secret no log line or report may carry.
const (
	codexRefresh = "rt-FIXTURE-codex-refresh-token-0001"
	codexLogin   = `{"OPENAI_API_KEY":null,"tokens":{"id_token":"idt-FIXTURE-codex-id-token-0001","access_token":"at-FIXTURE-codex-access-token-0001","refresh_token":"` + codexRefresh + `","account_id":"acct-1"},"last_refresh":"2026-09-28T00:00:00Z"}`
	codexKey     = "sk-proj-FIXTURE-openai-key-0001"
)

func codexSub() gateway.Credential {
	return gateway.Credential{ID: "oa-1", Label: "chatgpt-pro", Kind: AccountSubscription, Adapter: AdapterCodex, SecretVersion: 1, Secret: codexLogin}
}

func codexAPI() gateway.Credential {
	return gateway.Credential{ID: "oa-2", Label: "openai-key", Kind: AccountAPI, Adapter: AdapterCodex, SecretVersion: 1, Secret: codexKey, CapDailyUSD: 30}
}

func claudeSub() gateway.Credential {
	return gateway.Credential{ID: "cred-1", Label: "max-cloud", Kind: AccountSubscription, Adapter: AdapterClaudeCode, Secret: issuedSecret}
}

// cacheRoot points os.UserCacheDir at a temp dir and returns the Codex
// home root under it.
func cacheRoot(t *testing.T) string {
	t.Helper()
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", t.TempDir())
	return filepath.Join(cache, "mesh", "codex")
}

// An issued ChatGPT login lands verbatim as the session's
// CODEX_HOME/auth.json at 0600, under the Codex home root, with the
// home and the label on the env and no key; Cleanup takes it all away.
func TestPrepareCodexSubscription(t *testing.T) {
	root := cacheRoot(t)
	a := IssuedAccount(codexSub())
	if a.Adapter != AdapterCodex || a.Kind != AccountSubscription {
		t.Fatalf("issued account = %+v", a)
	}
	s, err := PrepareAccount(a, codexLogin)
	if err != nil {
		t.Fatal(err)
	}
	home, ok := envOf(s.Env, "CODEX_HOME")
	if !ok || filepath.Dir(home) != root {
		t.Fatalf("CODEX_HOME = %q, want a dir under %s", home, root)
	}
	if v, _ := envOf(s.Env, AccountEnv); v != "chatgpt-pro" {
		t.Errorf("%s = %q", AccountEnv, v)
	}
	for _, k := range []string{"CODEX_API_KEY", "OPENAI_API_KEY", "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if _, ok := envOf(s.Env, k); ok {
			t.Errorf("env carries %s", k)
		}
	}
	if st, err := os.Stat(home); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("home: %v %v", st, err)
	}
	auth := filepath.Join(home, "auth.json")
	data, err := os.ReadFile(auth)
	if err != nil || string(data) != codexLogin {
		t.Fatalf("auth.json is not the secret's bytes (err %v)", err)
	}
	if st, _ := os.Stat(auth); st.Mode().Perm() != 0o600 {
		t.Errorf("auth.json mode %v", st.Mode().Perm())
	}
	job := harness.Job{}
	s.Apply(&job)
	for _, want := range []string{codexLogin, codexRefresh} {
		if !containsStr(job.Secrets, want) {
			t.Errorf("job secrets leave out part of the login")
		}
	}
	for _, v := range CodexAuthEnv {
		if !containsStr(job.Unset, v) {
			t.Errorf("the operator's %s is not unset", v)
		}
	}
	if got := s.Redact("refreshed with " + codexRefresh); strings.Contains(got, codexRefresh) {
		t.Errorf("Redact leaves the token")
	}
	s.Cleanup()
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("home left after Cleanup: %v", err)
	}
	s.Cleanup()
}

// An issued OpenAI key is CODEX_API_KEY and OPENAI_API_KEY in the env
// only: the home holds no auth.json.
func TestPrepareCodexAPIKey(t *testing.T) {
	cacheRoot(t)
	s, err := PrepareAccount(IssuedAccount(codexAPI()), codexKey)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Cleanup()
	for _, k := range []string{"CODEX_API_KEY", "OPENAI_API_KEY"} {
		if v, _ := envOf(s.Env, k); v != codexKey {
			t.Errorf("%s is not the key", k)
		}
	}
	home, _ := envOf(s.Env, "CODEX_HOME")
	if home == "" {
		t.Fatal("no CODEX_HOME")
	}
	if _, err := os.Stat(filepath.Join(home, "auth.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("auth.json exists for an api key: %v", err)
	}
	job := harness.Job{}
	s.Apply(&job)
	if !containsStr(job.Secrets, codexKey) {
		t.Error("the key is not among the job's secrets")
	}
}

// A login that is not a JSON object is refused, and the error does not
// echo it.
func TestPrepareCodexRefusesNonJSON(t *testing.T) {
	root := cacheRoot(t)
	_, err := PrepareAccount(IssuedAccount(codexSub()), codexRefresh)
	if err == nil || strings.Contains(err.Error(), codexRefresh) {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := filepath.Glob(filepath.Join(root, "home-*")); len(entries) != 0 {
		t.Errorf("homes left: %v", entries)
	}
}

// A login a session refreshed is kept for its credential id and secret
// version and preferred when the same one is issued again; a newer
// secret version discards it for the issued login.
func TestCodexRefreshedLoginCache(t *testing.T) {
	root := cacheRoot(t)
	cr := codexSub()
	s, err := PrepareAccount(IssuedAccount(cr), cr.Secret)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := envOf(s.Env, "CODEX_HOME")
	refreshed := strings.ReplaceAll(codexLogin, codexRefresh, "rt-FIXTURE-rotated-refresh-0002")
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(refreshed), 0o600); err != nil {
		t.Fatal(err)
	}
	s.Cleanup()
	cache := CodexLoginCache(root, "oa-1", 1)
	if data, err := os.ReadFile(cache); err != nil || string(data) != refreshed {
		t.Fatalf("refreshed login not kept at %s: %v", cache, err)
	}
	if st, _ := os.Stat(cache); st.Mode().Perm() != 0o600 {
		t.Errorf("cache mode %v", st.Mode().Perm())
	}

	again, err := PrepareAccount(IssuedAccount(cr), cr.Secret)
	if err != nil {
		t.Fatal(err)
	}
	home, _ = envOf(again.Env, "CODEX_HOME")
	if data, _ := os.ReadFile(filepath.Join(home, "auth.json")); string(data) != refreshed {
		t.Error("the same id and version did not run on the refreshed login")
	}
	job := harness.Job{}
	again.Apply(&job)
	if !containsStr(job.Secrets, "rt-FIXTURE-rotated-refresh-0002") {
		t.Error("the refreshed token is not among the job's secrets")
	}
	again.Cleanup()

	cr.SecretVersion = 2
	newer, err := PrepareAccount(IssuedAccount(cr), cr.Secret)
	if err != nil {
		t.Fatal(err)
	}
	defer newer.Cleanup()
	home, _ = envOf(newer.Env, "CODEX_HOME")
	if data, _ := os.ReadFile(filepath.Join(home, "auth.json")); string(data) != codexLogin {
		t.Error("a newer secret version ran on the old refreshed login")
	}
	if _, err := os.Stat(cache); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the old version's login was not discarded: %v", err)
	}
}

// Credentials.Open chooses among the issued accounts of the session's
// adapter only: a Codex session a Codex account, a Claude session an
// Anthropic one; a Codex-only read for a Claude session is ErrNoAccount.
func TestOpenChoosesByAdapter(t *testing.T) {
	cacheRoot(t)
	var logs syncBuf
	ref := gateway.SessionRef{Kind: "evaluate", ItemID: item1}
	gw := &credsGW{issued: []gateway.Credential{claudeSub(), codexSub()}}
	known := &KnownAccounts{}
	c := &Credentials{GW: gw, Known: known, Log: log.New(&logs, "", 0)}

	s, err := c.Open(context.Background(), ref, "n", AdapterCodex)
	if err != nil {
		t.Fatal(err)
	}
	if s.Label != "chatgpt-pro" || s.Adapter != AdapterCodex {
		t.Fatalf("codex session chose %q (%s)", s.Label, s.Adapter)
	}
	if _, ok := envOf(s.Env, "CODEX_HOME"); !ok {
		t.Error("no CODEX_HOME")
	}
	s.Cleanup()

	s, err = c.Open(context.Background(), ref, "n", AdapterClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	if s.Label != "max-cloud" {
		t.Fatalf("claude session chose %q", s.Label)
	}
	if _, ok := envOf(s.Env, "CODEX_HOME"); ok {
		t.Error("a Claude session got a CODEX_HOME")
	}
	s.Cleanup()

	gw.issued = []gateway.Credential{codexSub(), codexAPI()}
	if _, err := c.Open(context.Background(), ref, "n", ""); !isNoCredential(err) || !strings.Contains(err.Error(), "no claude-code credential") {
		t.Fatalf("a codex-only read for a Claude session = %v, want ErrNoCredential", err)
	}

	// the known accounts note the codex ones with their adapter, and no
	// secret — checked here, before the read below withdraws them
	// (Reconcile, adr-005's amendment 2026-10-03): an anthropic-only read
	// for a Codex session names no codex credential, so it withdraws
	// every known codex label once it runs.
	var codex []string
	for _, a := range known.Accounts(time.Now()) {
		if a.IsCodex() {
			codex = append(codex, a.Label)
		}
	}
	if strings.Join(codex, ",") != "chatgpt-pro,openai-key" {
		t.Errorf("known codex accounts = %v", codex)
	}
	if out := logs.String(); strings.Contains(out, codexRefresh) || strings.Contains(out, codexKey) {
		t.Errorf("a secret is in the log:\n%s", out)
	}

	gw.issued = []gateway.Credential{claudeSub()}
	if _, err := c.Open(context.Background(), ref, "n", AdapterCodex); !isNoCredential(err) || !strings.Contains(err.Error(), "no codex credential") {
		t.Fatalf("an anthropic-only read for a Codex session = %v, want ErrNoCredential", err)
	}
}

// codexFake is a Codex harness: it reads the session's auth.json as
// Codex would and ends with usage on gpt-6.
type codexFake struct{ sawLogin bool }

func (h *codexFake) Name() string { return "codex" }
func (h *codexFake) Run(_ context.Context, job harness.Job, _ func(harness.Progress)) (harness.Result, error) {
	if home, ok := envOf(job.Env, "CODEX_HOME"); ok {
		data, _ := os.ReadFile(filepath.Join(home, "auth.json"))
		h.sawLogin = string(data) == codexLogin
	}
	return harness.Result{SessionID: "codex-thread", Success: true, CostUSD: 0.5,
		Usage:     &harness.Usage{Model: "gpt-6-sol", InputTokens: 1000, OutputTokens: 100},
		RateLimit: &harness.RateLimit{Status: "allowed", FiveHour: harness.RateLimitWindow{Utilization: 0.4}, ObservedAt: time.Now()}}, nil
}

// A Codex session on an issued login reports adapter codex, the
// credential's id and kind subscription, and its model; its reading
// lands under codex:<label>.
func TestCodexSessionUsageReport(t *testing.T) {
	cacheRoot(t)
	limits := &Limits{}
	c := &Credentials{GW: &credsGW{issued: []gateway.Credential{claudeSub(), codexSub()}}, Limits: limits}
	h := &codexFake{}
	s, err := c.Open(context.Background(), gateway.SessionRef{Kind: "evaluate", ItemID: item1}, "n", Adapter(h))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Cleanup()
	job := harness.Job{}
	s.Apply(&job)
	res, err := ObserveLimits(h, limits).Run(context.Background(), job, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !h.sawLogin {
		t.Error("the Codex session did not run on the issued login")
	}
	rep := s.Report(res)
	if rep.Adapter != "codex" || rep.CredentialID != "oa-1" || rep.CredentialKind != gateway.CredentialSubscription || rep.Model != "gpt-6-sol" {
		t.Errorf("report = %+v", rep)
	}
	if rep.FiveHourAfter == nil || *rep.FiveHourAfter != 0.4 {
		t.Errorf("five-hour after = %v", rep.FiveHourAfter)
	}
	b, _ := json.Marshal(rep)
	if bytes.Contains(b, []byte(codexRefresh)) {
		t.Error("the report carries the login")
	}
	if _, ok := limits.Reading("codex:chatgpt-pro"); !ok {
		t.Error("the reading is not under codex:chatgpt-pro")
	}
	if _, ok := limits.Readings()["chatgpt-pro"]; ok {
		t.Error("the Codex reading is among the Claude ones")
	}
}

// A Codex account's reading is kept under codex:<label>: Readings (the
// Claude accounts', what paces, chooses and scales) leaves it out,
// Reading and CodexReadings return it, and the heartbeat carries it
// with its prefix as the marker.
func TestCodexReadingsApart(t *testing.T) {
	l := &Limits{}
	rl := &harness.RateLimit{Status: "allowed", FiveHour: harness.RateLimitWindow{Utilization: 0.9}, ObservedAt: time.Now()}
	l.Observe(CodexLimitsAccount("chatgpt-pro"), rl)
	l.Observe("max-cloud", &harness.RateLimit{Status: "allowed", ObservedAt: time.Now()})
	if _, ok := l.Readings()[CodexLimitsAccount("chatgpt-pro")]; ok || len(l.Readings()) != 1 {
		t.Errorf("Readings = %v", l.Readings())
	}
	if got, ok := l.Reading("codex:chatgpt-pro"); !ok || got.FiveHour.Utilization != 0.9 {
		t.Errorf("Reading = %v %v", got, ok)
	}
	if got := l.CodexReadings(); len(got) != 1 || got["chatgpt-pro"].FiveHour.Utilization != 0.9 {
		t.Errorf("CodexReadings = %v", got)
	}
	var accounts []string
	for _, h := range l.Heartbeat(nil, time.Now(), LimitsConfig{}) {
		accounts = append(accounts, h.Account)
	}
	if strings.Join(accounts, ",") != "codex:chatgpt-pro,max-cloud" {
		t.Errorf("heartbeat accounts = %v", accounts)
	}
}

// The loop schedules on the Claude accounts alone: a known Codex
// account is judged apart (CodexAccounts), by its own readings.
func TestLoopKeepsCodexAccountsApart(t *testing.T) {
	now := time.Now()
	known := &KnownAccounts{}
	known.Note(IssuedAccount(claudeSub()), now)
	known.Note(IssuedAccount(codexSub()), now)
	l := &Loop{Known: known, Limits: &Limits{}, Now: func() time.Time { return now }}
	l.Limits.Observe(CodexLimitsAccount("chatgpt-pro"), &harness.RateLimit{Status: "allowed", ObservedAt: now})
	for _, a := range l.scheduled() {
		if a.IsCodex() {
			t.Errorf("the loop schedules on %s", a.Label)
		}
	}
	codex, readings := l.CodexAccounts()
	if len(codex) != 1 || codex[0].Label != "chatgpt-pro" || codex[0].Adapter != AdapterCodex {
		t.Errorf("CodexAccounts = %+v", codex)
	}
	if _, ok := readings["chatgpt-pro"]; !ok {
		t.Errorf("codex readings = %v", readings)
	}
}

func containsStr(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

// A Codex session with no issued Codex credential has no account: a
// read that issues nothing is ErrNoCredential (adr-005's amendment
// 2026-10), and no Credentials at all is a programming error, a panic —
// never the operator's own Codex login, which no longer exists as an
// account (codex:default).
func TestCodexOpenWithNothingIssuedIsNoAccount(t *testing.T) {
	cacheRoot(t)
	ref := gateway.SessionRef{Kind: "evaluate", ItemID: item1}
	c := &Credentials{GW: &credsGW{}, Log: log.New(&syncBuf{}, "", 0)}
	if _, err := c.Open(context.Background(), ref, "n", AdapterCodex); !isNoCredential(err) {
		t.Fatalf("nothing issued = %v, want ErrNoCredential", err)
	}
	defer func() {
		if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "nil *Credentials") {
			t.Fatalf("no Credentials recovered %v, want the nil *Credentials panic", r)
		}
	}()
	var none *Credentials
	_, _ = none.Open(context.Background(), ref, "n", AdapterCodex)
	t.Fatal("Open on a nil *Credentials returned")
}
