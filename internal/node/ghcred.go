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
	"projectmesh.io/mesh-cli/internal/validate"
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
// dir uses for base. A full clone's goes in its own .git/config
// (--local); a worktree's local config is its shared clone's, read by
// every item of the repo, so a worktree's goes in its own
// config.worktree (--worktree, extensions.worktreeConfig enabled on the
// clone) and one item's token never answers for another's (mesh ADR 017).
func writeHelper(ctx context.Context, dir, base, helper string) error {
	key := "credential." + strings.TrimRight(base, "/") + ".helper"
	scope := "--local"
	if isWorktree(dir) {
		if err := ensureWorktreeConfig(ctx, dir); err != nil {
			return err
		}
		scope = "--worktree"
	}
	// not through Git.run: its log would carry the token
	gitConfig := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "git", append([]string{"config", scope}, args...)...)
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

// ensureWorktreeConfig makes sure the shared clone of the worktree dir
// has extensions.worktreeConfig, so `git config --worktree` writes the
// worktree's own config.worktree. addWorktree enables it under the
// repo's lock; a worktree made before it did gets it here, read first so
// the steady state writes nothing to the clone's one .git/config.
func ensureWorktreeConfig(ctx context.Context, dir string) error {
	get := exec.CommandContext(ctx, "git", "config", "--local", "--type=bool", "--get", "extensions.worktreeConfig")
	get.Dir = dir
	if out, err := get.Output(); err == nil && strings.TrimSpace(string(out)) == "true" {
		return nil
	}
	cmd := exec.CommandContext(ctx, "git", "config", "--local", "extensions.worktreeConfig", "true")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git config extensions.worktreeConfig: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// isWorktree reports whether dir is a git worktree — its .git a file
// naming the clone's — rather than a clone of its own.
func isWorktree(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && !fi.IsDir()
}

type gitTokenKey struct{}

// gitToken is what withGitToken puts on a ctx.
type gitToken struct{ base, helper string }

// withGitToken makes every command Git runs under ctx answer base with
// tok alone, by the environment (GIT_CONFIG_COUNT), never a config file
// or the command line: how the shared clone is cloned and fetched on the
// token of the item it is for, leaving the token behind for no other
// item (mesh ADR 017). A missing or malformed token adds nothing.
func withGitToken(ctx context.Context, base string, tok *gateway.GitHubToken) context.Context {
	if tok == nil || !tokenShape.MatchString(tok.Token) {
		return ctx
	}
	return context.WithValue(ctx, gitTokenKey{}, gitToken{strings.TrimRight(base, "/"), credentialHelper(tok.Token)})
}

// gitTokenEnv is the environment withGitToken asks for, or nil.
func gitTokenEnv(ctx context.Context) []string {
	t, ok := ctx.Value(gitTokenKey{}).(gitToken)
	if !ok {
		return nil
	}
	key := "credential." + t.base + ".helper"
	return []string{"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=" + key, "GIT_CONFIG_VALUE_0=",
		"GIT_CONFIG_KEY_1=" + key, "GIT_CONFIG_VALUE_1=" + t.helper}
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

// TokenFallback is the old-platform fallback's one notice, shared by
// everything a node runs — poll-mode builds, evaluations and channel
// mode's dispatch — so a node claiming many items against a platform
// without issueItemRepoToken says so once for its whole run, not once
// per item. A nil *TokenFallback says it every time.
type TokenFallback struct {
	once sync.Once
}

// Note logs the fallback, the first time only.
func (f *TokenFallback) Note(itemID string, logf func(string, ...any)) {
	say := func() {
		logf("item %s: the platform issues no item GitHub tokens; using the operator's own gh login (said once, for every item this run)", itemID)
	}
	if f == nil {
		say()
		return
	}
	f.once.Do(say)
}

// NotePlan is Note for a plan's fetch on a platform without
// planGitHubToken (adr-022): the operator's own gh, said once per run.
// A planner keeps its own TokenFallback, so the items' notice never
// swallows the plan's.
func (f *TokenFallback) NotePlan(goalSlug string, logf func(string, ...any)) {
	say := func() {
		logf("goal %s: platform issues no plan token; fetching on the operator's gh", goalSlug)
	}
	if f == nil {
		say()
		return
	}
	f.once.Do(say)
}

// NoteResearch is NotePlan for a research session's fetch on a
// platform without researchGitHubToken (adr-022), keyed by the
// direction (<project>/<slug>): the operator's own gh, said once per
// Director.
func (f *TokenFallback) NoteResearch(direction string, logf func(string, ...any)) {
	say := func() {
		logf("direction %s: platform issues no research token; fetching on the operator's gh", direction)
	}
	if f == nil {
		say()
		return
	}
	f.once.Do(say)
}

// NoteBatch is Note for a merge batch candidate's fetch on a platform
// whose issueItemRepoToken takes no mergeBatchId (mesh-cli ADR-032):
// the operator's own login, said once per run.
func (f *TokenFallback) NoteBatch(batch string, logf func(string, ...any)) {
	say := func() {
		logf("%s: the platform issues no merge batch token; fetching candidates on the operator's own login (said once, for every batch this run)", batch)
	}
	if f == nil {
		say()
		return
	}
	f.once.Do(say)
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
	// issue, when set, mints the token in place of IssueItemRepoToken:
	// a merge batch candidate's (NewBatchCred), whose renewals go by the
	// batch's own form.
	issue func(ctx context.Context) (*gateway.GitHubToken, error)

	mu  sync.Mutex
	tok *gateway.GitHubToken
}

// NewItemCred asks for the item's token and configures the workspace
// with it, before anything clones, fetches or pushes. An old platform
// (gateway.IsUnsupported) answers nil, nil: the fallback, not a
// failure, noted once through fb. A refusal is an error — an item the platform will not give a
// token to is not run on the operator's login instead.
func NewItemCred(ctx context.Context, gw Gateway, ws Workspacer, scopes Scopes, item gateway.WorkItem, spec WorkspaceSpec, fb *TokenFallback, logf func(string, ...any)) (*ItemCred, error) {
	return NewItemCredAt(ctx, gw, ws, scopes, item, spec, fb, logf, nil)
}

// NewItemCredAt is NewItemCred with spec's checkout made first by
// checkout (nil: none), on ctx carrying the issued token (OnItemToken; the
// operator's own credentials on an old platform) and before Authorize
// writes the token into it: a rebase's fresh worktree (ADR-028). A
// checkout that cannot be made is a *WorkspaceError, as a token that
// cannot be written is.
func NewItemCredAt(ctx context.Context, gw Gateway, ws Workspacer, scopes Scopes, item gateway.WorkItem, spec WorkspaceSpec, fb *TokenFallback, logf func(string, ...any), checkout func(context.Context) error) (*ItemCred, error) {
	sc, _ := scopes.ByOrg(item.OrgID)
	tok, err := gw.IssueItemRepoToken(ctx, item.ID, sc.NodeID)
	if gateway.IsUnsupported(err) {
		fb.Note(item.ID, logf)
		if checkout != nil {
			if err := checkout(ctx); err != nil {
				return nil, &WorkspaceError{Repo: spec.Repo, Path: spec.Dir(), Err: err, Checkout: true}
			}
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("github token: %w", err)
	}
	if tok.Repository != "" && !strings.EqualFold(tok.Repository, spec.Repo) {
		return nil, fmt.Errorf("github token: issued for %s, the item is on %s", tok.Repository, spec.Repo)
	}
	if checkout != nil {
		if err := checkout(OnItemToken(ctx, ws, tok)); err != nil {
			// the checkout is the node's to make: a lock, a disk, a
			// shared clone that will not fetch — the node's condition
			return nil, &WorkspaceError{Repo: spec.Repo, Path: spec.Dir(), Err: err, Checkout: true}
		}
	}
	if err := ws.Authorize(ctx, spec, tok); err != nil {
		// the platform issued the token; writing it into the checkout
		// failed here — a stale lock in the repo cache, a disk — and that
		// is the node's condition, not the item's
		return nil, &WorkspaceError{Repo: spec.Repo, Path: RepoCachePath(ws, spec), Err: err}
	}
	return &ItemCred{gw: gw, ws: ws, spec: spec, itemID: item.ID, nodeID: sc.NodeID, tok: tok}, nil
}

// WorkspaceError is NewItemCred's local failure: the platform issued the
// item's token and the node could not write it into the checkout
// (Workspacer.Authorize — `git remote set-url` on the repo cache, a
// config lock a dead process left behind). It reads as before, `github
// token: <git's words>`, and is released as an infrastructure hold
// (ReleaseOnInfrastructure), never failed; a token the platform refused
// stays a plain error.
type WorkspaceError struct {
	Repo string // owner/name
	Path string // the repo cache the token was written into
	Err  error
	// Checkout: the checkout itself could not be made (NewItemCredAt),
	// before any token was written; Path is the checkout's.
	Checkout bool
}

func (e *WorkspaceError) Error() string {
	if e.Checkout {
		return "workspace: " + e.Err.Error()
	}
	return "github token: " + e.Err.Error()
}
func (e *WorkspaceError) Unwrap() error { return e.Err }

// WorkspaceErrorOf is err's *WorkspaceError, or nil.
func WorkspaceErrorOf(err error) *WorkspaceError {
	var we *WorkspaceError
	if errors.As(err, &we) {
		return we
	}
	return nil
}

// RepoCachePath is where spec's checkout keeps its git config: the
// repo's one shared clone for a worktree (ADR-028), else the checkout
// itself (its own clone: an evaluation, a full clone from before).
func RepoCachePath(ws Workspacer, spec WorkspaceSpec) string {
	if g, ok := ws.(*Git); ok {
		if p := g.sharedClone(spec); p != "" {
			return p
		}
	}
	return spec.Dir()
}

// mint asks the platform for a fresh token: the batch's form when c
// has one, else the item's.
func (c *ItemCred) mint(ctx context.Context) (*gateway.GitHubToken, error) {
	if c.issue != nil {
		return c.issue(ctx)
	}
	return c.gw.IssueItemRepoToken(ctx, c.itemID, c.nodeID)
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
	tok, err := c.mint(ctx)
	if err != nil {
		return fmt.Errorf("refresh github token: %w", err)
	}
	if err := c.ws.Authorize(ctx, c.spec, tok); err != nil {
		return fmt.Errorf("refresh github token: %w", err)
	}
	c.tok = tok
	return nil
}

// Reauthorize writes the token held on the workspace's credential helper
// again — Authorize, which rebuilds a worktree whose admin dir is gone
// first: what a rebuilt worktree, whose helper went with its
// config.worktree, needs before its next fetch. Nil does nothing.
func (c *ItemCred) Reauthorize(ctx context.Context) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ws.Authorize(ctx, c.spec, c.tok)
}

// Reissue asks the platform for a fresh token and rewrites the
// workspace's credential helper with it, whatever the current token's
// expiry: unlike Refresh it is for a token git has just refused.
func (c *ItemCred) Reissue(ctx context.Context) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	tok, err := c.mint(ctx)
	if err != nil {
		return fmt.Errorf("reissue github token: %w", err)
	}
	if err := c.ws.Authorize(ctx, c.spec, tok); err != nil {
		return fmt.Errorf("reissue github token: %w", err)
	}
	c.tok = tok
	return nil
}

// ForPush makes the checkout's credential helper answer with the
// build's own token — the one issued at claim, or its Refresh — and
// answers it: whatever else wrote into the helper since (an evaluation's
// read token, a later issue) is overwritten, so a push can only ever go
// out on the build's write token. Nil on the fallback.
func (c *ItemCred) ForPush(ctx context.Context) (*gateway.GitHubToken, error) {
	if c == nil {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	refreshErr := c.renewLocked(ctx)
	if err := c.ws.Authorize(ctx, c.spec, c.tok); err != nil {
		return c.tok, errors.Join(refreshErr, fmt.Errorf("github token for the push: %w", err))
	}
	return c.tok, refreshErr
}

// ForFetch is the item's token for a fetch the node runs itself — the
// validator's diff base and the newer head it answers — asked of the
// platform again when it is near expiry, as ForPush's is. It writes
// nothing: the caller runs git under OnItemToken, so the fetch never
// leans on whatever the checkout's helper holds by then. Nil on the
// fallback.
func (c *ItemCred) ForFetch(ctx context.Context) (*gateway.GitHubToken, error) {
	if c == nil {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tok, c.renewLocked(ctx)
}

// renewLocked asks the platform for a fresh token when the one held is
// near expiry; a refusal is answered and the token held kept, since it
// may yet serve. c.mu is held.
func (c *ItemCred) renewLocked(ctx context.Context) error {
	if !GitHubTokenNeedsRefresh(c.tok) {
		return nil
	}
	tok, err := c.mint(ctx)
	if err != nil {
		return fmt.Errorf("refresh github token: %w", err)
	}
	c.tok = tok
	return nil
}

// OnItemToken is ctx with every git command ws runs under it answering
// GitHub with tok alone, by the environment (withGitToken), never a
// config file: a push or fetch goes out on the token the platform issued
// whatever the checkout's helper or the shared clone holds (mesh ADR
// 017). A nil tok, the fallback, is ctx.
func OnItemToken(ctx context.Context, ws Workspacer, tok *gateway.GitHubToken) context.Context {
	base := GitHubURL
	if g, ok := ws.(*Git); ok {
		base = g.githubURL()
	}
	return withGitToken(ctx, base, tok)
}

// PushCredentialError is a build's push GitHub refused on the item's
// token, after one re-issue: the build's failure, the branch left for a
// retry and no finish recorded. It names the repo, the branch and the
// token's access, issue time and expiry, never the token.
type PushCredentialError struct {
	Repo, Branch string
	Token        TokenFacts
	Cause        error
}

func (e *PushCredentialError) Error() string {
	return fmt.Sprintf("push %s to %s refused on the item token (%s): %v", e.Branch, e.Repo, e.Token, e.Cause)
}

func (e *PushCredentialError) Unwrap() error { return e.Cause }

// EnvironmentKind is credential_refused (docs/adr/adr-030).
func (e *PushCredentialError) EnvironmentKind() validate.EnvironmentKind {
	return validate.CredentialRefused
}

// TokenFacts is what may be said of an item token in a log or an error:
// its access as the platform issued it, when it was issued and when it
// expires — never the token.
type TokenFacts struct {
	Access              string
	IssuedAt, ExpiresAt time.Time
}

// FactsOf is tok's TokenFacts (zero for nil, the fallback).
func FactsOf(tok *gateway.GitHubToken) TokenFacts {
	if tok == nil {
		return TokenFacts{}
	}
	return TokenFacts{Access: tok.Access, IssuedAt: tok.IssuedAt, ExpiresAt: tok.ExpiresAt}
}

// String is `access write, issued <t>, expires <t>`; an access the
// platform did not say is `access unknown`, the operator's fallback
// `the operator's login`.
func (f TokenFacts) String() string {
	if f == (TokenFacts{}) {
		return "the operator's login"
	}
	access := f.Access
	if access == "" {
		access = "unknown"
	}
	return fmt.Sprintf("access %s, issued %s, expires %s", access, expiryText(f.IssuedAt), expiryText(f.ExpiresAt))
}

// IsCredentialRefusal reports whether err is git (or gh) refused by
// GitHub on its credential rather than any other failure — by its words,
// validate.IsCredentialRefusal, the environment kind credential_refused.
func IsCredentialRefusal(err error) bool {
	return err != nil && validate.IsCredentialRefusal(err.Error())
}

// CredentialError is a workspace GitHub refused on the item's token
// after every re-issue: the node's credential problem, not the item's.
// It names the repo and the token's expiry and never the token.
type CredentialError struct {
	Repo    string
	Expiry  time.Time
	Retries int
	Cause   error
}

func (e *CredentialError) Error() string {
	return fmt.Sprintf("github refused the item token for %s (expires %s) after %d retries: %v", e.Repo, expiryText(e.Expiry), e.Retries, e.Cause)
}

func (e *CredentialError) Unwrap() error { return e.Cause }

// EnvironmentKind is credential_refused: the node's credential, never
// the item's code (docs/adr/adr-030).
func (e *CredentialError) EnvironmentKind() validate.EnvironmentKind {
	return validate.CredentialRefused
}

// Reason is what the item is released with: the repo, the token's
// expiry, how many tokens were refused and git's (scrubbed) answer —
// never the token.
func (e *CredentialError) Reason() string {
	return fmt.Sprintf("github credential: %s token expiring %s refused %d times: %s", e.Repo, expiryText(e.Expiry), e.Retries+1, truncate(e.Cause.Error(), 500))
}

// ReleaseOnCredential hands an item whose workspace GitHub refused on
// every re-issued token back to the platform (ReleaseWorkItem, attempts
// unchanged) instead of failing it: the credential is the node's
// condition, not the item's (mesh-specs trd §3). A platform that refuses
// the release would leave the item held by a node that has stopped
// working on it, so then the item is failed with the same reason. The
// reason opens with its environment kind, `credential_refused: github
// credential: …` (validate.WithKind, docs/adr/adr-030), for the platform
// to park the item rather than re-plan it. It answers the error the
// item's session ends with.
func ReleaseOnCredential(ctx context.Context, gw Gateway, itemID, nodeID string, ce *CredentialError, logf func(string, ...any)) error {
	cause := fmt.Errorf("workspace: %w", ce)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	reason := truncate(validate.WithKind(validate.CredentialRefused, ce.Reason()), 2000)
	err := gw.ReleaseWorkItem(ctx, itemID, nodeID, reason)
	if err == nil {
		logf("item %s: released, the node's condition: %s", itemID, reason)
		return cause
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return cause
	}
	logf("item %s: release refused (%v); failed instead: %s", itemID, err, reason)
	if err := gw.FailWorkItem(ctx, itemID, reason); err != nil && !errors.Is(err, context.Canceled) {
		logf("item %s: fail: %v", itemID, err)
	}
	return cause
}

func expiryText(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format(time.RFC3339)
}

// credentialBackoff is the wait before each retry of a refused Prepare,
// one per retry.
var credentialBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// credentialSleep waits d or until ctx ends; tests replace it.
var credentialSleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// urlUserinfo is the credential part of a URL git may echo.
var urlUserinfo = regexp.MustCompile(`(https?://)[^/@\s]+@`)

// scrubbed is err with every token it may carry taken out.
func scrubbed(err error, tokens []string) error {
	msg := urlUserinfo.ReplaceAllString(err.Error(), "${1}***@")
	for _, t := range tokens {
		if t != "" {
			msg = strings.ReplaceAll(msg, t, "***")
		}
	}
	if msg == err.Error() {
		return err
	}
	return errors.New(msg)
}

// PrepareWithRetry is ws.Prepare for an item's workspace. When GitHub
// refuses the item's token (IsCredentialRefusal) it re-issues the token
// and tries again after a short backoff, up to three times — a freshly
// minted installation token can be used before GitHub honours it — and
// then answers a *CredentialError. Any other failure, and the operator
// login's fallback (cred == nil), is not retried. The log names the
// token's repo and expiry, never the token.
func PrepareWithRetry(ctx context.Context, ws Workspacer, spec WorkspaceSpec, cred *ItemCred, logf func(string, ...any)) (string, error) {
	// a worktree whose .git/worktrees/<n> went from under it is rebuilt
	// once and the prepare tried once more (ADR-028, Rebuilding a
	// worktree): never the item's failure, never a release
	if _, _, gone := worktreeAdminGone(spec.Dir()); gone && cred != nil {
		// Authorize rebuilds it and writes its helper again, which lived
		// in the lost admin dir's config.worktree
		if err := cred.Reauthorize(ctx); err != nil {
			return "", err
		}
	}
	return rebuildAndRetry(ctx, ws, spec, func(retry bool) (string, error) {
		if retry {
			if err := cred.Reauthorize(ctx); err != nil {
				return "", err
			}
		}
		return prepareWithRetry(ctx, ws, spec, cred, logf)
	})
}

func prepareWithRetry(ctx context.Context, ws Workspacer, spec WorkspaceSpec, cred *ItemCred, logf func(string, ...any)) (string, error) {
	if cred == nil {
		return ws.Prepare(ctx, spec)
	}
	var seen []string
	for attempt := 0; ; attempt++ {
		tok := cred.Token()
		seen = append(seen, tok.Token)
		repo := tok.Repository
		if repo == "" {
			repo = spec.Repo
		}
		dir, err := ws.Prepare(ctx, spec)
		if err == nil {
			if attempt > 0 {
				logf("workspace %s: prepared on a re-issued token for %s (expires %s), attempt %d", spec.Dir(), repo, expiryText(tok.ExpiresAt), attempt+1)
			}
			return dir, nil
		}
		err = scrubbed(err, seen)
		if !IsCredentialRefusal(err) || ctx.Err() != nil {
			return "", err
		}
		logf("workspace %s: github refused the token for %s (expires %s), attempt %d: %v", spec.Dir(), repo, expiryText(tok.ExpiresAt), attempt+1, err)
		if attempt == len(credentialBackoff) {
			return "", &CredentialError{Repo: repo, Expiry: tok.ExpiresAt, Retries: attempt, Cause: err}
		}
		if err := credentialSleep(ctx, credentialBackoff[attempt]); err != nil {
			return "", err
		}
		if err := cred.Reissue(ctx); err != nil {
			return "", err
		}
	}
}
