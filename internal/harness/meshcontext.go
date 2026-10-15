package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// TokenFileName is a session's runsheet token file, in its own
// directory: `<workdir>.session/` on the host, the HOME of its pod,
// the CODEX_HOME of a Codex session (mesh-cli ADR-029).
const TokenFileName = "runsheet.token"

// RelayCommand is the mesh subcommand that relays stdio MCP to the
// gateway's /mcp on a bearer read from a file per request.
const RelayCommand = "mcp-relay"

// TokenKeeper keeps a session's runsheet token file fresh: Keep
// writes the bearer at path (0600, atomically) before it returns and
// rewrites it as the bearer rotates until release. release(true)
// removes the file — the session's end is handled; release(false)
// leaves it for a node that adopts the session (ADR-009).
type TokenKeeper interface {
	Keep(path string) (release func(remove bool), err error)
}

// MeshContextRelay is the runsheet server a session runs on the
// host: bin's RelayCommand to url on the bearer in tokenFile. The node
// renders it with tokenFile empty; the harness names the session's own
// file (placeMeshContext). No bearer rides it.
func MeshContextRelay(bin, url, tokenFile string) map[string]any {
	return map[string]any{"type": "stdio", "command": bin, "args": []string{RelayCommand, "--url", url, "--token-file", tokenFile}}
}

// relayOf reads a MeshContextRelay entry back: its url and token file.
func relayOf(srv any) (url, tokenFile string, ok bool) {
	m, isMap := srv.(map[string]any)
	if !isMap {
		return "", "", false
	}
	var args []string
	switch a := m["args"].(type) {
	case []string:
		args = a
	case []any:
		for _, v := range a {
			s, _ := v.(string)
			args = append(args, s)
		}
	}
	if len(args) == 0 || args[0] != RelayCommand {
		return "", "", false
	}
	for i := 1; i+1 < len(args); i++ {
		switch args[i] {
		case "--url":
			url = args[i+1]
		case "--token-file":
			tokenFile = args[i+1]
		}
	}
	return url, tokenFile, url != ""
}

// meshContextRelay is the job's runsheet relay, when its built-in
// server is one.
func meshContextRelay(job Job) (url string, ok bool) {
	url, _, ok = relayOf(job.MCPServers[BuiltinMCP])
	return url, ok
}

// withServer is servers with name set to srv, servers itself left as it
// is.
func withServer(servers map[string]any, name string, srv map[string]any) map[string]any {
	out := make(map[string]any, len(servers))
	for k, v := range servers {
		out[k] = v
	}
	out[name] = srv
	return out
}

// PlaceMeshContext points servers' runsheet relay at the token
// file in dir and has keeper keep it, returning the servers as the
// session runs them, the file's path and its release. Servers whose
// built-in is not a relay, or no keeper, come back as they are, with no
// path and a release that does nothing. pod names the relay as a pod
// runs it: RelayScript beside mcpPath. The channel session places its
// own (internal/channel); a harness's job, placeMeshContext.
func PlaceMeshContext(servers map[string]any, keeper TokenKeeper, dir string, pod bool, mcpPath string) (map[string]any, string, func(bool), error) {
	noop := func(bool) {}
	srv, _ := servers[BuiltinMCP].(map[string]any)
	url, _, ok := relayOf(srv)
	if !ok || keeper == nil {
		return servers, "", noop, nil
	}
	path := filepath.Join(dir, TokenFileName)
	release, err := keeper.Keep(path)
	if err != nil {
		return servers, "", noop, err
	}
	bin, _ := srv["command"].(string)
	next := MeshContextRelay(bin, url, path)
	if pod {
		next = sandboxRelayServer(mcpPath, url, path)
	}
	return withServer(servers, BuiltinMCP, next), path, release, nil
}

// placeMeshContext is PlaceMeshContext on a job's servers, the file
// named on the job for its state.json.
func placeMeshContext(job Job, keeper TokenKeeper, dir string, pod bool, mcpPath string) (Job, string, func(bool), error) {
	servers, path, release, err := PlaceMeshContext(job.MCPServers, keeper, dir, pod, mcpPath)
	if err != nil {
		return job, "", release, err
	}
	job.MCPServers, job.tokenFile = servers, path
	return job, path, release, nil
}

// readTokenFile is the bearer in a relay's token file, trimmed.
func readTokenFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// keepAdopted keeps the token file an adopted session's state names
// fresh, as the node that started it did (ADR-009's amendment): the
// adopting node's bearer from now on. A state with none, or a harness
// with no keeper, keeps nothing.
func (c *ClaudeCode) keepAdopted(st sessionunit.State) func(bool) {
	if st.TokenFile == "" || c.Tokens == nil {
		return func(bool) {}
	}
	release, err := c.Tokens.Keep(st.TokenFile)
	if err != nil {
		if c.Stderr != nil {
			fmt.Fprintf(c.Stderr, "harness claude-code: attach: runsheet token: %v\n", err)
		}
		return func(bool) {}
	}
	return release
}
