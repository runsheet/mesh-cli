package node

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// gitRunnerFile is the one file that may exec git itself: the hardened
// runner, hardenedGitCmd and hardenedGitCmdEnv.
const gitRunnerFile = "gitexec.go"

// gitRunners are the hardened runner's constructors: a git command made
// by one is checked for os.Environ() like a bare exec of git.
var gitRunners = map[string]bool{"hardenedGitCmd": true, "hardenedGitCmdEnv": true}

// TestNoGitExecBypassesHardenedRunner parses every non-test file of the
// package and fails, naming file:line, on an exec.Command or
// exec.CommandContext whose command is the literal "git" outside
// gitexec.go, and on an os.Environ() handed to a git command — inside the
// call's arguments (the runner's extra env included), or assigned to the
// Env of a variable the call made. Every git the node runs on the host
// runs hardened, so nothing a session wrote into .git runs as the
// operator (ADR-007's amendment).
func TestNoGitExecBypassesHardenedRunner(t *testing.T) {
	t.Run("package", func(t *testing.T) {
		files, err := filepath.Glob("*.go")
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(files)
		var bad []string
		runners := 0
		for _, name := range files {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			src, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			found, n, err := gitExecBypasses(name, src)
			if err != nil {
				t.Fatal(err)
			}
			bad = append(bad, found...)
			runners += n
		}
		if runners == 0 {
			t.Fatal("no hardened git command found in the package; the guard checks nothing")
		}
		for _, b := range bad {
			t.Errorf("%s", b)
		}
	})

	cases := []struct {
		name, file, src string
		want            []string
	}{
		{"bare exec of git", "x.go", `package node
import ("context"; "os/exec")
func f(ctx context.Context) { _ = exec.CommandContext(ctx, "git", "status") }
`, []string{`x.go:3: exec.CommandContext of "git" outside gitexec.go`}},
		{"exec.Command of git", "x.go", `package node
import "os/exec"
func f() { _ = exec.Command("git", "status") }
`, []string{`x.go:3: exec.Command of "git" outside gitexec.go`}},
		{"aliased import", "x.go", `package node
import (ex "os/exec")
func f() { _ = ex.Command("git") }
`, []string{`x.go:3: exec.Command of "git" outside gitexec.go`}},
		{"os.Environ on a git cmd's Env", "gitexec.go", `package node
import ("context"; "os"; "os/exec")
func f(ctx context.Context) {
	cmd := exec.CommandContext(ctx, "git", "status")
	cmd.Env = append(os.Environ(), "A=b")
}
`, []string{`gitexec.go:5: os.Environ() passed to a git command`}},
		{"os.Environ as the runner's extra env", "x.go", `package node
import ("context"; "os")
func f(ctx context.Context) { _ = hardenedGitCmdEnv(ctx, "", ".", os.Environ(), "status") }
`, []string{`x.go:3: os.Environ() passed to a git command`}},
		{"os.Environ on a runner cmd's Env", "x.go", `package node
import ("context"; "os")
func f(ctx context.Context) {
	cmd := hardenedGitCmd(ctx, "", ".", "status")
	cmd.Env = os.Environ()
}
`, []string{`x.go:5: os.Environ() passed to a git command`}},
		{"the runner itself", "gitexec.go", `package node
import ("context"; "os/exec")
func f(ctx context.Context) { _ = exec.CommandContext(ctx, "git", "status") }
`, nil},
		{"another command with os.Environ", "x.go", `package node
import ("context"; "os"; "os/exec")
func f(ctx context.Context) {
	cmd := exec.CommandContext(ctx, "gh", "auth")
	cmd.Env = os.Environ()
}
`, nil},
		{"the hardened runner", "x.go", `package node
import "context"
func f(ctx context.Context) { _ = hardenedGitCmdEnv(ctx, "", ".", []string{"GIT_EDITOR=true"}, "status") }
`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _, err := gitExecBypasses(c.file, []byte(c.src))
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// gitExecBypasses is the guard on one file: each bypass as "file:line:
// what", and how many git commands the hardened runner made there.
func gitExecBypasses(name string, src []byte) ([]string, int, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		return nil, 0, err
	}
	execName, osName := "", ""
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		local := filepath.Base(path)
		if imp.Name != nil {
			local = imp.Name.Name
		}
		switch path {
		case "os/exec":
			execName = local
		case "os":
			osName = local
		}
	}
	isEnviron := func(n ast.Node) bool {
		found := false
		ast.Inspect(n, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && isPkgCall(call, osName, "Environ") {
				found = true
			}
			return !found
		})
		return found
	}
	var bad []string
	report := func(pos token.Pos, what string) {
		bad = append(bad, fmt.Sprintf("%s:%d: %s", filepath.Base(name), fset.Position(pos).Line, what))
	}
	runners := 0
	// isGit reports whether call makes a git command, saying so when it
	// is a bare exec outside the runner's file.
	isGit := func(call *ast.CallExpr) bool {
		if id, ok := call.Fun.(*ast.Ident); ok && gitRunners[id.Name] {
			runners++
			return true
		}
		for fn, at := range map[string]int{"Command": 0, "CommandContext": 1} {
			if !isPkgCall(call, execName, fn) || len(call.Args) <= at {
				continue
			}
			lit, ok := call.Args[at].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return false
			}
			if v, _ := strconv.Unquote(lit.Value); v != "git" {
				return false
			}
			if filepath.Base(name) != gitRunnerFile {
				report(call.Pos(), "exec."+fn+` of "git" outside `+gitRunnerFile)
			}
			return true
		}
		return false
	}
	// each function body: the git commands' variables, then their Env
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		gitVars := map[string]bool{}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if isGit(n) {
					for _, a := range n.Args {
						if isEnviron(a) {
							report(a.Pos(), "os.Environ() passed to a git command")
						}
					}
				}
			case *ast.AssignStmt:
				for i, rhs := range n.Rhs {
					call, ok := ast.Unparen(rhs).(*ast.CallExpr)
					if !ok || i >= len(n.Lhs) || !callsGit(call, execName) {
						continue
					}
					if id, ok := n.Lhs[i].(*ast.Ident); ok {
						gitVars[id.Name] = true
					}
				}
			}
			return true
		})
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for i, lhs := range as.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Env" || i >= len(as.Rhs) {
					continue
				}
				if x, ok := sel.X.(*ast.Ident); ok && gitVars[x.Name] && isEnviron(as.Rhs[i]) {
					report(as.Rhs[i].Pos(), "os.Environ() passed to a git command")
				}
			}
			return true
		})
	}
	return bad, runners, nil
}

// callsGit is isGit without the reporting: whether call makes a git
// command, by the runner or a bare exec.
func callsGit(call *ast.CallExpr, execName string) bool {
	if id, ok := call.Fun.(*ast.Ident); ok && gitRunners[id.Name] {
		return true
	}
	for fn, at := range map[string]int{"Command": 0, "CommandContext": 1} {
		if isPkgCall(call, execName, fn) && len(call.Args) > at {
			if lit, ok := call.Args[at].(*ast.BasicLit); ok && lit.Value == `"git"` {
				return true
			}
		}
	}
	return false
}

// isPkgCall reports whether call is pkg.fn(…), pkg the file's local name
// for the import ("" when the file does not import it).
func isPkgCall(call *ast.CallExpr, pkg, fn string) bool {
	if pkg == "" {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != fn {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == pkg
}
