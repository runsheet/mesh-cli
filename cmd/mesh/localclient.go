package main

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
)

// LocalClient talks to the mesh daemon over ~/.mesh/daemon.sock. All HTTP
// requests use a fake "unix" host — the custom transport routes everything
// through the Unix socket regardless.
type LocalClient struct {
	http *http.Client
	sock string
}

// NewLocalClient returns a client pointed at ~/.mesh/daemon.sock.
func NewLocalClient() (*LocalClient, error) {
	sock, err := DaemonSocketPath()
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}
	return &LocalClient{
		http: &http.Client{Transport: transport, Timeout: 0},
		sock: sock,
	}, nil
}

func (c *LocalClient) url(path string) string {
	return "http://unix" + path
}

func (c *LocalClient) do(ctx context.Context, method, path string, body any, out any) error {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.url(path), reqBody)
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
	var parsed errorResponse
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &parsed); err == nil && parsed.Error != "" {
			return errors.New(parsed.Error)
		}
	}
	return fmt.Errorf("daemon returned %s: %s", status, strings.TrimSpace(string(raw)))
}

// -- typed surface --

func (c *LocalClient) ListSessions(ctx context.Context, all bool) ([]sessionView, error) {
	path := "/sessions"
	if all {
		path = "/sessions?all=true"
	}
	var out []sessionView
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *LocalClient) CreateSession(ctx context.Context, req createSessionRequest) (*createSessionResponse, error) {
	var out createSessionResponse
	if err := c.do(ctx, http.MethodPost, "/sessions", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *LocalClient) InspectSession(ctx context.Context, name string) (map[string]any, error) {
	var out map[string]any
	if err := c.do(ctx, http.MethodGet, "/sessions/"+url.PathEscape(name), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *LocalClient) RemoveSession(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/sessions/"+url.PathEscape(name), nil, nil)
}

func (c *LocalClient) RestartSession(ctx context.Context, name string) (map[string]string, error) {
	var out map[string]string
	if err := c.do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(name)+"/restart", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// pushAgent calls POST /sessions/<name>/push on the daemon socket. The raw
// status + body are returned so the CLI can branch on 409 (name collision) and
// 412 (standalone mode) without losing the structured error payload.
func (c *LocalClient) pushAgent(ctx context.Context, name string, req pushSessionRequest) (int, []byte, error) {
	return c.doRaw(ctx, http.MethodPost, "/sessions/"+url.PathEscape(name)+"/push", req)
}

// pullAgent calls POST /cluster/pull on the daemon socket.
func (c *LocalClient) pullAgent(ctx context.Context, req pullSessionRequest) (int, []byte, error) {
	return c.doRaw(ctx, http.MethodPost, "/cluster/pull", req)
}

// doRaw is like do but returns the raw status + body so callers can parse
// structured error responses (409 name_taken, already_adopted, etc.).
func (c *LocalClient) doRaw(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reqBody = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(path), reqBody)
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

func (c *LocalClient) UpdateSession(ctx context.Context, name string) (map[string]any, error) {
	var out map[string]any
	if err := c.do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(name)+"/update", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *LocalClient) ListTemplates(ctx context.Context) ([]TemplateCacheEntry, error) {
	var out []TemplateCacheEntry
	if err := c.do(ctx, http.MethodGet, "/templates", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *LocalClient) PullTemplate(ctx context.Context, source string) (*pullTemplateResponse, error) {
	var out pullTemplateResponse
	if err := c.do(ctx, http.MethodPost, "/templates/pull", pullTemplateRequest{Source: source}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *LocalClient) Logs(ctx context.Context, name string, follow bool, out io.Writer) error {
	q := ""
	if follow {
		q = "?follow=true"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/sessions/"+url.PathEscape(name)+"/logs"+q), nil)
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
// bidirectional bytes. Caller is responsible for raw-mode stdin and
// Ctrl-P/Ctrl-Q detection.
func (c *LocalClient) Attach(ctx context.Context, name string, stdin io.Reader, stdout io.Writer, onDetach func() bool) error {
	conn, err := net.Dial("unix", c.sock)
	if err != nil {
		return wrapDialErr(err, c.sock)
	}
	defer conn.Close()

	// Hand-rolled HTTP request: the stdlib client wants to reuse the
	// connection for the response body, which doesn't play well with our
	// arbitrary byte stream. Sending the request directly keeps us in charge.
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

	// daemon → stdout
	go func() {
		// Drain any bytes the reader already buffered past the headers first.
		if reader.Buffered() > 0 {
			if _, err := io.CopyN(stdout, reader, int64(reader.Buffered())); err != nil {
				errCh <- err
				return
			}
		}
		_, err := io.Copy(stdout, conn)
		errCh <- err
	}()

	// stdin → daemon, with detect-sequence interception
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				if onDetach != nil {
					// Filter detach sequence out of the bytes sent to daemon.
					clean := filterDetachSequence(chunk, onDetach)
					if len(clean) > 0 {
						if _, werr := conn.Write(clean); werr != nil {
							errCh <- werr
							return
						}
					}
					// If onDetach returned true, we stop pumping.
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

	// Block on context or either direction closing.
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

// filterDetachSequence watches for Ctrl-P (0x10) followed by Ctrl-Q (0x11)
// within ~250ms. When the sequence completes, the callback is invoked and the
// sequence bytes are NOT forwarded to the PTY. Incomplete sequences (just
// Ctrl-P) pass through intact so a user who hits Ctrl-P by accident doesn't
// freeze.
//
// State lives in a package-level var — the attach goroutine is single-caller
// and the CLI only runs one attach at a time, so this is fine.
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
				// Expired — flush the pending Ctrl-P and reset.
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
			// Pending Ctrl-P but the follow-up wasn't Ctrl-Q. Flush pending
			// and fall through to normal handling.
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

func resetDetachState() {
	detachState = detachFSM{}
	detachTrippedF = false
}
