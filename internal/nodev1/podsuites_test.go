package nodev1

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/nodetools"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// suiteSession is a session that writes its own .runsheet/validate.yml —
// one step that touches marker, a file on the host — and calls run_suite,
// keeping what the tool answered.
func suiteSession(t *testing.T, marker string, text *string, refused *bool) *stubSession {
	return &stubSession{result: SessionResult{SessionID: "sess-suite", Text: "ran"}, inspect: func(spec SessionSpec) {
		if err := os.MkdirAll(filepath.Join(spec.Workspace, ".runsheet"), 0o755); err != nil {
			t.Error(err)
			return
		}
		yml := "suites:\n  - name: unit\n    kind: unit\n    command: touch " + marker + "\n"
		if err := os.WriteFile(filepath.Join(spec.Workspace, ".runsheet", "validate.yml"), []byte(yml), 0o644); err != nil {
			t.Error(err)
			return
		}
		var err error
		if *text, *refused, err = callTool(context.Background(), spec.Servers[NodeToolsServer], nodetools.RunSuiteTool, map[string]any{}); err != nil {
			t.Errorf("run_suite: %v", err)
		}
	}}
}

// suiteAgents are the containers the fake sandbox ran as run_suite's
// steps.
func suiteAgents(sb *sandbox.Fake) []sandbox.AgentSpec {
	var out []sandbox.AgentSpec
	for _, a := range sb.AgentSpecs() {
		if strings.Contains(a.Name, "-suite-") {
			out = append(out, a)
		}
	}
	return out
}

// On a sandboxed node a lease's run_suite runs the steps of the
// validate.yml its session wrote in the lease's pod — a container
// beside its sidecars, the checkout mounted, none of the node's
// environment — and never on the host, even with HostSuites set: the
// step that would touch a host marker is recorded in the pod instead.
func TestRunSuiteRunsInTheLeasesPodOnASandboxedNode(t *testing.T) {
	t.Setenv("MESH_NODE_ONLY_PROBE", "the-node-s-own")
	marker := filepath.Join(t.TempDir(), "ran-on-the-host")
	var text string
	var refused bool
	g, sb := actorRig(t, suiteSession(t, marker, &text, &refused))
	g.rt.HostSuites = true
	g.rt.PodImage = "mesh-pod:test"
	job := g.offer(t, func(b *meshapi.Brief) {
		b.NodeTools = []string{nodetools.RunSuiteTool}
		b.Pods = []json.RawMessage{json.RawMessage(`{"name":"db","image":"postgres:17"}`)}
		b.Resources = meshapi.Resources{CPUs: 2, MemGB: 4}
	})
	run(t, g, context.Background())
	if refused {
		t.Fatalf("run_suite refused in a lease with a pod: %s", text)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the session's validate.yml step ran on the host")
	}
	steps := suiteAgents(sb)
	if len(steps) != 1 {
		t.Fatalf("the pod ran %d suite steps, want 1: %+v", len(steps), sb.AgentSpecs())
	}
	st := steps[0]
	if !slices.Equal(st.Command, []string{"sh", "-c", "touch " + marker}) || st.Image != "mesh-pod:test" ||
		len(st.Mounts) != 1 || st.Mounts[0].Source != st.Workdir || st.Workdir == "" || st.Limits.CPUs != 2 || st.Limits.Memory != "4096m" {
		t.Fatalf("the suite step %+v", st)
	}
	for _, kv := range st.Env {
		if strings.HasPrefix(kv, "MESH_NODE_ONLY_PROBE=") {
			t.Fatalf("the node's environment reached the pod's step: %v", st.Env)
		}
	}
	if !strings.HasPrefix(st.Name, PodName(g.fake.LeaseOf(job))) {
		t.Fatalf("the step %s is not in the lease's pod", st.Name)
	}
}

// On a sandboxed node a lease with no pod has nowhere to run its suites:
// run_suite refuses, even with HostSuites set, and nothing runs on the
// host or in any pod.
func TestRunSuiteRefusesWithoutAPodOnASandboxedNode(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran-on-the-host")
	var text string
	var refused bool
	g, sb := actorRig(t, suiteSession(t, marker, &text, &refused))
	g.rt.HostSuites = true
	g.offer(t, func(b *meshapi.Brief) { b.NodeTools = []string{nodetools.RunSuiteTool} })
	run(t, g, context.Background())
	if !refused || !strings.Contains(text, "cannot run here") {
		t.Fatalf("run_suite answered refused=%v %q, want a refusal", refused, text)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the session's validate.yml step ran on the host")
	}
	if steps := suiteAgents(sb); len(steps) != 0 {
		t.Fatalf("suite steps ran: %+v", steps)
	}
}

// A node with no sandbox runs run_suite on the host only when explicitly
// unsandboxed (HostSuites); the zero value refuses.
func TestRunSuiteOnTheHostIsAnExplicitOptIn(t *testing.T) {
	for _, c := range []struct {
		name       string
		hostSuites bool
		ran        bool
	}{
		{name: "the zero value refuses", hostSuites: false, ran: false},
		{name: "explicitly unsandboxed runs on the host", hostSuites: true, ran: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "ran-on-the-host")
			var text string
			var refused bool
			g := newRig(t, suiteSession(t, marker, &text, &refused))
			g.rt.HostSuites = c.hostSuites
			g.offer(t, func(b *meshapi.Brief) { b.NodeTools = []string{nodetools.RunSuiteTool} })
			run(t, g, context.Background())
			_, err := os.Stat(marker)
			if ran := err == nil; ran != c.ran || refused == c.ran {
				t.Fatalf("ran=%v refused=%v (%s), want ran=%v", ran, refused, text, c.ran)
			}
		})
	}
}
