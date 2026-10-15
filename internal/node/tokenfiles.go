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
)

// TokenRefreshEvery is how often TokenFiles asks for the bearer and
// rewrites each kept file it changed for: a quarter of the margin the
// machine credential refreshes at (auth.RefreshSkew, auth.NeedsRefresh),
// so the first tick after the credential renews falls at least three
// quarters of that margin before the old bearer's expiry, and a file
// never holds a bearer past its life (mesh-cli ADR-029).
const TokenRefreshEvery = auth.RefreshSkew / 4

// TokenFiles keeps every running session's mesh-context token file —
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

	mu    sync.Mutex
	files map[string]*keptToken
}

// keptToken is one kept file: the bearer last written and its refresher.
type keptToken struct {
	written string
	stop    chan struct{}
	done    chan struct{}
}

// Keep implements harness.TokenKeeper.
func (t *TokenFiles) Keep(path string) (func(remove bool), error) {
	if t == nil || t.Token == nil {
		return nil, errors.New("no token source")
	}
	tok, err := t.token()
	if err != nil {
		return nil, err
	}
	if err := writeTokenFile(path, tok, true); err != nil {
		return nil, err
	}
	k := &keptToken{written: tok, stop: make(chan struct{}), done: make(chan struct{})}
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

// Refresh asks for the bearer once and rewrites every kept file it
// changed for: what each file's ticker does every Every.
func (t *TokenFiles) Refresh() error {
	tok, err := t.token()
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var errs []error
	for path, k := range t.files {
		if err := t.rewrite(path, k, tok); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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
		tok, err := t.token()
		if err == nil {
			t.mu.Lock()
			if t.files[path] == k {
				err = t.rewrite(path, k, tok)
			}
			t.mu.Unlock()
		}
		if err != nil && t.Logf != nil {
			t.Logf("mesh-context token %s: refresh: %v", path, err)
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

func (t *TokenFiles) token() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tok, err := t.Token(ctx)
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
	f, err := os.CreateTemp(dir, ".mesh-context.token-*")
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
