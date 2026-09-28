package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// fakeTransport serves release assets from a map keyed by URL path.
type fakeTransport struct {
	files map[string][]byte
	seen  []string
}

func (ft *fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	ft.seen = append(ft.seen, r.URL.String())
	b, ok := ft.files[r.URL.Path]
	status := http.StatusOK
	if !ok {
		status, b = http.StatusNotFound, []byte("Not Found")
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       io.NopCloser(bytes.NewReader(b)),
		Request:    r,
	}, nil
}

// serve is a fakeTransport that answers rel's three assets under the public
// mirror's own path, runsheet/mesh, where Fetch now asks.
func serve(rel *Release) *fakeTransport {
	dir := "/runsheet/mesh/releases/download/" + rel.Version + "/"
	return &fakeTransport{files: map[string][]byte{
		dir + rel.Asset:   rel.Binary,
		dir + SumsAsset:   rel.Sums,
		dir + BundleAsset: rel.Bundle,
	}}
}

func TestFetchThenVerify(t *testing.T) {
	f := newFixture(t)
	ft := serve(f.rel)
	src := GitHub{Client: &http.Client{Transport: ft}}
	got, err := Fetch(context.Background(), src, testVersion, "linux", "amd64")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.Asset != "mesh-linux-amd64" || !bytes.Equal(got.Binary, f.rel.Binary) ||
		!bytes.Equal(got.Sums, f.rel.Sums) || !bytes.Equal(got.Bundle, f.rel.Bundle) {
		t.Fatalf("Fetch = %+v", got)
	}
	if err := Verify(got, f.policy); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	for _, u := range ft.seen {
		if !strings.HasPrefix(u, "https://github.com/runsheet/mesh/releases/download/v1.2.3/") {
			t.Errorf("fetched %s, outside the release", u)
		}
	}
}

// Fetch asks the public mirror runsheet/mesh, never runsheet/mesh-cli (which
// is private): exactly the three assets, by exact path.
func TestFetchRequestsTheMirror(t *testing.T) {
	f := newFixture(t)
	ft := serve(f.rel)
	src := GitHub{Client: &http.Client{Transport: ft}}
	if _, err := Fetch(context.Background(), src, testVersion, "linux", "amd64"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	want := []string{
		"https://github.com/runsheet/mesh/releases/download/v1.2.3/SHA256SUMS",
		"https://github.com/runsheet/mesh/releases/download/v1.2.3/SHA256SUMS.sigstore.json",
		"https://github.com/runsheet/mesh/releases/download/v1.2.3/mesh-linux-amd64",
	}
	if len(ft.seen) != len(want) {
		t.Fatalf("requested %v, want %v", ft.seen, want)
	}
	seen := map[string]bool{}
	for _, u := range ft.seen {
		seen[u] = true
	}
	for _, u := range want {
		if !seen[u] {
			t.Errorf("did not request %s", u)
		}
	}
}

func TestFetchMissingAsset(t *testing.T) {
	f := newFixture(t)
	ft := serve(f.rel)
	delete(ft.files, "/runsheet/mesh/releases/download/v1.2.3/SHA256SUMS.sigstore.json")
	_, err := Fetch(context.Background(), GitHub{Client: &http.Client{Transport: ft}}, testVersion, "linux", "amd64")
	if !errors.Is(err, ErrMissingAsset) {
		t.Fatalf("Fetch = %v, want ErrMissingAsset", err)
	}
	if r := Reason(err); r != "release v1.2.3 has no SHA256SUMS.sigstore.json on runsheet/mesh (404: not published there, or not a signed release)" {
		t.Errorf("Reason = %q", r)
	}

	// No binary for this OS/arch.
	_, err = Fetch(context.Background(), GitHub{Client: &http.Client{Transport: serve(f.rel)}}, testVersion, "windows", "amd64")
	if !errors.Is(err, ErrMissingAsset) {
		t.Fatalf("Fetch windows = %v, want ErrMissingAsset", err)
	}
}
