package nodev1

import (
	"slices"
	"strings"

	"projectmesh.io/mesh-cli/internal/egress"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// A kind's egress (projectmesh-io/api ADR 001 amendment, tool slots and
// egress; mesh-specs gap 11). A brief with no egress is served as before.
// A brief with egress makes the lease's relay its session's only exit,
// and the relay's allowlist exactly that egress, narrowed by the tenant's
// policy (none exists yet: tenantHosts) and never widened:
//
//   - hosts: HTTPS by CONNECT to host:443 through the lease's tunnel
//     (internal/egress), the host matched exactly and named again by the
//     TLS server name, dialled from the node on a public address. In a
//     pod, the session's HTTPS_PROXY names the relay through the pod's
//     bridge, and the pod's network is relay-only
//     (sandbox.NetworkPolicy.RelayOnly: no port out, no DNS), so a
//     client that ignores the proxy finds no route at all;
//   - mcp: the product MCP route, served only for a product the egress
//     (or the brief's access) names;
//   - model: read, never enforced here: the harness's own /model/ route
//     stays open whatever it says, since every job runs a harness.
//
// `{}` is nothing beyond the git route and the product MCP the brief's
// access names. A session on the host (an unsandboxed node) is given the
// same relay routes but nothing confines it: the node says so in its log.

// tenantHosts are the tenant's own egress hosts, which narrow the kind's:
// nil, since Mesh has no tenant egress policy yet. Here is where one is
// read once it does; it can only remove hosts.
func tenantHosts(*meshapi.Brief) []string { return nil }

// egressTunnel is the lease's tunnel under its egress, nil when the kind
// declares none.
func (r *Runtime) egressTunnel(b *meshapi.Brief, ic *interception) *egress.Tunnel {
	if b.Egress == nil {
		return nil
	}
	// each request is reported (security.go)
	report := r.egressReporter(b.LeaseID)
	t := &egress.Tunnel{
		Policy: egress.NewPolicy(b.Egress.Hosts, tenantHosts(b), nil),
		Dial:   r.EgressDial,
		Logf:   r.logf,
		Audit:  r.tunnelAudit(b.LeaseID, "egress", report),
		Done:   report,
	}
	// the hosts the lease's rules terminate (intercept.go)
	ic.install(r, b.LeaseID, t)
	return t
}

// mcpAllowed says the session may reach server, one of the brief's
// product servers: always without egress; with it, when its egress or
// its access names the product (or the server by name).
func mcpAllowed(b *meshapi.Brief, server string) bool {
	if b.Egress == nil {
		return true
	}
	names := slices.Clone(b.Egress.MCP)
	if b.Access != nil {
		names = append(names, b.Access.MCP...)
	}
	return slices.Contains(names, b.Product) || slices.Contains(names, server)
}

// proxyEnv points every HTTP client in a lease's pod at its relay, base
// (the bridge at the pod's loopback), and keeps the pod's own loopback —
// the bridge's git and model routes, its services — off the proxy.
func proxyEnv(base string) []string {
	noProxy := "127.0.0.1,localhost,::1"
	return []string{
		"HTTPS_PROXY=" + base, "https_proxy=" + base,
		"HTTP_PROXY=" + base, "http_proxy=" + base,
		"NO_PROXY=" + noProxy, "no_proxy=" + noProxy,
		// Node's own fetch reads the variables only when asked to
		"NODE_USE_ENV_PROXY=1",
		// the harness tries nothing beyond its model and tools
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	}
}

// egressLine is a lease's egress for the log.
func egressLine(e *meshapi.Egress) string {
	hosts := "no hosts"
	if len(e.Hosts) > 0 {
		hosts = strings.Join(e.Hosts, ", ")
	}
	line := "hosts " + hosts + "; mcp " + strings.Join(e.MCP, ", ") + map[bool]string{true: "; model", false: "; no model"}[e.Model]
	if len(e.Registries) > 0 {
		line += "; signs " + strings.Join(e.Registries, ", ")
	}
	return line
}
