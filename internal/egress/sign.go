package egress

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// RuleRegistries names SignRule in an Applied and the audit.
const RuleRegistries = "egress.registries"

// Signer makes a DPoP proof of one request for a token (dpop.Key).
type Signer interface {
	Proof(method, rawURL, token string) (string, error)
}

// SignRule signs the requests under a lease's registry prefixes with
// its session (mesh-specs gap 31): runsheet serves npm, OCI and
// artifacts at https://api.runsheet.dev/{npm,v2,artifacts}/ and takes a
// job's mesh-session token only in the DPoP scheme, which npm, docker,
// crane and curl cannot send. Every request whose URL is under a prefix
// goes on with `Authorization: DPoP <the session token of the moment>`
// and a fresh `DPoP` proof of it (htm the method, htu the URL with its
// path escaped as sent, ath the token's hash), whatever Authorization
// or DPoP the session sent; a request to the same host outside every
// prefix goes on as the session sent it.
type SignRule struct {
	// Token is the lease's session token, asked on every request, so a
	// refreshed token is used from the next one on.
	Token func(ctx context.Context) (string, error)
	// Key signs each request's proof.
	Key Signer

	prefixes []prefix
}

// prefix is one registry prefix: https, its host and port, its path.
type prefix struct {
	raw  string
	host string
	port string
	path string
}

// NewSignRule is the rule for prefixes, each an https URL with a path
// and no query; a prefix that is not one is refused, so a brief that
// names a garbled prefix fails the lease rather than signing nothing.
func NewSignRule(prefixes []string, token func(context.Context) (string, error), key Signer) (*SignRule, error) {
	r := &SignRule{Token: token, Key: key}
	for _, raw := range prefixes {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("egress: registry prefix %q is not an https URL", raw)
		}
		port := u.Port()
		if port == "" {
			port = Port
		}
		path := u.EscapedPath()
		if path == "" {
			path = "/"
		}
		r.prefixes = append(r.prefixes, prefix{raw: raw, host: norm(u.Hostname()), port: port, path: path})
	}
	return r, nil
}

// Hosts are the hosts the rule's prefixes name: what the lease's CA
// must be able to name.
func (r *SignRule) Hosts() []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range r.prefixes {
		if !seen[p.host] {
			seen[p.host] = true
			out = append(out, p.host)
		}
	}
	return out
}

// Terminates says a prefix names host.
func (r *SignRule) Terminates(host string) bool {
	for _, p := range r.prefixes {
		if p.host == host {
			return true
		}
	}
	return false
}

// Match is the prefix req's URL is under, "" when none.
func (r *SignRule) Match(req *http.Request) string {
	if p := r.match(req.URL); p != nil {
		return p.raw
	}
	return ""
}

func (r *SignRule) match(u *url.URL) *prefix {
	if u == nil || u.Scheme != "https" {
		return nil
	}
	port := u.Port()
	if port == "" {
		port = Port
	}
	// the path as it goes upstream; a dot segment, raw or escaped, is
	// never signed, so /npm/../mcp cannot carry the token to /mcp
	sent := u.EscapedPath()
	if hasDotSegment(u.Path) || hasDotSegment(sent) {
		return nil
	}
	host := norm(u.Hostname())
	for i := range r.prefixes {
		p := &r.prefixes[i]
		if p.host == host && p.port == port && underPath(sent, p.path) {
			return p
		}
	}
	return nil
}

// underPath says path is prefix or under it: at a segment boundary, so
// /npm/ never takes /npmx and /npm never takes /npmx either.
func underPath(path, prefix string) bool {
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	if strings.HasSuffix(prefix, "/") || len(path) == len(prefix) {
		return true
	}
	return path[len(prefix)] == '/'
}

func hasDotSegment(path string) bool {
	for _, seg := range strings.Split(path, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

// Apply signs req when it is under a prefix: the session token of the
// moment in the DPoP scheme and a fresh proof, replacing what the
// session sent. A token or proof that cannot be had refuses the request
// 502 credential_unavailable: nothing the session sent goes on in its
// place.
func (r *SignRule) Apply(req *http.Request) (*Applied, error) {
	p := r.match(req.URL)
	if p == nil {
		return nil, nil
	}
	ap := &Applied{Rule: RuleRegistries, Ref: p.raw, Form: "dpop"}
	if r.Token == nil || r.Key == nil {
		return ap, &Refusal{Status: http.StatusBadGateway, Code: "credential_unavailable", Message: "the relay holds no session for " + p.raw}
	}
	tok, err := r.Token(req.Context())
	if err != nil || tok == "" {
		return ap, &Refusal{Status: http.StatusBadGateway, Code: "credential_unavailable", Message: "the lease's session token could not be had"}
	}
	proof, err := r.Key.Proof(req.Method, req.URL.String(), tok)
	if err != nil {
		return ap, &Refusal{Status: http.StatusBadGateway, Code: "credential_unavailable", Message: "the request could not be signed"}
	}
	req.Header.Set("Authorization", "DPoP "+tok)
	req.Header.Set("DPoP", proof)
	return ap, nil
}
