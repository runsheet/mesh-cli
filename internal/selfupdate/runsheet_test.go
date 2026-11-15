package selfupdate

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// serveRunsheet is a fakeTransport that answers rel's three assets under
// runsheet's own release channel, where Runsheet.Asset asks.
func serveRunsheet(rel *Release) *fakeTransport {
	dir := "/releases/runsheet/mesh-cli/" + rel.Version + "/"
	return &fakeTransport{files: map[string][]byte{
		dir + rel.Asset:   rel.Binary,
		dir + SumsAsset:   rel.Sums,
		dir + BundleAsset: rel.Bundle,
	}}
}

func TestRunsheetSourceFetchesAndVerifies(t *testing.T) {
	f := newFixture(t)
	f.rel.Bundle = signSumsIssuer(t, f.vs, RunsheetIdentityPrefix+testVersion, RunsheetIssuer, f.rel.Sums)
	ft := serveRunsheet(f.rel)
	src := Runsheet{Client: &http.Client{Transport: ft}}
	got, err := Fetch(context.Background(), src, testVersion, "linux", "amd64")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := Verify(got, f.policy); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	for _, u := range ft.seen {
		if !strings.HasPrefix(u, "https://runsheet.dev/releases/runsheet/mesh-cli/v1.2.3/") {
			t.Errorf("fetched %s, outside runsheet's release channel", u)
		}
	}

	delete(ft.files, "/releases/runsheet/mesh-cli/v1.2.3/"+BundleAsset)
	_, err = Fetch(context.Background(), Runsheet{Client: &http.Client{Transport: ft}}, testVersion, "linux", "amd64")
	if !errors.Is(err, ErrMissingAsset) {
		t.Fatalf("Fetch with a missing asset = %v, want ErrMissingAsset", err)
	}
	if r := Reason(err); r != "release v1.2.3 has no SHA256SUMS.sigstore.json on runsheet's release channel (404: not published there, or not a signed release)" {
		t.Errorf("Reason = %q", r)
	}
}

// Latest asks runsheet's own channel for the latest release, which is never
// a draft or a prerelease, and refuses an answer that is one anyway — the
// same semantics as GitHub.Latest.
func TestRunsheetSourceLatest(t *testing.T) {
	ft := &fakeTransport{files: map[string][]byte{
		"/releases/runsheet/mesh-cli/latest": []byte(`{"tag_name":"v1.4.0","prerelease":false,"draft":false}`),
	}}
	got, err := Runsheet{Client: &http.Client{Transport: ft}}.Latest(context.Background())
	if err != nil || got != "v1.4.0" {
		t.Fatalf("Latest = %q, %v", got, err)
	}
	if len(ft.seen) != 1 || ft.seen[0] != RunsheetLatestURL {
		t.Fatalf("asked %v", ft.seen)
	}

	ft.files["/releases/runsheet/mesh-cli/latest"] = []byte(`{"tag_name":"v1.5.0-rc.1","prerelease":true}`)
	if got, err := (Runsheet{Client: &http.Client{Transport: ft}}).Latest(context.Background()); err == nil {
		t.Fatalf("Latest took a prerelease: %q", got)
	}

	delete(ft.files, "/releases/runsheet/mesh-cli/latest")
	if _, err := (Runsheet{Client: &http.Client{Transport: ft}}).Latest(context.Background()); err == nil || !strings.Contains(err.Error(), "no release yet") {
		t.Fatalf("Latest with no release = %v", err)
	}
}
