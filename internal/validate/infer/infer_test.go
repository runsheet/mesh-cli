package infer

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/validate"
)

// tree writes files (repo-relative path → content) under a new
// directory.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const gomod = "module example.com/m\n\ngo 1.24\n"

// Each fixture tree infers the suites, by name, kind and command, its
// files imply, read from the files named — and nothing else.
func TestInfer(t *testing.T) {
	type want struct {
		name, kind string
		// command holds each of these
		has []string
	}
	for _, c := range []struct {
		name  string
		files map[string]string
		from  []string
		want  []want
		note  string
	}{
		{
			name:  "go only",
			files: map[string]string{"go.mod": gomod, "a/a.go": "package a\n", "a/a_test.go": "package a\n"},
			from:  []string{"go.mod"},
			want: []want{
				{"go-build", "build", []string{"go build ./..."}},
				{"go-vet", "lint", []string{"go vet ./..."}},
				{"go-test", "unit", []string{"go test -json", "${MESH_SUITE_TARGETS:-./...}", "MESH_SUITE_TESTS"}},
			},
		},
		{
			name:  "go with no tests",
			files: map[string]string{"go.mod": gomod, "main.go": "package main\n"},
			from:  []string{"go.mod"},
			want:  []want{{"go-build", "build", nil}, {"go-vet", "lint", nil}},
			note:  "no _test.go file",
		},
		{
			name: "go.work and nested modules",
			files: map[string]string{
				"go.work":          "go 1.24\n\nuse (\n\t.\n\t./tools // the tools\n)\nuse ./deep/mod\n",
				"go.mod":           gomod,
				"tools/go.mod":     gomod,
				"tools/x_test.go":  "package tools\n",
				"deep/mod/go.mod":  gomod,
				"svc/go.mod":       gomod,
				"vendor/v/go.mod":  gomod,
				"node_modules/x/y": "",
			},
			from: []string{"go.work", "go.mod", "deep/mod/go.mod", "svc/go.mod", "tools/go.mod"},
			want: []want{
				{"go-build", "build", []string{`for d in . deep/mod svc tools; do (cd "$d" && go build ./...)`}},
				{"go-vet", "lint", []string{"go vet ./..."}},
				{"go-test", "unit", []string{`for d in . deep/mod svc tools`, "go test -json"}},
			},
		},
		{
			name: "pnpm only",
			files: map[string]string{
				"package.json":   `{"packageManager":"pnpm@9.12.0","scripts":{"lint":"eslint .","test":"vitest run","build":"vite build","dev":"vite"}}`,
				"pnpm-lock.yaml": "lockfileVersion: '9.0'\n",
			},
			from: []string{"package.json", "pnpm-lock.yaml"},
			want: []want{
				{"node-install", "build", []string{"pnpm install --frozen-lockfile"}},
				{"node-lint", "lint", []string{"CI=true", "pnpm run lint"}},
				{"node-test", "unit", []string{"pnpm run test"}},
				{"node-build", "build", []string{"pnpm run build"}},
			},
		},
		{
			name: "npm, its placeholder test passed over",
			files: map[string]string{
				"package.json":      `{"scripts":{"typecheck":"tsc --noEmit","test":"echo \"Error: no test specified\" && exit 1"}}`,
				"package-lock.json": "{}",
			},
			from: []string{"package.json", "package-lock.json"},
			want: []want{
				{"node-install", "build", []string{"npm ci"}},
				{"node-typecheck", "typecheck", []string{"npm run typecheck"}},
			},
		},
		{
			name:  "yarn berry",
			files: map[string]string{"package.json": `{"packageManager":"yarn@4.1.0","scripts":{"test":"jest"}}`, "yarn.lock": ""},
			from:  []string{"package.json", "yarn.lock"},
			want:  []want{{"node-install", "build", []string{"yarn install --immutable"}}, {"node-test", "unit", []string{"yarn run test"}}},
		},
		{
			name:  "package.json with no package manager",
			files: map[string]string{"package.json": `{"scripts":{"test":"jest"}}`, "Makefile": "test:\n\tgo test ./...\n"},
			from:  []string{"Makefile"},
			want:  []want{{"make-test", "unit", []string{"make test"}}},
			note:  "no packageManager field and no lockfile",
		},
		{
			name: "go and pnpm side by side",
			files: map[string]string{
				"go.mod": gomod, "x_test.go": "package m\n",
				"package.json": `{"scripts":{"build":"tsc"}}`, "pnpm-lock.yaml": "",
				"Makefile": "test:\n\tgo test ./...\n",
			},
			from: []string{"go.mod", "package.json", "pnpm-lock.yaml"},
			want: []want{
				{"go-build", "build", nil}, {"go-vet", "lint", nil}, {"go-test", "unit", nil},
				{"node-install", "build", nil}, {"node-build", "build", []string{"pnpm run build"}},
			},
			note: "Makefile is not read",
		},
		{
			name: "go and node in first-level directories",
			files: map[string]string{
				"api/go.mod": gomod, "api/main.go": "package main\n",
				"web/package.json": `{"packageManager":"pnpm@9.0.0","scripts":{"test":"vitest run"}}`, "web/pnpm-lock.yaml": "",
				"docs/README.md": "",
			},
			from: []string{"api/go.mod", "web/package.json", "web/pnpm-lock.yaml"},
			want: []want{
				{"go-build", "build", []string{`for d in api; do`}}, {"go-vet", "lint", nil},
				{"node-install-web", "build", []string{"cd web && pnpm install --frozen-lockfile"}},
				{"node-test-web", "unit", []string{"cd web && pnpm run test"}},
			},
		},
		{
			name:  "Makefile only",
			files: map[string]string{"Makefile": "CHECK := x\ncheck: lint\n\t./lint.sh\ntest :\n\t./t.sh\ntests:\n\techo no\n"},
			from:  []string{"Makefile"},
			want:  []want{{"make-check", "unit", []string{"make check"}}, {"make-test", "unit", []string{"make test"}}},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, p, err := Infer(tree(t, c.files))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(p.From, c.from) {
				t.Errorf("from %q, want %q", p.From, c.from)
			}
			var names []string
			for _, w := range c.want {
				names = append(names, w.name)
			}
			if !reflect.DeepEqual(p.Suites, names) {
				t.Fatalf("suites %q, want %q\n%s", p.Suites, names, p.File)
			}
			for i, w := range c.want {
				st := cfg.Steps[i]
				if string(st.Kind) != w.kind {
					t.Errorf("%s kind %q, want %q", st.Name, st.Kind, w.kind)
				}
				for _, h := range w.has {
					if !strings.Contains(st.Command, h) {
						t.Errorf("%s command %q, want it to hold %q", st.Name, st.Command, h)
					}
				}
				if st.CPUs != CPUs || st.Memory != Memory {
					t.Errorf("%s asks for %g CPUs and %q, want %d and %q", st.Name, st.CPUs, st.Memory, CPUs, Memory)
				}
				if st.Group != "" || st.Shards != 0 || len(st.Paths) != 0 {
					t.Errorf("%s has group %q, shards %d, paths %q; want none", st.Name, st.Group, st.Shards, st.Paths)
				}
			}
			if c.note != "" && !strings.Contains(strings.Join(p.Notes, "\n"), c.note) {
				t.Errorf("notes %q, want one holding %q", p.Notes, c.note)
			}
			// the profile's file is the config, read back as a declared one
			again, err := validate.Parse([]byte(p.File))
			if err != nil {
				t.Fatal(err)
			}
			if again.Hash() != p.SuiteHash || cfg.Hash() != p.SuiteHash {
				t.Errorf("suite hash %s, the file's %s", p.SuiteHash, again.Hash())
			}
		})
	}
}

// A checkout with nothing Infer recognises is ErrNothingToInfer, named
// with what was looked for and what was found and passed over.
func TestInferNothingToRun(t *testing.T) {
	_, _, err := Infer(tree(t, map[string]string{"README.md": "hi", "package.json": "{}", "Makefile": "all:\n\techo\n"}))
	if !errors.Is(err, ErrNothingToInfer) {
		t.Fatalf("err %v, want ErrNothingToInfer", err)
	}
	for _, want := range []string{"no .runsheet/validate.yml", "go.mod", "package.json", "pnpm-lock.yaml", "Makefile", "test or check",
		"package.json has no packageManager field", "Makefile has no target named test or check"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q, want it to name %q", err, want)
		}
	}
}

// An explicit validate.yml wins whole: Load answers its suites and no
// profile, whatever else the checkout holds; one that does not parse is
// its error, never an inference in its place.
func TestLoadPrefersTheDeclaredFile(t *testing.T) {
	dir := tree(t, map[string]string{
		"go.mod":                 gomod,
		".runsheet/validate.yml": "suites:\n  - name: mine\n    kind: unit\n    command: ./t.sh\n",
	})
	cfg, p, err := Load(dir)
	if err != nil || p != nil || len(cfg.Steps) != 1 || cfg.Steps[0].Name != "mine" {
		t.Fatalf("Load = %+v, %+v, %v; want the declared suite alone", cfg.Steps, p, err)
	}

	bad := tree(t, map[string]string{"go.mod": gomod, ".runsheet/validate.yml": "suites:\n  - name: x\n"})
	if _, p, err := Load(bad); err == nil || p != nil {
		t.Fatalf("Load of a broken file = %+v, %v; want its parse error", p, err)
	}

	cfg, p, err = Load(tree(t, map[string]string{"go.mod": gomod}))
	if err != nil || p == nil || p.Summary() != "inferred suites go-build, go-vet from go.mod" || len(cfg.Steps) != 2 {
		t.Fatalf("Load with no file = %+v, %+v, %v", cfg.Steps, p, err)
	}
}
