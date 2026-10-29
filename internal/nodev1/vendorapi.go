package nodev1

import (
	"fmt"
	"net/url"
	"strings"

	"projectmesh.io/mesh-cli/internal/meshapi"
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

// apiAllows is the brief's access.api check: some rule names the vendor
// (any case), lists the method (any case) and has a path pattern that
// matches (apiPathMatch, Latchkey's ProxyPathMatch).
func apiAllows(rules []meshapi.APIAccess) func(vendor, method, path string) bool {
	return func(vendor, method, path string) bool {
		for _, rule := range rules {
			if !strings.EqualFold(rule.Vendor, vendor) {
				continue
			}
			listed := false
			for _, m := range rule.Methods {
				if strings.EqualFold(m, method) {
					listed = true
				}
			}
			if !listed {
				continue
			}
			for _, p := range rule.Paths {
				if apiPathMatch(p, path) {
					return true
				}
			}
		}
		return false
	}
}

// apiPathMatch is Latchkey's ProxyPolicy path matcher (identity
// aggregates ProxyPathMatch, copied): slash-separated segments, each a
// literal (any case), `{name}` for exactly one segment, or a final `*`
// for one or more. A path with an empty, dot or encoded-slash segment
// matches nothing.
func apiPathMatch(pattern, path string) bool {
	ps := strings.Split(strings.TrimPrefix(strings.TrimSpace(pattern), "/"), "/")
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for _, s := range segs {
		d, err := url.PathUnescape(s)
		if err != nil || d == "" || d == "." || d == ".." || strings.Contains(d, "/") {
			return false
		}
	}
	for i, p := range ps {
		if p == "*" {
			return len(segs) > i
		}
		if i >= len(segs) {
			return false
		}
		if strings.HasPrefix(p, "{") {
			continue
		}
		if !strings.EqualFold(p, segs[i]) {
			return false
		}
	}
	return len(segs) == len(ps)
}

// promptWithRelay is the lease's prompt, and — when its brief allows
// vendor API calls — how the session makes them.
func promptWithRelay(b *meshapi.Brief, sock string) string {
	p := Prompt(b)
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
