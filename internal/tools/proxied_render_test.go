package tools_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	. "projectmesh.io/mesh-cli/internal/tools"
)

// shimFor is a stand-in for harness.ProxiedServer / the host's `mesh
// mcp-relay --socket/--server`: every server, remote, local or the
// built-in, gets the same shape of entry, naming the socket and the
// server alone.
func shimFor(name string) map[string]any {
	return map[string]any{"type": "stdio", "command": "node", "args": []string{"/shim.js", "/tmp/proxy.sock", name}}
}

func TestProxiedServersRendersShimsForEveryToolAndTheBuiltin(t *testing.T) {
	servers, upstreams, err := ProxiedServers("https://api.runsheet.dev/mcp", sample(), shimFor)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"runsheet", "search", "fs-tools"} {
		s, ok := servers[n].(map[string]any)
		if !ok {
			t.Fatalf("servers lack %s: %v", n, servers)
		}
		if s["command"] != "node" {
			t.Fatalf("%s: %v", n, s)
		}
		args, _ := s["args"].([]string)
		if len(args) != 3 || args[2] != n {
			t.Fatalf("%s args = %v, want the server named last", n, args)
		}
	}
	if upstreams["runsheet"].URL != "https://api.runsheet.dev/mcp" {
		t.Fatalf("runsheet upstream = %+v", upstreams["runsheet"])
	}

	search := upstreams["search"]
	if search.Transport != Remote || search.URL != "https://search.example/mcp" {
		t.Fatalf("search upstream = %+v", search)
	}
	wantHeaders := map[string]string{
		"X-Region":      "au",
		"Authorization": "Bearer " + bearerSecret,
		"X-Api-Key":     headerSecret,
	}
	if !reflect.DeepEqual(search.Headers, wantHeaders) {
		t.Fatalf("search headers = %v", search.Headers)
	}

	fsTools := upstreams["fs-tools"]
	if fsTools.Transport != Local || fsTools.Command != Npx || !reflect.DeepEqual(fsTools.Args, []string{"-y", "@example/fs-mcp", "--root", "."}) {
		t.Fatalf("fs-tools upstream = %+v", fsTools)
	}
	if !reflect.DeepEqual(fsTools.Env, map[string]string{"LOG_LEVEL": "warn"}) ||
		!reflect.DeepEqual(fsTools.Secrets, map[string]string{"FS_API_KEY": envSecret}) {
		t.Fatalf("fs-tools env/secrets = %+v / %+v", fsTools.Env, fsTools.Secrets)
	}
}

// TestProxiedServersSecretsOnlyInUpstreams is the proxied analogue of
// TestSecretsOnlyInEnv: a tool set carrying secrets renders a servers
// map with none of them (no MESH_TOOL_* entry even exists in proxied
// mode, since a shim's argv is the socket and the server name alone),
// while every secret is in the upstream specs the proxy is reached on.
func TestProxiedServersSecretsOnlyInUpstreams(t *testing.T) {
	secrets := []string{bearerSecret, headerSecret, envSecret}
	servers, upstreams, err := ProxiedServers("https://api.runsheet.dev/mcp", sample(), shimFor)
	if err != nil {
		t.Fatal(err)
	}
	js, _ := json.Marshal(servers)
	for _, s := range secrets {
		if strings.Contains(string(js), s) {
			t.Errorf("secret %s in servers JSON: %s", s, js)
		}
	}
	us, _ := json.Marshal(upstreams)
	for _, s := range secrets {
		if !strings.Contains(string(us), s) {
			t.Errorf("secret %s missing from the upstream specs", s)
		}
	}
}

func TestProxiedServersRefusesClashWithBuiltin(t *testing.T) {
	_, _, err := ProxiedServers("https://api.runsheet.dev/mcp", []Tool{{Name: "runsheet", Transport: Remote, URL: "https://x"}}, shimFor)
	if err == nil || !strings.Contains(err.Error(), "built-in") {
		t.Fatalf("err = %v", err)
	}
}
