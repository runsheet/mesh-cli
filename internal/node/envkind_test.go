package node

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/sandbox"
)

// oneStepYML is a validate.yml of one always step running cmd.
func oneStepYML(cmd string) map[string]string {
	return map[string]string{".runsheet/validate.yml": fmt.Sprintf("suites:\n  - name: check\n    kind: unit\n    command: %s\n    always: true\n", cmd)}
}

// Goal a-node-validates-every-enrolled-repo-with-the-tools-it-needs,
// item 4 (docs/adr/adr-030): what the node sends the gateway for a step
// that failed on the node carries the step's environment kind — the
// attestation's command its environmentKind, the failWorkItem reason
// opening `<kind>: ` — for exit 124 (step_timeout), exit 127
// (tool_missing) and podman's 125 on an image it could not pull
// (image_pull_refused); a plain exit 1 is a code failure and carries
// none. The credential refusal's kind rides the release reason
// (TestCredentialReleaseReasonCarriesItsKind).
func TestValidationEnvironmentKindReachesTheGateway(t *testing.T) {
	const pullRefused = "Trying to pull ghcr.io/runsheet/mesh-node:dev...\nError: initializing source docker://ghcr.io/runsheet/mesh-node:dev: reading manifest dev in ghcr.io/runsheet/mesh-node: unauthorized\n"
	for _, tc := range []struct {
		name, cmd string
		pod       bool // the step runs in a fake pod whose image pull is refused
		code      float64
		want      string
	}{
		{"exit 124", "exit 124", false, 124, "step_timeout"},
		{"exit 127", "mesh-no-such-tool --version", false, 127, "tool_missing"},
		{"image pull refused", "true", true, 125, "image_pull_refused"},
		{"exit 1", "exit 1", false, 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, _, f, item, _ := validatingRunner(t, oneStepYML(tc.cmd), map[string]string{"api/kind.go": "package api\n"})
			if tc.pod {
				rt := &sandbox.Fake{Agent: func(_ context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
					_, _ = io.WriteString(spec.Stderr, pullRefused)
					return sandbox.Exit{Code: 125}, nil
				}}
				r.Sandbox = &sandbox.Sandbox{Runtime: rt, Config: sandbox.Config{Image: "ghcr.io/runsheet/mesh-node:dev"}, TempDir: t.TempDir()}
				r.Inspect = func(context.Context, string, ...string) ([]byte, error) { return []byte("sha256:feedface\n"), nil }
			}
			if err := r.RunItem(context.Background(), item); err == nil {
				t.Fatal("want the step's failure")
			}
			a := uploadedAttestation(t, f)
			commands, _ := a["commands"].([]any)
			if len(commands) != 1 {
				t.Fatalf("commands = %v", commands)
			}
			c := commands[0].(map[string]any)
			if c["exitCode"] != tc.code {
				t.Fatalf("exitCode = %v, want %v", c["exitCode"], tc.code)
			}
			kind, has := c["environmentKind"]
			if tc.want == "" && has {
				t.Fatalf("a code failure carries environmentKind %v", kind)
			}
			if tc.want != "" && kind != tc.want {
				t.Fatalf("environmentKind = %v, want %s", kind, tc.want)
			}
			fails := f.CallsNamed("failWorkItem")
			if len(fails) != 1 {
				t.Fatalf("failWorkItem calls = %d", len(fails))
			}
			msg, _ := fails[0].Input["error"].(string)
			want := fmt.Sprintf("validation failed: step check exited %d", int(tc.code))
			if tc.want == "tool_missing" {
				want += ": command not found: mesh-no-such-tool (in `mesh-no-such-tool --version`)"
			}
			if tc.want != "" {
				want = tc.want + ": " + want
			}
			if msg != want {
				t.Fatalf("fail reason = %q, want %q", msg, want)
			}
		})
	}
}

// A workspace GitHub refused on every re-issued token is released with
// its kind first: `credential_refused: github credential: …`.
func TestCredentialReleaseReasonCarriesItsKind(t *testing.T) {
	f := seed(t)
	runRefused(t, f, 4, notFound("ghs_first"))
	rel := f.CallsNamed("releaseWorkItem")
	if len(rel) != 1 {
		t.Fatalf("releases = %+v", rel)
	}
	if reason, _ := rel[0].Input["note"].(string); !strings.HasPrefix(reason, "credential_refused: github credential: acme/widgets") {
		t.Fatalf("release reason = %q", reason)
	}
}
