package harness

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"projectmesh.io/mesh-cli/internal/mcprelay"
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

// SessionTokenKeeper is a TokenKeeper that keeps a session's file from
// that session's own bearer when it has one (mesh-cli ADR-035): the
// harness names the session — its kind and who it is for — and the
// keeper chooses the bearer, or refuses one it will not hand that
// session. A keeper without it keeps every file from one bearer.
type SessionTokenKeeper interface {
	TokenKeeper
	KeepSession(path string, kind Kind, meta SessionMeta) (release func(remove bool), err error)
}

// keepToken keeps path for the session kind and meta name, by
// KeepSession when keeper has it.
func keepToken(keeper TokenKeeper, path string, kind Kind, meta SessionMeta) (func(bool), error) {
	if sk, ok := keeper.(SessionTokenKeeper); ok {
		return sk.KeepSession(path, kind, meta)
	}
	return keeper.Keep(path)
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
// path and a release that does nothing. The channel session places its
// own (internal/channel); a harness's job, placeMeshContext, and a pod
// session's, placePodRelay.
func PlaceMeshContext(servers map[string]any, keeper TokenKeeper, dir string) (map[string]any, string, func(bool), error) {
	return placeRelay(servers, keeper, dir, func(path string) (func(bool), error) { return keeper.Keep(path) })
}

func placeRelay(servers map[string]any, keeper TokenKeeper, dir string, keep func(path string) (func(bool), error)) (map[string]any, string, func(bool), error) {
	noop := func(bool) {}
	srv, _ := servers[BuiltinMCP].(map[string]any)
	url, _, ok := relayOf(srv)
	if !ok || keeper == nil {
		return servers, "", noop, nil
	}
	path := filepath.Join(dir, TokenFileName)
	release, err := keep(path)
	if err != nil {
		return servers, "", noop, err
	}
	bin, _ := srv["command"].(string)
	return withServer(servers, BuiltinMCP, MeshContextRelay(bin, url, path)), path, release, nil
}

// placeMeshContext is PlaceMeshContext on a host job's servers, the file
// named on the job for its state.json: in job.TokenDir when the node
// named one, and kept from the job's own session's bearer by a
// SessionTokenKeeper (mesh-cli ADR-035).
func placeMeshContext(job Job, keeper TokenKeeper, dir string) (Job, string, func(bool), error) {
	if job.TokenDir != "" {
		dir = job.TokenDir
	}
	keep := func(path string) (func(bool), error) { return keepToken(keeper, path, job.Kind, job.Meta) }
	servers, path, release, err := placeRelay(job.MCPServers, keeper, dir, keep)
	if err != nil {
		return job, "", release, err
	}
	job.MCPServers, job.tokenFile = servers, path
	return job, path, release, nil
}

// relayDirPrefix names the temporary dir of the node's a pod session's
// token file goes in when the node named no TokenDir: a mesh-session-*
// dir, so the start-up sweep removes one no found session names.
const relayDirPrefix = "mesh-session-relay-"

// placePodRelay is the runsheet relay of a pod session whose HOME is
// home: the token file kept on the node — in job.TokenDir, else a
// temporary dir of the node's, never under home — the node's endpoint
// serving it on RelaySocketName in home (mcprelay.Serve), and the
// servers naming RelayScript in home on that socket. The pod holds the
// socket's path and nothing of the bearer. The release closes the
// endpoint and releases the file, removing the temporary dir with it
// when remove is set; a detach (remove false) leaves the file for the
// node that adopts the session, which serves the socket again
// (keepAdoptedToken). A job whose built-in is not a relay, or no
// keeper, comes back as it is.
func placePodRelay(job Job, keeper TokenKeeper, home string) (Job, func(bool), error) {
	noop := func(bool) {}
	url, ok := meshContextRelay(job)
	if !ok || keeper == nil {
		return job, noop, nil
	}
	dir, made := job.TokenDir, false
	if dir == "" {
		d, err := os.MkdirTemp("", relayDirPrefix)
		if err != nil {
			return job, noop, err
		}
		dir, made = d, true
	}
	path := filepath.Join(dir, TokenFileName)
	releaseFile, err := keepToken(keeper, path, job.Kind, job.Meta)
	if err != nil {
		if made {
			os.RemoveAll(dir)
		}
		return job, noop, err
	}
	sock := filepath.Join(home, RelaySocketName)
	ep, err := mcprelay.Serve(sock, url, path, nil)
	if err != nil {
		releaseFile(true)
		if made {
			os.RemoveAll(dir)
		}
		return job, noop, err
	}
	release := podRelayRelease(ep, releaseFile, path)
	job.MCPServers = withServer(job.MCPServers, BuiltinMCP, sandboxRelayServer(filepath.Join(home, relayScriptName), sock))
	job.tokenFile, job.relaySocket, job.relayURL = path, sock, url
	return job, release, nil
}

// podRelayRelease closes ep and releases the token file at path, its
// temporary dir removed with it on a removing release.
func podRelayRelease(ep *mcprelay.Endpoint, releaseFile func(bool), path string) func(bool) {
	var once sync.Once
	return func(remove bool) {
		once.Do(func() {
			if ep != nil {
				_ = ep.Close()
			}
			releaseFile(remove)
			if dir := filepath.Dir(path); remove && strings.HasPrefix(filepath.Base(dir), relayDirPrefix) {
				os.RemoveAll(dir)
			}
		})
	}
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
// adopting node's bearer from now on — the session's own, by
// KeepSession, when the node bound one (ADR-035). A state with none, or a harness
// with no keeper, keeps nothing.
func (c *ClaudeCode) keepAdopted(st sessionunit.State) func(bool) {
	return keepAdoptedToken(c.Tokens, c.Stderr, c.Name(), st)
}

// keepAdoptedToken is keepAdopted for any harness: tokens keeps the
// state's token file, a failure said on stderr as name's. A pod
// session's relay endpoint (the state's RelaySocket) is served again on
// that file, as the node that started it served it.
//
// A proxied session (st.Proxy) has no token file to keep: its bearer is
// held in memory by the proxy the adopting node restarts on the state's
// socket (node.SessionProxies.Adopt), so nothing is kept here.
func keepAdoptedToken(tokens TokenKeeper, stderr io.Writer, name string, st sessionunit.State) func(bool) {
	if st.TokenFile == "" || st.Proxy != nil || tokens == nil {
		return func(bool) {}
	}
	meta := SessionMeta{NodeID: st.NodeID, ItemID: st.ItemID, GoalID: st.GoalID, DirectionID: st.DirectionID, Attempt: st.Attempt}
	release, err := keepToken(tokens, st.TokenFile, Kind(st.Kind), meta)
	if err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "harness %s: attach: runsheet token: %v\n", name, err)
		}
		return func(bool) {}
	}
	if st.RelaySocket == "" || st.RelayURL == "" {
		return release
	}
	ep, err := mcprelay.Serve(st.RelaySocket, st.RelayURL, st.TokenFile, nil)
	if err != nil && stderr != nil {
		fmt.Fprintf(stderr, "harness %s: attach: runsheet relay endpoint: %v\n", name, err)
	}
	return podRelayRelease(ep, release, st.TokenFile)
}
