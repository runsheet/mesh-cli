package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

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

// AccountConfig is one entry of node.yaml's `accounts:` — a Claude
// account the node's sessions may run on. The secret itself is never in
// node.yaml: a subscription names the file holding its token, an api
// account the variable holding its key.
type AccountConfig struct {
	Label string `yaml:"label"`
	// Kind is AccountSubscription or AccountAPI.
	Kind string `yaml:"kind"`
	// TokenFile holds a `claude setup-token` OAuth token (subscription).
	// A leading ~ is the operator's home.
	TokenFile string `yaml:"token_file,omitempty"`
	// APIKeyEnv is the name of the variable holding the API key (api).
	APIKeyEnv string `yaml:"api_key_env,omitempty"`
	// DailyCapUSD and MonthlyCapUSD are an api key's caps in list-price
	// dollars (adr-005-capacity-across-accounts); 0 is the default,
	// DefaultDailyCapUSD and DefaultMonthlyCapUSD. A subscription has none.
	DailyCapUSD   float64 `yaml:"daily_cap,omitempty"`
	MonthlyCapUSD float64 `yaml:"monthly_cap,omitempty"`
	// Project is the project whose daily budget (Config.ProjectBudgets)
	// an api key spends; empty is none.
	Project string `yaml:"project,omitempty"`
	// ID is a platform credential's id (IssuedAccount); node.yaml's
	// accounts have none.
	ID string `yaml:"-"`
	// issued is a platform credential's secret, held in memory for the
	// one session it was issued for; a pointer, so printing the account
	// prints an address, never the secret.
	issued *string
}

// ValidateAccounts refuses a list the node could not run sessions on:
// a label missing or used twice, a kind it does not know, a
// subscription with no token_file, an api account with no api_key_env.
func ValidateAccounts(accounts []AccountConfig) error {
	seen := map[string]bool{}
	for i, a := range accounts {
		label := strings.TrimSpace(a.Label)
		if label == "" {
			return fmt.Errorf("accounts[%d]: a label is required", i)
		}
		if seen[label] {
			return fmt.Errorf("accounts[%d]: label %q is used twice", i, label)
		}
		seen[label] = true
		switch a.Kind {
		case AccountSubscription:
			if strings.TrimSpace(a.TokenFile) == "" {
				return fmt.Errorf("account %q: a subscription needs token_file", label)
			}
		case AccountAPI:
			if strings.TrimSpace(a.APIKeyEnv) == "" {
				return fmt.Errorf("account %q: an api account needs api_key_env", label)
			}
		default:
			return fmt.Errorf("account %q: kind %q is not %s or %s", label, a.Kind, AccountSubscription, AccountAPI)
		}
		if a.DailyCapUSD < 0 || a.MonthlyCapUSD < 0 {
			return fmt.Errorf("account %q: a cap cannot be negative", label)
		}
		if a.Kind == AccountSubscription && (a.DailyCapUSD > 0 || a.MonthlyCapUSD > 0 || a.Project != "") {
			return fmt.Errorf("account %q: caps and project are for an api account, not a subscription", label)
		}
	}
	return nil
}

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

// TokenPath is TokenFile with a leading ~ expanded.
func (a AccountConfig) TokenPath() (string, error) {
	p := strings.TrimSpace(a.TokenFile)
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p, nil
}

// Secret is the account's credential: the token file's contents,
// trimmed, or the API key variable's value. The error never carries the
// secret.
func (a AccountConfig) Secret() (string, error) {
	if a.issued != nil {
		if *a.issued == "" {
			return "", fmt.Errorf("account %s: the platform issued no secret", a.Label)
		}
		return *a.issued, nil
	}
	if a.ID != "" && a.TokenFile == "" && a.APIKeyEnv == "" {
		return "", fmt.Errorf("account %s: the platform issues its secret at claim only", a.Label)
	}
	switch a.Kind {
	case AccountSubscription:
		path, err := a.TokenPath()
		if err != nil {
			return "", fmt.Errorf("account %s: %w", a.Label, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("account %s: token file: %w", a.Label, err)
		}
		tok := strings.TrimSpace(string(data))
		if tok == "" {
			return "", fmt.Errorf("account %s: token file %s is empty", a.Label, path)
		}
		return tok, nil
	case AccountAPI:
		key := strings.TrimSpace(os.Getenv(a.APIKeyEnv))
		if key == "" {
			return "", fmt.Errorf("account %s: $%s is not set", a.Label, a.APIKeyEnv)
		}
		return key, nil
	}
	return "", fmt.Errorf("account %s: kind %q is not %s or %s", a.Label, a.Kind, AccountSubscription, AccountAPI)
}

// AccountSession is one session's hold on an account: the environment
// it runs with and the config dir that is its alone. A nil
// *AccountSession is a session on the operator's own login (no accounts
// configured): it adds nothing, unsets nothing and redacts nothing.
type AccountSession struct {
	Label string
	// ConfigDir is the session's CLAUDE_CONFIG_DIR, removed by Cleanup.
	ConfigDir string
	// Env is what the session adds: the credential, the config dir and
	// the label (AccountEnv).
	Env    []string
	secret string
}

// PrepareAccount makes a session's hold on an account: a fresh 0700
// temp dir for its CLAUDE_CONFIG_DIR, and the env that signs the CLI in
// — CLAUDE_CODE_OAUTH_TOKEN for a subscription, ANTHROPIC_API_KEY for an
// api account. The secret goes in the env only: nothing but the empty
// dir is written to disk.
func PrepareAccount(a AccountConfig, secret string) (*AccountSession, error) {
	if secret == "" {
		return nil, fmt.Errorf("account %s: no secret", a.Label)
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
		secret:    secret,
	}, nil
}

// OpenAccount is the account a session runs on: the one the loop chose
// for it (WithAccount, by ChooseAccount), else the first configured, or
// nil — the operator's own login — when none is. A chosen account the
// platform issued (its ID set) that is not among accounts is only the
// loop's prediction: its secret comes at the claim, never from here, so
// it is passed over for the first configured.
func OpenAccount(ctx context.Context, accounts []AccountConfig) (*AccountSession, error) {
	if len(accounts) == 0 {
		return nil, nil
	}
	a := chosenAmong(ctx, accounts)
	secret, err := a.Secret()
	if err != nil {
		return nil, err
	}
	return PrepareAccount(a, secret)
}

// chosenAmong is the account ctx carries — as accounts has it, when an
// issued one shares its label — else the first of accounts; an issued
// one accounts does not have is the first too. accounts is not empty.
func chosenAmong(ctx context.Context, accounts []AccountConfig) AccountConfig {
	a, ok := ChosenAccount(ctx)
	if !ok {
		return accounts[0]
	}
	if a.ID == "" {
		return a
	}
	if i := slices.IndexFunc(accounts, func(b AccountConfig) bool { return b.Label == a.Label }); i >= 0 {
		return accounts[i]
	}
	return accounts[0]
}

// Apply puts the account on a job: the inherited auth variables unset,
// its env beside the job's own, its secret among those redacted.
func (s *AccountSession) Apply(job *harness.Job) {
	if s == nil {
		return
	}
	job.Unset = append(job.Unset, AuthEnv...)
	job.Env = append(job.Env, s.Env...)
	job.Secrets = append(job.Secrets, s.secret)
}

// Redact is s with the account's secret taken out.
func (s *AccountSession) Redact(text string) string {
	if s == nil {
		return text
	}
	return redact(text, s.secret)
}

// RedactErr is err with the account's secret taken out of its message,
// still wrapping err for errors.Is and errors.As when there was nothing
// to take out.
func (s *AccountSession) RedactErr(err error) error {
	if err == nil || s == nil {
		return err
	}
	if msg := err.Error(); redact(msg, s.secret) != msg {
		return errors.New(redact(msg, s.secret))
	}
	return err
}

// Cleanup removes the session's config dir. It is safe on nil and to
// call twice.
func (s *AccountSession) Cleanup() {
	if s == nil || s.ConfigDir == "" {
		return
	}
	os.RemoveAll(s.ConfigDir)
}

// redact replaces every secret in s with harness.Redacted: what the
// node logs, reports and hands in as a failure passes through it.
func redact(s string, secrets ...string) string {
	return harness.Redact(s, secrets...)
}
