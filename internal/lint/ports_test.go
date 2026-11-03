// Package lint holds static checks the project runs against its own
// _test.go files as ordinary go tests, rather than as a separate tool: a
// repository-wide invariant a reviewer should not have to remember to
// look for.
package lint

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// violation is one finding: where, what kind, and the sentence a
// reviewer reads.
type violation struct {
	pos    string
	kind   string
	detail string
}

func (v violation) String() string {
	return fmt.Sprintf("%s: %s", v.pos, v.detail)
}

// badFixture is the checker's own bad-pattern fixture: a fixed, non-zero
// TCP port (any other process on the host, or a parallel test, can hold
// it already) and the race nodev1's freePort lost before it was fixed
// 2026-11-03 — a probed port closed, then only its number handed back,
// leaving a window before whatever reuses the number rebinds it.
const badFixture = `package fixture

import "net"

func fixedListener() (net.Listener, error) {
	return net.Listen("tcp", "127.0.0.1:9999")
}

func freePort() int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
`

// TestNoTestFixesOrRacesAPort fails any mesh-cli _test.go file that
// binds a fixed, non-zero TCP port, or that probes a free one, closes
// it, and hands back only the number for something else to rebind
// later (internal/nodev1's freePort, fixed 2026-11-03: see
// takeGitBridgeListener). Both leave a window another process on the
// host, or a parallel test, can take the port in first.
func TestNoTestFixesOrRacesAPort(t *testing.T) {
	t.Run("repo", func(t *testing.T) {
		files := testFiles(t)
		if len(files) == 0 {
			t.Fatal("no _test.go files found under the repo; the checker checks nothing")
		}
		fset := token.NewFileSet()
		var bad []string
		for _, path := range files {
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			vs, err := checkSource(fset, path, src)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			for _, v := range vs {
				bad = append(bad, v.String())
			}
		}
		for _, b := range bad {
			t.Errorf("%s", b)
		}
	})

	// the checker's own bad-pattern fixture: it must report both kinds
	// against it, proving the checker is not vacuously silent.
	t.Run("fixture", func(t *testing.T) {
		fset := token.NewFileSet()
		vs, err := checkSource(fset, "fixture_test.go", []byte(badFixture))
		if err != nil {
			t.Fatal(err)
		}
		if len(vs) == 0 {
			t.Fatal("the checker found no violation in its own bad-pattern fixture")
		}
		kinds := kindsOf(vs)
		if got := strings.Join(kinds, ","); got != "closed-port-reused,fixed-port" {
			t.Fatalf("fixture violations = %v, want one of each kind", kinds)
		}
	})

	t.Run("bites", func(t *testing.T) {
		for _, tc := range []struct {
			name, src string
			want      []string // kinds, sorted
		}{
			{"fixed tcp port", `package p
import "net"
func f() { net.Listen("tcp", "127.0.0.1:8080") }
`, []string{"fixed-port"}},
			{"dynamic tcp port", `package p
import "net"
func f() { net.Listen("tcp", "127.0.0.1:0") }
`, nil},
			{"a unix socket names no port to race for", `package p
import "net"
func f() { net.Listen("unix", "/tmp/whatever:1234") }
`, nil},
			{"fixed port on a variable address is not judged", `package p
import "net"
func f(addr string) { net.Listen("tcp", addr) }
`, nil},
			{"http.ListenAndServe on a fixed port", `package p
import "net/http"
func f() { http.ListenAndServe("127.0.0.1:8080", nil) }
`, []string{"fixed-port"}},
			{"probes a port, closes it, returns the number", `package p
import "net"
func freePort() int {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
`, []string{"closed-port-reused"}},
			{"closes with a bare call, not a defer, same shape", `package p
import "net"
func freePort() int {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}
`, []string{"closed-port-reused"}},
			{"keeps the listener open and hands it over", `package p
import "net"
func freeListener() net.Listener {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	return ln
}
`, nil},
			{"closed only at test cleanup, after the port is used while open", `package p
import "net"
func f(t T) net.Listener {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	t.Cleanup(func() { ln.Close() })
	return ln
}
type T interface{ Cleanup(func()) }
`, nil},
			{"the listener is used locally, never returned derived", `package p
import "net"
func f() {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	_ = ln.Addr().(*net.TCPAddr).Port
}
`, nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fset := token.NewFileSet()
				vs, err := checkSource(fset, "mem_test.go", []byte(tc.src))
				if err != nil {
					t.Fatal(err)
				}
				kinds := kindsOf(vs)
				want := slices.Clone(tc.want)
				sort.Strings(want)
				if !slices.Equal(kinds, want) {
					t.Fatalf("violations = %v, want %v", kinds, want)
				}
			})
		}
	})
}

// kindsOf is each violation's kind, sorted.
func kindsOf(vs []violation) []string {
	var kinds []string
	for _, v := range vs {
		kinds = append(kinds, v.kind)
	}
	sort.Strings(kinds)
	return kinds
}

// testFiles is every "_test.go" file under the repo this package's own
// file sits in two directories below, .git and any testdata (go itself
// never treats one as a package) left unwalked.
func testFiles(t *testing.T) []string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller: no caller info")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..")
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "testdata":
				return filepath.SkipDir
			}
			if d.Name() != "." && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), "_test.go") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	return files
}

// checkSource parses one file's source and reports every port
// violation it finds in it.
func checkSource(fset *token.FileSet, filename string, src []byte) ([]violation, error) {
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, err
	}
	var vs []violation
	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			if v, ok := fixedPortViolation(fset, node); ok {
				vs = append(vs, v)
			}
		case *ast.FuncDecl:
			if node.Body != nil {
				vs = append(vs, raceViolations(fset, funcLabel(node), node.Body)...)
			}
		case *ast.FuncLit:
			vs = append(vs, raceViolations(fset, "func literal", node.Body)...)
		}
		return true
	})
	return vs, nil
}

// funcLabel is how a FuncDecl names itself in a violation's detail: a
// method's receiver type included.
func funcLabel(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	return fmt.Sprintf("%s.%s", exprString(fn.Recv.List[0].Type), fn.Name.Name)
}

func exprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.StarExpr:
		return "*" + exprString(x.X)
	default:
		return "?"
	}
}

// fixedPortViolation says whether call binds a literal, non-zero TCP
// port: net.Listen and tls.Listen on a "host:port" string whose network
// is some form of "tcp", and http.ListenAndServe(TLS) the same way. A
// unix socket names no port to race for, and an address that is not a
// literal (a variable, built at runtime) cannot be judged here.
func fixedPortViolation(fset *token.FileSet, call *ast.CallExpr) (violation, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return violation{}, false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return violation{}, false
	}
	key := pkg.Name + "." + sel.Sel.Name

	var networkArg, addrArg ast.Expr
	switch key {
	case "net.Listen", "tls.Listen":
		if len(call.Args) < 2 {
			return violation{}, false
		}
		networkArg, addrArg = call.Args[0], call.Args[1]
	case "http.ListenAndServe", "http.ListenAndServeTLS":
		if len(call.Args) < 1 {
			return violation{}, false
		}
		addrArg = call.Args[0]
	default:
		return violation{}, false
	}

	if networkArg != nil {
		network, ok := stringLit(networkArg)
		if !ok || !strings.HasPrefix(network, "tcp") {
			return violation{}, false
		}
	}
	addr, ok := stringLit(addrArg)
	if !ok {
		return violation{}, false
	}
	port := addrPort(addr)
	if port == "" || port == "0" {
		return violation{}, false
	}
	return violation{
		pos:    fset.Position(call.Pos()).String(),
		kind:   "fixed-port",
		detail: fmt.Sprintf("%s binds the fixed port %s (%q); a parallel test or another process on the host may already hold it", key, port, addr),
	}, true
}

// stringLit is e's value when it is a plain string literal.
func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

// addrPort is the part of a "host:port" address after the last colon,
// or "" when addr has none.
func addrPort(addr string) string {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return ""
	}
	return addr[i+1:]
}

// isListenCall says whether call opens a listener whose port a caller
// could probe: net.Listen, net.ListenTCP, net.ListenUDP or tls.Listen.
func isListenCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	switch pkg.Name + "." + sel.Sel.Name {
	case "net.Listen", "net.ListenTCP", "net.ListenUDP", "tls.Listen":
		return true
	}
	return false
}

// closeTarget is the identifier call closes, when call is a plain
// "<ident>.Close()".
func closeTarget(call *ast.CallExpr) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Close" {
		return "", false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	return id.Name, true
}

// raceViolations finds, among body's own top-level statements (never a
// nested closure's — a func literal's statements are its own scope,
// scanned on its own when checkSource visits it), a listener opened
// and closed — a bare "ln.Close()" or "defer ln.Close()" — before the
// enclosing function returns, where a return hands back something
// derived from that listener (its port) rather than the listener
// itself. Closing it only inside a closure passed elsewhere (a
// t.Cleanup, run at the test's end, well after whatever reuses the
// port has already bound it) is not this: nothing here races.
func raceViolations(fset *token.FileSet, label string, body *ast.BlockStmt) []violation {
	listened := map[string]bool{}
	closedAt := map[string]token.Pos{}
	for _, stmt := range body.List {
		switch s := stmt.(type) {
		case *ast.AssignStmt:
			if len(s.Rhs) != 1 || len(s.Lhs) == 0 {
				continue
			}
			call, ok := s.Rhs[0].(*ast.CallExpr)
			if !ok || !isListenCall(call) {
				continue
			}
			if id, ok := s.Lhs[0].(*ast.Ident); ok && id.Name != "_" {
				listened[id.Name] = true
			}
		case *ast.DeferStmt:
			if name, ok := closeTarget(s.Call); ok {
				closedAt[name] = s.Pos()
			}
		case *ast.ExprStmt:
			if call, ok := s.X.(*ast.CallExpr); ok {
				if name, ok := closeTarget(call); ok {
					closedAt[name] = s.Pos()
				}
			}
		}
	}
	if len(listened) == 0 || len(closedAt) == 0 {
		return nil
	}

	// sourceOf traces a variable back to the listener it derives from: the
	// listener itself maps to its own name, and a variable assigned from an
	// expression naming a tracked variable (e.g. "port := ln.Addr()...")
	// maps to that variable's own origin, so a value handed through an
	// intermediate variable before being returned is still caught.
	sourceOf := map[string]string{}
	for name := range listened {
		sourceOf[name] = name
	}
	for _, stmt := range body.List {
		s, ok := stmt.(*ast.AssignStmt)
		if !ok {
			continue
		}
		for i, rhs := range s.Rhs {
			if i >= len(s.Lhs) {
				continue
			}
			lhsID, ok := s.Lhs[i].(*ast.Ident)
			if !ok || lhsID.Name == "_" {
				continue
			}
			for src, origin := range sourceOf {
				if containsIdent(rhs, src) {
					sourceOf[lhsID.Name] = origin
					break
				}
			}
		}
	}

	var vs []violation
	ast.Inspect(body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false // its own return belongs to the closure, not here
		}
		ret, ok := n.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		for _, res := range ret.Results {
			for name, origin := range sourceOf {
				closePos, closed := closedAt[origin]
				if !closed {
					continue
				}
				if id, ok := res.(*ast.Ident); ok && id.Name == origin {
					continue // returns the listener itself: fine
				}
				if !containsIdent(res, name) {
					continue
				}
				vs = append(vs, violation{
					pos:  fset.Position(ret.Pos()).String(),
					kind: "closed-port-reused",
					detail: fmt.Sprintf("%s: %s is closed at %s, then a return at %s hands back its port instead of the listener itself",
						label, origin, fset.Position(closePos).String(), fset.Position(ret.Pos()).String()),
				})
			}
		}
		return true
	})
	return vs
}

// containsIdent says whether expr contains an identifier named name
// anywhere within it.
func containsIdent(expr ast.Expr, name string) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if found {
			return false
		}
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			found = true
			return false
		}
		return true
	})
	return found
}
