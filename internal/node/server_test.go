package node

import (
	"context"

	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/sockettest"
)

// unixClient is an HTTP client on the unix socket at sock.
func unixClient(sock string) *http.Client {
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
}

func getOn(t *testing.T, sock, path string) (int, string) {
	t.Helper()
	resp, err := unixClient(sock).Get("http://node" + path)
	if err != nil {
		t.Fatalf("GET %s on %s: %v", path, sock, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// One server serves every listener, each request reaching the handler
// of the listener it came in on alone: a session's socket answers its
// own session and never the admin endpoints or another session's; a
// socket unlistened stops answering and is gone.
func TestServerRoutesEachListenerToItsOwnHandler(t *testing.T) {
	dir := sockettest.Dir(t, "srv") // short: a socket's path is bounded
	named := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, name+" "+r.URL.Path) })
	}
	s := &Server{Admin: named("admin")}
	defer s.Close()
	admin, a, b := filepath.Join(dir, "node.sock"), filepath.Join(dir, "a", "s"), filepath.Join(dir, "b", "s")
	if err := s.ListenAdmin(admin); err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(a, named("session-a")); err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(b, named("session-b")); err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(a, named("again")); err == nil {
		t.Fatal("a served path was listened on twice")
	}
	for sock, want := range map[string]string{admin: "admin /status", a: "session-a /status", b: "session-b /status"} {
		if _, got := getOn(t, sock, "/status"); got != want {
			t.Errorf("%s answered %q, want %q", sock, got, want)
		}
	}
	if fi, err := os.Stat(a); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode: %v %v", fi, err)
	}
	if err := s.Unlisten(a); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a); !os.IsNotExist(err) {
		t.Fatalf("unlistened socket still there: %v", err)
	}
	if _, got := getOn(t, b, "/x"); got != "session-b /x" {
		t.Fatalf("b after a went: %q", got)
	}
}
