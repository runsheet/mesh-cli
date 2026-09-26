package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// WorkflowsDir is where a repository's CI workflows live.
const WorkflowsDir = ".github/workflows"

// testCommand matches a run step that runs tests.
var testCommand = regexp.MustCompile(`\b(go test|(npm|pnpm|yarn)( run)? test|pytest|make test|cargo test|vitest|jest)\b`)

// workflow is the part of a GitHub Actions workflow Resolve reads.
type workflow struct {
	Name string            `yaml:"name"`
	Env  map[string]string `yaml:"env"`
	Jobs yaml.Node         `yaml:"jobs"`
}

type job struct {
	Name     string            `yaml:"name"`
	Env      map[string]string `yaml:"env"`
	Strategy struct {
		Matrix map[string]yaml.Node `yaml:"matrix"`
	} `yaml:"strategy"`
	Services yaml.Node `yaml:"services"`
	Steps    []struct {
		Run string `yaml:"run"`
	} `yaml:"steps"`
}

// fromWorkflows merges the services of every job that runs tests in
// repoDir's workflows, by name, first file then first job winning. They
// are not required: reading a workflow is a best effort.
func fromWorkflows(repoDir string) ([]Service, []string) {
	var files []string
	for _, pat := range []string{"*.yml", "*.yaml"} {
		m, _ := filepath.Glob(filepath.Join(repoDir, WorkflowsDir, pat))
		files = append(files, m...)
	}
	sort.Strings(files)
	var out []Service
	var notes []string
	seen := map[string]bool{}
	for _, f := range files {
		rel, _ := filepath.Rel(repoDir, f)
		rel = filepath.ToSlash(rel)
		b, err := os.ReadFile(f)
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s: %v", rel, err))
			continue
		}
		var wf workflow
		if err := yaml.Unmarshal(b, &wf); err != nil {
			notes = append(notes, fmt.Sprintf("%s skipped: %v", rel, err))
			continue
		}
		if wf.Jobs.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i+1 < len(wf.Jobs.Content); i += 2 {
			id := wf.Jobs.Content[i].Value
			var j job
			if err := wf.Jobs.Content[i+1].Decode(&j); err != nil {
				notes = append(notes, fmt.Sprintf("%s job %s skipped: %v", rel, id, err))
				continue
			}
			if j.Services.Kind != yaml.MappingNode || !runsTests(wf, id, j) {
				continue
			}
			resolve := resolver(wf, j)
			for k := 0; k+1 < len(j.Services.Content); k += 2 {
				name := j.Services.Content[k].Value
				if seen[name] {
					continue
				}
				var sp spec
				if err := j.Services.Content[k+1].Decode(&sp); err != nil {
					notes = append(notes, fmt.Sprintf("%s job %s service %s skipped: %v", rel, id, name, err))
					continue
				}
				s := build(name, sp, rel, resolve)
				if s.Image == "" {
					notes = append(notes, fmt.Sprintf("%s job %s service %s skipped: image %q cannot be resolved", rel, id, name, sp.Image))
					continue
				}
				seen[name] = true
				out = append(out, s)
			}
		}
	}
	return out, notes
}

// runsTests reports whether a job runs tests: a step runs a test command,
// or the job's id or name, or the workflow's name, says test or ci.
func runsTests(wf workflow, id string, j job) bool {
	for _, st := range j.Steps {
		if testCommand.MatchString(st.Run) {
			return true
		}
	}
	return namedTest(id) || namedTest(j.Name) || namedTest(wf.Name)
}

func namedTest(s string) bool {
	s = strings.ToLower(s)
	if strings.Contains(s, "test") {
		return true
	}
	for _, w := range strings.FieldsFunc(s, func(r rune) bool { return !('a' <= r && r <= 'z' || '0' <= r && r <= '9') }) {
		if w == "ci" {
			return true
		}
	}
	return false
}

var (
	exprRe   = regexp.MustCompile(`\$\{\{\s*(.*?)\s*\}\}`)
	numberRe = regexp.MustCompile(`^-?[0-9.]+$`)
)

// resolver replaces each ${{ … }} in a value by a literal it can know
// without running the workflow: a quoted or numeric literal, the first
// value of matrix.X, a literal env.X of the job or workflow, or the
// literal after `||`. Anything else fails the whole value.
func resolver(wf workflow, j job) func(string) (string, bool) {
	var term func(string) (string, bool)
	term = func(t string) (string, bool) {
		t = strings.TrimSpace(t)
		if l, r, ok := strings.Cut(t, "||"); ok {
			if v, ok := term(l); ok && v != "" {
				return v, true
			}
			return term(r)
		}
		switch {
		case len(t) >= 2 && (t[0] == '\'' && t[len(t)-1] == '\'' || t[0] == '"' && t[len(t)-1] == '"'):
			return t[1 : len(t)-1], true
		case numberRe.MatchString(t):
			return t, true
		case strings.HasPrefix(t, "matrix."):
			n, ok := j.Strategy.Matrix[strings.TrimPrefix(t, "matrix.")]
			if !ok {
				return "", false
			}
			if n.Kind == yaml.SequenceNode && len(n.Content) > 0 && n.Content[0].Kind == yaml.ScalarNode {
				return n.Content[0].Value, true
			}
			if n.Kind == yaml.ScalarNode && !strings.Contains(n.Value, "${{") {
				return n.Value, true
			}
		case strings.HasPrefix(t, "env."):
			k := strings.TrimPrefix(t, "env.")
			for _, env := range []map[string]string{j.Env, wf.Env} {
				if v, ok := env[k]; ok && !strings.Contains(v, "${{") {
					return v, true
				}
			}
		}
		return "", false
	}
	return func(v string) (string, bool) {
		ok := true
		r := exprRe.ReplaceAllStringFunc(v, func(m string) string {
			x, good := term(exprRe.FindStringSubmatch(m)[1])
			ok = ok && good
			return x
		})
		return r, ok
	}
}
