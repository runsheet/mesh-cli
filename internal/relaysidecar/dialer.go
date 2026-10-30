package relaysidecar

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// The sidecar's pod has no route out. Every connection it opens to an
// upstream — Latchkey, the product's MCP host, the git vendor, a host
// the brief's egress names — is an HTTP CONNECT on the lease's socket to
// the node, which allows it or refuses it by host and port and then
// only pipes bytes: TLS runs from the sidecar to the upstream, and the
// node reads nothing past the CONNECT line.

// connectTimeout bounds the CONNECT exchange itself, not the stream.
const connectTimeout = 30 * time.Second

// Dialer opens connections through the node's tunnel on Socket.
type Dialer struct {
	// Socket is the lease's socket to the node.
	Socket string
}

// RefusedError is the node's refusal of a CONNECT: the host it was
// asked for and why.
type RefusedError struct {
	Addr   string
	Status int
	Reason string
}

func (e *RefusedError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("the node's tunnel refused %s: %d", e.Addr, e.Status)
	}
	return fmt.Sprintf("the node's tunnel refused %s: %d %s", e.Addr, e.Status, e.Reason)
}

// DialContext is a connection to addr (host:port) through the tunnel.
// network must be a TCP one.
func (d *Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if !strings.HasPrefix(network, "tcp") {
		return nil, fmt.Errorf("relaysidecar: the tunnel carries tcp alone, not %s", network)
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return nil, fmt.Errorf("relaysidecar: %q is not host:port", addr)
	}
	var nd net.Dialer
	c, err := nd.DialContext(ctx, "unix", d.Socket)
	if err != nil {
		return nil, fmt.Errorf("relaysidecar: the node's tunnel: %w", err)
	}
	deadline := time.Now().Add(connectTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = c.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Now()) })
	defer stop()
	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", addr, addr); err != nil {
		c.Close()
		return nil, fmt.Errorf("relaysidecar: the node's tunnel: %w", err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		c.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("relaysidecar: the node's tunnel: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		c.Close()
		return nil, &RefusedError{Addr: addr, Status: resp.StatusCode, Reason: strings.TrimSpace(string(b))}
	}
	_ = c.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: c, r: br}, nil
	}
	return c, nil
}

// Transport is an http.Transport whose every connection is through the
// tunnel; TLS is the transport's own, end to end.
func (d *Dialer) Transport() *http.Transport {
	return &http.Transport{
		DialContext:         d.DialContext,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        16,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 30 * time.Second,
	}
}

// bufferedConn is a conn whose first bytes were read past the CONNECT
// answer.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// IsRefused says whether err is the node's tunnel refusing a host.
func IsRefused(err error) bool {
	var r *RefusedError
	return errors.As(err, &r)
}
