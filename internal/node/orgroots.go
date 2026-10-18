package node

// Each served org gets its own roots (ADR-034, docs/adr/adr-034-each-org-has-its-own-roots.md, amending ADR-028). A node
// that serves several orgs used to keep every org's checkouts under one
// ~/.mesh/work and every org's shared clones under one ~/.mesh/repos, so
// org A's worktrees and clone were siblings of org B's, and a clone of a
// repo two orgs both reach was one clone for both. Now each org has its
// own tree, ~/.mesh/orgs/<org>/, with its work, repos, scratch, package
// store, kept sessions, resume records and MCP data under it, keyed by
// the scope's Name() as ADR-023's shares are; a worktree of org A is only
// ever made from org A's clone, and a repo served for two orgs is two
// clones.
//
// A node serving exactly one org keeps today's paths (~/.mesh/work,
// ~/.mesh/repos, ~/.mesh/store, …): nothing to migrate, nothing to clone
// again, and nothing to sandbox it from. Only a node serving two or more
// orgs lays them out under ~/.mesh/orgs/.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// OrgsDirName is the per-org roots' parent under ~/.mesh.
const OrgsDirName = "orgs"

// OrgRoots are one org's directories on the node.
type OrgRoots struct {
	// Org is the org's key, its scope's Name(); "" for the single-org
	// layout's today's paths.
	Org string
	// Base is the org's own tree, ~/.mesh/orgs/<org>; "" for the
	// single-org layout, which has none of its own.
	Base string
	// Work is the parent of every checkout: <work>/<goal>/<seq>.
	Work string
	// Repos holds the shared clones, <repos>/<owner>/<name>.
	Repos string
	// Scratch is where a host run's TMPDIR is made (MakeScratch).
	Scratch string
	// Store is the pnpm store and npm cache (PackageStores).
	Store string
	// Kept is where sessions' streams are kept (harness kept-sessions).
	Kept string
	// Resumes holds the builds' resume records (ResumeStore).
	Resumes string
	// MCP holds the org's MCP data: session token files and state.
	MCP string
}

// Dirs are every directory of r, in a fixed order, for checks that
// two orgs' roots never meet.
func (r OrgRoots) Dirs() []string {
	return []string{r.Work, r.Repos, r.Scratch, r.Store, r.Kept, r.Resumes, r.MCP}
}

// ValidateOrgName refuses an org key that is not one path segment: empty,
// "." or "..", or carrying a path separator (either slash), a NUL, or
// surrounding space — any of which would put the org's roots outside
// ~/.mesh/orgs/ or on another org's.
func ValidateOrgName(org string) error {
	switch {
	case org == "":
		return errors.New("org roots: empty org name")
	case org == "." || org == "..":
		return fmt.Errorf("org roots: org name %q is not a directory name", org)
	case strings.ContainsAny(org, `/\`+"\x00"):
		return fmt.Errorf("org roots: org name %q holds a path separator", org)
	case strings.TrimSpace(org) != org:
		return fmt.Errorf("org roots: org name %q has surrounding space", org)
	}
	return nil
}

// RootsForOrg are org's roots under meshDir (~/.mesh):
// <meshDir>/orgs/<org>/{work,repos,store,resume,mcp}, scratch and kept
// sessions under its work root as today's are under ~/.mesh/work.
func RootsForOrg(meshDir, org string) (OrgRoots, error) {
	if err := ValidateOrgName(org); err != nil {
		return OrgRoots{}, err
	}
	base := filepath.Join(meshDir, OrgsDirName, org)
	work := filepath.Join(base, "work")
	return OrgRoots{
		Org:     org,
		Base:    base,
		Work:    work,
		Repos:   filepath.Join(base, "repos"),
		Scratch: filepath.Join(work, ScratchDirName),
		Store:   filepath.Join(base, "store"),
		Kept:    filepath.Join(work, "kept-sessions"),
		Resumes: filepath.Join(base, "resume"),
		MCP:     filepath.Join(base, "mcp"),
	}, nil
}

// SingleOrgRoots are today's paths under meshDir, which a node serving
// one org keeps: work, repos, store and resume beside each other in
// ~/.mesh, scratch and kept sessions under the work root.
func SingleOrgRoots(meshDir string) OrgRoots {
	work := filepath.Join(meshDir, "work")
	return OrgRoots{
		Work:    work,
		Repos:   filepath.Join(meshDir, "repos"),
		Scratch: filepath.Join(work, ScratchDirName),
		Store:   filepath.Join(meshDir, "store"),
		Kept:    filepath.Join(work, "kept-sessions"),
		Resumes: filepath.Join(meshDir, "resume"),
		MCP:     filepath.Join(meshDir, "mcp"),
	}
}

// OrgRootSet are the roots of every org a node serves, by org key.
type OrgRootSet struct {
	byOrg map[string]OrgRoots
	// single is the one org's roots when the node serves one: today's
	// paths, whatever org an item names.
	single *OrgRoots
}

// NewOrgRootSet lays out the roots of orgs (each a scope's Name()) under
// meshDir: one org keeps today's paths (SingleOrgRoots), several each get
// RootsForOrg. Every name is checked as one path segment, also with one
// org, so a bad name is refused before it is ever served by two; two
// names that differ only in case are refused, naming both, since on a
// case-insensitive filesystem (macOS) they would be one directory.
func NewOrgRootSet(meshDir string, orgs []string) (*OrgRootSet, error) {
	if len(orgs) == 0 {
		return nil, errors.New("org roots: no org served")
	}
	seen := map[string]string{}
	for _, o := range orgs {
		if err := ValidateOrgName(o); err != nil {
			return nil, err
		}
		folded := strings.ToLower(o)
		if prev, ok := seen[folded]; ok {
			if prev == o {
				return nil, fmt.Errorf("org roots: org %q served twice", o)
			}
			return nil, fmt.Errorf("org roots: org %q differs only in case from %q", o, prev)
		}
		seen[folded] = o
	}
	s := &OrgRootSet{byOrg: map[string]OrgRoots{}}
	if len(orgs) == 1 {
		r := SingleOrgRoots(meshDir)
		r.Org = orgs[0]
		s.single = &r
		s.byOrg[orgs[0]] = r
		return s, nil
	}
	for _, o := range orgs {
		r, err := RootsForOrg(meshDir, o)
		if err != nil {
			return nil, err
		}
		s.byOrg[o] = r
	}
	return s, nil
}

// NewOrgRootSetFor is NewOrgRootSet under ~/.mesh for the served scopes,
// keyed by each scope's Name().
func NewOrgRootSetFor(scopes Scopes) (*OrgRootSet, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		names = append(names, sc.Name())
	}
	return NewOrgRootSet(filepath.Join(home, ".mesh"), names)
}

// Single says the node serves one org and keeps today's paths.
func (s *OrgRootSet) Single() bool { return s != nil && s.single != nil }

// Orgs are the org keys, sorted.
func (s *OrgRootSet) Orgs() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.byOrg))
	for o := range s.byOrg {
		out = append(out, o)
	}
	sort.Strings(out)
	return out
}

// For are org's roots. A node serving one org answers its roots for any
// org (they are today's paths); a node serving several refuses an org it
// does not serve, naming it — never another org's roots in its place.
func (s *OrgRootSet) For(org string) (OrgRoots, error) {
	if s == nil {
		return OrgRoots{}, errors.New("org roots: none laid out")
	}
	if s.single != nil {
		return *s.single, nil
	}
	if r, ok := s.byOrg[org]; ok {
		return r, nil
	}
	return OrgRoots{}, fmt.Errorf("org roots: org %q is not served by this node", org)
}

// ForOrgID are the roots of the served org whose id is orgID (a work
// item's or goal's OrgID).
func (s *OrgRootSet) ForOrgID(scopes Scopes, orgID string) (OrgRoots, error) {
	if s.Single() {
		return *s.single, nil
	}
	sc, ok := scopes.ByOrg(orgID)
	if !ok {
		return OrgRoots{}, fmt.Errorf("org roots: org id %q is not served by this node", orgID)
	}
	return s.For(sc.Name())
}

// workRootFor is the work root a session of orgID works under: its org's
// when roots are laid out, else workRoot (today's one root).
func workRootFor(roots *OrgRootSet, scopes Scopes, orgID, workRoot string) (string, error) {
	if roots == nil {
		return workRoot, nil
	}
	r, err := roots.ForOrgID(scopes, orgID)
	if err != nil {
		return "", err
	}
	return r.Work, nil
}

// orgReposRootOf is the repos root of the org whose work root holds dir
// (<meshDir>/orgs/<org>/work/<goal>/<seq>, or any path that deep under
// it), else "".
func orgReposRootOf(dir string) string {
	work := filepath.Dir(filepath.Dir(filepath.Clean(dir)))
	base := filepath.Dir(work)
	if filepath.Base(work) != "work" || filepath.Base(filepath.Dir(base)) != OrgsDirName {
		return ""
	}
	if ValidateOrgName(filepath.Base(base)) != nil {
		return ""
	}
	return filepath.Join(base, "repos")
}

// workRootOfScope is the work root a plan or research of scope works
// under: its org's when roots are laid out, else workRoot.
func workRootOfScope(roots *OrgRootSet, scope Scope, workRoot string) (string, error) {
	if roots == nil {
		return workRoot, nil
	}
	r, err := roots.For(scope.Name())
	if err != nil {
		return "", err
	}
	return r.Work, nil
}

// orgSpec puts spec under the roots of the org whose id is orgID — its
// work root and its repos root, so its checkout is a worktree of that
// org's clone alone — when roots are laid out; else spec as it is.
func orgSpec(roots *OrgRootSet, scopes Scopes, orgID string, spec WorkspaceSpec) (WorkspaceSpec, error) {
	if roots == nil {
		return spec, nil
	}
	r, err := roots.ForOrgID(scopes, orgID)
	if err != nil {
		return spec, err
	}
	spec.Root, spec.Repos = r.Work, r.Repos
	return spec, nil
}

// scratchRootFor is the scratch root a host run for orgID makes its
// TMPDIR under: its org's when roots are laid out, else workRoot's
// (ScratchRoot).
func scratchRootFor(roots *OrgRootSet, scopes Scopes, orgID, workRoot string) (string, error) {
	if roots == nil {
		return ScratchRoot(workRoot)
	}
	r, err := roots.ForOrgID(scopes, orgID)
	if err != nil {
		return "", err
	}
	return r.Scratch, nil
}

// orgSandbox is the sandbox a session of orgID opens its pod on: sb
// itself — today's one ~/.mesh/store — when no roots are laid out or the
// node serves one org, else sb on that org's own store
// (PackageStores(roots.Store), sandbox.WithStores), so its pod mounts
// and its env names only its org's pnpm store and npm cache. An org the
// node does not serve is refused, never given another org's store. A nil
// sb is nil.
func orgSandbox(sb *sandbox.Sandbox, roots *OrgRootSet, scopes Scopes, orgID string) (*sandbox.Sandbox, error) {
	if sb == nil || roots == nil || roots.Single() {
		return sb, nil
	}
	r, err := roots.ForOrgID(scopes, orgID)
	if err != nil {
		return nil, err
	}
	return sb.WithStores(PackageStores(r.Store)), nil
}

// keptRootFor is the kept-sessions dir a session of orgID keeps its
// stream under (harness.Job.KeptRoot): its org's Kept when roots are
// laid out, else "" — the harness's own <work root>/kept-sessions, as
// before. An org the node does not serve is refused, never given
// another org's.
func keptRootFor(roots *OrgRootSet, scopes Scopes, orgID string) (string, error) {
	if roots == nil {
		return "", nil
	}
	r, err := roots.ForOrgID(scopes, orgID)
	if err != nil {
		return "", err
	}
	return r.Kept, nil
}

// keptRootOfScope is keptRootFor for a plan or research of scope.
func keptRootOfScope(roots *OrgRootSet, scope Scope) (string, error) {
	if roots == nil {
		return "", nil
	}
	r, err := roots.For(scope.Name())
	if err != nil {
		return "", err
	}
	return r.Kept, nil
}

// SweepKeptSessions runs harness.SweepKeptSessions over every served
// org's Kept dir (ADR-034), each org's own kept sessions only, so an org
// no new Codex session has started for is swept too. It goes on past
// an org it cannot read and returns the errors joined.
func (s *OrgRootSet) SweepKeptSessions(now time.Time) error {
	if s == nil {
		return nil
	}
	var errs []error
	for _, org := range s.Orgs() {
		roots, err := s.For(org)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := harness.SweepKeptSessions(roots.Kept, now); err != nil {
			errs = append(errs, fmt.Errorf("kept sessions of %s: %w", org, err))
		}
	}
	return errors.Join(errs...)
}
