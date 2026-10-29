package nodev1

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The lease waits for its socket: a listener that comes up late is
// waited for; one that never does is an error naming the socket.
func TestWaitListeningWaitsForALateSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "mv1r-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	go func() {
		time.Sleep(150 * time.Millisecond)
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Error(err)
			return
		}
		t.Cleanup(func() { ln.Close() })
	}()
	start := time.Now()
	if err := waitListening(context.Background(), sock, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Fatal("answered before the socket listened")
	}
	err = waitListening(context.Background(), filepath.Join(dir, "never"), 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "never") {
		t.Fatalf("a socket that never listens: %v", err)
	}
}
