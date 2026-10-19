package node

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

const scratchValidateYML = `suites:
  - name: scratch
    kind: unit
    always: true
    timeout: 1m
    command: |
      d=$(mktemp -d)
      touch "$d/left"
      {
        echo "MKTEMP=$d"
        echo "TMPDIR=$TMPDIR"
        echo "GOCACHE=$(go env GOCACHE)"
        echo "GOTMPDIR=$(go env GOTMPDIR)"
      } > "$OUT.part"
      mv "$OUT.part" "$OUT"
      if [ "$MODE" = hang ]; then sleep 60; fi
      [ "$MODE" = pass ]
`

// scratchRepo is a checkout of two commits whose validate.yml runs the
// scratch suite on every change.
func scratchRepo(t *testing.T) (dir, first string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "work", "g", "1")
	if err := os.MkdirAll(filepath.Join(dir, ".runsheet"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dir, "init", "--quiet", "-b", "main")
	gitOut(t, dir, "config", "user.email", "t@example.com")
	gitOut(t, dir, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, ".runsheet", "validate.yml"), []byte(scratchValidateYML), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dir, "add", ".")
	gitOut(t, dir, "commit", "--quiet", "-m", "base")
	first = strings.TrimSpace(gitOut(t, dir, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(dir, "x.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dir, "add", ".")
	gitOut(t, dir, "commit", "--quiet", "-m", "change")
	return dir, first
}

func readEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the step's report: %v", err)
	}
	out := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		k, v, _ := strings.Cut(l, "=")
		out[k] = v
	}
	return out
}

func dirNames(t *testing.T, dir string) map[string]bool {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, e := range es {
		out[e.Name()] = true
	}
	return out
}

// within reports whether path is dir or under it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// TestHostValidationRunsInItsOwnScratch: a host validation's TMPDIR and
// GOTMPDIR are a scratch dir of its own under the node's scratch root,
// its GOCACHE the node's build cache, and when it ends — passed, failed
// or its ctx cancelled — the scratch is gone and the node's TMPDIR holds
// nothing new.
func TestHostValidationRunsInItsOwnScratch(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	for _, mode := range []string{"pass", "fail", "hang"} {
		t.Run(mode, func(t *testing.T) {
			dir, first := scratchRepo(t)
			base := t.TempDir()
			workRoot := filepath.Join(base, "work-root")
			cache := filepath.Join(base, "cache", "go")
			nodeTmp := filepath.Join(base, "tmp")
			out := filepath.Join(base, "report")
			if err := os.MkdirAll(nodeTmp, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TMPDIR", nodeTmp)
			t.Setenv("GOTMPDIR", "")
			t.Setenv("GOCACHE", "")
			before := dirNames(t, nodeTmp)

			vr := validator{logf: t.Logf, WorkRoot: workRoot, BuildCache: cache}
			tg := validationTarget{item: gateway.WorkItem{ID: "item-1"}, dir: dir, base: "main", diffFrom: first,
				prefix: "item g#1", label: "g#1", session: func() string { return "s" },
				job: &harness.Job{Env: []string{"OUT=" + out, "MODE=" + mode}}}
			v, err := vr.start(context.Background(), tg)
			if err != nil || v == nil {
				t.Fatalf("start: %v, %v", v, err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "hang" {
				go func() {
					for ctx.Err() == nil {
						if _, err := os.Stat(out); err == nil {
							cancel()
							return
						}
						time.Sleep(20 * time.Millisecond)
					}
				}()
			}
			att, err := func() (any, error) {
				defer v.close()
				return vr.run(ctx, tg, v)
			}()
			switch mode {
			case "hang":
				if err == nil {
					t.Fatalf("a cancelled run = %+v, nil; want its ctx's error", att)
				}
			default:
				if err != nil {
					t.Fatalf("run: %v", err)
				}
			}

			seen := readEnvFile(t, out)
			root, _ := ScratchRoot(workRoot)
			scratch := seen["TMPDIR"]
			if filepath.Dir(scratch) != root || !strings.HasPrefix(filepath.Base(scratch), "validate-item-1-") {
				t.Fatalf("the step's TMPDIR = %q, want a validate-item-1-* dir under %s", scratch, root)
			}
			if seen["GOTMPDIR"] != scratch {
				t.Fatalf("the step's GOTMPDIR = %q, want its scratch %s", seen["GOTMPDIR"], scratch)
			}
			if !within(seen["MKTEMP"], scratch) {
				t.Fatalf("mktemp -d made %q, want it under the scratch %s", seen["MKTEMP"], scratch)
			}
			if seen["GOCACHE"] != cache {
				t.Fatalf("the step's GOCACHE = %q, want the node's build cache %s", seen["GOCACHE"], cache)
			}
			if _, err := os.Stat(scratch); !os.IsNotExist(err) {
				t.Fatalf("the run's scratch %s is still there after the validation ended (%v)", scratch, err)
			}
			for name := range dirNames(t, nodeTmp) {
				if !before[name] {
					t.Fatalf("the node's TMPDIR gained %s", name)
				}
			}
		})
	}
}

func TestHostValidationEnvCarriesOneOfEach(t *testing.T) {
	t.Setenv("TMPDIR", "/tmp/node")
	t.Setenv("GOTMPDIR", "/tmp/node-go")
	t.Setenv("GOCACHE", "/home/u/.cache/go-build")
	for _, job := range []*harness.Job{nil, {Env: []string{"TMPDIR=/tmp/job", "GOCACHE=/tmp/job-cache", "GOTMPDIR=/tmp/job-go", "A=b"}}} {
		env := hostValidationEnv(job, "/w/.scratch/validate-x-1", "/c/mesh/cache/go")
		want := map[string]string{"TMPDIR": "/w/.scratch/validate-x-1", "GOTMPDIR": "/w/.scratch/validate-x-1", "GOCACHE": "/c/mesh/cache/go"}
		got := map[string][]string{}
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			got[k] = append(got[k], v)
		}
		for k, w := range want {
			if len(got[k]) != 1 || got[k][0] != w {
				t.Fatalf("job %v: %s = %q, want exactly one %q", job != nil, k, got[k], w)
			}
		}
		if job != nil && (len(got["A"]) != 1 || got["A"][0] != "b") {
			t.Fatalf("the job's own A = %q, want b", got["A"])
		}
	}
}

func TestRemoveScratchTakesReadOnlyDirs(t *testing.T) {
	root := t.TempDir()
	dir, err := MakeScratch(root, "validate", "item-1")
	if err != nil {
		t.Fatal(err)
	}
	mod := filepath.Join(dir, "gopath", "pkg", "mod", "x@v1")
	if err := os.MkdirAll(mod, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mod, "f.go"), []byte("package x\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(mod, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := RemoveScratch(dir); err != nil {
		t.Fatalf("RemoveScratch: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("%s still there: %v", dir, err)
	}
	if err := RemoveScratch(dir); err != nil {
		t.Fatalf("RemoveScratch of a gone dir: %v", err)
	}
}

// TestMakeScratchNameIsShort: a run's scratch dir keeps its subject's
// first eight characters, so a step's own nesting under its TMPDIR — a
// test Postgres's socket, <tmp>/latchkey-testenv.XXXXXX/.s.PGSQL.5432 —
// still fits the 107 bytes a Unix socket path may have under the node's
// usual work root.
func TestMakeScratchNameIsShort(t *testing.T) {
	root := t.TempDir()
	dir, err := MakeScratch(root, "validate", "847dc509-c104-5a3f-932a-992bd9c2edb3")
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(dir)
	if !strings.HasPrefix(base, "validate-847dc509-") || len(base) != len("validate-847dc509-")+8 {
		t.Fatalf("scratch name %q, want validate-847dc509-<8 hex>", base)
	}
	socket := filepath.Join("/home/anythink/.mesh/work", ScratchDirName, base, "latchkey-testenv.C3G7QE", ".s.PGSQL.5432")
	if len(socket) > 107 {
		t.Fatalf("a test Postgres socket under the scratch is %d bytes, over 107: %s", len(socket), socket)
	}
	if dir, err := MakeScratch(root, "validate", "a-b-c-d-e-f-g"); err != nil || strings.Contains(filepath.Base(dir), "--") {
		t.Fatalf("a cut subject ends on a dash: %q %v", dir, err)
	}
}
