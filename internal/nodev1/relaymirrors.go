package nodev1

import (
	"io/fs"
	"os"
	"path/filepath"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/resources"
)

// A relay sidecar reads the node's mirrors (projectmesh-io/api ADR 001
// amendment, relay sidecar: "Mirrors stay with the node"). The node's
// per-(tenant, repository) mirrors are bind-mounted into the sidecar
// read-only, each at its own path: only this lease's tenant's mirror of
// its repository and of the brief's read_repos, never another
// tenant's, and only those the node already holds. The sidecar answers
// a fetch from its repository's mirror when the mirror holds what is
// wanted and goes upstream through the lease's tunnel otherwise
// (gitrelay.Relay.Mirror). The node syncs a mirror only for its own
// checkout, through the lease's grant; it never runs git on a request
// that came from the pod.

// leaseMirrors are b's mirrors a relay sidecar may read: the
// repository's (mirror, "" when the node holds none) and with it the
// read_repos' the node holds (all). Each is opened to the sidecar's own
// user, which reads what the node's wrote; the mirror root above them
// stays the node's alone.
func (r *Runtime) leaseMirrors(b *meshapi.Brief) (mirror string, all []string) {
	if r.mirrors == nil || b.Git == nil || b.Git.Repo == "" {
		return "", nil
	}
	pol := policyOf(b.Git)
	for i, repo := range append([]string{b.Git.Repo}, b.Git.ReadRepos...) {
		dir, err := r.mirrors.Dir(resources.MirrorKey{Tenant: b.Tenant, Vendor: pol.Vendor, Base: pol.Base, Repo: repo})
		if err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
			continue
		}
		if err := readableToOthers(dir); err != nil {
			r.logf("nodev1: lease %s: the mirror of %s is not shared with the relay sidecar: %v", b.LeaseID, repo, err)
			continue
		}
		if i == 0 {
			mirror = dir
		}
		all = append(all, dir)
	}
	return mirror, all
}

// readableToOthers lets any user read dir's tree: the relay sidecar's
// user is not the node's. Nothing above dir is changed, so the node's
// mirror root, 0700, keeps every other user of the host out.
func readableToOthers(dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		want := info.Mode().Perm() | 0o004
		if d.IsDir() {
			want |= 0o001
		}
		if want == info.Mode().Perm() {
			return nil
		}
		return os.Chmod(path, want)
	})
}
