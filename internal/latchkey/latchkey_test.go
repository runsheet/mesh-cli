package latchkey_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/latchkey"
	"projectmesh.io/mesh-cli/internal/latchkey/latchkeytest"
)

const (
	clientID = "6d2a0f11-0000-4000-8000-000000000001"
	operator = "9b2e6d41-1f7a-4a3c-8c55-0d6e7f1a2b3c"
)

func newFake(t *testing.T) *latchkeytest.Fake {
	t.Helper()
	f := latchkeytest.New(clientID, "runsheet", operator)
	t.Cleanup(f.Close)
	return f
}

// The whole flow a `mesh node login` runs: discovery, the device grant,
// polls while the person signs in, and the machine token that comes back.
func TestDeviceFlow(t *testing.T) {
	f := newFake(t)
	f.PendingPolls = 2
	c := &latchkey.Client{Issuer: f.URL(), ClientID: clientID}
	ctx := context.Background()

	ep, err := c.Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ep.DeviceAuthorization != f.URL()+"/oauth/device" || ep.Token != f.URL()+"/oauth/token" {
		t.Fatalf("endpoints: %+v", ep)
	}

	da, err := c.StartDevice(ctx, ep, "mini")
	if err != nil {
		t.Fatal(err)
	}
	if da.UserCode == "" || da.DeviceCode == "" {
		t.Fatalf("device auth: %+v", da)
	}
	if want := f.URL() + "/oauth/device?user_code=" + da.UserCode; da.URL() != want {
		t.Errorf("URL(): got %q want %q", da.URL(), want)
	}

	// The CLI polls while the person is still signing in.
	if _, err := c.PollDevice(ctx, ep, da.DeviceCode); latchkey.Code(err) != latchkey.ErrAuthorizationPending {
		t.Fatalf("first poll: got %v, want authorization_pending", err)
	}

	id := f.Approve(da.UserCode, "") // the consent page: register and approve
	if id == "" {
		t.Fatal("approve: no machine registered")
	}
	if got := f.MachineName(id); got != "mini" {
		t.Errorf("machine name: got %q want mini (the name the CLI sent)", got)
	}

	da.Interval = 0 // poll without waiting; the flow is the same
	toks, err := c.Await(ctx, ep, da, nil)
	if err != nil {
		t.Fatal(err)
	}
	if toks.RefreshToken == "" || toks.ExpiresIn <= 0 {
		t.Fatalf("tokens: %+v", toks)
	}

	claims, err := latchkey.ParseClaims(toks.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Role != "machine" {
		t.Errorf("role: got %q want machine", claims.Role)
	}
	if claims.Subject != "machine:"+id || claims.MachineID() != id {
		t.Errorf("sub: got %q want machine:%s", claims.Subject, id)
	}
	if claims.Org != "runsheet" || claims.Operator.Sub != operator {
		t.Errorf("org/operator: %q / %q", claims.Org, claims.Operator.Sub)
	}
	if claims.ExpiresAt().Before(time.Now()) {
		t.Errorf("exp: %s is already past", claims.ExpiresAt())
	}
	// The device code is single use: a replay finds nothing.
	if _, err := c.PollDevice(ctx, ep, da.DeviceCode); latchkey.Code(err) != "invalid_grant" {
		t.Errorf("replayed device code: got %v, want invalid_grant", err)
	}
}

// slow_down backs the polling off instead of failing.
func TestAwaitSlowsDown(t *testing.T) {
	f := newFake(t)
	f.SlowDown = true
	c := &latchkey.Client{Issuer: f.URL(), ClientID: clientID, SlowDown: time.Millisecond}
	ctx := context.Background()
	ep, err := c.Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	da, err := c.StartDevice(ctx, ep, "mini")
	if err != nil {
		t.Fatal(err)
	}
	f.Approve(da.UserCode, "")
	da.Interval = 0
	var notes []string
	if _, err := c.Await(ctx, ep, da, func(s string) { notes = append(notes, s) }); err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "slow_down") {
		t.Errorf("notes: %v, want one slow_down", notes)
	}
	if f.Polls() != 2 {
		t.Errorf("polls: got %d want 2 (one slow_down, one that works)", f.Polls())
	}
}

// A refused consent ends the flow with a plain message, not a retry.
func TestAwaitRefused(t *testing.T) {
	f := newFake(t)
	c := &latchkey.Client{Issuer: f.URL(), ClientID: clientID}
	ctx := context.Background()
	ep, _ := c.Discover(ctx)
	da, err := c.StartDevice(ctx, ep, "mini")
	if err != nil {
		t.Fatal(err)
	}
	f.Deny(da.UserCode)
	da.Interval = 0
	_, err = c.Await(ctx, ep, da, nil)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("await: got %v, want a refusal", err)
	}
}

// The refresh grant rotates: the new token works, the spent one does not.
func TestRefreshRotates(t *testing.T) {
	f := newFake(t)
	c := &latchkey.Client{Issuer: f.URL(), ClientID: clientID}
	ctx := context.Background()
	ep, _ := c.Discover(ctx)
	da, _ := c.StartDevice(ctx, ep, "mini")
	f.Approve(da.UserCode, "")
	da.Interval = 0
	first, err := c.Await(ctx, ep, da, nil)
	if err != nil {
		t.Fatal(err)
	}

	second, err := c.Refresh(ctx, ep.Token, first.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if second.AccessToken == first.AccessToken {
		t.Error("refresh returned the same access token")
	}
	if second.RefreshToken == first.RefreshToken {
		t.Error("refresh did not rotate the refresh token")
	}
	if _, err := c.Refresh(ctx, ep.Token, first.RefreshToken); latchkey.Code(err) != "invalid_grant" {
		t.Errorf("spent refresh token: got %v, want invalid_grant", err)
	}
}

// Revocation is immediate at Latchkey: the refresh is refused from then on.
func TestRefreshAfterRevoke(t *testing.T) {
	f := newFake(t)
	c := &latchkey.Client{Issuer: f.URL(), ClientID: clientID}
	ctx := context.Background()
	ep, _ := c.Discover(ctx)
	da, _ := c.StartDevice(ctx, ep, "mini")
	id := f.Approve(da.UserCode, "")
	da.Interval = 0
	toks, err := c.Await(ctx, ep, da, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.Revoke(id)
	if _, err := c.Refresh(ctx, ep.Token, toks.RefreshToken); latchkey.Code(err) != "invalid_grant" {
		t.Fatalf("refresh of a revoked machine: got %v, want invalid_grant", err)
	}
}

// A client the estate does not know gets an OAuth error, not a panic.
func TestStartDeviceUnknownClient(t *testing.T) {
	f := newFake(t)
	c := &latchkey.Client{Issuer: f.URL(), ClientID: "someone-else"}
	ctx := context.Background()
	ep, _ := c.Discover(ctx)
	if _, err := c.StartDevice(ctx, ep, "mini"); latchkey.Code(err) != "invalid_client" {
		t.Fatalf("got %v, want invalid_client", err)
	}
}

// An issuer without the device flow is named as such, before any code is
// printed at the person.
func TestDiscoverWithoutDeviceFlow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issuer":"https://auth.example","token_endpoint":"https://auth.example/oauth/token"}`))
	}))
	defer srv.Close()
	c := &latchkey.Client{Issuer: srv.URL}
	if _, err := c.Discover(context.Background()); err == nil || !strings.Contains(err.Error(), "device flow") {
		t.Fatalf("got %v, want a no-device-flow error", err)
	}
}

func TestParseClaimsRejectsNonJWT(t *testing.T) {
	for _, tok := range []string{"", "rs_0123456789", "a.b"} {
		if _, err := latchkey.ParseClaims(tok); err == nil {
			t.Errorf("ParseClaims(%q): expected an error", tok)
		}
	}
}

func TestMachineIDOfAPersonToken(t *testing.T) {
	// A person's token is a bare uuid subject — not a machine.
	c := &latchkey.Claims{Subject: operator}
	if got := c.MachineID(); got != "" {
		t.Errorf("MachineID of a person token: got %q want empty", got)
	}
}
