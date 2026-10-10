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
