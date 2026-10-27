package meshmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// hostedWorld is a joinWorld whose Join speaks to the fake's /mcp, the
// fallback line captured.
func hostedWorld(t *testing.T, served bool) (*joinWorld, *bytes.Buffer) {
	t.Helper()
	w := newJoinWorld(t)
	w.fake.HostedWorkTools = served
	stderr := &bytes.Buffer{}
	w.persona.Join.Hosted = &Hosted{BaseURL: w.fake.URL(), Source: gateway.StaticToken("rs_test"), Stderr: stderr}
	return w, stderr
}

// With the platform serving the hosted work tools, join → finish goes
// claims with claimWorkItem (mesh_claim is no MCP tool, runsheet ADR 069)
// and finishes through hosted mesh_finish: no finishWorkItem mutation,
// nothing said on stderr.
func TestHostedJoinFinish(t *testing.T) {
	w, stderr := hostedWorld(t, true)
	a := decodeJoin(t, w.join(t, map[string]any{"goal": "ship-it"}))
	head := commit(t, a.Path, "api/api_test.go", "package api\n")
	out := w.tool(t, "mesh_finish", map[string]any{})
	if want := "pushed mesh/ship-it-1 at " + head + "; finished: the item is evaluating"; out != want {
		t.Fatalf("mesh_finish = %q, want %q", out, want)
	}

	if n := len(w.fake.CallsNamed("claimWorkItem")); n != 1 {
		t.Fatalf("claimWorkItem %d times: the claim is the mutation (mesh_claim is no MCP tool)", n)
	}
	finishes := w.fake.CallsNamed("mesh_finish")
	if len(finishes) != 1 {
		t.Fatalf("mesh_finish %d times", len(finishes))
	}
	if in := finishes[0].Input; in["item_id"] != jItemID || in["branch"] != "mesh/ship-it-1" || in["head_sha"] != head {
		t.Fatalf("mesh_finish %v", in)
	}
	if n := len(w.fake.CallsNamed("finishWorkItem")); n != 0 {
		t.Fatalf("finishWorkItem sent %d times on the hosted path", n)
	}
	if got := w.fake.Item(jItemID)["status"]; got != "evaluating" {
		t.Fatalf("item is %v", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr %q", stderr)
	}
}

// With the platform serving none, the mutations run and the fallback
// line is said once across two joins.
func TestHostedFallbackSaidOnce(t *testing.T) {
	w, stderr := hostedWorld(t, false)
	w.secondItem()
	w.join(t, map[string]any{"goal": "ship-it", "item": float64(1)})
	w.join(t, map[string]any{"goal": "ship-it", "item": float64(2)})

	if n := len(w.fake.CallsNamed("claimWorkItem")); n != 2 {
		t.Fatalf("claimWorkItem %d times", n)
	}
	if n := len(w.fake.CallsNamed("mesh_claim")); n != 0 {
		t.Fatalf("mesh_claim %d times", n)
	}
	if n := strings.Count(stderr.String(), HostedFallbackLine); n != 1 {
		t.Fatalf("fallback line %d times in %q", n, stderr)
	}
	if got := stderr.String(); got != "mesh mcp: the platform serves no hosted work tools; using claimWorkItem\n" {
		t.Fatalf("stderr %q", got)
	}
	out := w.tool(t, "mesh_release", map[string]any{"item": float64(1), "reason": "blocked"})
	if out != "released ship-it/1: blocked" || len(w.fake.CallsNamed("releaseWorkItem")) != 1 {
		t.Fatalf("mesh_release = %q, %v", out, w.fake.CallsNamed("releaseWorkItem"))
	}
	if n := strings.Count(stderr.String(), HostedFallbackLine); n != 1 {
		t.Fatalf("fallback line %d times after a release", n)
	}
}

// mesh_ask reaches the hosted tool with the question when served, and
// is the `question: ` progress note when not.
func TestHostedAsk(t *testing.T) {
	const q = "Should the test live in api/?"
	t.Run("served", func(t *testing.T) {
		w, _ := hostedWorld(t, true)
		w.join(t, map[string]any{"goal": "ship-it"})
		out := w.tool(t, "mesh_ask", map[string]any{"question": q})
		if out != "asked; the answer comes back on the item" {
			t.Fatalf("mesh_ask = %q", out)
		}
		asks := w.fake.CallsNamed("mesh_ask")
		if len(asks) != 1 || asks[0].Input["question"] != q || asks[0].Input["item_id"] != jItemID {
			t.Fatalf("mesh_ask calls %v", asks)
		}
		if n := len(w.fake.CallsNamed("reportWorkItemProgress")); n != 0 {
			t.Fatalf("reportWorkItemProgress %d times", n)
		}
	})
	t.Run("not served", func(t *testing.T) {
		w, _ := hostedWorld(t, false)
		w.join(t, map[string]any{"goal": "ship-it"})
		w.tool(t, "mesh_ask", map[string]any{"question": q})
		notes := w.fake.CallsNamed("reportWorkItemProgress")
		if len(notes) != 1 || notes[0].Input["note"] != AskPrefix+q {
			t.Fatalf("progress notes %v", notes)
		}
		if n := len(w.fake.CallsNamed("mesh_ask")); n != 0 {
			t.Fatalf("mesh_ask %d times", n)
		}
	})
}

// A refused claim is the join's answer, the platform's text relayed (the
// claim is claimWorkItem even on the hosted path).
func TestHostedRefusalIsTheAnswer(t *testing.T) {
	w, _ := hostedWorld(t, true)
	w.fake.Refuse["claimWorkItem"] = "the goal is paused"
	out := w.join(t, map[string]any{"goal": "ship-it"})
	if !strings.Contains(out, "claim refused: the goal is paused") {
		t.Fatalf("mesh_join = %q", out)
	}
}

type contractEntry struct {
	Name         string         `json:"name"`
	Arguments    map[string]any `json:"arguments"`
	Answer       string         `json:"answer"`
	Alternatives []struct {
		Outcome   string         `json:"outcome"`
		Arguments map[string]any `json:"arguments"`
	} `json:"alternatives"`
}

func keysOf(m map[string]any) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

// specs/contracts/hostedworktools.json names the five tools, and the
// arguments the client sends each carry exactly its keys.
func TestHostedContractFixture(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "specs", "contracts", "hostedworktools.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Tools []contractEntry `json:"tools"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("hostedworktools.json: %v", err)
	}
	want := map[string]contractEntry{}
	for _, e := range doc.Tools {
		want[e.Name] = e
	}
	for _, n := range HostedWorkTools {
		if e, ok := want[n]; !ok || len(e.Arguments) == 0 || e.Answer == "" {
			t.Fatalf("the fixture has no %s with arguments and an answer", n)
		}
	}
	if len(want) != len(HostedWorkTools) {
		t.Fatalf("the fixture names %d tools, want %d", len(want), len(HostedWorkTools))
	}

	// every tool, through the client, against the fake's /mcp
	w, _ := hostedWorld(t, true)
	w.secondItem()
	w.join(t, map[string]any{"goal": "ship-it", "item": float64(1)})
	w.tool(t, "mesh_progress", map[string]any{"note": "the test is written"})
	w.tool(t, "mesh_ask", map[string]any{"question": "api/?"})
	w.tool(t, "mesh_release", map[string]any{"reason": "blocked"})
	a := decodeJoin(t, w.join(t, map[string]any{"goal": "ship-it", "item": float64(1)}))
	commit(t, a.Path, "api/api_test.go", "package api\n")
	w.tool(t, "mesh_finish", map[string]any{})
	w.join(t, map[string]any{"goal": "ship-it", "item": float64(2)})
	w.tool(t, "mesh_finish", map[string]any{"outcome": "already_done", "evidence": "PR 12"})

	for _, n := range HostedWorkTools {
		calls := w.fake.CallsNamed(n)
		if len(calls) == 0 {
			t.Fatalf("%s was never sent", n)
		}
		e := want[n]
		for _, c := range calls {
			got, ok := keysOf(c.Input), false
			if got == keysOf(e.Arguments) {
				ok = true
			}
			for _, alt := range e.Alternatives {
				ok = ok || got == keysOf(alt.Arguments)
			}
			if !ok {
				t.Fatalf("%s sent keys %s; the fixture's are %s", n, got, keysOf(e.Arguments))
			}
		}
	}
	// the already_done alternative was the one sent second
	if fin := w.fake.CallsNamed("mesh_finish"); len(fin) != 2 || keysOf(fin[1].Input) != keysOf(want["mesh_finish"].Alternatives[0].Arguments) {
		t.Fatalf("mesh_finish calls %v", fin)
	}
}

// The tools/list answer is kept an hour, then asked again.
func TestHostedReprobedHourly(t *testing.T) {
	w, _ := hostedWorld(t, false)
	now := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	h := w.persona.Join.Hosted
	h.Now = func() time.Time { return now }
	ctx := context.Background()
	if h.Serves(ctx) {
		t.Fatal("served before the platform serves them")
	}
	w.fake.HostedWorkTools = true
	now = now.Add(59 * time.Minute)
	if h.Serves(ctx) {
		t.Fatal("re-probed inside the hour")
	}
	now = now.Add(time.Minute)
	if !h.Serves(ctx) {
		t.Fatal("not re-probed after the hour")
	}
}
