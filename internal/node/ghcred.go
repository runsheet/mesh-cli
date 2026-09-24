package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// GitHubURL is where a workspace's repos live; Git.GitHubURL overrides
// it (tests stand a local server in for GitHub).
const GitHubURL = "https://github.com"

// githubTokenSkew is how long before expiry an item's GitHub token is
// renewed. The runner looks every ProgressEvery (60 s) and a token lives
// an hour, so five minutes leaves a push in flight room to finish.
const githubTokenSkew = 5 * time.Minute

// GitHubTokenNeedsRefresh reports whether an item's GitHub token has
// expired or expires within githubTokenSkew. A token with no recorded
// expiry is taken at face value.
func GitHubTokenNeedsRefresh(tok *gateway.GitHubToken) bool {
	if tok == nil || tok.Token == "" {
		return true
	}
	if tok.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().Add(githubTokenSkew).After(tok.ExpiresAt)
}

// tokenShape is what an installation token looks like (ghs_…); anything
// else is refused rather than written into a shell helper.
var tokenShape = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// credentialHelper is the git credential helper that answers with the
// token and nothing else.
func credentialHelper(token string) string {
	return `!f() { test "$1" = get || exit 0; echo username=x-access-token; echo password=` + token + `; }; f`
}

// WriteCredentialHelper makes the checkout in dir answer base (GitHub)
// with the item's token alone: in the checkout's own .git/config, never
// the operator's global one, an empty helper first — which drops every
// helper a lower config set, the operator's `gh auth git-credential`
// among them — then the token's. Rewriting it replaces the token.
func WriteCredentialHelper(ctx context.Context, dir, base string, tok *gateway.GitHubToken) error {
	if tok == nil || !tokenShape.MatchString(tok.Token) {
		return errors.New("github token: missing or malformed")
	}
	return writeHelper(ctx, dir, base, credentialHelper(tok.Token))
}

// writeHelper makes helper the only credential helper the checkout in
// dir uses for base.
func writeHelper(ctx context.Context, dir, base, helper string) error {
	key := "credential." + strings.TrimRight(base, "/") + ".helper"
	// not through Git.run: its log would carry the token
	gitConfig := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "git", append([]string{"config", "--local"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git config %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	// exit 5: nothing to unset yet
	if err := gitConfig("--unset-all", key); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 5 {
			return err
		}
	}
	if err := gitConfig("--add", key, ""); err != nil {
		return err
	}
	return gitConfig("--add", key, helper)
}

// OperatorGHConfigDir is where the operator's own gh keeps its login,
// by gh's rules: $GH_CONFIG_DIR, else $XDG_CONFIG_HOME/gh, else
// ~/.config/gh.
func OperatorGHConfigDir() (string, error) {
	if d := os.Getenv("GH_CONFIG_DIR"); d != "" {
		return d, nil
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "gh"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "gh"), nil
}

// AuthorizeOperator is the old platform's fallback for a checkout whose
// session runs with an empty GH_CONFIG_DIR (channel mode's shared
// session): the checkout's own helper asks the operator's gh, at its own
// config dir, as the operator's global `gh auth git-credential` did
// before. It reaches no checkout but this one.
func (g *Git) AuthorizeOperator(ctx context.Context, dir string) error {
	ghDir, err := OperatorGHConfigDir()
	if err != nil {
		return err
	}
	if strings.ContainsAny(ghDir, "'\n") {
		return fmt.Errorf("gh config dir %q cannot be quoted for a credential helper", ghDir)
	}
	return writeHelper(ctx, dir, g.githubURL(), "!GH_CONFIG_DIR='"+ghDir+"' gh auth git-credential")
}

// NewGHConfigDir is a fresh, empty directory for a session's
// GH_CONFIG_DIR, outside the workspace; the caller removes it.
func NewGHConfigDir() (string, error) {
	return os.MkdirTemp("", "mesh-gh-")
}

// SessionEnv is what a session running on the item's token adds to its
// environment: gh authenticates with the token and reads its config
// from ghConfigDir, never the operator's ~/.config/gh.
func SessionEnv(tok *gateway.GitHubToken, ghConfigDir string) []string {
	return []string{"GH_TOKEN=" + tok.Token, "GH_CONFIG_DIR=" + ghConfigDir}
}

// ItemCred is an item's GitHub token on its workspace, renewed before it
// expires. A nil *ItemCred is the old platform's fallback: the
// operator's own gh and git credentials, as before.
type ItemCred struct {
	gw     Gateway
	ws     Workspacer
	spec   WorkspaceSpec
	itemID string
	nodeID string

	mu  sync.Mutex
	tok *gateway.GitHubToken
}

// NewItemCred asks for the item's token and configures the workspace
// with it, before anything clones, fetches or pushes. An old platform
// (gateway.IsUnsupported) answers nil, nil: the fallback, not a
// failure. A refusal is an error — an item the platform will not give a
// token to is not run on the operator's login instead.
func NewItemCred(ctx context.Context, gw Gateway, ws Workspacer, scopes Scopes, item gateway.WorkItem, spec WorkspaceSpec, logf func(string, ...any)) (*ItemCred, error) {
	sc, _ := scopes.ByOrg(item.OrgID)
	tok, err := gw.IssueItemRepoToken(ctx, item.ID, sc.NodeID)
	if gateway.IsUnsupported(err) {
		logf("item %s: the platform issues no item GitHub tokens; using the operator's own gh login", item.ID)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("github token: %w", err)
	}
	if tok.Repository != "" && !strings.EqualFold(tok.Repository, spec.Repo) {
		return nil, fmt.Errorf("github token: issued for %s, the item is on %s", tok.Repository, spec.Repo)
	}
	if err := ws.Authorize(ctx, spec, tok); err != nil {
		return nil, fmt.Errorf("github token: %w", err)
	}
	return &ItemCred{gw: gw, ws: ws, spec: spec, itemID: item.ID, nodeID: sc.NodeID, tok: tok}, nil
}

// Token is the current token (nil on the fallback).
func (c *ItemCred) Token() *gateway.GitHubToken {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tok
}

// Refresh renews the token when it is near expiry and rewrites the
// workspace's credential helper with the new one.
func (c *ItemCred) Refresh(ctx context.Context) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !GitHubTokenNeedsRefresh(c.tok) {
		return nil
	}
	tok, err := c.gw.IssueItemRepoToken(ctx, c.itemID, c.nodeID)
	if err != nil {
		return fmt.Errorf("refresh github token: %w", err)
	}
	if err := c.ws.Authorize(ctx, c.spec, tok); err != nil {
		return fmt.Errorf("refresh github token: %w", err)
	}
	c.tok = tok
	return nil
}
