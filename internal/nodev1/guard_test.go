package nodev1

import (
	"os"
	"os/exec"
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
