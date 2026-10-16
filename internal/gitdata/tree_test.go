package gitdata

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
)

func TestTreeTruncatedIsATypedError(t *testing.T) {
	srv := httptest.NewServer(serveFixture(t, "tree_truncated.json"))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	_, err := c.Tree(context.Background(), "octo/widgets", "9b1a0c4b7b3f4a2e9c8d7e6f5a4b3c2d1e0f9a8b")
	if !IsTruncated(err) {
		t.Fatalf("err = %v, IsTruncated = false", err)
	}
	var te *TruncatedError
	if !errors.As(err, &te) {
		t.Fatalf("errors.As(%v, *TruncatedError) = false", err)
	}
	if te.Repo != "octo/widgets" || te.TreeSHA != "9b1a0c4b7b3f4a2e9c8d7e6f5a4b3c2d1e0f9a8b" {
		t.Fatalf("TruncatedError = %+v", te)
	}
}

func TestTreeEntriesRecursive(t *testing.T) {
	srv := httptest.NewServer(serveFixture(t, "tree_ok.json"))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	got, err := c.Tree(context.Background(), "octo/widgets", "6b2919a53dd3c3e5b3a98cee04f9622f46f8d2e4")
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	want := []TreeEntry{
		{Path: "README.md", Mode: "100644", Type: "blob", SHA: "95b966ae1c569b8dd995d08a2e8ef5e93b6a0987"},
		{Path: "cmd", Mode: "040000", Type: "tree", SHA: "4b825dc642cb6eb9a060e54bf8d69288fbee4904"},
		{Path: "cmd/main.go", Mode: "100755", Type: "blob", SHA: "2222222222222222222222222222222222222b"},
	}
	if len(got) != len(want) {
		t.Fatalf("entries = %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
