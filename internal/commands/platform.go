package commands

import (
	"context"
	"fmt"
	"os"
	"strings"

	"projectmesh.io/mesh-cli/internal/auth"
	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/node"
)

// The platform side of the CLI: one gateway, one token (mesh-specs adr-002).

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

// platformClient is the gateway client for the saved token.
func platformClient() (*gateway.Client, error) {
	tok, err := auth.LoadToken()
	if err != nil {
		return nil, err
	}
	return gateway.New(gatewayURL(), tok), nil
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
