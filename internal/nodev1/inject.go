package nodev1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/egress"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// The relay injects the credentials a job's offer carried (mesh-specs
// gap 28; ADR-046): each Latchkey Secret ref in access.secrets and each
// scoped connection in access.mint is asked of Latchkey on the lease's
// session token —
//
//	POST <Latchkey>/v1/credentials/for-job {credential_ref}
//	Authorization: DPoP <mesh-session token>, DPoP: <proof>
//	→ {inject, fields, version?, expires_at, revocable?, connection?, service?}
//
// — and becomes an egress.InjectRule on the lease's tunnel over the
// rule's hosts that the kind's egress allows, never another. The answer
// is held in this process's memory alone: never on disk, in a log line,
// an event, an AccessRecord or evidence, and never in the pod, which
// holds egress.Placeholder where a tool insists on a variable.
//
// Every ref is asked once as the lease opens, so a credential Latchkey
// will not give hands the lease back before the session runs — cause
// credential_timeout when Latchkey or the vendor could not be reached,
// product when Latchkey refused the product's configuration (a grant, a
// ref off the job, a connection to reconnect: mesh-specs gap 14). After
// that it is asked again on the first request past its expires_at, or
// past injectReask since it was last asked, so a grace-0 rotation
// reaches a running job; a re-ask that fails drops what was held and
// refuses the rule's requests 502 until one succeeds.

// injectReask is the longest a held credential is used before Latchkey
// is asked again (latchkey ADR 031 §2.4's cache_for).
const injectReask = 15 * time.Minute

// The kinds of ref the relay injects.
const (
	injectSecret     = "secret"
	injectConnection = "connection"
)

// forJobCredential is Latchkey's for-job answer for a Secret or a
// connection's child. It is never logged: String names the ref alone.
type forJobCredential struct {
	Inject     egress.InjectSpec `json:"inject"`
	Fields     map[string]string `json:"fields"`
	Version    json.RawMessage   `json:"version,omitempty"`
	ExpiresAt  time.Time         `json:"expires_at"`
	Revocable  bool              `json:"revocable,omitempty"`
	Connection string            `json:"connection,omitempty"`
	Service    string            `json:"service,omitempty"`
}

// version is the answer's version as text: a Secret's number, "" for a
// connection's child, which has none.
func (c *forJobCredential) version() string {
	v := strings.Trim(strings.TrimSpace(string(c.Version)), `"`)
	if v == "null" {
		return ""
	}
	return v
}

// CredentialForJob asks Latchkey for ref's credential on the session
// token. A refusal is a *latchkeyError carrying Latchkey's status and
// code.
func (s *sessionTokens) CredentialForJob(ctx context.Context, ref string) (*forJobCredential, error) {
	var out forJobCredential
	if err := s.onToken(ctx, http.MethodPost, forJobPath, map[string]string{"credential_ref": ref}, &out); err != nil {
		return nil, err
	}
	if len(out.Fields) == 0 {
		return nil, errors.New("Latchkey answered " + ref + " with no fields")
	}
	return &out, nil
}

// latchkeyError is Latchkey refusing a call on the session token: its
// status and the code it named.
type latchkeyError struct {
	Method, Path string
	Status       int
	Code         string
	Description  string
}

func (e *latchkeyError) Error() string {
	msg := strings.TrimSpace(e.Code + ": " + e.Description)
	if e.Code == "" && e.Description == "" {
		msg = ""
	}
	return fmt.Sprintf("Latchkey %s %s: %d %s", e.Method, e.Path, e.Status, msg)
}

// codeJobNotLive is Latchkey's 403 for a session whose lease is no
// longer the job's live one (latchkey #697): the lease is gone, which is
// not the product's configuration.
const codeJobNotLive = "job_not_live"

// productsConfig says a Latchkey refusal is the product's configuration
// — a grant, a ref off the job (not_the_jobs, purpose_not_granted), a
// secret missing, not injectable, disabled or expired (secret_not_found,
// secret_not_injectable, secret_disabled, secret_expired), a connection
// to reconnect, a scope outside the grant — not the infrastructure's: a
// 4xx other than an unaccepted token (401), a rate limit (429), or
// job_not_live, the lease having ended under the ask.
func (e *latchkeyError) productsConfig() bool {
	return e.Status >= 400 && e.Status < 500 && e.Status != http.StatusUnauthorized && e.Status != http.StatusTooManyRequests &&
		e.Code != codeJobNotLive
}

// injectRefusal is how a credential that could not be had refuses a
// request: credential_refused with Latchkey's code when it answered one,
// else credential_unavailable.
func injectRefusal(ref string, err error) *egress.Refusal {
	var le *latchkeyError
	if errors.As(err, &le) && le.Code != "" && le.Status != http.StatusServiceUnavailable && le.Status != http.StatusUnauthorized {
		return &egress.Refusal{Status: http.StatusBadGateway, Code: egress.CodeRefused,
			Message: fmt.Sprintf("Latchkey refused %s: %s (%d)", ref, le.Code, le.Status)}
	}
	return &egress.Refusal{Status: http.StatusBadGateway, Code: egress.CodeUnavailable,
		Message: "the credential for " + ref + " could not be had from Latchkey"}
}

// injectFailed is ref's credential not had as the lease opens: a
// credential not minted, the product's to fix when Latchkey refused its
// configuration (causes.go).
func injectFailed(ref string, err error) error {
	var le *latchkeyError
	ce := &credentialError{err: fmt.Errorf("the credential %s the job's access names: %w", ref, err)}
	if errors.As(err, &le) && le.productsConfig() {
		ce.product = true
	}
	return ce
}

// heldCredential is one ref's credential as the lease's relay holds it:
// an egress.InjectSource.
type heldCredential struct {
	ref, kind string
	ask       func(ctx context.Context, ref string) (*forJobCredential, error)
	now       func() time.Time
	// held hears every credential as it is taken, before it is used: the
	// lease's redactor (ADR-046)
	held func(ref string, c *egress.Credential)
	logf func(string, ...any)
	// service is the connection's vendor, as Latchkey named it
	service string

	mu      sync.Mutex
	spec    egress.InjectSpec
	cred    *egress.Credential
	asked   time.Time
	version string
	lastErr error
}

// failure is why the last ask failed, nil when it did not.
func (h *heldCredential) failure() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastErr
}

// cmpErr is the first error that is not nil.
func cmpErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// Spec is the rule Latchkey last answered.
func (h *heldCredential) Spec() egress.InjectSpec {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.spec
}

// Credential is the credential of the moment, asked again when due.
func (h *heldCredential) Credential(ctx context.Context) (*egress.Credential, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	if h.cred != nil && now.Before(h.due()) {
		return h.cred, nil
	}
	// a credential past its time is dropped whether or not the re-ask
	// gives another: nothing stale goes out
	h.cred = nil
	ans, err := h.ask(ctx, h.ref)
	if err == nil {
		if why := ans.Inject.Check(); why != "" {
			err = errors.New(why)
		}
	}
	if err != nil {
		h.lastErr = err
		h.logf("nodev1: credential %s (%s) could not be had: %s", h.ref, h.kind, err)
		return nil, injectRefusal(h.ref, err)
	}
	h.lastErr = nil
	c := &egress.Credential{Ref: h.ref, Spec: ans.Inject, Fields: ans.Fields, Version: ans.version(), ExpiresAt: ans.ExpiresAt}
	if h.held != nil {
		h.held(h.ref, c)
	}
	if h.version != "" && c.Version != h.version {
		h.logf("nodev1: credential %s is now version %s (was %s)", h.ref, c.Version, h.version)
	}
	h.spec, h.cred, h.asked, h.version = ans.Inject, c, now, c.Version
	if ans.Service != "" {
		h.service = ans.Service
	}
	return c, nil
}

// due is when the held credential is asked for again: injectReask after
// it was asked, or refreshSkew before it expires, whichever is first.
func (h *heldCredential) due() time.Time {
	due := h.asked.Add(injectReask)
	if h.cred != nil && !h.cred.ExpiresAt.IsZero() {
		if exp := h.cred.ExpiresAt.Add(-refreshSkew); exp.Before(due) {
			due = exp
		}
	}
	return due
}

// placeholderEnv are the variables a service's own tools read their
// credential from, which the session holds as the Placeholder so the
// tool sends it and the relay replaces it (latchkey ADR 027 §9's
// minters' services).
func placeholderEnv(service string) []string {
	switch strings.ToLower(service) {
	case "cloudflare":
		return []string{"CLOUDFLARE_API_TOKEN"}
	case "github":
		return []string{"GH_TOKEN", "GITHUB_TOKEN"}
	case "gitlab":
		return []string{"GITLAB_TOKEN"}
	}
	return nil
}

// carriedCredentials are the refs of the brief's access the relay
// injects, each with its kind, in the access's order.
func carriedCredentials(b *meshapi.Brief) [][2]string {
	if b.Access == nil {
		return nil
	}
	var out [][2]string
	for _, ref := range b.Access.Secrets {
		out = append(out, [2]string{ref, injectSecret})
	}
	for _, m := range b.Access.Mint {
		out = append(out, [2]string{m.ConnectionRef, injectConnection})
	}
	return out
}

// injects says the brief carries credentials for the relay to inject.
func injects(b *meshapi.Brief) bool { return len(carriedCredentials(b)) > 0 }

// injectRules asks Latchkey for each carried credential and answers its
// rule over the hosts of allowed that the credential's rule names, the
// placeholders its tools read, and every credential held. A ref whose
// hosts the egress names none of is held (its child is released at the
// lease's end) but injected nowhere, which is said.
func (r *Runtime) injectRules(ctx context.Context, b *meshapi.Brief, tokens *sessionTokens, allowed *egress.Policy, held func(string, *egress.Credential)) ([]egress.Rule, []string, []*heldCredential, error) {
	var (
		rules []egress.Rule
		env   []string
		all   []*heldCredential
	)
	for _, rc := range carriedCredentials(b) {
		ref, kind := rc[0], rc[1]
		h := &heldCredential{ref: ref, kind: kind, ask: tokens.CredentialForJob, now: time.Now, held: held, logf: r.logf}
		if _, err := h.Credential(ctx); err != nil {
			// the cause is Latchkey's own answer, not how a request
			// would be refused
			return nil, nil, all, injectFailed(ref, cmpErr(h.failure(), err))
		}
		all = append(all, h)
		spec := h.Spec()
		var hosts []string
		for _, host := range b.Egress.Hosts {
			if ok, _, _ := allowed.Allow(host, egress.Port); ok && spec.MatchesHost(host) {
				hosts = append(hosts, host)
			}
		}
		if len(hosts) == 0 {
			r.logf("nodev1: lease %s: the kind's egress hosts name none of %s's hosts (%s); it is not injected", b.LeaseID, ref, strings.Join(spec.Hosts, ", "))
			continue
		}
		rules = append(rules, egress.NewInjectRule(ref, kind, h, hosts))
		env = append(env, spec.Env...)
		env = append(env, placeholderEnv(h.service)...)
		r.logf("nodev1: lease %s: injects %s (%s, %s) on %s", b.LeaseID, ref, kind, spec.FormOf(), strings.Join(hosts, ", "))
	}
	slices.Sort(env)
	var vars []string
	for _, name := range slices.Compact(env) {
		if validEnvName(name) {
			vars = append(vars, name+"="+egress.Placeholder)
		}
	}
	return rules, vars, all, nil
}

// validEnvName says name is a variable a placeholder may be set as: not
// one the node itself sets for the session.
func validEnvName(name string) bool {
	if name == "" || strings.ContainsAny(name, "= \t\n\x00") {
		return false
	}
	switch strings.ToUpper(name) {
	case "PATH", "HOME", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS":
		return false
	}
	return true
}
