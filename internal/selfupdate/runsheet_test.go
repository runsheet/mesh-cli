package selfupdate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRunsheetSourceFetchesAndVerifies fetches and verifies a release
// end to end from a local HTTP server shaped like runsheet's own release
// store (api.runsheet.dev/releases/mesh/<version>/<asset>, ADR 002's
// 2026-11-15 amendment): a GitHub Source whose BaseURL points at it, which
// is exactly how Channel.Runsheet is built in production
// (DefaultChannel). The release is signed under the platform identity —
// our own Sigstore, not GitHub Actions' — and a 404 for an asset the
// store does not hold surfaces as ErrMissingAsset, not a generic error.
func TestRunsheetSourceFetchesAndVerifies(t *testing.T) {
	f := newFixtureFor(t, "v1.2.3")
	platformRelease(t, f, "v1.2.3", platformSAN, PlatformIssuer)

	mux := http.NewServeMux()
	dir := "/releases/runsheet/mesh-cli/" + f.rel.Version + "/"
	for name, b := range map[string][]byte{
		"mesh-linux-amd64":  f.rel.Binary,
		"mesh-linux-arm64":  []byte("arm64"),
		"mesh-darwin-arm64": []byte("darwin"),
		SumsAsset:           f.rel.Sums,
		BundleAsset:         f.rel.Bundle,
	} {
		b := b
		mux.HandleFunc(dir+name, func(w http.ResponseWriter, r *http.Request) {
			w.Write(b)
		})
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	src := GitHub{BaseURL: srv.URL + "/releases/runsheet/mesh-cli/"}
	got, err := Fetch(context.Background(), src, f.rel.Version, "linux", "amd64")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := Verify(got, f.policy); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if _, err := src.Asset(context.Background(), f.rel.Version, "mesh-windows-amd64"); !errors.Is(err, ErrMissingAsset) {
		t.Fatalf("missing asset: Asset = %v, want ErrMissingAsset", err)
	}
}
