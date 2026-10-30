// Package nodetools is the Mesh v1 node's own tools (Mesh v1 spec, "The
// mesh-cli node" and "Evidence signing"): run_suite, apply_onto and
// push, served in process on a lease's sessionproxy. Each is
// deterministic and records its own evidence: when it has run, it
// writes a ToolRecord — the tool, a digest of what it was given, when it
// ran, its exit, a summary and the digest of its output — as an in-toto
// statement, signs it (sign.go) inside the node process, uploads the
// output blob, and posts the record to Mesh, all before the model sees
// its answer. The model never produces evidence: it passes on the
// evidence ids the tools answer.
package nodetools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// StatementType is an in-toto v1 statement's _type, and PayloadType the
// DSSE payload type of one.
const (
	StatementType = "https://in-toto.io/Statement/v1"
	PayloadType   = "application/vnd.in-toto+json"
)

// Lease is what a tool's record names of its lease.
type Lease struct {
	JobID   string
	LeaseID string
	LeaseN  int
	Product string
	Tenant  string
	NodeID  string
	// EvidenceRequired are the tools whose records gate the job's
	// finish (the kind's evidence_required): their signing waits out a
	// Fulcio or Rekor that cannot be reached before it falls back.
	EvidenceRequired []string
}

// Record is one tool run, as its statement's predicate carries it.
type Record struct {
	Tool         string    `json:"tool"`
	JobID        string    `json:"job_id"`
	LeaseID      string    `json:"lease_id"`
	LeaseN       int       `json:"lease_n"`
	Product      string    `json:"product"`
	Tenant       string    `json:"tenant"`
	NodeID       string    `json:"node_id"`
	InputDigest  string    `json:"input_digest"`
	OutputDigest string    `json:"output_digest,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	EndedAt      time.Time `json:"ended_at"`
	Exit         int       `json:"exit"`
	Summary      string    `json:"summary"`
	// Selection is the part of the suite a run_suite ran when its call
	// named one (gap 27): absent for a whole run, never empty.
	Selection *Selection `json:"selection,omitempty"`
}

type subject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

type statement struct {
	Type          string    `json:"_type"`
	Subject       []subject `json:"subject"`
	PredicateType string    `json:"predicateType"`
	Predicate     Record    `json:"predicate"`
}

// Statement is the record's in-toto statement: its subjects the input
// and, when there is one, the output, by SHA-256; its predicate the
// record (meshapi.ToolPredicateType).
func Statement(r Record) ([]byte, error) {
	r.StartedAt, r.EndedAt = r.StartedAt.UTC(), r.EndedAt.UTC()
	subs := []subject{{Name: "input", Digest: map[string]string{"sha256": hexOf(r.InputDigest)}}}
	if r.OutputDigest != "" {
		subs = append(subs, subject{Name: "output", Digest: map[string]string{"sha256": hexOf(r.OutputDigest)}})
	}
	return json.Marshal(statement{Type: StatementType, Subject: subs, PredicateType: meshapi.ToolPredicateType, Predicate: r})
}

// Digest is "sha256:" and the hex SHA-256 of b: a blob's digest, and a
// statement's evidence id.
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// InputDigest is the digest of a tool's input, as JSON.
func InputDigest(v any) string {
	b, _ := json.Marshal(v)
	return Digest(b)
}

func hexOf(digest string) string {
	if len(digest) > 7 && digest[:7] == "sha256:" {
		return digest[7:]
	}
	return digest
}
