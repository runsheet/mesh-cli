package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// twoOrgTree lays out two orgs' trees, acme and beta, under one mesh
// dir as the node's OrgRoots do, and answers acme's org root and the
// mesh dir.
func twoOrgTree(t *testing.T) (OrgRoot, string) {
	t.Helper()
	mesh := t.TempDir()
	for _, org := range []string{"acme", "beta"} {
		for _, d := range []string{"work/goal/1", "repos", "store/pnpm", "store/npm"} {
			if err := os.MkdirAll(filepath.Join(mesh, "orgs", org, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	return OrgRoot{Org: "acme", Path: filepath.Join(mesh, "orgs", "acme")}, mesh
}

// confinedSession opens a session of acme's on a sandbox confined to its
// tree, its stores st and its hooks hooks.
func confinedSession(t *testing.T, org OrgRoot, st Stores, hooks string) (*Session, *Fake) {
	t.Helper()
	f := &Fake{}
	sb := testSandbox(t, f)
	sb.Logf = t.Logf
	sb.Config.Stores = st
	sess, err := sb.WithOrg(org).Open("ship-it/2", "build", GitIdentity{HooksDir: hooks})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess, f
}

// acmeStores are acme's own stores.
func acmeStores(org OrgRoot) Stores {
	return Stores{Pnpm: filepath.Join(org.Path, "store", "pnpm"), Npm: filepath.Join(org.Path, "store", "npm")}
}

// refusedNaming asserts err is a mount refusal naming what, path and
// the org.
func refusedNaming(t *testing.T, err error, what, path, org string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s %s: not refused", what, path)
	}
	if !errors.Is(err, ErrMountOutsideOrg) {
		t.Fatalf("%v: not ErrMountOutsideOrg", err)
	}
	var me *MountOutsideOrgError
	if !errors.As(err, &me) || me.What != what || me.Org != org {
		t.Fatalf("%v: want a %s refusal of org %s, got %+v", err, what, org, me)
	}
	for _, s := range []string{path, org} {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("%q does not name %q", err, s)
		}
	}
}

// A confined session whose every mount is inside its org's tree mounts
// them all, its HOME made under the tree.
func TestConfinedSessionMountsItsOwnOrgsTree(t *testing.T) {
	org, _ := twoOrgTree(t)
	hooks := filepath.Join(org.Path, "work", "goal", "1.hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	sess, _ := confinedSession(t, org, acmeStores(org), hooks)
	if !strings.HasPrefix(sess.Home, org.HomesDir()+string(filepath.Separator)) {
		t.Fatalf("home %s not under %s", sess.Home, org.HomesDir())
	}
	m, err := sess.CheckedMounts(filepath.Join(org.Path, "work", "goal", "1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 5 {
		t.Fatalf("mounts = %+v", m)
	}
}

// A workspace outside the org's tree — another org's checkout — is
// refused, and Run starts no agent.
func TestConfinedSessionRefusesAWorkspaceOutsideTheOrgRoot(t *testing.T) {
	org, mesh := twoOrgTree(t)
	sess, f := confinedSession(t, org, acmeStores(org), "")
	ws := filepath.Join(mesh, "orgs", "beta", "work", "goal", "1")
	_, err := sess.CheckedMounts(ws)
	refusedNaming(t, err, "workspace", ws, "acme")
	err = sess.Run(context.Background(), RunSpec{Workspace: ws, Command: []string{"claude", "-p"}, Stdin: strings.NewReader("p")})
	refusedNaming(t, err, "workspace", ws, "acme")
	if len(f.Agents) != 0 {
		t.Fatalf("an agent started: %+v", f.Agents)
	}
	if _, err := sess.StartDetached(context.Background(), DetachedSpec{Workspace: ws, Command: []string{"claude"}}); !errors.Is(err, ErrMountOutsideOrg) {
		t.Fatalf("detached: %v", err)
	}
	if len(f.Agents) != 0 || len(f.Pods) != 0 {
		t.Fatalf("a detached start made a pod or agent: %+v %+v", f.Pods, f.Agents)
	}
}

// A HOME outside the org's tree is refused.
func TestConfinedSessionRefusesAHomeOutsideTheOrgRoot(t *testing.T) {
	org, _ := twoOrgTree(t)
	sess, _ := confinedSession(t, org, acmeStores(org), "")
	sess.Home = t.TempDir()
	_, err := sess.CheckedMounts(filepath.Join(org.Path, "work", "goal", "1"))
	refusedNaming(t, err, "home", sess.Home, "acme")
}

// A worktree in acme's work root whose .git names a clone in beta's
// repos root is refused at its git common dir.
func TestConfinedSessionRefusesACommonDirInAnotherOrgsRepos(t *testing.T) {
	org, mesh := twoOrgTree(t)
	clone, _ := cloneAndWorktree(t)
	betaClone := filepath.Join(mesh, "orgs", "beta", "repos", "owner", "name")
	if err := os.MkdirAll(filepath.Dir(betaClone), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, filepath.Dir(clone), "clone", "-q", clone, betaClone)
	ws := filepath.Join(org.Path, "work", "goal", "2")
	gitRun(t, betaClone, "worktree", "add", "-q", "-b", "item-2", ws, "HEAD")
	sess, _ := confinedSession(t, org, acmeStores(org), "")
	_, err := sess.CheckedMounts(ws)
	common, _ := filepath.EvalSymlinks(filepath.Join(betaClone, ".git"))
	refusedNaming(t, err, "git common dir", common, "acme")
	if !strings.Contains(err.Error(), filepath.Join("orgs", "beta", "repos")) {
		t.Fatalf("%v does not name beta's repos root", err)
	}
}

// A package store of another org is refused.
func TestConfinedSessionRefusesAnotherOrgsStore(t *testing.T) {
	org, mesh := twoOrgTree(t)
	beta := filepath.Join(mesh, "orgs", "beta", "store", "pnpm")
	sess, _ := confinedSession(t, org, Stores{Pnpm: beta}, "")
	_, err := sess.CheckedMounts(filepath.Join(org.Path, "work", "goal", "1"))
	refusedNaming(t, err, "package store", beta, "acme")
}

// A hooks dir outside the org's tree is refused.
func TestConfinedSessionRefusesAHooksDirOutsideTheOrgRoot(t *testing.T) {
	org, _ := twoOrgTree(t)
	hooks := t.TempDir()
	sess, _ := confinedSession(t, org, acmeStores(org), hooks)
	_, err := sess.CheckedMounts(filepath.Join(org.Path, "work", "goal", "1"))
	refusedNaming(t, err, "hooks", hooks, "acme")
}

// A symlink inside the org's tree pointing out of it is followed and
// refused, naming the link and where it resolves.
func TestConfinedSessionRefusesASymlinkOutOfTheOrgRoot(t *testing.T) {
	org, mesh := twoOrgTree(t)
	link := filepath.Join(org.Path, "work", "goal", "3")
	target := filepath.Join(mesh, "orgs", "beta", "work", "goal", "1")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	sess, _ := confinedSession(t, org, acmeStores(org), "")
	_, err := sess.CheckedMounts(link)
	refusedNaming(t, err, "workspace", link, "acme")
	resolved, _ := filepath.EvalSymlinks(target)
	if !strings.Contains(err.Error(), resolved) {
		t.Fatalf("%v does not name where the link resolves, %s", err, resolved)
	}
}

// An unconfined session — a node serving one org — mounts as before.
func TestUnconfinedSessionChecksNothing(t *testing.T) {
	sess := openSession(t, t.TempDir())
	ws := t.TempDir()
	got, err := sess.CheckedMounts(ws)
	if err != nil {
		t.Fatal(err)
	}
	if want := sess.Mounts(ws); len(got) != len(want) {
		t.Fatalf("checked %+v, mounts %+v", got, want)
	}
}
