package release

import "testing"

func TestTargetsHasLinuxAmd64(t *testing.T) {
	want := Target{GOOS: "linux", GOARCH: "amd64", Name: "mesh-linux-amd64"}
	for _, got := range Targets() {
		if got == want {
			return
		}
	}
	t.Fatalf("Targets() = %v, want it to contain %v", Targets(), want)
}
