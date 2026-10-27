package nodecaps

import (
	"context"
	"errors"
	"runtime"
	"testing"
)

// TestProbeReportsWhatItFinds: the manifest is the node's os and arch,
// and each toolchain on PATH with the first dotted version its command
// prints; a tool not on PATH, or one that fails to run, is left out.
func TestProbeReportsWhatItFinds(t *testing.T) {
	on := map[string]string{
		"go":         "go1.26.10\n",
		"node":       "v22.11.0\n",
		"java":       "openjdk version \"21.0.4\" 2024-07-16\n",
		"xcodebuild": "Xcode 16.2\nBuild version 16C5032a\n",
		"podman":     "", // fails to run
	}
	look := func(bin string) (string, error) {
		if _, ok := on[bin]; ok {
			return "/usr/bin/" + bin, nil
		}
		return "", errors.New("not found")
	}
	run := func(_ context.Context, path string, _ ...string) (string, error) {
		bin := path[len("/usr/bin/"):]
		if bin == "podman" {
			return "", errors.New("exit status 125")
		}
		return on[bin], nil
	}
	m := Probe(context.Background(), look, run)
	if m.OS != runtime.GOOS || m.Arch != runtime.GOARCH {
		t.Fatalf("os/arch: %+v", m)
	}
	want := []Tool{{"go", "1.26.10"}, {"java", "21.0.4"}, {"node", "22.11.0"}, {"xcode", "16.2"}}
	if len(m.Tools) != len(want) {
		t.Fatalf("tools: %+v, want %+v", m.Tools, want)
	}
	for i := range want {
		if m.Tools[i] != want[i] {
			t.Fatalf("tool %d: %+v, want %+v", i, m.Tools[i], want[i])
		}
	}
}

// TestProbeFindsNothingOnABareHost: no toolchain is an empty list, not
// nil, so the platform stores "has none" rather than "never said".
func TestProbeFindsNothingOnABareHost(t *testing.T) {
	m := Probe(context.Background(), func(string) (string, error) { return "", errors.New("no") }, nil)
	if m.Tools == nil || len(m.Tools) != 0 {
		t.Fatalf("tools: %#v", m.Tools)
	}
}
