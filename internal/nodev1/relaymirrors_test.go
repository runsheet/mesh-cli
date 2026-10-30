package nodev1

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/relaysidecar"
	"projectmesh.io/mesh-cli/internal/resources"
)

// A relay sidecar is handed the node's mirrors read-only: the lease's
// tenant's mirror of its repository (synced by the node's own checkout)
// and of its read_repos the node holds, each opened to the sidecar's
// user, and never another tenant's mirror of the same repository. Its
// credentials name the repository's, which its relay fetches from.
func TestARelaySidecarIsHandedItsTenantsMirrorsAlone(t *testing.T) {
	g, sb, installed := sidecarRig(t)
	sb.Agent = hostAgent(t, filepath.Join(t.TempDir(), "claude"), "")
	var brief meshapi.Brief
	job := g.offer(t, func(b *meshapi.Brief) {
		b.FinishTool, b.FinishSchema = "", nil
		b.Git.ReadRepos = []string{"acme/lib", "acme/none-held"}
		brief = *b
	})
	// the node already holds the tenant's mirror of a read repo, and
	// another tenant's mirror of the lease's own repository
	pol := policyOf(brief.Git)
	keyOf := func(tenant, repo string) string {
		dir, err := (&resources.Mirrors{Root: filepath.Join(g.rt.WorkRoot, "mirrors")}).Dir(resources.MirrorKey{Tenant: tenant, Vendor: pol.Vendor, Base: pol.Base, Repo: repo})
		if err != nil {
			t.Fatal(err)
		}
		return dir
	}
	lib, other := keyOf(brief.Tenant, "acme/lib"), keyOf("another-tenant", brief.Git.Repo)
	for _, dir := range []string{lib, other} {
		os.MkdirAll(filepath.Dir(dir), 0o700)
		if out, err := exec.Command("git", "init", "-q", "--bare", dir).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		os.Chmod(filepath.Join(dir, "HEAD"), 0o600)
		// used now, so the node's trim keeps it
		os.WriteFile(filepath.Join(dir, "mesh-mirror-used"), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
	}
	run(t, g, context.Background())
	if n := len(g.fake.Calls("finish")); n != 1 {
		t.Fatalf("%d finishes; hand-backs %+v", n, g.fake.Calls("hand-back"))
	}
	own := keyOf(brief.Tenant, brief.Git.Repo)
	if len(sb.Relays) != 1 {
		t.Fatalf("relays %+v", sb.Relays)
	}
	if got := sb.Relays[0].Mirrors; !slices.Equal(got, []string{own, lib}) {
		t.Fatalf("the sidecar mounts %q, want the lease's repository's and its read repo's: %q", got, []string{own, lib})
	}
	var c relaysidecar.Credentials
	if err := json.Unmarshal((*installed)[0], &c); err != nil {
		t.Fatal(err)
	}
	if c.Git == nil || c.Git.Mirror != own {
		t.Fatalf("the sidecar's git mirror %+v", c.Git)
	}
	// the read repos ride in the policy, and the one the node holds a
	// mirror of names it at the path it is mounted at
	if !slices.Equal(c.Git.Policy.ReadRepos, []string{"acme/lib", "acme/none-held"}) || len(c.Git.ReadMirrors) != 1 || c.Git.ReadMirrors["acme/lib"] != lib {
		t.Fatalf("the sidecar's read repos %q and mirrors %v", c.Git.Policy.ReadRepos, c.Git.ReadMirrors)
	}
	if fi, err := os.Stat(filepath.Join(lib, "HEAD")); err != nil || fi.Mode().Perm()&0o004 == 0 {
		t.Fatalf("the read repo's mirror is not readable by the sidecar's user: %v %v", fi.Mode(), err)
	}
	if fi, _ := os.Stat(filepath.Join(other, "HEAD")); fi.Mode().Perm()&0o004 != 0 {
		t.Fatal("another tenant's mirror was opened")
	}
	if fi, _ := os.Stat(filepath.Join(g.rt.WorkRoot, "mirrors")); fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("the mirror root is %v, not the node's alone", fi.Mode())
	}
	_ = job
}
