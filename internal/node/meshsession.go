package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// MeshSessionOpener is the gateway's half of a session's own bearer
// (runsheet ADR 056): the open of a build claim's mesh session and the
// mint of its id tokens.
type MeshSessionOpener interface {
	OpenMeshSession(ctx context.Context, req gateway.MeshSessionRequest) (*gateway.MeshSession, error)
	MeshSessionToken(ctx context.Context, s *gateway.MeshSession, audience string) (string, time.Duration, error)
}

// MeshSessionRefresh is how long before its expiry a session's id token
// is minted again: runsheet ADR 056 §6's 120 seconds, covering /mcp's
// skew allowance and one Retry-After.
const MeshSessionRefresh = 120 * time.Second

// SessionBearers gives each build session the runsheet bearer of its
// own mesh session, never the machine's (mesh-cli ADR-035): Open opens
// the claim's session (POST /mesh/v1/sessions) and binds a source
// minting its id tokens in Tokens, so the session's token file — under
// its org's MCP root — holds that session's bearer alone, which /mcp
// narrows to the session's project and refuses once the claim ends.
type SessionBearers struct {
	GW     MeshSessionOpener
	Tokens *TokenFiles
	// Audience is the id tokens' audience: /mcp's URL, the relay's own
	// --url (MeshMCPURL).
	Audience string
	// Orgs is how many orgs the node serves; more than one refuses the
	// machine bearer when the platform opens no mesh sessions.
	Orgs func() int
	Logf func(format string, args ...any)
	// Now is the clock the id tokens' expiry is read on; nil is time.Now.
	Now func() time.Time

	fallbackOnce sync.Once
}

// MachineBearerRefusal is the refusal of a build on a node serving
// several orgs when the platform opens no mesh sessions: the reason the
// item is released with.
type MachineBearerRefusal struct {
	Orgs int
	Err  error
}

func (e *MachineBearerRefusal) Error() string {
	return fmt.Sprintf("mesh session: the platform opens no mesh sessions (POST %s), and a node serving %d orgs gives no session the machine bearer", gateway.MeshSessionsPath, e.Orgs)
}

func (e *MachineBearerRefusal) Unwrap() error { return ErrMachineBearerRefused }

// MachineBearerRefusalOf is err's refusal, when it is one.
func MachineBearerRefusalOf(err error) (*MachineBearerRefusal, bool) {
	var r *MachineBearerRefusal
	ok := errors.As(err, &r)
	return r, ok
}

// MeshSessionTokenDir is where a session's token file goes: under its
// org's MCP root, one directory per claim.
func MeshSessionTokenDir(mcpRoot, itemID string, attempt int) string {
	return filepath.Join(mcpRoot, "sessions", itemID+"-"+strconv.Itoa(attempt))
}

// Open opens the mesh session of item's claim held by nodeID and binds
// its bearer for the session's token file (TokenFiles.KeepSession). It
// answers the unbind, which clears the bearer. A platform without the
// route (IsUnsupported) falls back to the machine bearer on a node
// serving one org, said once per run, and is a *MachineBearerRefusal on
// one serving several; any other failure is returned as it is.
func (b *SessionBearers) Open(ctx context.Context, item gateway.WorkItem, nodeID string) (unbind func(), err error) {
	noop := func() {}
	if b == nil || b.GW == nil || b.Tokens == nil {
		return noop, nil
	}
	// the claim's seq is the item's attempts when claimed, as the
	// session's state names its claim (SessionMeta.Attempt)
	s, err := b.GW.OpenMeshSession(ctx, gateway.MeshSessionRequest{ItemID: item.ID, ClaimSeq: item.Attempts, NodeID: nodeID})
	if gateway.IsUnsupported(err) {
		if n := b.orgs(); n > 1 {
			return noop, &MachineBearerRefusal{Orgs: n, Err: err}
		}
		b.fallbackOnce.Do(func() {
			b.logf("the platform opens no mesh sessions; a session's runsheet reads on the node's bearer (one org served)")
		})
		return noop, nil
	}
	if err != nil {
		if !gateway.IsRefusal(err) {
			err = fmt.Errorf("mesh session: %w", err)
		}
		return noop, err
	}
	src := b.source(s)
	// the first id token now: a session whose lease the platform will
	// not mint for never starts
	if _, err := src(ctx); err != nil {
		return noop, fmt.Errorf("%s: id token: %w", s, err)
	}
	return b.Tokens.Bind(item.ID, item.Attempts, src), nil
}

// source mints s's id tokens for Audience, each kept until
// MeshSessionRefresh before it expires.
func (b *SessionBearers) source(s *gateway.MeshSession) TokenSource {
	var mu sync.Mutex
	var tok string
	var until time.Time
	return func(ctx context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		now := b.now()
		if tok != "" && now.Before(until) {
			return tok, nil
		}
		sent := now
		t, life, err := b.GW.MeshSessionToken(ctx, s, b.Audience)
		if err != nil {
			return "", err
		}
		tok, until = t, sent.Add(life-MeshSessionRefresh)
		return tok, nil
	}
}

// RemoveTokenDir removes a session's token directory once its end is
// handled.
func RemoveTokenDir(dir string) {
	if dir != "" {
		os.RemoveAll(dir)
	}
}

func (b *SessionBearers) orgs() int {
	if b.Orgs == nil {
		return 1
	}
	return b.Orgs()
}

func (b *SessionBearers) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func (b *SessionBearers) logf(format string, args ...any) {
	if b.Logf != nil {
		b.Logf(format, args...)
	}
}

// ServedOrgs is how many orgs the loop serves now, read under its lock:
// SessionBearers.Orgs and TokenFiles.MultiOrg as the orgs change.
func (l *Loop) ServedOrgs() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.Scopes)
}
