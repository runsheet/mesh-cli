package channel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The daemon and the channel server are two processes: Claude Code
// spawns the server on stdio, the daemon runs the loop. They meet on a
// unix socket (~/.mesh/node.sock): the daemon's Hub pushes events down
// it, the server's Conn sends "done" back when an item's session ended.

// Message is one line on the socket.
type Message struct {
	Type    string            `json:"type"` // event | done | hello
	Content string            `json:"content,omitempty"`
	Meta    map[string]string `json:"meta,omitempty"`
	ItemID  string            `json:"item_id,omitempty"`
	Outcome string            `json:"outcome,omitempty"`
}

// SockPath is ~/.mesh/node.sock.
func SockPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".mesh", "node.sock"), nil
}

// Hub is the daemon's side: it accepts channel servers and fans events
// out to them, holding events until one is connected.
type Hub struct {
	// OnDone hears the session's finish/fail reports.
	OnDone func(itemID, outcome string)

	mu      sync.Mutex
	conns   map[net.Conn]*json.Encoder
	pending []Message
	ln      net.Listener
}

// Listen starts the hub on path, replacing a stale socket.
func (h *Hub) Listen(path string) error {
	_ = os.Remove(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	_ = os.Chmod(path, 0o600)
	h.mu.Lock()
	h.ln = ln
	h.conns = map[net.Conn]*json.Encoder{}
	h.mu.Unlock()
	go h.accept(ln)
	return nil
}

// Close stops listening and drops the connections.
func (h *Hub) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.conns {
		_ = c.Close()
	}
	if h.ln != nil {
		return h.ln.Close()
	}
	return nil
}

// Connected reports whether a channel server is attached.
func (h *Hub) Connected() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns) > 0
}

func (h *Hub) accept(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		enc := json.NewEncoder(c)
		h.mu.Lock()
		h.conns[c] = enc
		pending := h.pending
		h.pending = nil
		h.mu.Unlock()
		for _, m := range pending {
			_ = enc.Encode(m)
		}
		go h.read(c)
	}
}

func (h *Hub) read(c net.Conn) {
	defer func() {
		h.mu.Lock()
		delete(h.conns, c)
		h.mu.Unlock()
		_ = c.Close()
	}()
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		var m Message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		if m.Type == "done" && h.OnDone != nil {
			h.OnDone(m.ItemID, m.Outcome)
		}
	}
}

// Push sends an event to the attached servers, or holds it for the next.
func (h *Hub) Push(content string, meta map[string]string) {
	m := Message{Type: "event", Content: content, Meta: meta}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.conns) == 0 {
		h.pending = append(h.pending, m)
		if len(h.pending) > 256 {
			h.pending = h.pending[len(h.pending)-256:]
		}
		return
	}
	for c, enc := range h.conns {
		if err := enc.Encode(m); err != nil {
			_ = c.Close()
			delete(h.conns, c)
		}
	}
}

// Conn is the server's side: events in, done out.
type Conn struct {
	mu  sync.Mutex
	c   net.Conn
	enc *json.Encoder
}

// Dial connects to the hub, retrying until ctx is done, and delivers
// every event to onEvent from a goroutine. It returns once connected.
func Dial(ctx context.Context, path string, onEvent func(content string, meta map[string]string)) (*Conn, error) {
	var c net.Conn
	for {
		var err error
		c, err = (&net.Dialer{}).DialContext(ctx, "unix", path)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	conn := &Conn{c: c, enc: json.NewEncoder(c)}
	_ = conn.send(Message{Type: "hello"})
	go func() {
		sc := bufio.NewScanner(c)
		sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
		for sc.Scan() {
			var m Message
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil || m.Type != "event" {
				continue
			}
			onEvent(m.Content, m.Meta)
		}
	}()
	return conn, nil
}

func (c *Conn) send(m Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.c == nil {
		return errors.New("hub: not connected")
	}
	return c.enc.Encode(m)
}

// Done tells the daemon an item's session ended.
func (c *Conn) Done(itemID, outcome string) error {
	return c.send(Message{Type: "done", ItemID: itemID, Outcome: outcome})
}

// Close closes the connection.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.c == nil {
		return nil
	}
	err := c.c.Close()
	c.c = nil
	return err
}
