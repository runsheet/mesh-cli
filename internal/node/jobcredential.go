package node

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// A build Mesh picked an account for at claim (runsheet ADR 078
// decision 5) runs on that account, read from Latchkey's for-job route
// (Latchkey ADR 023 §3) by the build's own mesh session, never on one of
// the node's stored accounts:
//
//	POST <session token's issuer>/v1/credentials/for-job
//	Authorization: DPoP <mesh-session token>, DPoP: <proof by the node's key>
//
// Latchkey reads the job from Mesh by the session's id (a build job's id
// is its mesh session's) and answers the secret of the account Mesh
// picked, or refuses. A claim with no credential_ref keeps the node's
// own path (Credentials.Open's platform read and ChooseAccount).

type credentialRefKey struct{}

// WithCredentialRef marks ctx as a claim Mesh picked an account for.
func WithCredentialRef(ctx context.Context, ref string) context.Context {
	if ref == "" {
		return ctx
	}
	return context.WithValue(ctx, credentialRefKey{}, ref)
}

// CredentialRefFrom is the claim's credential_ref, "" when Mesh picked
// none.
func CredentialRefFrom(ctx context.Context) string {
	ref, _ := ctx.Value(credentialRefKey{}).(string)
	return ref
}

type claimedJobKey struct{}

// claimedJob is what the claim answered about the job beyond its
// subject: the Mesh job it opened and the account Mesh picked for it.
type claimedJob struct{ id, accountRef string }

// WithJob marks ctx as the claim job answered: its credential_ref, the
// Mesh job and the account Mesh picked. A brief that names none of them
// leaves ctx as it is.
func WithJob(ctx context.Context, job gateway.JobBrief) context.Context {
	ctx = WithCredentialRef(ctx, job.CredentialRef)
	if job.JobID == "" && job.AccountRef == "" {
		return ctx
	}
	return context.WithValue(ctx, claimedJobKey{}, claimedJob{id: job.JobID, accountRef: job.AccountRef})
}

// JobIDFrom is the Mesh job the claim opened, "" when it named none.
func JobIDFrom(ctx context.Context) string {
	j, _ := ctx.Value(claimedJobKey{}).(claimedJob)
	return j.id
}

// AccountRefFrom is the opaque reference of the account Mesh picked for
// the claim, "" when it picked none.
func AccountRefFrom(ctx context.Context) string {
	j, _ := ctx.Value(claimedJobKey{}).(claimedJob)
	return j.accountRef
}

type jobCredentialKey struct{}

// JobCredentialFunc reads the claimed job's credential from Latchkey.
type JobCredentialFunc func(ctx context.Context) (gateway.Credential, error)

// withJobCredential has Credentials.Open take the session's account from
// read rather than choose one of the platform's issued credentials.
func withJobCredential(ctx context.Context, read JobCredentialFunc) context.Context {
	return context.WithValue(ctx, jobCredentialKey{}, read)
}

func jobCredentialFrom(ctx context.Context) JobCredentialFunc {
	f, _ := ctx.Value(jobCredentialKey{}).(JobCredentialFunc)
	return f
}

// ErrForJobUnsupported is a Latchkey without the for-job route: the
// build falls back to the node's own path, said once.
var ErrForJobUnsupported = errors.New("latchkey serves no credentials/for-job")

// ForJobRefusal is Latchkey refusing the job's credential, with its
// status and reason; the build is released, never run on another
// account.
type ForJobRefusal struct {
	Status int
	Reason string
}

func (e *ForJobRefusal) Error() string {
	return fmt.Sprintf("latchkey refused the job's credential (%d): %s", e.Status, e.Reason)
}

// forJobAnswer is the for-job route's answer; Secret is never logged.
type forJobAnswer struct {
	Secret     string `json:"secret"`
	Vendor     string `json:"vendor"`
	Kind       string `json:"kind"`
	Plan       string `json:"plan"`
	AccountRef string `json:"account_ref"`
}

// credential is the answer as the platform's issued credential would
// name it: the account's opaque ref as its id and label, an Anthropic
// account on Claude Code and an OpenAI one on Codex, and every kind of
// subscription or OAuth token a subscription, an api key api.
func (a forJobAnswer) credential() (gateway.Credential, error) {
	cr := gateway.Credential{ID: a.AccountRef, Label: a.AccountRef, Secret: a.Secret}
	switch strings.ToLower(a.Vendor) {
	case "anthropic":
		cr.Adapter = AdapterClaudeCode
	case "openai":
		cr.Adapter = AdapterCodex
	default:
		return cr, fmt.Errorf("the job's account is a %q account, which no adapter here runs", a.Vendor)
	}
	switch strings.ToLower(a.Kind) {
	case "api_key":
		cr.Kind = AccountAPI
	case "subscription", "personal_subscription", "oauth_token":
		cr.Kind = AccountSubscription
	default:
		return cr, fmt.Errorf("the job's account is of kind %q, which the node does not run", a.Kind)
	}
	if a.Secret == "" {
		return cr, errors.New("latchkey answered the job's credential with no secret")
	}
	return cr, nil
}

// forJobTimeout bounds one for-job read.
const forJobTimeout = 20 * time.Second

// ForJob reads item's claimed job's credential from Latchkey's for-job
// route on the mesh session Open opened for the claim: a fresh session
// token, sent with a DPoP proof by the node's key (a nonce Latchkey asks
// for is answered once). A route Latchkey does not serve is
// ErrForJobUnsupported; a refusal a *ForJobRefusal.
func (b *SessionBearers) ForJob(ctx context.Context, item gateway.WorkItem) (gateway.Credential, error) {
	if b == nil || b.GW == nil || b.Key == nil {
		return gateway.Credential{}, errors.New("no mesh session to read the job's credential on")
	}
	b.mu.Lock()
	s := b.sessions[jobKey(item.ID, item.Attempts)]
	b.mu.Unlock()
	if s == nil {
		return gateway.Credential{}, errors.New("no mesh session open for the claim")
	}
	tok, _, _, err := b.GW.MeshSessionToken(ctx, s, MeshSessionAudience, b.Key)
	if err != nil {
		return gateway.Credential{}, fmt.Errorf("session token: %w", err)
	}
	iss, err := tokenIssuer(tok)
	if err != nil {
		return gateway.Credential{}, err
	}
	url := strings.TrimRight(iss, "/") + "/v1/credentials/for-job"
	client := b.HTTP
	if client == nil {
		client = &http.Client{Timeout: forJobTimeout}
	}
	nonce := ""
	for try := 0; try < 2; try++ {
		proof, err := b.Key.ProofWithNonce(http.MethodPost, url, tok, nonce)
		if err != nil {
			return gateway.Credential{}, fmt.Errorf("dpop proof: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
		if err != nil {
			return gateway.Credential{}, err
		}
		req.Header.Set("Authorization", "DPoP "+tok)
		req.Header.Set("DPoP", proof)
		resp, err := client.Do(req)
		if err != nil {
			return gateway.Credential{}, fmt.Errorf("credentials/for-job: %w", err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusOK:
			var a forJobAnswer
			if err := json.Unmarshal(body, &a); err != nil {
				return gateway.Credential{}, fmt.Errorf("credentials/for-job: answer: %w", err)
			}
			return a.credential()
		case resp.StatusCode == http.StatusUnauthorized && nonce == "" && resp.Header.Get("DPoP-Nonce") != "":
			nonce = resp.Header.Get("DPoP-Nonce")
			continue
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
			return gateway.Credential{}, ErrForJobUnsupported
		default:
			return gateway.Credential{}, &ForJobRefusal{Status: resp.StatusCode, Reason: refusalReason(body)}
		}
	}
	return gateway.Credential{}, &ForJobRefusal{Status: http.StatusUnauthorized, Reason: "the DPoP nonce was refused twice"}
}

// refusalReason is a refusal body's error text, or the body itself cut
// short; never a secret, which a refusal does not carry.
func refusalReason(body []byte) string {
	var e struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal(body, &e) == nil && (e.Error != "" || e.Description != "") {
		return strings.TrimSpace(e.Error + " " + e.Description)
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// tokenIssuer is a JWT's iss: the host it was minted on, which the
// for-job read is made to.
func tokenIssuer(tok string) (string, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return "", errors.New("session token: not a JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("session token: %w", err)
	}
	var c struct {
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(raw, &c); err != nil || !strings.HasPrefix(c.Iss, "https://") && !strings.HasPrefix(c.Iss, "http://") {
		return "", fmt.Errorf("session token: no issuer URL (%q)", c.Iss)
	}
	return c.Iss, nil
}
