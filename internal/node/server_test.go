package node

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	dir, err := os.MkdirTemp("", "srv") // short: a socket's path is bounded
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
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

// The admin endpoints answer the loop live: health, the status with its
// connections, the jobs on disk, and Prometheus metrics.
func TestAdminHandlerAnswersTheLoop(t *testing.T) {
	l := testLayout(t)
	scopes := connected(Scopes{{Login: "acme", ID: "org-a", Cap: 2}})
	if _, err := openJob(l, scopes, "org-a", JobMeta{ID: "item-1", Unit: JobItem, ItemID: "item-1", Label: "ship-it/1"}); err != nil {
		t.Fatal(err)
	}
	loop := &Loop{Cfg: &Config{Name: "box", Slug: "box", NodeID: "node-a", Capacity: Fixed(3)}, Scopes: scopes, Version: "v1"}
	dir, _ := os.MkdirTemp("", "adm")
	t.Cleanup(func() { os.RemoveAll(dir) })
	s := &Server{Admin: AdminHandler(loop, l, time.Now())}
	defer s.Close()
	sock := filepath.Join(dir, "node.sock")
	if err := s.ListenAdmin(sock); err != nil {
		t.Fatal(err)
	}
	if code, body := getOn(t, sock, "/health"); code != 200 || !strings.Contains(body, `"ok": true`) {
		t.Fatalf("health %d %s", code, body)
	}
	_, body := getOn(t, sock, "/status")
	var st NodeStatus
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if st.Name != "box" || st.Version != "v1" || len(st.Connection) != 1 || st.Connection[0].ConnectionID != "conn-acme" || st.Connection[0].Cap != 2 {
		t.Fatalf("status %+v", st)
	}
	_, body = getOn(t, sock, "/jobs")
	var jobs []StatusJob
	if err := json.Unmarshal([]byte(body), &jobs); err != nil || len(jobs) != 1 || jobs[0].ID != "item-1" || jobs[0].ConnectionID != "conn-acme" {
		t.Fatalf("jobs %s (%v)", body, err)
	}
	_, body = getOn(t, sock, "/metrics")
	for _, want := range []string{`mesh_node_connection_cap{org="acme",connection="conn-acme"} 2`, `mesh_node_jobs{unit="item"} 1`, "mesh_node_capacity"} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q:\n%s", want, body)
		}
	}
	if code, _ := getOn(t, sock, "/nope"); code != http.StatusNotFound {
		t.Errorf("unknown path %d", code)
	}
}
