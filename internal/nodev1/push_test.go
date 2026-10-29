package nodev1

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// callTool calls one tool with args on the entry's server through the
// lease's sessionproxy socket, as the session's mcp-relay would, and
// answers the tool's text and whether it was a refusal.
func callTool(ctx context.Context, entry any, tool string, args map[string]any) (string, bool, error) {
	sock, name, ok := harness.ProxiedOf(entry)
	if !ok {
		return "", false, fmt.Errorf("not a proxied entry: %v", entry)
	}
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://proxy/mcp/"+name, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := c.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	var ans struct {
		Result *struct {
			Content []struct{ Text string } `json:"content"`
			IsError bool                    `json:"isError"`
		} `json:"result"`
		Error *struct{ Message string } `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ans); err != nil {
		return "", false, err
	}
	if ans.Error != nil {
		return "", false, fmt.Errorf("%s", ans.Error.Message)
	}
	var text strings.Builder
	for _, c := range ans.Result.Content {
		text.WriteString(c.Text)
	}
	return text.String(), ans.Result.IsError, nil
}

// sessionGit runs git in the session's checkout as the session would,
// with no credential of the node's.
func sessionGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=s", "GIT_AUTHOR_EMAIL=s@x", "GIT_COMMITTER_NAME=s", "GIT_COMMITTER_EMAIL=s@x")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func commitIn(t *testing.T, dir, msg string) string {
	t.Helper()
	os.WriteFile(filepath.Join(dir, "work.txt"), []byte(msg+"\n"), 0o644)
	if out, err := sessionGit(dir, "add", "-A"); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if out, err := sessionGit(dir, "commit", "-qm", msg); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}
	out, _ := sessionGit(dir, "rev-parse", "HEAD")
	return strings.TrimSpace(out)
}

// writeBrief is a write job whose kind names the push node tool, with
// push rules for its own branches (fast-forward-only by default) and
// create-only pins.
func writeBrief(b *meshapi.Brief) {
	b.Git = &meshapi.Git{Host: "github.com", Repo: "acme/fixture", Access: "write", Push: []meshapi.PushRule{
		{Ref: "refs/heads/mesh/*"},
		{Ref: "refs/pins/*", CreateOnly: true},
	}}
	b.NodeTools = []string{PushTool}
}

// The write path (Mesh v1 PR 1.10): the session pushes its work with the
// push node tool, through the lease's relay on a write token Latchkey
// minted for it. A push to an allowed ref lands; an unlisted ref, a
// force push, a delete and moving a create-only ref are each refused by
// the relay — the session hearing why — and every push, allowed or
// refused, is one of the lease's AccessRecords at Mesh.
func TestPushGoesThroughTheRelayUnderThePushRules(t *testing.T) {
	type step struct {
		name    string
		do      func(ctx context.Context, spec SessionSpec) (string, bool, error)
		refused string
	}
	var head, pinned string
	var results []string
	push := func(args map[string]any) func(context.Context, SessionSpec) (string, bool, error) {
		return func(ctx context.Context, spec SessionSpec) (string, bool, error) {
			return callTool(ctx, spec.Servers[NodeToolsServer], PushTool, args)
		}
	}
	raw := func(args ...string) func(context.Context, SessionSpec) (string, bool, error) {
		return func(_ context.Context, spec SessionSpec) (string, bool, error) {
			out, err := sessionGit(spec.Workspace, args...)
			return out, err != nil, nil
		}
	}
	steps := []step{
		{name: "an allowed ref", do: push(map[string]any{"ref": "mesh/j-1"})},
		{name: "a create-only ref", do: push(map[string]any{"ref": "refs/pins/p1"})},
		{name: "an unlisted ref", do: push(map[string]any{"ref": "main"}), refused: "may not push refs/heads/main"},
		{name: "moving a create-only ref", do: func(ctx context.Context, spec SessionSpec) (string, bool, error) {
			commitIn(t, spec.Workspace, "three")
			return callTool(ctx, spec.Servers[NodeToolsServer], PushTool, map[string]any{"ref": "refs/pins/p1"})
		}, refused: "created, never moved"},
		{name: "a force push", do: func(ctx context.Context, spec SessionSpec) (string, bool, error) {
			sessionGit(spec.Workspace, "reset", "-q", "--hard", "HEAD~2")
			commitIn(t, spec.Workspace, "rewritten")
			return raw("push", "--force", "origin", "HEAD:refs/heads/mesh/j-1")(ctx, spec)
		}, refused: "may only fast-forward"},
		{name: "a non-fast-forward by the tool", do: push(map[string]any{"ref": "mesh/j-1"}), refused: "may only fast-forward"},
		{name: "a delete", do: raw("push", "origin", ":refs/heads/mesh/j-1"), refused: "may not delete"},
	}
	sess := &stubSession{calls: []string{"conform/submit"}, result: SessionResult{SessionID: "sess-push", Text: "pushed"}}
	sess.inspect = func(spec SessionSpec) {
		ctx := context.Background()
		if _, ok := spec.Servers[NodeToolsServer]; !ok {
			t.Errorf("the session has no %s server: %v", NodeToolsServer, spec.Servers)
			return
		}
		sessionGit(spec.Workspace, "checkout", "-q", "-b", "work")
		head = commitIn(t, spec.Workspace, "two")
		pinned = head
		for _, s := range steps {
			text, refused, err := s.do(ctx, spec)
			if err != nil {
				t.Errorf("%s: %v", s.name, err)
				continue
			}
			results = append(results, s.name+": "+text)
			switch {
			case s.refused == "" && refused:
				t.Errorf("%s was refused: %s", s.name, text)
			case s.refused != "" && !refused:
				t.Errorf("%s went through: %s", s.name, text)
			}
		}
	}
	g := newRig(t, sess)
	job := g.offer(t, writeBrief)
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	lease := g.fake.LeaseOf(job)
	if g.fake.Ended(lease) != "finish" {
		t.Fatalf("lease %s ended %q", lease, g.fake.Ended(lease))
	}
	t.Logf("steps:\n%s", strings.Join(results, "\n"))

	// what landed, and what did not move
	if got := g.github.ref("refs/heads/mesh/j-1"); got != head {
		t.Errorf("mesh/j-1 is %q, want %s", got, head)
	}
	if got := g.github.ref("refs/pins/p1"); got != pinned {
		t.Errorf("the pin is %q, want %s", got, pinned)
	}
	if got := g.github.ref("refs/heads/main"); got != g.github.head {
		t.Errorf("main moved to %s", got)
	}
	// the tool says why the relay refused
	for _, want := range []string{"main: the relay refused", "moving a create-only ref: the relay refused the push to refs/pins/p1: refs/pins/p1 may be created, never moved"} {
		if !slices.ContainsFunc(results, func(r string) bool { return strings.Contains(r, strings.SplitN(want, ": ", 2)[1]) }) {
			t.Errorf("no tool answer says %q:\n%s", want, strings.Join(results, "\n"))
		}
	}

	// a write job's relay minted write tokens, and only those
	if acc := g.latchkey.accesses(); len(acc) == 0 || slices.ContainsFunc(acc, func(a string) bool { return a != "write" }) {
		t.Errorf("Latchkey minted %v", acc)
	}

	// every push the relay answered is a signed ToolRecord at Mesh —
	// node-signed here, the rig having no Fulcio — and the finish names
	// every one of them
	var evidence []meshapi.EvidencePost
	for _, c := range g.fake.Calls("evidence") {
		var req meshapi.EvidenceRequest
		json.Unmarshal(c.Body, &req)
		evidence = append(evidence, req.Records...)
	}
	if len(evidence) != 5 {
		t.Errorf("%d push records posted, want the five the tool ran", len(evidence))
	}
	for _, p := range evidence {
		if p.Signing != meshapi.SigningNode || p.UnverifiedReason == "" {
			t.Errorf("a push record is %q (%q), want node-signed with a reason", p.Signing, p.UnverifiedReason)
		}
	}
	var fin meshapi.FinishRequest
	if calls := g.fake.Calls("finish"); len(calls) == 1 {
		json.Unmarshal(calls[0].Body, &fin)
	}
	if len(fin.Evidence) != len(evidence) || !slices.ContainsFunc(results, func(r string) bool { return strings.Contains(r, `"evidence_id":"`+fin.Evidence[0]+`"`) }) {
		t.Errorf("the finish names evidence %v; the tool answered:\n%s", fin.Evidence, strings.Join(results, "\n"))
	}

	// every push is an AccessRecord at Mesh, the refusals with their reason
	var posted meshapi.AccessRequest
	for _, c := range g.fake.Calls("access") {
		var req meshapi.AccessRequest
		json.Unmarshal(c.Body, &req)
		posted.Records = append(posted.Records, req.Records...)
	}
	pushes := map[string][]meshapi.AccessRecord{}
	for _, rec := range posted.Records {
		if rec.Service == "receive-pack" && len(rec.Refs) > 0 {
			pushes[rec.Outcome] = append(pushes[rec.Outcome], rec)
		}
	}
	for _, want := range []struct{ outcome, ref, reason string }{
		{"ok", "refs/heads/mesh/j-1", ""},
		{"ok", "refs/pins/p1", ""},
		{"refused", "refs/heads/main", "may not push"},
		{"refused", "refs/pins/p1", "created, never moved"},
		{"refused", "refs/heads/mesh/j-1", "may only fast-forward"},
		{"refused", "refs/heads/mesh/j-1", "may not delete"},
	} {
		if !slices.ContainsFunc(pushes[want.outcome], func(r meshapi.AccessRecord) bool {
			return slices.Contains(r.Refs, want.ref) && strings.Contains(r.Reason, want.reason) && r.Vendor == "github" && r.Repo == "acme/fixture"
		}) {
			t.Errorf("no %s AccessRecord for %s (%q): %+v", want.outcome, want.ref, want.reason, posted.Records)
		}
	}
}

// A read job's relay mints read tokens alone: its push tool is refused
// by the relay, and the refusal is recorded.
func TestAReadJobMintsReadAndCannotPush(t *testing.T) {
	var text string
	var refused bool
	sess := &stubSession{calls: []string{"conform/submit"}, result: SessionResult{SessionID: "sess-read"}}
	sess.inspect = func(spec SessionSpec) {
		commitIn(t, spec.Workspace, "two")
		var err error
		if text, refused, err = callTool(context.Background(), spec.Servers[NodeToolsServer], PushTool, map[string]any{"ref": "mesh/j-1"}); err != nil {
			t.Error(err)
		}
	}
	g := newRig(t, sess)
	g.offer(t, func(b *meshapi.Brief) {
		b.Git = &meshapi.Git{Host: "github.com", Repo: "acme/fixture", Access: "read",
			Push: []meshapi.PushRule{{Ref: "refs/heads/mesh/*"}}} // rules on a read job are never used
		b.NodeTools = []string{PushTool}
	})
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	if !refused || !strings.Contains(text, "may only read") {
		t.Fatalf("a read job's push: refused %v, %q", refused, text)
	}
	if acc := g.latchkey.accesses(); len(acc) == 0 || slices.Contains(acc, "write") {
		t.Fatalf("Latchkey minted %v for a read job", acc)
	}
	if g.github.ref("refs/heads/mesh/j-1") != "" {
		t.Fatal("a read job's push landed")
	}
}

// The push tool writes only through the relay, whatever the session did
// to its checkout: an origin pointed at the vendor itself is ignored (the
// tool names the relay's URL), a URL rewrite in the checkout's config is
// refused, and a hook the session planted never runs as the node.
func TestPushToolIgnoresWhatTheSessionDidToItsCheckout(t *testing.T) {
	var answers []string
	var hookRan string
	sess := &stubSession{calls: []string{"conform/submit"}, result: SessionResult{SessionID: "sess-guard"}}
	var vendorURL string
	sess.inspect = func(spec SessionSpec) {
		ctx := context.Background()
		commitIn(t, spec.Workspace, "two")
		sessionGit(spec.Workspace, "remote", "set-url", "origin", vendorURL+"/acme/fixture.git")
		hookRan = filepath.Join(spec.Workspace, "hook-ran")
		hook := filepath.Join(spec.Workspace, ".git", "hooks", "pre-push")
		os.MkdirAll(filepath.Dir(hook), 0o755)
		os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+hookRan+"\n"), 0o755)
		text, refused, err := callTool(ctx, spec.Servers[NodeToolsServer], PushTool, map[string]any{"ref": "mesh/j-1"})
		answers = append(answers, fmt.Sprintf("origin at the vendor: refused=%v %s %v", refused, text, err))
		if refused || err != nil {
			t.Errorf("the push through the relay: %s %v", text, err)
		}
		sessionGit(spec.Workspace, "config", "url."+vendorURL+"/.insteadOf", "http://127.0.0.1")
		text, refused, err = callTool(ctx, spec.Servers[NodeToolsServer], PushTool, map[string]any{"ref": "mesh/j-2"})
		answers = append(answers, fmt.Sprintf("a URL rewrite: refused=%v %s %v", refused, text, err))
		if !refused || !strings.Contains(text, "rewrites URLs") {
			t.Errorf("a checkout with a URL rewrite pushed: %s %v", text, err)
		}
	}
	g := newRig(t, sess)
	vendorURL = g.github.srv.URL
	g.offer(t, writeBrief)
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s", strings.Join(answers, "\n"))
	if g.github.ref("refs/heads/mesh/j-1") == "" || g.github.ref("refs/heads/mesh/j-2") != "" {
		t.Fatalf("j-1 %q, j-2 %q", g.github.ref("refs/heads/mesh/j-1"), g.github.ref("refs/heads/mesh/j-2"))
	}
	if _, err := os.Stat(hookRan); err == nil {
		t.Fatal("the session's pre-push hook ran as the node")
	}
	// every request the vendor saw carried a token Latchkey minted
	minted, _, _ := g.latchkey.snapshot()
	for _, a := range g.github.auths() {
		if !slices.ContainsFunc(minted, func(m string) bool { return a == basicAuth(m) }) {
			t.Fatalf("the vendor saw %q, not a minted token", a)
		}
	}
}
