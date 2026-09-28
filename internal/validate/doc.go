// Package validate runs a repository's declared validation — its lint,
// typecheck, unit, build and integration steps — on the commit a build
// session finishes with, and produces the attestation the platform signs
// (runsheet ADR 032, goal node-attests-its-validation).
//
// As of 2026-09-28 neither ADR 032 nor the goal ci-uploads-test-evidence
// is indexed on mesh-context, so this package is built to the shape the
// item's brief describes rather than the ADR's own text; a later item
// should reconcile the two once the document lands.
//
// A repository opts in with a `.runsheet/validate.yml` at its root:
//
//	steps:
//	  - name: vet
//	    command: go vet ./...
//	    always: true
//	  - name: unit
//	    command: go test -json ./...
//	    paths: ["**/*.go", "go.mod", "go.sum"]
//	    timeout: 15m
//	    results: stdout
//
// Load reads it (ErrNoValidation when the file is absent, not a failure
// the caller need treat as an error); Select picks the steps a diff
// touches plus every step marked always; Run executes the selection
// through a Runner (HostRunner ships one that runs `sh -c` in the
// workspace); Build turns the results into an Attestation with every
// field the platform expects.
package validate
