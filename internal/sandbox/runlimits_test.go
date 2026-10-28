package sandbox

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// runWith runs one command asking for want in a sandbox bounded by
// maxCPUs and maxMemory, and answers its container's spec and what the
// sandbox logged.
func runWith(t *testing.T, want Limits, maxCPUs float64, maxMemory string) (AgentSpec, []string) {
	t.Helper()
	f := &Fake{}
	sb := testSandbox(t, f)
	sb.Config.MaxCPUs, sb.Config.MaxMemory = maxCPUs, maxMemory
	var logs []string
	sb.Logf = func(format string, args ...any) {
		// only the limits' lines: a workspace with no .git says so too
		if line := fmt.Sprintf(format, args...); strings.Contains(line, "sandbox.max_") {
			logs = append(logs, line)
		}
	}
	sess, err := sb.Open("ship-it/2", "validate", GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.Run(context.Background(), RunSpec{Workspace: t.TempDir(), Command: []string{"true"}, Limits: want}); err != nil {
		t.Fatal(err)
	}
	if len(f.Agents) != 1 {
		t.Fatalf("agents: %+v", f.Agents)
	}
	return f.Agents[0], logs
}

// A run asking for nothing of its own runs with the sandbox's limits, as
// before.
func TestRunWithNoLimitsOfItsOwnIsTheSandboxs(t *testing.T) {
	spec, logs := runWith(t, Limits{}, 4, "8g")
	if spec.Limits != (Limits{CPUs: DefaultCPUs, Memory: DefaultMemory, Pids: DefaultPids}) {
		t.Fatalf("limits = %+v, want the sandbox's", spec.Limits)
	}
	args := AgentRunArgs(Pod{Name: "p"}, spec)
	if flagValue(args, "--cpus") != "2" {
		t.Fatalf("args %v, want --cpus 2", args)
	}
	if len(logs) != 0 {
		t.Fatalf("logged %v", logs)
	}
}

// A run's own cpus and memory replace the sandbox's in its container's
// podman args, its pids the sandbox's still, and its tools are told the
// cpus.
func TestRunWithLimitsOfItsOwn(t *testing.T) {
	spec, logs := runWith(t, Limits{CPUs: 16, Memory: "24g"}, 96, "")
	if spec.Limits != (Limits{CPUs: 16, Memory: "24g", Pids: DefaultPids}) {
		t.Fatalf("limits = %+v, want 16 cpus, 24g and the sandbox's pids", spec.Limits)
	}
	args := AgentRunArgs(Pod{Name: "p"}, spec)
	if flagValue(args, "--cpus") != "16" || flagValue(args, "--cgroup-conf") != fmt.Sprintf("memory.high=%d", int64(24)<<30) {
		t.Fatalf("args %v, want --cpus 16 and memory.high at 24g", args)
	}
	if !strings.Contains(strings.Join(ContainerEnv(spec.Limits, spec.Env), " "), "GOMAXPROCS=16") {
		t.Fatalf("GOMAXPROCS is not the run's cpus")
	}
	if len(logs) != 0 {
		t.Fatalf("logged %v", logs)
	}
	// only one of the two: the other is the sandbox's
	spec, _ = runWith(t, Limits{CPUs: 8}, 96, "")
	if spec.Limits.CPUs != 8 || spec.Limits.Memory != DefaultMemory {
		t.Fatalf("limits = %+v, want 8 cpus and the sandbox's memory", spec.Limits)
	}
}

// A request past sandbox.max_cpus or max_memory is clamped to it, with
// a log line each.
func TestRunLimitsAreClampedToTheMaxima(t *testing.T) {
	spec, logs := runWith(t, Limits{CPUs: 64, Memory: "64g"}, 12, "32g")
	if spec.Limits.CPUs != 12 || spec.Limits.Memory != "32g" {
		t.Fatalf("limits = %+v, want 12 cpus and 32g", spec.Limits)
	}
	if len(logs) != 2 || !strings.Contains(logs[0], "64 cpus") || !strings.Contains(logs[0], "clamped to 12") ||
		!strings.Contains(logs[1], "memory 64g") || !strings.Contains(logs[1], "clamped to 32g") {
		t.Fatalf("logs %q, want one line per clamp", logs)
	}
	// within the maxima: as asked, nothing said
	spec, logs = runWith(t, Limits{CPUs: 12, Memory: "16384m"}, 12, "32g")
	if spec.Limits.CPUs != 12 || spec.Limits.Memory != "16384m" || len(logs) != 0 {
		t.Fatalf("limits = %+v logs %q, want as asked and no line", spec.Limits, logs)
	}
}

// flagValue is the last value of flag in args, "" when absent.
func flagValue(args []string, flag string) string {
	vs := flagValues(args, flag)
	if len(vs) == 0 {
		return ""
	}
	return vs[len(vs)-1]
}
