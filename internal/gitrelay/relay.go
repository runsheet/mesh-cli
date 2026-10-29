// Package gitrelay is the node's git smart-HTTP relay:
// a session's checkout fetches and pushes through it, the relay adds the
// job's token on the way out in its vendor's shape (vendor.go), and the
// job's git policy decides what it may reach. No vendor token is in the
// session's env, disk or git config, and in Mesh v1 every write to a
// vendor goes through it.
//
// It serves /git/<owner>/<repo>.git/{info/refs,git-upload-pack,
// git-receive-pack} and streams both ways: a pack is never buffered.
package gitrelay

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Prefix is the path every repository is served under.
const Prefix = "/git/"

// Access is what a job may do to its repository.
type Access string

const (
	Read  Access = "read"
	Write Access = "write"
)

// Policy is one job's git policy as the product's claim names it, opaque
// to the relay: which repository, read or write, and which refs a push
// may touch. The relay knows nothing of what a ref means; it enforces
// these and nothing else.
type Policy struct {
	// Repo is owner/name, the only repository the job reaches.
	Repo string
	// Access is read or write; read refuses git-receive-pack.
	Access Access
	// Push are the refs a push may update; anything else is refused, and
	// so is any deletion. A rule that is FastForwardOnly refuses a
	// non-fast-forward itself; one that is not leaves it to git and the
	// vendor's own branch rules (the legacy node's, which names none).
	Push []RefRule
	// Vendor is the git vendor the job reaches (VendorNamed): GitHub
	// when empty.
	Vendor string
	// Base is the vendor's git origin for this job — a self-hosted
	// GitLab's, say — when it is not the vendor's own (Vendor.Origin).
	Base string
}

// RefRule is one ref a push may touch: an exact ref, or a prefix ending
// "/*" for any ref under it; CreateOnly allows creating it but never
// moving one that exists; FastForwardOnly allows moving it only to a
// descendant of where it is, which the relay proves in the pushing
// checkout (fastforward.go). Mesh v1's briefs default it true.
type RefRule struct {
	Ref             string
	CreateOnly      bool
	FastForwardOnly bool
}

func (r RefRule) matches(ref string) bool {
	if prefix, ok := strings.CutSuffix(r.Ref, "*"); ok && strings.HasSuffix(prefix, "/") {
		return strings.HasPrefix(ref, prefix) && len(ref) > len(prefix)
	}
	return ref == r.Ref
}

// Token is the job's GitHub token and when it expires.
type Token struct {
	Value     string
	ExpiresAt time.Time
}

// Event is one git request the relay answered, for the node's audit.
type Event struct {
	// Lease is the lease whose grant answered it (Server.Open); empty
	// for a token-keyed registration's.
	Lease string
	// Vendor is the git vendor the relay reached (Vendor.Name).
	Vendor  string
	Repo    string
	Service string // upload-pack or receive-pack
	Refs    []string
	Outcome string // ok, refused or failed
	Reason  string
	Bytes   int64
	At      time.Time
}

// Relay is one job's git relay.
type Relay struct {
	// Policy is the job's git policy.
	Policy Policy
	// Mint returns a fresh token for the job; the relay caches it until
	// RefreshBefore of its expiry.
	Mint func(ctx context.Context) (Token, error)
	// Upstream is the vendor's git origin; the Vendor's own when empty.
	Upstream string
	// Vendor is the adapter that puts the token on the request going
	// upstream and filters what comes back; GitHub when nil.
	Vendor Vendor
	// IsAncestor proves a FastForwardOnly rule's update is a
	// fast-forward; with none, such an update of an existing ref is
	// refused.
	IsAncestor Ancestry
	// HTTP reaches Upstream; a client with no timeout when nil, since a
	// clone may stream for as long as it takes.
	HTTP *http.Client
	// Audit takes one event per request; dropped when nil.
	Audit func(Event)
	// RefreshBefore is how long before expiry a cached token is
	// replaced; ten minutes when zero.
	RefreshBefore time.Duration
	// Now is the clock; time.Now when nil.
	Now func() time.Time

	mu    sync.Mutex
	token Token
}

// forwarded are the only client headers sent upstream: what git's smart
// HTTP needs. Authorization and anything else never leave the relay.
var forwarded = []string{"Content-Type", "Accept", "Content-Encoding", "Git-Protocol", "User-Agent"}

// ServeHTTP relays one git request after checking it against the policy.
func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	repo, rest, ok := splitPath(req.URL.Path)
	if !ok {
		http.Error(w, "gitrelay: not a git path", http.StatusNotFound)
		return
	}
	service := serviceOf(req, rest)
	vendor := r.vendor()
	ev := Event{Vendor: vendor.Name(), Repo: repo, Service: service, At: r.now()}
	refuse := func(reason string) {
		ev.Outcome, ev.Reason = "refused", reason
		r.audit(ev)
		http.Error(w, "gitrelay: "+reason, http.StatusForbidden)
	}
	if !strings.EqualFold(repo, r.Policy.Repo) {
		refuse(fmt.Sprintf("repository %s is not this job's (%s)", repo, r.Policy.Repo))
		return
	}
	switch service {
	case "upload-pack":
	case "receive-pack":
		if r.Policy.Access != Write {
			refuse("this job may only read " + repo)
			return
		}
	default:
		refuse("not a git smart-HTTP request")
		return
	}

	body := io.Reader(req.Body)
	if req.Body == nil {
		body = http.NoBody
	}
	if service == "receive-pack" && req.Method == http.MethodPost {
		cmds, replay, err := readCommands(body, req.Header.Get("Content-Encoding"))
		if err != nil {
			refuse("reading the push: " + err.Error())
			return
		}
		for _, c := range cmds {
			ev.Refs = append(ev.Refs, c.ref)
			if reason := r.check(req.Context(), c); reason != "" {
				refuse(reason)
				return
			}
		}
		body = replay
	}

	tok, err := r.tokenFor(req.Context())
	if err != nil {
		ev.Outcome, ev.Reason = "failed", "token: "+err.Error()
		r.audit(ev)
		http.Error(w, "gitrelay: no token for "+repo+": "+err.Error(), http.StatusBadGateway)
		return
	}
	out, err := http.NewRequestWithContext(req.Context(), req.Method, r.upstream()+"/"+repo+".git"+rest, body)
	if err != nil {
		http.Error(w, "gitrelay: "+err.Error(), http.StatusBadGateway)
		return
	}
	out.URL.RawQuery = req.URL.RawQuery
	// the replayed push is the same bytes as the request, so a known
	// length still holds; an unknown one streams chunked, as git does
	out.ContentLength = req.ContentLength
	for _, h := range forwarded {
		if v := req.Header.Get(h); v != "" {
			out.Header.Set(h, v)
		}
	}
	vendor.Authorize(out, tok)
	resp, err := r.client().Do(out)
	if err != nil {
		ev.Outcome, ev.Reason = "failed", strings.ReplaceAll(err.Error(), tok, "***")
		r.audit(ev)
		http.Error(w, "gitrelay: "+ev.Reason, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range vendor.Returned() {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(flushWriter{w}, resp.Body)
	ev.Bytes = n
	ev.Outcome = "ok"
	if resp.StatusCode/100 != 2 {
		ev.Outcome, ev.Reason = "failed", fmt.Sprintf("%s answered %d", vendor.Name(), resp.StatusCode)
	}
	r.audit(ev)
}

// splitPath reads /git/<owner>/<repo>[.git]<rest>: git asks for a remote
// named with or without .git, and GitHub answers both.
func splitPath(p string) (repo, rest string, ok bool) {
	p, ok = strings.CutPrefix(p, Prefix)
	if !ok {
		return "", "", false
	}
	parts := strings.SplitN(p, "/", 3)
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	owner, name := parts[0], strings.TrimSuffix(parts[1], ".git")
	if name == "" || strings.Contains(owner+name, "..") || owner == "." || name == "." {
		return "", "", false
	}
	return owner + "/" + name, "/" + parts[2], true
}

// serviceOf names the git service a request is for: upload-pack (fetch,
// clone) or receive-pack (push).
func serviceOf(req *http.Request, rest string) string {
	switch {
	case rest == "/info/refs" && req.Method == http.MethodGet:
		return strings.TrimPrefix(req.URL.Query().Get("service"), "git-")
	case rest == "/git-upload-pack" && req.Method == http.MethodPost:
		return "upload-pack"
	case rest == "/git-receive-pack" && req.Method == http.MethodPost:
		return "receive-pack"
	}
	return ""
}

type command struct {
	old, new, ref string
}

const zeroID = "0000000000000000000000000000000000000000"

// check refuses a ref update the policy does not name, any deletion,
// moving a create-only ref, and a fast-forward-only ref's update that is
// not proved a fast-forward.
func (r *Relay) check(ctx context.Context, c command) string {
	rule, reason := r.Policy.check(c)
	if reason != "" || !rule.FastForwardOnly || strings.Trim(c.old, "0") == "" {
		return reason
	}
	if r.IsAncestor == nil {
		return c.ref + " may only fast-forward, and this relay cannot prove one"
	}
	if err := r.IsAncestor(ctx, c.old, c.new); err != nil {
		return c.ref + " may only fast-forward: " + err.Error()
	}
	return ""
}

// check answers the rule that allows c, or why none does: c deletes a
// ref, names one no rule names, or moves a create-only one.
func (p Policy) check(c command) (RefRule, string) {
	if strings.Trim(c.new, "0") == "" {
		return RefRule{}, "this job may not delete " + c.ref
	}
	for _, r := range p.Push {
		if !r.matches(c.ref) {
			continue
		}
		if r.CreateOnly && strings.Trim(c.old, "0") != "" {
			return r, c.ref + " may be created, never moved"
		}
		return r, ""
	}
	return RefRule{}, "this job may not push " + c.ref
}

// readCommands reads a receive-pack request's command list (pkt-lines up
// to the first flush) and returns a reader that replays the whole body.
func readCommands(body io.Reader, encoding string) ([]command, io.Reader, error) {
	if encoding != "" && encoding != "identity" {
		return nil, nil, fmt.Errorf("a %s-encoded push is not read; push without compression", encoding)
	}
	br := bufio.NewReader(body)
	var head bytes.Buffer
	var cmds []command
	for {
		var lenHex [4]byte
		if _, err := io.ReadFull(br, lenHex[:]); err != nil {
			return nil, nil, fmt.Errorf("pkt-line length: %w", err)
		}
		head.Write(lenHex[:])
		var n int
		if _, err := fmt.Sscanf(string(lenHex[:]), "%04x", &n); err != nil {
			return nil, nil, fmt.Errorf("pkt-line length %q", lenHex)
		}
		if n == 0 {
			break // flush: the command list ends
		}
		if n < 4 || n > 65520 {
			return nil, nil, fmt.Errorf("pkt-line length %d", n)
		}
		line := make([]byte, n-4)
		if _, err := io.ReadFull(br, line); err != nil {
			return nil, nil, fmt.Errorf("pkt-line: %w", err)
		}
		head.Write(line)
		s := string(line)
		if i := strings.IndexByte(s, 0); i >= 0 {
			s = s[:i] // capabilities follow the first command
		}
		f := strings.Fields(strings.TrimSpace(s))
		if len(f) == 3 && f[0] != "shallow" {
			cmds = append(cmds, command{old: f[0], new: f[1], ref: f[2]})
		}
	}
	if len(cmds) == 0 {
		return nil, nil, errors.New("no ref update in the push")
	}
	return cmds, io.MultiReader(&head, br), nil
}

func (r *Relay) tokenFor(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	before := r.RefreshBefore
	if before == 0 {
		before = 10 * time.Minute
	}
	if r.token.Value != "" && r.now().Add(before).Before(r.token.ExpiresAt) {
		return r.token.Value, nil
	}
	if r.Mint == nil {
		return "", errors.New("no token source")
	}
	t, err := r.Mint(ctx)
	if err != nil {
		return "", err
	}
	if t.Value == "" {
		return "", errors.New("the token source answered no token")
	}
	r.token = t
	return t.Value, nil
}

func (r *Relay) upstream() string {
	if r.Upstream != "" {
		return strings.TrimRight(r.Upstream, "/")
	}
	return strings.TrimRight(r.vendor().Origin(), "/")
}

func (r *Relay) vendor() Vendor {
	if r.Vendor != nil {
		return r.Vendor
	}
	return GitHub{}
}

func (r *Relay) client() *http.Client {
	if r.HTTP != nil {
		return r.HTTP
	}
	return &http.Client{}
}

func (r *Relay) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Relay) audit(ev Event) {
	if r.Audit != nil {
		r.Audit(ev)
	}
}

type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}
