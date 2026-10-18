package node

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

// recordValidateYML is validateYML whose api step prints twenty-five
// numbered lines and exits 3 while api/broken exists.
const recordValidateYML = `suites:
  - name: api
    kind: unit
    command: if test -e api/broken; then seq 1 25; exit 3; fi
    paths: ["api/**"]
`

// recordHarness is commitHarness that carries calls[n] on its n-th run's
// stream, each a mesh-work call as the harness passes it on (Progress's
// WorkCall), and hands back the runs handBack names. env is the session's
// environment as it saw it: the node's own, then the job's.
type recordHarness struct {
	*commitHarness
	calls    [][]harness.WorkCall
	handBack map[int]string
	env      []string
	runs     int
}

func (h *recordHarness) Run(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
	n := h.runs
	h.runs++
	if n == 0 {
		h.env = append(os.Environ(), job.Env...)
	}
	if n < len(h.calls) {
		for _, c := range h.calls[n] {
			c := c
			on(harness.Progress{WorkCall: &c})
		}
	}
	res, err := h.commitHarness.Run(ctx, job, on)
	if spent, ok := h.handBack[n]; ok {
		res.HandedBack, res.SpentOn, res.Text = true, spent, "Stopped at the mark; committed the parser."
	}
	return res, err
}

func workCall(tool, field, text string) harness.WorkCall {
	in, _ := json.Marshal(map[string]string{"item_id": item1, field: text})
	return harness.WorkCall{Tool: tool, Input: in}
}

// recordRunner is a runner on the fake whose checkout's origin carries
// validate (nil: none), its resume store a temp dir, its log in logs.
func recordRunner(t *testing.T, h *recordHarness, validate map[string]string) (*Runner, gateway.WorkItem, *ResumeStore, *syncBuf, *gatewaytest.Fake) {
	t.Helper()
	f := seed(t)
	r, ws, item, _ := runnerOnFake(t, f, h, nil)
	item.Acceptance = "go test ./api/... passes"
	r.WS = &gitWS{fakeWS: ws, t: t, origin: originRepo(t, validate)}
	r.Resumes = &ResumeStore{Dir: t.TempDir()}
	logs := &syncBuf{}
	r.Log = log.New(logs, "", 0)
	return r, item, r.Resumes, logs, f
}

// readRecord is every entry of the item's record.jsonl, and the file.
func readRecord(t *testing.T, store *ResumeStore, itemID string) ([]RecordEntry, string) {
	t.Helper()
	data, err := os.ReadFile(store.recordJSONLPath(itemID))
	if err != nil {
		t.Fatalf("record.jsonl: %v", err)
	}
	var entries []RecordEntry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e RecordEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("record line %q: %v", line, err)
		}
		entries = append(entries, e)
	}
	return entries, string(data)
}

func entriesOf(entries []RecordEntry, kind string) []RecordEntry {
	var out []RecordEntry
	for _, e := range entries {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// A build whose stream carries three mesh_progress notes and a mesh_ask,
// and whose validation fails once, leaves record.jsonl with the notes in
// order and the question exactly as the calls said them, the failed
// step's check with its exit code and last twenty lines, and a start and
// an end turn entry for the build and for its retry turn; a note quoting
// an env value keeps it, and nothing else of the session's environment —
// MESH_CONTEXT_TOKEN by name or value — is in the file.
func TestItemRecordFromABuildStream(t *testing.T) {
	t.Setenv("MESH_CONTEXT_TOKEN", "tok-context-5f3a9c")
	t.Setenv("MESH_RECORD_CACHE", "/srv/cache-7781")
	notes := []string{
		"the failing test is written",
		"cache at /srv/cache-7781 holds é calls\nsecond line",
		"state: done the parser; next the node; open none",
	}
	question := "which fixture?"
	h := &recordHarness{
		commitHarness: &commitHarness{t: t, turns: []map[string]string{{"api/broken": "x"}, {"api/broken": ""}}},
		calls: [][]harness.WorkCall{{
			workCall("mesh_progress", "note", notes[0]),
			workCall("mesh_progress", "note", notes[1]),
			workCall("mesh_ask", "question", question),
			workCall("mesh_finish", "branch", "ignored"),
			workCall("mesh_progress", "note", notes[2]),
		}},
	}
	r, item, store, logs, _ := recordRunner(t, h, map[string]string{".runsheet/validate.yml": recordValidateYML})
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if h.runs != 2 {
		t.Fatalf("sessions = %d, want the build and its retry turn:\n%s", h.runs, logs.String())
	}
	var hasToken bool
	for _, kv := range h.env {
		hasToken = hasToken || kv == "MESH_CONTEXT_TOKEN=tok-context-5f3a9c"
	}
	if !hasToken {
		t.Fatal("the session's env lacks MESH_CONTEXT_TOKEN")
	}
	entries, file := readRecord(t, store, item.ID)

	gotNotes := entriesOf(entries, recordNote)
	if len(gotNotes) != len(notes) {
		t.Fatalf("notes = %v, want %d", gotNotes, len(notes))
	}
	for i, n := range gotNotes {
		if n.Text != notes[i] {
			t.Fatalf("note %d = %q, want the call's %q", i, n.Text, notes[i])
		}
	}
	qs := entriesOf(entries, recordQuestion)
	if len(qs) != 1 || qs[0].Text != question {
		t.Fatalf("questions = %v, want %q", qs, question)
	}
	checks := entriesOf(entries, recordCheck)
	if len(checks) != 1 {
		t.Fatalf("checks = %v, want the one failed step", checks)
	}
	var last []string
	for i := 6; i <= 25; i++ {
		last = append(last, itoa(i))
	}
	want := "step api (if test -e api/broken; then seq 1 25; exit 3; fi) exited 3\n" + strings.Join(last, "\n")
	if checks[0].Text != want {
		t.Fatalf("check = %q, want %q", checks[0].Text, want)
	}
	turns := entriesOf(entries, recordTurn)
	wantTurns := []string{
		"start: claude-code, fresh session",
		"end: claude-code session s-1, fresh, ended",
		"start: claude-code, resumed session s-1",
		"end: claude-code session s-1, resumed, ended",
	}
	if len(turns) != len(wantTurns) {
		t.Fatalf("turns = %v, want %v", turns, wantTurns)
	}
	for i, tu := range turns {
		if tu.Text != wantTurns[i] {
			t.Fatalf("turn %d = %q, want %q", i, tu.Text, wantTurns[i])
		}
	}
	// in the order it happened: the build's turn, its calls, its end,
	// the check, the retry turn
	var kinds []string
	for _, e := range entries {
		kinds = append(kinds, e.Kind)
	}
	if got := strings.Join(kinds, " "); got != "turn note note question note turn check turn turn" {
		t.Fatalf("kinds = %s", got)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].At.Before(entries[i-1].At) {
			t.Fatalf("entry %d is older than the one before it", i)
		}
	}
	if n := strings.Count(file, "MESH_CONTEXT_TOKEN"); n != 0 {
		t.Fatalf("grep -c MESH_CONTEXT_TOKEN record.jsonl = %d, want 0", n)
	}
	for _, leak := range []string{"tok-context-5f3a9c", "MESH_RECORD_CACHE"} {
		if strings.Contains(file, leak) {
			t.Fatalf("record.jsonl carries %q from the environment:\n%s", leak, file)
		}
	}
	// kept beside the resume record, which Keep swapped in after the build
	if _, ok := store.Lookup(item.ID); !ok {
		t.Fatal("no resume record kept beside record.jsonl")
	}
}

// itoa is strconv.Itoa for the small numbers seq prints.
func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

// A turn that ends on hand-back, or after a question, with no
// mesh_progress note opening state: appends a turn entry saying so — a
// hand-back beside its hand-back entry; one that left the note does
// not, and none is refused: the item finishes.
func TestTurnEntryWhenNoStateNote(t *testing.T) {
	const onHandBack = "the turn ended on hand-back with no mesh_progress note opening state:"
	const onQuestion = "the turn ended on a question with no mesh_progress note opening state:"
	for _, tc := range []struct {
		name     string
		calls    []harness.WorkCall
		handBack bool
		want     string
	}{
		{"hand-back with no state note", []harness.WorkCall{workCall("mesh_progress", "note", "parser half done")}, true, onHandBack},
		{"hand-back with a state note", []harness.WorkCall{workCall("mesh_progress", "note", "State: done the parser; next the node; open none")}, true, ""},
		{"question with no state note", []harness.WorkCall{workCall("mesh_ask", "question", "which fixture?")}, false, onQuestion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &recordHarness{
				commitHarness: &commitHarness{t: t, turns: []map[string]string{{"api/api.go": "package api\n\nfunc A() {}\n"}}},
				calls:         [][]harness.WorkCall{tc.calls},
			}
			if tc.handBack {
				h.handBack = map[int]string{0: "reading the parser's tests"}
			}
			r, item, store, logs, f := recordRunner(t, h, nil)
			if err := r.RunItem(context.Background(), item); err != nil {
				t.Fatal(err)
			}
			entries, _ := readRecord(t, store, item.ID)
			hb := entriesOf(entries, recordHandback)
			if tc.handBack && (len(hb) != 1 || hb[0].Text != "handed back: reading the parser's tests\nStopped at the mark; committed the parser.") {
				t.Fatalf("handback entries = %v", hb)
			}
			if !tc.handBack && len(hb) != 0 {
				t.Fatalf("handback entries = %v, want none", hb)
			}
			var said []string
			for _, e := range entriesOf(entries, recordTurn) {
				if e.Text == onHandBack || e.Text == onQuestion {
					said = append(said, e.Text)
				}
			}
			if tc.want == "" && len(said) != 0 || tc.want != "" && (len(said) != 1 || said[0] != tc.want) {
				t.Fatalf("no-state turn entries = %q, want %q: %v", said, tc.want, entries)
			}
			if len(f.CallsNamed("finishWorkItem")) != 1 {
				t.Fatalf("the item was not finished:\n%s", logs.String())
			}
		})
	}
}
