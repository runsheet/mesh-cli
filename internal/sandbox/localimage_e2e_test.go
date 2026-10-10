//go:build e2e

package sandbox

// This file holds the local-only-image e2e: a session started from an
// image that is only in the local store, under a registry name podman
// cannot resolve, starts without a pull. unresolvableRef names that
// image (item 21); no test is defined yet.
