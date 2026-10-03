package node

import (
	"reflect"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/validate"
)

// gatewayAttestation turns validate's attestation into the platform's
// input field for field: one command per step, the tests summed, the
// validate.yml's hash as the suite hash, each artifact's digest, and a
// report for each step that parsed one — none for one that did not.
func TestGatewayAttestationIsThePlatformsShape(t *testing.T) {
	at := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	got := gatewayAttestation(validate.Attestation{
		Commit: "c0ffee", Tree: "7ree", RunnerImage: "host:mini", Sandboxed: false,
		NodeID: "node-1", SessionID: "s-1", ItemID: "item-1", At: at,
		Validation: validate.ValidationFile{File: ".runsheet/validate.yml", Hash: "vhash"},
		Steps: []validate.StepResult{
			{Name: "vet", Command: "go vet ./...", ExitCode: 0, DurationMs: 900},
			{Name: "unit", Command: "go test -json ./...", ExitCode: 1, DurationMs: 61000,
				TestsRun: 12, TestsPassed: 11, TestsFailed: 1, Report: `{"Action":"fail"}`,
				Artifacts: []validate.Artifact{{Path: "cover.out", SHA256: "beef"}}},
			{Name: "e2e", Command: "go test ./e2e", ExitCode: 0, DurationMs: 5, TestsRun: 3, TestsPassed: 3},
		},
	})
	want := gateway.ValidationAttestation{
		Commit: "c0ffee", TreeHash: "7ree", RunnerImage: "host:mini", SuiteHash: "vhash", SessionID: "s-1",
		TestsRun: 15, TestsPassed: 14, TestsFailed: 1, ArtifactDigests: []string{"cover.out@sha256:beef"}, At: at,
		Commands: []gateway.ValidationCommand{
			{Suite: "vet", Command: "go vet ./...", ExitCode: 0, DurationMs: 900},
			{Suite: "unit", Command: "go test -json ./...", ExitCode: 1, DurationMs: 61000},
			{Suite: "e2e", Command: "go test ./e2e", ExitCode: 0, DurationMs: 5},
		},
		Reports: []gateway.ValidationReport{{Suite: "unit", Content: `{"Action":"fail"}`}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}
