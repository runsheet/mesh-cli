package nodev1

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/egress"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// A lease's TLS termination (mesh-specs gap 31, and the ground gaps 28
// and 32 build on): the lease's tunnel answers the session's TLS for the
// hosts its rules name, with a CA of the lease's own, and splices every
// other host by SNI as before (internal/egress, intercept.go).
//
//   - The CA is per lease: made in the node's memory when the lease's
//     relay opens, its key never written anywhere and never handed to
//     the pod or a relay sidecar, gone with the lease's tunnel. Its
//     certificate is name-constrained to the rules' hosts and lives
//     egress.CALifetime; each host's certificate is minted on the first
//     request, an hour long. One per node would be a key every lease's
//     pod trusts, for as long as the node runs; one per lease trusts
//     nothing past the lease, costs one P-256 key, and no pod trusts
//     another lease's.
//   - The pod gets the certificate alone, in its HOME (trustFiles): the
//     node's system bundle with the CA appended, named by SSL_CERT_FILE
//     (Go, OpenSSL, Python's ssl, uv), CURL_CA_BUNDLE, REQUESTS_CA_BUNDLE
//     and PIP_CERT (Python's requests and pip), GIT_SSL_CAINFO and
//     CARGO_HTTP_CAINFO; and the CA alone in NODE_EXTRA_CA_CERTS, which
//     node and npm add to their own roots.
//   - Only a pod's session is pointed at the tunnel (HTTPS_PROXY): a
//     session on the host has no CA and nothing is terminated for it.
//     A relay sidecar's own upstream connections are spliced whatever
//     the rules (egress.SpliceOnly): its clients speak TLS to the host
//     on their own credentials.
//
// The one rule today is the registries': every request under a prefix
// of the brief's egress.registries goes on the lease's session token in
// the DPoP scheme with a fresh proof (egress.SignRule), audited as an
// AccessRecord {kind api, vendor registry, service signed, path the
// prefix}, never the token. A prefix whose host the brief's egress
// hosts do not name signs nothing: it never widens egress.

// interception is a lease's CA and the rules its tunnel terminates for;
// nil when the lease terminates nothing. env are the placeholders the
// pod holds for its injected credentials' tools (inject.go), and held
// every credential the relay holds for the lease.
type interception struct {
	ca    *egress.CA
	rules []egress.Rule
	env   []string
	held  []*heldCredential
	// inspect sees every terminated request for the lease's canaries
	// (canary.go): set when the lease injects anything
	inspect egress.Inspector
}

// watchRule terminates every host a lease's egress allows and writes
// nothing: a lease that injects credentials has every request it sends
// through its tunnel read for them (canary.go), so a value is seen on
// whatever host it is sent to.
type watchRule struct{ hosts []string }

func (w watchRule) Terminates(host string) bool { return slices.Contains(w.hosts, host) }

func (w watchRule) Apply(*http.Request) (*egress.Applied, error) { return nil, nil }

// signsRegistries says the brief names registries to sign.
func signsRegistries(b *meshapi.Brief) bool {
	return b.Egress != nil && len(b.Egress.Registries) > 0
}

// leaseInterception is the lease's interception: its registries' sign
// rule, kept to the prefixes its egress hosts allow, the rule of each
// credential its access carries (inject.go), and a CA over every rule's
// hosts. nil when the brief names nothing its hosts allow, or the lease
// runs on the host, where no session is pointed at its tunnel.
func (r *Runtime) leaseInterception(ctx context.Context, b *meshapi.Brief, tokens *sessionTokens, vals *injectedValues) (*interception, error) {
	if !signsRegistries(b) && !injects(b) {
		return nil, nil
	}
	if r.Pods == nil {
		r.logf("nodev1: lease %s: a session on the host is not pointed at its relay; its registry requests are not signed and no credential is injected", b.LeaseID)
		return nil, nil
	}
	if b.Egress == nil {
		r.logf("nodev1: lease %s: the kind declares no egress; its access's credentials are not injected", b.LeaseID)
		return nil, nil
	}
	allowed := egress.NewPolicy(b.Egress.Hosts, tenantHosts(b), nil)
	var prefixes []string
	for _, p := range b.Egress.Registries {
		u, err := url.Parse(p)
		if err != nil || u.Hostname() == "" {
			r.logf("nodev1: lease %s: registry prefix %q is not a URL; not signed", b.LeaseID, p)
			continue
		}
		port := u.Port()
		if port == "" {
			port = egress.Port
		}
		if ok, _, _ := allowed.Allow(u.Hostname(), port); !ok {
			r.logf("nodev1: lease %s: the kind's egress hosts do not name %s; registry prefix %s is not signed", b.LeaseID, u.Hostname(), p)
			continue
		}
		prefixes = append(prefixes, p)
	}
	if (len(prefixes) > 0 || injects(b)) && tokens == nil {
		return nil, credentialFailed(errors.New("the lease has no Latchkey session to sign its registry requests or read its credentials with"))
	}
	ic := &interception{}
	var hosts []string
	if len(prefixes) > 0 {
		sign, err := egress.NewSignRule(prefixes, tokens.Token, tokens.Key())
		if err != nil {
			return nil, err
		}
		ic.rules = append(ic.rules, sign)
		hosts = append(hosts, sign.Hosts()...)
	}
	if injects(b) {
		rules, env, all, err := r.injectRules(ctx, b, tokens, allowed, vals.add)
		ic.held = all
		if err != nil {
			return ic, err
		}
		for _, rule := range rules {
			hosts = append(hosts, rule.(*egress.InjectRule).Hosts()...)
		}
		ic.rules, ic.env = append(ic.rules, rules...), env
		if len(rules) > 0 && vals != nil {
			// every host the egress allows is read for the lease's
			// canaries, not only the injected ones
			var watched []string
			for _, h := range b.Egress.Hosts {
				if ok, _, _ := allowed.Allow(h, egress.Port); ok && validCAHost(h) {
					watched = append(watched, strings.ToLower(strings.TrimSuffix(h, ".")))
				}
			}
			ic.rules = append(ic.rules, watchRule{hosts: watched})
			hosts = append(hosts, watched...)
			ic.inspect = vals
		}
	}
	if len(ic.rules) == 0 {
		return ic, nil
	}
	slices.Sort(hosts)
	ca, err := egress.NewCA(b.LeaseID, slices.Compact(hosts), 0)
	if err != nil {
		return ic, err
	}
	ic.ca = ca
	return ic, nil
}

// install has t terminate the interception's hosts, each request
// audited on the lease.
func (ic *interception) install(r *Runtime, lease string, t *egress.Tunnel) {
	if ic == nil || ic.ca == nil || t == nil {
		return
	}
	t.CA, t.Rules, t.Inspect = ic.ca, ic.rules, ic.inspect
	t.Upstream = r.EgressUpstream
	t.Request = r.interceptAudit(lease)
}

// interceptAudit keeps each signed or injected request of lease's
// tunnel as an AccessRecord: the prefix, or the credential's ref, form
// and version, with the method, status and bytes — never a token, a
// value or the path past the prefix.
func (r *Runtime) interceptAudit(lease string) func(egress.Request) {
	return func(q egress.Request) {
		for _, a := range q.Applied {
			var rec meshapi.AccessRecord
			switch a.Rule {
			case egress.RuleRegistries:
				rec = meshapi.AccessRecord{Kind: "api", Vendor: "registry", Service: "signed", Path: a.Ref}
			case egress.RuleInject:
				// vendor names what the ref is (secret, connection),
				// service the form, path the ref and reason the version
				rec = meshapi.AccessRecord{Kind: "api", Vendor: a.Kind, Service: "inject " + a.Form, Path: a.Ref}
			default:
				continue
			}
			rec.Method, rec.Outcome, rec.Reason, rec.Bytes, rec.At = q.Method, "ok", strconv.Itoa(q.Status), q.Bytes, time.Now().UTC()
			if a.Version != "" {
				rec.Reason += " version " + a.Version
			}
			switch {
			case q.Refused == egress.CodePreset:
				rec.Outcome, rec.Reason = "refused", q.Refused
			case q.Refused != "":
				rec.Outcome, rec.Reason = "failed", q.Refused
			}
			r.keepAccess(lease, rec)
		}
	}
}

// Where the pod's trust files go, under the session's HOME.
const (
	trustCAFile     = ".mesh/trust/relay-ca.pem"
	trustBundleFile = ".mesh/trust/ca-bundle.pem"
)

// systemBundles are where a node's system roots are, the first found
// read (crypto/x509's own list, and macOS's).
var systemBundles = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/ssl/ca-bundle.pem",
	"/etc/pki/tls/cacert.pem",
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
	"/etc/ssl/cert.pem",
}

// systemBundle is the node's system roots, PEM: SSL_CERT_FILE's when
// the node has one, else the first of systemBundles; nil when none.
func systemBundle() []byte {
	paths := systemBundles
	if f := os.Getenv("SSL_CERT_FILE"); f != "" {
		paths = append([]string{f}, paths...)
	}
	for _, p := range paths {
		if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
			return b
		}
	}
	return nil
}

// trust puts the lease's CA in the pod's trust store: the files in its
// HOME and the variables that name them, added to what points the
// pod's clients at the relay. Without a system bundle on the node only
// node and npm (NODE_EXTRA_CA_CERTS) trust the relay, which is said.
func (p *leasePod) trust(ca *egress.CA, logf func(string, ...any), lease string) error {
	if ca == nil {
		return nil
	}
	caPath, err := p.sess.WriteFile(trustCAFile, ca.CertPEM())
	if err != nil {
		return err
	}
	vars := []string{"NODE_EXTRA_CA_CERTS=" + caPath}
	if sys := systemBundle(); sys != nil {
		bundle := append(append([]byte(nil), sys...), '\n')
		bundle = append(bundle, ca.CertPEM()...)
		path, err := p.sess.WriteFile(trustBundleFile, bundle)
		if err != nil {
			return err
		}
		for _, name := range trustBundleEnv {
			vars = append(vars, name+"="+path)
		}
	} else {
		logf("nodev1: lease %s: the node has no system CA bundle; only node and npm trust the relay's certificates", lease)
	}
	p.proxyEnv = append(p.proxyEnv, vars...)
	return nil
}

// trustBundleEnv name the bundle of system roots and the lease's CA for
// each client that reads one.
var trustBundleEnv = []string{
	"SSL_CERT_FILE",      // Go, OpenSSL (curl, Python's ssl), uv
	"CURL_CA_BUNDLE",     // curl
	"REQUESTS_CA_BUNDLE", // Python's requests
	"PIP_CERT",           // pip
	"GIT_SSL_CAINFO",     // git over https
	"CARGO_HTTP_CAINFO",  // cargo
}
