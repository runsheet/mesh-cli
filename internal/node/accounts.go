package node

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// The kinds of Claude account a node runs sessions on (adr-005).
const (
	// AccountSubscription is a Claude plan (Max) signed in with the
	// OAuth token `claude setup-token` prints.
	AccountSubscription = "subscription"
	// AccountAPI is an Anthropic API key.
	AccountAPI = "api"
)

// AccountEnv names the variable a session's env carries its account's
// label in, so Account keys the plan limits by label rather than by the
// session's throwaway config dir.
const AccountEnv = "MESH_ACCOUNT"

// AuthEnv are the inherited variables that choose a Claude login. A
// session on a configured account drops every one of them from the
// node's environment before its own are added, so the operator's own
// login never leaks into a session of another account.
var AuthEnv = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR", AccountEnv}

// CodexAuthEnv are the inherited variables that choose a Codex login,
// dropped from a session on a Codex account for the same reason.
var CodexAuthEnv = []string{"CODEX_HOME", "CODEX_API_KEY", "OPENAI_API_KEY"}

// The adapters an account signs in (AccountConfig.Adapter): the agent
// CLI whose sessions it runs.
const (
	AdapterClaudeCode = gateway.ClaudeCodeAdapter
	AdapterCodex      = gateway.CodexAdapter
)

// AccountConfig is an account a session may run on: a credential the
// claim-time read issued (IssuedAccount), as Known remembers it and as
// the session opens it (OpenAccount). node.yaml names none — its
// `accounts:` is retired (Config.Retired, adr-005's amendments 2026-10).
type AccountConfig struct {
	Label string
	// Kind is AccountSubscription or AccountAPI.
	Kind string
	// DailyCapUSD and MonthlyCapUSD are an api key's caps in list-price
	// dollars (adr-005-capacity-across-accounts); 0 is the default,
	// DefaultDailyCapUSD and DefaultMonthlyCapUSD. A subscription has none.
	DailyCapUSD   float64
	MonthlyCapUSD float64
	// Project is the project whose daily budget (Config.ProjectBudgets)
	// an api key spends; empty is none.
	Project string
	// ID is the platform credential's id (IssuedAccount).
	ID string
	// Adapter is the agent CLI the account signs in: empty or
	// AdapterClaudeCode for a Claude account, AdapterCodex for an issued
	// OpenAI credential serving codex.
	Adapter string
	// SecretVersion is an issued credential's secret version: a Codex
	// login refreshed under one is kept for it (CodexLoginCache).
	SecretVersion int
	// Priorities are the goal priorities the account serves (urgent,
	// high, normal, low): ChooseAccount never picks it for work of any
	// other. Empty serves every priority, as before (adr-005's amendment
	// 2026-10, a credential names the priorities it serves).
	Priorities []string
	// PaceFiveHour and PaceSevenDay are the account's own pace points,
	// fractions of its windows (the credential's windowTargets): past
	// either it takes only urgent and high work. 0 is the node-wide rule
	// (LimitsConfig.PaceFiveHour; no seven-day pace point).
	PaceFiveHour, PaceSevenDay float64
	// issued is a platform credential's secret, held in memory for the
	// one session it was issued for; a pointer, so printing the account
	// prints an address, never the secret.
	issued *string
}

// AdapterOf is the adapter the account signs in, AdapterClaudeCode
// when none is named.
func (a AccountConfig) AdapterOf() string {
	if a.Adapter == "" {
		return AdapterClaudeCode
	}
	return a.Adapter
}

// Serves says the account serves work of that priority: every
// priority when it names none, and work of no priority (research, "")
// on every account.
func (a AccountConfig) Serves(priority string) bool {
	return len(a.Priorities) == 0 || priority == "" || slices.Contains(a.Priorities, priority)
}

// IsCodex says the account is a Codex one.
func (a AccountConfig) IsCodex() bool { return a.Adapter == AdapterCodex }

// Caps are an api key's daily and monthly caps, the defaults where
// node.yaml names none.
func (a AccountConfig) Caps() (daily, monthly float64) {
	c := CapacityAccount{DailyCapUSD: a.DailyCapUSD, MonthlyCapUSD: a.MonthlyCapUSD}
	return c.dailyCap(), c.monthlyCap()
}

// capacity is the account as the accounts scaler sees it, with its
// project's daily budget from budgets.
func (a AccountConfig) capacity(budgets map[string]float64) CapacityAccount {
	c := CapacityAccount{Name: a.Label, Kind: a.Kind}
	if a.Kind == AccountAPI {
		c.DailyCapUSD, c.MonthlyCapUSD = a.Caps()
		c.Project, c.ProjectDailyBudgetUSD = a.Project, budgets[a.Project]
	}
	return c
}

// Secret is the account's credential, as the claim-time read issued it
// for this session; an account Known remembers carries none (the
// platform issues it at claim only). The error never carries the
// secret.
func (a AccountConfig) Secret() (string, error) {
	if a.issued == nil {
		return "", fmt.Errorf("account %s: the platform issues its secret at claim only", a.Label)
	}
	if *a.issued == "" {
		return "", fmt.Errorf("account %s: the platform issued no secret", a.Label)
	}
	return *a.issued, nil
}

// AccountSession is one session's hold on an account: the environment
// it runs with and the config dir that is its alone. A nil
// *AccountSession (a test's job with no account) adds nothing, unsets
// nothing and redacts nothing; no started session has one.
type AccountSession struct {
	Label string
	// ConfigDir is the session's CLAUDE_CONFIG_DIR — for a Codex
	// account its CODEX_HOME — removed by Cleanup.
	ConfigDir string
	// Env is what the session adds: the credential (for a Codex login,
	// the home that holds it), the config dir and the label (AccountEnv).
	Env []string
	// secrets are what the session redacts: the secret, and for a Codex
	// login the auth.json it runs on and each token in it.
	secrets []string
	codex   bool
	// login is a Codex login's cache file (CodexLoginCache) and wrote the
	// bytes put in the home's auth.json: Cleanup keeps a changed one there.
	login string
	wrote []byte
}

// PrepareAccount makes a session's hold on an account. A Claude account
// gets a fresh 0700 temp dir for its CLAUDE_CONFIG_DIR, and the env that
// signs the CLI in — CLAUDE_CODE_OAUTH_TOKEN for a subscription,
// ANTHROPIC_API_KEY for an api account; the secret goes in the env
// only: nothing but the empty dir is written to disk. A Codex account
// (PrepareCodexAccount) gets a CODEX_HOME.
func PrepareAccount(a AccountConfig, secret string) (*AccountSession, error) {
	if secret == "" {
		return nil, fmt.Errorf("account %s: no secret", a.Label)
	}
	if a.IsCodex() {
		return PrepareCodexAccount(a, secret)
	}
	var cred string
	switch a.Kind {
	case AccountSubscription:
		cred = "CLAUDE_CODE_OAUTH_TOKEN=" + secret
	case AccountAPI:
		cred = "ANTHROPIC_API_KEY=" + secret
	default:
		return nil, fmt.Errorf("account %s: kind %q is not %s or %s", a.Label, a.Kind, AccountSubscription, AccountAPI)
	}
	dir, err := os.MkdirTemp("", "mesh-claude-")
	if err != nil {
		return nil, fmt.Errorf("account %s: config dir: %w", a.Label, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("account %s: config dir: %w", a.Label, err)
	}
	return &AccountSession{
		Label:     a.Label,
		ConfigDir: dir,
		Env:       []string{cred, "CLAUDE_CONFIG_DIR=" + dir, AccountEnv + "=" + a.Label},
		secrets:   []string{secret},
	}, nil
}

// CodexHomeRoot is where a Codex account's session homes are made, as
// harness.Codex makes its own: <user cache dir>/mesh/codex. Not /tmp:
// Codex refuses to put its helper binaries under /tmp.
func CodexHomeRoot() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "mesh", "codex"), nil
}

// CodexLoginCache is where a Codex login an issued credential's session
// refreshed is kept for the next session on it: <root>/logins/<id>.<secretVersion>.json.
func CodexLoginCache(root, id string, version int) string {
	return filepath.Join(root, "logins", id+"."+strconv.Itoa(version)+".json")
}

// PrepareCodexAccount makes a Codex session's hold on an issued OpenAI
// account: a fresh 0700 CODEX_HOME under CodexHomeRoot, named on Env
// with the label (AccountEnv). A subscription's secret is the auth.json
// `codex login` writes, written verbatim as <home>/auth.json at 0600 —
// the one account secret the node puts on disk, in the session's own
// home, gone with it (Cleanup); a secret that is not a JSON object is
// refused, unechoed. A login the platform issued before at the same
// secret version and a session refreshed since (CodexLoginCache) is
// preferred to the issued one, since Codex rotates the refresh token;
// one at another version is discarded. An api key goes on Env as
// CODEX_API_KEY and OPENAI_API_KEY (codex exec reads the first), and
// nothing is written.
func PrepareCodexAccount(a AccountConfig, secret string) (*AccountSession, error) {
	if secret == "" {
		return nil, fmt.Errorf("account %s: no secret", a.Label)
	}
	if a.Kind != AccountSubscription && a.Kind != AccountAPI {
		return nil, fmt.Errorf("account %s: kind %q is not %s or %s", a.Label, a.Kind, AccountSubscription, AccountAPI)
	}
	var login []byte
	if a.Kind == AccountSubscription {
		login = []byte(secret)
		if !jsonObject(login) {
			return nil, fmt.Errorf("account %s: the issued Codex login is not a JSON object (the auth.json `codex login` writes)", a.Label)
		}
	}
	root, err := CodexHomeRoot()
	if err != nil {
		return nil, fmt.Errorf("account %s: codex home: %w", a.Label, err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("account %s: codex home: %w", a.Label, err)
	}
	dir, err := os.MkdirTemp(root, "home-")
	if err != nil {
		return nil, fmt.Errorf("account %s: codex home: %w", a.Label, err)
	}
	fail := func(err error) (*AccountSession, error) {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("account %s: codex home: %w", a.Label, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fail(err)
	}
	s := &AccountSession{Label: a.Label, ConfigDir: dir, codex: true,
		Env: []string{"CODEX_HOME=" + dir, AccountEnv + "=" + a.Label}, secrets: []string{secret}}
	if a.Kind == AccountAPI {
		s.Env = append(s.Env, "CODEX_API_KEY="+secret, "OPENAI_API_KEY="+secret)
		return s, nil
	}
	s.secrets = append(s.secrets, jsonSecrets(login)...)
	if a.ID != "" {
		s.login = CodexLoginCache(root, a.ID, a.SecretVersion)
		if kept := cachedLogin(s.login); kept != nil {
			login = kept
			s.secrets = append(s.secrets, string(kept))
			s.secrets = append(s.secrets, jsonSecrets(kept)...)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), login, 0o600); err != nil {
		return fail(err)
	}
	s.wrote = login
	return s, nil
}

// cachedLogin is the refreshed login kept at path, when it is still a
// JSON object; every other version of the same credential's is removed
// — a new secret version is a login pasted or rotated on the platform,
// which wins over what a session refreshed from the old one.
func cachedLogin(path string) []byte {
	dir, base := filepath.Split(path)
	id := strings.TrimSuffix(base, ".json")
	id = id[:strings.LastIndex(id, ".")]
	if others, _ := filepath.Glob(filepath.Join(dir, id+".*.json")); len(others) > 0 {
		for _, o := range others {
			if o != path {
				os.Remove(o)
			}
		}
	}
	data, err := os.ReadFile(path)
	if err != nil || !jsonObject(data) {
		return nil
	}
	return data
}

// keepLogin keeps a Codex login the session refreshed (its home's
// auth.json no longer what was written) at the credential's cache file,
// written through a temp file at 0600.
func (s *AccountSession) keepLogin() {
	if s.login == "" {
		return
	}
	now, err := os.ReadFile(filepath.Join(s.ConfigDir, "auth.json"))
	if err != nil || bytes.Equal(now, s.wrote) || !jsonObject(now) {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.login), 0o700); err != nil {
		return
	}
	tmp := s.login + ".tmp-" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, now, 0o600); err != nil {
		os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, s.login); err != nil {
		os.Remove(tmp)
	}
}

// jsonObject says data is one JSON object.
func jsonObject(data []byte) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(data, &m) == nil && m != nil
}

// jsonSecrets are the string values in a login's JSON long enough to be
// a token, so a token quoted alone is redacted too.
func jsonSecrets(data []byte) []string {
	var v any
	if json.Unmarshal(data, &v) != nil {
		return nil
	}
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		case string:
			if len(x) >= 16 {
				out = append(out, x)
			}
		}
	}
	walk(v)
	return out
}

// OpenAccount opens an account the claim-time read issued for one
// session (Credentials.Open): its secret, held in memory, put in a
// temporary config dir (PrepareAccount). There is no account but an
// issued one — node.yaml names none and the operator's own login is
// never opened (adr-005's amendments 2026-10).
func OpenAccount(a AccountConfig) (*AccountSession, error) {
	secret, err := a.Secret()
	if err != nil {
		return nil, err
	}
	return PrepareAccount(a, secret)
}

// Apply puts the account on a job: the inherited auth variables unset,
// its env beside the job's own, its secret among those redacted.
func (s *AccountSession) Apply(job *harness.Job) {
	if s == nil {
		return
	}
	job.Unset = append(job.Unset, AuthEnv...)
	if s.codex {
		job.Unset = append(job.Unset, CodexAuthEnv...)
	}
	job.Env = append(job.Env, s.Env...)
	job.Secrets = append(job.Secrets, s.secrets...)
}

// Redact is s with the account's secret taken out.
func (s *AccountSession) Redact(text string) string {
	if s == nil {
		return text
	}
	return redact(text, s.secrets...)
}

// RedactErr is err with the account's secret taken out of its message,
// still wrapping err for errors.Is and errors.As when there was nothing
// to take out.
func (s *AccountSession) RedactErr(err error) error {
	if err == nil || s == nil {
		return err
	}
	if msg := err.Error(); redact(msg, s.secrets...) != msg {
		return errors.New(redact(msg, s.secrets...))
	}
	return err
}

// Cleanup removes the session's config dir — a Codex account's home
// with its auth.json, a login the session refreshed kept first
// (CodexLoginCache). It is safe on nil and to call twice.
func (s *AccountSession) Cleanup() {
	if s == nil || s.ConfigDir == "" {
		return
	}
	s.keepLogin()
	os.RemoveAll(s.ConfigDir)
	s.ConfigDir = ""
}

// redact replaces every secret in s with harness.Redacted: what the
// node logs, reports and hands in as a failure passes through it.
func redact(s string, secrets ...string) string {
	return harness.Redact(s, secrets...)
}
