// Package client provides an HTTP-over-Unix-socket client for the mesh daemon.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/paths"
)

// Client talks to the mesh daemon over ~/.mesh/daemon.sock. All HTTP
// requests use a fake "unix" host — the custom transport routes everything
// through the Unix socket regardless.
type Client struct {
	http *http.Client
	sock string
}

// New returns a client pointed at ~/.mesh/daemon.sock.
func New() (*Client, error) {
	sock, err := paths.DaemonSocketPath()
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}
	return &Client{
		http: &http.Client{Transport: transport, Timeout: 0},
		sock: sock,
	}, nil
}

func (c *Client) baseURL(path string) string {
	return "http://unix" + path
}

// Do sends an HTTP request to the daemon and JSON-decodes the response into out
// (may be nil to discard). body is JSON-marshalled if non-nil.
func (c *Client) Do(ctx context.Context, method, path string, body any, out any) error {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL(path), reqBody)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return wrapDialErr(err, c.sock)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return apiErrorFromBody(resp.Status, raw)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// DoRaw is like Do but returns the raw status + body so callers can parse
// structured error responses (e.g. 409 name_taken).
func (c *Client) DoRaw(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reqBody = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL(path), reqBody)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, wrapDialErr(err, c.sock)
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		return resp.StatusCode, nil, rerr
	}
	return resp.StatusCode, raw, nil
}

// Logs streams the session log output to out. If follow is true the stream
// stays open until ctx is cancelled or the daemon closes the connection.
func (c *Client) Logs(ctx context.Context, name string, follow bool, out io.Writer) error {
	q := ""
	if follow {
		q = "?follow=true"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL("/sessions/"+url.PathEscape(name)+"/logs"+q), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return wrapDialErr(err, c.sock)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return apiErrorFromBody(resp.Status, raw)
	}
	if dropped := resp.Header.Get("X-Mesh-Dropped-Bytes"); dropped != "" && dropped != "0" {
		fmt.Fprintf(out, "[mesh] %s bytes dropped from ring buffer\n", dropped)
	}
	_, err = io.Copy(out, resp.Body)
	return err
}

// Attach hijacks the underlying Unix connection and gives the caller raw
// bidirectional bytes.
func (c *Client) Attach(ctx context.Context, name string, stdin io.Reader, stdout io.Writer, onDetach func() bool) error {
	conn, err := net.Dial("unix", c.sock)
	if err != nil {
		return wrapDialErr(err, c.sock)
	}
	defer conn.Close()

	fmt.Fprintf(conn, "POST /sessions/%s/attach HTTP/1.1\r\nHost: unix\r\nUpgrade: mesh-attach\r\nConnection: Upgrade\r\n\r\n", url.PathEscape(name))

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		return fmt.Errorf("read attach response: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return apiErrorFromBody(resp.Status, raw)
	}

	errCh := make(chan error, 2)

	go func() {
		if reader.Buffered() > 0 {
			if _, err := io.CopyN(stdout, reader, int64(reader.Buffered())); err != nil {
				errCh <- err
				return
			}
		}
		_, err := io.Copy(stdout, conn)
		errCh <- err
	}()

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				if onDetach != nil {
					clean := filterDetachSequence(chunk, onDetach)
					if len(clean) > 0 {
						if _, werr := conn.Write(clean); werr != nil {
							errCh <- werr
							return
						}
					}
					if detachTripped(onDetach) {
						errCh <- nil
						return
					}
				} else {
					if _, werr := conn.Write(chunk); werr != nil {
						errCh <- werr
						return
					}
				}
			}
			if err != nil {
				errCh <- err
				return
			}
		}
	}()

	select {
	case <-ctx.Done():
		_ = conn.Close()
		return ctx.Err()
	case e := <-errCh:
		_ = conn.Close()
		if errors.Is(e, io.EOF) {
			return nil
		}
		return e
	}
}

// APIErrorFromBody parses a daemon error response body into an error value.
func APIErrorFromBody(status string, raw []byte) error {
	return apiErrorFromBody(status, raw)
}

func wrapDialErr(err error, sock string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "no such file") || strings.Contains(msg, "connect: connection refused") {
		return fmt.Errorf("daemon not running; start it with `mesh node start` (socket=%s)", sock)
	}
	return err
}

func apiErrorFromBody(status string, raw []byte) error {
	var parsed struct {
		Error string `json:"error"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &parsed); err == nil && parsed.Error != "" {
			return errors.New(parsed.Error)
		}
	}
	return fmt.Errorf("daemon returned %s: %s", status, strings.TrimSpace(string(raw)))
}

var (
	detachState    detachFSM
	detachTrippedF bool
)

type detachFSM struct {
	seenCtrlP bool
	at        time.Time
}

const (
	ctrlP             = 0x10
	ctrlQ             = 0x11
	detachSeqDeadline = 250 * time.Millisecond
)

func filterDetachSequence(in []byte, onDetach func() bool) []byte {
	out := make([]byte, 0, len(in))
	for _, b := range in {
		if detachState.seenCtrlP {
			if time.Since(detachState.at) > detachSeqDeadline {
				out = append(out, ctrlP)
				detachState = detachFSM{}
			}
		}
		if detachState.seenCtrlP && b == ctrlQ {
			detachState = detachFSM{}
			detachTrippedF = true
			if onDetach != nil {
				_ = onDetach()
			}
			return out
		}
		if detachState.seenCtrlP {
			out = append(out, ctrlP)
			detachState = detachFSM{}
		}
		if b == ctrlP {
			detachState = detachFSM{seenCtrlP: true, at: time.Now()}
			continue
		}
		out = append(out, b)
	}
	return out
}

func detachTripped(_ func() bool) bool { return detachTrippedF }

// DetachTripped reports whether the Ctrl-P Ctrl-Q detach sequence was
// completed during the last Attach call.
func DetachTripped() bool { return detachTrippedF }

// ResetDetachState resets the Ctrl-P/Ctrl-Q detection state. Exported for
// test use.
func ResetDetachState() {
	detachState = detachFSM{}
	detachTrippedF = false
}
