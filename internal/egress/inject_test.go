package egress

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fixedSource is an InjectSource holding one credential, or refusing.
type fixedSource struct {
	cred   *Credential
	refuse error
}

func (s *fixedSource) Spec() InjectSpec { return s.cred.Spec }
func (s *fixedSource) Credential(context.Context) (*Credential, error) {
	if s.refuse != nil {
		return nil, s.refuse
	}
	return s.cred, nil
}

func injectReq(method, url string, headers ...string) *http.Request {
	req := httptest.NewRequest(method, url, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	return req
}

// Each form writes what latchkey ADR 031 §2.3 says it writes, the
// version and form audited and never the value.
func TestEachInjectFormWritesItsCredential(t *testing.T) {
	basic := base64.StdEncoding.EncodeToString([]byte("acme:hunter2-very-secret"))
	for _, tc := range []struct {
		name   string
		spec   InjectSpec
		fields map[string]string
		header string
		want   string
		query  string
	}{
		{"header", InjectSpec{Form: "header", Header: "X-Api-Key", Prefix: "Key "}, map[string]string{"api_key": "k-123456789"}, "X-Api-Key", "Key k-123456789", ""},
		{"a connection's rule, no form", InjectSpec{Header: "Authorization", Prefix: "Bearer "}, map[string]string{"access_token": "cf-child-123456"}, "Authorization", "Bearer cf-child-123456", ""},
		{"bearer", InjectSpec{Form: "bearer"}, map[string]string{"token": "tok-123456789"}, "Authorization", "Bearer tok-123456789", ""},
		{"bearer of a named field", InjectSpec{Form: "bearer", Field: "b"}, map[string]string{"a": "nope-123456", "b": "yes-1234567"}, "Authorization", "Bearer yes-1234567", ""},
		{"basic", InjectSpec{Form: "basic"}, map[string]string{"username": "acme", "password": "hunter2-very-secret"}, "Authorization", "Basic " + basic, ""},
		{"basic by templates", InjectSpec{Form: "basic", User: "{{u}}", Pass: "{{p}}"}, map[string]string{"u": "acme", "p": "hunter2-very-secret"}, "Authorization", "Basic " + basic, ""},
		{"template", InjectSpec{Form: "template", Headers: map[string]string{"X-Auth": "Token {{key}}:{{ secret }}"}}, map[string]string{"key": "kid", "secret": "s3cr3t-value-1"}, "X-Auth", "Token kid:s3cr3t-value-1", ""},
		{"query", InjectSpec{Form: "query", Params: map[string]string{"ApiUser": "{{username}}", "ApiKey": "{{api_key}}"}}, map[string]string{"username": "acme", "api_key": "nc-key-123456"}, "", "", "ApiKey=nc-key-123456&ApiUser=acme&Command=x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.spec.Hosts = []string{"api.example.com"}
			if why := tc.spec.Check(); why != "" {
				t.Fatal(why)
			}
			src := &fixedSource{cred: &Credential{Ref: "sec_1", Spec: tc.spec, Fields: tc.fields, Version: "3"}}
			r := NewInjectRule("sec_1", "secret", src, []string{"API.example.com"})
			if !r.Terminates("api.example.com") || r.Terminates("example.com") {
				t.Fatal("the rule terminates the wrong hosts")
			}
			req := injectReq("GET", "https://api.example.com/xml.response?Command=x")
			ap, err := r.Apply(req)
			if err != nil || ap == nil || ap.Rule != RuleInject || ap.Ref != "sec_1" || ap.Kind != "secret" || ap.Version != "3" || ap.Form != tc.spec.FormOf() {
				t.Fatalf("applied %+v, %v", ap, err)
			}
			if tc.header != "" && req.Header.Get(tc.header) != tc.want {
				t.Fatalf("%s is %q, want %q", tc.header, req.Header.Get(tc.header), tc.want)
			}
			if tc.query != "" && req.URL.RawQuery != tc.query {
				t.Fatalf("the query is %q, want %q", req.URL.RawQuery, tc.query)
			}
		})
	}
}

// A request that already carries what the rule writes is refused 403
// credential_preset; the placeholder alone is replaced.
func TestAPresetCredentialIsRefusedButThePlaceholderReplaced(t *testing.T) {
	bearer := &fixedSource{cred: &Credential{Ref: "conn_cf", Spec: InjectSpec{Hosts: []string{"api.cloudflare.com"}, Header: "Authorization", Prefix: "Bearer "},
		Fields: map[string]string{"access_token": "cf-child-123456"}}}
	r := NewInjectRule("conn_cf", "connection", bearer, []string{"api.cloudflare.com"})
	for _, preset := range []string{"Bearer my-own-token", "mesh-but-not-only"} {
		req := injectReq("GET", "https://api.cloudflare.com/client/v4/zones", "Authorization", preset)
		_, err := r.Apply(req)
		var ref *Refusal
		if !errors.As(err, &ref) || ref.Status != http.StatusForbidden || ref.Code != CodePreset {
			t.Fatalf("preset %q: %v", preset, err)
		}
		if req.Header.Get("Authorization") != preset {
			t.Fatalf("a refused request was written: %q", req.Header.Get("Authorization"))
		}
	}
	for _, ph := range []string{"Bearer mesh", "mesh", "token mesh", "Basic " + base64.StdEncoding.EncodeToString([]byte("mesh:mesh"))} {
		req := injectReq("GET", "https://api.cloudflare.com/client/v4/zones", "Authorization", ph)
		if _, err := r.Apply(req); err != nil || req.Header.Get("Authorization") != "Bearer cf-child-123456" {
			t.Fatalf("placeholder %q: %v, %q", ph, err, req.Header.Get("Authorization"))
		}
	}
	query := &fixedSource{cred: &Credential{Ref: "sec_nc", Spec: InjectSpec{Hosts: []string{"api.namecheap.com"}, Form: "query", Params: map[string]string{"ApiKey": "{{k}}"}},
		Fields: map[string]string{"k": "nc-key-123456"}}}
	q := NewInjectRule("sec_nc", "secret", query, []string{"api.namecheap.com"})
	_, err := q.Apply(injectReq("GET", "https://api.namecheap.com/xml.response?ApiKey=mine"))
	var ref *Refusal
	if !errors.As(err, &ref) || ref.Code != CodePreset {
		t.Fatalf("a preset parameter: %v", err)
	}
	req := injectReq("GET", "https://api.namecheap.com/xml.response?ApiKey=mesh")
	if _, err := q.Apply(req); err != nil || req.URL.Query().Get("ApiKey") != "nc-key-123456" {
		t.Fatalf("the placeholder parameter: %v %q", err, req.URL.RawQuery)
	}
}

// Paths and methods narrow the rule: a request outside them goes on as
// sent, and a dot segment never matches.
func TestARequestOutsideTheRuleGoesOnUntouched(t *testing.T) {
	src := &fixedSource{cred: &Credential{Ref: "sec_1", Spec: InjectSpec{Hosts: []string{"*.example.com"}, Paths: []string{"/v1/"}, Methods: []string{"POST"}, Form: "bearer"},
		Fields: map[string]string{"token": "tok-123456789"}}}
	r := NewInjectRule("sec_1", "secret", src, []string{"api.example.com"})
	for _, tc := range []struct{ method, url string }{
		{"GET", "https://api.example.com/v1/x"},
		{"POST", "https://api.example.com/v2/x"},
		{"POST", "https://api.example.com/v1/../v2/x"},
	} {
		req := injectReq(tc.method, tc.url)
		if ap, err := r.Apply(req); ap != nil || err != nil || req.Header.Get("Authorization") != "" {
			t.Fatalf("%s %s: %+v %v", tc.method, tc.url, ap, err)
		}
	}
	req := injectReq("POST", "https://api.example.com/v1/x")
	if ap, err := r.Apply(req); ap == nil || err != nil || req.Header.Get("Authorization") != "Bearer tok-123456789" {
		t.Fatalf("the rule's own request: %+v %v", ap, err)
	}
}

// A credential that cannot be had refuses the request with what the
// source said, and writes nothing.
func TestACredentialNotHadRefusesTheRequest(t *testing.T) {
	src := &fixedSource{cred: &Credential{Spec: InjectSpec{Hosts: []string{"api.example.com"}, Form: "bearer"}},
		refuse: &Refusal{Status: http.StatusBadGateway, Code: CodeRefused, Message: "Latchkey refused sec_1: purpose_not_granted (403)"}}
	r := NewInjectRule("sec_1", "secret", src, []string{"api.example.com"})
	req := injectReq("GET", "https://api.example.com/x")
	ap, err := r.Apply(req)
	var ref *Refusal
	if ap == nil || !errors.As(err, &ref) || ref.Code != CodeRefused || req.Header.Get("Authorization") != "" {
		t.Fatalf("%+v %v", ap, err)
	}
	// a field the template names that the credential lacks
	src = &fixedSource{cred: &Credential{Spec: InjectSpec{Hosts: []string{"api.example.com"}, Form: "template", Headers: map[string]string{"X-K": "{{nope}}"}}, Fields: map[string]string{"k": "v"}}}
	_, err = NewInjectRule("sec_2", "secret", src, []string{"api.example.com"}).Apply(injectReq("GET", "https://api.example.com/x"))
	if !errors.As(err, &ref) || ref.Code != CodeUnavailable {
		t.Fatalf("a missing field: %v", err)
	}
}

// Check refuses a rule the relay cannot apply.
func TestAnInjectSpecIsChecked(t *testing.T) {
	for _, s := range []InjectSpec{
		{Form: "bearer"},
		{Hosts: []string{"api.example.com/x"}, Form: "bearer"},
		{Hosts: []string{"api.example.com"}, Form: "header", Header: "Bad Header"},
		{Hosts: []string{"api.example.com"}, Form: "query"},
		{Hosts: []string{"api.example.com"}, Form: "signing"},
		{Hosts: []string{"api.example.com"}, Form: "carrier-pigeon"},
	} {
		if s.Check() == "" {
			t.Errorf("%+v passed", s)
		}
	}
	if !(InjectSpec{Hosts: []string{"*.example.com"}}).MatchesHost("a.b.example.com") || (InjectSpec{Hosts: []string{"*.example.com"}}).MatchesHost("example.com") {
		t.Error("a wildcard host")
	}
}

// A rule that Overwrites (a federation's cloud token, mesh-specs gap 32)
// replaces whatever Authorization the request carries rather than refuse
// it, and is audited with its purpose; one that does not still refuses.
func TestAnOverwritingRuleReplacesThePresetHeader(t *testing.T) {
	spec := InjectSpec{Hosts: []string{"*.googleapis.com"}, Form: FormBearer, Field: "access_token", Overwrite: true}
	src := &fixedSource{cred: &Credential{Ref: "fed_prod", Spec: spec, Fields: map[string]string{"access_token": "ya29.cloud-1234567"}}}
	r := NewInjectRule("fed_prod", "federation", src, []string{"storage.googleapis.com"})
	r.Purpose = "configure"
	for _, sent := range []string{"", "Bearer mesh", "Bearer the-sandboxs-own"} {
		req := injectReq("GET", "https://storage.googleapis.com/storage/v1/b", "Authorization", sent)
		ap, err := r.Apply(req)
		if err != nil || ap == nil || ap.Purpose != "configure" || ap.Kind != "federation" || ap.Form != FormBearer {
			t.Fatalf("sent %q: %+v %v", sent, ap, err)
		}
		if got := req.Header.Values("Authorization"); len(got) != 1 || got[0] != "Bearer ya29.cloud-1234567" {
			t.Fatalf("sent %q: Authorization %q", sent, got)
		}
	}
	if ap, _ := r.Apply(injectReq("GET", "https://example.com/x")); ap != nil {
		t.Fatal("applied off its hosts")
	}
	spec.Overwrite = false
	src.cred.Spec = spec
	_, err := r.Apply(injectReq("GET", "https://storage.googleapis.com/x", "Authorization", "Bearer the-sandboxs-own"))
	var rf *Refusal
	if !errors.As(err, &rf) || rf.Code != CodePreset {
		t.Fatalf("without Overwrite: %v", err)
	}
}
