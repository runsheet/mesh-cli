package release

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestTargetsHasLinuxAmd64(t *testing.T) {
	want := Target{GOOS: "linux", GOARCH: "amd64", Name: "mesh-linux-amd64"}
	for _, got := range Targets() {
		if got == want {
			return
		}
	}
	t.Fatalf("Targets() = %v, want it to contain %v", Targets(), want)
}

func TestTargetsAreExactlyTheThreeAssets(t *testing.T) {
	want := []Target{
		{GOOS: "linux", GOARCH: "amd64", Name: "mesh-linux-amd64"},
		{GOOS: "linux", GOARCH: "arm64", Name: "mesh-linux-arm64"},
		{GOOS: "darwin", GOARCH: "arm64", Name: "mesh-darwin-arm64"},
	}
	got := Targets()
	if len(got) != len(want) {
		t.Fatalf("Targets() = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Targets()[%d] = %v, want %v (Targets() = %v)", i, got[i], want[i], got)
		}
	}
}

func TestBuildPlanBuildsEachTargetAsReleaseTagDoes(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	dir := t.TempDir()
	plan := BuildPlan("v1.2.3", sha, dir)
	ldflags := "-s -w -X projectmesh.io/mesh-cli/internal/version.Version=v1.2.3 -X projectmesh.io/mesh-cli/internal/version.Sha=0123456"
	want := []struct{ goos, goarch, out string }{
		{"linux", "amd64", filepath.Join(dir, "mesh-linux-amd64")},
		{"linux", "arm64", filepath.Join(dir, "mesh-linux-arm64")},
		{"darwin", "arm64", filepath.Join(dir, "mesh-darwin-arm64")},
	}
	if len(plan) != len(want) {
		t.Fatalf("BuildPlan gave %d invocations, want %d: %v", len(plan), len(want), plan)
	}
	for i, w := range want {
		inv := plan[i]
		for _, e := range []string{"GOOS=" + w.goos, "GOARCH=" + w.goarch, "CGO_ENABLED=0"} {
			if !slices.Contains(inv.Env, e) {
				t.Errorf("invocation %d env %v lacks %s", i, inv.Env, e)
			}
		}
		if !slices.Contains(inv.Args, "-trimpath") {
			t.Errorf("invocation %d args %v lack -trimpath", i, inv.Args)
		}
		if got := flagValue(inv.Args, "-ldflags"); got != ldflags {
			t.Errorf("invocation %d -ldflags = %q, want %q", i, got, ldflags)
		}
		if got := flagValue(inv.Args, "-o"); got != w.out {
			t.Errorf("invocation %d -o = %q, want %q", i, got, w.out)
		}
		wantArgs := []string{"go", "build", "-trimpath", "-ldflags", ldflags, "-o", w.out, "./cmd/mesh/"}
		if !slices.Equal(inv.Args, wantArgs) {
			t.Errorf("invocation %d args = %q, want %q", i, inv.Args, wantArgs)
		}
	}
}

func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
