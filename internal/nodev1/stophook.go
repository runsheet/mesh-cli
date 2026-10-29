package nodev1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/nodetools"
)

// A session cannot end without its finish (projectmesh-io/api ADR 001
// amendment, finish enforcement; mesh-specs gap 1). The node puts a
// Claude Code Stop hook in each one-shot session's --settings: claude
// runs it every time the session would end its turn, and while the
// kind's finish tool has not answered, the node's stop gate refuses —
// naming the tool still owed, and asking for learnings by name when the
// kind has a learnings tool the session has not called. After
// StopRefusals refusals the gate lets the session stop, and the node
// checks its last message against the kind's finish_schema (settle.go).
//
// The hook is `mesh session-stop-hook --socket <lease socket dir>/h`: the gate
// listens on a socket of its own in the lease's socket directory, beside
// the sessionproxy's. A hook that cannot reach the gate lets the session
// stop; the node's check after the session still holds the finish, so a
// broken hook never wedges a session and never finishes one with prose.

// StopHookCommand is the mesh subcommand a session's Stop hook runs.
const StopHookCommand = "session-stop-hook"

// stopSocketName is the gate's socket in the lease's socket directory:
// one byte, as the proxy's own (leaseSocketName) is, so it fits wherever
// that does.
const stopSocketName = "h"

// DefaultStopRefusals is how many times the gate refuses a session's
// stop before it lets the session end and the node checks its last
// message.
const DefaultStopRefusals = 3

// stopHookTimeoutS bounds one run of the hook, as claude reads it.
const stopHookTimeoutS = 30

// stopGate is one lease's answer to its session's Stop hook.
type stopGate struct {
	// finishTool and learningsTool are as the session names them
	// (mcp__<server>__<tool>); learningsTool is "" when the kind has none.
	finishTool, learningsTool string
	// max is how many stops it refuses.
	max int
	// finished and learned read the lease's finishCapture.
	finished, learned func() bool

	mu       sync.Mutex
	refusals int
	srv      *http.Server
	path     string
}

// stopAnswer is a Stop hook's answer, as claude reads it: a block keeps
// the turn going with reason its next input; no decision lets it end.
type stopAnswer struct {
	Decision string `json:"decision,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// decide answers one stop: a refusal while the finish is owed and
// refusals remain, else leave to stop.
func (g *stopGate) decide() stopAnswer {
	if g.finished() {
		return stopAnswer{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.refusals >= g.max {
		return stopAnswer{}
	}
	g.refusals++
	return stopAnswer{Decision: "block", Reason: g.reason(g.refusals)}
}

// Refused is how many stops the gate refused.
func (g *stopGate) Refused() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.refusals
}

// reason is the n-th refusal's words to the session.
func (g *stopGate) reason(n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "This job is not finished: it ends only when you call its finish tool, %s, with your result. ", g.finishTool)
	if g.learningsTool != "" && !g.learned() {
		fmt.Fprintf(&b, "Before it, call %s with {\"learnings\": [{\"lesson\": \"…\", \"kind\": \"trap|tool|doc|process\", \"area\": \"…\"}]}: "+
			"what you learned doing this job that would help the next one; {\"learnings\": []} when there is nothing. ", g.learningsTool)
	}
	fmt.Fprintf(&b, "Call %s now. (Refusal %d of %d: after the last, the node ends the session and takes your last message as the result "+
		"only when it is the result itself, as JSON that fits the kind's finish_schema; otherwise the job fails no_finish.)", g.finishTool, n, g.max)
	return b.String()
}

// open serves the gate on dir's stop socket.
func (g *stopGate) open(dir string) error {
	g.path = filepath.Join(dir, stopSocketName)
	_ = os.Remove(g.path)
	ln, err := net.Listen("unix", g.path)
	if err != nil {
		return fmt.Errorf("the stop gate's socket: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /stop", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(g.decide())
	})
	g.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = g.srv.Serve(ln) }()
	return nil
}

// close stops serving and removes the socket.
func (g *stopGate) close() {
	if g.srv != nil {
		_ = g.srv.Close()
	}
	if g.path != "" {
		_ = os.Remove(g.path)
	}
}

// settings are the session's --settings hooks: the Stop hook that runs
// bin's StopHookCommand on the gate's socket.
func (g *stopGate) settings(bin string) map[string]any {
	command := shellQuote(bin) + " " + StopHookCommand + " --socket " + shellQuote(g.path)
	return map[string]any{"hooks": map[string]any{"Stop": []any{
		map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": stopHookTimeoutS}}},
	}}}
}

// shellQuote quotes s for sh: claude runs a hook's command in a shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// RunStopHook is the hook's side (mesh session-stop-hook): it reads
// claude's Stop input from in, asks the gate on socket and writes the
// gate's answer to out. Anything that goes wrong lets the session stop,
// with nothing written: the node's check after the session holds the
// finish.
func RunStopHook(ctx context.Context, socket string, in io.Reader, out io.Writer) error {
	body, _ := io.ReadAll(io.LimitReader(in, 64<<10))
	ctx, cancel := context.WithTimeout(ctx, (stopHookTimeoutS-5)*time.Second)
	defer cancel()
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://stop/stop", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the stop gate answered %d", resp.StatusCode)
	}
	var a stopAnswer
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&a); err != nil {
		return err
	}
	if a.Decision != "block" {
		return nil
	}
	if a.Reason == "" {
		return errors.New("the stop gate refused with no reason")
	}
	return json.NewEncoder(out).Encode(a)
}

// openStopGate opens b's stop gate beside the lease's sessionproxy socket
// sock: a one-shot session with a finish tool has one; a thread, which
// ends by park or close, has none. A gate that cannot open is logged and
// left out: the check after the session still holds the finish.
func (r *Runtime) openStopGate(b *meshapi.Brief, sock string, fin *finishCapture, hb *sessionHandBack) *stopGate {
	owed := sessionToolName(b, b.FinishTool)
	if b.FinishTool == "" && slices.Contains(b.NodeTools, nodetools.ResultTool) {
		// a kind that finishes through the node's result tool owes that
		owed = "mcp__" + NodeToolsServer + "__" + nodetools.ResultTool
	}
	if b.Thread != nil || owed == "" || sock == "" {
		return nil
	}
	n := r.StopRefusals
	if n <= 0 {
		n = DefaultStopRefusals
	}
	// a session that gave the job back with hand_back owes no finish
	ended := func() bool { return fin.finished() || hb.asked() != nil }
	g := &stopGate{finishTool: owed, max: n, finished: ended, learned: fin.learnedAlready}
	if b.LearningsTool != "" {
		g.learningsTool = sessionToolName(b, b.LearningsTool)
	}
	if err := g.open(filepath.Dir(sock)); err != nil {
		r.logf("nodev1: lease %s: %v; the session runs with no Stop hook", b.LeaseID, err)
		return nil
	}
	return g
}

// sessionSettings are the session's --settings hooks, nil without a gate.
func (g *stopGate) sessionSettings(bin string) map[string]any {
	if g == nil {
		return nil
	}
	return g.settings(bin)
}

// refused is how many stops the gate refused; 0 without one.
func (g *stopGate) refused() int {
	if g == nil {
		return 0
	}
	return g.Refused()
}

// sessionToolName is tool as the session names it, mcp__<server>__<tool>
// on the brief's product server, when tool is a bare name.
func sessionToolName(b *meshapi.Brief, tool string) string {
	server := productServer(b)
	if server == "" || strings.ContainsAny(tool, ".") || strings.HasPrefix(tool, "mcp__") {
		return tool
	}
	return "mcp__" + server + "__" + tool
}
