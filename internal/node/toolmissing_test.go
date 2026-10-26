package node

import (
	"context"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/validate"
)

// Goal a-node-low-on-disk-claims-nothing-and-a-missing-tool-is-named,
// item 1: a tool_missing step's failure names its command and the
// missing tool, not just its exit code (batch 17, anythink node.log
// 2026/10/15 17:53 local, `tool_missing: validation failed: step X
// exited 127`). validationFailure.Error() and validate.Reason carry
// `tool_missing`, the missing tool read from the step's own "not found"
// and the step's command.
func TestToolMissingStepNamesTheCommand(t *testing.T) {
	r, _, _, f, item, _ := validatingRunner(t, oneStepYML("mesh-no-such-tool --version"), map[string]string{"api/kind.go": "package api\n"})
	if err := r.RunItem(context.Background(), item); err == nil {
		t.Fatal("want the step's failure")
	}
	fails := f.CallsNamed("failWorkItem")
	if len(fails) != 1 {
		t.Fatalf("failWorkItem calls = %d", len(fails))
	}
	msg, _ := fails[0].Input["error"].(string)
	if !strings.HasPrefix(msg, "tool_missing: ") {
		t.Fatalf("fail reason = %q, want it to open tool_missing:", msg)
	}
	if !strings.Contains(msg, "mesh-no-such-tool") {
		t.Fatalf("fail reason = %q, want the missing tool named", msg)
	}
	if !strings.Contains(msg, "mesh-no-such-tool --version") {
		t.Fatalf("fail reason = %q, want the step's command named", msg)
	}

	vf := &validationFailure{Step: validate.StepResult{
		Name: "check", Command: "mesh-no-such-tool --version", ExitCode: 127,
		EnvironmentKind: validate.ToolMissing, Output: "sh: 1: mesh-no-such-tool: not found",
	}}
	if err := vf.Error(); !strings.Contains(err, "mesh-no-such-tool") || !strings.Contains(err, "mesh-no-such-tool --version") {
		t.Fatalf("validationFailure.Error() = %q, want the tool and the command", err)
	}
	if reason := validate.Reason(vf); !strings.HasPrefix(reason, "tool_missing: ") || !strings.Contains(reason, "mesh-no-such-tool") {
		t.Fatalf("validate.Reason = %q, want tool_missing and the tool named", reason)
	}
}

// A tool_missing step whose output names nothing a shell says for a
// missing command falls back to naming just the step's command.
func TestToolMissingWithNoMissingToolInOutputNamesTheCommand(t *testing.T) {
	vf := &validationFailure{Step: validate.StepResult{
		Name: "check", Command: "make check", ExitCode: 127,
		EnvironmentKind: validate.ToolMissing, Output: "make: *** No rule to make target.",
	}}
	want := "validation failed: step check exited 127: `make check`"
	if got := vf.Error(); got != want {
		t.Fatalf("validationFailure.Error() = %q, want %q", got, want)
	}
}

// A code failure (exit 1) keeps its wording: no command or tool named,
// no kind.
func TestToolMissingCodeFailureUnchanged(t *testing.T) {
	r, _, _, f, item, _ := validatingRunner(t, oneStepYML("exit 1"), map[string]string{"api/kind.go": "package api\n"})
	if err := r.RunItem(context.Background(), item); err == nil {
		t.Fatal("want the step's failure")
	}
	fails := f.CallsNamed("failWorkItem")
	if len(fails) != 1 {
		t.Fatalf("failWorkItem calls = %d", len(fails))
	}
	msg, _ := fails[0].Input["error"].(string)
	if want := "validation failed: step check exited 1"; msg != want {
		t.Fatalf("fail reason = %q, want %q", msg, want)
	}
}

// A merge batch candidate whose step exits 127 logs "… not attested"
// and returns an error naming the command: the same validationFailure
// the build's failItem path uses, through validate.Reason.
func TestToolMissingBatchCandidateNotAttested(t *testing.T) {
	r, f, batch, _, logs := candidateRunner(t, oneStepYML("mesh-no-such-tool --version"))
	rt := &sandbox.Fake{Agent: func(_ context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		_, _ = spec.Stdout.Write([]byte("sh: 1: mesh-no-such-tool: not found\n"))
		return sandbox.Exit{Code: 127}, nil
	}}
	r.Sandbox = &sandbox.Sandbox{Runtime: rt, Config: sandbox.Config{Image: "ghcr.io/runsheet/mesh-node:dev"}, TempDir: t.TempDir()}
	r.Inspect = func(context.Context, string, ...string) ([]byte, error) { return []byte("sha256:feedface\n"), nil }

	_, err := r.validateCandidate(onBatchToken(r), batch, nil)
	if err == nil {
		t.Fatal("want the step's failure")
	}
	if k := validate.Kinded(err); k != validate.ToolMissing {
		t.Fatalf("kind = %q, want tool_missing", k)
	}
	if !strings.Contains(err.Error(), "mesh-no-such-tool --version") {
		t.Fatalf("err = %q, want the command named", err)
	}
	if n := len(f.CallsNamed("recordValidationAttestation")); n != 0 {
		t.Fatalf("attestations = %d, want none", n)
	}
	if !strings.Contains(logs.String(), "not attested") {
		t.Fatalf("log = %q, want a not attested line", logs.String())
	}
	if !strings.Contains(logs.String(), "tool_missing: validation failed: step check exited 127: command not found: mesh-no-such-tool") {
		t.Fatalf("log = %q, want the kinded reason naming the tool", logs.String())
	}
}
