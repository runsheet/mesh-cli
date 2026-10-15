package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"projectmesh.io/mesh-cli/internal/sandbox"
)

// runSandboxed is Run in the session's own pod (ADR 007), as
// ClaudeCode.runSandboxed: the CODEX_HOME made at Session.CodexHome
// under its temporary HOME (prepareSandboxHome), `codex exec` run from
// the node image with the prompt on stdin, the stream read as on the
// host. Every secret is in a file in the HOME or the env by name, never
// on the argv, and the log is redacted. A kill by the pod's limits is
// returned as the *sandbox.LimitError it is. The job's own Session (its
// services started by the node) is used and left open; without one the
// harness opens and closes its own.
func (c *Codex) runSandboxed(ctx context.Context, job Job, onProgress func(Progress)) (Result, error) {
	sess := job.Session
	if sess == nil {
		if c.Sandbox == nil {
			return Result{}, errors.New("harness codex: a job's session with no sandbox")
		}
		var err error
		if sess, err = c.Sandbox.Open(job.Label, string(job.Kind), sandbox.GitIdentityFromEnv(job.Env)); err != nil {
			return Result{}, fmt.Errorf("harness codex: %w", err)
		}
		defer sess.CloseAndLog()
	}
	home, env, err := c.prepareSandboxHome(job, sess)
	if errors.Is(err, ErrNoIssuedCredential) {
		return Result{}, fmt.Errorf("harness codex: %w", err)
	}
	if err != nil {
		return Result{}, fmt.Errorf("harness codex: %s", Redact(err.Error(), job.Secrets...))
	}
	defer func() {
		if err := home.close(); err != nil && c.Stderr != nil {
			fmt.Fprintf(c.Stderr, "harness codex: write back the refreshed login: %v\n", err)
		}
	}()
	secrets := append(append([]string{}, job.Secrets...), home.secrets...)

	kept := c.keepSession(job, secrets)
	defer kept.close()

	if job.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, job.Timeout)
		defer cancel()
	}
	bin := c.SandboxBinary
	if bin == "" {
		bin = "codex"
	}
	var errBuf bytes.Buffer
	stderr := kept.stderr(&errBuf, c.Stderr)
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := sess.Run(ctx, sandbox.RunSpec{
			Workspace: job.Workspace,
			Env:       env,
			Command:   append([]string{bin}, c.Args(job)...),
			Timeout:   job.Timeout,
			Stdin:     strings.NewReader(job.Prompt),
			Stdout:    pw,
			Stderr:    stderr,
		})
		pw.Close()
		done <- err
	}()
	rawOut := &capBuffer{max: 64 << 10}
	res, failure := c.readStream(io.TeeReader(pr, kept.stdout(rawOut)), job, onProgress)
	_, _ = io.Copy(io.Discard, pr) // a stream the parser gave up on still drains
	res.KeptSessionDir = kept.dir
	waitErr := <-done
	if le := (*sandbox.LimitError)(nil); errors.As(waitErr, &le) && ctx.Err() == nil {
		return res, le
	}
	return codexEnd(ctx, res, failure, waitErr, &errBuf, rawOut, secrets, home.dir)
}

// prepareSandboxHome makes the session's CODEX_HOME at
// sess.CodexHome(), inside the pod's HOME, and returns it with the pod
// session's env: job.Env with CODEX_HOME naming that directory (which
// Session.Env passes by name), and the home's own variables. The login
// is placed as prepareHome places it — a job's own CODEX_HOME (an
// issued credential's auth.json) copied in and written back when Codex
// refreshed it, else Account's plan login copied likewise or its API key
// in the env only — and config.toml renders the job's MCP servers, the
// mesh-context relay as RelayScript in the HOME on a token file in the
// CODEX_HOME. The HOME is the session's: Close removes it, so the home
// is never owned here.
func (c *Codex) prepareSandboxHome(job Job, sess *sandbox.Session) (*codexHome, []string, error) {
	dir := sess.CodexHome()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	h := &codexHome{dir: dir}
	fail := func(err error) (*codexHome, []string, error) {
		h.release(true)
		return nil, nil, err
	}
	prov := c.Account.Provider
	placed := jobCodexHome(job.Env)
	switch {
	case placed != "":
		// an issued credential's home: OpenAI's own, no provider (adr-015)
		prov = CodexProvider{}
		if err := h.copyLogin(placed); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail(err)
		}
	case c.Account.IsAPIKey():
		key, err := c.Account.apiKey()
		if err != nil {
			return fail(err)
		}
		h.env = append(h.env, "CODEX_API_KEY="+key, "OPENAI_API_KEY="+key)
		h.secrets = append(h.secrets, key)
	case c.Account.Home != "":
		login, err := c.Account.loginHome()
		if err != nil {
			return fail(err)
		}
		if err := h.copyLogin(login); err != nil {
			return fail(fmt.Errorf("codex account %q: no login in %s (run `CODEX_HOME=%s codex login`): %w", c.Account.Name, login, login, err))
		}
	default:
		// no placed home and no account: never the operator's own login
		return fail(ErrNoIssuedCredential)
	}
	if _, ok := meshContextRelay(job); ok && c.Tokens != nil {
		// the pod has no mesh binary: the relay is RelayScript in its
		// HOME, on a token file in the CODEX_HOME the node keeps fresh
		script, err := sess.WriteFile(relayScriptName, RelayScript())
		if err != nil {
			return fail(fmt.Errorf("mesh-context relay: %w", err))
		}
		var path string
		if job, path, h.releaseToken, err = placeMeshContext(job, c.Tokens, dir, true, script); err != nil {
			return fail(fmt.Errorf("mesh-context token: %w", err))
		}
		h.tokenFile = path
	}
	cfg, cfgEnv, err := codexConfig(job, prov)
	if err != nil {
		return fail(err)
	}
	if cfg != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o600); err != nil {
			return fail(err)
		}
		h.env = append(h.env, cfgEnv...)
		for _, kv := range cfgEnv {
			_, v, _ := strings.Cut(kv, "=")
			h.secrets = append(h.secrets, v)
		}
	}
	env := make([]string, 0, len(job.Env)+len(h.env)+1)
	for _, kv := range job.Env {
		if name, _, _ := strings.Cut(kv, "="); name != "CODEX_HOME" {
			env = append(env, kv)
		}
	}
	env = append(env, "CODEX_HOME="+dir)
	return h, append(env, h.env...), nil
}

// copyLogin copies from's auth.json into the home, remembering it so
// close writes a refreshed one back, and adds its tokens to the secrets
// the log is redacted of.
func (h *codexHome) copyLogin(from string) error {
	login := filepath.Join(from, "auth.json")
	orig, err := os.ReadFile(login)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(h.dir, "auth.json"), orig, 0o600); err != nil {
		return err
	}
	h.login, h.orig = login, orig
	h.secrets = append(h.secrets, loginSecrets(orig)...)
	return nil
}

// loginSecrets are the string values of an auth.json long enough to be
// a token or a key: what a log must never show.
func loginSecrets(auth []byte) []string {
	var v any
	if json.Unmarshal(auth, &v) != nil {
		return []string{strings.TrimSpace(string(auth))}
	}
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		case string:
			if len(x) >= 16 {
				out = append(out, x)
			}
		}
	}
	walk(v)
	return out
}
