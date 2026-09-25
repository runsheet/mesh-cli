// Package nodeimage carries the node image's Containerfile in the binary,
// so `mesh node update` can build the image where it cannot pull it
// (mesh-specs ADR 007).
package nodeimage

import _ "embed"

// Containerfile is images/node/Containerfile.
//
//go:embed Containerfile
var Containerfile []byte
