package relaysidecar

import (
	"net/url"
	"strings"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// APIAllows is the brief's access.api check: some rule names the vendor
// (any case), lists the method (any case) and has a path pattern that
// matches (APIPathMatch, Latchkey's ProxyPathMatch).
func APIAllows(rules []meshapi.APIAccess) func(vendor, method, path string) bool {
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
				if APIPathMatch(p, path) {
					return true
				}
			}
		}
		return false
	}
}

// APIPathMatch is Latchkey's ProxyPolicy path matcher (identity
// aggregates ProxyPathMatch, copied): slash-separated segments, each a
// literal (any case), `{name}` for exactly one segment, or a final `*`
// for one or more. A path with an empty, dot or encoded-slash segment
// matches nothing.
func APIPathMatch(pattern, path string) bool {
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
