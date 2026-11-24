package nodev1

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// A node runs each job kind on the harness its route names (goal
// a-node-runs-each-kind-on-the-harness-its-route-names; ADR-011's
// amendment of 2026-11-24, which replaces its decision 1). node.yaml's
// `v1.harness_routes` is a list of routes:
//
//	v1:
//	  harness_routes:
//	    - {kind: build, harness: codex}
//	    - {kind: evaluate, tenant: grapevine, harness: claude-code}
//	    - {kind: deploy, repo: runsheet/runsheet, harness: either}
//
// A route names a kind (its short name, or * for every kind), and
// optionally a tenant or a repository (owner/name) it is held to, and
// the harness: claude-code, codex, or either. The node reads the list
// again before every take (live, no restart) and sends it with the take;
// Mesh places a routed job on the routed harness, and for either on the
// first of the job's harnesses whose account has room. A lease Mesh
// gives on a harness outside its route, or on a harness this node cannot
// start, is handed back cannot_run naming the harness; one whose account
// the node last read as limited is handed back model_rate_limited
// naming it too. With no route set the take carries none, and every
// lease runs on the node's --harness session, as before.

// The harnesses a route may name.
const (
	HarnessClaudeCode = "claude-code"
	HarnessCodex      = "codex"
	// HarnessEither is a route's "whichever has an account with room".
	HarnessEither = "either"
)

// harnessesKnown are the harnesses a node runs, in Mesh's order of
// preference for an either route when a kind names both.
var harnessesKnown = []string{HarnessClaudeCode, HarnessCodex}

// HarnessRoute is one entry of node.yaml's v1.harness_routes.
type HarnessRoute struct {
	Kind    string `yaml:"kind"`
	Tenant  string `yaml:"tenant,omitempty"`
	Repo    string `yaml:"repo,omitempty"`
	Harness string `yaml:"harness"`
}

// harnesses are the harnesses the route lets a lease run on.
func (h HarnessRoute) harnesses() []string {
	if h.Harness == HarnessEither {
		return slices.Clone(harnessesKnown)
	}
	return []string{h.Harness}
}

// String names the route as a reason reads it.
func (h HarnessRoute) String() string {
	scope := ""
	switch {
	case h.Repo != "":
		scope = " on " + h.Repo
	case h.Tenant != "":
		scope = " for " + h.Tenant
	}
	return fmt.Sprintf("the route for %s%s names %s", h.Kind, scope, h.Harness)
}

type nodeYAMLRoutes struct {
	V1 struct {
		HarnessRoutes []HarnessRoute `yaml:"harness_routes"`
	} `yaml:"v1"`
}

// ReadHarnessRoutes reads node.yaml's v1.harness_routes: none when the
// file does not exist or names none. A route with no kind, or a harness
// other than claude-code, codex or either, refuses the whole list.
func ReadHarnessRoutes(path string) ([]HarnessRoute, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cfg nodeYAMLRoutes
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("node.yaml: %w", err)
	}
	out := make([]HarnessRoute, 0, len(cfg.V1.HarnessRoutes))
	for i, h := range cfg.V1.HarnessRoutes {
		h.Kind = shortKind(strings.TrimSpace(h.Kind))
		h.Tenant = strings.TrimSpace(h.Tenant)
		h.Repo = strings.TrimSpace(h.Repo)
		h.Harness = strings.ToLower(strings.TrimSpace(h.Harness))
		if h.Kind == "" {
			return nil, fmt.Errorf("node.yaml: v1.harness_routes[%d] names no kind", i)
		}
		if h.Tenant != "" && h.Repo != "" {
			return nil, fmt.Errorf("node.yaml: v1.harness_routes[%d] names a tenant and a repo; a route is held to one", i)
		}
		if h.Harness != HarnessEither && !slices.Contains(harnessesKnown, h.Harness) {
			return nil, fmt.Errorf("node.yaml: v1.harness_routes[%d]: harness must be claude-code, codex or either, not %q", i, h.Harness)
		}
		out = append(out, h)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// routeFor is the route a lease of kind in tenant on repo runs by: a
// repository's route before a tenant's before one for every tenant, and
// the kind's own before *'s. False when none matches.
func routeFor(routes []HarnessRoute, kind, tenant, repo string) (HarnessRoute, bool) {
	kind = shortKind(kind)
	best, bestScore := HarnessRoute{}, -1
	for _, h := range routes {
		score := 0
		switch {
		case h.Kind == kind:
			score += 1
		case h.Kind != "*":
			continue
		}
		switch {
		case h.Repo != "":
			if repo == "" || !strings.EqualFold(h.Repo, repo) {
				continue
			}
			score += 4
		case h.Tenant != "":
			if !strings.EqualFold(h.Tenant, tenant) {
				continue
			}
			score += 2
		}
		if score > bestScore {
			best, bestScore = h, score
		}
	}
	return best, bestScore >= 0
}

// shortKind is a kind's own name, without a "<product>/" before it.
func shortKind(kind string) string {
	if i := strings.LastIndex(kind, "/"); i >= 0 {
		return kind[i+1:]
	}
	return kind
}

// takeRoutes are routes as a take carries them.
func takeRoutes(routes []HarnessRoute) []meshapi.TakeRoute {
	if len(routes) == 0 {
		return nil
	}
	out := make([]meshapi.TakeRoute, 0, len(routes))
	for _, h := range routes {
		out = append(out, meshapi.TakeRoute{Kind: h.Kind, Tenant: h.Tenant, Repo: h.Repo, Harnesses: h.harnesses()})
	}
	return out
}

// routesCache is node.yaml's routes as last read, read again when the
// file changes.
type routesCache struct {
	mu      sync.Mutex
	mod     time.Time
	size    int64
	read    bool
	routes  []HarnessRoute
	lastErr string
}

// harnessRoutes are the node's routes now: node.yaml's, read again
// whenever it changed since the last take. A file that does not parse
// keeps the routes last read, logged once per error.
func (r *Runtime) harnessRoutes() []HarnessRoute {
	path := r.HarnessRoutesPath
	if path == "" {
		return nil
	}
	c := &r.routes
	c.mu.Lock()
	defer c.mu.Unlock()
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if c.routes != nil {
				r.logf("nodev1: harness routes: node.yaml is gone; no route is set")
			}
			c.routes, c.read, c.mod, c.size = nil, true, time.Time{}, 0
		}
		return c.routes
	}
	if c.read && fi.ModTime().Equal(c.mod) && fi.Size() == c.size {
		return c.routes
	}
	routes, err := ReadHarnessRoutes(path)
	if err != nil {
		if msg := err.Error(); msg != c.lastErr {
			r.logf("nodev1: harness routes: %v; the routes last read stay in force", err)
			c.lastErr = msg
		}
		return c.routes
	}
	c.lastErr = ""
	if c.read && !sameRoutes(c.routes, routes) || !c.read && len(routes) > 0 {
		r.logf("nodev1: harness routes: %s", describeRoutes(routes))
	}
	c.routes, c.read, c.mod, c.size = routes, true, fi.ModTime(), fi.Size()
	return c.routes
}

func sameRoutes(a, b []HarnessRoute) bool {
	return slices.Equal(a, b)
}

func describeRoutes(routes []HarnessRoute) string {
	if len(routes) == 0 {
		return "none set; every lease runs on the node's own harness"
	}
	parts := make([]string, 0, len(routes))
	for _, h := range routes {
		p := h.Kind
		if h.Repo != "" {
			p += "@" + h.Repo
		} else if h.Tenant != "" {
			p += "@" + h.Tenant
		}
		parts = append(parts, p+" → "+h.Harness)
	}
	return strings.Join(parts, ", ")
}

// takeHarnesses are the harnesses a take offers: the node's registered
// ones, then every harness a route names that the node holds a session
// for.
func (r *Runtime) takeHarnesses(routes []HarnessRoute) []string {
	out := slices.Clone(r.Node.Harnesses)
	for _, h := range routes {
		for _, name := range h.harnesses() {
			if _, ok := r.Sessions[name]; ok && !slices.Contains(out, name) {
				out = append(out, name)
			}
		}
	}
	return out
}

// harnessError is a lease the node will not start on its harness: the
// harness its brief names is outside its route, or one the node cannot
// start, or its account has no headroom. It is handed back with cause,
// the harness named in the reason.
type harnessError struct {
	harness string
	cause   string
	why     string
	wrapped error
}

func (e *harnessError) Error() string { return e.harness + ": " + e.why }

func (e *harnessError) Unwrap() error { return e.wrapped }

// sessionFor is the session a lease runs on: the harness its brief
// names, out of the node's registry, or the node's own session when the
// registry is empty, the brief names no harness or names the node's
// own. A routed lease whose harness is outside its route, or that this
// node has no session for, is a harnessError.
func (r *Runtime) sessionFor(b *meshapi.Brief) (Session, error) {
	name := b.Harness.Name
	route, routed := routeFor(r.harnessRoutes(), b.Kind, b.Tenant, briefRepo(b))
	if name == "" || name == "none" {
		return r.Session, nil
	}
	if routed && !slices.Contains(route.harnesses(), name) {
		return nil, &harnessError{harness: name, cause: meshapi.CauseCannotRun,
			why: fmt.Sprintf("%s, but Mesh leased it on %s", route, name)}
	}
	if s, ok := r.Sessions[name]; ok && s != nil {
		return s, nil
	}
	// no registry and no route: the node's own session, whatever the
	// brief names, as before routes; else its own session only for its
	// own harness
	if len(r.Sessions) == 0 && !routed || slices.Contains(r.Node.Harnesses, name) {
		return r.Session, nil
	}
	why := "this node cannot start it: it holds no " + name + " session"
	if routed {
		why = route.String() + ", and " + why
	}
	return nil, &harnessError{harness: name, cause: meshapi.CauseCannotRun, why: why}
}

// briefRepo is the repository a brief's job works on, owner/name.
func briefRepo(b *meshapi.Brief) string {
	if b.Code != nil && b.Code.Repo.FullName != "" {
		return b.Code.Repo.FullName
	}
	if b.Git != nil {
		return b.Git.Repo
	}
	return ""
}

// noHeadroom is a routed lease whose account the node last read as
// limited — its vendor said it may not run until a time still to come
// — as a harnessError handed back model_rate_limited; nil otherwise, and
// always nil for a lease no route names, which runs as it always has.
func (r *Runtime) noHeadroom(b *meshapi.Brief, ref string, now time.Time) error {
	if ref == "" {
		return nil
	}
	route, routed := routeFor(r.harnessRoutes(), b.Kind, b.Tenant, briefRepo(b))
	if !routed {
		return nil
	}
	r.mu.Lock()
	reading := r.accounts[ref]
	r.mu.Unlock()
	if reading == nil || reading.report.LimitedUntil == nil || !reading.report.LimitedUntil.After(now) {
		return nil
	}
	return &harnessError{harness: b.Harness.Name, cause: meshapi.CauseModelRateLimited,
		why: fmt.Sprintf("%s, and its account %s has no headroom until %s", route, ref, reading.report.LimitedUntil.UTC().Format(time.RFC3339))}
}

// providerOf is the provider a lease's session ran on: its account's
// vendor, or the harness's own when the session ran on no account the
// node knows.
func providerOf(a *sessionAccount, harnessName string) string {
	if a != nil {
		a.mu.Lock()
		p := a.auth.provider
		if a.cred != nil && a.cred.Vendor != "" {
			p = strings.ToLower(a.cred.Vendor)
		}
		a.mu.Unlock()
		if p != "" {
			return p
		}
	}
	if harnessName == HarnessCodex {
		return "openai"
	}
	return "anthropic"
}

// harnessDidNotStart is err, or — for a routed lease whose session
// ended in error before it said anything (no session id, no turn) while
// its context still ran — a harnessError naming the harness, handed back
// cannot_run. The error it wraps is kept for errors.Is.
func (r *Runtime) harnessDidNotStart(b *meshapi.Brief, res SessionResult, err error, ctx interface{ Err() error }) error {
	if err == nil || ctx.Err() != nil || res.SessionID != "" || res.Turns > 0 || b.Harness.Name == "" {
		return err
	}
	route, routed := routeFor(r.harnessRoutes(), b.Kind, b.Tenant, briefRepo(b))
	if !routed {
		return err
	}
	return &harnessError{harness: b.Harness.Name, cause: meshapi.CauseCannotRun,
		why: fmt.Sprintf("%s, and it could not start: %v", route, err), wrapped: err}
}
