package harness

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// A job's state names its proxy — the socket and the servers, sorted —
// whether its entries are a host's ProxyRelay or a pod's shim, and no
// argument of an entry that is not the proxy's; a job with nothing
// proxied names none.
func TestProxyStateOfNamesSocketAndServers(t *testing.T) {
	sock := "/m/px/1/s"
	for name, servers := range map[string]map[string]any{
		"host": {"runsheet": ProxyRelay("mesh", sock, "runsheet"), "search": ProxyRelay("mesh", sock, "search")},
		"pod":  {"runsheet": ProxiedServer("/h/"+ProxyShimScriptName, sock, "runsheet"), "search": ProxiedServer("/h/"+ProxyShimScriptName, sock, "search")},
	} {
		servers["mesh-node"] = map[string]any{"type": "stdio", "command": "node", "args": []string{"/h/work.js", "--token", "secret-x"}}
		job := Job{Kind: KindBuild, MCPServers: servers}
		st := job.state("detached", sessionunit.Handle{PID: 1}, "claude-code")
		if st.Proxy == nil || st.Proxy.Socket != sock || !slices.Equal(st.Proxy.Servers, []string{"runsheet", "search"}) {
			t.Errorf("%s: state proxy = %+v", name, st.Proxy)
		}
		b, _ := json.Marshal(st)
		if strings.Contains(string(b), "secret-x") {
			t.Errorf("%s: state holds an entry's argument: %s", name, b)
		}
	}
	if p := ProxyStateOf(map[string]any{"runsheet": MeshContextRelay("mesh", "https://g/mcp", "/t/runsheet.token")}); p != nil {
		t.Errorf("an unproxied relay named a proxy: %+v", p)
	}
}

// An adopted proxied session keeps no token file: the proxy the adopting
// node restarts holds its bearer.
func TestProxyAdoptedKeepsNoTokenFile(t *testing.T) {
	k := &countingKeeper{}
	release := keepAdoptedToken(k, nil, "claude-code", sessionunit.State{TokenFile: "/t/runsheet.token", Proxy: &sessionunit.Proxy{Socket: "/m/s", Servers: []string{"runsheet"}}})
	release(true)
	if k.n != 0 {
		t.Errorf("kept %d token files for a proxied session", k.n)
	}
}

type countingKeeper struct{ n int }

func (k *countingKeeper) Keep(string) (func(bool), error) { k.n++; return func(bool) {}, nil }
