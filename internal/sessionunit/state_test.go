package sessionunit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A sandboxed session's state names its pod, container and HOME, and a
// host session's carries none of them.
func TestStateNamesASandboxedSessionsPod(t *testing.T) {
	dir := t.TempDir()
	st := State{NodeID: "n", Kind: "build", Runner: SandboxRunner, Unit: "mesh-p-agent", Pod: "mesh-p", Container: "mesh-p-agent", Home: "/tmp/mesh-home-1", TempDirs: []string{"/tmp/cfg"}}
	if err := WriteState(dir, st); err != nil {
		t.Fatal(err)
	}
	got, err := ReadState(dir)
	if err != nil || got.Pod != st.Pod || got.Container != st.Container || got.Home != st.Home || got.Runner != "podman" || len(got.TempDirs) != 1 {
		t.Fatalf("read %+v, %v", got, err)
	}
	if err := WriteState(dir, State{Kind: "build", Runner: "detached", PID: 7}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, StateFile))
	for _, k := range []string{`"pod"`, `"container"`, `"home"`} {
		if strings.Contains(string(b), k) {
			t.Fatalf("a host session's state carries %s: %s", k, b)
		}
	}
}

// A proxied session's state names its proxy's socket and servers, and
// reads them back; a session with none writes no proxy at all.
func TestProxyStateRoundTrips(t *testing.T) {
	dir := t.TempDir()
	in := State{Kind: "build", Runner: "detached", Proxy: &Proxy{Socket: "/m/px/1/s", Servers: []string{"runsheet", "search"}}}
	if err := WriteState(dir, in); err != nil {
		t.Fatal(err)
	}
	got, err := ReadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Proxy == nil || got.Proxy.Socket != in.Proxy.Socket || strings.Join(got.Proxy.Servers, ",") != "runsheet,search" {
		t.Fatalf("proxy = %+v", got.Proxy)
	}
	if err := WriteState(dir, State{Kind: "build", Runner: "detached"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, StateFile))
	if strings.Contains(string(b), "proxy") {
		t.Errorf("an unproxied state names a proxy:\n%s", b)
	}
}
