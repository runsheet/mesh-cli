package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func storesSandbox(t *testing.T) (*Sandbox, Stores) {
	t.Helper()
	sb := testSandbox(t, &Fake{})
	sb.Logf = t.Logf
	root := filepath.Join(t.TempDir(), "store")
	sb.Config.Stores = Stores{Pnpm: filepath.Join(root, "pnpm"), Npm: filepath.Join(root, "npm")}
	return sb, sb.Config.Stores
}

// Every session of a node installs against the node's one pnpm store and
// one npm cache: two sessions of different items get the same three
// variables, naming the node's dirs (mesh-specs ADR 017 §3).
func TestSessionEnvNamesTheNodesStores(t *testing.T) {
	sb, st := storesSandbox(t)
	want := []string{"npm_config_store_dir=" + st.Pnpm, "pnpm_config_store_dir=" + st.Pnpm, "PNPM_HOME=" + st.Pnpm, "npm_config_cache=" + st.Npm}
	var envs [][]string
	for _, label := range []string{"ship-it/2", "other-goal/7"} {
		sess, err := sb.Open(label, "build", GitIdentity{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { sess.Close() })
		env := sess.Env([]string{"npm_config_store_dir=/elsewhere", "GH_TOKEN=t"})
		for _, kv := range want {
			if !slices.Contains(env, kv) {
				t.Errorf("%s: env lacks %s: %v", label, kv, env)
			}
		}
		if slices.Contains(env, "npm_config_store_dir=/elsewhere") {
			t.Errorf("%s: the job's own store reached the pod: %v", label, env)
		}
		var stores []string
		for _, kv := range env {
			for _, w := range want {
				if kv == w {
					stores = append(stores, kv)
				}
			}
		}
		envs = append(envs, stores)
	}
	if !slices.Equal(envs[0], envs[1]) {
		t.Errorf("two items' sessions name different stores: %v and %v", envs[0], envs[1])
	}
	for _, d := range []string{st.Pnpm, st.Npm} {
		if info, err := os.Stat(d); err != nil || !info.IsDir() {
			t.Errorf("Open did not make %s: %v", d, err)
		}
	}
}

// The store and the cache are mounted read-write, each at its own path.
func TestMountsIncludeTheNodesStores(t *testing.T) {
	sb, st := storesSandbox(t)
	sess, err := sb.Open("ship-it/2", "build", GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	m := sess.Mounts(t.TempDir())
	for _, d := range []string{st.Pnpm, st.Npm} {
		if !slices.Contains(m, Mount{Source: d, Target: d}) {
			t.Errorf("mounts lack %s read-write: %+v", d, m)
		}
	}
}

// A node that names no stores sets no package variable and mounts none.
func TestNoStoresNoPackageEnv(t *testing.T) {
	sb := testSandbox(t, &Fake{})
	sb.Logf = t.Logf
	sess, err := sb.Open("ship-it/2", "build", GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	for _, kv := range sess.Env(nil) {
		for _, p := range []string{"npm_config_", "pnpm_config_", "PNPM_HOME"} {
			if len(kv) >= len(p) && kv[:len(p)] == p {
				t.Errorf("env holds %s with no stores", kv)
			}
		}
	}
	if n := len(sess.Mounts(t.TempDir())); n != 2 {
		t.Errorf("mounts = %d, want the workspace and HOME alone", n)
	}
}
