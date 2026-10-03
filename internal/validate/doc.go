// Package validate runs a repository's declared validation — its lint,
// typecheck, unit, build and integration steps — on the commit a build
// session finishes with, and produces the attestation the platform signs
// (runsheet ADR 032, goal node-attests-its-validation).
//
// The file is the platform's (runsheet ADR 032, The file:
// .runsheet/validate.yml; platform/validatespec) plus the node's own
// keys — timeout, always, artifacts, results:
//
//	suites:
//	  - name: vet
//	    kind: lint
//	    command: go vet ./...
//	  - name: unit
//	    kind: unit
//	    command: go test -json ./...
//	    paths: ["**/*.go", "go.mod", "go.sum"]
//	    timeout: 15m
//	    results: stdout
//
// `steps:` is read as a deprecated alias of `suites:`.
//
// Load reads it (ErrNoValidation when the file is absent, not a failure
// the caller need treat as an error); Select picks the suites a diff
// touches, every suite with no paths and every one marked always; Run executes the selection
// through a Runner (HostRunner ships one that runs `sh -c` in the
// workspace); Build turns the results into an Attestation with every
// field the platform expects, the file's SuiteHash among them.
package validate
