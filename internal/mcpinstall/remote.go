package mcpinstall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// BearerEnvVar is the environment variable a bearer remote entry reads
// its token from — the same MESH_TOKEN the mesh CLI itself prefers over
// ~/.mesh/token. The config file holds its name, never its value
// (ADR-006: secrets in the environment only).
const BearerEnvVar = "MESH_TOKEN"

// RemoteAuth is how a remote entry signs in: OAuth, the client
// discovering the issuer from the URL and registering itself, or Bearer,
// a header read from an environment variable.
type RemoteAuth interface {
	// Describe is the install's `auth:` line.
	Describe() string
	remoteAuth()
}

// OAuth is an issuer offering dynamic client registration: the entry
// carries the URL alone.
type OAuth struct{ Issuer string }

func (a OAuth) Describe() string { return "oauth (dynamic registration at " + a.Issuer + ")" }
func (OAuth) remoteAuth()        {}

// Bearer is a token read from EnvVar into the Authorization header.
type Bearer struct{ EnvVar string }

func (a Bearer) Describe() string { return "bearer from $" + a.EnvVar }
func (Bearer) remoteAuth()        {}

// remoteEntry is a JSON remote server: Claude Code's
// {"type": "http", "url", "headers"}, Cursor's {"url", "headers"}.
type remoteEntry struct {
	Type    string            `json:"type,omitempty"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// RemoteEntry is the client's entry for the hosted MCP server at url in
// its format (ADR-025, Remote): a Bearer names its variable — Codex's
// bearer_token_env_var, a JSON client's Authorization header in its own
// EnvRef form — and an OAuth entry is the URL alone, each client
// discovering the issuer itself (Codex's `auth` defaults to oauth).
func RemoteEntry(c Client, url string, auth RemoteAuth) ([]byte, error) {
	bearer, isBearer := auth.(Bearer)
	if isBearer && bearer.EnvVar == "" {
		return nil, errors.New("a bearer entry needs an environment variable")
	}
	switch c.Format {
	case TOML:
		body := "url = " + TOMLString(url) + "\n"
		if isBearer {
			body += "bearer_token_env_var = " + TOMLString(bearer.EnvVar) + "\n"
		}
		return []byte(body), nil
	default:
		e := remoteEntry{Type: c.RemoteType, URL: url}
		if isBearer {
			if c.EnvRef == nil {
				return nil, fmt.Errorf("%s: no environment reference form for a bearer header", c.Name)
			}
			e.Headers = map[string]string{"Authorization": "Bearer " + c.EnvRef(bearer.EnvVar)}
		}
		return json.Marshal(e)
	}
}

// discoverTimeout bounds the whole discovery.
const discoverTimeout = 10 * time.Second

// DiscoverAuth asks the MCP server at mcpURL how it is signed into: its
// protected-resource metadata (RFC 9728) names the authorization
// server, whose metadata (RFC 8414, else OpenID Connect discovery)
// answers OAuth when it has a registration_endpoint (RFC 7591; Latchkey's
// goal an-mcp-client-signs-in-with-one-click). Anything else — no
// metadata, no issuer, no registration, a network error — is Bearer from
// BearerEnvVar.
func DiscoverAuth(ctx context.Context, mcpURL string) RemoteAuth {
	ctx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()
	if issuer, ok := registeringIssuer(ctx, mcpURL); ok {
		return OAuth{Issuer: issuer}
	}
	return Bearer{EnvVar: BearerEnvVar}
}

func registeringIssuer(ctx context.Context, mcpURL string) (string, bool) {
	var resource struct {
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if !getFirst(ctx, wellKnown(mcpURL, "oauth-protected-resource"), &resource) {
		return "", false
	}
	for _, issuer := range resource.AuthorizationServers {
		var meta struct {
			RegistrationEndpoint string `json:"registration_endpoint"`
		}
		urls := append(wellKnown(issuer, "oauth-authorization-server"), wellKnown(issuer, "openid-configuration")...)
		urls = append(urls, strings.TrimRight(issuer, "/")+"/.well-known/openid-configuration")
		if getFirst(ctx, urls, &meta) && meta.RegistrationEndpoint != "" {
			return issuer, true
		}
	}
	return "", false
}

// wellKnown is the metadata URLs for u: the suffix inserted before u's
// path (RFC 8414 §3, RFC 9728 §3), then at the host's root.
func wellKnown(u, suffix string) []string {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return nil
	}
	path := strings.TrimRight(p.Path, "/")
	p.RawQuery, p.Fragment, p.RawPath = "", "", ""
	var out []string
	if path != "" {
		p.Path = "/.well-known/" + suffix + path
		out = append(out, p.String())
	}
	p.Path = "/.well-known/" + suffix
	return append(out, p.String())
}

// getFirst decodes the first of urls that answers 200 with JSON into v.
func getFirst(ctx context.Context, urls []string, v any) bool {
	for _, u := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Accept", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			continue
		}
		if json.Unmarshal(body, v) == nil {
			return true
		}
	}
	return false
}
