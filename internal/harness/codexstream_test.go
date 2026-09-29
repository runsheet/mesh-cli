package harness

import (
	"math"
	"os"
	"strings"
	"testing"
)

// parseCodexFixture parses a recorded `codex exec --json` run.
func parseCodexFixture(t *testing.T, name, model string) (Result, []CodexEvent) {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var seen []CodexEvent
	res := ParseCodexStream(f, model, func(ev CodexEvent) { seen = append(seen, ev) })
	return res, seen
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// The API-key run: its final message, thread id, token counts and the
// cost at gpt-6-astra's list price; an API login reports no plan
// limits.
func TestParseCodexStreamAPIKey(t *testing.T) {
	res, seen := parseCodexFixture(t, "codex_exec_apikey.jsonl", "gpt-6-astra")
	if !res.Success || res.EmptyOutput {
		t.Fatalf("result = %+v", res)
	}
	if res.Text != "hello from codex" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.SessionID != "01a0d7ef-290d-70d1-9303-01b1bf7584bc" {
		t.Errorf("SessionID = %q", res.SessionID)
	}
	if res.Turns != 1 {
		t.Errorf("Turns = %d", res.Turns)
	}
	want := Usage{Model: "gpt-6-astra", InputTokens: 12624, CachedInputTokens: 0,
		CacheWriteInputTokens: 12621, OutputTokens: 8, ReasoningOutputTokens: 0}
	if res.Usage == nil || *res.Usage != want {
		t.Errorf("Usage = %+v, want %+v", res.Usage, want)
	}
	// 3 plain input × $10 + 12621 cache writes × $12.50 + 8 output × $50, per 1M.
	if !near(res.CostUSD, 0.1581925) {
		t.Errorf("CostUSD = %v, want 0.1581925", res.CostUSD)
	}
	if res.RateLimit != nil {
		t.Errorf("RateLimit = %+v, want nil on an API-key login", res.RateLimit)
	}
	if len(seen) != 4 || seen[0].Type != "thread.started" || seen[3].Type != "turn.completed" {
		t.Errorf("events = %+v", seen)
	}
}

// A run with commands and cached input: the last agent message is the
// final one, and cached tokens are priced at the cached rate.
func TestParseCodexStreamTools(t *testing.T) {
	res, seen := parseCodexFixture(t, "codex_exec_tools.jsonl", "gpt-6-astra")
	if !res.Success || res.Text != "done" || res.SessionID != "01a0d7ef-cca9-7951-8f4a-1f3b721e5b0a" {
		t.Fatalf("result = %+v", res)
	}
	want := Usage{Model: "gpt-6-astra", InputTokens: 25133, CachedInputTokens: 12481,
		CacheWriteInputTokens: 12646, OutputTokens: 78}
	if res.Usage == nil || *res.Usage != want {
		t.Errorf("Usage = %+v, want %+v", res.Usage, want)
	}
	// 6 × $10 + 12481 × $1 + 12646 × $12.50 + 78 × $50, per 1M.
	if !near(res.CostUSD, 0.174516) {
		t.Errorf("CostUSD = %v, want 0.174516", res.CostUSD)
	}
	var cmd *CodexItem
	for _, ev := range seen {
		if ev.Type == "item.completed" && ev.Item.Type == "command_execution" {
			cmd = ev.Item
			break
		}
	}
	if cmd == nil || cmd.Command != "/bin/bash -lc ls" || cmd.AggregatedOutput != "README.md\n" ||
		cmd.ExitCode == nil || *cmd.ExitCode != 0 || cmd.Status != "completed" {
		t.Errorf("command item = %+v", cmd)
	}
}

// An error item is a warning, not a failure: the turn completes.
func TestParseCodexStreamErrorItemIsNotFailure(t *testing.T) {
	res, _ := parseCodexFixture(t, "codex_exec_item_error.jsonl", "gpt-6-astra")
	if !res.Success || res.Text != "hello from codex" || res.SessionID != "01a0d7ed-81b5-7160-9beb-5cecbe5617bf" {
		t.Fatalf("result = %+v", res)
	}
}

// An unknown model: retries, then turn.failed. Not a success, no usage,
// no cost, the thread id still read.
func TestParseCodexStreamError(t *testing.T) {
	res, seen := parseCodexFixture(t, "codex_exec_error.jsonl", "no-such-model")
	if res.Success || res.EmptyOutput {
		t.Fatalf("result = %+v", res)
	}
	if res.SessionID != "01a0d7ee-6b27-7e52-9336-294cf68a6cf8" || res.Text != "" ||
		res.Usage != nil || res.CostUSD != 0 || res.Turns != 0 {
		t.Errorf("result = %+v", res)
	}
	last := seen[len(seen)-1]
	if last.Type != "turn.failed" || last.Error == nil ||
		!strings.Contains(last.Error.Message, "The model `no-such-model` does not exist") {
		t.Errorf("last event = %+v", last)
	}
}

// A model with no list price costs 0 and says so.
func TestParseCodexStreamUnpricedModel(t *testing.T) {
	res, _ := parseCodexFixture(t, "codex_exec_apikey.jsonl", "gpt-unknown")
	if res.CostUSD != 0 || res.Usage == nil || !res.Usage.Unpriced || res.Usage.Model != "gpt-unknown" {
		t.Errorf("result = %+v, usage = %+v", res, res.Usage)
	}
	if res.Usage.InputTokens != 12624 {
		t.Errorf("InputTokens = %d", res.Usage.InputTokens)
	}
}

// Nothing at all, or nothing parseable, is EmptyOutput and not a success.
func TestParseCodexStreamEmpty(t *testing.T) {
	for _, in := range []string{"", "Reading additional input from stdin...\n"} {
		res := ParseCodexStream(strings.NewReader(in), "gpt-6-astra", nil)
		if !res.EmptyOutput || res.Success || res.Usage != nil {
			t.Errorf("%q: result = %+v", in, res)
		}
	}
}

// Every command failed on bubblewrap: SandboxBroken is the first bwrap
// line, whatever verdict the session wrote after. One clean command
// among them leaves it empty, as does a session with clean commands.
func TestParseCodexStreamSandboxBroken(t *testing.T) {
	res, _ := parseCodexFixture(t, "codex_exec_bwrap.jsonl", "gpt-6-sol")
	if res.SandboxBroken != "bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted" {
		t.Fatalf("SandboxBroken = %q", res.SandboxBroken)
	}
	if !res.Success || !strings.HasSuffix(res.Text, "VERDICT: PASS") {
		t.Fatalf("result = %+v", res)
	}
	if res, _ := parseCodexFixture(t, "codex_exec_bwrap_one_clean.jsonl", "gpt-6-sol"); res.SandboxBroken != "" {
		t.Fatalf("one clean command: SandboxBroken = %q", res.SandboxBroken)
	}
	if res, _ := parseCodexFixture(t, "codex_exec_tools.jsonl", "gpt-6-astra"); res.SandboxBroken != "" {
		t.Fatalf("clean commands: SandboxBroken = %q", res.SandboxBroken)
	}
	// capped at 300
	long := `{"type":"item.completed","item":{"id":"i","type":"command_execution","aggregated_output":"x bwrap: ` + strings.Repeat("a", 400) + `\n","exit_code":1,"status":"failed"}}`
	if got := ParseCodexStream(strings.NewReader(long), "gpt-6-sol", nil).SandboxBroken; len(got) != 300 || !strings.HasPrefix(got, "bwrap: aaa") {
		t.Fatalf("long: %d %q", len(got), got)
	}
}

// A sandbox with no network: the test commands died on a refused
// socket (one of them before that on `go: command not found`, which
// never ran), so SandboxBroken is the first socket line, whatever verdict
// the session wrote — while go vet ran clean. One test command that
// passed, or that failed on its own merits, leaves it empty (ADR 011, amendment 2026-09-29).
func TestCodexStreamSocketRefusedIsSandboxBroken(t *testing.T) {
	res, _ := parseCodexFixture(t, "codex_exec_socket.jsonl", "gpt-6-sol")
	want := "panic: httptest: failed to listen on a port: listen tcp6 [::1]:0: socket: operation not permitted [recovered, repanicked]"
	if res.SandboxBroken != want {
		t.Fatalf("SandboxBroken = %q", res.SandboxBroken)
	}
	if !strings.Contains(res.SandboxBroken, "socket: operation not permitted") || !strings.HasSuffix(res.Text, "VERDICT: FAIL") {
		t.Fatalf("result = %+v", res)
	}
	if res, _ := parseCodexFixture(t, "codex_exec_socket_one_passed.jsonl", "gpt-6-sol"); res.SandboxBroken != "" {
		t.Fatalf("one test command passed: SandboxBroken = %q", res.SandboxBroken)
	}
	// with no test command, every failed command refused is broken too;
	// a bind refusal counts as a socket one
	gh := `{"type":"item.completed","item":{"id":"i","type":"command_execution","command":"gh pr view 1","aggregated_output":"dial: bind: operation not permitted\n","exit_code":1,"status":"failed"}}` + "\n" +
		`{"type":"item.completed","item":{"id":"j","type":"command_execution","command":"git log","aggregated_output":"abc\n","exit_code":0,"status":"completed"}}`
	if got := ParseCodexStream(strings.NewReader(gh), "gpt-6-sol", nil).SandboxBroken; got != "dial: bind: operation not permitted" {
		t.Fatalf("no test command: %q", got)
	}
	// a refused test command beside one that failed on its own merits
	// is a verdict: the second ran
	mixed := `{"type":"item.completed","item":{"id":"i","type":"command_execution","command":"go test ./internal/gateway/","aggregated_output":"listen tcp6 [::1]:0: socket: operation not permitted\n","exit_code":1,"status":"failed"}}` + "\n" +
		`{"type":"item.completed","item":{"id":"j","type":"command_execution","command":"go test ./internal/node/","aggregated_output":"--- FAIL: TestX\n    x_test.go:9: got 1, want 2\n","exit_code":1,"status":"failed"}}`
	if got := ParseCodexStream(strings.NewReader(mixed), "gpt-6-sol", nil).SandboxBroken; got != "" {
		t.Fatalf("a refused test beside a real failure: %q", got)
	}
	if res, _ := parseCodexFixture(t, "codex_exec_socket_mixed.jsonl", "gpt-6-sol"); res.SandboxBroken != "" {
		t.Fatalf("mixed fixture: SandboxBroken = %q", res.SandboxBroken)
	}
	// a test that failed on its own merits is a verdict
	fail := `{"type":"item.completed","item":{"id":"i","type":"command_execution","command":"go test ./...","aggregated_output":"--- FAIL: TestX\n","exit_code":1,"status":"failed"}}`
	if got := ParseCodexStream(strings.NewReader(fail), "gpt-6-sol", nil).SandboxBroken; got != "" {
		t.Fatalf("a real failure: %q", got)
	}
}

// Only a test command that executed counts against the socket rule: a
// search that merely names a runner (`rg 'go test'`, an echo) is no
// test, and a test Codex declined or that reported no exit code never
// ran — so beside them a socket-refused `go test` is still SandboxBroken
// (ADR 011, amendment 2026-09-29).
func TestCodexStreamSocketRefusedCountsOnlyTestsThatRan(t *testing.T) {
	want := "panic: httptest: failed to listen on a port: listen tcp6 [::1]:0: socket: operation not permitted [recovered, repanicked]"
	for _, name := range []string{"codex_exec_socket_grep.jsonl", "codex_exec_socket_declined.jsonl"} {
		res, _ := parseCodexFixture(t, name, "gpt-6-sol")
		if res.SandboxBroken != want {
			t.Errorf("%s: SandboxBroken = %q", name, res.SandboxBroken)
		}
	}
}

func TestCodexIsTestCommand(t *testing.T) {
	for cmd, want := range map[string]bool{
		"/bin/bash -lc 'go test ./...'":                               true,
		"/bin/bash -lc '/usr/local/go/bin/go test ./... 2>&1 | tail'": true,
		"/bin/bash -lc 'cd web && npm run test'":                      true,
		"bash -lc \"GOFLAGS=-count=1 timeout -k 5 300 go test ./x\"":  true,
		"/bin/bash -lc 'make check'":                                  true,
		"go test ./internal/node/":                                    true,
		"/bin/bash -lc \"rg -n 'go test' AGENTS.md\"":                 false,
		"/bin/bash -lc 'echo \"go test\"'":                            false,
		"/bin/bash -lc 'grep -rn npm README.md'":                      false,
		"/bin/bash -lc 'go vet ./...'":                                false,
		"/bin/bash -lc 'git log --grep=\"go test\"'":                  false,
	} {
		if got := isTestCommand(cmd); got != want {
			t.Errorf("isTestCommand(%q) = %v, want %v", cmd, got, want)
		}
	}
}
