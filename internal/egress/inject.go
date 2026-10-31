package egress

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// Injection (mesh-specs gap 28): a credential Latchkey holds — a
// tenant's Secret (latchkey ADR 031) or a child minted of a scoped
// Connection (ADR 027 §9) — is written onto the session's requests to
// the hosts its inject rule names, by the relay, so the session never
// holds it. One InjectRule per ref: it terminates the rule's hosts that
// the lease's egress allows (never another), and on each request that
// matches the rule's paths and methods it writes the credential of the
// moment in the rule's form:
//
//   - header:   {header}: {prefix}{value}
//   - bearer:   Authorization: Bearer {value}
//   - basic:    Authorization: Basic base64(user:pass)
//   - query:    each params entry, a {{field}} template, as a parameter
//   - template: each headers entry, a {{field}} template, as a header
//
// A request that already carries a header or parameter the rule writes
// is refused 403 credential_preset — the sandbox may not choose its own
// value — unless what it carries is the Placeholder, which a tool that
// insists on a variable holds and the rule replaces, or the rule is one
// that Overwrites (a federation's cloud token, gap 32). A credential that
// cannot be had refuses the request 502 (credential_unavailable, or
// credential_refused with Latchkey's code): nothing the sandbox sent
// goes on in its place. A request outside the rule's paths or methods
// goes on untouched.

// RuleInject names an InjectRule in an Applied and the audit.
const RuleInject = "inject"

// Placeholder is what a session's tool holds where it insists on a
// variable for an injected credential: no secret, and the one preset
// value a rule replaces rather than refuses.
const Placeholder = "mesh"

// The inject rule's forms (latchkey ADR 031 §2.3).
const (
	FormHeader   = "header"
	FormBearer   = "bearer"
	FormBasic    = "basic"
	FormQuery    = "query"
	FormTemplate = "template"
	FormSigning  = "signing"
)

// The relay's own refusals of an injected host's request.
const (
	CodePreset      = "credential_preset"
	CodeUnavailable = "credential_unavailable"
	CodeRefused     = "credential_refused"
)

// InjectSpec is Latchkey's inject rule as the relay reads it: ADR 031
// §2.3's for a Secret, and ADR 027 §9's {hosts, header, prefix} for a
// connection's child (no form: a header).
type InjectSpec struct {
	Hosts   []string `json:"hosts"`
	Paths   []string `json:"paths,omitempty"`
	Methods []string `json:"methods,omitempty"`
	Form    string   `json:"form,omitempty"`
	// Header and Prefix are the header form's; Field names the field
	// header and bearer write, when the credential has more than one.
	Header string `json:"header,omitempty"`
	Prefix string `json:"prefix,omitempty"`
	Field  string `json:"field,omitempty"`
	// User and Pass are the basic form's templates; absent, the fields
	// user and pass (or username and password).
	User string `json:"user,omitempty"`
	Pass string `json:"pass,omitempty"`
	// Params are the query form's parameters and Headers the template
	// form's headers, each a {{field}} template.
	Params  map[string]string `json:"params,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// Env names the variables a tool insists on, which the session holds
	// as the Placeholder.
	Env []string `json:"env,omitempty"`
	// Overwrite has the rule replace whatever the request carries in the
	// headers it writes rather than refuse it: a federation's cloud token
	// on the cloud's hosts, whose own tools always send one (mesh-specs
	// gap 32). Never Latchkey's to set: the node sets it on the rules it
	// makes itself.
	Overwrite bool `json:"-"`
}

// FormOf is the spec's form: a connection's rule names none and is a
// header.
func (s InjectSpec) FormOf() string {
	if s.Form == "" && s.Header != "" {
		return FormHeader
	}
	return strings.ToLower(s.Form)
}

// Check says why the spec is not one the relay can apply, "" when it is.
func (s InjectSpec) Check() string {
	if len(s.Hosts) == 0 {
		return "the inject rule names no host"
	}
	for _, h := range s.Hosts {
		if !hostPattern.MatchString(norm(h)) {
			return fmt.Sprintf("the inject rule's host %q is not a host or *.host", h)
		}
	}
	switch s.FormOf() {
	case FormHeader:
		if !headerName.MatchString(s.Header) {
			return fmt.Sprintf("the header form's header %q is not a header name", s.Header)
		}
	case FormBearer, FormBasic:
	case FormQuery:
		if len(s.Params) == 0 {
			return "the query form names no parameter"
		}
	case FormTemplate:
		if len(s.Headers) == 0 {
			return "the template form names no header"
		}
		for h := range s.Headers {
			if !headerName.MatchString(h) {
				return fmt.Sprintf("the template form's header %q is not a header name", h)
			}
		}
	case FormSigning:
		return "the signing form is not one this node applies yet"
	default:
		return fmt.Sprintf("the inject form %q is not one this node knows", s.Form)
	}
	return ""
}

var (
	hostPattern = regexp.MustCompile(`^(\*\.)?[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)
	headerName  = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
	templateVar = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_.-]+)\s*\}\}`)
)

// MatchesHost says one of the spec's hosts names host: exactly, or a
// leading *. naming any one-or-more-label subdomain of the rest.
func (s InjectSpec) MatchesHost(host string) bool {
	host = norm(host)
	for _, h := range s.Hosts {
		h = norm(h)
		if rest, ok := strings.CutPrefix(h, "*."); ok {
			if strings.HasSuffix(host, "."+rest) {
				return true
			}
			continue
		}
		if h == host {
			return true
		}
	}
	return false
}

// matches says req is the rule's: its method among Methods and its path
// under one of Paths, when they are named. A path with a dot segment is
// never the rule's.
func (s InjectSpec) matches(req *http.Request) bool {
	if len(s.Methods) > 0 && !slices.ContainsFunc(s.Methods, func(m string) bool { return strings.EqualFold(m, req.Method) }) {
		return false
	}
	if len(s.Paths) == 0 {
		return true
	}
	sent := req.URL.EscapedPath()
	if hasDotSegment(req.URL.Path) || hasDotSegment(sent) {
		return false
	}
	for _, p := range s.Paths {
		if underPath(sent, p) || underPath(req.URL.Path, p) {
			return true
		}
	}
	return false
}

// Credential is one ref's credential of the moment, as Latchkey answered
// it: held in the relay's memory alone.
type Credential struct {
	Ref       string
	Spec      InjectSpec
	Fields    map[string]string
	Version   string
	ExpiresAt time.Time
}

// Value is the one value header and bearer write: Field's, the only
// field, or a token-named one.
func (c *Credential) Value() (string, error) {
	if c.Spec.Field != "" {
		v, ok := c.Fields[c.Spec.Field]
		if !ok {
			return "", fmt.Errorf("the credential has no field %q", c.Spec.Field)
		}
		return v, nil
	}
	if len(c.Fields) == 1 {
		for _, v := range c.Fields {
			return v, nil
		}
	}
	for _, k := range []string{"access_token", "token", "api_token", "api_key", "key", "value", "secret"} {
		if v, ok := c.Fields[k]; ok {
			return v, nil
		}
	}
	return "", fmt.Errorf("the credential has %d fields and the rule names none", len(c.Fields))
}

// render is t with each {{field}} the credential's.
func (c *Credential) render(t string) (string, error) {
	var missing string
	out := templateVar.ReplaceAllStringFunc(t, func(m string) string {
		name := templateVar.FindStringSubmatch(m)[1]
		v, ok := c.Fields[name]
		if !ok && missing == "" {
			missing = name
		}
		return v
	})
	if missing != "" {
		return "", fmt.Errorf("the rule's template names a field %q the credential has not", missing)
	}
	return out, nil
}

// basic is the basic form's user and password.
func (c *Credential) basic() (string, string, error) {
	field := func(tmpl string, names ...string) (string, error) {
		if tmpl != "" {
			return c.render(tmpl)
		}
		for _, n := range names {
			if v, ok := c.Fields[n]; ok {
				return v, nil
			}
		}
		return "", fmt.Errorf("the credential has no %s field", names[0])
	}
	u, err := field(c.Spec.User, "user", "username")
	if err != nil {
		return "", "", err
	}
	p, err := field(c.Spec.Pass, "pass", "password")
	return u, p, err
}

// Writes is what the credential's rule writes on a request: its headers
// (canonical) and its query parameters, each with its value. Never
// logged.
func (c *Credential) Writes() (headers, params map[string]string, err error) {
	headers, params = map[string]string{}, map[string]string{}
	switch c.Spec.FormOf() {
	case FormHeader, FormBearer:
		v, err := c.Value()
		if err != nil {
			return nil, nil, err
		}
		name, prefix := c.Spec.Header, c.Spec.Prefix
		if c.Spec.FormOf() == FormBearer {
			name, prefix = "Authorization", "Bearer "
		}
		headers[http.CanonicalHeaderKey(name)] = prefix + v
	case FormBasic:
		u, p, err := c.basic()
		if err != nil {
			return nil, nil, err
		}
		headers["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(u+":"+p))
	case FormQuery:
		for k, t := range c.Spec.Params {
			v, err := c.render(t)
			if err != nil {
				return nil, nil, err
			}
			params[k] = v
		}
	case FormTemplate:
		for k, t := range c.Spec.Headers {
			v, err := c.render(t)
			if err != nil {
				return nil, nil, err
			}
			headers[http.CanonicalHeaderKey(k)] = v
		}
	default:
		return nil, nil, fmt.Errorf("the inject form %q is not one this node applies", c.Spec.Form)
	}
	for k, v := range headers {
		if strings.ContainsAny(v, "\r\n\x00") {
			return nil, nil, fmt.Errorf("the value for %s is not a header value", k)
		}
	}
	return headers, params, nil
}

// InjectSource is a ref's credential as the relay holds it.
type InjectSource interface {
	// Spec is the rule Latchkey last answered: what a request is matched
	// on, whether or not its credential can be had now.
	Spec() InjectSpec
	// Credential is the credential of the moment, asked of Latchkey again
	// when it is due; an error refuses the request (a *Refusal says how).
	Credential(ctx context.Context) (*Credential, error)
}

// InjectRule writes one ref's credential onto its hosts' requests.
type InjectRule struct {
	// Ref is the Secret's or Connection's ref, for the audit.
	Ref string
	// Kind is secret, connection or federation, for the audit.
	Kind string
	// Purpose is what the credential is minted for, when its ref names
	// one (a federation target's), for the audit.
	Purpose string
	source  InjectSource
	hosts   []string
}

// NewInjectRule is ref's rule over hosts: the hosts of its spec the
// lease's egress allows, exactly. It terminates no other.
func NewInjectRule(ref, kind string, source InjectSource, hosts []string) *InjectRule {
	hs := make([]string, 0, len(hosts))
	for _, h := range hosts {
		hs = append(hs, norm(h))
	}
	sort.Strings(hs)
	return &InjectRule{Ref: ref, Kind: kind, source: source, hosts: slices.Compact(hs)}
}

// Hosts are the hosts the rule terminates: what the lease's CA must name.
func (r *InjectRule) Hosts() []string { return slices.Clone(r.hosts) }

// Terminates says the rule writes onto host's requests.
func (r *InjectRule) Terminates(host string) bool { return slices.Contains(r.hosts, norm(host)) }

// Apply writes the credential onto req when it is the rule's.
func (r *InjectRule) Apply(req *http.Request) (*Applied, error) {
	spec := r.source.Spec()
	if !spec.MatchesHost(req.URL.Hostname()) || !spec.matches(req) {
		return nil, nil
	}
	ap := &Applied{Rule: RuleInject, Ref: r.Ref, Form: spec.FormOf(), Kind: r.Kind, Purpose: r.Purpose}
	cred, err := r.source.Credential(req.Context())
	if err != nil {
		if _, ok := err.(*Refusal); ok {
			return ap, err
		}
		return ap, &Refusal{Status: http.StatusBadGateway, Code: CodeUnavailable, Message: "the credential for " + r.Ref + " could not be had"}
	}
	ap.Version, ap.Form = cred.Version, cred.Spec.FormOf()
	headers, params, err := cred.Writes()
	if err != nil {
		return ap, &Refusal{Status: http.StatusBadGateway, Code: CodeUnavailable, Message: r.Ref + ": " + err.Error()}
	}
	q := req.URL.Query()
	for k := range headers {
		if cred.Spec.Overwrite {
			break
		}
		if sent := req.Header.Values(k); len(sent) > 0 && !(len(sent) == 1 && IsPlaceholder(sent[0])) {
			return ap, &Refusal{Status: http.StatusForbidden, Code: CodePreset,
				Message: "the request already carries " + k + ", which the relay writes for " + r.Ref + ": send none, or " + Placeholder}
		}
	}
	for k := range params {
		if sent, ok := q[k]; ok && !(len(sent) == 1 && sent[0] == Placeholder) {
			return ap, &Refusal{Status: http.StatusForbidden, Code: CodePreset,
				Message: "the request already carries the parameter " + k + ", which the relay writes for " + r.Ref + ": send none, or " + Placeholder}
		}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if len(params) > 0 {
		for k, v := range params {
			q.Set(k, v)
		}
		req.URL.RawQuery = q.Encode()
	}
	return ap, nil
}

// IsPlaceholder says a header value carries the Placeholder and nothing
// else: "mesh", a scheme and "mesh" (Bearer mesh, token mesh), or Basic
// of mesh and mesh (or mesh and nothing).
func IsPlaceholder(v string) bool {
	v = strings.TrimSpace(v)
	if v == Placeholder {
		return true
	}
	scheme, rest, ok := strings.Cut(v, " ")
	if !ok {
		return false
	}
	rest = strings.TrimSpace(rest)
	if rest == Placeholder {
		return true
	}
	if strings.EqualFold(scheme, "basic") {
		dec, err := base64.StdEncoding.DecodeString(rest)
		if err != nil {
			return false
		}
		u, p, _ := strings.Cut(string(dec), ":")
		return u == Placeholder && (p == Placeholder || p == "")
	}
	return false
}
