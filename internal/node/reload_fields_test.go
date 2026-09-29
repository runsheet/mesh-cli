package node

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// Every field Reload carries is proved live from disk: a node loop
// whose Reload is Reloader over LoadConfig, under a temp HOME, ticks,
// ~/.mesh/node.yaml is rewritten, it ticks again, and the new value is
// in force through the loop's own accessor, logged as one whole line
// with its old and new value. Capacity is TestScalingReloadClampsIntoTheNewRange's
// and the capacity 6 → 8 test's.

// reloadYAML is the registration every test's node.yaml starts with.
const reloadYAML = "slug: box\nname: Box\norgs: [acme]\ncapacity: 1\n"

// reloadPrefixes are how a reload's log lines begin, one per field.
var reloadPrefixes = []string{"limits:", "accounts ", "account ", "budget ", "project budget ", "context:", "claim:", "orgs "}

// tickLines runs one tick and returns the lines it logged.
func tickLines(t *testing.T, loop *Loop, logs *strings.Builder) []string {
	t.Helper()
	before := logs.Len()
	loop.Tick(context.Background())
	loop.wg.Wait()
	return strings.Split(strings.TrimSpace(logs.String()[before:]), "\n")
}

// reloadLines are the lines of a reload's changes among lines.
func reloadLines(lines []string) []string {
	var out []string
	for _, l := range lines {
		for _, p := range reloadPrefixes {
			if strings.HasPrefix(l, p) {
				out = append(out, l)
				break
			}
		}
	}
	return out
}

// wantReloadLines fails unless the reload lines of a tick are want,
// whole and in order — nothing else is logged for a field left alone.
func wantReloadLines(t *testing.T, tick string, lines []string, want ...string) {
	t.Helper()
	if got := reloadLines(lines); !slices.Equal(got, want) {
		t.Fatalf("%s: reload lines\n%q\nwant\n%q\nin\n%s", tick, got, want, strings.Join(lines, "\n"))
	}
}

func TestReloadLimitsFromDisk(t *testing.T) {
	loop, _, logs := newPriorityFake(t).liveLoop(t, reloadYAML+"limits: {pace_five_hour: 70, stop: 90}\n")
	wantReloadLines(t, "first tick", tickLines(t, loop, logs))

	writeNodeYAML(t, reloadYAML+"limits: {pace_five_hour: 80, stop: 95}\n")
	wantReloadLines(t, "after the edit", tickLines(t, loop, logs), "limits: pace five-hour 70% → 80%, stop 90% → 95%")
	if got := loop.Thresholds(); got != (LimitsConfig{PaceFiveHour: 0.80, Stop: 0.95}) {
		t.Fatalf("thresholds in force %+v, want 80%%/95%%", got)
	}
}

func TestReloadAccountsFromDisk(t *testing.T) {
	const a = "  - {label: sub-a, kind: subscription, token_file: ~/a.token}\n"
	loop, _, logs := newPriorityFake(t).liveLoop(t, reloadYAML+"accounts:\n"+a+
		"  - {label: api-b, kind: api, api_key_env: KEY_B, daily_cap: 20}\n")
	wantReloadLines(t, "first tick", tickLines(t, loop, logs))

	// the same labels, one's cap and the other's credential changed
	writeNodeYAML(t, reloadYAML+"accounts:\n"+
		"  - {label: sub-a, kind: subscription, token_file: ~/a2.token}\n"+
		"  - {label: api-b, kind: api, api_key_env: KEY_B, daily_cap: 30}\n")
	wantReloadLines(t, "caps and credential", tickLines(t, loop, logs),
		"account sub-a: token_file ~/a.token → ~/a2.token",
		"account api-b: daily_cap $20.00 → $30.00")
	accounts := loop.nodeAccounts()
	if len(accounts) != 2 || accounts[0].TokenFile != "~/a2.token" || accounts[1].DailyCapUSD != 30 {
		t.Fatalf("accounts in force %+v, want sub-a on ~/a2.token and api-b at $30", accounts)
	}

	// one removed, one added, and api-b's cap back to its default
	writeNodeYAML(t, reloadYAML+"accounts:\n"+
		"  - {label: api-b, kind: api, api_key_env: KEY_B}\n"+
		"  - {label: api-c, kind: api, api_key_env: KEY_C, project: mesh}\n")
	wantReloadLines(t, "membership", tickLines(t, loop, logs),
		"accounts sub-a, api-b → api-b, api-c",
		"account api-b: daily_cap $30.00 → $50.00")
	if got := labels(loop.nodeAccounts()); got != "api-b, api-c" {
		t.Fatalf("accounts in force %s, want api-b, api-c", got)
	}
}

func TestReloadBudgetsFromDisk(t *testing.T) {
	loop, _, logs := newPriorityFake(t).liveLoop(t, reloadYAML)
	wantReloadLines(t, "first tick", tickLines(t, loop, logs))

	writeNodeYAML(t, reloadYAML+"budgets: {build: {sonnet: 7}, evaluate: {opus: 5}}\nproject_budgets: {mesh: 20}\n")
	wantReloadLines(t, "added", tickLines(t, loop, logs),
		"budget build/sonnet $4.00 → $7.00",
		"budget evaluate/opus none → $5.00",
		"project budget mesh none → $20.00")
	if got, _ := loop.KindModelBudget("build", "sonnet"); got != 7 {
		t.Fatalf("build/sonnet in force $%.2f, want $7.00", got)
	}
	if got := loop.projectBudgets()["mesh"]; got != 20 {
		t.Fatalf("project mesh in force $%.2f, want $20.00", got)
	}

	writeNodeYAML(t, reloadYAML+"budgets: {build: {sonnet: 7}, evaluate: {opus: 6}}\nproject_budgets: {mesh: 25}\n")
	wantReloadLines(t, "changed", tickLines(t, loop, logs),
		"budget evaluate/opus $5.00 → $6.00",
		"project budget mesh $20.00 → $25.00")
	if got, _ := loop.KindModelBudget("evaluate", "claude-opus-5"); got != 6 {
		t.Fatalf("evaluate/opus in force $%.2f, want $6.00", got)
	}

	writeNodeYAML(t, reloadYAML)
	wantReloadLines(t, "removed", tickLines(t, loop, logs),
		"budget build/sonnet $7.00 → $4.00",
		"budget evaluate/opus $6.00 → none",
		"project budget mesh $25.00 → none")
	if got, src := loop.KindModelBudget("build", "sonnet"); got != 4 || src != "the default for build/sonnet" {
		t.Fatalf("build/sonnet in force $%.2f from %s, want the default $4.00", got, src)
	}
	if got, _ := loop.KindModelBudget("evaluate", "opus"); got != 0 {
		t.Fatalf("evaluate/opus in force $%.2f, want none", got)
	}
}

func TestReloadContextFromDisk(t *testing.T) {
	loop, _, logs := newPriorityFake(t).liveLoop(t, reloadYAML)
	wantReloadLines(t, "first tick", tickLines(t, loop, logs))

	writeNodeYAML(t, reloadYAML+"context: {bytes_max: 131072, doc_bytes_max: 32768}\n")
	wantReloadLines(t, "after the edit", tickLines(t, loop, logs),
		"context: bytes_max 245760 → 131072, doc_bytes_max 61440 → 32768")
	if got := loop.ContextBudget(); got != (ContextConfig{BytesMax: 131072, DocBytesMax: 32768}) {
		t.Fatalf("context budget in force %+v, want 131072/32768", got)
	}
}

func TestReloadClaimFromDisk(t *testing.T) {
	loop, _, logs := newPriorityFake(t).liveLoop(t, reloadYAML)
	wantReloadLines(t, "first tick", tickLines(t, loop, logs))

	writeNodeYAML(t, reloadYAML+"claim: {unmerged_per_repo: 2}\n")
	wantReloadLines(t, "after the edit", tickLines(t, loop, logs), "claim: unmerged_per_repo 0 → 2")
	if got := loop.UnmergedPerRepo(); got != 2 {
		t.Fatalf("unmerged_per_repo in force %d, want 2", got)
	}
}

// The orgs restriction is node.yaml's; MyOrgs (acme and globex) never
// changes.
func TestReloadOrgsFromDisk(t *testing.T) {
	loop, _, logs := newPriorityFake(t).liveLoop(t, reloadYAML)
	wantReloadLines(t, "first tick", tickLines(t, loop, logs))

	writeNodeYAML(t, "slug: box\nname: Box\norgs: [acme, globex]\ncapacity: 1\n")
	wantReloadLines(t, "widened", tickLines(t, loop, logs), "orgs acme → acme, globex")
	if got := loop.Scopes.Names(); !slices.Equal(got, []string{"acme", "globex"}) {
		t.Fatalf("serving %v, want acme, globex", got)
	}

	writeNodeYAML(t, reloadYAML)
	wantReloadLines(t, "narrowed", tickLines(t, loop, logs), "orgs acme, globex → acme")
	if got := loop.Scopes.Names(); !slices.Equal(got, []string{"acme"}) {
		t.Fatalf("serving %v, want acme", got)
	}
}

// A node.yaml rewritten as it was changes nothing, and says nothing.
func TestReloadUnchangedLogsNothing(t *testing.T) {
	const yaml = reloadYAML + "limits: {pace_five_hour: 70, stop: 90}\n" +
		"accounts:\n  - {label: api-b, kind: api, api_key_env: KEY_B, daily_cap: 20, project: mesh}\n" +
		"project_budgets: {mesh: 20}\nbudgets: {build: {sonnet: 7}}\n" +
		"context: {bytes_max: 131072}\nclaim: {unmerged_per_repo: 2}\n"
	loop, _, logs := newPriorityFake(t).liveLoop(t, yaml)
	for i := range 3 {
		writeNodeYAML(t, yaml)
		if got := reloadLines(tickLines(t, loop, logs)); len(got) != 0 {
			t.Fatalf("tick %d with node.yaml unchanged logged %q", i+1, got)
		}
	}
}
