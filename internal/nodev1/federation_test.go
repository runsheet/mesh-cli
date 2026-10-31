package nodev1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/egress"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// The tokens of these tests: none may be seen anywhere but on the wire
// to the host it is for.
const (
	fedSubjectPrefix = "fed-subject-token-"
	fedSTSPrefix     = "sts-federated-token-"
	fedCloudPrefix   = "ya29.cloud-access-token-"
	fedSA            = "deployer@runsheet-prod.iam.gserviceaccount.com"
	fedProvider      = "projects/123/locations/global/workloadIdentityPools/latchkey/providers/latchkey"
)

// fakeFederation is the fake Latchkey's federation-token route: the
// exchange it answers by target, the refusals it answers in their place,
// and each mint's body.
type fakeFederation struct {
	exchange map[string]map[string]any // target → exchange
	cloud    map[string]string         // target → cloud, gcp when unset
	refuse   map[string]carriedRefusal // target → refusal; code "" is a bare 404
	bodies   []string
	issued   []string
}

// fedTarget has the fake mint ref's subject token with an exchange at g.
func (l *fakeLatchkey) fedTarget(ref string, g *fakeGoogle, serviceAccount string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fed.exchange == nil {
		l.fed.exchange = map[string]map[string]any{}
	}
	l.fed.exchange[ref] = map[string]any{
		"sts_url": g.srv.URL + "/v1/token", "audience": "//iam.googleapis.com/" + fedProvider, "workload_identity_provider": fedProvider,
		"service_account": serviceAccount, "scopes": []string{"https://www.googleapis.com/auth/cloud-platform"},
		"iam_credentials_url": g.srv.URL + "/v1", "subject_token_type": jwtTokenType, "duration": 3600,
	}
}

func (l *fakeLatchkey) fedRefuse(ref string, rf carriedRefusal) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fed.refuse == nil {
		l.fed.refuse = map[string]carriedRefusal{}
	}
	l.fed.refuse[ref] = rf
}

func (l *fakeLatchkey) fedMints() ([]string, []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.fed.bodies...), append([]string(nil), l.fed.issued...)
}

// federationToken is latchkey #693's POST /v1/federation/token for a
// job's session: the DPoP-bound session token and a proof, a body of
// {target, purpose} and nothing else.
func (l *fakeLatchkey) federationToken(w http.ResponseWriter, r *http.Request) {
	if _, err := l.checkToken(r, l.srv.URL+r.URL.Path); err != nil {
		l.fail(w, http.StatusUnauthorized, "federation: "+err.Error())
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var body struct {
		Target  string `json:"target"`
		Purpose string `json:"purpose"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		l.fail(w, http.StatusBadRequest, "federation: "+err.Error())
		return
	}
	l.mu.Lock()
	l.fed.bodies = append(l.fed.bodies, string(raw))
	rf, refused := l.fed.refuse[body.Target]
	ex, known := l.fed.exchange[body.Target]
	cloud := l.fed.cloud[body.Target]
	tok := fmt.Sprintf("%s%04d", fedSubjectPrefix, len(l.fed.issued)+1)
	if !refused && known {
		l.fed.issued = append(l.fed.issued, tok)
	}
	l.mu.Unlock()
	switch {
	case refused && rf.code == "":
		// an org without the federation feature: a bare 404
		http.NotFound(w, r)
	case refused:
		w.WriteHeader(rf.status)
		json.NewEncoder(w).Encode(map[string]string{"error": rf.code, "error_description": "refused by the fake"})
	case !known:
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"error": "not_the_jobs", "error_description": "the job's access names no such federation target"})
	default:
		if cloud == "" {
			cloud = "gcp"
		}
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(map[string]any{"subject_token": tok, "issued_token_type": jwtTokenType, "expires_in": 300,
			"target_ref": body.Target, "cloud": cloud, "exchange": ex})
	}
}

// fakeGoogle is Google's STS and IAM Credentials: STS trades a subject
// token the fake Latchkey issued for a federated token, and
// generateAccessToken that for a service account's access token.
type fakeGoogle struct {
	srv   *httptest.Server
	subj  func(string) bool
	mu    sync.Mutex
	sts   []url.Values
	iam   []string // each call: sa lifetime scope
	seen  int
	cloud []string
	// life is the access token's life; refuseSTS and refuseIAM, when set,
	// are the status and body Google answers instead
	life                 time.Duration
	refuseSTS, refuseIAM *googleRefusal
}

type googleRefusal struct {
	status int
	body   string
}

func newFakeGoogle(t *testing.T, l *fakeLatchkey) *fakeGoogle {
	g := &fakeGoogle{life: time.Hour}
	g.subj = func(tok string) bool {
		_, issued := l.fedMints()
		for _, i := range issued {
			if i == tok {
				return true
			}
		}
		return false
	}
	var stsTokens []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		g.mu.Lock()
		defer g.mu.Unlock()
		g.seen++
		g.sts = append(g.sts, r.PostForm)
		if g.refuseSTS != nil {
			w.WriteHeader(g.refuseSTS.status)
			io.WriteString(w, g.refuseSTS.body)
			return
		}
		f := r.PostForm
		if f.Get("grant_type") != tokenExchange || f.Get("audience") != "//iam.googleapis.com/"+fedProvider ||
			f.Get("subject_token_type") != jwtTokenType || f.Get("requested_token_type") != accessTokenType || !g.subj(f.Get("subject_token")) {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"invalid_request","error_description":"not the exchange the fake takes"}`)
			return
		}
		tok := fmt.Sprintf("%s%04d", fedSTSPrefix, len(stsTokens)+1)
		stsTokens = append(stsTokens, tok)
		json.NewEncoder(w).Encode(map[string]any{"access_token": tok, "issued_token_type": accessTokenType, "token_type": "Bearer", "expires_in": 3600})
	})
	mux.HandleFunc("POST /v1/projects/-/serviceAccounts/{sa}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Scope    []string `json:"scope"`
			Lifetime string   `json:"lifetime"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		g.mu.Lock()
		defer g.mu.Unlock()
		g.seen++
		sa, _ := strings.CutSuffix(r.PathValue("sa"), ":generateAccessToken")
		g.iam = append(g.iam, fmt.Sprintf("%s %s %s", sa, body.Lifetime, strings.Join(body.Scope, ",")))
		if g.refuseIAM != nil {
			w.WriteHeader(g.refuseIAM.status)
			io.WriteString(w, g.refuseIAM.body)
			return
		}
		auth, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		known := false
		for _, s := range stsTokens {
			known = known || s == auth
		}
		if !known {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"code":401,"message":"not a federated token","status":"UNAUTHENTICATED"}}`)
			return
		}
		tok := fmt.Sprintf("%s%04d", fedCloudPrefix, len(g.cloud)+1)
		g.cloud = append(g.cloud, tok)
		json.NewEncoder(w).Encode(map[string]any{"accessToken": tok, "expireTime": time.Now().Add(g.life).UTC().Format(time.RFC3339)})
	})
	g.srv = httptest.NewTLSServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGoogle) calls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.seen
}

// fedRig points the runtime's trades at the fake Google: the check lets
// its address through as Google's own, which no build of the node can.
func fedRig(t *testing.T, g *rig) *fakeGoogle {
	fg := newFakeGoogle(t, g.latchkey)
	g.rt.federationHTTP = fg.srv.Client()
	g.rt.federationCheck = func(raw, host string) (*url.URL, error) {
		u, err := url.Parse(raw)
		if err != nil || u.Host != fg.srv.Listener.Addr().String() {
			return googleURL(raw, host)
		}
		return u, nil
	}
	return fg
}

// fedBrief is a configure kind whose offer carried runsheet-prod's
// target, its egress two Google APIs and a host of another vendor.
func fedBrief(b *meshapi.Brief) {
	b.FinishTool, b.FinishSchema = "", nil
	b.Kind = "runsheet/configure"
	b.Egress = &meshapi.Egress{Hosts: []string{"storage.googleapis.com", "compute.googleapis.com", "api.example.test"}, MCP: []string{"conform"}, Model: true}
	b.Access = &meshapi.Access{Repo: true, MCP: []string{"conform"}, Model: true, Federation: []meshapi.FederationRef{{Ref: "fed_prod"}}}
}

// assertNoToken fails when a subject, federated or cloud token is in what.
func assertNoToken(t *testing.T, where, what string) {
	t.Helper()
	for _, p := range []string{fedSubjectPrefix, fedSTSPrefix, fedCloudPrefix} {
		if strings.Contains(what, p) {
			t.Fatalf("%s carries a token %q…", where, p)
		}
	}
}

// The relay mints the job's federation token on its session token,
// trades it at Google's STS and IAM Credentials itself, and writes the
// cloud token as a bearer on the kind's Google hosts, replacing the
// placeholder and any Authorization the sandbox sent; the pod holds
// placeholders alone, and the log, the records and the environment hold
// no token; each request is audited by the target's ref and purpose
// (mesh-specs gap 32, items 2, 3 and 6).
func TestTheRelayTradesAJobsFederationTokenAtTheCloud(t *testing.T) {
	needCurl(t)
	g, sb, _ := podRig(t)
	logs := &logSink{t: t}
	g.rt.Logf = logs.logf
	host := injectHostRig(t, g)
	fg := fedRig(t, g)
	g.latchkey.fedTarget("fed_prod", fg, fedSA)
	dir := t.TempDir()
	answers := filepath.Join(dir, "answers")
	sb.Agent = hostAgent(t, filepath.Join(dir, "claude"), curlScript(answers,
		`-H "Authorization: Bearer $GOOGLE_OAUTH_ACCESS_TOKEN" https://storage.googleapis.com/storage/v1/b?project=runsheet-prod`,
		`-H "Authorization: Bearer $CLOUDSDK_AUTH_ACCESS_TOKEN" https://compute.googleapis.com/compute/v1/projects/runsheet-prod/zones`,
		`-H "Authorization: Bearer mine" https://storage.googleapis.com/storage/v1/b`,
		`https://compute.googleapis.com/compute/v1/projects`,
	))
	g.offer(t, fedBrief)
	run(t, g, context.Background())
	if n := len(g.fake.Calls("finish")); n != 1 {
		t.Fatalf("%d finishes; hand-backs %+v", n, g.fake.Calls("hand-back"))
	}
	got, _ := os.ReadFile(answers)
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	if len(lines) != 4 {
		t.Fatalf("the pod's answers %q", got)
	}
	for i, l := range lines {
		if !strings.Contains(l, " 200") {
			t.Fatalf("answer %d is %q", i, l)
		}
	}
	cloud := fedCloudPrefix + "0001"
	seen := strings.Join(host.lines(), "\n")
	for _, want := range []string{
		`GET storage.googleapis.com /storage/v1/b?project=runsheet-prod auth="Bearer ` + cloud + `"`,
		`GET compute.googleapis.com /compute/v1/projects/runsheet-prod/zones? auth="Bearer ` + cloud + `"`,
		`GET storage.googleapis.com /storage/v1/b? auth="Bearer ` + cloud + `"`,
		`GET compute.googleapis.com /compute/v1/projects? auth="Bearer ` + cloud + `"`,
	} {
		if !strings.Contains(seen, want) {
			t.Fatalf("the hosts saw\n%s\nnot %s", seen, want)
		}
	}
	if strings.Contains(seen, "mine") || strings.Contains(seen, "Bearer mesh") {
		t.Fatalf("a sandbox's Authorization went out:\n%s", seen)
	}
	// minted once, on {target, purpose} alone, the purpose the kind's
	bodies, _ := g.latchkey.fedMints()
	if len(bodies) != 1 || bodies[0] != `{"purpose":"configure","target":"fed_prod"}`+"\n" && bodies[0] != `{"purpose":"configure","target":"fed_prod"}` {
		t.Fatalf("Latchkey was asked %q", bodies)
	}
	fg.mu.Lock()
	sts, iam := fg.sts, fg.iam
	fg.mu.Unlock()
	if len(sts) != 1 || sts[0].Get("scope") != googleScope || len(iam) != 1 || iam[0] != fedSA+" 3600s "+googleScope {
		t.Fatalf("Google was asked sts %v iam %v", sts, iam)
	}
	env := sb.AgentSpecs()[0].Env
	for _, name := range googleTokenEnv {
		if envValue(env, name) != egress.Placeholder {
			t.Fatalf("the pod's %s is %q", name, envValue(env, name))
		}
	}
	assertNoToken(t, "the pod's environment", strings.Join(env, "\n"))
	assertNoToken(t, "the node's log", logs.String())
	var injected []string
	for _, c := range g.fake.Calls("access") {
		assertNoToken(t, "an access record", string(c.Body))
		var req meshapi.AccessRequest
		json.Unmarshal(c.Body, &req)
		for _, r := range req.Records {
			if strings.HasPrefix(r.Service, "inject ") {
				injected = append(injected, fmt.Sprintf("%s %s %s %s %s", r.Vendor, r.Service, r.Path, r.Outcome, r.Reason))
			}
		}
	}
	want := strings.Repeat("federation inject bearer fed_prod ok 200 purpose configure\n", 4)
	if strings.Join(injected, "\n")+"\n" != want {
		t.Fatalf("the injected requests recorded\n%s", strings.Join(injected, "\n"))
	}
}

// A federation token the relay cannot have as the lease opens hands the
// lease back before its session runs, its reason naming
// credential_refused and Latchkey's code or gcp_sts / gcp_iam and
// Google's error: product for a refusal of the product's configuration,
// credential_timeout for Latchkey or the cloud not serving (mesh-specs
// gap 32 item 5, gap 14).
func TestAFederationTokenRefusedAtTheStartHandsTheLeaseBack(t *testing.T) {
	for _, tc := range []struct {
		name  string
		set   func(l *fakeLatchkey, fg *fakeGoogle)
		cause string
		says  []string
	}{
		{"purpose_not_granted", func(l *fakeLatchkey, _ *fakeGoogle) {
			l.fedRefuse("fed_prod", carriedRefusal{http.StatusForbidden, "purpose_not_granted"})
		}, meshapi.CauseProduct, []string{"credential_refused", "purpose_not_granted"}},
		{"not_the_jobs", func(l *fakeLatchkey, _ *fakeGoogle) {
			l.fedRefuse("fed_prod", carriedRefusal{http.StatusForbidden, "not_the_jobs"})
		}, meshapi.CauseProduct, []string{"credential_refused", "not_the_jobs"}},
		{"target_disabled", func(l *fakeLatchkey, _ *fakeGoogle) {
			l.fedRefuse("fed_prod", carriedRefusal{http.StatusConflict, "target_disabled"})
		}, meshapi.CauseProduct, []string{"credential_refused", "target_disabled"}},
		{"target_not_found", func(l *fakeLatchkey, _ *fakeGoogle) {
			l.fedRefuse("fed_prod", carriedRefusal{http.StatusNotFound, "target_not_found"})
		}, meshapi.CauseProduct, []string{"credential_refused", "target_not_found"}},
		{"feature_off", func(l *fakeLatchkey, _ *fakeGoogle) {
			l.fedRefuse("fed_prod", carriedRefusal{http.StatusNotFound, ""})
		}, meshapi.CauseProduct, []string{"credential_refused", "target_not_found"}},
		{"latchkey_unavailable", func(l *fakeLatchkey, _ *fakeGoogle) {
			l.fedRefuse("fed_prod", carriedRefusal{http.StatusServiceUnavailable, "temporarily_unavailable"})
		}, meshapi.CauseCredentialTimeout, []string{"credential_unavailable"}},
		{"gcp_sts", func(_ *fakeLatchkey, fg *fakeGoogle) {
			fg.refuseSTS = &googleRefusal{http.StatusBadRequest, `{"error":"invalid_grant","error_description":"The audience in ID Token does not match the expected audience."}`}
		}, meshapi.CauseProduct, []string{"credential_refused", "gcp_sts", "invalid_grant", "does not match"}},
		{"gcp_iam", func(_ *fakeLatchkey, fg *fakeGoogle) {
			fg.refuseIAM = &googleRefusal{http.StatusForbidden, `{"error":{"code":403,"message":"Permission 'iam.serviceAccounts.getAccessToken' denied","status":"PERMISSION_DENIED"}}`}
		}, meshapi.CauseProduct, []string{"credential_refused", "gcp_iam", "PERMISSION_DENIED", "getAccessToken"}},
		{"gcp_unavailable", func(_ *fakeLatchkey, fg *fakeGoogle) {
			fg.refuseSTS = &googleRefusal{http.StatusServiceUnavailable, `{"error":"temporarily_unavailable"}`}
		}, meshapi.CauseCredentialTimeout, []string{"credential_unavailable", "gcp_sts"}},
		{"aws", func(l *fakeLatchkey, _ *fakeGoogle) {
			l.mu.Lock()
			l.fed.cloud = map[string]string{"fed_prod": "aws"}
			l.mu.Unlock()
		}, meshapi.CauseProduct, []string{"credential_refused", "cloud_not_supported", "aws"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, sb, _ := podRig(t)
			logs := &logSink{t: t}
			g.rt.Logf = logs.logf
			injectHostRig(t, g)
			fg := fedRig(t, g)
			g.latchkey.fedTarget("fed_prod", fg, fedSA)
			tc.set(g.latchkey, fg)
			ran := false
			sb.Agent = func(context.Context, sandbox.AgentSpec) (sandbox.Exit, error) { ran = true; return sandbox.Exit{}, nil }
			job := g.offer(t, fedBrief)
			run(t, g, context.Background())
			hb := onlyHandBack(t, g, job)
			if hb.Cause != tc.cause || !strings.Contains(hb.Reason, "fed_prod") {
				t.Fatalf("handed back %+v", hb)
			}
			for _, s := range tc.says {
				if !strings.Contains(hb.Reason, s) {
					t.Fatalf("the reason %q does not say %q", hb.Reason, s)
				}
			}
			if ran {
				t.Fatal("the session ran without its cloud token")
			}
			assertNoToken(t, "the hand-back", hb.Reason)
			assertNoToken(t, "the node's log", logs.String())
		})
	}
}

// An exchange whose STS or IAM URL is not Google's is never sent a
// token: the node's own check, as every build runs it, refuses it before
// any request.
func TestAFederationExchangeOffGoogleIsSentNoToken(t *testing.T) {
	g, sb, _ := podRig(t)
	injectHostRig(t, g)
	fg := newFakeGoogle(t, g.latchkey)
	g.rt.federationHTTP = fg.srv.Client()
	g.latchkey.fedTarget("fed_prod", fg, fedSA)
	ran := false
	sb.Agent = func(context.Context, sandbox.AgentSpec) (sandbox.Exit, error) { ran = true; return sandbox.Exit{}, nil }
	job := g.offer(t, fedBrief)
	run(t, g, context.Background())
	hb := onlyHandBack(t, g, job)
	if hb.Cause != meshapi.CauseCredentialTimeout || !strings.Contains(hb.Reason, "gcp_sts") || !strings.Contains(hb.Reason, "not Google's sts.googleapis.com") {
		t.Fatalf("handed back %+v", hb)
	}
	if ran || fg.calls() != 0 {
		t.Fatalf("ran %v; the fake Google was called %d times", ran, fg.calls())
	}
}

// googleURL takes Google's two endpoints on https and nothing else.
func TestGoogleURLTakesGooglesEndpointsAlone(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://sts.googleapis.com/v1/token":                true,
		"https://sts.googleapis.com:443/v1/token":            true,
		"https://STS.googleapis.com/v1/token":                true,
		"http://sts.googleapis.com/v1/token":                 false,
		"https://sts.googleapis.com.evil.test/v1/token":      false,
		"https://evil.test/v1/token?host=sts.googleapis.com": false,
		"https://user@sts.googleapis.com/v1/token":           false,
		"https://sts.googleapis.com:8443/v1/token":           false,
		"https://iamcredentials.googleapis.com/v1":           false,
		"https://127.0.0.1/v1/token":                         false,
		"sts.googleapis.com/v1/token":                        false,
	} {
		if _, err := googleURL(raw, googleSTSHost); (err == nil) != ok {
			t.Errorf("googleURL(%q) = %v, want ok %v", raw, err, ok)
		}
	}
	if _, err := googleURL("https://iamcredentials.googleapis.com/v1", googleIAMHost); err != nil {
		t.Fatal(err)
	}
}

// A held federation token is minted and traded again a minute before the
// cloud token's end, for as long as the lease lives; every token is
// heard by the redactor before it is used; a refusal drops what was held;
// and at the lease's end the token is dropped and never minted again.
func TestAFederationTokenIsRefreshedBeforeItLapsesAndDroppedAtTheEnd(t *testing.T) {
	l := newFakeLatchkey(t)
	fg := newFakeGoogle(t, l)
	fg.life = 10 * time.Minute
	l.fedTarget("fed_prod", fg, fedSA)
	now := time.Now()
	mints := 0
	var mintErr error
	var heard []string
	redactor := newInjectedValues("lease-fed", t.Logf)
	h := &heldFederation{ref: "fed_prod", purpose: "configure", now: func() time.Time { return now }, logf: t.Logf,
		trader: gcpTrader{http: fg.srv.Client(), check: func(raw, _ string) (*url.URL, error) { return url.Parse(raw) }},
		held: func(ref string, c *egress.Credential) {
			redactor.add(ref, c)
			for name, v := range c.Fields {
				heard = append(heard, ref+" "+name+" "+v)
			}
		},
		mint: func(ctx context.Context, ref, purpose string) (*federationToken, error) {
			mints++
			if mintErr != nil {
				return nil, mintErr
			}
			n := mints
			l.mu.Lock()
			tok := fmt.Sprintf("%s%04d", fedSubjectPrefix, n)
			l.fed.issued = append(l.fed.issued, tok)
			ex, _ := json.Marshal(l.fed.exchange[ref])
			l.mu.Unlock()
			return &federationToken{SubjectToken: tok, Cloud: "gcp", TargetRef: ref, Exchange: ex}, nil
		}}
	get := func() (*egress.Credential, error) { return h.Credential(context.Background()) }
	c, err := get()
	if err != nil || mints != 1 {
		t.Fatalf("first: %v %d", err, mints)
	}
	if v, _ := c.Value(); v != fedCloudPrefix+"0001" {
		t.Fatalf("the cloud token is %q", v)
	}
	want := []string{"fed_prod subject_token " + fedSubjectPrefix + "0001", "fed_prod sts_token " + fedSTSPrefix + "0001", "fed_prod access_token " + fedCloudPrefix + "0001"}
	if strings.Join(heard, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the redactor heard\n%s", strings.Join(heard, "\n"))
	}
	// every token reads as the target's mark wherever it might echo
	line := redactor.Redact("subject " + fedSubjectPrefix + "0001 sts " + fedSTSPrefix + "0001 cloud Bearer " + fedCloudPrefix + "0001")
	if line != "subject [secret:fed_prod] sts [secret:fed_prod] cloud Bearer [secret:fed_prod]" {
		t.Fatalf("redacted: %q", line)
	}
	// held until a minute before its end
	now = c.ExpiresAt.Add(-federationRefreshBefore - time.Second)
	if _, err := get(); err != nil || mints != 1 {
		t.Fatalf("re-minted %d times before it was due", mints)
	}
	now = c.ExpiresAt.Add(-federationRefreshBefore + time.Second)
	c2, err := get()
	if v, _ := c2.Value(); err != nil || mints != 2 || v != fedCloudPrefix+"0002" {
		t.Fatalf("the refresh: %v %d %q", err, mints, v)
	}
	// a refusal at the next refresh drops the token and says why
	now = c2.ExpiresAt
	mintErr = &latchkeyError{Method: "POST", Path: federationPath, Status: http.StatusConflict, Code: "target_disabled"}
	_, err = get()
	var rf *egress.Refusal
	if !errors.As(err, &rf) || rf.Status != http.StatusBadGateway || rf.Code != egress.CodeRefused || !strings.Contains(rf.Message, "target_disabled") {
		t.Fatalf("a refused refresh: %v", err)
	}
	mintErr = nil
	if _, err := get(); err != nil || mints != 4 {
		t.Fatalf("taken again: %v %d", err, mints)
	}
	// the lease ends: its releaser drops the token, never minted again
	rt := &Runtime{Logf: t.Logf}
	vals := rt.openInjected("lease-end")
	vals.setReleaser(&releaser{fed: func() []*heldFederation { return []*heldFederation{h} }})
	rt.releaseCarried("lease-end")
	if _, err := get(); !errors.As(err, &rf) || rf.Code != egress.CodeUnavailable || mints != 4 {
		t.Fatalf("after the lease: %v %d", err, mints)
	}
	h.mu.Lock()
	held := h.cred
	h.mu.Unlock()
	if held != nil {
		t.Fatal("a token outlived the lease")
	}
}

// A brief's federation purpose is the offer's, else the kind's name.
func TestAFederationPurposeIsTheKindsByDefault(t *testing.T) {
	for _, tc := range []struct {
		f    meshapi.FederationRef
		kind string
		want string
	}{
		{meshapi.FederationRef{Ref: "t"}, "runsheet/configure", "configure"},
		{meshapi.FederationRef{Ref: "t"}, "deploy", "deploy"},
		{meshapi.FederationRef{Ref: "t", Purpose: "Observe"}, "runsheet/configure", "observe"},
	} {
		if got := federationPurpose(tc.f, tc.kind); got != tc.want {
			t.Errorf("federationPurpose(%+v, %q) = %q, want %q", tc.f, tc.kind, got, tc.want)
		}
	}
}

// A federation target is injected on the kind's egress hosts under
// *.googleapis.com alone, a host of one target never another's; a target
// whose hosts the egress names none of is not minted.
func TestAFederationTargetIsInjectedOnItsEgressHostsAlone(t *testing.T) {
	g, _, _ := podRig(t)
	fg := fedRig(t, g)
	g.latchkey.fedTarget("fed_a", fg, fedSA)
	g.latchkey.fedTarget("fed_b", fg, "")
	tokens := leaseTokens(t, g)
	b := &meshapi.Brief{LeaseID: "lease-hosts", Kind: "deploy",
		Egress: &meshapi.Egress{Hosts: []string{"storage.googleapis.com", "googleapis.com", "*.googleapis.com", "api.example.test"}},
		Access: &meshapi.Access{Federation: []meshapi.FederationRef{{Ref: "fed_a"}, {Ref: "fed_b", Purpose: "observe"}}}}
	allowed := egress.NewPolicy(b.Egress.Hosts, nil, nil)
	rules, env, held, err := g.rt.federationRules(context.Background(), b, tokens, allowed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || len(held) != 1 {
		t.Fatalf("%d rules, %d held", len(rules), len(held))
	}
	ir := rules[0].(*egress.InjectRule)
	if ir.Ref != "fed_a" || strings.Join(ir.Hosts(), ",") != "storage.googleapis.com" || ir.Purpose != "deploy" {
		t.Fatalf("the rule %+v on %v", ir, ir.Hosts())
	}
	if strings.Join(env, ",") != "CLOUDSDK_AUTH_ACCESS_TOKEN=mesh,GOOGLE_OAUTH_ACCESS_TOKEN=mesh" {
		t.Fatalf("env %v", env)
	}
	// fed_b had no host left: not minted
	bodies, _ := g.latchkey.fedMints()
	if len(bodies) != 1 || !strings.Contains(bodies[0], `"fed_a"`) {
		t.Fatalf("Latchkey was asked %q", bodies)
	}
}

// leaseTokens is a lease's session on the rig's fake Latchkey.
func leaseTokens(t *testing.T, g *rig) *sessionTokens {
	t.Helper()
	key, err := dpop.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	cred, _ := g.latchkey.open("lease-hosts", key.Thumbprint())
	return &sessionTokens{cred: cred, key: key, http: http.DefaultClient}
}
