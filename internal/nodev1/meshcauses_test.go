package nodev1

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/nodetools"
)

// meshHandBackCauses is Mesh's own hand-back enum, copied from
// projectmesh-io/api service/meshgen/models_gen.go (HandBackCause.Valid)
// less "expired", which server/leases.go's handBack refuses from a node.
// Its fail (server/leases.go fail) takes these and work,
// stopped_by_rule and credential_exposed. When Mesh's enum changes,
// change this and meshapi.HandBackCauses together.
var meshHandBackCauses = []string{
	"cannot_run", "pod_killed", "stalled", "node_update", "node_restart",
	"panic", "revoked", "parked", "product", "platform", "infrastructure",
	"cancelled", "no_finish", "credential_timeout", "tool_unavailable",
	"account_refused", "model_rate_limited",
}

var meshFailCauses = append([]string{"work", "stopped_by_rule", "credential_exposed"}, meshHandBackCauses...)

// meshCauseConsts reads internal/meshapi's string constants named
// Cause*, by name.
func meshCauseConsts(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob(filepath.Join("..", "meshapi", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Cause") || i >= len(vs.Values) {
					continue
				}
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					v, _ := strconv.Unquote(lit.Value)
					out[name.Name] = v
				}
			}
			return true
		})
	}
	return out
}

// Every cause the node can send Mesh is one Mesh takes: a cause outside
// its enum is refused 422 and the lease is never handed back (dl360,
// 2026-11-16: egress_died). The node's own source is read for every
// meshapi.Cause* it names, so a new one is checked the day it is used;
// the session's hand_back causes and meshapi's own mirror are checked
// beside it.
func TestEveryCauseTheNodeSendsIsOneMeshTakes(t *testing.T) {
	consts := meshCauseConsts(t)
	if len(consts) == 0 {
		t.Fatal("read no meshapi.Cause* constants")
	}
	srcs, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	used := regexp.MustCompile(`meshapi\.(Cause[A-Za-z]+)\b`)
	seen := map[string]bool{}
	for _, f := range srcs {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range used.FindAllStringSubmatch(string(b), -1) {
			seen[m[1]] = true
		}
	}
	if len(seen) == 0 {
		t.Fatal("found no meshapi.Cause* in the node's source")
	}
	for name := range seen {
		v, ok := consts[name]
		if !ok {
			t.Errorf("meshapi.%s is not a string constant of meshapi", name)
			continue
		}
		// fail takes every hand-back cause as well
		if !slices.Contains(meshFailCauses, meshapi.MeshCause(v)) {
			t.Errorf("the node sends meshapi.%s = %q, which Mesh refuses", name, v)
		}
	}
	for _, c := range nodetools.SessionCauses {
		if !slices.Contains(meshHandBackCauses, c) {
			t.Errorf("a session may hand back %q, which Mesh refuses", c)
		}
	}
	for _, c := range meshapi.HandBackCauses {
		if !slices.Contains(meshHandBackCauses, c) {
			t.Errorf("meshapi.HandBackCauses has %q, which Mesh refuses", c)
		}
	}
	for _, c := range meshapi.FailCauses {
		if !slices.Contains(meshFailCauses, c) {
			t.Errorf("meshapi.FailCauses has %q, which Mesh refuses", c)
		}
	}
	// egress_died, read back from an older node's actor.json, goes as
	// pod_killed
	if got := meshapi.MeshCause(meshapi.CauseEgressDied); got != meshapi.CausePodKilled {
		t.Errorf("MeshCause(egress_died) = %q", got)
	}
	if seen["CauseEgressDied"] {
		t.Error("the node still names meshapi.CauseEgressDied")
	}
}
