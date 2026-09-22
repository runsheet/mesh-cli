package commands

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/auth"
	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/node"
)

// The platform side of the CLI: one gateway, one credential — the node's
// Latchkey machine token, or the rs_* console token it falls back to
// (mesh-specs adr-002, latchkey adr-machine-identity).

var gatewayFlag string

// gatewayURL is --gateway, else MESH_GATEWAY, else node.yaml's, else the default.
func gatewayURL() string {
	if v := strings.TrimSpace(gatewayFlag); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("MESH_GATEWAY")); v != "" {
		return v
	}
	if cfg, err := node.LoadConfig(); err == nil && cfg.Gateway != "" {
		return cfg.Gateway
	}
	return gateway.DefaultURL
}

// platformClient is the gateway client for the saved credential: a
// Latchkey machine credential (refreshed before expiry) or an rs_*
// console token.
func platformClient() (*gateway.Client, error) {
	src, err := tokenSource()
	if err != nil {
		return nil, err
	}
	return gateway.NewWithSource(gatewayURL(), src), nil
}

// tokenSource loads ~/.mesh/token and wires Latchkey's refresh grant to it.
func tokenSource() (*auth.Source, error) {
	cred, err := auth.LoadCredential()
	if err != nil {
		return nil, err
	}
	return auth.NewSource(cred, refreshMachine), nil
}

// credentialLine is the one line `mesh node status` opens with: which of
// the two credentials this machine runs on. While both shapes are live
// (latchkey adr-machine-identity, rollout phases 1–3) an operator
// looking at a node needs to know which one it holds — and, for a
// machine, what to revoke in the Latchkey console.
func credentialLine(cred *auth.Credential) string {
	from := "~/.mesh/token"
	if cred != nil && cred.FromEnv {
		from = "MESH_TOKEN"
	}
	if !cred.IsMachine() {
		return fmt.Sprintf("Credential: an rs_* console token (%s) — the operator's own bearer, the fallback. `mesh node login` registers a Latchkey machine instead.", from)
	}
	m := cred.Machine
	who := m.MachineID
	if who == "" {
		who = "unnamed"
	}
	if m.DefaultOrg != "" {
		who += " in " + m.DefaultOrg
	}
	if m.UserID != "" {
		who += ", operator " + m.UserID
	}
	expiry := "no recorded expiry"
	if m.ExpiresAt != nil {
		expiry = "access token until " + m.ExpiresAt.Local().Format(time.RFC3339) + ", refreshed before it expires"
	}
	return fmt.Sprintf("Credential: Latchkey machine %s (%s) — %s.", who, from, expiry)
}

// resolveOrg finds the org by login (--org), else the registered node's.
func resolveOrg(ctx context.Context, gw *gateway.Client, login string) (*gateway.Org, error) {
	login = strings.TrimSpace(login)
	if login == "" {
		cfg, err := node.LoadConfig()
		if err != nil {
			return nil, fmt.Errorf("--org required (no registered node to take it from)")
		}
		switch len(cfg.Orgs) {
		case 1:
			login = cfg.Orgs[0]
		default:
			orgs, err := gw.MyOrgs(ctx)
			if err != nil {
				return nil, err
			}
			if len(orgs) == 1 {
				return &orgs[0], nil
			}
			return nil, fmt.Errorf("--org required (this node serves more than one)")
		}
	}
	return gw.OrgByLogin(ctx, login)
}
