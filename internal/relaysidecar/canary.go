package relaysidecar

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"

	"projectmesh.io/mesh-cli/internal/sessionproxy"
)

// The lease's injected values are never in the pod, so the sidecar, which
// serves the session's product MCP and vendor API calls, cannot look for
// them itself (mesh-specs gap 28 item 5). It hands each call's scanned
// parts to the node (ScanPath), which holds the values and records an
// exposure, and each answer to the node to redact (RedactPath), before
// the session reads it. Either failing fails closed: the call is refused,
// the answer withheld.

// Paths of the lease's socket for the canary.
const (
	// ScanPath takes part of a call: its bytes as the body, where and
	// toward in the headers below. 204 when it holds no injected value,
	// 403 when it does.
	ScanPath = "/relay/scan"
	// RedactPath takes an answer and answers it redacted.
	RedactPath = "/relay/redact"
)

// The headers of a ScanPath post.
const (
	whereHeader  = "X-Mesh-Where"
	towardHeader = "X-Mesh-Toward"
)

// canaryBodyMax bounds a ScanPath or RedactPath post: the proxy's largest
// held answer, with room.
const canaryBodyMax = 32 << 20

// canaryTimeout bounds one canary call to the node.
const canaryTimeout = 30 * time.Second

// unchecked is what the session reads in place of an answer the node
// could not redact.
const unchecked = "relay: this answer could not be checked for injected credentials"

// remoteCanary is the node's canary, reached on the lease's socket.
type remoteCanary struct {
	node *http.Client
	logf func(string, ...any)
}

func (c *remoteCanary) post(path string, body []byte, header map[string]string) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), canaryTimeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://node"+path, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.node.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{resp.Body, closeAll{resp.Body, cancel}}
	return resp, nil
}

type closeAll struct {
	c      io.Closer
	cancel context.CancelFunc
}

func (c closeAll) Close() error { err := c.c.Close(); c.cancel(); return err }

func (c *remoteCanary) Scan(where, toward string, b []byte) error {
	resp, err := c.post(ScanPath, b, map[string]string{whereHeader: where, towardHeader: toward})
	if err != nil {
		c.logf("relaysidecar: the node did not scan %s: %v", where, err)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return &scanRefused{status: resp.StatusCode}
	}
	return nil
}

func (c *remoteCanary) Redact(s string) string {
	resp, err := c.post(RedactPath, []byte(s), nil)
	if err != nil {
		c.logf("relaysidecar: the node did not redact an answer: %v", err)
		return unchecked
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, canaryBodyMax+1))
	if err != nil || resp.StatusCode != http.StatusOK || len(out) > canaryBodyMax {
		return unchecked
	}
	return string(out)
}

type scanRefused struct{ status int }

func (e *scanRefused) Error() string {
	if e.status == http.StatusForbidden {
		return sessionproxy.CodeExposed
	}
	return "the node's scan answered " + http.StatusText(e.status)
}

// serveScan scans a sidecar's call on the node.
func (n *Node) serveScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", http.StatusMethodNotAllowed)
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, canaryBodyMax+1))
	if err != nil || len(b) > canaryBodyMax {
		http.Error(w, "not a scannable body", http.StatusRequestEntityTooLarge)
		return
	}
	if n.Canary == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := n.Canary.Scan(r.Header.Get(whereHeader), r.Header.Get(towardHeader), b); err != nil {
		http.Error(w, sessionproxy.CodeExposed, http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// serveRedact redacts a sidecar's answer on the node.
func (n *Node) serveRedact(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", http.StatusMethodNotAllowed)
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, canaryBodyMax+1))
	if err != nil || len(b) > canaryBodyMax {
		http.Error(w, "not a redactable answer", http.StatusRequestEntityTooLarge)
		return
	}
	s := string(b)
	if n.Canary != nil {
		s = n.Canary.Redact(s)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = io.WriteString(w, s)
}
