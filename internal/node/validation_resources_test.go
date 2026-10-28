package node

import (
	"context"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/validate"
)

// sandbox.max_cpus left out is the host's CPU count and max_memory
// bounds nothing; both are read from node.yaml, and a negative max_cpus
// is refused.
func TestSandboxMaximaFromNodeYAML(t *testing.T) {
	s := sandboxOf(t, "slug: n\n").Settings("v1")
	if s.MaxCPUs != float64(runtime.NumCPU()) || s.MaxMemory != "" {
		t.Fatalf("maxima = %g / %q, want %d cpus and no memory bound", s.MaxCPUs, s.MaxMemory, runtime.NumCPU())
	}
	s = sandboxOf(t, "slug: n\nsandbox:\n  max_cpus: 24\n  max_memory: 48g\n").Settings("v1")
	if s.MaxCPUs != 24 || s.MaxMemory != "48g" {
		t.Fatalf("maxima = %g / %q, want 24 and 48g", s.MaxCPUs, s.MaxMemory)
	}
	if err := (SandboxConfig{MaxCPUs: -1}).Validate(); err == nil || !strings.Contains(err.Error(), "max_cpus") {
		t.Fatalf("negative max_cpus: %v", err)
	}
	if (SandboxConfig{MaxCPUs: 4}).IsZero() {
		t.Fatal("a block with max_cpus alone is not zero")
	}
}

// A validation's sharded suite asking for its own cpus and memory runs
// every shard's container with them, each in its own lane's pod, the
// node clamping the cpus to sandbox.max_cpus; a suite asking for
// nothing runs with the node's sandbox.cpus and memory, as before.
func TestASuitesCPUsAndMemoryReachEveryShardsContainer(t *testing.T) {
	rt := &sandbox.Fake{}
	lr, _ := laneRunnerOn(t, rt, servicesYML)
	steps := []validate.Step{
		{Name: "lint", Kind: validate.KindLint, Command: "true", Timeout: time.Minute},
		{Name: "integration", Kind: validate.KindIntegration, Command: "go test ./e2e", Timeout: time.Minute, Shards: 3, CPUs: 16, Memory: "24g"},
	}
	if _, err := validate.Run(context.Background(), steps, lr, validate.Options{Dir: lr.dir}); err != nil {
		t.Fatal(err)
	}
	// sandbox.max_cpus is this host's CPU count: 16 on a host with fewer
	// is clamped to it
	wantCPUs := strconv.FormatFloat(min(16, float64(runtime.NumCPU())), 'f', -1, 64)
	var shards, lint int
	for _, a := range rt.Agents {
		cmd := strings.Join(a.Command, " ")
		args := sandbox.AgentRunArgs(sandbox.Pod{Name: "p"}, a)
		switch {
		case strings.Contains(cmd, "go test ./e2e"):
			shards++
			if flagValue(args, "--cpus") != wantCPUs || flagValue(args, "--cgroup-conf") != "memory.high="+bytesOf(t, "24g") {
				t.Errorf("shard container %s args %v, want --cpus %s and memory.high at 24g", a.Name, args, wantCPUs)
			}
		case strings.HasSuffix(cmd, "true"):
			lint++
			if flagValue(args, "--cpus") != "2" || flagValue(args, "--cgroup-conf") != "memory.high="+bytesOf(t, "6g") {
				t.Errorf("lint container args %v, want the node's 2 cpus and 6g", args)
			}
		}
	}
	if shards != 3 || lint != 1 {
		t.Fatalf("ran %d shard and %d lint containers, want 3 and 1", shards, lint)
	}
}
