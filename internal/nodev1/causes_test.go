package nodev1

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/nodetools"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// The node says when a failure is the infrastructure's with a hand-back
// cause Mesh v1 takes, never text a product must match (mesh-specs
// gap 14).

// A git token Latchkey will not mint fails the checkout before the
// session starts: the lease is handed back credential_timeout and no
// session runs.
func TestAGitTokenNotMintedIsCredentialTimeout(t *testing.T) {
	sess := &stubSession{}
	g := newRig(t, sess)
	g.latchkey.refuseSource = "the installation is suspended"
	job := g.offer(t, nil)
	run(t, g, context.Background())
	hb := onlyHandBack(t, g, job)
	if hb.Cause != meshapi.CauseCredentialTimeout || !strings.Contains(hb.Reason, "not minted") || hb.ExitCode != 0 {
		t.Fatalf("hand-back %+v", hb)
	}
	if len(sess.specs) != 0 {
		t.Fatal("a session ran with no git token")
	}
}

// A node tool the kind names that this node does not have is handed back
// tool_unavailable before anything is minted or any session runs.
func TestANodeToolTheNodeLacksIsToolUnavailable(t *testing.T) {
	sess := &stubSession{}
	g := newRig(t, sess)
	job := g.offer(t, func(b *meshapi.Brief) { b.NodeTools = []string{nodetools.RunSuiteTool, "warp_drive"} })
	run(t, g, context.Background())
	hb := onlyHandBack(t, g, job)
	if hb.Cause != meshapi.CauseToolUnavailable || !strings.Contains(hb.Reason, "warp_drive") {
		t.Fatalf("hand-back %+v", hb)
	}
	if len(sess.specs) != 0 {
		t.Fatal("a session ran without its node tool")
	}
	if minted, _, _ := g.latchkey.snapshot(); len(minted) != 0 {
		t.Fatalf("tokens minted for a lease that could not run: %v", minted)
	}
}

// suiteLease offers a job whose kind names run_suite and that has a pod
// for its steps, on a sandboxed actor node whose pod steps exit as step
// says.
func suiteLease(t *testing.T, step func(context.Context, sandbox.AgentSpec) (sandbox.Exit, error)) (*rig, string, *stubSession, *string) {
	t.Helper()
	var text string
	var refused bool
	sess := suiteSession(t, t.TempDir()+"/marker", &text, &refused)
	g, sb := actorRig(t, sess)
	g.rt.PodImage = "mesh-pod:test"
	sb.Agent = step
	job := g.offer(t, func(b *meshapi.Brief) {
		b.NodeTools = []string{nodetools.RunSuiteTool}
		b.Pods = []json.RawMessage{json.RawMessage(`{"name":"db","image":"postgres:17"}`)}
	})
	run(t, g, context.Background())
	return g, job, sess, &text
}

// A run_suite step killed by SIGKILL or SIGTERM, not by the node, is
// the host's: the lease is handed back pod_killed carrying the step's
// exit code, never failed or finished as the work's red.
func TestARunSuiteStepKilledIsPodKilledWithItsExitCode(t *testing.T) {
	for _, code := range []int{137, 143} {
		g, job, _, text := suiteLease(t, func(context.Context, sandbox.AgentSpec) (sandbox.Exit, error) {
			return sandbox.Exit{Code: code}, nil
		})
		hb := onlyHandBack(t, g, job)
		if hb.Cause != meshapi.CausePodKilled || hb.ExitCode != code || !strings.Contains(hb.Reason, "step unit exited") {
			t.Fatalf("exit %d: hand-back %+v (run_suite answered %q)", code, hb, *text)
		}
	}
}

// A step that fails on its own (exit 1) is the work's: the session hears
// it red and the lease ends as the session does (this one never
// submits, so no_finish), never handed back as the infrastructure's.
func TestARunSuiteStepThatFailsIsTheWorks(t *testing.T) {
	g, job, _, text := suiteLease(t, func(context.Context, sandbox.AgentSpec) (sandbox.Exit, error) {
		return sandbox.Exit{Code: 1}, nil
	})
	if hb := g.fake.Calls("hand-back"); len(hb) != 0 {
		t.Fatalf("a red step handed the lease back: %+v", hb)
	}
	if *text == "" || g.fake.Ended(g.fake.LeaseOf(job)) == "hand-back" {
		t.Fatalf("run_suite answered %q; the lease ended %q", *text, g.fake.Ended(g.fake.LeaseOf(job)))
	}
}

// run_suite's runner failing under the session — the container runtime
// not answering — is tool_unavailable.
func TestARunSuiteRunnerFailingIsToolUnavailable(t *testing.T) {
	g, job, _, _ := suiteLease(t, func(context.Context, sandbox.AgentSpec) (sandbox.Exit, error) {
		return sandbox.Exit{}, errors.New("podman is not answering")
	})
	hb := onlyHandBack(t, g, job)
	if hb.Cause != meshapi.CauseToolUnavailable || !strings.Contains(hb.Reason, "podman is not answering") {
		t.Fatalf("hand-back %+v", hb)
	}
}
