//go:build e2e

package sandbox

import "strings"

// unresolvableRef names image under a registry host that cannot
// resolve, so a podman pull of it fails and only the local store —
// holding image under its real ref — can serve a podman run against
// it.
func unresolvableRef(image string) string {
	path := image
	if i := strings.Index(image, "/"); i >= 0 {
		host := image[:i]
		if host == "localhost" || strings.ContainsAny(host, ".:") {
			path = image[i+1:]
		}
	}
	return "mesh-local.invalid/" + path
}
