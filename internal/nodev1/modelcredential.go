package nodev1

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// A job Mesh picked a model account for runs its session on that
// account, not on the node's own login (Mesh v1 spec, "Accounts" and
// "The git relay and the Latchkey token relay"; runsheet ADR 078
// decision 5). The brief's credential_ref names the account's grant;
// the node reads the credential on the lease's mesh-session token:
//
//	POST <Latchkey>/v1/credentials/for-job
//	Authorization: DPoP <mesh-session token>, DPoP: <proof>
//	→ {secret, vendor, kind, plan, account_ref, expires_at}
//
// Latchkey reads the job from Mesh, checks the lease is live and the
// grant is the job's product's, and answers the account's secret. The
// secret is the one credential that reaches a session, because the
// harness must hold it: it is in the session process's environment for
// that run alone, never written to disk, never in a checkout, and masked
// in anything the session's error carries. A brief with no
// credential_ref runs on the node's own login, as before.

// forJobPath is Latchkey's model-credential route.
const forJobPath = "/v1/credentials/for-job"

// modelAuthEnv are the variables that sign the Claude CLI in: none of
// the node's own reaches a session that runs on the job's account.
var modelAuthEnv = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}

// modelCredential is Latchkey's answer: the account's secret and what
// it is. Its String never names the secret.
type modelCredential struct {
	Secret     string    `json:"secret"`
	Vendor     string    `json:"vendor"`
	Kind       string    `json:"kind"`
	Plan       string    `json:"plan"`
	AccountRef string    `json:"account_ref"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func (m *modelCredential) String() string {
	return fmt.Sprintf("account %s (%s %s %s)", m.AccountRef, m.Vendor, m.Kind, m.Plan)
}

// apiKey is whether the account is charged per token (an api key), so
// the session's list cost is what it actually cost; a subscription's
// actual cost is nothing.
func (m *modelCredential) apiKey() bool {
	return m != nil && strings.EqualFold(m.Kind, "api_key")
}

// env is the variable that signs the Claude CLI in on the account: an
// Anthropic subscription's OAuth token as CLAUDE_CODE_OAUTH_TOKEN, an
// api key as ANTHROPIC_API_KEY. Any other vendor or kind is an account
// this node's harness cannot run.
func (m *modelCredential) env() (string, error) {
	if m.Secret == "" {
		return "", errors.New("Latchkey answered the job's model credential with no secret")
	}
	if !strings.EqualFold(m.Vendor, "anthropic") {
		return "", fmt.Errorf("the job's %s is a %q account, which the node's claude-code harness cannot run", m, m.Vendor)
	}
	switch strings.ToLower(m.Kind) {
	case "subscription", "personal_subscription", "oauth_token":
		return "CLAUDE_CODE_OAUTH_TOKEN=" + m.Secret, nil
	case "api_key":
		return "ANTHROPIC_API_KEY=" + m.Secret, nil
	}
	return "", fmt.Errorf("the job's %s is of kind %q, which the node does not run", m, m.Kind)
}

// ModelCredential reads the job's model credential on the session token.
func (s *sessionTokens) ModelCredential(ctx context.Context) (*modelCredential, error) {
	var out modelCredential
	if err := s.onToken(ctx, http.MethodPost, forJobPath, nil, &out); err != nil {
		return nil, fmt.Errorf("the job's model credential: %w", err)
	}
	if out.Secret == "" {
		return nil, errors.New("the job's model credential: Latchkey answered no secret")
	}
	return &out, nil
}

// sessionAccount is what a session runs on: the job's model account, or
// the node's own login when the brief names none.
type sessionAccount struct {
	cred *modelCredential
}

// open reads the brief's account when it names one.
func openAccount(ctx context.Context, credentialRef string, tokens *sessionTokens) (*sessionAccount, error) {
	if credentialRef == "" {
		return &sessionAccount{}, nil
	}
	if tokens == nil {
		return nil, errors.New("the brief names a model account but the lease has no session to read it on")
	}
	cred, err := tokens.ModelCredential(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := cred.env(); err != nil {
		return nil, err
	}
	return &sessionAccount{cred: cred}, nil
}

// ref is the account's opaque ref; "" on the node's own login.
func (a *sessionAccount) ref() string {
	if a == nil || a.cred == nil {
		return ""
	}
	return a.cred.AccountRef
}

// apply puts the account into the session's spec: its variable in the
// session's environment, the node's own model variables dropped, and
// the secret masked. The node's own login is left alone.
func (a *sessionAccount) apply(spec *SessionSpec) {
	if a == nil || a.cred == nil {
		return
	}
	kv, _ := a.cred.env() // checked by openAccount
	spec.Unset = append(slices.Clone(spec.Unset), modelAuthEnv...)
	spec.Env = append(spec.Env, kv)
	spec.Secrets = append(spec.Secrets, a.cred.Secret)
}
