package gitdata

import (
	"context"
	"net/http/httptest"
	"testing"
)

func TestBlobDecodesBase64Content(t *testing.T) {
	srv := httptest.NewServer(serveFixture(t, "blob.json"))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	got, err := c.Blob(context.Background(), "octo/widgets", "95b966ae1c569b8dd995d08a2e8ef5e93b6a0987")
	if err != nil {
		t.Fatalf("Blob: %v", err)
	}
	if string(got) != "Hello, world!" {
		t.Fatalf("Blob = %q, want %q", got, "Hello, world!")
	}
}
