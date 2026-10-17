package node

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// nodeStoresSandbox is a sandbox on the fake runtime carrying the node's
// one store under ~/.mesh/store, as OpenSandbox sets it.
func nodeStoresSandbox(t *testing.T) *sandbox.Sandbox {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	root, err := StoreRoot()
	if err != nil {
		t.Fatal(err)
	}
	return &sandbox.Sandbox{Runtime: &sandbox.Fake{}, TempDir: t.TempDir(), Logf: t.Logf, Config: sandbox.Config{
		Image:   "ghcr.io/runsheet/mesh-node:v1",
		Network: sandbox.DefaultNetwork(),
		Stores:  PackageStores(root),
	}}
}

func envValue(env []string, name string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v
		}
	}
	return ""
}

func underDir(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// A node serving orgs A and B opens an org A session's pod on A's own
// package store: its Stores dirs are under A's OrgRoots.Store, its env's
// npm_config_cache and pnpm store variables name A's dirs, and neither
// its mounts nor its env name anything under B's root (ADR-034).
func TestTwoOrgsSessionsInstallAgainstTheirOwnStore(t *testing.T) {
	sb := nodeStoresSandbox(t)
	home, _ := os.UserHomeDir()
	mesh := filepath.Join(home, ".mesh")
	scopes := Scopes{{Login: "acme", ID: "org-a"}, {Login: "grapevine", ID: "org-b"}}
	set, err := NewOrgRootSet(mesh, []string{"acme", "grapevine"})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := set.For("acme")
	b, _ := set.For("grapevine")

	osb, err := orgSandbox(sb, set, scopes, "org-a")
	if err != nil {
		t.Fatal(err)
	}
	st := osb.Config.Stores
	if st != PackageStores(a.Store) {
		t.Fatalf("org A's stores = %+v, want %+v", st, PackageStores(a.Store))
	}
	for _, d := range []string{st.Pnpm, st.Npm} {
		if !underDir(d, a.Store) || underDir(d, b.Base) {
			t.Errorf("store dir %s: want under %s and not under %s", d, a.Store, b.Base)
		}
	}
	if sb.Config.Stores != PackageStores(filepath.Join(mesh, "store")) {
		t.Errorf("the node's own sandbox was changed: %+v", sb.Config.Stores)
	}

	ws := t.TempDir()
	pod, err := openOrgSessionPod(context.Background(), sb, set, scopes, "org-a", ws, "ship-it/2", harness.KindBuild, sandbox.GitIdentity{}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer pod.Close()
	env := pod.Session.Env(nil)
	for name, want := range map[string]string{
		"npm_config_cache":      filepath.Join(a.Store, "npm"),
		"npm_config_store_dir":  filepath.Join(a.Store, "pnpm"),
		"pnpm_config_store_dir": filepath.Join(a.Store, "pnpm"),
		"PNPM_HOME":             filepath.Join(a.Store, "pnpm"),
	} {
		if got := envValue(env, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	for _, kv := range env {
		if strings.Contains(kv, b.Base) || strings.Contains(kv, filepath.Join(mesh, "store")) {
			t.Errorf("org A's env names another store: %s", kv)
		}
	}
	mounts := pod.Session.Mounts(ws)
	for _, d := range []string{st.Pnpm, st.Npm} {
		if !slices.Contains(mounts, sandbox.Mount{Source: d, Target: d}) {
			t.Errorf("mounts lack %s: %+v", d, mounts)
		}
	}
	for _, m := range mounts {
		if underDir(m.Source, b.Base) || underDir(m.Source, filepath.Join(mesh, "store")) {
			t.Errorf("org A's pod mounts %s", m.Source)
		}
	}

	// org B's own, never A's
	bsb, err := orgSandbox(sb, set, scopes, "org-b")
	if err != nil {
		t.Fatal(err)
	}
	if bsb.Config.Stores != PackageStores(b.Store) {
		t.Errorf("org B's stores = %+v, want %+v", bsb.Config.Stores, PackageStores(b.Store))
	}
	// an org the node does not serve gets no store and no pod
	if _, err := orgSandbox(sb, set, scopes, "org-c"); err == nil {
		t.Error("an unserved org was given a store")
	}
	if _, err := openOrgSessionPod(context.Background(), sb, set, scopes, "org-c", ws, "x/1", harness.KindBuild, sandbox.GitIdentity{}, t.Logf); err == nil {
		t.Error("an unserved org's pod was opened")
	}
}

// A node serving one org — or with no roots laid out — keeps today's
// store, ~/.mesh/store/{pnpm,npm}.
func TestOneOrgSessionsKeepTodaysStore(t *testing.T) {
	sb := nodeStoresSandbox(t)
	home, _ := os.UserHomeDir()
	mesh := filepath.Join(home, ".mesh")
	want := sandbox.Stores{Pnpm: filepath.Join(mesh, "store", "pnpm"), Npm: filepath.Join(mesh, "store", "npm")}
	set, err := NewOrgRootSet(mesh, []string{"acme"})
	if err != nil {
		t.Fatal(err)
	}
	scopes := Scopes{{Login: "acme", ID: "org-a"}}
	for name, roots := range map[string]*OrgRootSet{"one org": set, "no roots": nil} {
		osb, err := orgSandbox(sb, roots, scopes, "org-a")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if osb.Config.Stores != want {
			t.Errorf("%s: stores = %+v, want %+v", name, osb.Config.Stores, want)
		}
		pod, err := openOrgSessionPod(context.Background(), sb, roots, scopes, "org-a", t.TempDir(), "ship-it/2", harness.KindBuild, sandbox.GitIdentity{}, t.Logf)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		env := pod.Session.Env(nil)
		pod.Close()
		if got := envValue(env, "npm_config_cache"); got != want.Npm {
			t.Errorf("%s: npm_config_cache = %q, want %q", name, got, want.Npm)
		}
		if got := envValue(env, "pnpm_config_store_dir"); got != want.Pnpm {
			t.Errorf("%s: pnpm_config_store_dir = %q, want %q", name, got, want.Pnpm)
		}
	}
}
