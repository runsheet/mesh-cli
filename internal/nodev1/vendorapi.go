package nodev1

import (
	"fmt"
	"strings"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/relaysidecar"
)

// A lease's vendor API calls — a pull request, a comment, a merge — go
// through its relay (Mesh v1 spec, "every write through the relay";
// wave 2.7): the session calls /latchkey/<vendor>/<path> on its lease's
// socket, named by RelaySocketEnv and the prompt, and the sessionproxy
// forwards the call to Latchkey's proxy on the lease's session token.
// The node holds each call to the brief's access.api rules before it
// leaves (apiAllows), as Latchkey does again on its side.

// RelaySocketEnv names the lease's relay socket in the session's
// environment when its brief allows vendor API calls: a path, never a
// credential — the socket answers only this lease's calls.
const RelaySocketEnv = "MESH_RELAY_SOCKET"

// apiRules are the brief's access.api rules.
func apiRules(b *meshapi.Brief) []meshapi.APIAccess {
	if b == nil || b.Access == nil {
		return nil
	}
	return b.Access.API
}

// apiAllows is the brief's access.api check (relaysidecar.APIAllows,
// which the lease's relay sidecar runs too).
func apiAllows(rules []meshapi.APIAccess) func(vendor, method, path string) bool {
	return relaysidecar.APIAllows(rules)
}

// apiPathMatch is Latchkey's ProxyPolicy path matcher
// (relaysidecar.APIPathMatch).
func apiPathMatch(pattern, path string) bool { return relaysidecar.APIPathMatch(pattern, path) }

// promptWithRelay is the lease's prompt, and — when its brief allows
// vendor API calls — how the session makes them.
func promptWithRelay(b *meshapi.Brief, sock string, resumed *resumePoint) string {
	p := leasePrompt(b, resumed)
	rules := apiRules(b)
	if len(rules) == 0 || sock == "" {
		return p
	}
	var sb strings.Builder
	sb.WriteString(p)
	sb.WriteString("\nVendor API calls (pull requests, comments, merges, statuses) go only through the node's relay: HTTP to ")
	fmt.Fprintf(&sb, "/latchkey/<vendor>/<api path> on the unix socket $%s (for example `curl --unix-socket \"$%s\" http://relay/latchkey/%s/<path>`). ", RelaySocketEnv, RelaySocketEnv, rules[0].Vendor)
	sb.WriteString("The relay adds the credential; send none. This job may call:\n")
	for _, r := range rules {
		fmt.Fprintf(&sb, "- %s: %s %s\n", r.Vendor, strings.Join(r.Methods, ", "), strings.Join(r.Paths, ", "))
	}
	return sb.String()
}
