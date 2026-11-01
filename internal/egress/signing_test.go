package egress

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// AWS's Signature Version 4 test suite's credentials and clock.
var (
	awsSuiteFields = map[string]string{"access_key_id": "AKIDEXAMPLE", "secret_access_key": "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
	awsSuiteNow    = time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
)

func atAWSSuiteTime(t *testing.T) {
	t.Helper()
	old := signNow
	signNow = func() time.Time { return awsSuiteNow }
	t.Cleanup(func() { signNow = old })
}

func sigV4Rule(service string, fields map[string]string) *InjectRule {
	spec := InjectSpec{Hosts: []string{"example.amazonaws.com", "*.s3.amazonaws.com"}, Form: FormSigning, Scheme: SchemeSigV4, Region: "us-east-1", Service: service}
	return NewInjectRule("sec_aws", "secret", &fixedSource{cred: &Credential{Ref: "sec_aws", Spec: spec, Fields: fields, Version: "2"}}, spec.Hosts)
}

// The sandbox's SDK signs on the placeholder key; the relay signs again
// with the real one, as AWS's own test suite says it must read.
func TestSigV4ResignsAsAWSTestSuite(t *testing.T) {
	atAWSSuiteTime(t)
	dummy := "AWS4-HMAC-SHA256 Credential=mesh/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=00"
	for _, tc := range []struct {
		name, method, url, want string
	}{
		{"get-vanilla", "GET", "https://example.amazonaws.com/", "5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"},
		{"post-vanilla", "POST", "https://example.amazonaws.com/", "5da7c1a2acd57cee7505fc6676e4e544621c30862966e37dddb68e92efbe5d6b"},
		{"get-vanilla-query-order-key-case", "GET", "https://example.amazonaws.com/?Param2=value2&Param1=value1", "b97d918cfa904a5beff61c982a1b6f458b799221646efd99d3219ec94cdf2500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := injectReq(tc.method, tc.url, "Authorization", dummy, "X-Amz-Date", "20000101T000000Z")
			ap, err := sigV4Rule("service", awsSuiteFields).Apply(req)
			if err != nil {
				t.Fatal(err)
			}
			if ap.Form != "signing aws_sigv4" || ap.Version != "2" {
				t.Fatalf("audit: %+v", ap)
			}
			want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=" + tc.want
			if got := req.Header.Get("Authorization"); got != want {
				t.Fatalf("Authorization\n got %s\nwant %s", got, want)
			}
			if req.Header.Get("X-Amz-Date") != "20150830T123600Z" {
				t.Fatalf("X-Amz-Date = %q", req.Header.Get("X-Amz-Date"))
			}
		})
	}
}

// A session token is written and signed; the sandbox's own is refused
// unless it is the placeholder.
func TestSigV4SessionTokenAndPresets(t *testing.T) {
	atAWSSuiteTime(t)
	fields := map[string]string{"access_key_id": "AKIDEXAMPLE", "secret_access_key": "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "session_token": "tok-session-123456"}
	req := injectReq("GET", "https://example.amazonaws.com/", "X-Amz-Security-Token", "mesh")
	if _, err := sigV4Rule("service", fields).Apply(req); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("X-Amz-Security-Token") != "tok-session-123456" || !strings.Contains(req.Header.Get("Authorization"), "SignedHeaders=host;x-amz-date;x-amz-security-token,") {
		t.Fatalf("headers: %v", req.Header)
	}
	for name, req := range map[string]*http.Request{
		"another key's signature": injectReq("GET", "https://example.amazonaws.com/", "Authorization", "AWS4-HMAC-SHA256 Credential=AKIAREAL/20150830/us-east-1/service/aws4_request, SignedHeaders=host, Signature=00"),
		"a bearer":                injectReq("GET", "https://example.amazonaws.com/", "Authorization", "Bearer abcdefgh"),
		"its own session token":   injectReq("GET", "https://example.amazonaws.com/", "X-Amz-Security-Token", "theirs"),
		"a presigned query":       injectReq("GET", "https://example.amazonaws.com/?X-Amz-Signature=00&X-Amz-Credential=mesh"),
	} {
		_, err := sigV4Rule("service", fields).Apply(req)
		var ref *Refusal
		if !errors.As(err, &ref) || ref.Code != CodePreset || ref.Status != http.StatusForbidden {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// The payload hash: a body read and hashed, the sandbox's SHA-256 kept, a
// large body unsigned on S3 and refused elsewhere, a chunk-signed body
// refused.
func TestSigV4Payloads(t *testing.T) {
	atAWSSuiteTime(t)
	body := []byte(`{"TableName":"t"}`)
	req := httptest.NewRequest("POST", "https://example.amazonaws.com/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "DynamoDB_20120810.ListTables")
	if _, err := sigV4Rule("dynamodb", awsSuiteFields).Apply(req); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(req.Header.Get("Authorization"), "SignedHeaders=content-type;host;x-amz-date;x-amz-target,") {
		t.Fatalf("Authorization %s", req.Header.Get("Authorization"))
	}
	if got, _ := io.ReadAll(req.Body); !bytes.Equal(got, body) || req.ContentLength != int64(len(body)) {
		t.Fatalf("the body went on as %q (%d)", got, req.ContentLength)
	}

	sum := sha256.Sum256([]byte("x"))
	kept := hex.EncodeToString(sum[:])
	req = httptest.NewRequest("PUT", "https://bucket.s3.amazonaws.com/key", strings.NewReader("x"))
	req.Header.Set("X-Amz-Content-Sha256", kept)
	if _, err := sigV4Rule("s3", awsSuiteFields).Apply(req); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("X-Amz-Content-Sha256") != kept {
		t.Fatal("the sandbox's SHA-256 was not kept")
	}

	big := bytes.Repeat([]byte("a"), maxSignedBody+1)
	req = httptest.NewRequest("PUT", "https://bucket.s3.amazonaws.com/key", bytes.NewReader(big))
	req.ContentLength = -1
	if _, err := sigV4Rule("s3", awsSuiteFields).Apply(req); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("X-Amz-Content-Sha256") != unsignedPayload {
		t.Fatalf("a large S3 body: %q", req.Header.Get("X-Amz-Content-Sha256"))
	}
	if got, _ := io.ReadAll(req.Body); len(got) != len(big) {
		t.Fatalf("a large body went on as %d bytes", len(got))
	}

	for name, req := range map[string]*http.Request{
		"a large body off S3": httptest.NewRequest("POST", "https://example.amazonaws.com/", bytes.NewReader(big)),
		"chunk signatures":    injectReq("PUT", "https://example.amazonaws.com/k", "X-Amz-Content-Sha256", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"),
	} {
		_, err := sigV4Rule("service", awsSuiteFields).Apply(req)
		var ref *Refusal
		if !errors.As(err, &ref) || ref.Code != CodeNeedsBody || ref.Status != http.StatusRequestEntityTooLarge {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// hmac_sha256 signs the body, or the timestamp and the body.
func TestHMACSigns(t *testing.T) {
	old := signNow
	signNow = func() time.Time { return time.Unix(1700000000, 0) }
	t.Cleanup(func() { signNow = old })
	body := `{"event":"ping"}`
	mac := func(s string) string {
		m := hmac.New(sha256.New, []byte("whsec-very-secret"))
		m.Write([]byte(s))
		return hex.EncodeToString(m.Sum(nil))
	}
	for _, tc := range []struct {
		name   string
		spec   InjectSpec
		header string
		want   string
		ts     string
	}{
		{"body", InjectSpec{Header: "X-Hub-Signature-256", Prefix: "sha256=", Canonical: CanonicalBody}, "X-Hub-Signature-256", "sha256=" + mac(body), ""},
		{"ts_body", InjectSpec{Header: "X-Signature", Canonical: CanonicalTSBody, TSHeader: "X-Timestamp"}, "X-Signature", mac("1700000000." + body), "1700000000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.spec.Hosts, tc.spec.Form, tc.spec.Scheme = []string{"hooks.example.com"}, FormSigning, SchemeHMAC
			if why := tc.spec.Check(); why != "" {
				t.Fatal(why)
			}
			r := NewInjectRule("sec_hook", "secret", &fixedSource{cred: &Credential{Spec: tc.spec, Fields: map[string]string{"secret": "whsec-very-secret"}}}, tc.spec.Hosts)
			req := httptest.NewRequest("POST", "https://hooks.example.com/in", strings.NewReader(body))
			if _, err := r.Apply(req); err != nil {
				t.Fatal(err)
			}
			if req.Header.Get(tc.header) != tc.want || req.Header.Get("X-Timestamp") != tc.ts {
				t.Fatalf("headers %v", req.Header)
			}
			if got, _ := io.ReadAll(req.Body); string(got) != body {
				t.Fatalf("body %q", got)
			}
			req = httptest.NewRequest("POST", "https://hooks.example.com/in", strings.NewReader(body))
			req.Header.Set(tc.header, "sha256=chosen")
			_, err := r.Apply(req)
			var ref *Refusal
			if !errors.As(err, &ref) || ref.Code != CodePreset {
				t.Fatalf("a preset signature: %v", err)
			}
		})
	}
}

func TestSigningSpecCheck(t *testing.T) {
	ok := []InjectSpec{
		{Scheme: "aws_sigv4", Region: "ap-southeast-2", Service: "s3"},
		{Scheme: "hmac_sha256", Header: "X-Sig", Canonical: "body"},
		{Scheme: "hmac_sha256", Header: "X-Sig", Canonical: "ts_body", TSHeader: "X-Ts"},
	}
	bad := []InjectSpec{
		{Scheme: "aws_sigv4", Region: "nowhere", Service: "s3"},
		{Scheme: "aws_sigv4", Region: "us-east-1"},
		{Scheme: "hmac_sha256", Header: "X-Sig"},
		{Scheme: "hmac_sha256", Header: "X-Sig", Canonical: "ts_body"},
		{Scheme: "hmac_sha256", Header: "X-Sig", Canonical: "body", TSHeader: "X-Ts"},
		{Scheme: "rsa"},
	}
	for _, s := range ok {
		s.Hosts, s.Form = []string{"a.example.com"}, FormSigning
		if why := s.Check(); why != "" {
			t.Errorf("%+v: %s", s, why)
		}
	}
	for _, s := range bad {
		s.Hosts, s.Form = []string{"a.example.com"}, FormSigning
		if s.Check() == "" {
			t.Errorf("%+v passed", s)
		}
	}
	if !(InjectSpec{Form: FormSigning, Scheme: SchemeSigV4}).NotSecret("access_key_id") || (InjectSpec{Form: FormBearer}).NotSecret("access_key_id") {
		t.Fatal("NotSecret")
	}
}
