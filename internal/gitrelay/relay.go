// Package gitrelay is the node's git smart-HTTP relay (runsheet ADR 076):
// a session's checkout fetches and pushes through it, the relay adds the
// item's GitHub token on the way out, and the job's git policy decides
// what it may reach. No GitHub token is in the session's env, disk or
// git config.
//
// It serves /git/<owner>/<repo>.git/{info/refs,git-upload-pack,
// git-receive-pack} and streams both ways: a pack is never buffered.
package gitrelay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
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

// Policy is one job's git policy, from runsheet's claim answer (ADR 076
// decision 3).
type Policy struct {
	// Repo is owner/name, the only repository the job reaches.
	Repo string
	// Access is read or write; read refuses git-receive-pack.
	Access Access
	// PushRef is the branch a push may update, refs/heads/<branch>;
	// beside it a push may only create pinned changeset refs
	// (ChangesetRefPrefix).
	// Whether the update may be a non-fast-forward is left to git and
	// GitHub's branch rules; what a pushed head holds is judged by
	// runsheet against the changeset's tree (ADR 076 decision 5).
	PushRef string
}

// Token is the job's GitHub token and when it expires.
type Token struct {
	Value     string
	ExpiresAt time.Time
}

// Event is one git request the relay answered, for runsheet's audit
// (ADR 076 decision 7).
type Event struct {
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
	// Upstream is GitHub's git origin; https://github.com when empty.
	Upstream string
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

// returned are the only upstream headers sent back.
var returned = []string{"Content-Type", "Cache-Control", "Expires", "Pragma"}

// ServeHTTP relays one git request after checking it against the policy.
func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	repo, rest, ok := splitPath(req.URL.Path)
	if !ok {
		http.Error(w, "gitrelay: not a git path", http.StatusNotFound)
		return
	}
	service := serviceOf(req, rest)
	ev := Event{Repo: repo, Service: service, At: r.now()}
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
		pins := 0
		for _, c := range cmds {
			ev.Refs = append(ev.Refs, c.ref)
			if reason := r.Policy.check(c); reason != "" {
				refuse(reason)
				return
			}
			if strings.HasPrefix(c.ref, ChangesetRefPrefix) {
				pins++
			}
		}
		if pins > 1 {
			refuse("a push pins one changeset, not several")
			return
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
	out.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:"+tok)))
	resp, err := r.client().Do(out)
	if err != nil {
		ev.Outcome, ev.Reason = "failed", strings.ReplaceAll(err.Error(), tok, "***")
		r.audit(ev)
		http.Error(w, "gitrelay: "+ev.Reason, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range returned {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(flushWriter{w}, resp.Body)
	ev.Bytes = n
	ev.Outcome = "ok"
	if resp.StatusCode/100 != 2 {
		ev.Outcome, ev.Reason = "failed", fmt.Sprintf("GitHub answered %d", resp.StatusCode)
	}
	r.audit(ev)
}

// splitPath reads /git/<owner>/<repo>.git<rest>.
func splitPath(p string) (repo, rest string, ok bool) {
	p, ok = strings.CutPrefix(p, Prefix)
	if !ok {
		return "", "", false
	}
	i := strings.Index(p, ".git/")
	if i <= 0 {
		return "", "", false
	}
	repo, rest = p[:i], p[i+len(".git"):]
	if strings.Count(repo, "/") != 1 || strings.HasPrefix(repo, "/") || strings.HasSuffix(repo, "/") || strings.Contains(repo, "..") {
		return "", "", false
	}
	return repo, rest, true
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

// ChangesetRefPrefix is where a job pins the commit of a changeset it
// records (runsheet ADR 076 decision 5): refs/runsheet/changesets/<id>,
// so the commit is never collected and runsheet reads the changeset's
// content from git.
const ChangesetRefPrefix = "refs/runsheet/changesets/"

// check refuses a ref update the policy does not allow: a push may
// update the job's branch and create pinned changeset refs, nothing
// else, and may delete neither.
func (p Policy) check(c command) string {
	if strings.Trim(c.new, "0") == "" {
		return "this job may not delete " + c.ref
	}
	if c.ref == p.PushRef {
		return ""
	}
	if id, ok := strings.CutPrefix(c.ref, ChangesetRefPrefix); ok {
		if !isUUID(id) {
			return "a changeset ref names a changeset id: " + c.ref
		}
		if strings.Trim(c.old, "0") != "" {
			return "a changeset ref is pinned once and never moved: " + c.ref
		}
		return ""
	}
	return fmt.Sprintf("this job may push only %s and %s<changeset>, not %s", p.PushRef, ChangesetRefPrefix, c.ref)
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdef", r) {
				return false
			}
		}
	}
	return true
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
	return "https://github.com"
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
