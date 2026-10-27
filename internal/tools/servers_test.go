package tools

import "testing"

// WithServers adds the brief's servers beside the built-in, leaving the
// built-in's name, a name a tool already holds, and a nameless or
// URL-less entry alone.
func TestWithServers(t *testing.T) {
	shim := func(name string) map[string]any { return map[string]any{"server": name} }
	servers, ups, err := ProxiedServers("https://platform.test/mcp", nil, shim)
	if err != nil {
		t.Fatal(err)
	}
	ups["taken"] = UpstreamSpec{Transport: Remote, URL: "https://tool.test/mcp"}
	added := WithServers(servers, ups, []Server{
		{Name: "jobs", URL: "https://platform.test/jobs/mcp"},
		{Name: Builtin, URL: "https://elsewhere.test/mcp"},
		{Name: "taken", URL: "https://platform.test/other"},
		{Name: "", URL: "https://platform.test/x"},
		{Name: "nourl"},
	}, shim)
	if len(added) != 1 || added[0] != "jobs" {
		t.Fatalf("added %v, want [jobs]", added)
	}
	if s := ups["jobs"]; s.URL != "https://platform.test/jobs/mcp" || !s.Session {
		t.Fatalf("jobs spec %+v", s)
	}
	if ups[Builtin].URL != "https://platform.test/mcp" || ups["taken"].URL != "https://tool.test/mcp" {
		t.Fatalf("an existing server moved: %v", ups)
	}
	if s, _ := servers["jobs"].(map[string]any); s["server"] != "jobs" {
		t.Fatalf("jobs server %v", servers["jobs"])
	}
}
