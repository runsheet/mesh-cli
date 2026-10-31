package nodev1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/egress"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// The carried credentials' values in these tests: none may be seen
// anywhere but on the wire to its own host.
const (
	injSecretToken  = "sec-bearer-value-0001"
	injSecretToken2 = "sec-bearer-value-0002"
	injChildToken   = "cf-child-token-value-01"
	injQueryKey     = "namecheap-api-key-0001"
)

// injectHost is a fake set of vendor hosts with TLS, reached by the
// lease's tunnel for every host: it echoes what credential each request
// carried.
type injectHost struct {
	mu   sync.Mutex
	seen []string
}

func (h *injectHost) lines() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.seen...)
}

func injectHostRig(t *testing.T, g *rig) *injectHost {
	t.Helper()
	h := &injectHost{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		line := fmt.Sprintf("%s %s %s?%s auth=%q key=%q body=%d", r.Method, r.Host, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), len(body))
		h.mu.Lock()
		h.seen = append(h.seen, line)
		h.mu.Unlock()
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

// injectBrief is a kind whose offer carried a Secret (bearer), a
// connection's child (Latchkey's {hosts, header, prefix}) and a Secret
// in the query form, every host in its egress.
func injectBrief(b *meshapi.Brief) {
	b.FinishTool, b.FinishSchema = "", nil
	b.Egress = &meshapi.Egress{Hosts: []string{"api.example.test", "api.cloudflare.test", "api.namecheap.test", "elsewhere.test"}, MCP: []string{"conform"}, Model: true}
	b.Access = &meshapi.Access{Repo: true, MCP: []string{"conform"}, Model: true,
		Secrets: []string{"sec_bearer", "sec_query"},
		Mint:    []meshapi.MintGrant{{ConnectionRef: "conn_cloudflare", Permissions: []string{"zone:dns:edit"}, TTL: 900}}}
}

func carryAll(l *fakeLatchkey) {
	exp := time.Now().Add(time.Hour).UTC()
	l.setCarried("sec_bearer", forJobCredential{Inject: egress.InjectSpec{Hosts: []string{"api.example.test"}, Form: "bearer", Paths: []string{"/v1/"}},
		Fields: map[string]string{"token": injSecretToken}, Version: json.RawMessage("1"), ExpiresAt: exp})
	l.setCarried("sec_query", forJobCredential{Inject: egress.InjectSpec{Hosts: []string{"api.namecheap.test"}, Form: "query",
		Params: map[string]string{"ApiUser": "{{username}}", "ApiKey": "{{api_key}}"}},
		Fields: map[string]string{"username": "acme", "api_key": injQueryKey}, Version: json.RawMessage("4"), ExpiresAt: exp})
	l.setCarried("conn_cloudflare", forJobCredential{Inject: egress.InjectSpec{Hosts: []string{"api.cloudflare.test"}, Header: "Authorization", Prefix: "Bearer "},
		Fields: map[string]string{"access_token": injChildToken}, ExpiresAt: exp, Revocable: true, Connection: "conn_cloudflare", Service: "cloudflare"})
}

// curlScript is the agent's pre-session script: each request through
// $HTTPS_PROXY on the lease's CA, its status and the relay's error header
// appended to answers.
func curlScript(answers string, calls ...string) string {
	var b strings.Builder
	for _, c := range calls {
		fmt.Fprintf(&b, `curl -sS --max-time 5 --cacert "$NODE_EXTRA_CA_CERTS" -o /dev/null -D - %s | tr -d '\r' | grep -iE '^(HTTP/|x-mesh-error)' | tr '\n' ' ' >> "%s"; echo >> "%s"; `, c, answers, answers)
	}
	return strings.TrimSuffix(b.String(), "; ")
}

func needCurl(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl on this machine")
	}
}

// logSink keeps every line the runtime logs.
type logSink struct {
	t  *testing.T
	mu sync.Mutex
	b  strings.Builder
}

func (s *logSink) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	s.mu.Lock()
	s.b.WriteString(line + "\n")
	s.mu.Unlock()
	s.t.Log(line)
}

func (s *logSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// assertNoValue fails when any carried value is in what.
func assertNoValue(t *testing.T, where, what string) {
	t.Helper()
	for _, v := range []string{injSecretToken, injSecretToken2, injChildToken, injQueryKey} {
		if strings.Contains(what, v) {
			t.Fatalf("%s carries a credential's value %q", where, v)
		}
	}
}

// The relay injects each credential the offer carried on its hosts
// alone, in its form — a Secret as a bearer and as query parameters, a
// connection's child as Latchkey's header rule — with the pod holding
// nothing but placeholders; a request the sandbox already credentialed
// is refused credential_preset, the placeholder replaced; every injected
// request is an AccessRecord naming the ref, form and version; no value
// is in the pod's environment, the node's log or a record (mesh-specs
// gap 28, items 2-4 and 8).
func TestTheRelayInjectsTheCarriedCredentials(t *testing.T) {
	needCurl(t)
	g, sb, _ := podRig(t)
	logs := &logSink{t: t}
	g.rt.Logf = logs.logf
	host := injectHostRig(t, g)
	carryAll(g.latchkey)
	dir := t.TempDir()
	answers := filepath.Join(dir, "answers")
	sb.Agent = hostAgent(t, filepath.Join(dir, "claude"), curlScript(answers,
		`https://api.example.test/v1/zones`,
		`https://api.example.test/v2/elsewhere`,
		`-H "Authorization: Bearer mine" https://api.example.test/v1/zones`,
		`-H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" https://api.cloudflare.test/client/v4/zones`,
		`"https://api.namecheap.test/xml.response?Command=namecheap.domains.getList"`,
	))
	g.offer(t, injectBrief)
	run(t, g, context.Background())
	if n := len(g.fake.Calls("finish")); n != 1 {
		t.Fatalf("%d finishes; hand-backs %+v", n, g.fake.Calls("hand-back"))
	}
	got, _ := os.ReadFile(answers)
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	if len(lines) != 5 {
		t.Fatalf("the pod's answers %q", got)
	}
	for i, want := range [][]string{{" 200"}, {" 200"}, {" 403", "x-mesh-error: credential_preset"}, {" 200"}, {" 200"}} {
		for _, w := range want {
			if !strings.Contains(strings.ToLower(lines[i]), w) {
				t.Fatalf("answer %d is %q, want %q", i, lines[i], want)
			}
		}
	}
	seen := strings.Join(host.lines(), "\n")
	for _, want := range []string{
		`GET api.example.test /v1/zones? auth="Bearer ` + injSecretToken + `"`,
		`GET api.example.test /v2/elsewhere? auth=""`,
		`GET api.cloudflare.test /client/v4/zones? auth="Bearer ` + injChildToken + `"`,
		`ApiKey=` + injQueryKey,
	} {
		if !strings.Contains(seen, want) {
			t.Fatalf("the hosts saw\n%s\nnot %s", seen, want)
		}
	}
	if strings.Contains(seen, "mine") {
		t.Fatalf("a preset credential went out:\n%s", seen)
	}
	env := sb.AgentSpecs()[0].Env
	if envValue(env, "CLOUDFLARE_API_TOKEN") != egress.Placeholder {
		t.Fatalf("the pod's CLOUDFLARE_API_TOKEN is %q", envValue(env, "CLOUDFLARE_API_TOKEN"))
	}
	assertNoValue(t, "the pod's environment", strings.Join(env, "\n"))
	assertNoValue(t, "the node's log", logs.String())
	var recs []meshapi.AccessRecord
	for _, c := range g.fake.Calls("access") {
		assertNoValue(t, "an access record", string(c.Body))
		var req meshapi.AccessRequest
		json.Unmarshal(c.Body, &req)
		recs = append(recs, req.Records...)
	}
	var injected []string
	for _, r := range recs {
		if strings.HasPrefix(r.Service, "inject ") {
			injected = append(injected, fmt.Sprintf("%s %s %s %s %s", r.Vendor, r.Service, r.Path, r.Outcome, r.Reason))
		}
	}
	want := []string{
		"secret inject bearer sec_bearer ok 200 version 1",
		"secret inject bearer sec_bearer refused credential_preset",
		"connection inject header conn_cloudflare ok 200",
		"secret inject query sec_query ok 200 version 4",
	}
	if strings.Join(injected, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the injected requests recorded\n%s\nwant\n%s", strings.Join(injected, "\n"), strings.Join(want, "\n"))
	}
	// each ref was asked once: at the lease's start
	for _, ref := range []string{"sec_bearer", "sec_query", "conn_cloudflare"} {
		if n := g.latchkey.asks(ref); n != 1 {
			t.Fatalf("%s asked %d times", ref, n)
		}
	}
}

// A credential Latchkey will not give as the lease opens hands the lease
// back before its session runs: product when Latchkey refused the
// product's configuration, credential_timeout when Latchkey or the
// vendor failed (mesh-specs gaps 14 and 28 item 7).
func TestACarriedCredentialRefusedAtTheStartHandsTheLeaseBack(t *testing.T) {
	for _, tc := range []struct {
		refusal carriedRefusal
		cause   string
	}{
		{carriedRefusal{http.StatusForbidden, "purpose_not_granted"}, meshapi.CauseProduct},
		{carriedRefusal{http.StatusConflict, "connection_needs_reauth"}, meshapi.CauseProduct},
		{carriedRefusal{http.StatusBadGateway, "vendor_unreachable"}, meshapi.CauseCredentialTimeout},
		{carriedRefusal{http.StatusServiceUnavailable, "temporarily_unavailable"}, meshapi.CauseCredentialTimeout},
	} {
		t.Run(tc.refusal.code, func(t *testing.T) {
			g, sb, _ := podRig(t)
			injectHostRig(t, g)
			carryAll(g.latchkey)
			g.latchkey.refuseCarried = map[string]carriedRefusal{"conn_cloudflare": tc.refusal}
			ran := false
			sb.Agent = func(context.Context, sandbox.AgentSpec) (sandbox.Exit, error) { ran = true; return sandbox.Exit{}, nil }
			job := g.offer(t, injectBrief)
			run(t, g, context.Background())
			hb := onlyHandBack(t, g, job)
			if hb.Cause != tc.cause || !strings.Contains(hb.Reason, "conn_cloudflare") || !strings.Contains(hb.Reason, tc.refusal.code) {
				t.Fatalf("handed back %+v", hb)
			}
			if ran {
				t.Fatal("the session ran without its credential")
			}
		})
	}
}

// A credential is asked again once it nears its expires_at, so a
// rotation reaches the running job; a re-ask Latchkey refuses drops the
// held value and refuses the rule's requests 502 credential_refused with
// Latchkey's code, never sending what it held before.
func TestARotatedCredentialReachesTheRunningJob(t *testing.T) {
	needCurl(t)
	g, sb, _ := podRig(t)
	host := injectHostRig(t, g)
	carryAll(g.latchkey)
	// due a second after it is taken (refreshSkew before its expiry)
	g.latchkey.setCarried("sec_bearer", forJobCredential{Inject: egress.InjectSpec{Hosts: []string{"api.example.test"}, Form: "bearer"},
		Fields: map[string]string{"token": injSecretToken}, Version: json.RawMessage("1"), ExpiresAt: time.Now().Add(refreshSkew + time.Second).UTC()})
	dir := t.TempDir()
	answers := filepath.Join(dir, "answers")
	rotate := filepath.Join(dir, "rotate")
	refuse := filepath.Join(dir, "refuse")
	sb.Agent = hostAgent(t, filepath.Join(dir, "claude"), curlScript(answers, `https://api.example.test/a`)+
		`; touch "`+rotate+`"; sleep 1.5; `+curlScript(answers, `https://api.example.test/b`)+
		`; touch "`+refuse+`"; sleep 1.5; `+curlScript(answers, `https://api.example.test/c`))
	// the fake rotates the secret when the agent says, then refuses it
	go func() {
		for i := 0; i < 400; i++ {
			if _, err := os.Stat(rotate); err == nil {
				g.latchkey.setCarried("sec_bearer", forJobCredential{Inject: egress.InjectSpec{Hosts: []string{"api.example.test"}, Form: "bearer"},
					Fields: map[string]string{"token": injSecretToken2}, Version: json.RawMessage("2"), ExpiresAt: time.Now().Add(refreshSkew + time.Second).UTC()})
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		for i := 0; i < 800; i++ {
			if _, err := os.Stat(refuse); err == nil {
				g.latchkey.mu.Lock()
				g.latchkey.refuseCarried = map[string]carriedRefusal{"sec_bearer": {http.StatusConflict, "secret_disabled"}}
				g.latchkey.mu.Unlock()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	g.offer(t, injectBrief)
	run(t, g, context.Background())
	got, _ := os.ReadFile(answers)
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], " 200") || !strings.Contains(lines[1], " 200") ||
		!strings.Contains(lines[2], " 502") || !strings.Contains(strings.ToLower(lines[2]), "credential_refused") {
		t.Fatalf("the pod's answers %q", got)
	}
	seen := strings.Join(host.lines(), "\n")
	if !strings.Contains(seen, `/a? auth="Bearer `+injSecretToken+`"`) || !strings.Contains(seen, `/b? auth="Bearer `+injSecretToken2+`"`) || strings.Contains(seen, "/c?") {
		t.Fatalf("the host saw\n%s", seen)
	}
	// asked at the start, and again for each request once due
	if n := g.latchkey.asks("sec_bearer"); n < 3 {
		t.Fatalf("sec_bearer asked %d times, want 3 or more", n)
	}
}

// A held credential is asked again at most every injectReask and at its
// expiry, dropped when the re-ask fails, and taken again once Latchkey
// answers.
func TestAHeldCredentialIsAskedAgainWhenDue(t *testing.T) {
	now := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	answer := &forJobCredential{Inject: egress.InjectSpec{Hosts: []string{"api.example.test"}, Form: "bearer"}, Fields: map[string]string{"token": "v1-token-value"},
		Version: json.RawMessage("1"), ExpiresAt: now.Add(time.Hour)}
	var askErr error
	asks := 0
	var held []string
	h := &heldCredential{ref: "sec_1", kind: injectSecret, now: func() time.Time { return now }, logf: t.Logf,
		held: func(ref string, c *egress.Credential) { held = append(held, c.Version) },
		ask: func(context.Context, string) (*forJobCredential, error) {
			asks++
			if askErr != nil {
				return nil, askErr
			}
			a := *answer
			return &a, nil
		}}
	get := func() (*egress.Credential, error) { return h.Credential(context.Background()) }
	if c, err := get(); err != nil || c.Version != "1" || asks != 1 {
		t.Fatalf("first: %+v %v %d", c, err, asks)
	}
	now = now.Add(injectReask - time.Second)
	if _, err := get(); err != nil || asks != 1 {
		t.Fatalf("asked again before it was due: %d", asks)
	}
	now = now.Add(2 * time.Second)
	answer.Fields, answer.Version = map[string]string{"token": "v2-token-value"}, json.RawMessage(`"2"`)
	answer.ExpiresAt = now.Add(time.Minute)
	if c, err := get(); err != nil || c.Version != "2" || c.Fields["token"] != "v2-token-value" || asks != 2 {
		t.Fatalf("the 15-minute re-ask: %+v %v %d", c, err, asks)
	}
	now = now.Add(time.Minute - refreshSkew)
	askErr = &latchkeyError{Method: "POST", Path: forJobPath, Status: http.StatusForbidden, Code: "purpose_not_granted"}
	_, err := get()
	var ref *egress.Refusal
	if !errors.As(err, &ref) || ref.Code != egress.CodeRefused || !strings.Contains(ref.Message, "purpose_not_granted") || asks != 3 {
		t.Fatalf("a refused re-ask at expiry: %v %d", err, asks)
	}
	if _, err := get(); err == nil || asks != 4 {
		t.Fatal("a dropped credential was used without asking again")
	}
	askErr = fmt.Errorf("dial tcp: connection refused")
	if _, err := get(); !errors.As(err, &ref) || ref.Code != egress.CodeUnavailable {
		t.Fatalf("Latchkey unreachable: %v", err)
	}
	askErr = nil
	if c, err := get(); err != nil || c.Version != "2" {
		t.Fatalf("taken again: %+v %v", c, err)
	}
	if strings.Join(held, ",") != "1,2,2" {
		t.Fatalf("held %v", held)
	}
}

// With a relay sidecar the node's tunnel injects just the same.
func TestARelaySidecarsSessionHasItsCredentialsInjected(t *testing.T) {
	needCurl(t)
	g, sb, _ := sidecarRig(t)
	host := injectHostRig(t, g)
	carryAll(g.latchkey)
	dir := t.TempDir()
	answers := filepath.Join(dir, "answers")
	sb.Agent = hostAgent(t, filepath.Join(dir, "claude"), curlScript(answers,
		`-H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" https://api.cloudflare.test/client/v4/zones`))
	g.offer(t, injectBrief)
	run(t, g, context.Background())
	if n := len(g.fake.Calls("finish")); n != 1 {
		t.Fatalf("%d finishes; hand-backs %+v", n, g.fake.Calls("hand-back"))
	}
	if seen := strings.Join(host.lines(), "\n"); !strings.Contains(seen, `auth="Bearer `+injChildToken+`"`) {
		t.Fatalf("the host saw\n%s", seen)
	}
	assertNoValue(t, "the pod's environment", strings.Join(sb.AgentSpecs()[0].Env, "\n"))
}
