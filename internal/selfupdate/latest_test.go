package selfupdate

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// Latest asks GitHub's API for the latest release, which is never a draft
// or a prerelease, and refuses an answer that is one anyway.
func TestLatest(t *testing.T) {
	ft := &fakeTransport{files: map[string][]byte{
		"/repos/runsheet/mesh/releases/latest": []byte(`{"tag_name":"v1.4.0","prerelease":false,"draft":false}`),
	}}
	got, err := GitHub{Client: &http.Client{Transport: ft}}.Latest(context.Background())
	if err != nil || got != "v1.4.0" {
		t.Fatalf("Latest = %q, %v", got, err)
	}
	if len(ft.seen) != 1 || ft.seen[0] != LatestReleaseURL {
		t.Fatalf("asked %v", ft.seen)
	}

	ft.files["/repos/runsheet/mesh/releases/latest"] = []byte(`{"tag_name":"v1.5.0-rc.1","prerelease":true}`)
	if got, err := (GitHub{Client: &http.Client{Transport: ft}}).Latest(context.Background()); err == nil {
		t.Fatalf("Latest took a prerelease: %q", got)
	}

	delete(ft.files, "/repos/runsheet/mesh/releases/latest")
	if _, err := (GitHub{Client: &http.Client{Transport: ft}}).Latest(context.Background()); err == nil || !strings.Contains(err.Error(), "no release yet") {
		t.Fatalf("Latest with no release = %v", err)
	}
}
