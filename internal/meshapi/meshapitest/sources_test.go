package meshapitest_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi/meshapitest"
)

// own are the fixtures that are mesh-cli's alone, and why: every other
// fixture in testdata is a byte-for-byte copy of projectmesh-io/api's
// contracts/v1/<name>.json, which the api's e2e writes from the real
// service. To bring the copies up to api main:
//
//	for f in internal/meshapi/testdata/*.json; do n=$(basename $f)
//	  [ -f <api>/contracts/v1/$n ] && cp <api>/contracts/v1/$n $f; done
//
// and MESH_API_CONTRACTS=<api>/contracts/v1 go test ./internal/meshapi/...
// says which copies differ.
var own = map[string]string{
	// the stateful fake's brief (Fake.Offer): a richer brief than the
	// api's take.json, with the git, access and budget fields nodev1's
	// tests exercise
	"take-lease": "the fake's brief",
	// the api publishes no renew of a lease that is no longer live
	"lease-renew-gone": "a 409 lease_not_live renew",
}

// Every fixture is a copy of contracts/v1 (it carries the caller the api
// writes on each) or is named in own; with MESH_API_CONTRACTS set to an
// api checkout's contracts/v1, every copy is that file, byte for byte.
func TestFixturesNameTheirSource(t *testing.T) {
	api := os.Getenv("MESH_API_CONTRACTS")
	for name, f := range meshapitest.Load(t) {
		if _, ok := own[name]; ok {
			continue
		}
		if f.Caller == "" {
			t.Errorf("%s names no caller: it is not a contracts/v1 copy, so name it in own", name)
		}
		if api == "" {
			continue
		}
		want, err := os.ReadFile(filepath.Join(api, name+".json"))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		got, err := os.ReadFile(filepath.Join(meshapitest.Dir(), name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from %s", name, api)
		}
	}
	for name := range own {
		if api == "" {
			break
		}
		if _, err := os.Stat(filepath.Join(api, name+".json")); err == nil {
			t.Errorf("%s is named in own but the api publishes it: copy it instead", name)
		}
	}
}
