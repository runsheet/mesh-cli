package main

// This file is a thin compatibility shim. Node token I/O and registration
// helpers now live in internal/auth (I/O) and internal/node (HTTP logic).
// The wrappers below let cmd/mesh/ continue to use the old names without
// change until Phase 8.
//
// TODO(spec22-phase8): inline all call sites to internal/auth and internal/node.

import (
	"time"

	"projectmesh.io/mesh-cli/internal/auth"
	"projectmesh.io/mesh-cli/internal/node"
)

func LoadNodeToken() (string, error)   { return auth.LoadNodeToken() }
func SaveNodeToken(token string) error { return auth.SaveNodeToken(token) }
func ClearNodeToken() error            { return auth.ClearNodeToken() }

func RequestNodeRegistrationCode(baseURL string) (string, error) {
	return node.RequestNodeRegistrationCode(baseURL)
}

func PollAndSaveNodeToken(baseURL, code string, timeout time.Duration) (string, error) {
	return node.PollAndSaveNodeToken(baseURL, code, timeout)
}
