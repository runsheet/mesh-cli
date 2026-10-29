package mcprelay

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/sockettest"
)

// A session's first MCP call that races its lease's sessionproxy coming
// up waits for the socket and succeeds, where it failed with "dial unix
// …: connect: no such file or directory" (TestWritePath, 2026-10-30); a
// socket that never comes is still an error, once SocketWait passes.
func TestASocketRelayWaitsForALateSocket(t *testing.T) {
	dir := sockettest.Dir(t, "late")
	sock := filepath.Join(dir, "s")
	go func() {
		time.Sleep(300 * time.Millisecond)
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Error(err)
			return
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != SocketPrefix+"conform" {
				http.NotFound(w, r)
				return
			}
			b, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", "s1")
			w.Write([]byte(strings.Replace(string(b), `"method":"initialize"`, `"result":{}`, 1)))
		})}
		t.Cleanup(func() { srv.Close() })
		srv.Serve(ln)
	}()
	var out bytes.Buffer
	r := &Relay{Socket: sock, Server: "conform", Stderr: io.Discard}
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n")
	if err := r.Run(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != `{"jsonrpc":"2.0","id":1,"result":{}}` {
		t.Fatalf("the relay answered %s", got)
	}

	out.Reset()
	r = &Relay{Socket: filepath.Join(dir, "never"), Server: "conform", Stderr: io.Discard, SocketWait: 200 * time.Millisecond}
	start := time.Now()
	in = strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"initialize"}` + "\n")
	if err := r.Run(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no such file or directory") || time.Since(start) > 5*time.Second {
		t.Fatalf("a socket that never comes: %s after %s", out.String(), time.Since(start))
	}
}
