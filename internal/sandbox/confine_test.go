package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// jobTree is the jobs and connections trees under one mesh dir as the
// node's Layout makes them (mesh-cli ADR-039): jobs a and b, a of
// connection ca and b of cb, each job with a checkout and each
// connection with repos and stores. It answers job a's confinement and
// the mesh dir.
type jobTree struct {
	Confinement
	mesh string
}

// job is a path in job id's directory.
func (j jobTree) job(id string, parts ...string) string {
	return filepath.Join(append([]string{j.mesh, "jobs", id}, parts...)...)
}

// conn is a path in connection id's caches.
func (j jobTree) conn(id string, parts ...string) string {
	return filepath.Join(append([]string{j.mesh, "connections", id}, parts...)...)
}

func twoJobTree(t *testing.T) jobTree {
	t.Helper()
	mesh := t.TempDir()
	for _, d := range []string{"jobs/a/checkout", "jobs/b/checkout"} {
		if err := os.MkdirAll(filepath.Join(mesh, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []string{"ca", "cb"} {
		for _, d := range []string{"repos", "store/pnpm", "store/npm"} {
			if err := os.MkdirAll(filepath.Join(mesh, "connections", c, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	jt := jobTree{mesh: mesh}
	jt.Confinement = Confinement{Job: "a", Roots: []string{jt.job("a"), jt.conn("ca")}, Homes: jt.job("a", "homes")}
	return jt
}

// confinedSession opens a session of job a's on a sandbox confined to
// it, its stores st and its hooks hooks.
func confinedSession(t *testing.T, jt jobTree, st Stores, hooks string) (*Session, *Fake) {
	t.Helper()
	f := &Fake{}
	sb := testSandbox(t, f)
	sb.Logf = t.Logf
	sb.Config.Stores = st
	sess, err := sb.WithConfinement(jt.Confinement).Open("ship-it/2", "build", GitIdentity{HooksDir: hooks})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess, f
}

// ownStores are job a's connection's stores.
func ownStores(jt jobTree) Stores {
	return Stores{Pnpm: jt.conn("ca", "store", "pnpm"), Npm: jt.conn("ca", "store", "npm")}
}

// refusedNaming asserts err is a mount refusal naming what, path and
// the job.
func refusedNaming(t *testing.T, err error, what, path, job string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s %s: not refused", what, path)
	}
	if !errors.Is(err, ErrMountOutsideJob) {
		t.Fatalf("%v: not ErrMountOutsideJob", err)
	}
	var me *MountOutsideJobError
	if !errors.As(err, &me) || me.What != what || me.Job != job {
		t.Fatalf("%v: want a %s refusal of job %s, got %+v", err, what, job, me)
	}
	for _, s := range []string{path, job} {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("%q does not name %q", err, s)
		}
	}
}

// A confined session whose every mount is inside its job or its
// connection mounts them all, its HOME made under the job.
func TestConfinedSessionMountsItsOwnJobAndConnection(t *testing.T) {
	jt := twoJobTree(t)
	hooks := jt.job("a", "checkout.hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	sess, _ := confinedSession(t, jt, ownStores(jt), hooks)
	if !strings.HasPrefix(sess.Home, jt.Homes+string(filepath.Separator)) {
		t.Fatalf("home %s not under %s", sess.Home, jt.Homes)
	}
	m, err := sess.CheckedMounts(jt.job("a", "checkout"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 5 {
		t.Fatalf("mounts = %+v", m)
	}
}

// A workspace outside the job — another job's checkout — is refused,
// and Run starts no agent.
func TestConfinedSessionRefusesAWorkspaceOutsideTheJob(t *testing.T) {
	jt := twoJobTree(t)
	sess, f := confinedSession(t, jt, ownStores(jt), "")
	ws := jt.job("b", "checkout")
	_, err := sess.CheckedMounts(ws)
	refusedNaming(t, err, "workspace", ws, "a")
	err = sess.Run(context.Background(), RunSpec{Workspace: ws, Command: []string{"claude", "-p"}, Stdin: strings.NewReader("p")})
	refusedNaming(t, err, "workspace", ws, "a")
	if len(f.Agents) != 0 {
		t.Fatalf("an agent started: %+v", f.Agents)
	}
	if _, err := sess.StartDetached(context.Background(), DetachedSpec{Workspace: ws, Command: []string{"claude"}}); !errors.Is(err, ErrMountOutsideJob) {
		t.Fatalf("detached: %v", err)
	}
	if len(f.Agents) != 0 || len(f.Pods) != 0 {
		t.Fatalf("a detached start made a pod or agent: %+v %+v", f.Pods, f.Agents)
	}
}

// A HOME outside the job is refused.
func TestConfinedSessionRefusesAHomeOutsideTheJob(t *testing.T) {
	jt := twoJobTree(t)
	sess, _ := confinedSession(t, jt, ownStores(jt), "")
	sess.Home = t.TempDir()
	_, err := sess.CheckedMounts(jt.job("a", "checkout"))
	refusedNaming(t, err, "home", sess.Home, "a")
}

// A worktree in job a whose .git names a clone in another connection's
// repos is refused at its git common dir.
func TestConfinedSessionRefusesACommonDirInAnotherConnectionsRepos(t *testing.T) {
	jt := twoJobTree(t)
	clone, _ := cloneAndWorktree(t)
	betaClone := jt.conn("cb", "repos", "owner", "name")
	if err := os.MkdirAll(filepath.Dir(betaClone), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, filepath.Dir(clone), "clone", "-q", clone, betaClone)
	ws := jt.job("a", "eval")
	gitRun(t, betaClone, "worktree", "add", "-q", "-b", "item-2", ws, "HEAD")
	sess, _ := confinedSession(t, jt, ownStores(jt), "")
	_, err := sess.CheckedMounts(ws)
	common, _ := filepath.EvalSymlinks(filepath.Join(betaClone, ".git"))
	refusedNaming(t, err, "git common dir", common, "a")
	if !strings.Contains(err.Error(), filepath.Join("connections", "cb", "repos")) {
		t.Fatalf("%v does not name cb's repos", err)
	}
}

// A package store of another connection is refused.
func TestConfinedSessionRefusesAnotherConnectionsStore(t *testing.T) {
	jt := twoJobTree(t)
	beta := jt.conn("cb", "store", "pnpm")
	sess, _ := confinedSession(t, jt, Stores{Pnpm: beta}, "")
	_, err := sess.CheckedMounts(jt.job("a", "checkout"))
	refusedNaming(t, err, "package store", beta, "a")
}

// A hooks dir outside the job is refused.
func TestConfinedSessionRefusesAHooksDirOutsideTheJob(t *testing.T) {
	jt := twoJobTree(t)
	hooks := t.TempDir()
	sess, _ := confinedSession(t, jt, ownStores(jt), hooks)
	_, err := sess.CheckedMounts(jt.job("a", "checkout"))
	refusedNaming(t, err, "hooks", hooks, "a")
}

// A symlink inside the job pointing out of it is followed and refused,
// naming the link and where it resolves.
func TestConfinedSessionRefusesASymlinkOutOfTheJob(t *testing.T) {
	jt := twoJobTree(t)
	link := jt.job("a", "rebase")
	target := jt.job("b", "checkout")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	sess, _ := confinedSession(t, jt, ownStores(jt), "")
	_, err := sess.CheckedMounts(link)
	refusedNaming(t, err, "workspace", link, "a")
	resolved, _ := filepath.EvalSymlinks(target)
	if !strings.Contains(err.Error(), resolved) {
		t.Fatalf("%v does not name where the link resolves, %s", err, resolved)
	}
}

// An unconfined session — one in no job — mounts as before.
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
