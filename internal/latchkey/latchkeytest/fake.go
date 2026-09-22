// Package latchkeytest is an httptest stand-in for Latchkey: discovery,
// the RFC 8628 device flow and the refresh grant, minting machine tokens
// with the claim set oidc/machine.go signs (sub machine:<id>, role
// machine, org, ns, operator). The signature is a stub — nothing in the
// CLI verifies it; the gateway does.
package latchkeytest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// Fake is the server and its state.
type Fake struct {
	Server *httptest.Server
	// ClientID is the public client the CLI must start the flow from.
	ClientID string
	// Org and Operator ride every minted token.
	Org      string
	Operator string
	// AccessTTL is the life of a minted access token (default 1h).
	AccessTTL time.Duration
	// PendingPolls is how many polls answer authorization_pending before
	// the approval is handed over; SlowDown answers slow_down once first.
	PendingPolls int
	SlowDown     bool

	mu       sync.Mutex
	grants   map[string]*grant // device code → grant
	byUser   map[string]*grant // user code → grant
	machines map[string]*machine
	refresh  map[string]string // refresh token → machine id
	polls    int
	seq      int
	// Issued is every access token minted, in order.
	Issued []string
}

type grant struct {
	deviceCode string
	userCode   string
	name       string
	approved   bool
	denied     bool
	consumed   bool
	machineID  string
	expiresAt  time.Time
}

type machine struct {
	id      string
	name    string
	revoked bool
}

// New starts a fake that accepts clientID and registers machines for
// operator in org.
func New(clientID, org, operator string) *Fake {
	f := &Fake{
		ClientID: clientID, Org: org, Operator: operator, AccessTTL: time.Hour,
		grants: map[string]*grant{}, byUser: map[string]*grant{}, machines: map[string]*machine{}, refresh: map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", f.discovery)
	mux.HandleFunc("/oauth/device", f.device)
	mux.HandleFunc("/oauth/token", f.token)
	f.Server = httptest.NewServer(mux)
	return f
}

// URL is the fake's issuer.
func (f *Fake) URL() string { return f.Server.URL }

// Close shuts the server down.
func (f *Fake) Close() { f.Server.Close() }

// Polls is how many device-code polls the fake has answered.
func (f *Fake) Polls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.polls
}

// UserCodes lists the user codes of the grants started so far, in order
// of their device codes — a test with one flow takes the only one.
func (f *Fake) UserCodes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	codes := make([]string, 0, len(f.byUser))
	for code := range f.byUser {
		codes = append(codes, code)
	}
	return codes
}

// Approve is the person signing in and consenting: it registers the
// machine and approves the grant, as the consent page does. It returns
// the machine's id.
func (f *Fake) Approve(userCode, name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := f.byUser[strings.ToUpper(strings.TrimSpace(userCode))]
	if g == nil {
		return ""
	}
	if name == "" {
		name = g.name
	}
	f.seq++
	id := fmt.Sprintf("3f1c9a2e-5d0b-4c7e-9a6f-2b8d1e4c7a%02d", f.seq)
	f.machines[id] = &machine{id: id, name: name}
	g.approved, g.machineID = true, id
	return id
}

// Deny is the person refusing on the consent page.
func (f *Fake) Deny(userCode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if g := f.byUser[strings.ToUpper(strings.TrimSpace(userCode))]; g != nil {
		g.denied = true
	}
}

// Revoke ends a machine: its refresh token is refused from here on.
func (f *Fake) Revoke(machineID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m := f.machines[machineID]; m != nil {
		m.revoked = true
	}
}

// MachineName is what the machine is called in the console.
func (f *Fake) MachineName(machineID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m := f.machines[machineID]; m != nil {
		return m.name
	}
	return ""
}

func (f *Fake) discovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                        f.Server.URL,
		"device_authorization_endpoint": f.Server.URL + "/oauth/device",
		"token_endpoint":                f.Server.URL + "/oauth/token",
	})
}

func (f *Fake) device(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		oauthErr(w, http.StatusMethodNotAllowed, "invalid_request", "POST only")
		return
	}
	_ = r.ParseForm()
	if r.Form.Get("client_id") != f.ClientID {
		oauthErr(w, http.StatusBadRequest, "invalid_client", "unknown client")
		return
	}
	f.mu.Lock()
	f.seq++
	g := &grant{
		deviceCode: fmt.Sprintf("dev-%d.secret", f.seq),
		userCode:   fmt.Sprintf("WXYZ-%04d", f.seq),
		name:       r.Form.Get("name"),
		expiresAt:  time.Now().Add(10 * time.Minute),
	}
	f.grants[g.deviceCode] = g
	f.byUser[g.userCode] = g
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"device_code": g.deviceCode, "user_code": g.userCode,
		"verification_uri":          f.Server.URL + "/oauth/device",
		"verification_uri_complete": f.Server.URL + "/oauth/device?user_code=" + g.userCode,
		"expires_in":                600, "interval": 1,
	})
}

func (f *Fake) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		oauthErr(w, http.StatusMethodNotAllowed, "invalid_request", "POST only")
		return
	}
	_ = r.ParseForm()
	switch r.Form.Get("grant_type") {
	case "urn:ietf:params:oauth:grant-type:device_code":
		f.deviceGrant(w, r)
	case "refresh_token":
		f.refreshGrant(w, r)
	default:
		oauthErr(w, http.StatusBadRequest, "unsupported_grant_type", r.Form.Get("grant_type"))
	}
}

func (f *Fake) deviceGrant(w http.ResponseWriter, r *http.Request) {
	if r.Form.Get("client_id") != f.ClientID {
		oauthErr(w, http.StatusBadRequest, "invalid_client", "unknown client")
		return
	}
	f.mu.Lock()
	f.polls++
	poll := f.polls
	g := f.grants[r.Form.Get("device_code")]
	switch {
	case g == nil || g.consumed:
		f.mu.Unlock()
		oauthErr(w, http.StatusBadRequest, "invalid_grant", "unknown device code")
		return
	case g.denied:
		f.mu.Unlock()
		oauthErr(w, http.StatusBadRequest, "access_denied", "the person refused")
		return
	case time.Now().After(g.expiresAt):
		f.mu.Unlock()
		oauthErr(w, http.StatusBadRequest, "expired_token", "the code expired")
		return
	case f.SlowDown && poll == 1:
		f.mu.Unlock()
		oauthErr(w, http.StatusBadRequest, "slow_down", "poll less often")
		return
	case !g.approved || poll <= f.PendingPolls:
		f.mu.Unlock()
		oauthErr(w, http.StatusBadRequest, "authorization_pending", "not yet approved")
		return
	}
	g.consumed = true
	m := f.machines[g.machineID]
	access, refresh := f.mintLocked(m)
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": access, "refresh_token": refresh, "token_type": "Bearer",
		"expires_in": int64(f.AccessTTL / time.Second),
	})
}

func (f *Fake) refreshGrant(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	id, ok := f.refresh[r.Form.Get("refresh_token")]
	if !ok {
		f.mu.Unlock()
		oauthErr(w, http.StatusBadRequest, "invalid_grant", "unknown refresh token")
		return
	}
	m := f.machines[id]
	if m == nil || m.revoked {
		f.mu.Unlock()
		oauthErr(w, http.StatusBadRequest, "invalid_grant", "the machine was revoked")
		return
	}
	delete(f.refresh, r.Form.Get("refresh_token")) // Latchkey rotates on every use
	access, refresh := f.mintLocked(m)
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": access, "refresh_token": refresh, "token_type": "Bearer",
		"expires_in": int64(f.AccessTTL / time.Second),
	})
}

// mintLocked signs a machine token and a fresh refresh token. Caller
// holds the lock.
func (f *Fake) mintLocked(m *machine) (access, refresh string) {
	f.seq++
	now := time.Now()
	claims := map[string]any{
		"iss": f.Server.URL, "sub": "machine:" + m.id, "aud": "latchkey",
		"role": "machine", "org": f.Org, "name": m.name,
		"ns":       map[string]string{f.Org: "admin"},
		"operator": map[string]string{"sub": f.Operator},
		"jti":      fmt.Sprintf("c0ffee%02d-0000-4000-8000-000000000000", f.seq),
		"iat":      now.Unix(), "exp": now.Add(f.AccessTTL).Unix(),
	}
	payload, _ := json.Marshal(claims)
	access = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." +
		base64.RawURLEncoding.EncodeToString(payload) + ".sig"
	refresh = fmt.Sprintf("%s.refresh-%d", m.id, f.seq)
	f.refresh[refresh] = m.id
	f.Issued = append(f.Issued, access)
	return access, refresh
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func oauthErr(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}
