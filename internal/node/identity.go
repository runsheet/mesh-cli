package node

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// AgentIdentity is the git identity a session of role on the node slug
// commits as: `Mesh <role> (<slug>) <mesh-<role>+<slug>@runsheet.dev>`,
// never the node's owner (ADR-024).
func AgentIdentity(role, slug string) (name, email string) {
	return fmt.Sprintf("Mesh %s (%s)", role, slug), fmt.Sprintf("mesh-%s+%s@runsheet.dev", role, slug)
}

// Pairer is a person paired with a session: one Co-Authored-By trailer
// on each of its commits.
type Pairer struct {
	Name  string `yaml:"name"`
	Email string `yaml:"email"`
}

// Trailer is the pairer's Co-Authored-By line.
func (p Pairer) Trailer() string {
	return "Co-Authored-By: " + p.Name + " <" + p.Email + ">"
}

// Pairers are the people paired with item's session. The platform
// serves no paired field on WorkItem or Goal yet (schema.json), so the
// node's own `pair:` (configured) is the interim source (ADR-024); once
// it does, the item's and goal's are read here first. A pairer without
// a name and an email, or with a line break in either, is left out.
func Pairers(item gateway.WorkItem, goal *gateway.Goal, configured []Pairer) []Pairer {
	_, _ = item, goal
	var out []Pairer
	seen := map[string]bool{}
	for _, p := range configured {
		p.Name, p.Email = strings.TrimSpace(p.Name), strings.TrimSpace(p.Email)
		if p.Name == "" || p.Email == "" || strings.ContainsAny(p.Name+p.Email, "\r\n<>") {
			continue
		}
		if k := strings.ToLower(p.Email); !seen[k] {
			seen[k] = true
			out = append(out, p)
		}
	}
	return out
}

// Identity is what a session's commits carry: the agent as author and
// committer, a trailer per pairer.
type Identity struct {
	Name    string
	Email   string
	Pairers []Pairer
}

// identityFor is the identity of a role's session on item: the agent of
// the node's slug, and the item's pairers.
func identityFor(cfg *Config, role string, item gateway.WorkItem, goal *gateway.Goal) Identity {
	var pair []Pairer
	if cfg != nil {
		pair = cfg.Pair
	}
	name, email := AgentIdentity(role, agentSlug(cfg))
	return Identity{Name: name, Email: email, Pairers: Pairers(item, goal, pair)}
}

// agentSlug is the node slug a session's agent is named for: node.yaml's
// slug, else "node".
func agentSlug(cfg *Config) string {
	if cfg == nil || cfg.Slug == "" {
		return "node"
	}
	return cfg.Slug
}

// Env is the identity as GIT_AUTHOR_*/GIT_COMMITTER_*: a session that
// commits past the checkout's config still commits as the agent.
func (id Identity) Env() []string {
	return []string{
		"GIT_AUTHOR_NAME=" + id.Name, "GIT_AUTHOR_EMAIL=" + id.Email,
		"GIT_COMMITTER_NAME=" + id.Name, "GIT_COMMITTER_EMAIL=" + id.Email,
	}
}

// Apply puts the identity on job's environment.
func (id Identity) Apply(job *harness.Job) {
	job.Env = append(job.Env, id.Env()...)
}

// Sandbox is the identity a session's pod is opened with: the agent in
// its .gitconfig and the hooks directory mounted beside the workspace.
func (id Identity) Sandbox(dir string) sandbox.GitIdentity {
	return sandbox.GitIdentity{Name: id.Name, Email: id.Email, HooksDir: HooksDir(dir)}
}

// HooksDir is where the node writes a checkout's hooks: beside it, not
// in `<workdir>.session`, which every session start clears.
func HooksDir(dir string) string { return filepath.Clean(dir) + ".hooks" }

// Identify implements Workspacer: the checkout's own config names the
// agent (`git config --local user.name/user.email`) and its
// core.hooksPath is HooksDir, whose prepare-commit-msg adds each
// pairer's Co-Authored-By unless the message carries it already. With
// no pairer the hook adds nothing.
func (g *Git) Identify(ctx context.Context, dir string, id Identity) error {
	hooks := HooksDir(dir)
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		return fmt.Errorf("identify: %w", err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "prepare-commit-msg"), []byte(prepareCommitMsg(id.Pairers)), 0o755); err != nil {
		return fmt.Errorf("identify: %w", err)
	}
	for _, kv := range [][2]string{{"user.name", id.Name}, {"user.email", id.Email}, {"core.hooksPath", hooks}} {
		cmd := exec.CommandContext(ctx, "git", "config", "--local", kv[0], kv[1])
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git config %s: %w: %s", kv[0], err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// prepareCommitMsg is the hook: one `git interpret-trailers` per pairer,
// `--if-exists addIfDifferent` so a message that already has that
// pairer's trailer gains no second one while a second pairer still gets
// theirs.
func prepareCommitMsg(pairers []Pairer) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n# written by mesh node: the paired person's Co-Authored-By (ADR-024)\n")
	for _, p := range pairers {
		fmt.Fprintf(&b, "git interpret-trailers --in-place --if-exists addIfDifferent --trailer %s \"$1\" || exit 1\n", shellQuote(p.Trailer()))
	}
	b.WriteString("exit 0\n")
	return b.String()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// actorContext marks ctx as a role's session on the node: every command
// sent on it carries agent:<role>@<slug>, the slug identityFor names the
// agent's commits for (ADR-015's amendment 2026-10-03).
func actorContext(ctx context.Context, cfg *Config, role gateway.Role) context.Context {
	return gateway.WithActor(ctx, role, agentSlug(cfg))
}
