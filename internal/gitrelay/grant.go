package gitrelay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"
)

// A Grant is one lease's git relay on the node's Server (Mesh v1 spec,
// "The git relay and the Latchkey token relay"): the job actor opens it
// while preparing and closes it while releasing. Its capability is keyed
// by the lease, not by a token, so it is stable for the lease's whole
// life while the relay refreshes the token behind it through Mint. Close
// is the first of the three layers that end a lease's tokens: the
// capability answers 404 at once, every stream in flight is cut, and
// Revoke is called once with every token the grant minted.

// Mint answers a fresh token for a grant's repository: Latchkey's
// for-job source mint on the lease's session token, in nodev1.
type Mint func(ctx context.Context) (Token, error)

// Revoke ends the tokens a grant minted, once, when it closes. minted
// may be empty: a grant whose checkout never ran minted nothing.
type Revoke func(ctx context.Context, minted []Token) error

// grantRefreshBefore is how long before expiry a grant's relay replaces
// its token: a for-job source token lives as long as GitHub's
// installation token, and two minutes leaves a long fetch room to start.
const grantRefreshBefore = 2 * time.Minute

// closeWait bounds how long Close waits for the streams it cut to
// return before it revokes; a stalled client write cannot hold the
// revoke back.
const closeWait = 10 * time.Second

// revokeTimeout bounds the Revoke call itself.
const revokeTimeout = 30 * time.Second

// Grant is one lease's registration on the Server.
type Grant struct {
	// Lease is the lease the grant serves.
	Lease string

	server     *Server
	capability string
	policy     Policy
	relay      *Relay
	revoke     Revoke

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	closed   bool
	inflight sync.WaitGroup
	minted   []Token
	checkout string

	once     sync.Once
	closeErr error
}

// Open serves policy for leaseID until the grant is closed, minting its
// token through mint and revoking what it minted through revoke. The
// same lease opened again with the same policy answers the same grant;
// with another policy it is refused, since a lease's git policy is the
// brief's and never moves.
func (s *Server) Open(leaseID string, policy Policy, mint Mint, revoke Revoke) (*Grant, error) {
	if strings.TrimSpace(leaseID) == "" {
		return nil, errors.New("gitrelay: a grant needs a lease")
	}
	if mint == nil {
		return nil, errors.New("gitrelay: a grant needs a token source")
	}
	vendor, err := VendorNamed(policy.Vendor)
	if err != nil {
		return nil, err
	}
	if _, err := vendor.RepoPath(policy.Repo); err != nil {
		return nil, err
	}
	upstream, err := upstreamOf(s.Upstream, policy.Base)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil, errors.New("gitrelay: the server is not started")
	}
	for _, e := range s.entries {
		if e.grant == nil || e.grant.Lease != leaseID {
			continue
		}
		if !reflect.DeepEqual(e.grant.policy, policy) {
			return nil, fmt.Errorf("gitrelay: lease %s already holds a grant for another policy", leaseID)
		}
		return e.grant, nil
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	g := &Grant{Lease: leaseID, server: s, capability: hex.EncodeToString(b[:]), policy: policy, revoke: revoke}
	g.ctx, g.cancel = context.WithCancel(context.Background())
	g.relay = &Relay{
		Policy: policy, Upstream: upstream, Vendor: vendor, HTTP: s.HTTP, Now: s.Now,
		RefreshBefore: grantRefreshBefore,
		IsAncestor: func(ctx context.Context, old, new string) error {
			g.mu.Lock()
			dir := g.checkout
			g.mu.Unlock()
			return CheckoutAncestry(dir)(ctx, old, new)
		},
		Audit: func(e Event) {
			e.Lease = leaseID
			s.audit(e)
		},
		Mint: func(ctx context.Context) (Token, error) {
			t, err := mint(ctx)
			if err == nil && t.Value != "" {
				g.mu.Lock()
				g.minted = append(g.minted, t)
				g.mu.Unlock()
			}
			return t, err
		},
	}
	s.entries[g.capability] = &entry{grant: g}
	return g, nil
}

// Grant answers the open grant of leaseID, nil when it has none.
func (s *Server) Grant(leaseID string) *Grant {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.grant != nil && e.grant.Lease == leaseID {
			return e.grant
		}
	}
	return nil
}

// Leases are the leases holding an open grant, for the supervisor's
// reconcile to find a grant no actor owns.
func (s *Server) Leases() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.entries {
		if e.grant != nil {
			out = append(out, e.grant.Lease)
		}
	}
	return out
}

// Base is the URL a git command's remote names for the grant's
// repository: http://127.0.0.1:<port>/j/<capability>/git/, the same for
// the lease's whole life.
func (g *Grant) Base() string {
	g.server.mu.Lock()
	defer g.server.mu.Unlock()
	if g.server.ln == nil {
		return ""
	}
	return g.server.base(g.capability)
}

// Policy is the grant's git policy.
func (g *Grant) Policy() Policy { return g.policy }

// SetCheckout names the lease's checkout, where the relay proves a
// fast-forward-only push is one (fastforward.go): the job actor sets it
// once its checkout through the grant is made. Until then a push that
// moves a fast-forward-only ref is refused.
func (g *Grant) SetCheckout(dir string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.checkout = dir
}

// upstreamOf is a grant's git origin: the server's when it names one
// (`--git-upstream`, a test's override for every lease), else the
// policy's base, else "" for the vendor's own. A base is an absolute
// http(s) URL.
func upstreamOf(override, base string) (string, error) {
	if override != "" {
		return override, nil
	}
	base = strings.TrimSpace(base)
	if base == "" {
		return "", nil
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("gitrelay: the git base %q is not an http(s) origin", base)
	}
	return strings.TrimRight(base, "/"), nil
}

// ServeHTTP serves /git/<owner>/<repo>.git/… for the grant: what the
// session's sessionproxy hands its /git/ route to, as the loopback
// capability serves the node's own commands. A closed grant answers 404.
func (g *Grant) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		http.Error(w, "gitrelay: this lease's relay is closed", http.StatusNotFound)
		return
	}
	g.inflight.Add(1)
	g.mu.Unlock()
	defer g.inflight.Done()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(g.ctx, cancel)
	defer stop()
	g.relay.ServeHTTP(w, r.WithContext(ctx))
}

// Closed says whether Close has run.
func (g *Grant) Closed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closed
}

// Close removes the grant's capability, so it answers 404, cuts every
// stream in flight, and calls Revoke once with every token it minted.
// Closing again answers the first close's error and does nothing more.
func (g *Grant) Close() error {
	g.once.Do(func() {
		g.server.mu.Lock()
		delete(g.server.entries, g.capability)
		g.server.mu.Unlock()
		g.mu.Lock()
		g.closed = true
		g.mu.Unlock()
		g.cancel()
		done := make(chan struct{})
		go func() { g.inflight.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(closeWait):
		}
		if g.revoke == nil {
			return
		}
		g.mu.Lock()
		minted := append([]Token(nil), g.minted...)
		g.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), revokeTimeout)
		defer cancel()
		if err := g.revoke(ctx, minted); err != nil {
			g.closeErr = fmt.Errorf("gitrelay: revoking lease %s's git tokens: %w", g.Lease, err)
		}
	})
	return g.closeErr
}
