package nodev1

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// forbidden are the packages the v1 runtime never reaches, directly or
// through another package (Mesh v1 spec, "The mesh-cli node"): the
// legacy runtime and runsheet's gateway client.
var forbidden = []string{
	"projectmesh.io/mesh-cli/internal/gateway",
	"projectmesh.io/mesh-cli/internal/node",
}

// nodev1 and meshapi speak Mesh v1 alone: neither imports runsheet's
// gateway or the legacy node, even transitively.
func TestNoGatewayImport(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH")
	}
	for _, pkg := range []string{"./", "../meshapi/..."} {
		cmd := exec.Command(gobin, "list", "-deps", "-test", pkg)
		cmd.Env = append(os.Environ(), "GOWORK=off")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go list %s: %v\n%s", pkg, err, out)
		}
		for _, dep := range strings.Fields(string(out)) {
			for _, f := range forbidden {
				if dep == f {
					t.Errorf("%s depends on %s", pkg, f)
				}
			}
		}
	}
}

// vendorMarks are what a path to a git vendor would name: its hosts and
// the shapes its tokens go in. Only the relay (internal/gitrelay) and
// Latchkey's for-job routes, reached on the session token, know them.
var vendorMarks = []string{
	"github.com", "githubusercontent.com", "gitlab.com", "bitbucket.org", "dev.azure.com", "gitea.",
	"x-access-token", "oauth2:", "x-token-auth", "ghs_", "glpat-",
}

// Every write goes through the relay (Mesh v1 spec, "Many git vendors,
// and every write through the relay"): nodev1's own code names no vendor
// host or token shape, runs no `git push` but the push tool's (which
// names the grant's URL), and depends on no vendor client — so it has
// no path that writes to a vendor except through the relay.
func TestNoWritePathToAVendorButTheRelay(t *testing.T) {
	fset := token.NewFileSet()
	// the node tools (internal/nodetools) run in nodev1's process: they
	// are held to the same rule, push.go there being the push tool
	var files []string
	for _, pattern := range []string{"*.go", "../nodetools/*.go"} {
		got, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, got...)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, _ := strconv.Unquote(lit.Value)
			for _, m := range vendorMarks {
				if strings.Contains(strings.ToLower(s), m) {
					t.Errorf("%s names %q (%s): a vendor is reached only through the relay", fset.Position(lit.Pos()), s, m)
				}
			}
			if s == "push" && name != "../nodetools/push.go" {
				t.Errorf("%s runs git push outside the push tool", fset.Position(lit.Pos()))
			}
			return true
		})
	}
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH")
	}
	cmd := exec.Command(gobin, "list", "-deps", "./")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		// gitdata is the legacy node's GitHub REST client; go-github and
		// its kin are vendor clients
		if dep == "projectmesh.io/mesh-cli/internal/gitdata" || strings.HasPrefix(dep, "github.com/google/go-github") ||
			strings.Contains(dep, "gitlab.com/gitlab-org/api") || strings.Contains(dep, "xanzy/go-gitlab") {
			t.Errorf("nodev1 depends on %s, a vendor client", dep)
		}
	}
}

// longLived are nodev1's types that live as long as the node, not a
// lease: none may keep a lease's state in a map of its own.
var longLived = []string{"Runtime", "fleet", "updateNode"}

// mapsAllowed are the long-lived maps that are no lease's: what each
// holds outlives every lease.
var mapsAllowed = map[string]string{
	"Runtime.accounts": "a model account's standing, read across its leases for the heartbeat",
}

// Actors own everything (ADR-044, Mesh v1 PR 3.9): the node keeps no
// in-flight map of its own. A lease's state — its cancel, progress,
// mailbox, pod, access records — lives in its record in the lease
// registry (actor.Registry), with its job actor under --actors, and goes
// when the lease ends; no long-lived type of nodev1 holds a map or a
// sync.Map, and the package declares no map variable, beside the
// allowed few.
func TestNoInFlightMaps(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	isMap := func(e ast.Expr) bool {
		switch x := e.(type) {
		case *ast.MapType:
			return true
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok && id.Name == "sync" && x.Sel.Name == "Map" {
				return true
			}
		}
		return false
	}
	seen := map[string]bool{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				switch s := spec.(type) {
				case *ast.ValueSpec:
					if gd.Tok != token.VAR {
						continue
					}
					for i, n := range s.Names {
						typed := s.Type != nil && isMap(s.Type)
						lit := i < len(s.Values) && isMapLit(s.Values[i])
						if typed || lit {
							t.Errorf("%s: package variable %s is a map: a lease's state belongs to its record", fset.Position(n.Pos()), n.Name)
						}
					}
				case *ast.TypeSpec:
					seen[s.Name.Name] = true
					if !slices.Contains(longLived, s.Name.Name) {
						continue
					}
					st, ok := s.Type.(*ast.StructType)
					if !ok {
						continue
					}
					for _, fld := range st.Fields.List {
						if !isMap(fld.Type) {
							continue
						}
						for _, n := range fld.Names {
							key := s.Name.Name + "." + n.Name
							if _, ok := mapsAllowed[key]; !ok {
								t.Errorf("%s: %s is a map on a long-lived type: in-flight state belongs to the lease's record (actor.Registry)", fset.Position(n.Pos()), key)
							}
						}
					}
				}
			}
		}
	}
	for _, typ := range longLived {
		if !seen[typ] {
			t.Errorf("the guard names %s, which nodev1 no longer declares", typ)
		}
	}
}

func isMapLit(e ast.Expr) bool {
	if c, ok := e.(*ast.CompositeLit); ok {
		_, isMap := c.Type.(*ast.MapType)
		return isMap
	}
	return false
}
