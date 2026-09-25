package commands

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/auth"
	"projectmesh.io/mesh-cli/internal/latchkey"
	"projectmesh.io/mesh-cli/internal/latchkey/latchkeytest"
	"projectmesh.io/mesh-cli/internal/node"
)

const (
	testClientID = "6d2a0f11-0000-4000-8000-000000000001"
	testOperator = "9b2e6d41-1f7a-4a3c-8c55-0d6e7f1a2b3c"
)

// fakeGateway is enough of the platform to answer a sign-in, recording
// the bearer each request carried.
type fakeGateway struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []string
}

func newFakeGateway(t *testing.T) *fakeGateway {
	t.Helper()
	f := &fakeGateway{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.got = append(f.got, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		f.mu.Unlock()
		var body struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(body.Query, "signIn") {
			_, _ = w.Write([]byte(`{"data":{"signIn":{"id":"user-1"}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"myOrgs":[{"id":"org-1","login":"runsheet","accountId":"acct-1","role":"admin"}]}}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// bearers is every token the gateway was handed, in order.
func (f *fakeGateway) bearers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

// login sets the CLI up against a fake Latchkey and a fake gateway, runs
// `mesh node login` with no argument, and returns them.
func login(t *testing.T) (*latchkeytest.Fake, *fakeGateway) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	lk := latchkeytest.New(testClientID, "runsheet", testOperator)
	t.Cleanup(lk.Close)
	gw := newFakeGateway(t)

	set(t, &latchkeyFlag, lk.URL())
	set(t, &latchkeyClientFlag, testClientID)
	set(t, &machineNameFlag, "mini")
	set(t, &gatewayFlag, gw.srv.URL)
	noBrowser := noBrowserFlag
	noBrowserFlag = true
	t.Cleanup(func() { noBrowserFlag = noBrowser })

	// The person signing in and approving, while the CLI polls.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if codes := lk.UserCodes(); len(codes) > 0 {
				lk.Approve(codes[0], "")
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	if err := deviceLogin(context.Background()); err != nil {
		t.Fatalf("mesh node login: %v", err)
	}
	<-done
	return lk, gw
}

// set restores a package-level flag variable after the test.
func set(t *testing.T, p *string, v string) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

func savedCredential(t *testing.T) *auth.Session {
	t.Helper()
	cred, err := auth.LoadCredential()
	if err != nil {
		t.Fatal(err)
	}
	if !cred.IsMachine() {
		t.Fatal("~/.mesh/token does not hold a machine credential")
	}
	return cred.Machine
}

// `mesh node login` with no argument runs the device flow and leaves a
// machine credential — and what it saves is what a refresh needs.
func TestNodeLoginRunsTheDeviceFlow(t *testing.T) {
	lk, gw := login(t)

	sess := savedCredential(t)
	if sess.Kind != auth.MachineKind {
		t.Errorf("kind: got %q want %q", sess.Kind, auth.MachineKind)
	}
	if sess.TokenEndpoint != lk.URL()+"/oauth/token" || sess.ClientID != testClientID {
		t.Errorf("refresh needs the endpoint and client: %+v", sess)
	}
	if sess.RefreshToken == "" {
		t.Error("no refresh token saved")
	}
	if sess.ExpiresAt == nil || sess.ExpiresAt.Before(time.Now()) {
		t.Errorf("expiry: %v", sess.ExpiresAt)
	}

	claims, err := latchkey.ParseClaims(sess.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Role != "machine" || claims.Org != "runsheet" || claims.Operator.Sub != testOperator {
		t.Errorf("claims: %+v", claims)
	}
	if claims.Name != "mini" {
		t.Errorf("machine name: got %q want mini (--name)", claims.Name)
	}
	if sess.MachineID != claims.MachineID() || sess.UserID != testOperator || sess.DefaultOrg != "runsheet" {
		t.Errorf("credential does not record the machine and its operator: %+v", sess)
	}

	// The gateway was greeted as the machine, not as a console token.
	bearers := gw.bearers()
	if len(bearers) == 0 || bearers[0] != sess.AccessToken {
		t.Fatalf("the gateway saw %v, want the machine's access token", bearers)
	}

	// The issuer and client are remembered although this machine is not
	// registered yet — registering is itself done on the credential, so
	// a login before one must not be the login that needs --client-id.
	settings, err := node.LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.Latchkey != lk.URL() || settings.LatchkeyClient != testClientID {
		t.Errorf("node.yaml did not remember the flow: %+v", settings)
	}
	registered := *settings // as `mesh node register` writes it, carrying them over
	registered.Slug, registered.Name, registered.Capacity = "mini", "mini", node.Fixed(1)
	if err := registered.Save(); err != nil {
		t.Fatal(err)
	}
	set(t, &latchkeyClientFlag, "")
	t.Setenv("MESH_LATCHKEY_CLIENT_ID", "")
	if got := latchkeyClientID(); got != testClientID {
		t.Errorf("a second login needs no --client-id: got %q", got)
	}
}

// The gateway client — what `mesh node start` and the goal commands run
// on — refreshes the machine token before it expires and keeps the
// rotated pair for the next process.
func TestPlatformClientRefreshesTheMachineToken(t *testing.T) {
	_, gw := login(t)
	first := savedCredential(t)

	// Age the credential as a long-running node would.
	past := time.Now().Add(-time.Minute).UTC()
	first.ExpiresAt = &past
	if err := auth.SaveMachine(first); err != nil {
		t.Fatal(err)
	}

	client, err := platformClient()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.SignIn(context.Background()); err != nil {
		t.Fatal(err)
	}

	bearers := gw.bearers()
	sent := bearers[len(bearers)-1]
	if sent == first.AccessToken {
		t.Error("the gateway was given the expired token")
	}
	claims, err := latchkey.ParseClaims(sent)
	if err != nil {
		t.Fatal(err)
	}
	if claims.MachineID() != first.MachineID {
		t.Errorf("the refreshed token is for machine %q, want %q", claims.MachineID(), first.MachineID)
	}
	saved := savedCredential(t)
	if saved.AccessToken != sent {
		t.Error("the refreshed access token was not saved")
	}
	if saved.RefreshToken == first.RefreshToken {
		t.Error("the rotated refresh token was not saved; the next refresh would be refused")
	}
}

// An rs_* console token still runs the node: the same client, no refresh.
func TestPlatformClientOnAConsoleToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	gw := newFakeGateway(t)
	set(t, &gatewayFlag, gw.srv.URL)
	if err := auth.SaveToken("rs_console"); err != nil {
		t.Fatal(err)
	}
	client, err := platformClient()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.SignIn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := gw.bearers(); len(got) != 1 || got[0] != "rs_console" {
		t.Errorf("the gateway saw %v, want the console token", got)
	}
	if raw, err := os.ReadFile(filepath.Join(home, ".mesh", "token")); err != nil || strings.TrimSpace(string(raw)) != "rs_console" {
		t.Errorf("~/.mesh/token: %q, %v", raw, err)
	}
}

// Without a client id there is nothing to start the flow from, and the
// message says how to supply one.
func TestNodeLoginWithoutAClientID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MESH_LATCHKEY_CLIENT_ID", "")
	set(t, &latchkeyClientFlag, "")
	err := deviceLogin(context.Background())
	if err == nil || !strings.Contains(err.Error(), "--client-id") {
		t.Fatalf("got %v, want a missing-client-id error", err)
	}
}

// `mesh node status` opens with the credential this machine runs on:
// while both shapes are live an operator must be able to tell which one
// a node holds, and what to revoke in the Latchkey console.
func TestCredentialLine(t *testing.T) {
	expires := time.Now().Add(time.Hour)
	machine := &auth.Credential{Machine: &auth.Session{Kind: auth.MachineKind, AccessToken: "jwt",
		MachineID: "3f1c9a2e", DefaultOrg: "runsheet", UserID: "9b2e6d41", ExpiresAt: &expires}}
	line := credentialLine(machine)
	for _, want := range []string{"Latchkey machine", "3f1c9a2e", "in runsheet", "operator 9b2e6d41", "~/.mesh/token", "refreshed before it expires"} {
		if !strings.Contains(line, want) {
			t.Errorf("machine line %q lacks %q", line, want)
		}
	}
	machine.FromEnv = true
	if !strings.Contains(credentialLine(machine), "MESH_TOKEN") {
		t.Errorf("a MESH_TOKEN credential must say so: %q", credentialLine(machine))
	}

	console := credentialLine(&auth.Credential{Console: "rs_console"})
	for _, want := range []string{"rs_* console token", "the fallback", "mesh node login"} {
		if !strings.Contains(console, want) {
			t.Errorf("console line %q lacks %q", console, want)
		}
	}
}
