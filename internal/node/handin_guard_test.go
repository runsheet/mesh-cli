package node

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// handInMethods are the gateway commands that hand a claim back: a
// finish, a fail, a release, a verdict. Every error one answers goes
// through handInRefused, so a refusal is logged with the gateway's answer
// (docs/adr/adr-042-a-session-that-ends-says-so.md).
var handInMethods = map[string]bool{
	"FinishWorkItem":            true,
	"FinishWorkItemAlreadyDone": true,
	"FinishWorkItemChangeset":   true,
	"FailWorkItem":              true,
	"ReleaseWorkItem":           true,
	"ReleasePlan":               true,
	"EvaluateWorkItem":          true,
	"EvaluateWorkItemNoVerdict": true,
}

// handInWrappers pass a hand-in's error on, the usage report's
// resend-without-a-field answers absorbed: acct.Carried, validationUsage,
// the changeset finish's c.carried.
var handInWrappers = map[string]bool{"Carried": true, "validationUsage": true, "carried": true}

// TestEveryHandInErrorGoesThroughTheHelper parses every non-test file of
// the package and fails, naming file:line, on a hand-in whose error does
// not reach handInRefused: either the call is inside its argument
// (directly, or wrapped by acct.Carried, validationUsage or c.carried), or
// its error is assigned to a variable — `err := gw.X(…)`, an if's or a
// switch's init, or the result of sendReport over a func returning the
// call — that a later handInRefused call in the same function takes. A
// call discarded (`_ = gw.FailWorkItem(…)`), left as a statement, or
// assigned and never handed to the helper is reported. The interfaces
// declaring the methods are no calls, and a method of this package named
// for one (a forwarding wrapper) is skipped.
func TestEveryHandInErrorGoesThroughTheHelper(t *testing.T) {
	t.Run("package", func(t *testing.T) {
		files, err := filepath.Glob("*.go")
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(files)
		fset := token.NewFileSet()
		var bad []string
		checked := 0
		for _, name := range files {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			src, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			f, err := parser.ParseFile(fset, name, src, 0)
			if err != nil {
				t.Fatal(err)
			}
			found, n := unhelpedHandIns(fset, f)
			bad = append(bad, found...)
			checked += n
		}
		if checked == 0 {
			t.Fatal("no hand-in call found in the package; the guard checks nothing")
		}
		for _, b := range bad {
			t.Errorf("%s", b)
		}
	})

	t.Run("bites", func(t *testing.T) {
		for _, tc := range []struct {
			name, body string
			reported   bool
		}{
			{"discarded", `_ = gw.FailWorkItem(ctx, id, reason)`, true},
			{"bare statement", `gw.ReleaseWorkItem(ctx, id, node, reason)`, true},
			{"assigned, never helped", `err := gw.EvaluateWorkItem(ctx, id); log(err)`, true},
			{"inside the helper", `_ = handInRefused(logf, nil, id, "FailWorkItem", gw.FailWorkItem(ctx, id, reason))`, false},
			{"wrapped by Carried", `_ = handInRefused(logf, nil, id, "FinishWorkItem", acct.Carried(gw.FinishWorkItem(ctx, id)))`, false},
			{"assigned, then helped", `err := gw.ReleasePlan(ctx, id, node, reason); if err != nil { _ = handInRefused(logf, nil, id, "ReleasePlan", err) }`, false},
			{"switch init, then helped", `switch rerr := gw.ReleasePlan(ctx, id, node, reason); { default: _ = handInRefused(logf, nil, id, "ReleasePlan", rerr) }`, false},
			{"helped before the call", `var err error; _ = handInRefused(logf, nil, id, "FailWorkItem", err); err = gw.FailWorkItem(ctx, id, reason)`, true},
			{"sendReport, then helped", `err := sendReport(ctx, func(ctx context.Context) error { return gw.FailWorkItem(ctx, id, reason) }); _ = handInRefused(logf, nil, id, "FailWorkItem", err)`, false},
			{"sendReport, discarded", `_ = sendReport(ctx, func(ctx context.Context) error { return gw.FailWorkItem(ctx, id, reason) })`, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				src := "package node\n\nfunc f() {\n\t" + tc.body + "\n}\n"
				fset := token.NewFileSet()
				f, err := parser.ParseFile(fset, "mem.go", src, 0)
				if err != nil {
					t.Fatal(err)
				}
				bad, n := unhelpedHandIns(fset, f)
				if n != 1 {
					t.Fatalf("checked %d hand-in calls, want 1", n)
				}
				if got := len(bad) == 1; got != tc.reported {
					t.Fatalf("reported %v, want reported %v", bad, tc.reported)
				}
				if tc.reported && !strings.HasPrefix(bad[0], "mem.go:4:") {
					t.Errorf("report %q does not name mem.go:4", bad[0])
				}
			})
		}
	})
}

// unhelpedHandIns answers each hand-in call in f whose error does not
// reach handInRefused, as `file:line: <call> …`, and how many hand-in
// calls it checked.
func unhelpedHandIns(fset *token.FileSet, f *ast.File) (bad []string, checked int) {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if fn.Recv != nil && handInMethods[fn.Name.Name] {
			continue // a forwarding wrapper of the method itself
		}
		helped := helperCalls(fn.Body)
		var stack []ast.Node
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := handInName(call)
			if name == "" {
				return true
			}
			checked++
			if !reachesHelper(stack, helped) {
				pos := fset.Position(call.Pos())
				bad = append(bad, fmt.Sprintf("%s:%d: %s's error does not go through handInRefused", filepath.Base(pos.Filename), pos.Line, name))
			}
			return true
		})
	}
	return bad, checked
}

// handInName is the method a call makes when it is a hand-in, else "".
func handInName(call *ast.CallExpr) string {
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok && handInMethods[sel.Sel.Name] {
		return sel.Sel.Name
	}
	return ""
}

// calleeName is the name a call's function is known by: f or x.f.
func calleeName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	}
	return ""
}

// helperCalls are the handInRefused calls in body.
func helperCalls(body *ast.BlockStmt) []*ast.CallExpr {
	var calls []*ast.CallExpr
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && calleeName(call) == "handInRefused" {
			calls = append(calls, call)
		}
		return true
	})
	return calls
}

// reachesHelper says whether the hand-in at the top of stack (its
// ancestors below it) reaches handInRefused: inside one of its arguments,
// or assigned to a variable a later helper call takes.
func reachesHelper(stack []ast.Node, helped []*ast.CallExpr) bool {
	for _, n := range stack[:len(stack)-1] {
		if call, ok := n.(*ast.CallExpr); ok && calleeName(call) == "handInRefused" {
			return true
		}
	}
	// follow the value up: through the wrappers, and out of a func
	// literal returning it to the call it is an argument of (sendReport)
	i := len(stack) - 1
	for i > 0 {
		value, parent := stack[i], stack[i-1]
		switch p := parent.(type) {
		case *ast.CallExpr:
			if !handInWrappers[calleeName(p)] {
				return false
			}
			i--
		case *ast.ReturnStmt:
			// return x → block → func literal → the call taking it
			if i < 4 {
				return false
			}
			if _, ok := stack[i-2].(*ast.BlockStmt); !ok {
				return false
			}
			if _, ok := stack[i-3].(*ast.FuncLit); !ok {
				return false
			}
			if _, ok := stack[i-4].(*ast.CallExpr); !ok {
				return false
			}
			i -= 4
		case *ast.AssignStmt:
			for k, rhs := range p.Rhs {
				if rhs != value || k >= len(p.Lhs) {
					continue
				}
				id, ok := p.Lhs[k].(*ast.Ident)
				if !ok || id.Name == "_" {
					return false
				}
				return helpedAfter(id.Name, p.End(), helped)
			}
			return false
		case *ast.ParenExpr:
			i--
		default:
			return false
		}
	}
	return false
}

// helpedAfter says whether a helper call after pos takes the variable
// name among its arguments, at any depth (acct.Carried(err) included).
func helpedAfter(name string, pos token.Pos, helped []*ast.CallExpr) bool {
	for _, call := range helped {
		if call.Pos() < pos {
			continue
		}
		found := false
		for _, arg := range call.Args {
			ast.Inspect(arg, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && id.Name == name {
					found = true
				}
				return !found
			})
		}
		if found {
			return true
		}
	}
	return false
}
