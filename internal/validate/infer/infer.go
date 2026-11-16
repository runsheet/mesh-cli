// Package infer works out a repository's validation from the repository
// itself when it declares none (ADR-053; runsheet goal
// mesh-works-out-how-to-build-and-test-a-repo): a checkout with no
// .runsheet/validate.yml is read for go.mod, go.work, package.json and a
// Makefile, and the suites they imply are written as a validate.yml and
// read back through validate.Parse, so what runs is exactly what the
// same file committed to the repository would run.
//
// An explicit .runsheet/validate.yml always wins, whole: Load never
// merges an inferred suite into a declared file.
//
// The rules, in order:
//
//   - Go: each module — the root's go.mod, every `use` of a root go.work,
//     and each first-level directory holding a go.mod — is built
//     (go-build: go build ./...) and vetted (go-vet: go vet ./...), and,
//     when any of them has a _test.go file, tested (go-test: go test
//     -json ./..., results on stdout).
//   - Node: the root's package.json, or, when the root has none, each
//     first-level directory's, whose package manager is named by its
//     packageManager field or its lockfile (pnpm-lock.yaml,
//     package-lock.json, yarn.lock), is installed from the lockfile
//     (node-install) and then runs its lint, typecheck, test and build
//     scripts — only the ones package.json defines, never one it does
//     not, and never npm's "no test specified" placeholder.
//   - Makefile: only when neither of those gives a test, the root
//     Makefile's targets literally named test and check.
//   - Documents: only when none of those gives any suite, a checkout
//     that holds only documents (DocumentsOnly: Markdown and the like,
//     outside its dot-paths) runs docs, the node's built-in docs check
//     (docscheck; ADR-053 amendment 2026-11-16).
//
// Every inferred suite asks for 4 CPUs and 8g, what a lane held before
// suites named their own room (ADR-049 amendment), and none names a
// group, so they run one after another in the lease's own pod.
package infer

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"projectmesh.io/mesh-cli/internal/validate"
)

// ErrNothingToInfer is Infer's answer for a checkout with nothing it
// knows how to build or test; the error it is wrapped in names what was
// looked for.
var ErrNothingToInfer = errors.New("infer: nothing to build or test")

// lookedFor is what Infer reads, as its cannot-run error names it.
const lookedFor = "go.mod or go.work; package.json with a packageManager field or a lockfile (pnpm-lock.yaml, package-lock.json, yarn.lock); " +
	"a Makefile with a target named test or check — at the repository's root, and go.mod and package.json in its first-level directories; " +
	"or only documents (Markdown, text and images, outside its dot-paths) for the node's docs check"

// The room every inferred suite asks for (validate.Step.CPUs, Memory).
const (
	CPUs   = 4
	Memory = "8g"
)

// goPath puts the usual Go install directories on PATH behind the
// pod's own, as every Go suite in the estate's validate.yml files does:
// a sandbox's shell PATH may not name the toolchain.
const goPath = `export PATH="$PATH:/usr/local/go/bin:$HOME/go/bin:$HOME/sdk/go/bin"; `

// Profile is what Infer learned: what it read, the suites it inferred,
// what it passed over and why, and the validate.yml it wrote — the file
// a product may store, or a repository commit, to run the same suites
// declared.
type Profile struct {
	// From are the repo-relative files the suites were inferred from.
	From []string `json:"from"`
	// Suites are the inferred suites' names, in the order they run.
	Suites []string `json:"suites"`
	// Notes say what was found and passed over, and why.
	Notes []string `json:"notes,omitempty"`
	// File is the inferred validate.yml.
	File string `json:"validate_yml"`
	// SuiteHash is validate.SuiteHash of File.
	SuiteHash string `json:"suite_hash"`
}

// Summary is the profile in one line: "inferred suites go-build,
// go-vet, go-test from go.mod".
func (p Profile) Summary() string {
	return fmt.Sprintf("inferred suites %s from %s", strings.Join(p.Suites, ", "), strings.Join(p.From, ", "))
}

// Load is the checkout's validation: its .runsheet/validate.yml when it
// has one (and a nil Profile), else what Infer finds. A validate.yml
// that does not parse is that error, never an inference in its place.
func Load(dir string) (validate.Config, *Profile, error) {
	cfg, err := validate.Load(dir)
	if !errors.Is(err, validate.ErrNoValidation) {
		return cfg, nil, err
	}
	cfg, p, err := Infer(dir)
	if err != nil {
		return validate.Config{}, nil, err
	}
	return cfg, &p, nil
}

// suite is one inferred suite as the file writes it.
type suite struct {
	Name    string  `yaml:"name"`
	Kind    string  `yaml:"kind"`
	Command string  `yaml:"command"`
	Timeout string  `yaml:"timeout,omitempty"`
	Results string  `yaml:"results,omitempty"`
	CPUs    float64 `yaml:"cpus"`
	Memory  string  `yaml:"memory"`
}

// inference is Infer's working state.
type inference struct {
	dir    string
	from   []string
	notes  []string
	suites []suite
}

func (in *inference) read(rel string) { in.from = appendOnce(in.from, rel) }
func (in *inference) note(format string, args ...any) {
	in.notes = append(in.notes, fmt.Sprintf(format, args...))
}
func (in *inference) add(s suite) {
	s.CPUs, s.Memory = CPUs, Memory
	in.suites = append(in.suites, s)
}

// hasTest says whether an inferred suite is a test: a unit suite.
func (in *inference) hasTest() bool {
	return slices.ContainsFunc(in.suites, func(s suite) bool { return s.Kind == string(validate.KindUnit) })
}

// Infer reads dir — a checkout — for what builds and tests it, and
// answers the validation it implies, parsed from the validate.yml it
// writes (Profile.File), as validate.Load would answer that file. A
// checkout with nothing it recognises is ErrNothingToInfer, wrapped in
// an error naming what was looked for.
func Infer(dir string) (validate.Config, Profile, error) {
	in := &inference{dir: dir}
	in.goModules()
	in.node()
	in.makefile()
	if len(in.suites) == 0 {
		in.docs()
	}
	if len(in.suites) == 0 {
		msg := fmt.Sprintf("no .runsheet/validate.yml, and nothing to infer its suites from: looked for %s", lookedFor)
		if len(in.notes) > 0 {
			msg += "; " + strings.Join(in.notes, "; ")
		}
		return validate.Config{}, Profile{}, fmt.Errorf("%w: %s", ErrNothingToInfer, msg)
	}
	file, err := yaml.Marshal(map[string]any{"suites": in.suites})
	if err != nil {
		return validate.Config{}, Profile{}, err
	}
	header := "# inferred by the node from " + strings.Join(in.from, ", ") +
		" (no .runsheet/validate.yml); commit as .runsheet/validate.yml to change it\n"
	file = append([]byte(header), file...)
	cfg, err := validate.Parse(file)
	if err != nil {
		// the suites are the node's own: a refusal here is a bug in Infer
		return validate.Config{}, Profile{}, fmt.Errorf("infer: the inferred validate.yml does not parse: %w", err)
	}
	p := Profile{From: in.from, Notes: in.notes, File: string(file), SuiteHash: cfg.Hash()}
	for _, s := range cfg.Steps {
		p.Suites = append(p.Suites, s.Name)
	}
	return cfg, p, nil
}

// skipDir says a directory is never a module or package of the
// repository's own: hidden, vendored, a dependency tree or a fixture.
func skipDir(name string) bool {
	return strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules" || name == "testdata" || name == "third_party"
}

// firstLevel are dir's first-level directories, skipDir's passed over,
// in name order.
func (in *inference) firstLevel() []string {
	entries, err := os.ReadDir(in.dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && !skipDir(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

func (in *inference) exists(rel string) bool {
	st, err := os.Stat(filepath.Join(in.dir, filepath.FromSlash(rel)))
	return err == nil && !st.IsDir()
}

// goModules infers go-build, go-vet and go-test from the repository's
// Go modules.
func (in *inference) goModules() {
	var mods []string
	if in.exists("go.work") {
		in.read("go.work")
		for _, u := range in.workUses() {
			if in.exists(path.Join(u, "go.mod")) {
				mods = appendOnce(mods, u)
			} else {
				in.note("go.work uses %s, which has no go.mod", u)
			}
		}
	}
	if in.exists("go.mod") {
		mods = appendOnce(mods, ".")
	}
	for _, d := range in.firstLevel() {
		if in.exists(d + "/go.mod") {
			mods = appendOnce(mods, d)
		}
	}
	if len(mods) == 0 {
		return
	}
	slices.SortFunc(mods, func(a, b string) int {
		// the root first, then the rest in name order
		if a == "." || b == "." {
			return boolCmp(b == ".", a == ".")
		}
		return strings.Compare(a, b)
	})
	tested := false
	for _, m := range mods {
		in.read(path.Join(m, "go.mod"))
		if in.goTests(m) {
			tested = true
		}
	}
	in.add(suite{Name: "go-build", Kind: string(validate.KindBuild), Command: goPath + eachModule(mods, "go build ./...")})
	in.add(suite{Name: "go-vet", Kind: string(validate.KindLint), Command: goPath + eachModule(mods, "go vet ./...")})
	if !tested {
		in.note("no _test.go file in %s: no go-test suite", strings.Join(mods, ", "))
		return
	}
	// a run of failing tests by name (MESH_SUITE_TESTS, the node's
	// supervisor) runs only those; MESH_SUITE_TARGETS narrows a single
	// root module's packages
	run := `run=; [ -n "${MESH_SUITE_TESTS:-}" ] && run="-run=^($(printf '%s' "$MESH_SUITE_TESTS" | tr ' ' '|'))\$"; `
	test := `go test -json ${run:+"$run"} ./...`
	if len(mods) == 1 && mods[0] == "." {
		test = `go test -json ${run:+"$run"} ${MESH_SUITE_TARGETS:-./...}`
	}
	in.add(suite{Name: "go-test", Kind: string(validate.KindUnit), Command: goPath + run + eachModule(mods, test),
		Timeout: "15m", Results: "stdout"})
}

// eachModule is cmd run in each module: in the root as it is, otherwise
// in each module's directory, every module run whatever an earlier one
// did and the suite red when any is.
func eachModule(mods []string, cmd string) string {
	if len(mods) == 1 && mods[0] == "." {
		return cmd
	}
	return fmt.Sprintf(`rc=0; for d in %s; do (cd "$d" && %s) || rc=1; done; exit $rc`, strings.Join(mods, " "), cmd)
}

// workUses are the directories go.work's use directives name, cleaned
// and repo-relative; one outside the repository is passed over.
func (in *inference) workUses() []string {
	f, err := os.Open(filepath.Join(in.dir, "go.work"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	block := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		var dir string
		switch {
		case block && line == ")":
			block = false
			continue
		case block:
			dir = line
		case line == "use (":
			block = true
			continue
		case strings.HasPrefix(line, "use "):
			dir = strings.TrimSpace(strings.TrimPrefix(line, "use "))
		default:
			continue
		}
		dir = path.Clean(strings.Trim(dir, `"`))
		if dir == "" || path.IsAbs(dir) || dir == ".." || strings.HasPrefix(dir, "../") {
			if dir != "" {
				in.note("go.work uses %s, outside the repository", dir)
			}
			continue
		}
		out = appendOnce(out, dir)
	}
	return out
}

// errFound stops goTests' walk at its first test file.
var errFound = errors.New("found")

// goTests says whether module m has a _test.go file of its own: a
// nested module's directory is its own, not m's.
func (in *inference) goTests(m string) bool {
	root := filepath.Join(in.dir, filepath.FromSlash(m))
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p == root {
				return nil
			}
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), "_test.go") {
			return errFound
		}
		return nil
	})
	return errors.Is(err, errFound)
}

// packageJSON is what Infer reads of a package.json.
type packageJSON struct {
	PackageManager string            `json:"packageManager"`
	Scripts        map[string]string `json:"scripts"`
}

// The scripts Infer runs, in order, and the kind each is.
var nodeScripts = []struct {
	name string
	kind validate.Kind
}{
	{"lint", validate.KindLint},
	{"typecheck", validate.KindTypecheck},
	{"test", validate.KindUnit},
	{"build", validate.KindBuild},
}

// node infers node-install and a suite per script from the root's
// package.json, or each first-level directory's when the root has none.
func (in *inference) node() {
	if in.exists("package.json") {
		in.nodePackage(".")
		for _, d := range in.firstLevel() {
			if in.exists(d + "/package.json") {
				in.note("%s/package.json is not read on its own: the root's package.json is taken to drive it", d)
			}
		}
		return
	}
	for _, d := range in.firstLevel() {
		if in.exists(d + "/package.json") {
			in.nodePackage(d)
		}
	}
}

// nodePackage infers the suites of the package in dir.
func (in *inference) nodePackage(dir string) {
	rel := func(name string) string { return path.Join(dir, name) }
	data, err := os.ReadFile(filepath.Join(in.dir, filepath.FromSlash(rel("package.json"))))
	if err != nil {
		return
	}
	var pkg packageJSON
	if err := json.Unmarshal(data, &pkg); err != nil {
		in.note("%s does not parse: %v", rel("package.json"), err)
		return
	}
	pm, version := "", ""
	if pkg.PackageManager != "" {
		pm, version, _ = strings.Cut(pkg.PackageManager, "@")
		switch pm {
		case "pnpm", "npm", "yarn":
		default:
			in.note("%s names packageManager %q, which the node does not run", rel("package.json"), pkg.PackageManager)
			return
		}
	}
	lock := ""
	for _, l := range []struct{ file, pm string }{{"pnpm-lock.yaml", "pnpm"}, {"package-lock.json", "npm"}, {"yarn.lock", "yarn"}} {
		if in.exists(rel(l.file)) && (pm == "" || pm == l.pm) {
			lock, pm = l.file, l.pm
			break
		}
	}
	if pm == "" {
		in.note("%s has no packageManager field and no lockfile beside it: not installed", rel("package.json"))
		return
	}
	in.read(rel("package.json"))
	if lock != "" {
		in.read(rel(lock))
	}
	var install string
	switch {
	case pm == "pnpm" && lock != "":
		install = "pnpm install --frozen-lockfile"
	case pm == "pnpm":
		install = "pnpm install"
	case pm == "npm" && lock != "":
		install = "npm ci"
	case pm == "npm":
		install = "npm install"
	case pm == "yarn" && (in.exists(rel(".yarnrc.yml")) || (version != "" && version[0] >= '2' && version[0] <= '9')):
		install = "yarn install --immutable"
	case pm == "yarn" && lock != "":
		install = "yarn install --frozen-lockfile"
	default:
		install = "yarn install"
	}
	if lock == "" {
		in.note("%s names %s but has no lockfile: installed with %q, not from a frozen lockfile", rel("package.json"), pm, install)
	}
	prefix := "export CI=true; "
	suffix := ""
	if dir != "." {
		prefix += "cd " + dir + " && "
		suffix = "-" + dir
	}
	in.add(suite{Name: "node-install" + suffix, Kind: string(validate.KindBuild), Command: prefix + install})
	for _, s := range nodeScripts {
		body := strings.TrimSpace(pkg.Scripts[s.name])
		if body == "" || (s.name == "test" && strings.Contains(body, "no test specified")) {
			continue
		}
		st := suite{Name: "node-" + s.name + suffix, Kind: string(s.kind), Command: prefix + pm + " run " + s.name}
		if s.kind == validate.KindUnit {
			st.Timeout = "15m"
		}
		in.add(st)
	}
}

// makeTarget is a Makefile rule for test or check: the target at the
// start of a line, then a colon that is not an assignment's.
var makeTarget = regexp.MustCompile(`(?m)^(test|check)[ \t]*:([^=]|$)`)

// makefile infers make-test and make-check from the root's Makefile,
// only when neither Go nor Node gave a test.
func (in *inference) makefile() {
	// make's own order; read from the directory, since a Stat on a
	// case-insensitive file system finds Makefile as makefile
	entries, _ := os.ReadDir(in.dir)
	name := ""
	for _, n := range []string{"GNUmakefile", "makefile", "Makefile"} {
		if slices.ContainsFunc(entries, func(e fs.DirEntry) bool { return e.Name() == n && !e.IsDir() }) {
			name = n
			break
		}
	}
	if name == "" {
		return
	}
	if in.hasTest() {
		in.note("%s is not read: go.mod or package.json already gives a test", name)
		return
	}
	data, err := os.ReadFile(filepath.Join(in.dir, name))
	if err != nil {
		return
	}
	var targets []string
	for _, m := range makeTarget.FindAllStringSubmatch(string(data), -1) {
		targets = appendOnce(targets, m[1])
	}
	if len(targets) == 0 {
		in.note("%s has no target named test or check", name)
		return
	}
	in.read(name)
	slices.Sort(targets) // check before test
	for _, t := range targets {
		in.add(suite{Name: "make-" + t, Kind: string(validate.KindUnit), Command: "make " + t, Timeout: "15m"})
	}
}

func appendOnce(ss []string, s string) []string {
	if slices.Contains(ss, s) {
		return ss
	}
	return append(ss, s)
}

func boolCmp(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}
