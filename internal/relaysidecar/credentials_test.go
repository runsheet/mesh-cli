package relaysidecar

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/gitrelay"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

const (
	sessionSecret = "mesh-session-secret-value"
	gitSecret     = "ghs_git-secret-value"
)

func testCreds(t *testing.T) Credentials {
	t.Helper()
	k, err := dpop.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	h, err := k.Handoff()
	if err != nil {
		t.Fatal(err)
	}
	return Credentials{
		Lease:    "lease-1",
		Key:      h,
		Session:  Token{Value: sessionSecret, ExpiresAt: time.Now().Add(10 * time.Minute)},
		Latchkey: "https://latchkey.test",
		API:      []meshapi.APIAccess{{Vendor: "github", Methods: []string{"GET"}, Paths: []string{"/repos/{o}/{r}/pulls"}}},
		Servers:  []Server{{Name: "runsheet", URL: "https://product.test/mcp"}},
		Git: &Git{Policy: gitrelay.Policy{Repo: "acme/app", Access: gitrelay.Write, Push: []gitrelay.RefRule{{Ref: "refs/heads/mesh/g-1", FastForwardOnly: true}}},
			Token: Token{Value: gitSecret, ExpiresAt: time.Now().Add(time.Hour)}},
	}
}

func privDir(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "creds")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

// The node's credentials reach the sidecar's tmpfs as one file, mode
// 0400, the sidecar's own; Load reads back exactly what was installed.
func TestInstallWritesTheFileReadableByItsOwnerAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix modes")
	}
	c := testCreds(t)
	raw, _ := json.Marshal(c)
	dir := privDir(t)
	if err := Install(strings.NewReader(string(raw)), dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, CredentialsFile)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o400 {
		t.Fatalf("the credential file is mode %#o, want 0400", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("the tmpfs holds %d entries, want the credential file alone", len(entries))
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Session.Value != sessionSecret || got.Git == nil || got.Git.Token.Value != gitSecret || got.Lease != "lease-1" {
		t.Fatalf("Load did not read back what was installed: %s", got)
	}
	// installed again (the node renews by writing it whole), it is
	// replaced, never written in place
	c.Session.Value = "second"
	raw, _ = json.Marshal(c)
	if err := Install(strings.NewReader(string(raw)), dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := Load(path); got.Session.Value != "second" {
		t.Fatalf("a second install did not replace the file")
	}
}

// A directory or file anyone else may read is refused, as is anything
// that is not the credentials' shape.
func TestInstallAndLoadRefuseWhatOthersCouldRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix modes")
	}
	raw, _ := json.Marshal(testCreds(t))
	loose := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Install(strings.NewReader(string(raw)), loose); err == nil {
		t.Fatal("Install wrote into a directory others can enter")
	}
	dir := privDir(t)
	if err := Install(strings.NewReader(string(raw)), dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, CredentialsFile)
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load read a world-readable credential file")
	}
	for name, body := range map[string]string{
		"not json":      "nope",
		"unknown field": `{"lease":"l","machine_token":"x"}`,
		"no lease":      strings.Replace(string(raw), `"lease":"lease-1"`, `"lease":""`, 1),
	} {
		if err := Install(strings.NewReader(body), privDir(t)); err == nil {
			t.Errorf("%s: Install took it", name)
		}
	}
}

// No way of printing the credentials prints a token or the key.
func TestCredentialsNeverPrintASecret(t *testing.T) {
	c := testCreds(t)
	for _, s := range []string{
		fmt.Sprint(c), fmt.Sprintf("%v %+v %#v %s", c, c, c, c),
		fmt.Sprintf("%v %+v %#v", c.Session, *c.Git, c.Key),
	} {
		for _, secret := range []string{sessionSecret, gitSecret, c.Key.D} {
			if strings.Contains(s, secret) {
				t.Fatalf("printed a secret: %s", s)
			}
		}
	}
}
