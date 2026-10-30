package nodev1

import (
	"net/http"
	"net/url"
	"strconv"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/registryrelay"
)

// A kind's artifact registry through the relay (projectmesh-io/api ADR
// 001 amendment, registry through the relay; mesh-specs gap 18). The
// brief names it (`registry: {base_url}`); the lease's relay serves it
// on the pod's loopback, at the relay's own port, and the session gets
// MESH_REGISTRY=127.0.0.1:<port>: /v2/ and /artifacts/, each request
// forwarded on the lease's session token (internal/registryrelay). The
// pod holds no registry credential. The in-process relay serves it from
// the node; a relay sidecar serves it in the pod, reaching the registry
// through the lease's tunnel, its host a fixed upstream. A session on
// the host, with no pod, has no loopback relay and no MESH_REGISTRY.

// registryRelay is the lease's registry on the in-process relay, nil
// when the brief names none. client reaches the registry: the node's
// own when nil (RegistryHTTP in tests).
func (r *Runtime) registryRelay(b *meshapi.Brief, tokens *sessionTokens, client *http.Client) http.Handler {
	if b.Registry == nil || b.Registry.BaseURL == "" || tokens == nil {
		return nil
	}
	if client == nil {
		client = r.RegistryHTTP
	}
	return &registryrelay.Relay{Upstream: b.Registry.BaseURL, HTTP: client, Token: tokens.Token, Key: tokens.Key(), Logf: r.logf}
}

// registryEnv is MESH_REGISTRY for a pod whose relay listens on port,
// nil when the brief names no registry.
func registryEnv(b *meshapi.Brief, port int) []string {
	if b.Registry == nil || b.Registry.BaseURL == "" {
		return nil
	}
	return []string{registryrelay.Env + "=127.0.0.1:" + strconv.Itoa(port)}
}

// registryHost is the registry's host:port, a fixed upstream of a relay
// sidecar's tunnel; "" when the brief names none.
func registryHost(b *meshapi.Brief) string {
	if b.Registry == nil || b.Registry.BaseURL == "" {
		return ""
	}
	u, err := url.Parse(b.Registry.BaseURL)
	if err != nil {
		return ""
	}
	return u.Host
}
