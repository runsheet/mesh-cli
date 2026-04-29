package channels

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/ipc"
)

// fakeGateway listens on a unix socket and handles exactly one subscriber at a
// time. It sends a subscribe-ack, then sits idle until Close() is called.
// connectCount counts completed handshakes (subscribe + ack sent).
type fakeGateway struct {
	ln           net.Listener
	sockPath     string
	connectCount atomic.Int32
	stopCh       chan struct{}
}

func newFakeGateway(t *testing.T) *fakeGateway {
	t.Helper()
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "gw.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("fake gateway listen: %v", err)
	}
	fg := &fakeGateway{ln: ln, sockPath: sockPath, stopCh: make(chan struct{})}
	go fg.serve()
	return fg
}

func (fg *fakeGateway) serve() {
	for {
		conn, err := fg.ln.Accept()
		if err != nil {
			return
		}
		go fg.handleConn(conn)
	}
}

func (fg *fakeGateway) handleConn(conn net.Conn) {
	defer conn.Close()
	rd := ipc.NewReader(conn)
	wr := ipc.NewWriter(conn)

	// Read the subscribe envelope.
	env, err := rd.Read()
	if err != nil || env.Type != "subscribe" {
		return
	}

	// Send subscribe-ack.
	if err := wr.Write(ipc.AckEnv("test-agent")); err != nil {
		return
	}
	fg.connectCount.Add(1)

	// Hold the connection open until the gateway is closed from the server
	// side (simulates normal message delivery period).
	<-fg.stopCh
}

func (fg *fakeGateway) Close() {
	select {
	case <-fg.stopCh:
	default:
		close(fg.stopCh)
	}
	fg.ln.Close()
}

// restart resets the stop channel and re-listens, simulating a gateway respawn.
func (fg *fakeGateway) restart(t *testing.T) {
	t.Helper()
	fg.stopCh = make(chan struct{})
	ln, err := net.Listen("unix", fg.sockPath)
	if err != nil {
		t.Fatalf("fake gateway restart listen: %v", err)
	}
	fg.ln = ln
	go fg.serve()
}

// TestResubscribeOnGatewayDrop verifies that when a gateway connection drops
// (server closes the conn), the router re-establishes the subscription
// automatically without a daemon restart.
func TestResubscribeOnGatewayDrop(t *testing.T) {
	fg := newFakeGateway(t)
	defer fg.ln.Close()

	// Patch WhatsappGatewaySockPath to return our fake socket.
	// We use a whatsapp channel spec pointing at the fake socket via account name.
	// The paths package resolves the socket path; we work around it by creating
	// a symlink from the expected path to our fake socket, OR by using a custom
	// test setup.
	//
	// Simpler: use subscribeWhatsappGateway directly with a spec whose account
	// resolves to the fake socket path. But paths.WhatsappGatewaySockPath uses
	// the account name to build a fixed path (~/.mesh/...). We can't easily
	// override that without dependency injection.
	//
	// Instead, test resubscribeGateway using subscribeDiscordGateway — same
	// logic — by pre-creating the expected socket symlink in a temp mesh dir.

	// Set MESH_DIR to temp dir so paths resolve under it.
	origHome := os.Getenv("HOME")
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	// Create the directory structure the paths package expects, then symlink
	// our fake socket. paths.WhatsappGatewaySockPath("testaccount") returns
	// ~/.mesh/whatsapp-accounts/testaccount/gateway.sock
	meshDir := filepath.Join(tmpHome, ".mesh", "whatsapp-accounts", "testaccount")
	if err := os.MkdirAll(meshDir, 0o700); err != nil {
		t.Fatalf("mkdir mesh dir: %v", err)
	}
	gwSockPath := filepath.Join(meshDir, "gateway.sock")
	// Symlink our fake gateway socket.
	if err := os.Symlink(fg.sockPath, gwSockPath); err != nil {
		t.Fatalf("symlink gateway: %v", err)
	}

	ch := Spec{
		Type:   "whatsapp",
		Config: map[string]any{"account": "testaccount", "chatJid": "123@s.whatsapp.net"},
	}

	r := &agentRouter{
		agentName: "test-agent",
		gwConns:   map[string]*routerGWConn{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Launch resubscribeGateway — it should connect immediately.
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.resubscribeGateway(ctx, ch, func(format string, args ...any) {
			t.Logf("[router] "+format, args...)
		})
	}()

	// Wait for first connection to complete.
	deadline := time.Now().Add(2 * time.Second)
	for fg.connectCount.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("timeout waiting for first gateway connection")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("first connection established (count=%d)", fg.connectCount.Load())

	// Drop the gateway from the server side by closing all connections.
	// We do this by closing the stopCh — existing handleConn goroutines will
	// close their conns, causing pumpGateway to get a read error.
	fg.Close()

	// Remove the stale symlink and re-create the fake gateway at the same path.
	time.Sleep(50 * time.Millisecond) // let pumpGateway detect the drop
	if err := os.Remove(gwSockPath); err != nil {
		t.Fatalf("remove stale symlink: %v", err)
	}
	fg.restart(t)
	// Re-symlink to the new listener.
	if err := os.Symlink(fg.sockPath, gwSockPath); err != nil {
		t.Fatalf("re-symlink gateway: %v", err)
	}

	// Wait for second connection — the resubscribe loop should re-dial.
	deadline = time.Now().Add(4 * time.Second)
	for fg.connectCount.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for resubscribe reconnect (count=%d)", fg.connectCount.Load())
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("resubscribe reconnect confirmed (count=%d)", fg.connectCount.Load())

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("resubscribeGateway did not exit after ctx cancel")
	}
}

// TestResubscribeProtocolErrorNoRetry verifies that a protocol error (gateway
// returns "error" envelope during handshake) causes resubscribeGateway to exit
// immediately without retrying.
func TestResubscribeProtocolErrorNoRetry(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "gw-proto.sock")

	// A fake gateway that always rejects with an error envelope.
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	var handshakeCount atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				rd := ipc.NewReader(c)
				wr := ipc.NewWriter(c)
				env, err := rd.Read()
				if err != nil || env.Type != "subscribe" {
					return
				}
				handshakeCount.Add(1)
				_ = wr.Write(ipc.ErrorEnv("unauthorized", "token rejected"))
			}(conn)
		}
	}()
	defer ln.Close()

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	meshDir := filepath.Join(tmpHome, ".mesh", "whatsapp-accounts", "errtest")
	if err := os.MkdirAll(meshDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	gwSock := filepath.Join(meshDir, "gateway.sock")
	if err := os.Symlink(sockPath, gwSock); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	ch := Spec{
		Type:   "whatsapp",
		Config: map[string]any{"account": "errtest", "chatJid": "456@s.whatsapp.net"},
	}
	r := &agentRouter{agentName: "err-agent", gwConns: map[string]*routerGWConn{}}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.resubscribeGateway(ctx, ch, func(format string, args ...any) {
			t.Logf("[router] "+format, args...)
		})
	}()

	// resubscribeGateway must exit promptly (protocol error, not transport).
	select {
	case <-done:
		// Good — exited without retrying.
	case <-time.After(2 * time.Second):
		t.Error("resubscribeGateway did not exit promptly on protocol error")
	}

	// Should have only tried once (no retry on protocol error).
	if n := handshakeCount.Load(); n != 1 {
		t.Errorf("handshakeCount = %d, want 1 (no retry on protocol error)", n)
	}
}

// TestBackoffSequence verifies the exponential backoff cap-doubling behaviour
// in isolation using the constants from the package.
func TestBackoffSequence(t *testing.T) {
	delay := gwResubscribeInitialDelay
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30} // seconds
	for i, w := range want {
		if delay != w*time.Second {
			t.Errorf("step %d: got %v, want %v", i, delay, w*time.Second)
		}
		delay *= 2
		if delay > gwResubscribeMaxDelay {
			delay = gwResubscribeMaxDelay
		}
	}
}
