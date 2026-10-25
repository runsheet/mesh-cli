// Package contracts holds runsheet's published contract fixtures as
// scripts/contracts-pull.sh copied them (runsheet specs/contracts/*.json,
// runsheet ADR 026 and ADR 069), and PULLED, the ref and commit they came
// from. Never edit a fixture by hand: pull again.
package contracts

import "embed"

// FS is every pulled fixture and PULLED.
//
//go:embed *.json PULLED
var FS embed.FS
