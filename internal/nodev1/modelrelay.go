package nodev1

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/egress"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/sessionproxy"
)

// No model key in the session (projectmesh-io/api ADR 001 amendment
// "model relay", 2026-10-30). A lease's account — the job's, read from
// Latchkey on the session token, or a key in the node's own environment
// — stays in the node's memory. The lease's sessionproxy serves it at
// /model/<provider>/ (sessionproxy.Model), stripping what the session
// sent and setting the real credential on each request; the session's
// environment names the relay as its harness's base URL and holds a
// fixed dummy in the variable the harness reads, so the harness signs
// in the way the credential's kind asks (a subscription's OAuth bearer,
// or an API key), and the dummy is all a session can ever leak.
// CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC keeps Claude Code from
// opening any connection but the relay's.

// ModelDummy is the credential a session's harness holds in place of
// the real one. It is no secret; the relay drops it.
const ModelDummy = "mesh-relay-dummy"

// ModelUpstreamEnv names a test's override of a provider's origin
// (MESH_MODEL_UPSTREAM_ANTHROPIC, MESH_MODEL_UPSTREAM_OPENAI).
const ModelUpstreamEnv = "MESH_MODEL_UPSTREAM_"

// refreshSkew reads a credential again this long before it expires.
const refreshSkew = 30 * time.Second

// modelAuth is how a credential travels: the provider whose route it
// serves, the header the relay sets upstream (Authorization as a bearer,
// or X-Api-Key), and the variable the session's harness holds the dummy
// in. baseURL and headers are the account's own, when it names them: an
// account behind a gateway of its own, or one that wants a static header
// (an org id) on every call (ADR-050, ADR-046).
type modelAuth struct {
	provider string
	header   string
	bearer   bool
	envVar   string
	baseURL  string
	headers  map[string]string
}

// modelVendor is whether authOf recognizes vendor, so a vendor it does
// not run at all is told apart from a known vendor's unsupported kind.
func modelVendor(vendor string) bool {
	switch strings.ToLower(vendor) {
	case "anthropic", "openai":
		return true
	}
	return false
}

// authOf is the travel of a vendor's credential of a kind; false when
// the relay does not carry it.
func authOf(vendor, kind string) (modelAuth, bool) {
	switch strings.ToLower(vendor) {
	case "anthropic":
		switch strings.ToLower(kind) {
		case "subscription", "personal_subscription", "oauth_token":
			return modelAuth{provider: "anthropic", header: "Authorization", bearer: true, envVar: "CLAUDE_CODE_OAUTH_TOKEN"}, true
		case "auth_token":
			return modelAuth{provider: "anthropic", header: "Authorization", bearer: true, envVar: "ANTHROPIC_AUTH_TOKEN"}, true
		case "api_key":
			return modelAuth{provider: "anthropic", header: "X-Api-Key", envVar: "ANTHROPIC_API_KEY"}, true
		}
	case "openai":
		// Codex's account: a ChatGPT Pro subscription's OAuth token or
		// an API key, both a bearer on OpenAI's API. The harness is
		// claude-code, so a v1 node runs one only behind an account's
		// own base_url (an OpenAI-compatible gateway), but the relay
		// carries it all the same.
		switch strings.ToLower(kind) {
		case "subscription", "personal_subscription", "oauth_token", "api_key":
			return modelAuth{provider: "openai", header: "Authorization", bearer: true, envVar: "OPENAI_API_KEY"}, true
		}
	}
	return modelAuth{}, false
}

// relayEnv is the session's environment for a relayed credential: the
// harness's base URL on the relay, its dummy, and for Claude Code no
// traffic but the relay's.
func (a modelAuth) relayEnv(base string) []string {
	switch a.provider {
	case "openai":
		return []string{
			"OPENAI_BASE_URL=" + base + sessionproxy.ModelPrefix + "openai/v1",
			"OPENAI_API_KEY=" + ModelDummy, "CODEX_API_KEY=" + ModelDummy,
		}
	}
	return []string{
		"ANTHROPIC_BASE_URL=" + base + sessionproxy.ModelPrefix + "anthropic",
		a.envVar + "=" + ModelDummy,
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	}
}

// sessionAccount is what a session runs on: the job's model account,
// the node's own key, or (neither set) the CLI's own sign-in on the
// node's disk, which is not relayed.
type sessionAccount struct {
	auth modelAuth
	// reread reads the job's account again (Latchkey's for-job route);
	// nil for the node's own key, which does not change under a lease.
	reread func(ctx context.Context) (*modelCredential, error)
	// upstream is where the account's calls go: the node's own
	// ANTHROPIC_BASE_URL for its own key, or the job's account's own
	// base_url (ADR-050); "" is the provider's default.
	upstream string
	// base is the relay's address as the session reaches it, once
	// served.
	base string
	// resolve looks up an account base_url's host (upstreamsWith); nil
	// is net.DefaultResolver. A test's own answers.
	resolve func(ctx context.Context, host string) ([]net.IPAddr, error)

	// inspect sees each model request's body: the lease's canaries
	// (canary.go); nil sees none
	inspect func([]byte) error
	// onRefused hears the first refusal of the job's account by its
	// vendor (accountrefused.go): it ends the lease. nil hears none.
	onRefused func(error)
	mu        sync.Mutex
	cred      *modelCredential // the job's account; nil otherwise
	own       string           // the node's own key
	refusal   *accountRefusedError
}

// ownLogin is the node's own login, as its environment holds it: the
// variable Claude Code itself would sign in with (ANTHROPIC_AUTH_TOKEN,
// then ANTHROPIC_API_KEY, then CLAUDE_CODE_OAUTH_TOKEN). None is the
// CLI's own sign-in, which nothing relays.
func ownLogin(environ []string) *sessionAccount {
	get := func(name string) string {
		for i := len(environ) - 1; i >= 0; i-- {
			if k, v, ok := strings.Cut(environ[i], "="); ok && k == name {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	for _, c := range []struct{ env, kind string }{
		{"ANTHROPIC_AUTH_TOKEN", "auth_token"}, {"ANTHROPIC_API_KEY", "api_key"}, {"CLAUDE_CODE_OAUTH_TOKEN", "oauth_token"},
	} {
		if v := get(c.env); v != "" {
			a, _ := authOf("anthropic", c.kind)
			return &sessionAccount{auth: a, own: v, upstream: get("ANTHROPIC_BASE_URL")}
		}
	}
	return &sessionAccount{}
}

// relays is whether the relay carries the account's credential.
func (a *sessionAccount) relays() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cred != nil || a.own != ""
}

// ref is the account's opaque ref; "" on the node's own login.
func (a *sessionAccount) ref() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cred == nil {
		return ""
	}
	return a.cred.AccountRef
}

// apiKey is whether the job's account is charged per token.
func (a *sessionAccount) apiKey() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cred.apiKey()
}

// credential is the account's credential for one request on provider's
// route: read again from Latchkey when fresh (the upstream refused the
// last one) or about to expire. A provider not the account's is refused,
// so a session cannot spend the account on another vendor's API.
func (a *sessionAccount) credential(ctx context.Context, provider string, fresh bool) (sessionproxy.ModelCredential, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if provider != a.auth.provider {
		return sessionproxy.ModelCredential{}, fmt.Errorf("%w: the lease's model account is not for %s", sessionproxy.ErrModelRefused, provider)
	}
	secret := a.own
	if a.cred != nil {
		expiring := !a.cred.ExpiresAt.IsZero() && time.Until(a.cred.ExpiresAt) < refreshSkew
		if (fresh || expiring) && a.reread != nil {
			cred, err := a.reread(ctx)
			if err != nil {
				return sessionproxy.ModelCredential{}, err
			}
			auth, err := cred.auth()
			if err != nil {
				return sessionproxy.ModelCredential{}, err
			}
			if auth.provider != a.auth.provider {
				return sessionproxy.ModelCredential{}, fmt.Errorf("the job's account is now a %s account", auth.provider)
			}
			// the route was checked and fixed on the base_url the lease
			// opened with (upstreamsWith); a reread naming another is
			// never sent the credential
			if auth.baseURL != a.auth.baseURL {
				return sessionproxy.ModelCredential{}, fmt.Errorf("the job's %s now names another base_url, which the lease's model route was not checked for", cred)
			}
			a.cred, a.auth = cred, auth
		}
		secret = a.cred.Secret
	}
	v := secret
	if a.auth.bearer {
		v = "Bearer " + secret
	}
	return sessionproxy.ModelCredential{Header: a.auth.header, Value: v, Extra: a.auth.headers}, nil
}

// serve has p carry the account's model calls, reaching upstreams
// (each provider's origin, else sessionproxy's defaults, else the
// node's own base for its own key), and logs each call against the
// lease. A host session reaches it on loopback (ListenModel); base is
// that address.
func (a *sessionAccount) serve(p *sessionproxy.Proxy, upstreams map[string]string, logf func(string, ...any), lease string) error {
	if !a.relays() {
		return nil
	}
	ups, err := a.upstreamsWith(upstreams)
	if err != nil {
		return fmt.Errorf("the lease's model relay: %w", err)
	}
	p.Model = a.model(ups, logf, lease)
	base, err := p.ListenModel()
	if err != nil {
		return fmt.Errorf("the lease's model relay: %w", err)
	}
	a.base = base
	return nil
}

// forward has p, the node's side of a lease's socket to its relay
// sidecar, carry the account's model calls on the forwarder's paths
// alone (sessionproxy.ForwarderPaths): the one place the node reads HTTP
// a pod sent, since no lease-scoped model token exists to hand the
// sidecar. base is where the session reaches it: the sidecar's loopback.
func (a *sessionAccount) forward(p *sessionproxy.Proxy, upstreams map[string]string, logf func(string, ...any), lease, base string) error {
	if !a.relays() {
		return nil
	}
	ups, err := a.upstreamsWith(upstreams)
	if err != nil {
		return fmt.Errorf("the lease's model relay: %w", err)
	}
	m := a.model(ups, logf, lease)
	m.Paths = sessionproxy.ForwarderPaths
	p.Model = m
	a.base = base
	return nil
}

// upstreamsWith are each provider's origin: sessionproxy's defaults, the
// node's own base for its own key, then upstreams (which stand in for
// each provider's API), then the job's account's own base_url (ADR-050):
// an account naming its upstream is never sent to the provider's API.
// That base_url is Latchkey's answer, not the operator's, so it is held
// to accountUpstream before the credential is sent there.
func (a *sessionAccount) upstreamsWith(upstreams map[string]string) (map[string]string, error) {
	ups := map[string]string{}
	for k, v := range sessionproxy.DefaultModelUpstreams {
		ups[k] = v
	}
	own := a.cred == nil
	if a.upstream != "" && own {
		ups[a.auth.provider] = a.upstream
	}
	for k, v := range upstreams {
		ups[k] = v
	}
	if a.upstream != "" && !own {
		if err := a.accountUpstream(upstreams); err != nil {
			return nil, err
		}
		ups[a.auth.provider] = a.upstream
	}
	return ups, nil
}

// accountUpstreamWait bounds the lookup of an account base_url's host.
const accountUpstreamWait = 10 * time.Second

// accountUpstream is whether the job's account's own base_url may carry
// its credential: an https URL with a host and no userinfo whose host
// resolves only to public addresses (egress.PublicIP: no loopback,
// private, link-local, CGNAT, multicast or unspecified range), so a bad
// or tampered base_url cannot send the account's secret into the node's
// LAN or host. A base_url on the origin of one of the operator's own
// upstreams (Runtime.ModelUpstreams, $MESH_MODEL_UPSTREAM_<PROVIDER>) is
// the operator's choice and allowed as it is.
func (a *sessionAccount) accountUpstream(operator map[string]string) error {
	refuse := func(why string) error {
		return fmt.Errorf("the job's %s names base_url %q, which %s; the relay does not send its credential there", a.cred, a.upstream, why)
	}
	u, err := url.Parse(a.upstream)
	if err != nil || u.Host == "" || u.User != nil {
		return refuse("is not a URL with a host")
	}
	for _, v := range operator {
		if o, err := url.Parse(v); err == nil && o.Host != "" && strings.EqualFold(o.Scheme, u.Scheme) && strings.EqualFold(o.Host, u.Host) {
			return nil
		}
	}
	if u.Scheme != "https" {
		return refuse("is not https")
	}
	host := u.Hostname()
	var ips []net.IPAddr
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IPAddr{{IP: ip}}
	} else {
		resolve := a.resolve
		if resolve == nil {
			resolve = net.DefaultResolver.LookupIPAddr
		}
		ctx, cancel := context.WithTimeout(context.Background(), accountUpstreamWait)
		defer cancel()
		ips, err = resolve(ctx, host)
		if err != nil {
			return refuse("does not resolve: " + err.Error())
		}
	}
	if len(ips) == 0 {
		return refuse("resolves to no address")
	}
	for _, ip := range ips {
		if !egress.PublicIP(ip.IP) {
			return refuse(fmt.Sprintf("reaches %s, not a public address", ip.IP))
		}
	}
	return nil
}

// model is the account's model route to ups, logging each call against
// the lease.
func (a *sessionAccount) model(ups map[string]string, logf func(string, ...any), lease string) *sessionproxy.Model {
	return &sessionproxy.Model{
		Upstreams:  ups,
		Credential: a.credential,
		// the lease's injected values are canaries in every model
		// request (canary.go)
		Inspect: a.inspect,
		Log: func(c sessionproxy.ModelCall) {
			line := fmt.Sprintf("nodev1: lease %s: model %s %s %s answered %d, %d bytes in %s", lease, c.Provider, c.Method, c.Path, c.Status, c.Bytes, c.Latency.Round(time.Millisecond))
			if c.Refreshed {
				line += ", after reading the credential again"
			}
			if c.Err != "" {
				line += ": " + c.Err
			}
			// the vendor's own error on a 4xx or 5xx: its type and message,
			// bounded and redacted by the relay (ADR-048)
			if c.Status >= 400 && (c.ErrorType != "" || c.ErrorMessage != "") {
				line += ": " + vendorErrorLine(c.ErrorType, c.ErrorMessage)
			}
			logf("%s", line)
			a.noteRefusal(c, logf, lease)
		},
	}
}

// apply puts the account into the session's spec. None of the node's
// own model variables reach it, ever; a relayed account adds the
// relay's base URL and the dummy. No secret is in the spec.
func (a *sessionAccount) apply(spec *SessionSpec) {
	spec.Unset = append(slices.Clone(spec.Unset), modelAuthEnv...)
	if a.relays() && a.base != "" {
		spec.Env = append(spec.Env, a.auth.relayEnv(a.base)...)
	}
}

// podEnv is env for a session in the lease's pod: the relay's address
// the account was served on (the host's loopback, which the pod cannot
// reach) replaced by base, the pod's bridge onto the lease's socket,
// whose sessionproxy serves the same /model/ route. The dummy and
// CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC stay as they are.
func (a *sessionAccount) podEnv(env []string, base string) []string {
	if !a.relays() || a.base == "" {
		return env
	}
	return sandbox.SetEnv(env, a.auth.relayEnv(base))
}

// modelUpstreams are the test overrides of each provider's origin in
// the node's environment (ModelUpstreamEnv).
func modelUpstreams() map[string]string {
	out := map[string]string{}
	for p := range sessionproxy.DefaultModelUpstreams {
		if v := strings.TrimSpace(os.Getenv(ModelUpstreamEnv + strings.ToUpper(p))); v != "" {
			out[p] = v
		}
	}
	return out
}
