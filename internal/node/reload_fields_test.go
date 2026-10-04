package node

import (
	"context"
	"log"
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
var reloadPrefixes = []string{"limits:", "accounts ", "account ", "budget ", "project budget ", "context:", "claim:", "shares ", "shares:", "orgs ", "host:"}

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
	if got := loop.Thresholds(); got != (LimitsConfig{PaceFiveHour: 0.80, Stop: 0.95, Reserve: DefaultReserve}) {
		t.Fatalf("thresholds in force %+v, want 80%%/95%%", got)
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
		"context: bytes_max 81920 → 131072, doc_bytes_max 61440 → 32768")
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

// node.yaml's `host:` block is reloaded every tick like the claim
// limits, and each threshold's change is logged once, by itself, as
// `host: <field> <old> → <new>`; a field left alone logs nothing
// (ADR-004's amendment, the-node-backs-off-a-hot-host).
func TestReloadHostFromDisk(t *testing.T) {
	loop, _, logs := newPriorityFake(t).liveLoop(t, reloadYAML)
	wantReloadLines(t, "first tick", tickLines(t, loop, logs))
	if got := loop.HostThresholds(); got != (HostConfig{LoadPerCoreMax: 1.5, PackageTempMaxC: 90}) {
		t.Fatalf("host thresholds in force %+v, want the defaults 1.5/90", got)
	}

	writeNodeYAML(t, reloadYAML+"host: {load_per_core_max: 2}\n")
	wantReloadLines(t, "load_per_core_max edited", tickLines(t, loop, logs), "host: load_per_core_max 1.5 → 2")
	if got := loop.HostThresholds(); got != (HostConfig{LoadPerCoreMax: 2, PackageTempMaxC: 90}) {
		t.Fatalf("host thresholds in force %+v, want 2/90", got)
	}

	writeNodeYAML(t, reloadYAML+"host: {load_per_core_max: 2, package_temp_max_c: 85}\n")
	wantReloadLines(t, "package_temp_max_c edited", tickLines(t, loop, logs), "host: package_temp_max_c 90 → 85")
	if got := loop.HostThresholds(); got != (HostConfig{LoadPerCoreMax: 2, PackageTempMaxC: 85}) {
		t.Fatalf("host thresholds in force %+v, want 2/85", got)
	}

	writeNodeYAML(t, reloadYAML+"host: {load_per_core_max: 2, package_temp_max_c: 85}\n")
	wantReloadLines(t, "unchanged", tickLines(t, loop, logs))
}

// node.yaml's `shares:` is reloaded every tick like the claim limits,
// and a change is logged once as `shares <old> → <new>` (ADR-023). It
// reads through OwnedReloader, which never touches MyOrgs, so the loop
// serves two scopes named a and b directly.
func TestReloadSharesFromDisk(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeNodeYAML(t, "slug: box\nname: Box\ncapacity: 1\nshares: {a: 1, b: 1}\n")

	var logs strings.Builder
	loop := &Loop{Cfg: &Config{Capacity: Fixed(1)}, Scopes: Scopes{{Login: "a", ID: "org-a"}, {Login: "b", ID: "org-b"}}, Log: log.New(&logs, "", 0)}
	loop.Reload = OwnedReloader(LoadConfig)

	loop.reload(context.Background())
	if got := loop.Weights(); !sharesEqual(got, map[string]int{"a": 1, "b": 1}) {
		t.Fatalf("weights after the first tick %v, want a:1 b:1", got)
	}
	logs.Reset()

	writeNodeYAML(t, "slug: box\nname: Box\ncapacity: 1\nshares: {a: 3, b: 1}\n")
	loop.reload(context.Background())
	if got := strings.TrimSpace(logs.String()); got != "shares a 1, b 1 → a 3, b 1" {
		t.Fatalf("after the edit, logged %q, want the shares change line", got)
	}
	if got := loop.Weights(); !sharesEqual(got, map[string]int{"a": 3, "b": 1}) {
		t.Fatalf("weights after the edit %v, want a:3 b:1", got)
	}
	logs.Reset()

	writeNodeYAML(t, "slug: box\nname: Box\ncapacity: 1\nshares: {a: 3, b: 1}\n")
	loop.reload(context.Background())
	if got := logs.String(); got != "" {
		t.Fatalf("node.yaml unchanged logged %q", got)
	}
}

// A weight naming an org the node does not serve is logged once and
// ignored: it is dropped from Weights(), the served org's own weight
// still in force. node.yaml left unchanged after that — ghost still
// named in it — logs the warning no further: "an unchanged file logs
// nothing" holds even when the file names an unserved org.
func TestReloadSharesUnservedOrgIgnored(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeNodeYAML(t, "slug: box\nname: Box\ncapacity: 1\nshares: {a: 2, ghost: 5}\n")

	var logs strings.Builder
	loop := &Loop{Cfg: &Config{Capacity: Fixed(1)}, Scopes: Scopes{{Login: "a", ID: "org-a"}}, Log: log.New(&logs, "", 0)}
	loop.Reload = OwnedReloader(LoadConfig)
	loop.reload(context.Background())

	if got := loop.Weights(); !sharesEqual(got, map[string]int{"a": 2}) {
		t.Fatalf("weights %v, want a:2 alone (ghost dropped)", got)
	}
	if got := logs.String(); strings.Count(got, "shares: ghost is not a served org") != 1 {
		t.Fatalf("logged %q, want the unserved-org line exactly once", got)
	}
	logs.Reset()

	// the same file, unchanged, reloaded again: ghost is still unserved
	// but already logged, and the shares themselves did not change —
	// nothing is logged on this tick.
	loop.reload(context.Background())
	if got := logs.String(); got != "" {
		t.Fatalf("second tick of the unchanged file logged %q, want nothing", got)
	}

	// a third tick, still unchanged: the ghost line stays said once.
	loop.reload(context.Background())
	if got := logs.String(); got != "" {
		t.Fatalf("third tick of the unchanged file logged %q, want nothing", got)
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
