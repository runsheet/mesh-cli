package node

import (
	"fmt"
	"os"
	"path/filepath"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// The sessions' paths on a node with a Layout (ADR-039): each helper
// answers a session of one job, by the job's key — the item's, goal's,
// direction's or batch's id. A nil Layout is a work root's paths
// (workRoot), which only the tests lay out.

// openJob opens the job keyed id for the served org orgID (Layout.OpenJob):
// its directory and job.json naming the org's connection. An org the
// node does not serve, or one the platform named no connection for, is
// refused.
func openJob(l *Layout, scopes Scopes, orgID string, m JobMeta) (JobDirs, error) {
	sc, err := connectionOf(scopes, orgID)
	if err != nil {
		return JobDirs{}, err
	}
	m.ConnectionID, m.OrgID, m.Product = sc.ConnectionID, orgID, sc.Product
	return l.OpenJob(m)
}

// itemJobMeta is the job.json of item's job.
func itemJobMeta(item gateway.WorkItem, label string) JobMeta {
	return JobMeta{ID: item.ID, Unit: JobItem, ItemID: item.ID, GoalID: item.GoalID, Label: label}
}

// itemSpec puts spec in item's job (ADR-039): its checkout
// <job>/checkout, a clone of its own under the job's repo locks. Without
// a Layout, spec as it is.
func itemSpec(l *Layout, scopes Scopes, item gateway.WorkItem, label string, spec WorkspaceSpec) (WorkspaceSpec, error) {
	if l == nil {
		return spec, nil
	}
	j, err := openJob(l, scopes, item.OrgID, itemJobMeta(item, label))
	if err != nil {
		return spec, err
	}
	spec.Job, spec.Path, spec.Root, spec.Repos = j.Dir, j.Checkout(), j.Dir, ""
	return spec, nil
}

// jobDirsOf are the job's directories under l, else false (no Layout,
// or a key that is not one).
func jobDirsOf(l *Layout, id string) (JobDirs, bool) {
	if l == nil {
		return JobDirs{}, false
	}
	j, err := l.Job(id)
	return j, err == nil
}

// scratchRootFor is the scratch root a host run of job id makes its
// TMPDIR under: the job's own, else workRoot's (ScratchRoot).
func scratchRootFor(l *Layout, id, workRoot string) (string, error) {
	if l == nil {
		return ScratchRoot(workRoot)
	}
	j, err := l.Job(id)
	if err != nil {
		return "", err
	}
	return j.Scratch, nil
}

// keptRootFor is the kept-sessions dir a session of job id keeps its
// stream under (harness.Job.KeptRoot): the job's, else "" — the
// harness's own <work root>/kept-sessions.
func keptRootFor(l *Layout, id string) (string, error) {
	if l == nil {
		return "", nil
	}
	j, err := l.Job(id)
	if err != nil {
		return "", err
	}
	return j.Kept, nil
}

// mcpRootFor is the MCP root of job id, else "" (SessionProxies.Root).
func mcpRootFor(l *Layout, id string) string {
	if j, ok := jobDirsOf(l, id); ok {
		return j.MCP
	}
	return ""
}

// jobSandbox is the sandbox a session working in dir opens its pod on:
// sb on the job's own package store (PackageStores, sandbox.WithStores),
// so its pod mounts and its env names only its job's pnpm store and npm
// cache, which no other job writes. A dir in no job (the tests' work
// root) is sb itself; a nil sb is nil.
func jobSandbox(sb *sandbox.Sandbox, dir string) *sandbox.Sandbox {
	if sb == nil {
		return nil
	}
	job, _, ok := jobOf(dir)
	if !ok {
		return sb
	}
	return sb.WithStores(PackageStores(filepath.Join(job, "store")))
}

// confinementOf is what a session working in dir is confined to: its
// job's directory, when dir is in a job, else nothing.
func confinementOf(dir string) sandbox.Confinement {
	job, m, ok := jobOf(dir)
	if !ok {
		return sandbox.Confinement{}
	}
	return sandbox.Confinement{Job: m.ID, Roots: []string{job}, Homes: filepath.Join(job, "homes")}
}

// confineTo is sb with its sessions confined to the job that holds dir
// (confinementOf, Sandbox.WithConfinement) on the job's own store
// (jobSandbox); a dir in no job, or a nil sb, is sb.
func confineTo(sb *sandbox.Sandbox, dir string) *sandbox.Sandbox {
	return jobSandbox(sb, dir).WithConfinement(confinementOf(dir))
}

// planDirFor is where goal's planner works: <job>/plan in the goal's
// job, else PlanDir under workRoot.
func planDirFor(l *Layout, scope Scope, goal gateway.Goal, workRoot string) (string, error) {
	if l == nil {
		return PlanDir(workRoot, goal.Slug), nil
	}
	j, err := openJob(l, Scopes{scope}, scope.ID, JobMeta{ID: goal.ID, Unit: JobPlan, GoalID: goal.ID, Label: goal.Slug})
	if err != nil {
		return "", err
	}
	return j.Plan(), nil
}

// researchDirFor is where a direction's research works: <job>/research
// in the direction's job, else ResearchDir under workRoot.
func researchDirFor(l *Layout, scope Scope, project gateway.Project, direction gateway.Direction, workRoot string) (string, error) {
	if l == nil {
		return ResearchDir(workRoot, project.Slug, direction.Slug), nil
	}
	j, err := openJob(l, Scopes{scope}, scope.ID, JobMeta{ID: direction.ID, Unit: JobResearch, DirectionID: direction.ID, Label: project.Slug + "/" + direction.Slug})
	if err != nil {
		return "", err
	}
	return j.Research(), nil
}

// batchDirFor is where an attempt of a batch-like job checks out (a
// merge batch's candidate, a merged item's main validation): in the
// job keyed id, <job>/<what>-<attempt>; else legacy under workRoot.
func batchDirFor(l *Layout, scopes Scopes, orgID, id, what string, attempt int, label string, legacy func(root string) string, workRoot string) (string, error) {
	if l == nil {
		return legacy(workRoot), nil
	}
	j, err := openJob(l, scopes, orgID, JobMeta{ID: id, Unit: JobBatch, BatchID: id, Label: label})
	if err != nil {
		return "", err
	}
	return j.Attempt(what, attempt), nil
}

// mainJobID is the job key of a merged item's main validation: its own,
// beside the item's job, which the sweep may already have removed.
func mainJobID(itemID string) string { return fmt.Sprintf("%s-main", itemID) }

// validationJobID is the job a validation runs in: the job holding its
// checkout (an item's, a batch's candidate, a main validation), else
// its item's.
func validationJobID(t validationTarget) string {
	if _, m, ok := jobOf(t.dir); ok {
		return m.ID
	}
	return t.item.ID
}

// evalSpec puts an evaluation's spec (EvalWorkspaceSpec) in item's job:
// its own clone at <job>/eval, under the job's locks. Without a Layout,
// spec as it is.
func evalSpec(l *Layout, scopes Scopes, item gateway.WorkItem, label string, spec WorkspaceSpec) (WorkspaceSpec, error) {
	if l == nil {
		return spec, nil
	}
	j, err := openJob(l, scopes, item.OrgID, itemJobMeta(item, label))
	if err != nil {
		return spec, err
	}
	spec.Job, spec.Path, spec.Root, spec.Repos = j.Dir, j.Eval(), j.Dir, ""
	return spec, nil
}

// jobCheckoutOf is the build checkout of the job holding dir, when it is
// a clone: what a validation of that job's head may read objects from.
func jobCheckoutOf(dir string) string {
	job, _, ok := jobOf(dir)
	if !ok {
		return ""
	}
	co := filepath.Join(job, "checkout")
	if _, err := os.Stat(filepath.Join(co, ".git")); err != nil {
		return ""
	}
	return co
}

// nodeIDOf is the node a session of orgID speaks as: its scope's node,
// else the registration's.
func nodeIDOf(scopes Scopes, orgID string, cfg *Config) string {
	if sc, ok := scopes.ByOrg(orgID); ok && sc.NodeID != "" {
		return sc.NodeID
	}
	if cfg != nil {
		return cfg.NodeID
	}
	return ""
}
