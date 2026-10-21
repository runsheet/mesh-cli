package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/auth"
	"projectmesh.io/mesh-cli/internal/crash"
	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/harness"
)

// TokenRefreshEvery is how often TokenFiles asks for the bearer and
// rewrites each kept file it changed for: a quarter of the margin the
// machine credential refreshes at (auth.RefreshSkew, auth.NeedsRefresh),
// so the first tick after the credential renews falls at least three
// quarters of that margin before the old bearer's expiry, and a file
// never holds a bearer past its life (mesh-cli ADR-029).
const TokenRefreshEvery = auth.RefreshSkew / 4

// TokenFiles keeps every running session's runsheet token file —
// the bearer its relay reads per request (`mesh mcp-relay`,
// harness.RelayScript) — fresh for as long as the session runs
// (mesh-cli ADR-029): Keep writes the file at once (0600, atomically:
// a temp file beside it renamed over it, so a relay never reads half a
// token), and every Every the bearer is asked for again and every file
// whose bearer changed is rewritten. It is the harnesses'
// harness.TokenKeeper; an adopted session's file is kept the same way
// from the path its state.json names (ADR-009's amendment).
type TokenFiles struct {
	// Token is the bearer to write: gateway.Client.Bearer, which refreshes
	// the rotating machine credential near its expiry.
	Token func(ctx context.Context) (string, error)
	// Every is the refresh interval; zero is TokenRefreshEvery.
	Every time.Duration
	// Ticker, when set, is each kept file's clock in place of
	// time.NewTicker: its channel and its stop. A test drives the
	// refresh on a fake clock with it.
	Ticker func(every time.Duration) (<-chan time.Time, func())
	// Logf, when set, hears a refresh that failed.
	Logf func(format string, args ...any)
	// MultiOrg, when set and true, says the node serves several orgs: a
	// build session with no bearer of its own bound (Bind) is refused
	// the machine bearer, which reads every served org's MCP data
	// (mesh-cli ADR-035).
	MultiOrg func() bool

	mu       sync.Mutex
	files    map[string]*keptToken
	sessions map[sessionKey]TokenSource
	// keys is the DPoP key each bound session's tokens are bound to.
	keys map[sessionKey]*dpop.Key
}

// TokenSource answers a bearer for a token file.
type TokenSource func(ctx context.Context) (string, error)

// sessionKey names one claim: its item and the attempts it was claimed at.
type sessionKey struct {
	item    string
	attempt int
}

// ErrMachineBearerRefused is KeepSession's refusal of the machine
// bearer to a build of a node serving several orgs (ADR-035).
var ErrMachineBearerRefused = errors.New("a node serving several orgs gives no session the machine bearer")

// keptToken is one kept file: the bearer last written, its source and
// its refresher.
type keptToken struct {
	written string
	src     TokenSource
	// bound is the claim whose bearer src is; zero for the machine's.
	bound sessionKey
	stop  chan struct{}
	done  chan struct{}
}

// Bind makes src the bearer of the session of item's claim at attempt —
// its own mesh session's (ADR-035) — for every file KeepSession keeps
// for it from now on. The returned unbind clears it: the session's
// bearer is forgotten, and a file still kept for it stops refreshing.
// key, when set, is the DPoP key src's tokens are bound to (KeyOf): the
// session's proxy signs every request with it.
func (t *TokenFiles) Bind(item string, attempt int, src TokenSource, dk *dpop.Key) (unbind func()) {
	key := sessionKey{item, attempt}
	t.mu.Lock()
	if t.sessions == nil {
		t.sessions = map[sessionKey]TokenSource{}
		t.keys = map[sessionKey]*dpop.Key{}
	}
	t.sessions[key] = src
	if dk != nil {
		t.keys[key] = dk
	}
	t.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			delete(t.sessions, key)
			delete(t.keys, key)
			for _, k := range t.files {
				if k.bound == key {
					k.src = unboundSource
				}
			}
			t.mu.Unlock()
		})
	}
}

// KeyOf is the DPoP key the bound session of meta's claim signs with,
// nil when its bearer is not bound to one (the machine's, an unbound
// mint).
func (t *TokenFiles) KeyOf(meta harness.SessionMeta) *dpop.Key {
	if t == nil || meta.ItemID == "" {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.keys[sessionKey{meta.ItemID, meta.Attempt}]
}

// Bound says whether a session bearer is bound for item's claim at
// attempt.
func (t *TokenFiles) Bound(item string, attempt int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessions[sessionKey{item, attempt}] != nil
}

// unboundSource is a file's source once its session's bearer was
// cleared: no bearer, so the file is never rewritten with another.
func unboundSource(context.Context) (string, error) {
	return "", errors.New("the session's bearer was cleared")
}

// KeepSession implements harness.SessionTokenKeeper: path is kept from
// the bearer bound for meta's claim (Bind) when there is one; else a
// build of a node serving several orgs is refused
// (ErrMachineBearerRefused), and any other session is kept from the
// machine bearer as Keep keeps it.
func (t *TokenFiles) KeepSession(path string, kind harness.Kind, meta harness.SessionMeta) (func(remove bool), error) {
	if t == nil {
		return nil, errors.New("no token source")
	}
	key := sessionKey{meta.ItemID, meta.Attempt}
	t.mu.Lock()
	src := t.sessions[key]
	t.mu.Unlock()
	if src == nil || meta.ItemID == "" {
		if kind == harness.KindBuild && t.MultiOrg != nil && t.MultiOrg() {
			return nil, fmt.Errorf("item %s: no mesh session bearer bound: %w", meta.ItemID, ErrMachineBearerRefused)
		}
		return t.Keep(path)
	}
	return t.keep(path, src, key)
}

// Source is the bearer KeepSession would keep a file from for the
// session kind and meta name, held in memory rather than written: the
// bound session's (Bind), else the machine's (Token) — refused, as
// KeepSession refuses it, to a build of a node serving several orgs. A
// session's sessionproxy reaches runsheet on it, so no token file is
// written anywhere its pod mounts (mesh-cli goal
// a-session-reaches-every-mcp-server-through-one-proxy). The bound
// source is read through on every call, so an unbind (the session's
// end) stops it giving a bearer.
func (t *TokenFiles) Source(kind harness.Kind, meta harness.SessionMeta) (TokenSource, error) {
	if t == nil {
		return nil, errors.New("no token source")
	}
	key := sessionKey{meta.ItemID, meta.Attempt}
	t.mu.Lock()
	_, bound := t.sessions[key]
	t.mu.Unlock()
	if bound && meta.ItemID != "" {
		return func(ctx context.Context) (string, error) {
			t.mu.Lock()
			src := t.sessions[key]
			t.mu.Unlock()
			if src == nil {
				return unboundSource(ctx)
			}
			return src(ctx)
		}, nil
	}
	if kind == harness.KindBuild && t.MultiOrg != nil && t.MultiOrg() {
		return nil, fmt.Errorf("item %s: no mesh session bearer bound: %w", meta.ItemID, ErrMachineBearerRefused)
	}
	if t.Token == nil {
		return nil, errors.New("no token source")
	}
	return t.Token, nil
}

// Keep implements harness.TokenKeeper: path kept from the machine
// bearer, Token.
func (t *TokenFiles) Keep(path string) (func(remove bool), error) {
	if t == nil || t.Token == nil {
		return nil, errors.New("no token source")
	}
	return t.keep(path, t.Token, sessionKey{})
}

func (t *TokenFiles) keep(path string, src TokenSource, bound sessionKey) (func(remove bool), error) {
	tok, err := sourceToken(src)
	if err != nil {
		return nil, err
	}
	if err := writeTokenFile(path, tok, true); err != nil {
		return nil, err
	}
	k := &keptToken{written: tok, src: src, bound: bound, stop: make(chan struct{}), done: make(chan struct{})}
	t.mu.Lock()
	if t.files == nil {
		t.files = map[string]*keptToken{}
	}
	// a second Keep of one path takes it over: the first's refresher
	// stops writing, its release removes nothing of the second's
	t.files[path] = k
	t.mu.Unlock()
	// the ticker opens before Keep returns, so a tick from then on is
	// never missed for want of a refresher listening
	every := t.Every
	if every <= 0 {
		every = TokenRefreshEvery
	}
	ticks, stopTicks := t.ticker(every)
	go t.refresh(path, k, ticks, stopTicks)
	var once sync.Once
	return func(remove bool) {
		once.Do(func() {
			close(k.stop)
			<-k.done
			t.mu.Lock()
			if t.files[path] == k {
				delete(t.files, path)
			}
			t.mu.Unlock()
			if remove && !t.kept(path) {
				os.Remove(path)
			}
		})
	}, nil
}

// Kept is the paths kept now, for a test and the node's status.
func (t *TokenFiles) Kept() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.files))
	for p := range t.files {
		out = append(out, p)
	}
	return out
}

func (t *TokenFiles) kept(path string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.files[path] != nil
}

// Refresh asks each kept file's source for its bearer once and
// rewrites every file whose bearer changed: what each file's ticker
// does every Every. A file is only ever rewritten from its own source —
// a session's bearer never lands in another session's file.
func (t *TokenFiles) Refresh() error {
	t.mu.Lock()
	paths := make(map[string]*keptToken, len(t.files))
	for p, k := range t.files {
		paths[p] = k
	}
	t.mu.Unlock()
	var errs []error
	for path, k := range paths {
		if err := t.refreshOne(path, k); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// refreshOne asks k's source for its bearer and rewrites path if it
// changed and path is still k's.
func (t *TokenFiles) refreshOne(path string, k *keptToken) error {
	t.mu.Lock()
	src := k.src
	t.mu.Unlock()
	tok, err := sourceToken(src)
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.files[path] != k {
		return nil
	}
	return t.rewrite(path, k, tok)
}

// rewrite writes tok at path unless it is already what the file holds.
// Caller holds t.mu.
func (t *TokenFiles) rewrite(path string, k *keptToken, tok string) error {
	if tok == k.written {
		return nil
	}
	if err := writeTokenFile(path, tok, false); err != nil {
		return err
	}
	k.written = tok
	return nil
}

func (t *TokenFiles) refresh(path string, k *keptToken, ticks <-chan time.Time, stop func()) {
	defer crash.Guard()
	defer close(k.done)
	defer stop()
	for {
		select {
		case <-k.stop:
			return
		case <-ticks:
		}
		err := t.refreshOne(path, k)
		if err != nil && t.Logf != nil {
			t.Logf("runsheet token %s: refresh: %v", path, err)
		}
	}
}

func (t *TokenFiles) ticker(every time.Duration) (<-chan time.Time, func()) {
	if t.Ticker != nil {
		return t.Ticker(every)
	}
	tick := time.NewTicker(every)
	return tick.C, tick.Stop
}

// sourceToken asks src for its bearer, under a 30-second limit.
func sourceToken(src TokenSource) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tok, err := src(ctx)
	if err != nil {
		return "", err
	}
	if tok == "" {
		return "", errors.New("the token source gave an empty bearer")
	}
	return tok, nil
}

// writeTokenFile writes tok at path, 0600, by a rename over it. mkdir
// makes its directory first: Keep's first write does, a refresh never —
// a session directory removed under a kept file is not made again.
func writeTokenFile(path, tok string, mkdir bool) error {
	dir := filepath.Dir(path)
	if mkdir {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	f, err := os.CreateTemp(dir, ".runsheet.token-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.WriteString(tok + "\n")
	cerr := f.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmp, 0o600)); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
