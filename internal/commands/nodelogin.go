package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/auth"
	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/latchkey"
	"projectmesh.io/mesh-cli/internal/node"
)

// `mesh node login` with no argument: Latchkey's device flow
// (adr-machine-identity, rollout phase 2). The machine is registered by
// the person who signs in — the operator — and this machine keeps its
// tokens in ~/.mesh/token. `mesh node login <rs_token>` stays as the
// fallback until the console stops minting console tokens.

var (
	latchkeyFlag       string
	latchkeyClientFlag string
	machineNameFlag    string
)

// latchkeyIssuer is --latchkey, else MESH_LATCHKEY, else node.yaml's,
// else Latchkey.
func latchkeyIssuer() string {
	if v := strings.TrimSpace(latchkeyFlag); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("MESH_LATCHKEY")); v != "" {
		return v
	}
	if cfg, err := node.LoadSettings(); err == nil && cfg.Latchkey != "" {
		return cfg.Latchkey
	}
	return latchkey.DefaultIssuer
}

// latchkeyClientID is --client-id, else MESH_LATCHKEY_CLIENT_ID, else
// node.yaml's. There is no default: the public client the flow starts
// from is registered per estate (runsheet's `mesh-cli` client, tagged to
// the org with device_enabled), and a guessed id would only fail later.
func latchkeyClientID() string {
	if v := strings.TrimSpace(latchkeyClientFlag); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("MESH_LATCHKEY_CLIENT_ID")); v != "" {
		return v
	}
	if cfg, err := node.LoadSettings(); err == nil {
		return strings.TrimSpace(cfg.LatchkeyClient)
	}
	return ""
}

// machineName is what the machine is called in the Latchkey console:
// --name, else the node's registered name, else the hostname.
func machineName() string {
	if v := strings.TrimSpace(machineNameFlag); v != "" {
		return v
	}
	if cfg, err := node.LoadSettings(); err == nil && cfg.Name != "" {
		return cfg.Name
	}
	host, err := os.Hostname()
	if err != nil {
		return ""
	}
	return host
}

// deviceLogin runs the device flow and saves the machine credential.
func deviceLogin(ctx context.Context) error {
	clientID := latchkeyClientID()
	if clientID == "" {
		return errors.New("no Latchkey client id: pass --client-id (the org's public mesh-cli client) or set MESH_LATCHKEY_CLIENT_ID; `mesh node login <rs_token>` still takes a console token")
	}
	lk := &latchkey.Client{Issuer: latchkeyIssuer(), ClientID: clientID}
	ep, err := lk.Discover(ctx)
	if err != nil {
		return err
	}
	da, err := lk.StartDevice(ctx, ep, machineName())
	if err != nil {
		return err
	}
	fmt.Printf("Open %s and enter the code:\n\n    %s\n\n", da.VerificationURI, da.UserCode)
	if !noBrowserFlag {
		if err := openBrowser(da.URL()); err != nil {
			fmt.Printf("(could not open a browser: %v)\n", err)
		}
	}
	fmt.Println("Waiting for you to sign in and approve…")
	toks, err := lk.Await(ctx, ep, da, func(s string) { fmt.Println(s) })
	if err != nil {
		return err
	}

	sess := &auth.Session{
		Kind:          auth.MachineKind,
		BaseURL:       ep.Issuer,
		TokenEndpoint: ep.Token,
		ClientID:      clientID,
		CreatedAt:     time.Now().UTC(),
	}
	applyTokens(sess, toks)
	claims, _ := latchkey.ParseClaims(sess.AccessToken)
	if claims != nil {
		sess.MachineID = claims.MachineID()
		sess.UserID = claims.Operator.Sub
		if claims.Org != "" {
			sess.DefaultOrg = claims.Org
		}
	}
	if err := auth.SaveMachine(sess); err != nil {
		return err
	}
	describeMachine(claims)
	rememberLatchkey(ep.Issuer, clientID)
	return greetGateway(ctx, sess)
}

// describeMachine says what was registered, from the token's own claims.
func describeMachine(claims *latchkey.Claims) {
	switch {
	case claims == nil:
		fmt.Println("Machine credential saved to ~/.mesh/token.")
	case claims.MachineID() == "":
		fmt.Printf("Credential saved to ~/.mesh/token (subject %s).\n", claims.Subject)
	default:
		name := claims.Name
		if name == "" {
			name = "this machine"
		}
		fmt.Printf("Machine %q (%s) registered in %s for operator %s. Saved to ~/.mesh/token.\n",
			name, claims.MachineID(), claims.Org, claims.Operator.Sub)
	}
}

// rememberLatchkey keeps the issuer and client in node.yaml, so a later
// `mesh node login` needs no flags — before the machine is registered
// too, since the credential is what `mesh node register` itself runs on.
func rememberLatchkey(issuer, clientID string) {
	if err := node.SaveLatchkey(issuer, clientID); err != nil {
		fmt.Printf("warning: could not record the Latchkey client in node.yaml: %v\n", err)
	}
}

// greetGateway tells the operator whether the platform already takes the
// machine token. It is not an error if it does not: the gateway learns
// the machine principal in its own rollout phase, and the rs_* token
// keeps working until it does.
func greetGateway(ctx context.Context, sess *auth.Session) error {
	gw := gateway.New(gatewayURL(), sess.AccessToken)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	id, err := gw.SignIn(ctx)
	if err != nil {
		fmt.Printf("The gateway at %s does not accept the machine token yet (%v).\nUntil it does, `mesh auth token rs_…` keeps the node running.\n", gw.BaseURL, err)
		return nil
	}
	orgs, _ := gw.MyOrgs(ctx)
	logins := make([]string, 0, len(orgs))
	for _, o := range orgs {
		logins = append(logins, o.Name())
	}
	fmt.Printf("The gateway knows you as %s; orgs: %s\n", id, strings.Join(logins, ", "))
	return nil
}

// applyTokens copies a token response onto the credential. The expiry is
// the token's own `exp` when it carries one — Latchkey's word on when it
// dies — else what expires_in said.
func applyTokens(sess *auth.Session, toks *latchkey.Tokens) {
	sess.AccessToken = toks.AccessToken
	if toks.RefreshToken != "" {
		sess.RefreshToken = toks.RefreshToken
	}
	if toks.TokenType != "" {
		sess.TokenType = toks.TokenType
	}
	var expires time.Time
	if claims, err := latchkey.ParseClaims(toks.AccessToken); err == nil {
		expires = claims.ExpiresAt()
	}
	if expires.IsZero() && toks.ExpiresIn > 0 {
		expires = time.Now().Add(time.Duration(toks.ExpiresIn) * time.Second)
	}
	if expires.IsZero() {
		sess.ExpiresAt = nil
		return
	}
	expires = expires.UTC()
	sess.ExpiresAt = &expires
}

// refreshMachine is auth.Source's refresh hook: Latchkey's refresh
// grant, which rotates the refresh token on every use.
func refreshMachine(ctx context.Context, sess *auth.Session) (*auth.Session, error) {
	lk := &latchkey.Client{Issuer: sess.BaseURL, ClientID: sess.ClientID}
	toks, err := lk.Refresh(ctx, sess.TokenEndpoint, sess.RefreshToken)
	if err != nil {
		return nil, err
	}
	next := *sess
	applyTokens(&next, toks)
	return &next, nil
}
