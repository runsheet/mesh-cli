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

// Two jobs' sessions install against their own job's package store
// (ADR-039): a session's pod working in job A's checkout has Stores
// under A's <job>/store, its env's npm_config_cache and pnpm store
// variables name A's dirs, and neither its mounts nor its env name job
// B's store or a store of the node's.
func TestTwoJobsSessionsInstallAgainstTheirOwnStore(t *testing.T) {
	sb := nodeStoresSandbox(t)
	home, _ := os.UserHomeDir()
	mesh := filepath.Join(home, ".mesh")
	scopes := connected(Scopes{{Login: "acme", ID: "org-a"}, {Login: "grapevine", ID: "org-b"}})
	l := &Layout{Dir: mesh}
	a, err := openJob(l, scopes, "org-a", JobMeta{ID: "item-a", Unit: JobItem, ItemID: "item-a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := openJob(l, scopes, "org-b", JobMeta{ID: "item-b", Unit: JobItem, ItemID: "item-b"})
	if err != nil {
		t.Fatal(err)
	}
	ws := a.Checkout()
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	st := jobSandbox(sb, ws).Config.Stores
	if st != PackageStores(a.Store) {
		t.Fatalf("job A's stores = %+v, want %+v", st, PackageStores(a.Store))
	}
	if sb.Config.Stores != PackageStores(filepath.Join(mesh, "store")) {
		t.Errorf("the node's own sandbox was changed: %+v", sb.Config.Stores)
	}
	if got := jobSandbox(sb, b.Checkout()).Config.Stores; got != PackageStores(b.Store) {
		t.Errorf("job B's stores = %+v, want %+v", got, PackageStores(b.Store))
	}

	pod, err := openOrgSessionPod(context.Background(), sb, l, scopes, "org-a", ws, "ship-it/2", harness.KindBuild, sandbox.GitIdentity{}, t.Logf)
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
		if strings.Contains(kv, b.Dir) || strings.Contains(kv, filepath.Join(mesh, "store")) {
			t.Errorf("job A's env names another store: %s", kv)
		}
	}
	mounts := pod.Session.Mounts(ws)
	for _, d := range []string{st.Pnpm, st.Npm} {
		if !slices.Contains(mounts, sandbox.Mount{Source: d, Target: d}) {
			t.Errorf("mounts lack %s: %+v", d, mounts)
		}
	}
	for _, m := range mounts {
		if underDir(m.Source, b.Dir) || underDir(m.Source, filepath.Join(mesh, "store")) {
			t.Errorf("job A's pod mounts %s", m.Source)
		}
	}
}

// A session working in no job — the tests' work root — keeps the
// node's own sandbox's store.
func TestNoJobKeepsTheSandboxesStore(t *testing.T) {
	sb := nodeStoresSandbox(t)
	want := sb.Config.Stores
	scopes := Scopes{{Login: "acme", ID: "org-a"}}
	if osb := jobSandbox(sb, t.TempDir()); osb.Config.Stores != want {
		t.Errorf("stores = %+v, want %+v", osb.Config.Stores, want)
	}
	pod, err := openOrgSessionPod(context.Background(), sb, nil, scopes, "org-a", t.TempDir(), "ship-it/2", harness.KindBuild, sandbox.GitIdentity{}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	env := pod.Session.Env(nil)
	pod.Close()
	if got := envValue(env, "npm_config_cache"); got != want.Npm {
		t.Errorf("npm_config_cache = %q, want %q", got, want.Npm)
	}
}
