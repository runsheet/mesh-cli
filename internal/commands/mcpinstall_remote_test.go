package commands

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"projectmesh.io/mesh-cli/internal/mcpinstall"
)

// mesh mcp install --remote writes each client's remote-server entry for
// the hosted /mcp (ADR-025, Remote): OAuth when the issuer offers
// dynamic registration, else a bearer named by its environment variable
// — never a token in a file (ADR-006).

const (
	envSecret  = "rs_secret-from-the-environment-0123456789"
	fileSecret = "rs_secret-from-the-token-file-9876543210"
)

// issuer serves the MCP server's protected-resource metadata and the
// issuer's authorization-server metadata, with a registration_endpoint
// when registers. It answers 401 at /mcp itself, as a protected server
// does.
func issuer(t *testing.T, registers bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"resource":              srv.URL + "/mcp",
			"authorization_servers": []string{srv.URL},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		meta := map[string]any{
			"issuer":                 srv.URL,
			"authorization_endpoint": srv.URL + "/authorize",
			"token_endpoint":         srv.URL + "/token",
		}
		if registers {
			meta["registration_endpoint"] = srv.URL + "/register"
		}
		json.NewEncoder(w).Encode(meta)
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// remoteHome is installHome with MESH_TOKEN set and ~/.mesh/token
// written, the two secrets no installed file may hold.
func remoteHome(t *testing.T) string {
	t.Helper()
	home := installHome(t)
	t.Setenv("MESH_TOKEN", envSecret)
	if err := os.MkdirAll(filepath.Join(home, ".mesh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".mesh", "token"), []byte(fileSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func installRemote(t *testing.T, url string, dryRun bool, clients ...string) (string, error) {
	t.Helper()
	mcpInstallRemote, mcpInstallURL = true, url
	t.Cleanup(func() { mcpInstallRemote, mcpInstallURL = false, "" })
	return install(t, false, dryRun, clients...)
}

func mustInstallRemote(t *testing.T, url string, clients ...string) string {
	t.Helper()
	out, err := installRemote(t, url, false, clients...)
	if err != nil {
		t.Fatalf("install --remote %v: %v\n%s", clients, err, out)
	}
	return out
}

// noSecrets fails on any file under home, but ~/.mesh/token itself,
// holding either secret.
func noSecrets(t *testing.T, home string) {
	t.Helper()
	token := filepath.Join(home, ".mesh", "token")
	n := 0
	if err := filepath.Walk(home, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || p == token {
			return err
		}
		n++
		b := read(t, p)
		for _, s := range []string{envSecret, fileSecret} {
			if bytes.Contains(b, []byte(s)) {
				t.Errorf("%s holds a token: %q", p, s)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no file written to check")
	}
}

type remoteJSON struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Command string            `json:"command"`
	Headers map[string]string `json:"headers"`
}

func jsonRemote(t *testing.T, path string) remoteJSON {
	t.Helper()
	var doc struct {
		MCPServers map[string]remoteJSON `json:"mcpServers"`
	}
	if err := json.Unmarshal(read(t, path), &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return doc.MCPServers[mcpinstall.ServerName]
}

func tomlRemote(t *testing.T, path string) map[string]any {
	t.Helper()
	var doc struct {
		MCPServers map[string]map[string]any `toml:"mcp_servers"`
	}
	if err := toml.Unmarshal(read(t, path), &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return doc.MCPServers[mcpinstall.ServerName]
}

func remoteFiles(home string) []string {
	return []string{
		filepath.Join(home, ".claude.json"),
		filepath.Join(home, ".claude", "skills", "mesh", "SKILL.md"),
		filepath.Join(home, ".codex", "config.toml"),
		filepath.Join(home, ".cursor", "mcp.json"),
	}
}

func TestMcpInstallRemoteWithoutRegistrationWritesABearerFromTheEnvironment(t *testing.T) {
	home := remoteHome(t)
	srv := issuer(t, false)
	url := srv.URL + "/mcp"
	out := mustInstallRemote(t, url, "claude", "codex", "cursor")
	if !strings.Contains(out, "auth: bearer from $MESH_TOKEN\n") {
		t.Fatalf("no bearer auth line:\n%s", out)
	}

	claude := jsonRemote(t, filepath.Join(home, ".claude.json"))
	if claude.Type != "http" || claude.URL != url || claude.Command != "" ||
		len(claude.Headers) != 1 || claude.Headers["Authorization"] != "Bearer ${MESH_TOKEN}" {
		t.Fatalf("claude entry = %+v", claude)
	}
	cursor := jsonRemote(t, filepath.Join(home, ".cursor", "mcp.json"))
	if cursor.Type != "" || cursor.URL != url || cursor.Command != "" ||
		len(cursor.Headers) != 1 || cursor.Headers["Authorization"] != "Bearer ${env:MESH_TOKEN}" {
		t.Fatalf("cursor entry = %+v", cursor)
	}
	codex := tomlRemote(t, filepath.Join(home, ".codex", "config.toml"))
	if codex["url"] != url || codex["bearer_token_env_var"] != mcpinstall.BearerEnvVar || len(codex) != 2 {
		t.Fatalf("codex entry = %v", codex)
	}
	// the instruction files are written as for a local install
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "mesh", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	noSecrets(t, home)
	remoteAgainIsANoOp(t, home, url)
}

func TestMcpInstallRemoteWithRegistrationWritesTheURLAlone(t *testing.T) {
	home := remoteHome(t)
	srv := issuer(t, true)
	url := srv.URL + "/mcp"
	out := mustInstallRemote(t, url, "claude,codex,cursor")
	if !strings.Contains(out, "auth: oauth (dynamic registration at "+srv.URL+")\n") {
		t.Fatalf("no oauth auth line:\n%s", out)
	}
	for _, f := range []string{filepath.Join(home, ".claude.json"), filepath.Join(home, ".cursor", "mcp.json")} {
		e := jsonRemote(t, f)
		if e.URL != url || e.Headers != nil || e.Command != "" || strings.Contains(string(read(t, f)), "MESH_TOKEN") {
			t.Fatalf("%s: entry = %+v", f, e)
		}
	}
	codex := tomlRemote(t, filepath.Join(home, ".codex", "config.toml"))
	if codex["url"] != url || len(codex) != 1 {
		t.Fatalf("codex entry = %v", codex)
	}
	if !strings.Contains(out, "codex mcp login runsheet") {
		t.Fatalf("no codex login hint:\n%s", out)
	}
	noSecrets(t, home)
	remoteAgainIsANoOp(t, home, url)
}

// remoteAgainIsANoOp re-runs the install and sees every file unchanged.
func remoteAgainIsANoOp(t *testing.T, home, url string) {
	t.Helper()
	before := map[string][]byte{}
	for _, f := range remoteFiles(home) {
		before[f] = read(t, f)
	}
	out := mustInstallRemote(t, url, "all")
	for _, f := range remoteFiles(home) {
		if !strings.Contains(out, "unchanged "+f) || !bytes.Equal(read(t, f), before[f]) {
			t.Fatalf("second run changed %s:\n%s", f, out)
		}
	}
	noSecrets(t, home)
}

func TestMcpInstallRemoteDryRunWritesNothing(t *testing.T) {
	home := remoteHome(t)
	srv := issuer(t, false)
	out, err := installRemote(t, srv.URL+"/mcp", true, "all")
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	for _, f := range remoteFiles(home) {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatalf("dry run wrote %s", f)
		}
		if !strings.Contains(out, "would write "+f) {
			t.Fatalf("dry run lacks %s:\n%s", f, out)
		}
	}
	if !strings.Contains(out, `+bearer_token_env_var = "MESH_TOKEN"`) {
		t.Fatalf("dry run's diff lacks the codex bearer:\n%s", out)
	}
	if strings.Contains(out, envSecret) || strings.Contains(out, fileSecret) {
		t.Fatalf("dry run printed a token:\n%s", out)
	}
}

// A stdio entry already there is replaced by the remote one in place.
func TestMcpInstallRemoteReplacesTheStdioEntry(t *testing.T) {
	home := remoteHome(t)
	mustInstall(t, false, "codex")
	srv := issuer(t, false)
	mustInstallRemote(t, srv.URL+"/mcp", "codex")
	codex := tomlRemote(t, filepath.Join(home, ".codex", "config.toml"))
	if _, ok := codex["command"]; ok || codex["url"] != srv.URL+"/mcp" {
		t.Fatalf("codex entry = %v", codex)
	}
}

// With no --url the entry points at the gateway's /mcp.
func TestMcpInstallRemoteDefaultsToTheGatewaysMCP(t *testing.T) {
	remoteHome(t)
	srv := issuer(t, false)
	t.Setenv("MESH_GATEWAY", srv.URL)
	out := mustInstallRemote(t, "", "cursor")
	if !strings.Contains(out, "url: "+srv.URL+"/mcp\n") {
		t.Fatalf("url line:\n%s", out)
	}
	if got := jsonRemote(t, filepath.Join(os.Getenv("HOME"), ".cursor", "mcp.json")).URL; got != srv.URL+"/mcp" {
		t.Fatalf("url = %q", got)
	}
}
