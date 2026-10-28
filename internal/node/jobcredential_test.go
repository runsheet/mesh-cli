package node

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/tools"
)

// issuerOpener mints session tokens issued by iss.
type issuerOpener struct{ iss string }

func (o issuerOpener) OpenMeshSession(context.Context, gateway.MeshSessionRequest) (*gateway.MeshSession, error) {
	return &gateway.MeshSession{SessionID: "job-1"}, nil
}

func (o issuerOpener) MeshSessionToken(context.Context, *gateway.MeshSession, string, gateway.Prover) (string, time.Duration, bool, error) {
	payload, _ := json.Marshal(map[string]any{"iss": o.iss, "session_id": "job-1"})
	return "eyJhbGciOiJFUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".sig", time.Hour, true, nil
}

// forJobBearers is SessionBearers with the claim's session open, its
// tokens issued by the Latchkey at url.
func forJobBearers(t *testing.T, url string) (*SessionBearers, gateway.WorkItem) {
	t.Helper()
	key, err := dpop.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	item := gateway.WorkItem{ID: "item-1", Attempts: 2}
	b := &SessionBearers{GW: issuerOpener{iss: url}, Key: key}
	b.sessions = map[string]*gateway.MeshSession{jobKey(item.ID, item.Attempts): {SessionID: "job-1"}}
	return b, item
}

// The node reads a build's account from Latchkey's for-job route on its
// mesh session: the session token on Authorization: DPoP with a proof,
// a nonce Latchkey asks for answered once, and the answer made the
// platform's credential shape — the account's ref its id and label.
func TestForJobReadsTheJobsCredential(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/credentials/for-job" {
			http.NotFound(w, r)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "DPoP eyJ") || r.Header.Get("DPoP") == "" {
			http.Error(w, "no DPoP", http.StatusUnauthorized)
			return
		}
		if calls.Load() == 1 {
			w.Header().Set("DPoP-Nonce", "n-1")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"secret": "sk-ant-oat01-secret", "vendor": "anthropic",
			"kind": "personal_subscription", "plan": "max", "account_ref": "acct_shh"})
	}))
	defer srv.Close()
	b, item := forJobBearers(t, srv.URL)
	cr, err := b.ForJob(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	if cr.ID != "acct_shh" || cr.Label != "acct_shh" || cr.Kind != AccountSubscription || cr.Adapter != AdapterClaudeCode || cr.Secret != "sk-ant-oat01-secret" {
		t.Fatalf("credential %+v", cr)
	}
	if calls.Load() != 2 {
		t.Fatalf("%d calls, want the nonce answered once", calls.Load())
	}
}

// A Latchkey without the route is ErrForJobUnsupported; a refusal is a
// *ForJobRefusal naming why, never a fallback to another account.
func TestForJobRefusalAndUnsupported(t *testing.T) {
	for _, c := range []struct {
		status int
		want   func(error) bool
	}{
		{http.StatusNotFound, func(err error) bool { return errors.Is(err, ErrForJobUnsupported) }},
		{http.StatusForbidden, func(err error) bool {
			var r *ForJobRefusal
			return errors.As(err, &r) && r.Status == 403 && strings.Contains(r.Reason, "not active")
		}},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "access_denied", "error_description": "the account link is not active"})
		}))
		b, item := forJobBearers(t, srv.URL)
		_, err := b.ForJob(context.Background(), item)
		srv.Close()
		if !c.want(err) {
			t.Fatalf("status %d: err %v", c.status, err)
		}
	}
}

// openCreds is a CredentialGateway whose platform read issues one Claude
// subscription of the node's own and one tool.
type openCreds struct{ reads int }

func (g *openCreds) Session(context.Context, gateway.SessionRef, string) (*gateway.Session, error) {
	g.reads++
	return &gateway.Session{Credentials: []gateway.Credential{{ID: "c-own", Label: "own", Kind: AccountSubscription, Adapter: AdapterClaudeCode, Secret: "own-secret"}},
		Tools: []tools.Tool{{Name: "context7"}}}, nil
}

func (g *openCreds) RecordToolUsage(context.Context, string, string, string, gateway.UsageReport) error {
	return nil
}

// A build Mesh picked an account for runs on that account and takes the
// platform's read for its tools alone; a refused read releases the
// build (ErrNoAccount) rather than run it on the node's own account; a
// Latchkey without the route falls back to the node's own path.
func TestOpenTakesMeshsPickForTheJob(t *testing.T) {
	ref := gateway.SessionRef{Kind: "build", ItemID: "item-1"}
	pick := func(context.Context) (gateway.Credential, error) {
		return gateway.Credential{ID: "acct_shh", Label: "acct_shh", Kind: AccountSubscription, Adapter: AdapterClaudeCode, Secret: "picked-secret"}, nil
	}
	gw := &openCreds{}
	c := &Credentials{GW: gw}
	s, err := c.Open(withJobCredential(context.Background(), pick), ref, "node-1", AdapterClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	if s.account.Label != "acct_shh" || len(s.Tools) != 1 {
		t.Fatalf("opened on %q with tools %v, want Mesh's pick and the platform's tools", s.account.Label, s.Tools)
	}
	s.Cleanup()

	refused := func(context.Context) (gateway.Credential, error) {
		return gateway.Credential{}, &ForJobRefusal{Status: 403, Reason: "the account link is not active"}
	}
	if _, err := c.Open(withJobCredential(context.Background(), refused), ref, "node-1", AdapterClaudeCode); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("a refused read: %v, want ErrNoAccount", err)
	}

	unsupported := func(context.Context) (gateway.Credential, error) { return gateway.Credential{}, ErrForJobUnsupported }
	s, err = c.Open(withJobCredential(context.Background(), unsupported), ref, "node-1", AdapterClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	if s.account.Label != "own" {
		t.Fatalf("fell back to %q, want the node's own account", s.account.Label)
	}
	s.Cleanup()

	codex := func(context.Context) (gateway.Credential, error) {
		return gateway.Credential{ID: "acct_oai", Label: "acct_oai", Kind: AccountSubscription, Adapter: AdapterCodex, Secret: "x"}, nil
	}
	if _, err := c.Open(withJobCredential(context.Background(), codex), ref, "node-1", AdapterClaudeCode); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("an account of another adapter: %v, want ErrNoAccount", err)
	}
}

// A queued build's credential_ref reaches the build's context.
func TestAQueuedBuildCarriesItsCredentialRef(t *testing.T) {
	r := newQueueRig(t, 1)
	var got string
	r.loop.Dispatch = func(ctx context.Context, it gateway.WorkItem) { got = CredentialRefFrom(ctx) }
	r.f.Queue = []gatewaytest.QueuedJob{{Kind: "runsheet/build", SubjectType: "item", SubjectID: item1, CredentialRef: "3abb0726-e444-5cb0-b9b6-2dec9961a32e"}}
	r.tick()
	if got != "3abb0726-e444-5cb0-b9b6-2dec9961a32e" {
		t.Fatalf("the build's ctx carries %q\n%s", got, r.logs.String())
	}
}
