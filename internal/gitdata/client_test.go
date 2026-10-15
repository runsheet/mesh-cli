package gitdata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// serveFixture answers every request with testdata/name, untouched.
func serveFixture(t *testing.T, name string) http.HandlerFunc {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}
}

func TestClientSendsTheContextToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"sha":"x","commit":{"tree":{"sha":"t"}},"parents":[]}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	ctx := WithToken(context.Background(), "item-token-123")
	if _, err := c.Commit(ctx, "octo/widgets", "x"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if gotAuth != "Bearer item-token-123" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
}

func TestClientTokenSourceTakesPrecedenceOverContext(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"sha":"x","commit":{"tree":{"sha":"t"}},"parents":[]}`))
	}))
	defer srv.Close()

	calls := 0
	c := &Client{BaseURL: srv.URL, Token: func(ctx context.Context) (string, error) {
		calls++
		return "fresh-token", nil
	}}
	ctx := WithToken(context.Background(), "stale-token")
	if _, err := c.Commit(ctx, "octo/widgets", "x"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if gotAuth != "Bearer fresh-token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if calls != 1 {
		t.Fatalf("Token called %d times", calls)
	}
}

func TestClientTellsA403FromA404(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/octo/widgets/commits/forbidden", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
	})
	mux.HandleFunc("/repos/octo/widgets/commits/missing", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}

	_, err := c.Commit(context.Background(), "octo/widgets", "forbidden")
	if !IsForbidden(err) {
		t.Fatalf("forbidden: err = %v, IsForbidden = false", err)
	}
	if IsNotFound(err) {
		t.Fatalf("forbidden: IsNotFound = true, want false")
	}

	_, err = c.Commit(context.Background(), "octo/widgets", "missing")
	if !IsNotFound(err) {
		t.Fatalf("missing: err = %v, IsNotFound = false", err)
	}
	if IsForbidden(err) {
		t.Fatalf("missing: IsForbidden = true, want false")
	}

	se := StatusErrorOf(err)
	if se == nil || se.StatusCode != http.StatusNotFound {
		t.Fatalf("StatusErrorOf = %#v", se)
	}
}

func TestClientA401IsUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	_, err := c.Commit(context.Background(), "octo/widgets", "x")
	if !IsUnauthorized(err) {
		t.Fatalf("err = %v, IsUnauthorized = false", err)
	}
}
