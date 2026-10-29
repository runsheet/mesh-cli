package nodev1

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
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
// secret never reaches the session (projectmesh-io/api ADR 001
// amendment "model relay", 2026-10-30): the lease's relay holds it in
// memory and adds it to each model request the session makes through
// the sessionproxy's /model/<provider>/ route (modelrelay.go). The
// session holds a fixed dummy in the variable its harness reads, and
// its base URL names the relay. A brief with no credential_ref runs on
// the node's own login, as before; a key of the node's environment is
// relayed the same way, never passed on.

// forJobPath is Latchkey's model-credential route.
const forJobPath = "/v1/credentials/for-job"

// modelAuthEnv are the variables a harness reads a model credential or
// its model's address from: none of the node's own reaches a v1
// session. The relay's own (relayEnv) are set in their place.
var modelAuthEnv = harness.ModelEnv

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

// auth is how the account's secret travels: the relay's header and the
// variable the session's harness holds the dummy in. Any other vendor
// or kind is an account this node's claude-code harness cannot run.
func (m *modelCredential) auth() (modelAuth, error) {
	if m.Secret == "" {
		return modelAuth{}, errors.New("Latchkey answered the job's model credential with no secret")
	}
	if !strings.EqualFold(m.Vendor, "anthropic") {
		return modelAuth{}, fmt.Errorf("the job's %s is a %q account, which the node's claude-code harness cannot run", m, m.Vendor)
	}
	a, ok := authOf(m.Vendor, m.Kind)
	if !ok {
		return modelAuth{}, fmt.Errorf("the job's %s is of kind %q, which the node does not run", m, m.Kind)
	}
	return a, nil
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

// openAccount reads the brief's account when it names one; else the
// node's own login, relayed when it is a key in the node's environment
// (environ), or left to the CLI's own sign-in on the node's disk.
func openAccount(ctx context.Context, credentialRef string, tokens *sessionTokens, environ []string) (*sessionAccount, error) {
	if credentialRef == "" {
		return ownLogin(environ), nil
	}
	if tokens == nil {
		return nil, errors.New("the brief names a model account but the lease has no session to read it on")
	}
	cred, err := tokens.ModelCredential(ctx)
	if err != nil {
		return nil, err
	}
	a, err := cred.auth()
	if err != nil {
		return nil, err
	}
	return &sessionAccount{cred: cred, auth: a, reread: tokens.ModelCredential}, nil
}
