// Package nodeimage carries the node image's Containerfile in the binary,
// so a node set to `sandbox.image: build` builds its image from the file
// its own release was cut with (mesh-specs ADR 007).
package nodeimage

import _ "embed"

// Containerfile is images/node/Containerfile.
//
//go:embed Containerfile
var Containerfile []byte
