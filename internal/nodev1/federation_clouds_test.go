package nodev1

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/egress"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// The other clouds' tokens of these tests: none may be seen anywhere but
// on the wire to the host it is for.
const (
	awsKeyID     = "ASIAFAKEROLEKEY0001"
	awsSecret    = "aws-secret-access-key-0001"
	awsSession   = "aws-session-token-0001"
	entraPrefix  = "entra-access-token-"
	oidcPrefix   = "oidc-traded-token-"
	fedAWSRole   = "arn:aws:iam::123456789012:role/runsheet-configure"
	fedAzureApp  = "0b7c4e1e-3c1a-4d1e-9b0a-2b2f6d0c9a11"
	fedAzureTent = "72f988bf-86f1-41af-91ab-2d7cd011db47"
)

// fedExchange has the fake Latchkey mint ref as a cloud target with ex.
func (l *fakeLatchkey) fedExchange(ref, cloud string, ex map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fed.exchange == nil {
		l.fed.exchange = map[string]map[string]any{}
	}
	if l.fed.cloud == nil {
		l.fed.cloud = map[string]string{}
	}
	l.fed.exchange[ref], l.fed.cloud[ref] = ex, cloud
}

// fakeClouds is AWS's STS, Entra's token endpoint and an RFC 8693
// endpoint: each trades a subject token the fake Latchkey issued, in its
// own shape, and keeps what it was sent.
type fakeClouds struct {
	srv    *httptest.Server
	subj   func(string) bool
	mu     sync.Mutex
	sent   []url.Values
	issued int
	// refuse, when set, is the status and body every endpoint answers
	refuse *googleRefusal
}

func newFakeClouds(t *testing.T, l *fakeLatchkey) *fakeClouds {
	c := &fakeClouds{}
	c.subj = func(tok string) bool {
		_, issued := l.fedMints()
		for _, i := range issued {
			if i == tok {
				return true
			}
		}
		return false
	}
	c.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		c.mu.Lock()
		defer c.mu.Unlock()
		c.sent = append(c.sent, r.PostForm)
		if c.refuse != nil {
			w.WriteHeader(c.refuse.status)
			io.WriteString(w, c.refuse.body)
			return
		}
		f := r.PostForm
		switch {
		case r.URL.Path == "/aws/":
			if f.Get("Action") != "AssumeRoleWithWebIdentity" || f.Get("RoleArn") != fedAWSRole || !c.subj(f.Get("WebIdentityToken")) {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `<ErrorResponse><Error><Type>Sender</Type><Code>InvalidIdentityToken</Code><Message>not the fake's</Message></Error></ErrorResponse>`)
				return
			}
			fmt.Fprintf(w, `<AssumeRoleWithWebIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleWithWebIdentityResult><Credentials><AccessKeyId>%s</AccessKeyId><SecretAccessKey>%s</SecretAccessKey><SessionToken>%s</SessionToken><Expiration>%s</Expiration></Credentials></AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`,
				awsKeyID, awsSecret, awsSession, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case r.URL.Path == "/"+fedAzureTent+"/oauth2/v2.0/token":
			if f.Get("grant_type") != "client_credentials" || f.Get("client_id") != fedAzureApp || f.Get("client_assertion_type") != clientAssertionJWT || !c.subj(f.Get("client_assertion")) {
				w.WriteHeader(http.StatusUnauthorized)
				io.WriteString(w, `{"error":"invalid_client","error_description":"AADSTS70021: No matching federated identity record found."}`)
				return
			}
			c.issued++
			json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("%s%04d", entraPrefix, c.issued), "expires_in": 3599, "token_type": "Bearer"})
		case r.URL.Path == "/oidc/token":
			if f.Get("grant_type") != tokenExchange || f.Get("audience") != "vault.acme.test" || !c.subj(f.Get("subject_token")) {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"error":"invalid_grant","error_description":"not the fake's"}`)
				return
			}
			c.issued++
			json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("%s%04d", oidcPrefix, c.issued), "expires_in": 600})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// cloudsRig points the runtime's trades at the fake clouds: the check
// lets its address through as each cloud's own.
func cloudsRig(t *testing.T, g *rig) *fakeClouds {
	fc := newFakeClouds(t, g.latchkey)
	g.rt.federationHTTP = fc.srv.Client()
	g.rt.federationCheck = func(raw, host string) (*url.URL, error) {
		u, err := url.Parse(raw)
		if err != nil || u.Host != fc.srv.Listener.Addr().String() {
			return cloudURL(raw, host)
		}
		return u, nil
	}
	return fc
}

// assertNoCloudToken fails when any cloud's secret is in what.
func assertNoCloudToken(t *testing.T, where, what string) {
	t.Helper()
	assertNoToken(t, where, what)
	for _, p := range []string{awsSecret, awsSession, entraPrefix, oidcPrefix} {
		if strings.Contains(what, p) {
			t.Fatalf("%s carries a token %q…", where, p)
		}
	}
}

// awsHost is a fake set of AWS hosts: it checks each request's signature
// as AWS does, with the role's secret, and says what it saw.
type awsHost struct {
	mu   sync.Mutex
	seen []string
}

func awsHostRig(t *testing.T, g *rig) *awsHost {
	t.Helper()
	h := &awsHost{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		scope, err := verifySigV4(r, body)
		verdict := "signed"
		if err != nil {
			verdict = "bad: " + err.Error()
		}
		if r.Header.Get("X-Amz-Security-Token") != awsSession {
			verdict += " no-session-token"
		}
		h.mu.Lock()
		h.seen = append(h.seen, fmt.Sprintf("%s %s %s %s body=%d", verdict, r.Method, r.Host, scope, len(body)))
		h.mu.Unlock()
		if !strings.HasPrefix(verdict, "signed") {
			w.WriteHeader(http.StatusForbidden)
		}
		io.WriteString(w, "ok\n")
	}))
	t.Cleanup(srv.Close)
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	g.rt.EgressDial = dial
	up := srv.Client().Transport.(*http.Transport).Clone()
	up.TLSClientConfig.ServerName = "example.com"
	up.DialContext = dial
	g.rt.EgressUpstream = up
	return h
}

// verifySigV4 checks a received request's signature as AWS does, with
// the role's secret: the scope it names (<region>/<service>), or why it
// does not hold.
func verifySigV4(r *http.Request, body []byte) (string, error) {
	auth := r.Header.Get("Authorization")
	_, cred, ok := strings.Cut(auth, "Credential=")
	if !ok {
		return "", fmt.Errorf("no AWS4-HMAC-SHA256 Authorization")
	}
	cred, _, _ = strings.Cut(cred, ",")
	p := strings.Split(cred, "/")
	if len(p) != 5 || p[0] != awsKeyID {
		return "", fmt.Errorf("signed by %q", cred)
	}
	at, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil || time.Since(at) > time.Minute {
		return "", fmt.Errorf("X-Amz-Date %q", r.Header.Get("X-Amz-Date"))
	}
	payload := r.Header.Get("X-Amz-Content-Sha256")
	if payload == "" {
		sum := sha256.Sum256(body)
		payload = hex.EncodeToString(sum[:])
	}
	again := r.Clone(r.Context())
	again.Header.Del("Authorization")
	if want := egress.SigV4Authorization(again, payload, awsKeyID, awsSecret, p[2], p[3], at); want != auth {
		return "", fmt.Errorf("the signature is not the role's")
	}
	return p[2] + "/" + p[3], nil
}

func (h *awsHost) lines() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.seen...)
}

// The relay assumes a job's AWS role with its federation token and signs
// the sandbox's AWS requests with the role's keys: a request its SDK
// signed on the placeholder is signed again for the region and service
// it named, an unsigned one for its host's, a body is hashed; a request
// signed with a key of the sandbox's own is refused credential_preset;
// the pod holds the placeholder keys alone, and no key reaches the log,
// the records or the environment (mesh-specs gap 32, AWS).
func TestTheRelaySignsAJobsAWSRequestsWithTheRolesKeys(t *testing.T) {
	needCurl(t)
	g, sb, _ := podRig(t)
	logs := &logSink{t: t}
	g.rt.Logf = logs.logf
	host := awsHostRig(t, g)
	fc := cloudsRig(t, g)
	g.latchkey.fedExchange("fed_aws", "aws", map[string]any{
		"sts_url": fc.srv.URL + "/aws/", "action": "AssumeRoleWithWebIdentity", "version": "2011-06-15", "role_arn": fedAWSRole,
		"role_session_name": "mesh-lease-aws", "region": "ap-southeast-2", "session_policy": `{"Version":"2012-10-17"}`, "duration": 900,
		"hosts": []string{"*.amazonaws.com"},
	})
	dir := t.TempDir()
	answers := filepath.Join(dir, "answers")
	sb.Agent = hostAgent(t, filepath.Join(dir, "claude"), curlScript(answers,
		`--aws-sigv4 "aws:amz:ap-southeast-2:s3" --user "$AWS_ACCESS_KEY_ID:$AWS_SECRET_ACCESS_KEY" https://my-bucket.s3.ap-southeast-2.amazonaws.com/state/terraform.tfstate`,
		`https://lambda.ap-southeast-2.amazonaws.com/2015-03-31/functions`,
		`-X POST -H "Content-Type: application/x-www-form-urlencoded" --data "Action=GetCallerIdentity&Version=2011-06-15" https://sts.ap-southeast-2.amazonaws.com/`,
		`--aws-sigv4 "aws:amz:ap-southeast-2:s3" --user "AKIAOWNKEY:own-secret" https://my-bucket.s3.ap-southeast-2.amazonaws.com/x`,
	))
	g.offer(t, func(b *meshapi.Brief) {
		fedBrief(b)
		b.Egress.Hosts = []string{"my-bucket.s3.ap-southeast-2.amazonaws.com", "lambda.ap-southeast-2.amazonaws.com", "sts.ap-southeast-2.amazonaws.com", "api.example.test"}
		b.Access.Federation = []meshapi.FederationRef{{Ref: "fed_aws"}}
	})
	run(t, g, context.Background())
	if n := len(g.fake.Calls("finish")); n != 1 {
		t.Fatalf("%d finishes; hand-backs %+v", n, g.fake.Calls("hand-back"))
	}
	got, _ := os.ReadFile(answers)
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	if len(lines) != 4 || !strings.Contains(lines[0], " 200") || !strings.Contains(lines[1], " 200") || !strings.Contains(lines[2], " 200") ||
		!strings.Contains(lines[3], " 403") || !strings.Contains(lines[3], "credential_preset") {
		t.Fatalf("the pod's answers %q", got)
	}
	seen := strings.Join(host.lines(), "\n")
	for _, want := range []string{
		"signed GET my-bucket.s3.ap-southeast-2.amazonaws.com ap-southeast-2/s3 body=0",
		"signed GET lambda.ap-southeast-2.amazonaws.com ap-southeast-2/lambda body=0",
		"signed POST sts.ap-southeast-2.amazonaws.com ap-southeast-2/sts body=43",
	} {
		if !strings.Contains(seen, want) {
			t.Fatalf("AWS saw\n%s\nnot %s", seen, want)
		}
	}
	if len(host.lines()) != 3 {
		t.Fatalf("AWS saw\n%s", seen)
	}
	fc.mu.Lock()
	sent := fc.sent
	fc.mu.Unlock()
	if len(sent) != 1 || sent[0].Get("RoleSessionName") != "mesh-lease-aws" || sent[0].Get("DurationSeconds") != "900" || sent[0].Get("Policy") != `{"Version":"2012-10-17"}` {
		t.Fatalf("STS was sent %v", sent)
	}
	env := sb.AgentSpecs()[0].Env
	for _, name := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		if envValue(env, name) != egress.Placeholder {
			t.Fatalf("the pod's %s is %q", name, envValue(env, name))
		}
	}
	assertNoCloudToken(t, "the pod's environment", strings.Join(env, "\n"))
	assertNoCloudToken(t, "the node's log", logs.String())
	var injected []string
	for _, c := range g.fake.Calls("access") {
		assertNoCloudToken(t, "an access record", string(c.Body))
		var req meshapi.AccessRequest
		json.Unmarshal(c.Body, &req)
		for _, r := range req.Records {
			if strings.HasPrefix(r.Service, "inject ") {
				injected = append(injected, fmt.Sprintf("%s %s %s %s", r.Service, r.Path, r.Outcome, r.Reason))
			}
		}
	}
	if len(injected) != 4 || injected[0] != "inject signing aws_sigv4 fed_aws ok 200 purpose configure" || !strings.Contains(injected[3], "credential_preset") {
		t.Fatalf("the injected requests recorded\n%s", strings.Join(injected, "\n"))
	}
}

// The relay trades an Azure target at Entra and an oidc target at its own
// endpoint, and writes a direct oidc target's subject token itself, each
// as a bearer on the hosts Latchkey names for it that the kind's egress
// names, replacing what the sandbox sent (mesh-specs gap 32, Azure and
// generic OIDC).
func TestTheRelayTradesAzureAndOIDCTargets(t *testing.T) {
	needCurl(t)
	g, sb, _ := podRig(t)
	logs := &logSink{t: t}
	g.rt.Logf = logs.logf
	host := injectHostRig(t, g)
	fc := cloudsRig(t, g)
	g.latchkey.fedExchange("fed_azure", "azure", map[string]any{
		"token_url": fc.srv.URL + "/" + fedAzureTent + "/oauth2/v2.0/token", "grant_type": "client_credentials", "client_id": fedAzureApp,
		"scope": "https://management.azure.com/.default", "client_assertion_type": clientAssertionJWT, "hosts": []string{"management.azure.com"},
	})
	g.latchkey.fedExchange("fed_vault", "oidc", map[string]any{
		"mode": "token_exchange", "token_url": fc.srv.URL + "/oidc/token", "grant_type": tokenExchange, "audience": "vault.acme.test",
		"subject_token_type": jwtTokenType, "requested_token_type": accessTokenType, "hosts": []string{"vault.acme.test"},
	})
	g.latchkey.fedExchange("fed_direct", "oidc", map[string]any{"mode": "direct", "audience": "api.direct.test", "hosts": []string{"*.direct.test"}})
	dir := t.TempDir()
	answers := filepath.Join(dir, "answers")
	sb.Agent = hostAgent(t, filepath.Join(dir, "claude"), curlScript(answers,
		`-H "Authorization: Bearer mine" https://management.azure.com/subscriptions?api-version=2022-12-01`,
		`https://vault.acme.test/v1/secret/data/x`,
		`https://api.direct.test/v1/things`,
	))
	g.offer(t, func(b *meshapi.Brief) {
		fedBrief(b)
		b.Egress.Hosts = []string{"management.azure.com", "vault.acme.test", "api.direct.test", "api.example.test"}
		b.Access.Federation = []meshapi.FederationRef{{Ref: "fed_azure"}, {Ref: "fed_vault"}, {Ref: "fed_direct", Purpose: "read"}}
	})
	run(t, g, context.Background())
	if n := len(g.fake.Calls("finish")); n != 1 {
		t.Fatalf("%d finishes; hand-backs %+v", n, g.fake.Calls("hand-back"))
	}
	got, _ := os.ReadFile(answers)
	if lines := strings.Split(strings.TrimSpace(string(got)), "\n"); len(lines) != 3 || strings.Count(string(got), " 200 OK") != 3 {
		t.Fatalf("the pod's answers %q", got)
	}
	_, issued := g.latchkey.fedMints()
	seen := strings.Join(host.lines(), "\n")
	for _, want := range []string{
		`GET management.azure.com /subscriptions?api-version=2022-12-01 auth="Bearer ` + entraPrefix,
		`GET vault.acme.test /v1/secret/data/x? auth="Bearer ` + oidcPrefix,
		`GET api.direct.test /v1/things? auth="Bearer ` + fedSubjectPrefix,
	} {
		if !strings.Contains(seen, want) {
			t.Fatalf("the hosts saw\n%s\nnot %s", seen, want)
		}
	}
	if strings.Contains(seen, "mine") || len(issued) != 3 {
		t.Fatalf("the hosts saw\n%s\n(issued %d)", seen, len(issued))
	}
	if env := sb.AgentSpecs()[0].Env; envValue(env, "GOOGLE_OAUTH_ACCESS_TOKEN") != "" || envValue(env, "AWS_ACCESS_KEY_ID") != "" {
		t.Fatalf("a cloud's placeholders for no target of it: %v", env)
	}
	assertNoCloudToken(t, "the node's log", logs.String())
	assertNoCloudToken(t, "the pod's environment", strings.Join(sb.AgentSpecs()[0].Env, "\n"))
	for _, c := range g.fake.Calls("access") {
		assertNoCloudToken(t, "an access record", string(c.Body))
	}
}

// A cloud's refusal of a target's trade hands the lease back as the
// product's configuration, naming the cloud's step and its own error;
// the cloud not serving is the infrastructure's; an exchange that names
// no hosts, or a URL that is not the cloud's, is refused before any
// token is sent.
func TestACloudRefusingATargetHandsTheLeaseBack(t *testing.T) {
	awsEx := func(fc *fakeClouds) map[string]any {
		return map[string]any{"sts_url": fc.srv.URL + "/aws/", "role_arn": fedAWSRole, "role_session_name": "mesh", "duration": 900}
	}
	for _, tc := range []struct {
		name  string
		cloud string
		ex    func(fc *fakeClouds) map[string]any
		set   func(fc *fakeClouds)
		hosts []string
		cause string
		says  []string
	}{
		{"aws_sts refused", "aws", awsEx, func(fc *fakeClouds) {
			fc.refuse = &googleRefusal{http.StatusForbidden, `<ErrorResponse><Error><Code>AccessDenied</Code><Message>Not authorized to perform sts:AssumeRoleWithWebIdentity</Message></Error></ErrorResponse>`}
		}, []string{"s3.us-east-1.amazonaws.com"}, meshapi.CauseProduct, []string{"credential_refused", "aws_sts", "AccessDenied", "AssumeRoleWithWebIdentity"}},
		{"aws_sts down", "aws", awsEx, func(fc *fakeClouds) {
			fc.refuse = &googleRefusal{http.StatusServiceUnavailable, `<ErrorResponse><Error><Code>ServiceUnavailable</Code><Message>x</Message></Error></ErrorResponse>`}
		}, []string{"s3.us-east-1.amazonaws.com"}, meshapi.CauseCredentialTimeout, []string{"credential_unavailable", "aws_sts"}},
		{"aws sts off AWS", "aws", func(fc *fakeClouds) map[string]any {
			return map[string]any{"sts_url": "https://sts.evil.test/", "role_arn": fedAWSRole}
		}, nil, []string{"s3.us-east-1.amazonaws.com"}, meshapi.CauseCredentialTimeout, []string{"aws_sts", "not AWS STS's"}},
		{"azure refused", "azure", func(fc *fakeClouds) map[string]any {
			return map[string]any{"token_url": fc.srv.URL + "/" + fedAzureTent + "/oauth2/v2.0/token", "client_id": "someone-else", "scope": "https://management.azure.com/.default", "hosts": []string{"management.azure.com"}}
		}, nil, []string{"management.azure.com"}, meshapi.CauseProduct, []string{"credential_refused", "azure_entra", "AADSTS70021"}},
		{"azure no hosts", "azure", func(fc *fakeClouds) map[string]any {
			return map[string]any{"token_url": fc.srv.URL + "/" + fedAzureTent + "/oauth2/v2.0/token", "client_id": fedAzureApp}
		}, nil, []string{"management.azure.com"}, meshapi.CauseProduct, []string{"credential_refused", "azure_entra", "names no hosts"}},
		{"oidc refused", "oidc", func(fc *fakeClouds) map[string]any {
			return map[string]any{"mode": "token_exchange", "token_url": fc.srv.URL + "/oidc/token", "audience": "elsewhere", "hosts": []string{"vault.acme.test"}}
		}, nil, []string{"vault.acme.test"}, meshapi.CauseProduct, []string{"credential_refused", "oidc_token", "invalid_grant"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, sb, _ := podRig(t)
			logs := &logSink{t: t}
			g.rt.Logf = logs.logf
			injectHostRig(t, g)
			fc := cloudsRig(t, g)
			g.latchkey.fedExchange("fed_x", tc.cloud, tc.ex(fc))
			if tc.set != nil {
				tc.set(fc)
			}
			ran := false
			sb.Agent = func(context.Context, sandbox.AgentSpec) (sandbox.Exit, error) { ran = true; return sandbox.Exit{}, nil }
			job := g.offer(t, func(b *meshapi.Brief) {
				fedBrief(b)
				b.Egress.Hosts = tc.hosts
				b.Access.Federation = []meshapi.FederationRef{{Ref: "fed_x"}}
			})
			run(t, g, context.Background())
			hb := onlyHandBack(t, g, job)
			if hb.Cause != tc.cause || !strings.Contains(hb.Reason, "fed_x") {
				t.Fatalf("handed back %+v", hb)
			}
			for _, s := range tc.says {
				if !strings.Contains(hb.Reason, s) {
					t.Fatalf("the reason %q does not say %q", hb.Reason, s)
				}
			}
			if ran {
				t.Fatal("the session ran without its cloud credential")
			}
			assertNoCloudToken(t, "the hand-back", hb.Reason)
			assertNoCloudToken(t, "the node's log", logs.String())
		})
	}
}

// cloudURL takes each cloud's endpoint on https and nothing else.
func TestCloudURLTakesEachCloudsEndpointsAlone(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
		ok        bool
	}{
		{"https://sts.amazonaws.com/", awsSTSHost, true},
		{"https://sts.ap-southeast-2.amazonaws.com/", awsSTSHost, true},
		{"https://sts.cn-north-1.amazonaws.com.cn/", awsSTSHost, true},
		{"https://sts.us-east-1.amazonaws.com:443/", awsSTSHost, true},
		{"http://sts.amazonaws.com/", awsSTSHost, false},
		{"https://sts.amazonaws.com.evil.test/", awsSTSHost, false},
		{"https://s3.amazonaws.com/", awsSTSHost, false},
		{"https://evil.sts.amazonaws.com/", awsSTSHost, false},
		{"https://u@sts.amazonaws.com/", awsSTSHost, false},
		{"https://login.microsoftonline.com/t/oauth2/v2.0/token", azureLoginHost, true},
		{"https://login.microsoftonline.com.evil.test/t", azureLoginHost, false},
		{"https://login.windows.net/t", azureLoginHost, false},
		{"https://vault.acme.test:8200/v1/token", anyHTTPSHost, true},
		{"http://vault.acme.test/v1/token", anyHTTPSHost, false},
		{"https://u:p@vault.acme.test/", anyHTTPSHost, false},
		{"https://sts.googleapis.com/v1/token", googleSTSHost, true},
		{"https://sts.amazonaws.com/", googleSTSHost, false},
	} {
		if _, err := cloudURL(tc.raw, tc.want); (err == nil) != tc.ok {
			t.Errorf("cloudURL(%q, %q) = %v, want ok %v", tc.raw, tc.want, err, tc.ok)
		}
	}
}

// A direct oidc target's subject token is minted again before its five
// minutes end; an aws target's keys are refreshed before the role's
// session ends.
func TestADirectTargetIsMintedAgainBeforeItsSubjectTokenEnds(t *testing.T) {
	now := time.Now()
	mints := 0
	h := &heldFederation{ref: "fed_direct", purpose: "read", now: func() time.Time { return now }, logf: t.Logf,
		mint: func(context.Context, string, string) (*federationToken, error) {
			mints++
			return &federationToken{SubjectToken: fmt.Sprintf("%s%04d", fedSubjectPrefix, mints), ExpiresIn: 300, Cloud: "oidc",
				Exchange: json.RawMessage(`{"mode":"direct","hosts":["api.direct.test"]}`)}, nil
		}}
	spec, err := h.open(context.Background())
	if err != nil || spec.FormOf() != egress.FormBearer || !spec.MatchesHost("api.direct.test") || !spec.Overwrite {
		t.Fatalf("spec %+v %v", spec, err)
	}
	c, err := h.Credential(context.Background())
	if v, _ := c.Value(); err != nil || mints != 1 || v != fedSubjectPrefix+"0001" {
		t.Fatalf("first: %v %d %q", err, mints, v)
	}
	now = now.Add(5*time.Minute - federationRefreshBefore + time.Second)
	c, err = h.Credential(context.Background())
	if v, _ := c.Value(); err != nil || mints != 2 || v != fedSubjectPrefix+"0002" {
		t.Fatalf("refreshed: %v %d %q", err, mints, v)
	}
}
