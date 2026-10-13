package node

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// pullRunner is the injected runner of the pull tests: it reads a
// pull's --authfile while the pull runs, and fails the pull when told.
type pullRunner struct {
	imageRunner
	fail     bool
	authFile string
	auth     []byte
}

func (r *pullRunner) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := r.imageRunner.run(ctx, name, args...)
	if len(args) == 0 || args[0] != "pull" {
		return out, err
	}
	for i, a := range args {
		if a == "--authfile" {
			r.authFile = args[i+1]
			r.auth, _ = os.ReadFile(args[i+1])
		}
	}
	if r.fail {
		return []byte("Error: initializing source docker://ghcr.io/runsheet/mesh-node:dev: unauthorized"), errors.New("exit status 125")
	}
	return out, err
}

// The node image an update ensures is pulled on the registry credential
// the platform issues the node: the fake gateway is asked
// nodeRegistryToken for ghcr.io, the pull carries --authfile logging in
// to ghcr.io with its token, and the file is gone after a pull that
// succeeds and after one that fails (ADR-007's amendment 2026-10-13).
func TestEnsureImagePullsOnTheGatewaysRegistryToken(t *testing.T) {
	for _, fail := range []bool{false, true} {
		f := gatewaytest.New("rs_test")
		f.RegistryTokens = map[string]map[string]any{
			"ghcr.io": {"registry": "ghcr.io", "token": "ghs_node_packages_read", "expiresAt": "2026-10-13T12:00:00Z"},
		}
		gw := gateway.New(f.URL(), "rs_test")
		r := &pullRunner{fail: fail}
		img, err := sandboxOf(t, "sandbox:\n  image: ghcr.io/runsheet/mesh-node:dev\n").
			EnsureImage(context.Background(), r.run, "dev", "", RegistryBrokerOf(gw))
		if fail != (err != nil) {
			t.Fatalf("fail=%v: %+v, %v", fail, img, err)
		}
		asked := f.CallsNamed("nodeRegistryToken")
		if len(asked) != 1 || asked[0].Input["registry"] != "ghcr.io" {
			t.Errorf("fail=%v: the gateway was asked %+v; want nodeRegistryToken(ghcr.io)", fail, asked)
		}
		pulls := r.sub("pull")
		if len(pulls) != 1 || r.authFile == "" || pulls[0] != "podman pull --authfile "+r.authFile+" ghcr.io/runsheet/mesh-node:dev" {
			t.Fatalf("fail=%v: pulls = %q", fail, pulls)
		}
		var doc struct {
			Auths map[string]struct {
				Auth string `json:"auth"`
			} `json:"auths"`
		}
		if err := json.Unmarshal(r.auth, &doc); err != nil {
			t.Fatalf("auth.json %s: %v", r.auth, err)
		}
		if raw, _ := base64.StdEncoding.DecodeString(doc.Auths["ghcr.io"].Auth); string(raw) != "x-access-token:ghs_node_packages_read" {
			t.Errorf("fail=%v: auth.json logs in to ghcr.io as %q", fail, raw)
		}
		if _, err := os.Stat(r.authFile); !os.IsNotExist(err) {
			t.Errorf("fail=%v: %s left after the pull (%v)", fail, r.authFile, err)
		}
	}
}

// A platform without nodeRegistryToken gives no credential: the pull
// goes without --authfile on podman's own login, as before.
func TestEnsureImagePullOnAnOlderPlatform(t *testing.T) {
	f := gatewaytest.New("rs_test")
	f.Unsupported = map[string]bool{"nodeRegistryToken": true}
	gw := gateway.New(f.URL(), "rs_test")
	if cred, err := RegistryBrokerOf(gw)(context.Background(), "ghcr.io"); cred != nil || err != nil {
		t.Fatalf("broker on an older platform = %+v, %v; want none and no error", cred, err)
	}
	r := &pullRunner{}
	if _, err := sandboxOf(t, "sandbox: {}\n").EnsureImage(context.Background(), r.run, "v1.1.0", "", RegistryBrokerOf(gw)); err != nil {
		t.Fatal(err)
	}
	if pulls := r.sub("pull"); len(pulls) != 1 || strings.Contains(pulls[0], "--authfile") {
		t.Errorf("pulls = %q; want a plain pull", pulls)
	}
}
