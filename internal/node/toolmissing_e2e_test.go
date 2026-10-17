package node

import (
	"context"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/validate"
)

// Goal a-node-low-on-disk-claims-nothing-and-a-missing-tool-is-named,
// item 3 (docs/adr/adr-030, Amendment 2026-10-16): a validate.yml step
// whose command is not installed exits 127, and what the node sends the
// platform names `tool_missing`, the missing tool and the step's
// command — never only `step X exited 127` (batch 17, anythink node.log
// 2026/10/15 17:53 local).

const missingTool = "mesh-no-such-tool"

func TestToolMissingE2E(t *testing.T) {
	t.Run("batch candidate", toolMissingBatchCandidate)
	t.Run("build", toolMissingBuild)
}

// A merge batch candidate, on the node's own Loop as `mesh node start`
// runs it: the batch is claimed, its step exits 127 in the pod saying
// the shell's "not found", and the failMergeBatchValidation reason the
// fake gateway receives opens `tool_missing: ` and names the tool and
// the command. An environment failure is no verdict on the candidate:
// nothing is attested and the batch is not finished.
func toolMissingBatchCandidate(t *testing.T) {
	cmd := missingTool + " --version"
	r, f, batch, _, logs := candidateRunner(t, oneStepYML(cmd))
	loop, _ := mergeBatchLoop(t, r, f)
	r.Sandbox.Runtime.(*sandbox.Fake).Agent = func(_ context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		_, _ = spec.Stdout.Write([]byte("sh: 1: " + missingTool + ": not found\n"))
		return sandbox.Exit{Code: 127}, nil
	}
	runUntil(t, loop, logs, func() bool { return len(f.CallsNamed("failMergeBatchValidation")) > 0 })

	if n := len(f.CallsNamed("claimMergeBatchValidation")); n == 0 {
		t.Fatalf("the batch was never claimed\n%s", logs.String())
	}
	fails := f.CallsNamed("failMergeBatchValidation")
	reason, _ := fails[0].Input["reason"].(string)
	if validate.KindOfReason(reason) != validate.ToolMissing {
		t.Errorf("reason = %q, want it to open tool_missing:", reason)
	}
	want := "tool_missing: validation failed: step check exited 127: command not found: " + missingTool + " (in `" + cmd + "`)"
	if !strings.Contains(reason, want) {
		t.Errorf("reason = %q, want it to carry %q", reason, want)
	}
	if fails[0].Input["aggregateId"] != batch.MergeBatchID {
		t.Errorf("fail input = %v, want batch %s", fails[0].Input, batch.MergeBatchID)
	}
	if n := len(batchAttestations(f)); n != 0 {
		t.Errorf("%d attestations for a candidate whose tool is missing, want none", n)
	}
	if n := len(f.CallsNamed("finishMergeBatchValidation")); n != 0 {
		t.Errorf("%d finish calls for a candidate whose tool is missing", n)
	}
	if !strings.Contains(logs.String(), want) {
		t.Errorf("node.log lacks %q:\n%s", want, logs.String())
	}
}

// A build whose step exits 127 on the host — a real shell over a real
// checkout, the command really missing: the attestation's command
// carries environmentKind tool_missing and the step's command, and the
// failWorkItem reason opens `tool_missing: ` and names both the tool
// and the command.
func toolMissingBuild(t *testing.T) {
	cmd := missingTool + " --version"
	r, _, _, f, item, logs := validatingRunner(t, oneStepYML(cmd), map[string]string{"api/kind.go": "package api\n"})
	if err := r.RunItem(context.Background(), item); err == nil {
		t.Fatalf("want the step's failure\n%s", logs.String())
	}

	a := uploadedAttestation(t, f)
	commands, _ := a["commands"].([]any)
	if len(commands) != 1 {
		t.Fatalf("attested commands = %v, want one", commands)
	}
	c := commands[0].(map[string]any)
	if c["exitCode"] != float64(127) || c["environmentKind"] != "tool_missing" {
		t.Errorf("attested command = %v, want exit 127 and environmentKind tool_missing", c)
	}
	if c["command"] != cmd {
		t.Errorf("attested command's command = %v, want %q", c["command"], cmd)
	}

	fails := f.CallsNamed("failWorkItem")
	if len(fails) != 1 {
		t.Fatalf("failWorkItem calls = %d, want 1", len(fails))
	}
	msg, _ := fails[0].Input["error"].(string)
	want := "tool_missing: validation failed: step check exited 127: command not found: " + missingTool + " (in `" + cmd + "`)"
	if msg != want {
		t.Errorf("fail reason = %q, want %q", msg, want)
	}
	if n := len(f.CallsNamed("finishWorkItem")); n != 0 {
		t.Errorf("%d finishWorkItem calls for a build whose tool is missing", n)
	}
}
