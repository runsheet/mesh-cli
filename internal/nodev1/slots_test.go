package nodev1

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// slotSession stands in for claude on a kind that names its tools: it
// lists every server's tools, then calls each of calls (server, tool)
// through the lease's sessionproxy, keeping the answers.
type slotSession struct {
	calls [][2]string

	mu      sync.Mutex
	listed  map[string]map[string]string // server → name → description
	answers []string
}

func (s *slotSession) Run(ctx context.Context, spec SessionSpec) (SessionResult, error) {
	s.mu.Lock()
	s.listed = map[string]map[string]string{}
	s.mu.Unlock()
	for name, entry := range spec.Servers {
		raw, _, err := rpcThroughProxy(ctx, entry, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		if err != nil {
			return SessionResult{}, err
		}
		var list struct {
			Result struct {
				Tools []struct {
					Name        string `json:"name"`
					Description string `json:"description"`
				} `json:"tools"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(raw), &list); err != nil {
			return SessionResult{}, fmt.Errorf("%s's list %s: %w", name, raw, err)
		}
		got := map[string]string{}
		for _, t := range list.Result.Tools {
			got[t.Name] = t.Description
		}
		s.mu.Lock()
		s.listed[name] = got
		s.mu.Unlock()
	}
	for _, c := range s.calls {
		text, _, err := rpcThroughProxy(ctx, spec.Servers[c[0]], fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":%q,"arguments":{}}}`, c[1]))
		if err != nil {
			return SessionResult{}, err
		}
		s.mu.Lock()
		s.answers = append(s.answers, text)
		s.mu.Unlock()
	}
	return SessionResult{SessionID: "sess-slots", Text: "done"}, nil
}

// conformTools are the conform kind's tools as api main's take fixture
// publishes them, its node tools from mesh.
func conformTools() *meshapi.BriefTools {
	return &meshapi.BriefTools{
		Required: []meshapi.SlotTool{
			{Name: "brief", From: meshapi.ToolFromProduct, Description: "Call first, once."},
			{Name: "submit", From: meshapi.ToolFromProduct, Description: "Call last, to finish."},
			{Name: "result", From: meshapi.ToolFromMesh, Description: "Finish with the job's result."},
			{Name: "progress", From: meshapi.ToolFromMesh, Description: "Say what you are doing."},
		},
		Optional: []meshapi.SlotTool{
			{Name: "web_search", From: meshapi.ToolFromConnection, Description: "Search the web."},
		},
	}
}

// A kind that names its tools starts its session with exactly them: the
// product's server lists only its named tools, under the kind's
// descriptions, and refuses a call of any other before it reaches the
// product; the node's server serves only the node tools it names, though
// node_tools names more; an unfilled optional slot is not served; and the
// usage names every tool served and the calls made, by name alone.
func TestATaggedKindsSessionIsServedExactlyItsTools(t *testing.T) {
	sess := &slotSession{calls: [][2]string{{"conform", "admin_delete"}, {"conform", "brief"}}}
	g := newRig(t, sess)
	job := g.offer(t, func(b *meshapi.Brief) {
		b.Tools = conformTools()
		b.NodeTools = []string{"result", "hand_back", "progress"}
	})
	run(t, g, context.Background())

	want := map[string]map[string]string{
		"conform":       {"brief": "Call first, once.", "submit": "Call last, to finish."},
		NodeToolsServer: {"result": "Finish with the job's result.", "progress": "Say what you are doing."},
	}
	if !reflect.DeepEqual(sess.listed, want) {
		t.Fatalf("the session was served %v, want %v", sess.listed, want)
	}
	if len(sess.answers) != 2 || !strings.Contains(sess.answers[0], "not served to this session") || sess.answers[1] != "ok" {
		t.Fatalf("answers %q", sess.answers)
	}
	calls, _ := g.product.snapshot()
	for _, c := range calls {
		if strings.Contains(c, "admin_delete") {
			t.Fatalf("a hidden tool reached the product: %v", calls)
		}
	}

	usages := g.fake.Calls("usage")
	if len(usages) == 0 {
		t.Fatalf("no usage for job %s", job)
	}
	var u meshapi.UsageRequest
	json.Unmarshal(usages[len(usages)-1].Body, &u)
	var served []string
	for _, tc := range u.Tools {
		served = append(served, tc.Tool)
	}
	sort.Strings(served)
	wantServed := []string{"mcp__conform__brief", "mcp__conform__submit", "mcp__mesh-node__progress", "mcp__mesh-node__result"}
	if !reflect.DeepEqual(served, wantServed) {
		t.Fatalf("usage tools %v, want %v (%s)", served, wantServed, usages[len(usages)-1].Body)
	}
}

// A kind with no tools is served as before: every tool its product has
// and every node tool it names, under their own descriptions.
func TestAKindWithoutToolsIsServedAsBefore(t *testing.T) {
	sess := &slotSession{}
	g := newRig(t, sess)
	g.offer(t, func(b *meshapi.Brief) { b.NodeTools = []string{"result", "hand_back", "progress"} })
	run(t, g, context.Background())
	if got := sess.listed["conform"]; len(got) != 4 || got["admin_delete"] != "the product's own admin_delete" {
		t.Fatalf("the product's tools %v", got)
	}
	if got := sess.listed[NodeToolsServer]; len(got) != 3 {
		t.Fatalf("the node's tools %v", got)
	}
}

// A required tool the node cannot fill — a connection slot — hands the
// lease back tool_unavailable before any session starts.
func TestARequiredConnectionSlotHandsBackToolUnavailable(t *testing.T) {
	sess := &slotSession{}
	g := newRig(t, sess)
	job := g.offer(t, func(b *meshapi.Brief) {
		b.Tools = conformTools()
		b.Tools.Required = append(b.Tools.Required, meshapi.SlotTool{Name: "web_search", From: meshapi.ToolFromConnection, Description: "Search."})
	})
	run(t, g, context.Background())
	if sess.listed != nil {
		t.Fatal("a session started without its required slot")
	}
	lease := g.fake.LeaseOf(job)
	if g.fake.Ended(lease) != "hand-back" {
		t.Fatalf("the lease ended %q", g.fake.Ended(lease))
	}
	var hb meshapi.HandBackRequest
	json.Unmarshal(g.fake.Calls("hand-back")[0].Body, &hb)
	if hb.Cause != meshapi.CauseToolUnavailable || !strings.Contains(hb.Reason, "web_search") {
		t.Fatalf("hand-back %+v", hb)
	}
}
