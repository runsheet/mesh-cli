package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"projectmesh.io/mesh-cli/internal/sandbox"
)

// PodProbeLoopback is the preflight's sandbox probe as the node image
// runs it (Codex.PodProbe left empty): the pod has no mesh binary, so
// node — which the image carries — listens on 127.0.0.1:0, writes the
// address it got and closes it, as ProbeLoopback does on the host. A
// refused socket is written `bind: operation not permitted`, the line
// socketLine finds.
var PodProbeLoopback = []string{"node", "-e", `const s=require("net").createServer();` +
	`s.on("error",(e)=>{console.error("bind: "+((e.code==="EPERM"||e.code==="EACCES")?"operation not permitted":e.message));process.exit(1)});` +
	`s.listen(0,"127.0.0.1",()=>{const a=s.address();console.log(a.address+":"+a.port);s.close()});`}

// podProbeCommand is c.PodProbe, else PodProbeLoopback.
func (c *Codex) podProbeCommand() []string {
	if len(c.PodProbe) > 0 {
		return c.PodProbe
	}
	return PodProbeLoopback
}

// preflightSandboxed is Preflight in the session's own pod (ADR 007,
// ADR 011): the job's Session — the one the node hands the run, so
// preflight and session share the pod — else, with a Sandbox, one of the
// harness's own opened and closed here. The CODEX_HOME is made at
// Session.CodexHome inside the session's HOME exactly as runSandboxed
// makes it (prepareSandboxHome), and every step runs by Session.Exec in
// the pod with the env the session gets, by name: (1) the image's `codex
// mcp get <builtin>`, (2) the tools/list handshake — the built-in's
// relay as config.toml names it, run in the pod and spoken to over its
// stdio (podListTools), so the bearer it sends is the token file the
// session's relay reads — and (3) `codex sandbox … -- <PodProbe>`. An
// http built-in (no relay) is handshaken from the node, as on the host:
// the pod has no client for it but codex itself. Errors read as the
// host's do, and never carry a secret.
func (c *Codex) preflightSandboxed(ctx context.Context, job Job) error {
	sess := job.Session
	if sess == nil {
		if c.Sandbox == nil {
			return errors.New("codex: a job's session with no sandbox")
		}
		var err error
		if sess, err = c.Sandbox.Open(job.Label+" preflight", string(job.Kind), sandbox.GitIdentity{}); err != nil {
			return fmt.Errorf("codex: %w", err)
		}
		defer sess.CloseAndLog()
	}
	home, env, placed, err := c.prepareSandboxHome(job, sess)
	if errors.Is(err, ErrNoIssuedCredential) {
		return err
	}
	if err != nil {
		return fmt.Errorf("harness codex: %s", Redact(err.Error(), job.Secrets...))
	}
	defer func() {
		if err := home.close(); err != nil && c.Stderr != nil {
			fmt.Fprintf(c.Stderr, "harness codex: write back the refreshed login: %v\n", err)
		}
		// the session makes its own again; a preflight that failed leaves
		// no Codex login in a HOME a Claude session may run in next
		os.RemoveAll(home.dir)
	}()
	secrets := append(append([]string{}, job.Secrets...), home.secrets...)
	redact := func(msg string) string { return Redact(msg, secrets...) }
	bin := c.SandboxBinary
	if bin == "" {
		bin = "codex"
	}
	exec := func(cmd []string, stdin string, stdout, stderr *bytes.Buffer) error {
		ectx, cancel := context.WithTimeout(ctx, PreflightTimeout)
		defer cancel()
		spec := sandbox.RunSpec{Workspace: job.Workspace, Env: env, Command: cmd, Timeout: PreflightTimeout, Stdout: stdout, Stderr: stderr}
		if stdin != "" {
			spec.Stdin = strings.NewReader(stdin)
		}
		return sess.Exec(ectx, spec)
	}

	if srv, ok := job.MCPServers[BuiltinMCP].(map[string]any); ok {
		url, _ := srv["url"].(string)
		relayURL, relay := meshContextRelay(job)
		if relay {
			url = relayURL
		}
		// a proxied built-in is reached through the session's socket:
		// listed by its shim, as a relay is, with no token file to check
		_, _, proxied := ProxiedOf(srv)
		if proxied {
			url = "sessionproxy"
		}
		if url != "" {
			// (1) the image's codex sees the server in the config it will read
			var out bytes.Buffer
			if err := exec([]string{bin, "mcp", "get", BuiltinMCP}, "", &out, &out); err != nil {
				why := strings.TrimSpace(out.String())
				if why == "" {
					why = err.Error()
				}
				return fmt.Errorf("codex mcp get %s: %s", BuiltinMCP, truncateRunes(redact(why), 200))
			}
			// (2) the server answers the session's relay, in the pod
			switch {
			case relay || proxied:
				if err := c.podListTools(placed, home, exec, redact, proxied); err != nil {
					return err
				}
			default:
				s := &codexSession{c: c, home: home, environ: env, secrets: secrets}
				if err := c.preflightHTTP(ctx, job, s, url); err != nil {
					return err
				}
			}
		}
	}

	// (3) Codex's own sandbox, in the pod, on a command that binds loopback
	var stdout, stderr bytes.Buffer
	err = exec(append([]string{bin}, SandboxProbe(c.podProbeCommand())...), "", &stdout, &stderr)
	why := strings.TrimSpace(stderr.String())
	if err == nil && !strings.Contains(why, "bwrap:") {
		return nil
	}
	if line := bwrapLine(why); line != "" {
		why = line
	} else if line := socketLine(why + "\n" + stdout.String()); line != "" {
		why = line
	} else if why == "" {
		why = strings.TrimSpace(stdout.String())
	}
	if why == "" {
		why = err.Error()
	}
	return fmt.Errorf("sandbox: %s", truncateRunes(redact(why), 200))
}

// podListTools is the preflight's (2) in the pod: the built-in's relay,
// as the placed job (and so config.toml) names it, run there and sent an
// MCP client's opening on its stdin — initialize,
// notifications/initialized, tools/list — its answer to tools/list read
// from its stdout, which must list at least one tool.
func (c *Codex) podListTools(placed Job, home *codexHome, exec func([]string, string, *bytes.Buffer, *bytes.Buffer) error, redact func(string) string, proxied bool) error {
	if !proxied {
		if home.tokenFile == "" {
			return fmt.Errorf("%s relays on no token file", BuiltinMCP)
		}
		if tok, err := readTokenFile(home.tokenFile); err != nil || tok == "" {
			return fmt.Errorf("%s's token file %s is missing or empty", BuiltinMCP, home.tokenFile)
		}
	}
	srv, _ := placed.MCPServers[BuiltinMCP].(map[string]any)
	command, _ := srv["command"].(string)
	if command == "" {
		return fmt.Errorf("%s relays on no command", BuiltinMCP)
	}
	cmd := append([]string{command}, stringSlice(srv["args"])...)
	var in strings.Builder
	for _, m := range []map[string]any{
		{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "mesh-preflight", "version": "1"},
		}},
		{"jsonrpc": "2.0", "method": "notifications/initialized"},
		{"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{}},
	} {
		b, _ := json.Marshal(m)
		in.Write(b)
		in.WriteByte('\n')
	}
	var stdout, stderr bytes.Buffer
	runErr := exec(cmd, in.String(), &stdout, &stderr)
	for _, step := range []struct {
		id     int
		method string
	}{{1, "initialize"}, {2, "tools/list"}} {
		r := findMCPLine(stdout.Bytes(), step.id)
		if r == nil {
			why := strings.TrimSpace(stderr.String())
			if why == "" && runErr != nil {
				why = runErr.Error()
			}
			if why == "" {
				why = "no response"
			}
			return fmt.Errorf("unreachable: %s: %s", step.method, truncateRunes(redact(why), 200))
		}
		if r.Error != nil {
			msg := redact(r.Error.Message)
			if strings.Contains(msg, " answered 401") || strings.Contains(msg, " answered 403") {
				return fmt.Errorf("refuses auth: %s", truncateRunes(msg, 200))
			}
			return fmt.Errorf("%s: error %d: %s", step.method, r.Error.Code, truncateRunes(msg, 200))
		}
		if step.id == 2 {
			var result struct {
				Tools []json.RawMessage `json:"tools"`
			}
			if err := json.Unmarshal(r.Result, &result); err != nil {
				return fmt.Errorf("tools/list: %v", err)
			}
			if len(result.Tools) == 0 {
				return fmt.Errorf("%s lists no tools", BuiltinMCP)
			}
		}
	}
	return nil
}

// findMCPLine is the response to id among a stdio server's lines.
func findMCPLine(out []byte, id int) *mcpResponse {
	want := fmt.Sprint(id)
	for _, line := range bytes.Split(out, []byte("\n")) {
		var r mcpResponse
		if json.Unmarshal(bytes.TrimSpace(line), &r) == nil && string(r.ID) == want {
			return &r
		}
	}
	return nil
}

// stringSlice is v as a []string: a []string, or a []any of strings.
func stringSlice(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
