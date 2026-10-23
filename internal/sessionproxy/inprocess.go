package sessionproxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
)

// InProcess is a server the node serves itself, in this process: an
// http.Handler speaking streamable-HTTP MCP (mcptool.Handler), served
// at /mcp/<name> like any upstream but reached by a call, never a
// socket or a child. It is how the node's own tools — mesh_result and
// mesh_fail (internal/jobresult; mesh-cli ADR-041) — reach a session on
// the host or in a pod on the one path every other server takes, so the
// node holds what the session hands in and refuses what does not fit
// before anything is sent. It carries no credential and masks nothing
// of its own: what it answers is the node's words.
type InProcess struct {
	Handler http.Handler
}

func (InProcess) endpoint() string { return "in-process" }

func (InProcess) retries401() bool { return false }

func (InProcess) authorize(context.Context, *http.Request, bool) ([]string, error) {
	return nil, nil
}

// serveInProcess hands the request to the handler with its body read
// again, the client's MCP headers as they came.
func (p *Proxy) serveInProcess(w http.ResponseWriter, req *http.Request, name string, ip InProcess, body []byte) {
	if ip.Handler == nil {
		writeError(w, http.StatusBadGateway, -32603, "sessionproxy: "+name+": no handler")
		return
	}
	in := req.Clone(req.Context())
	in.Body = io.NopCloser(bytes.NewReader(body))
	in.ContentLength = int64(len(body))
	ip.Handler.ServeHTTP(w, in)
}
