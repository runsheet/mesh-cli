package egress

import (
	"bytes"
	"cmp"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The signing form (latchkey ADR 031 §2.3, mesh-specs gap 28 item 3):
// the relay signs the request itself with the secret, so the key never
// enters the sandbox.
//
//   - aws_sigv4 {region, service}: the sandbox's AWS SDK or CLI runs on
//     the placeholder keys (AWS_ACCESS_KEY_ID=mesh); the relay drops its
//     signature and signs again with the secret's access_key_id,
//     secret_access_key and session_token? and a fresh X-Amz-Date. A
//     rule naming no region or service (a federation target's, whose
//     keys reach every service: mesh-specs gap 32) signs for the scope
//     the sandbox's own signature names, else the one its host's name
//     says (awsHostScope), else refuses 400 signing_scope_unknown. The
//     payload hash is the one the sandbox sent when it is a SHA-256 or
//     unsigned (AWS checks it against the body), else the body's, read
//     up to maxSignedBody; a larger body is UNSIGNED-PAYLOAD on S3 and
//     refused 413 signing_needs_body elsewhere, as is a payload signed
//     chunk by chunk (STREAMING-AWS4-HMAC-SHA256-*), whose chunk
//     signatures the dummy key made.
//   - hmac_sha256 {header, prefix?, field?, canonical, ts_header?}: the
//     lower-case hex HMAC-SHA256 of the body (canonical body), or of
//     "<unix seconds>.<body>" with the seconds also written to
//     ts_header (canonical ts_body), written to header after prefix. The
//     body is read whole, up to maxSignedBody, else 413
//     signing_needs_body.
//
// A request that carries its own signature is refused 403
// credential_preset, as any other form's preset header: for aws_sigv4 an
// Authorization or a session token whose key is not the placeholder, or
// a presigned query; for hmac_sha256 the header or ts_header set to
// anything but the placeholder.

// The signing form's schemes.
const (
	SchemeSigV4 = "aws_sigv4"
	SchemeHMAC  = "hmac_sha256"
)

// The hmac_sha256 scheme's canonical strings.
const (
	CanonicalBody   = "body"
	CanonicalTSBody = "ts_body"
)

// CodeNeedsBody refuses a request the relay cannot sign without holding
// more of its body than it will.
const CodeNeedsBody = "signing_needs_body"

// CodeScopeUnknown refuses an aws_sigv4 request whose region and service
// neither the rule, the sandbox's signature nor its host names.
const CodeScopeUnknown = "signing_scope_unknown"

// maxSignedBody is the most of a body the relay holds to sign it.
const maxSignedBody = 10 << 20

// signNow is the signing clock; tests set it.
var signNow = time.Now

var (
	awsRegion  = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)
	awsService = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
	sha256Hex  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// checkSigning says why a signing spec is not one the relay applies.
func (s InjectSpec) checkSigning() string {
	switch strings.ToLower(s.Scheme) {
	case SchemeSigV4:
		// either absent is read off each request (a federation target's
		// rule, which the node makes; Latchkey's Secret rules name both)
		if s.Region != "" && !awsRegion.MatchString(s.Region) {
			return fmt.Sprintf("the aws_sigv4 scheme's region %q is not an AWS region", s.Region)
		}
		if s.Service != "" && !awsService.MatchString(s.Service) {
			return fmt.Sprintf("the aws_sigv4 scheme's service %q is not an AWS service name", s.Service)
		}
	case SchemeHMAC:
		if !headerName.MatchString(s.Header) {
			return fmt.Sprintf("the hmac_sha256 scheme's header %q is not a header name", s.Header)
		}
		switch s.Canonical {
		case CanonicalBody:
			if s.TSHeader != "" {
				return "the hmac_sha256 scheme names a ts_header but signs the body alone"
			}
		case CanonicalTSBody:
			if !headerName.MatchString(s.TSHeader) {
				return fmt.Sprintf("the hmac_sha256 scheme's ts_header %q is not a header name", s.TSHeader)
			}
		default:
			return fmt.Sprintf("the hmac_sha256 scheme's canonical %q is not body or ts_body", s.Canonical)
		}
	default:
		return fmt.Sprintf("the signing scheme %q is not one this node applies", s.Scheme)
	}
	return ""
}

// signedHeaders are the headers a signing spec writes, canonical.
func (s InjectSpec) signedHeaders() []string {
	switch strings.ToLower(s.Scheme) {
	case SchemeSigV4:
		return []string{"Authorization", "X-Amz-Security-Token"}
	case SchemeHMAC:
		out := []string{http.CanonicalHeaderKey(s.Header)}
		if s.TSHeader != "" {
			out = append(out, http.CanonicalHeaderKey(s.TSHeader))
		}
		return out
	}
	return nil
}

// NotSecret says field is one a signing spec writes in the clear — the
// AWS access key id names the key in every request's Authorization — so
// it is neither redacted nor a canary.
func (s InjectSpec) NotSecret(field string) bool {
	return s.FormOf() == FormSigning && strings.EqualFold(s.Scheme, SchemeSigV4) && field == "access_key_id"
}

// sign signs req with c, a signing credential.
func (c *Credential) sign(req *http.Request, ref string) error {
	switch strings.ToLower(c.Spec.Scheme) {
	case SchemeSigV4:
		return c.signSigV4(req, ref)
	case SchemeHMAC:
		return c.signHMAC(req, ref)
	}
	return &Refusal{Status: http.StatusBadGateway, Code: CodeUnavailable, Message: ref + ": the signing scheme " + c.Spec.Scheme + " is not one this node applies"}
}

// field is the credential's field name, or a refusal naming it.
func (c *Credential) field(ref, name string) (string, error) {
	v, ok := c.Fields[name]
	if !ok || v == "" {
		return "", &Refusal{Status: http.StatusBadGateway, Code: CodeUnavailable, Message: ref + ": the credential has no field " + name}
	}
	return v, nil
}

// readBody holds req's body, up to maxSignedBody: false when it is
// larger, the body then left unread.
func readBody(req *http.Request) ([]byte, bool, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, true, nil
	}
	if req.ContentLength > maxSignedBody {
		return nil, false, nil
	}
	b, err := io.ReadAll(io.LimitReader(req.Body, maxSignedBody+1))
	if err != nil {
		return nil, false, err
	}
	if len(b) > maxSignedBody {
		// what was read goes on before the rest
		req.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(b), req.Body), req.Body}
		return nil, false, nil
	}
	req.Body.Close()
	req.Body, req.ContentLength = io.NopCloser(bytes.NewReader(b)), int64(len(b))
	if len(b) == 0 {
		req.Body = http.NoBody
	}
	return b, true, nil
}

func needsBody(ref string) error {
	return &Refusal{Status: http.StatusRequestEntityTooLarge, Code: CodeNeedsBody,
		Message: fmt.Sprintf("%s: the relay signs a body it holds, at most %d MiB", ref, maxSignedBody>>20)}
}

func signPreset(ref, what string) error {
	return &Refusal{Status: http.StatusForbidden, Code: CodePreset,
		Message: "the request carries " + what + ", which the relay writes for " + ref + ": sign with the key " + Placeholder}
}

// signHMAC writes the hmac_sha256 signature of req's body.
func (c *Credential) signHMAC(req *http.Request, ref string) error {
	name := c.Spec.Field
	if name == "" {
		if len(c.Fields) == 1 {
			for k := range c.Fields {
				name = k
			}
		} else {
			for _, k := range []string{"secret", "key", "value"} {
				if _, ok := c.Fields[k]; ok {
					name = k
					break
				}
			}
		}
	}
	key, err := c.field(ref, name)
	if err != nil {
		return err
	}
	for _, h := range c.Spec.signedHeaders() {
		if sent := req.Header.Values(h); len(sent) > 0 && !(len(sent) == 1 && IsPlaceholder(sent[0])) {
			return signPreset(ref, h)
		}
	}
	body, ok, err := readBody(req)
	if err != nil {
		return &Refusal{Status: http.StatusBadRequest, Code: "bad_body", Message: "reading the request body: " + err.Error()}
	}
	if !ok {
		return needsBody(ref)
	}
	mac := hmac.New(sha256.New, []byte(key))
	if c.Spec.Canonical == CanonicalTSBody {
		ts := strconv.FormatInt(signNow().Unix(), 10)
		mac.Write([]byte(ts + "."))
		req.Header.Set(c.Spec.TSHeader, ts)
	}
	mac.Write(body)
	v := c.Spec.Prefix + hex.EncodeToString(mac.Sum(nil))
	if strings.ContainsAny(v, "\r\n\x00") {
		return &Refusal{Status: http.StatusBadGateway, Code: CodeUnavailable, Message: ref + ": the signature is not a header value"}
	}
	req.Header.Set(c.Spec.Header, v)
	return nil
}

// The payload hashes AWS reads that the relay may keep as the sandbox
// sent them: AWS checks a SHA-256 against the body, and the others sign
// no payload.
const (
	unsignedPayload        = "UNSIGNED-PAYLOAD"
	unsignedPayloadTrailer = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
)

// signSigV4 drops the sandbox's AWS signature and signs req again.
func (c *Credential) signSigV4(req *http.Request, ref string) error {
	akid, err := c.field(ref, "access_key_id")
	if err != nil {
		return err
	}
	secret, err := c.field(ref, "secret_access_key")
	if err != nil {
		return err
	}
	token := c.Fields["session_token"]
	q := req.URL.Query()
	for _, k := range []string{"X-Amz-Signature", "X-Amz-Credential", "X-Amz-Security-Token"} {
		if q.Has(k) {
			return signPreset(ref, "a presigned query ("+k+")")
		}
	}
	if auth := req.Header.Get("Authorization"); auth != "" && !IsPlaceholder(auth) && sigV4Key(auth) != Placeholder {
		return signPreset(ref, "an Authorization not signed with the key "+Placeholder)
	}
	if t := req.Header.Values("X-Amz-Security-Token"); len(t) > 0 && !(len(t) == 1 && IsPlaceholder(t[0])) {
		return signPreset(ref, "X-Amz-Security-Token")
	}
	region, service := c.Spec.Region, strings.ToLower(c.Spec.Service)
	if region == "" || service == "" {
		r, sv, ok := sigV4Scope(req.Header.Get("Authorization"))
		if !ok {
			r, sv, ok = awsHostScope(req.URL.Hostname())
		}
		if !ok {
			return &Refusal{Status: http.StatusBadRequest, Code: CodeScopeUnknown,
				Message: ref + ": the request names no AWS region and service to sign for: sign it on the key " + Placeholder + ", or send it to the service's regional host"}
		}
		region, service = cmp.Or(region, r), cmp.Or(service, sv)
	}
	s3 := service == "s3"
	payload := strings.TrimSpace(req.Header.Get("X-Amz-Content-Sha256"))
	switch {
	case sha256Hex.MatchString(payload), payload == unsignedPayload, payload == unsignedPayloadTrailer:
	case strings.HasPrefix(payload, "STREAMING-"):
		// chunk signatures chained from the dummy key's seed
		return needsBody(ref)
	default:
		body, ok, err := readBody(req)
		if err != nil {
			return &Refusal{Status: http.StatusBadRequest, Code: "bad_body", Message: "reading the request body: " + err.Error()}
		}
		switch {
		case ok:
			sum := sha256.Sum256(body)
			payload = hex.EncodeToString(sum[:])
		case s3:
			payload = unsignedPayload
		default:
			return needsBody(ref)
		}
		if s3 || req.Header.Get("X-Amz-Content-Sha256") != "" {
			req.Header.Set("X-Amz-Content-Sha256", payload)
		}
	}
	now := signNow().UTC()
	amzDate := now.Format("20060102T150405Z")
	req.Header.Del("Authorization")
	req.Header.Set("X-Amz-Date", amzDate)
	if token != "" {
		req.Header.Set("X-Amz-Security-Token", token)
	} else {
		req.Header.Del("X-Amz-Security-Token")
	}
	req.Header.Set("Authorization", SigV4Authorization(req, payload, akid, secret, region, service, now))
	return nil
}

// sigV4Key is the access key id an AWS4-HMAC-SHA256 Authorization was
// signed with, "" when it is none.
func sigV4Key(auth string) string {
	rest, ok := strings.CutPrefix(strings.TrimSpace(auth), "AWS4-HMAC-SHA256 ")
	if !ok {
		return ""
	}
	for _, part := range strings.Split(rest, ",") {
		if cred, ok := strings.CutPrefix(strings.TrimSpace(part), "Credential="); ok {
			k, _, _ := strings.Cut(cred, "/")
			return k
		}
	}
	return ""
}

// sigV4Scope is the region and service an AWS4-HMAC-SHA256 Authorization
// was signed for: its Credential's <key>/<date>/<region>/<service>/aws4_request.
func sigV4Scope(auth string) (region, service string, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(auth), "AWS4-HMAC-SHA256 ")
	if !found {
		return "", "", false
	}
	for _, part := range strings.Split(rest, ",") {
		if cred, found := strings.CutPrefix(strings.TrimSpace(part), "Credential="); found {
			p := strings.Split(cred, "/")
			if len(p) == 5 && p[4] == "aws4_request" && awsRegion.MatchString(p[2]) && awsService.MatchString(p[3]) {
				return p[2], p[3], true
			}
		}
	}
	return "", "", false
}

// awsHostScope is the region and service an AWS host's name says:
// <service>.<region>.amazonaws.com, a bucket's virtual host
// (<bucket>.s3.<region>.amazonaws.com, <bucket>.s3.amazonaws.com,
// <bucket>.s3-<region>.amazonaws.com), a dual-stack or FIPS endpoint, or
// a global one (sts.amazonaws.com, iam.amazonaws.com: us-east-1;
// cn-north-1 in the .cn partition). false for a host not AWS's.
func awsHostScope(host string) (region, service string, ok bool) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	global := "us-east-1"
	rest, found := strings.CutSuffix(host, ".amazonaws.com")
	if !found {
		if rest, found = strings.CutSuffix(host, ".amazonaws.com.cn"); !found {
			return "", "", false
		}
		global = "cn-north-1"
	}
	labels := strings.Split(rest, ".")
	i := len(labels) - 1
	if labels[i] == "" {
		return "", "", false
	}
	region = global
	if awsRegion.MatchString(labels[i]) {
		region = labels[i]
		i--
	}
	for i >= 0 && labels[i] == "dualstack" {
		i--
	}
	if i < 0 {
		return "", "", false
	}
	service = strings.TrimSuffix(labels[i], "-fips")
	if r, found := strings.CutPrefix(service, "s3-"); found && awsRegion.MatchString(r) {
		return r, "s3", true
	}
	if !awsService.MatchString(service) {
		return "", "", false
	}
	return region, service, true
}

// SigV4Authorization is req's AWS Signature Version 4 Authorization at
// now, its payload hash payload: over the host, the content type and MD5
// and every X-Amz-* header req carries.
func SigV4Authorization(req *http.Request, payload, akid, secret, region, service string, now time.Time) string {
	headers := map[string]string{"host": req.Host}
	if headers["host"] == "" {
		headers["host"] = req.URL.Host
	}
	for k, vs := range req.Header {
		lk := strings.ToLower(k)
		if lk == "authorization" || !(strings.HasPrefix(lk, "x-amz-") || lk == "content-type" || lk == "content-md5") {
			continue
		}
		vals := make([]string, len(vs))
		for i, v := range vs {
			vals[i] = strings.Join(strings.Fields(v), " ")
		}
		headers[lk] = strings.Join(vals, ",")
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")
	canonical := strings.Join([]string{
		req.Method,
		sigV4URI(req.URL, service == "s3"),
		sigV4Query(req.URL.RawQuery),
		canonHeaders.String(),
		signed,
		payload,
	}, "\n")
	date := now.UTC().Format("20060102")
	scope := date + "/" + region + "/" + service + "/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + now.UTC().Format("20060102T150405Z") + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))
	return "AWS4-HMAC-SHA256 Credential=" + akid + "/" + scope + ", SignedHeaders=" + signed + ", Signature=" + sig
}

func hmacSHA256(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

// sigV4URI is the canonical URI: the path as sent for S3, every segment
// escaped again for the other services.
func sigV4URI(u *url.URL, s3 bool) string {
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	if s3 {
		return p
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = awsEscape(s)
	}
	return strings.Join(segs, "/")
}

// sigV4Query is the canonical query: each name and value escaped, sorted.
func sigV4Query(raw string) string {
	if raw == "" {
		return ""
	}
	var pairs []string
	for _, kv := range strings.Split(raw, "&") {
		if kv == "" {
			continue
		}
		k, v, _ := strings.Cut(kv, "=")
		dk, err := url.QueryUnescape(k)
		if err != nil {
			dk = k
		}
		dv, err := url.QueryUnescape(v)
		if err != nil {
			dv = v
		}
		pairs = append(pairs, awsEscape(dk)+"="+awsEscape(dv))
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "&")
}

// awsEscape is RFC 3986 escaping as AWS reads it: every byte but the
// unreserved characters as %XX.
func awsEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}
