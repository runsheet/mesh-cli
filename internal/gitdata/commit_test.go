package gitdata

import (
	"context"
	"net/http/httptest"
	"testing"
)

func TestCommitTreeAndParents(t *testing.T) {
	srv := httptest.NewServer(serveFixture(t, "commit.json"))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	got, err := c.Commit(context.Background(), "octo/widgets", "6dcb09b5b57875f334f61aebed695e2e4193db5")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got.TreeSHA != "6b2919a53dd3c3e5b3a98cee04f9622f46f8d2e4" {
		t.Fatalf("TreeSHA = %q", got.TreeSHA)
	}
	want := []string{"7638417db6d59f3c431d3e1f261cc637155684c", "7add17db6d59f3c431d3e1f261cc637155684cd"}
	if len(got.Parents) != len(want) || got.Parents[0] != want[0] || got.Parents[1] != want[1] {
		t.Fatalf("Parents = %v, want %v", got.Parents, want)
	}
}
