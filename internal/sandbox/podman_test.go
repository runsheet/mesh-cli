package sandbox

import (
	"slices"
	"strings"
	"testing"
)

// LimitEnv ceils a fractional CPU limit, floors at 1, and is empty for
// no limit, so Go, OpenMP and make fan out to the container's share of
// the host rather than its full core count.
func TestLimitEnv(t *testing.T) {
	for _, tc := range []struct {
		name string
		l    Limits
		want []string
	}{
		{"two cpus", Limits{CPUs: 2}, []string{"GOMAXPROCS=2", "OMP_NUM_THREADS=2", "MAKEFLAGS=-j2"}},
		{"fractional rounds up", Limits{CPUs: 1.5}, []string{"GOMAXPROCS=2", "OMP_NUM_THREADS=2", "MAKEFLAGS=-j2"}},
		{"no limit", Limits{}, nil},
	} {
		if got := LimitEnv(tc.l); !slices.Equal(got, tc.want) {
			t.Errorf("%s: LimitEnv(%+v) = %v, want %v", tc.name, tc.l, got, tc.want)
		}
	}
}

// agentArgs (through AgentRunArgs) and SidecarRunArgs carry the derived
// env as --env NAME=value, not through envArgs' bare form, and keep an
// explicitly set value over the derived one.
func TestLimitEnvInRunArgs(t *testing.T) {
	pod := Pod{Name: "p"}
	spec := AgentSpec{Name: "a", Image: "img", Limits: Limits{CPUs: 2}}
	args := AgentRunArgs(pod, spec)
	for _, want := range []string{"--env", "GOMAXPROCS=2"} {
		if !slices.Contains(args, want) {
			t.Fatalf("AgentRunArgs = %v, missing %q", args, want)
		}
	}
	if !slices.Contains(args, "OMP_NUM_THREADS=2") || !slices.Contains(args, "MAKEFLAGS=-j2") {
		t.Fatalf("AgentRunArgs = %v, missing OMP_NUM_THREADS/MAKEFLAGS", args)
	}

	sargs := SidecarRunArgs(pod, SidecarSpec{Name: "s", Image: "img", Limits: Limits{CPUs: 2}})
	if !strings.Contains(strings.Join(sargs, " "), "--env GOMAXPROCS=2") {
		t.Fatalf("SidecarRunArgs = %v, missing --env GOMAXPROCS=2", sargs)
	}

	// an explicit GOMAXPROCS in the spec's env wins over the derived one:
	// envArgs gives it as a bare `--env GOMAXPROCS`, its value read from
	// podman's own environment (podmanEnv), never the derived --env
	// GOMAXPROCS=2.
	explicit := AgentSpec{Name: "a", Image: "img", Limits: Limits{CPUs: 2}, Env: []string{"GOMAXPROCS=8"}}
	eargs := AgentRunArgs(pod, explicit)
	joined := strings.Join(eargs, " ")
	if strings.Contains(joined, "--env GOMAXPROCS=2") {
		t.Fatalf("args = %s: the derived GOMAXPROCS=2 overrode the explicit one", joined)
	}
	if !strings.Contains(joined, "--env GOMAXPROCS ") && !strings.HasSuffix(joined, "--env GOMAXPROCS") {
		t.Fatalf("args = %s: the explicit GOMAXPROCS is gone", joined)
	}

	sexplicit := SidecarSpec{Name: "s", Image: "img", Limits: Limits{CPUs: 2}, Env: []string{"GOMAXPROCS=8"}}
	sjoined := strings.Join(SidecarRunArgs(pod, sexplicit), " ")
	if strings.Contains(sjoined, "--env GOMAXPROCS=2") {
		t.Fatalf("sidecar args = %s: the derived GOMAXPROCS=2 overrode the explicit one", sjoined)
	}
}
