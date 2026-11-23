package nodetools

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/mcptool"
	"projectmesh.io/mesh-cli/internal/meshapi"
	rawresults "projectmesh.io/mesh-cli/internal/results"
	"projectmesh.io/mesh-cli/internal/validate"
	"projectmesh.io/mesh-cli/internal/validate/docscheck"
	"projectmesh.io/mesh-cli/internal/validate/infer"
)

// RunSuiteTool is the run_suite node tool's name. It runs the
// checkout's declared validation (.runsheet/validate.yml, through
// internal/validate), or, when it declares none, the suites the node
// infers from the checkout itself (internal/validate/infer, ADR-053) —
// every suite, or the ones named — on the node, and records the result:
// its output blob is every step's outcome and every test's final event,
// go test -json normalised to {package, test, action, elapsed}. A step
// whose command is the node's own docs check (docscheck.Command) runs in
// process on the checkout (withBuiltins).
const RunSuiteTool = "run_suite"

// SuiteRunner runs validate's steps in a workspace (validate.Run on a
// HostRunner in it), each step's environment also carrying env: the
// call's selection (SelectionEnv), empty when it named none.
type SuiteRunner func(ctx context.Context, workspace string, steps []validate.Step, env []string) ([]validate.StepResult, error)

// The variables a suite's command reads its selection from (projectmesh-io/api
// ADR 001 amendment, evidence waivers; mesh-specs gap 27): each space
// separated, and unset when the call named none. A command opts in, e.g.
// go test ${MESH_SUITE_TARGETS:-./...} ${MESH_SUITE_TESTS:+-run "^(${MESH_SUITE_TESTS// /|})$"};
// one that ignores them runs whole, which is still correct.
const (
	SuiteTargetsEnv = "MESH_SUITE_TARGETS"
	// SuiteTestsEnv is also how the node's supervisor names the failing
	// tests it runs again (validate.SuiteTestsEnv): one way to run a
	// test by name.
	SuiteTestsEnv = validate.SuiteTestsEnv
)

// HostSuites runs steps on the host, in the workspace, every step run
// whatever an earlier one did. The steps are the session's own, so they
// see none of the node's model variables (harness.ModelEnv; ADR 001
// amendment "model relay"), and no selection but the call's.
func HostSuites(ctx context.Context, workspace string, steps []validate.Step, env []string) ([]validate.StepResult, error) {
	var base []string
	for _, kv := range harness.EnvironWithoutModel() {
		if !strings.HasPrefix(kv, SuiteTargetsEnv+"=") && !strings.HasPrefix(kv, SuiteTestsEnv+"=") {
			base = append(base, kv)
		}
	}
	host := validate.HostRunner{Dir: workspace, Env: append(base, env...)}
	return validate.Run(ctx, steps, host, validate.Options{Dir: workspace, ContinueOnFailure: true})
}

// withBuiltins is run with the steps the node runs itself taken out: a
// step whose command is the node's docs check (docscheck.IsCommand, the
// suite infer gives a repository that holds only documents) runs here,
// in process, on the checkout — the pod image carries no mesh binary,
// and the check only reads the checkout, through an os.Root, and
// executes nothing of it (ADR-053 amendment 2026-11-16). Every other
// step goes to run as before, and the results come back in the steps'
// order. A red built-in under fail_fast runs nothing more.
func withBuiltins(run SuiteRunner, failFast bool) SuiteRunner {
	return func(ctx context.Context, workspace string, steps []validate.Step, env []string) ([]validate.StepResult, error) {
		own := map[string]validate.StepResult{}
		var rest []validate.Step
		red := false
		for _, st := range steps {
			if !docscheck.IsCommand(st.Command) {
				rest = append(rest, st)
				continue
			}
			r := docsCheckStep(workspace, st)
			own[st.Name] = r
			red = red || r.ExitCode != 0
		}
		if len(own) == 0 {
			return run(ctx, workspace, steps, env)
		}
		var got []validate.StepResult
		var err error
		if len(rest) > 0 && !(red && failFast) && ctx.Err() == nil {
			got, err = run(ctx, workspace, rest, env)
		}
		ran := map[string]validate.StepResult{}
		for _, r := range got {
			ran[r.Name] = r
		}
		out := make([]validate.StepResult, 0, len(own)+len(got))
		for _, st := range steps {
			if r, ok := own[st.Name]; ok {
				out = append(out, r)
			} else if r, ok := ran[st.Name]; ok {
				out = append(out, r)
				delete(ran, st.Name)
			}
		}
		for _, r := range got {
			if _, left := ran[r.Name]; left {
				out = append(out, r) // a result the runner named apart from its step
			}
		}
		return out, err
	}
}

// docsCheckOutputCap bounds the output a docs check's StepResult keeps,
// as validate's own steps are bounded: the end of the report, where the
// summary and the last problems are.
const docsCheckOutputCap = 8 * 1024

// docsCheckStep runs the node's docs check on workspace as step st.
// Each document it read is one of the step's tests, package the step's
// name and test the document's path — passing, or failing with its
// problems as its output — so a red check names the documents that are
// wrong wherever a failing test is named: run_suite's answer, the
// validation's failing tests, the rework's brief.
func docsCheckStep(workspace string, st validate.Step) validate.StepResult {
	start := time.Now()
	r := validate.StepResult{Name: st.Name, Command: st.Command}
	res, err := docscheck.Check(workspace)
	var report string
	switch {
	case err != nil:
		r.ExitCode = 2
		report = "docs-check: the checkout could not be read: " + err.Error() + "\n"
	case !res.OK():
		r.ExitCode = 1
		report = res.Report()
	default:
		report = res.Report()
	}
	if err == nil {
		wrong := map[string][]string{}
		var order []string
		for _, p := range res.Problems {
			if _, seen := wrong[p.Path]; !seen {
				order = append(order, p.Path)
			}
			wrong[p.Path] = append(wrong[p.Path], p.String())
		}
		for _, path := range res.Paths {
			if _, bad := wrong[path]; !bad {
				r.Tests = append(r.Tests, validate.TestResult{Package: st.Name, Name: path, Action: "pass"})
			}
		}
		for _, path := range order {
			r.Tests = append(r.Tests, validate.TestResult{Package: st.Name, Name: path, Action: "fail",
				Output: validate.RedactTokens(strings.Join(wrong[path], "\n") + "\n")})
		}
		r.TestsRun = len(r.Tests)
		r.TestsFailed = len(order)
		r.TestsPassed = r.TestsRun - r.TestsFailed
	}
	r.DurationMs = time.Since(start).Milliseconds()
	report = validate.RedactTokens(report)
	r.Output = report
	if len(report) > docsCheckOutputCap {
		r.Output = "[mesh: the docs check's report is cut to its last 8 KiB]\n" + report[len(report)-docsCheckOutputCap:]
	}
	if r.ExitCode != 0 {
		lines := strings.Split(strings.TrimRight(report, "\n"), "\n")
		if len(lines) > validate.TailLines {
			lines = lines[len(lines)-validate.TailLines:]
		}
		r.Tail = strings.Join(lines, "\n")
	}
	return r
}

// Selection is the part of a suite a run_suite call named: the targets
// (packages, paths or build targets) and the tests (test names). The
// signed record carries it exactly as named, either list omitted when
// empty; a call that named neither has none, and its record no
// selection key at all.
type Selection struct {
	Targets []string `json:"targets,omitempty"`
	Tests   []string `json:"tests,omitempty"`
}

// maxSelection is how many targets, and how many tests, a selection may
// name: Mesh refuses a record naming more.
const maxSelection = 500

// SelectionEnv is the selection as its suite's command reads it:
// MESH_SUITE_TARGETS and MESH_SUITE_TESTS, each space separated, each
// absent when the selection names none.
func SelectionEnv(s *Selection) []string {
	if s == nil {
		return nil
	}
	var env []string
	if len(s.Targets) > 0 {
		env = append(env, SuiteTargetsEnv+"="+strings.Join(s.Targets, " "))
	}
	if len(s.Tests) > 0 {
		env = append(env, SuiteTestsEnv+"="+strings.Join(s.Tests, " "))
	}
	return env
}

// ErrNoSuiteRunner is run_suite refused: the lease has nowhere to run
// its steps. The steps are commands the session wrote
// (.runsheet/validate.yml in its own checkout), so they run where the
// session runs — in its lease's pod — and never on the host of a node
// that sandboxes its sessions.
var ErrNoSuiteRunner = errors.New("run_suite cannot run here: the lease has no sandbox to run its suites in, and this node runs none on the host")

// RunSuite is one lease's run_suite tool.
type RunSuite struct {
	Workspace string
	Recorder  *Recorder
	// Run runs the steps: the lease's pod's runner on a sandboxed node.
	// Nil runs them on the host (HostSuites) only with OnHost, and
	// otherwise refuses (ErrNoSuiteRunner) before reading validate.yml.
	Run SuiteRunner
	// OnHost is the explicit opt-in to running the steps on the host
	// when Run is nil: only for a node whose sessions themselves run on
	// the host, unsandboxed (dev, test), where the steps can do nothing
	// the session could not. The zero value never runs on the host.
	OnHost bool
	// Escalate hears a red run, its summary why (PR 2.11, trigger
	// "red"), and answers the model the session moves to, "" when it
	// does not; nil never escalates.
	Escalate func(why string) string
	// OnActivity hears the run working (ADR-049 amendment, step
	// progress): each write a unit's command makes, each test event and
	// a lane opening (validate.WithOnActivity), unthrottled — the node
	// moves the lease's progress on it, at most once a minute. A run
	// that writes nothing calls it never, so a silent run shows Mesh no
	// progress and its stall limit can end it. Nil hears nothing.
	OnActivity func(what string)
	// Logf hears what the node's log should: the suites it inferred for
	// a checkout with no validate.yml (ADR-053). Nil logs nothing.
	Logf func(format string, args ...any)

	// Results is the brief's results location (meshapi.Brief.Results):
	// when it names an UploadURL, a run's complete raw output goes there
	// (internal/results.Upload), on Session, and the signed record names
	// the upload's digest alone, never Mesh's own blob storage. The zero
	// value keeps today's path: the output blob posted to the Recorder's
	// Mesh as before.
	Results meshapi.ResultsLocation
	// Session authenticates the upload to Results; unused when Results
	// names no UploadURL.
	Session rawresults.Session

	// A run outlives the call that starts it (ADR-049): the call waits
	// at most Wait (DefaultSuiteWait when zero) — under the MCP call's
	// own timeout — and then answers the run's handle, {run_id, status:
	// running}, which the session polls with run_suite {run_id}. The run
	// goes on in Context, the lease's (context.Background when nil), so
	// it ends with the lease and never with a call.
	Wait    time.Duration
	Context context.Context
	// Budget is the repository's validation budget (validate.yml's
	// budget:, read from the checkout as the lease began) and Began the
	// lease's start: a run still going at Began+Budget is stopped and
	// answered red, out of time, and a run asked for after it runs
	// nothing and is answered the same. Zero Budget bounds nothing.
	Budget time.Duration
	Began  time.Time
	// OnFinished, set, hears each run's answer as the run ends, whether
	// or not any call delivers it: what the lease reports when its
	// budget stops the session before the session reports itself.
	OnFinished func(answer string)
	// OnCoverage, set, hears the content of an e2e coverage map a run's
	// step wrote (CoverageReport among its reports, runsheet's
	// e2ecoverage suite on main's validation): what the node carries on
	// the validate result's "coverage" key (nodev1 withCoverage). A map
	// cut at the attestation's report bound, or not JSON, is never
	// heard: Logf says why it was skipped.
	OnCoverage func(content string)

	mu   sync.Mutex
	runs map[string]*suiteRun
	n    int
}

// DefaultSuiteWait is how long one run_suite call waits on its run
// before it answers the run's handle: well under the 30 minutes a
// session's MCP call gives up after (mesh-specs gap 36).
const DefaultSuiteWait = 10 * time.Minute

// suiteRun is one run_suite run, started by a call and polled by its id.
type suiteRun struct {
	id      string
	suites  []string
	sel     *Selection
	started time.Time
	done    chan struct{}
	answer  string
	err     error
	// delivered is whether a call has answered the run's end: a run
	// that ended unanswered is what a repeat call for the same
	// selection answers, instead of running it again. Guarded by
	// RunSuite.mu.
	delivered bool

	liveMu sync.Mutex
	// live is each test's result as it streamed in (validate.WithOnTest),
	// while the run is still going: a poll mid-run answers it so a
	// session watching a long suite sees a failure as it happens. Read
	// only while the run is going (await's handle); the finished answer
	// is Normalise's own, authoritative list, never this one.
	live []TestResult

	laneMu sync.Mutex
	// lanes is each lane's stop func, by name (validate.WithOnLane), as
	// each opens: stop_suite names a lane and calls it, ending that
	// shard's (or unsharded step's) command and pod alone, while the
	// run's other lanes go on.
	lanes map[string]func(reason string)
}

// addLive appends a streamed test's result to the run's live list.
func (r *suiteRun) addLive(t TestResult) {
	r.liveMu.Lock()
	r.live = append(r.live, t)
	r.liveMu.Unlock()
}

// liveTests is the run's live list so far.
func (r *suiteRun) liveTests() []TestResult {
	r.liveMu.Lock()
	defer r.liveMu.Unlock()
	return slices.Clone(r.live)
}

// registerLane records name's stop func as its lane opens.
func (r *suiteRun) registerLane(name string, stop func(reason string)) {
	r.laneMu.Lock()
	if r.lanes == nil {
		r.lanes = map[string]func(reason string){}
	}
	r.lanes[name] = stop
	r.laneMu.Unlock()
}

// laneStop is name's registered stop func, and whether it has one: a
// lane not yet open, or never sharded, or already ended, has none.
func (r *suiteRun) laneStop(name string) (func(reason string), bool) {
	r.laneMu.Lock()
	defer r.laneMu.Unlock()
	stop, ok := r.lanes[name]
	return stop, ok
}

// SuiteOutput is run_suite's output blob.
type SuiteOutput struct {
	Steps []SuiteStep  `json:"steps"`
	Tests []TestResult `json:"tests"`
	// Inferred is what the node inferred the suites from, and the
	// validate.yml it wrote, when the checkout declares none (ADR-053):
	// what a product may store to run the same suites again. Absent for
	// a declared validate.yml.
	Inferred *infer.Profile `json:"inferred,omitempty"`
}

// SuiteStep is one step's outcome; a sharded step's (validate.yml
// shards:) also each shard's, the step's exit the first failing
// shard's and its duration the longest shard's.
type SuiteStep struct {
	Name        string `json:"name"`
	Command     string `json:"command"`
	Exit        int    `json:"exit"`
	DurationMs  int64  `json:"duration_ms"`
	TestsRun    int    `json:"tests_run"`
	TestsPassed int    `json:"tests_passed"`
	TestsFailed int    `json:"tests_failed"`
	// TestsFlaky counts the tests that failed and passed when the node
	// ran them again by name (validate's supervisor): no failure of the
	// step's, and named in the tests list with action flaky.
	TestsFlaky int          `json:"tests_flaky,omitempty"`
	Shards     []SuiteShard `json:"shards,omitempty"`
	// Stopped is whether the step (or, for a sharded step, the shard
	// whose exit the step took) was ended on purpose by stop_suite,
	// never by failing on its own; its Exit is still non-zero.
	Stopped    bool   `json:"stopped,omitempty"`
	StopReason string `json:"stop_reason,omitempty"`
	// Reason is why the node called the step red when its command's own
	// exit did not: "ran 0 tests" under the suite's min_tests.
	Reason string `json:"reason,omitempty"` // Hung is a run whose go test ended in its timeout panic twice, the
	// second time in a fresh lane (validate's hung.go): a hang, never a
	// failing test of the change's; HungTests are the tests the panic
	// named as running, and Rerun what the node's re-run found (also
	// when the re-run did not hang). LogEvidence is the step_log record
	// keeping the run's whole output — the goroutine dump — and LogDigest
	// that output's blob.
	Hung        bool     `json:"hung,omitempty"`
	HungTests   []string `json:"hung_tests,omitempty"`
	Rerun       string   `json:"rerun,omitempty"`
	LogEvidence string   `json:"log_evidence,omitempty"`
	LogDigest   string   `json:"log_digest,omitempty"`
}

// SuiteShard is one shard of a sharded step: its index (0-based, as
// MESH_SHARD_INDEX) of total, and how it ended — or that it never ran
// (not_run: a fail-fast run stopped first, or its lane did not open).
type SuiteShard struct {
	Index       int   `json:"index"`
	Total       int   `json:"total"`
	Exit        int   `json:"exit"`
	DurationMs  int64 `json:"duration_ms"`
	TestsRun    int   `json:"tests_run"`
	TestsPassed int   `json:"tests_passed"`
	TestsFailed int   `json:"tests_failed"`
	TestsFlaky  int   `json:"tests_flaky,omitempty"`
	NotRun      bool  `json:"not_run,omitempty"`
	// Stopped is whether this shard was ended on purpose, mid-run, by a
	// stop_suite call naming its lane, rather than by failing on its
	// own: its Exit is still non-zero, never 0, and StopReason names why.
	Stopped    bool   `json:"stopped,omitempty"`
	StopReason string `json:"stop_reason,omitempty"`
	Reason     string `json:"reason,omitempty"` // Hung is a run whose go test ended in its timeout panic twice, the
	// second time in a fresh lane (validate's hung.go): a hang, never a
	// failing test of the change's; HungTests are the tests the panic
	// named as running, and Rerun what the node's re-run found (also
	// when the re-run did not hang). LogEvidence is the step_log record
	// keeping the run's whole output — the goroutine dump — and LogDigest
	// that output's blob.
	Hung        bool     `json:"hung,omitempty"`
	HungTests   []string `json:"hung_tests,omitempty"`
	Rerun       string   `json:"rerun,omitempty"`
	LogEvidence string   `json:"log_evidence,omitempty"`
	LogDigest   string   `json:"log_digest,omitempty"`
}

// TestResult is one test's (or, Test empty, one package's) final event:
// go test's pass, fail or skip, timeout for a test still running when
// its step was killed, and the node's supervisor's flaky (failed, then
// passed when the node ran it again by name) and stalled (its silence
// had its unit stopped).
type TestResult struct {
	Package string  `json:"package"`
	Test    string  `json:"test"`
	Action  string  `json:"action"`
	Elapsed float64 `json:"elapsed"`
	// Output is the test's own output (validate.TestResult.Output, from
	// the full untruncated go test -json stream): kept only for a
	// failing or still-running (timeout) test, never a pass or a skip;
	// empty for a step whose report predates it, read instead by
	// goTestEvents' Report reparse.
	Output string `json:"output,omitempty"`
	// Retries is how many times the node ran the test again by name
	// after it failed (validate.TestResult.Retries): on a flaky test the
	// retry it passed on, on a fail the retries it failed again on — a
	// confirmed failure.
	Retries int `json:"retries,omitempty"`
}

// answerTestsCap bounds run_suite's answer's "tests" list in marshaled
// bytes: a failing or still-running test's output is kept whole as long
// as the list fits under it; once it would not, the last such test's
// output is stripped, repeatedly, until it fits or none is left to
// strip — never cutting an output in half — and tests_truncated is set.
const answerTestsCap = 16 * 1024

// answerTest is one test's entry in run_suite's answer.
type answerTest struct {
	Name       string `json:"name"`
	Package    string `json:"package"`
	Outcome    string `json:"outcome"`
	DurationMs int64  `json:"duration_ms"`
	Output     string `json:"output,omitempty"`
	Retries    int    `json:"retries,omitempty"`
}

// answerTestsOf is tests (a step's own TestResult, a package-level
// rollup's empty name skipped) as the answer's bounded "tests" list.
func answerTestsOf(tests []TestResult) ([]answerTest, bool) {
	var list []answerTest
	for _, t := range tests {
		if t.Test == "" {
			continue
		}
		list = append(list, answerTest{Name: t.Test, Package: t.Package, Outcome: t.Action,
			DurationMs: int64(math.Round(t.Elapsed * 1000)), Output: t.Output, Retries: t.Retries})
	}
	truncated := false
	for marshaledLen(list) > answerTestsCap {
		i := lastWithOutput(list)
		if i < 0 {
			break
		}
		list[i].Output = ""
		truncated = true
	}
	return list, truncated
}

// lastWithOutput is the last entry of list still carrying its output, -1
// when none does.
func lastWithOutput(list []answerTest) int {
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].Output != "" {
			return i
		}
	}
	return -1
}

func marshaledLen(list []answerTest) int {
	b, _ := json.Marshal(list)
	return len(b)
}

// Tool is run_suite {suite?, suites?, targets?, tests?}.
func (s *RunSuite) Tool() mcptool.Tool {
	list := func(desc string) map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
	}
	return mcptool.Tool{
		Name: RunSuiteTool,
		Description: "Run the repository's declared validation (.runsheet/validate.yml) on your checkout as it stands — or, when it declares none, the suites the node infers from go.mod, package.json or a Makefile, named in the answer's inferred — " +
			"every suite or the one you name, whole or only the targets and tests you name. A sharded suite's shards run at once, each beside its own services. " +
			"Answers each step's exit and test counts, the failing tests, " +
			"and every test's own name, package, outcome and duration (a test still running when a step was killed at its timeout named outcome timeout); " +
			"a failing or still-running test's own output is kept whole until the tests list would pass a size cap, after which the rest are named with their output stripped and tests_truncated is set. " +
			"and the evidence id of the node's signed record of the run: pass that id on, never your own account of it. " +
			"A run of named targets or tests is recorded as that selection, never as the suite's full run. " +
			"The node supervises the tests itself, so never re-run a failing test to see whether it is flaky: a failing test is run again by name once its step or shard ends, " +
			"one that then passes is named with outcome flaky (in flaky_tests) and does not fail its step, and one that fails again (retries on its entry) is a confirmed failure, " +
			"which stops the rest of the run unless its suite says run_to_end; a step or shard whose tests all fall silent past its suite's stall limit is stopped, the tests it was running named with outcome stalled (one slow test beside others that still report is not a stall); " +
			"and a step that exits 0 having run no tests is red with reason \"ran 0 tests\". " +
			"A run still going after a while answers {run_id, status: running} instead: call run_suite {run_id} again to wait for it, as often as it takes; " +
			"a call naming no run_id while a run of the same suites is still going on this lease starts nothing: it answers that run, exactly as run_suite {run_id} would (and a run that ended unanswered answers its result to a call for the same suites, targets and tests). " +
			"Two runs of one suite never run at once: a call whose suites overlap a running run's — whatever targets or tests either names — answers that run with a note, and you call again once it ends to run your own selection; " +
			"suites that overlap no running run start their own run beside it. A run past the repository's budget is stopped and answered red with out_of_time: finish red, saying it ran out of time.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"suite":  map[string]any{"type": "string", "description": "the suite to run, by name"},
				"suites": list("the suites to run, by name; every suite when neither suite nor suites is given"),
				"targets": list("the packages, paths or build targets to run, exported to the suite's command as " +
					SuiteTargetsEnv + " (space separated); the suite's whole set when absent"),
				"tests": list("the test names to run, exported to the suite's command as " + SuiteTestsEnv +
					" (space separated); every test when absent"),
				"shards": map[string]any{"type": "integer", "minimum": 1,
					"description": "with targets or tests, how many of a sharded suite's shards to start: the first shards ones, each still told the suite's declared shard count, so every test keeps its shard; every shard when absent, or without targets or tests. Name it when your brief says how many shards a selection needs"},
				"fail_fast": map[string]any{"type": "boolean", "description": "stop the run at its first failing suite or shard, the others cancelled and named not run"},
				"run_id":    map[string]any{"type": "string", "description": "a running run's id, as an earlier call answered it: wait for that run instead of starting one"},
				"wait_seconds": map[string]any{"type": "integer", "minimum": 0,
					"description": "with run_id, how long to wait for the run before answering it is still running; at most the node's own wait, which is the default"},
			},
		},
		Call: s.call,
	}
}

// strings is a call's list argument, its blank entries dropped and the
// rest trimmed, in order.
func stringsArg(args map[string]any, key string) ([]string, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return nil, nil
	}
	raw, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s is a list of strings", key)
	}
	var out []string
	for _, e := range raw {
		n, ok := e.(string)
		if !ok {
			return nil, fmt.Errorf("%s is a list of strings", key)
		}
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out, nil
}

// selectionArg is the call's selection, nil when it named neither
// targets nor tests. A name holding whitespace could not be told apart
// in its space-separated variable, so it is refused.
func selectionArg(args map[string]any) (*Selection, error) {
	targets, err := stringsArg(args, "targets")
	if err != nil {
		return nil, err
	}
	tests, err := stringsArg(args, "tests")
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 && len(tests) == 0 {
		return nil, nil
	}
	for key, list := range map[string][]string{"targets": targets, "tests": tests} {
		if len(list) > maxSelection {
			return nil, fmt.Errorf("%s names at most %d", key, maxSelection)
		}
		for _, n := range list {
			if strings.ContainsFunc(n, unicode.IsSpace) {
				return nil, fmt.Errorf("%s entry %q holds whitespace: each is one name", key, n)
			}
		}
	}
	return &Selection{Targets: targets, Tests: tests}, nil
}

func (s *RunSuite) call(ctx context.Context, args map[string]any) (string, error) {
	if id, _ := args["run_id"].(string); strings.TrimSpace(id) != "" {
		wait, err := s.waitArg(args)
		if err != nil {
			return "", err
		}
		return s.poll(ctx, strings.TrimSpace(id), wait)
	}
	run := s.Run
	if run == nil {
		if !s.OnHost {
			return "", ErrNoSuiteRunner
		}
		run = HostSuites
	}
	names, err := stringsArg(args, "suites")
	if err != nil {
		return "", err
	}
	if one, _ := args["suite"].(string); strings.TrimSpace(one) != "" && !slices.Contains(names, strings.TrimSpace(one)) {
		names = append([]string{strings.TrimSpace(one)}, names...)
	}
	sel, err := selectionArg(args)
	if err != nil {
		return "", err
	}
	shards, err := shardsArg(args)
	if err != nil {
		return "", err
	}
	failFast := false
	if v, ok := args["fail_fast"]; ok && v != nil {
		if failFast, ok = v.(bool); !ok {
			return "", errors.New("fail_fast is true or false")
		}
	}
	// an explicit validate.yml wins whole; with none, the node infers
	// the suites from the checkout (ADR-053)
	cfg, profile, err := infer.Load(s.Workspace)
	if errors.Is(err, infer.ErrNothingToInfer) {
		return "", fmt.Errorf("the suites cannot run: %w", err)
	}
	if err != nil {
		return "", err
	}
	source := "validate.yml"
	if profile != nil {
		source = "the inferred suites (" + strings.Join(profile.Suites, ", ") + ")"
		if s.Logf != nil {
			s.Logf("run_suite: no validate.yml: %s", profile.Summary())
		}
	}
	steps := cfg.Steps
	if len(names) > 0 {
		steps = nil
		for _, n := range names {
			i := slices.IndexFunc(cfg.Steps, func(st validate.Step) bool { return st.Name == n })
			if i < 0 {
				return "", fmt.Errorf("%s has no suite %q", source, n)
			}
			steps = append(steps, cfg.Steps[i])
		}
	}
	if len(steps) == 0 {
		return "", fmt.Errorf("%s declares no suites", source)
	}
	steps = startShards(steps, shards, sel, s.Logf)
	r, fresh, note := s.startOrJoin(steps, sel)
	if !fresh {
		// a repeat call: the same suites are already running on this
		// lease, so no lane opens and the call answers that run, as a
		// call naming its run_id would
		return s.awaitJoined(ctx, r, s.wait(), note)
	}
	go func() {
		defer close(r.done)
		r.answer, r.err = s.execute(r, run, steps, sel, failFast, profile)
		if s.OnFinished != nil && r.err == nil {
			s.OnFinished(r.answer)
		}
	}()
	return s.await(ctx, r, s.wait())
}

// shardsArg is the call's shards, 0 when it names none.
func shardsArg(args map[string]any) (int, error) {
	v, ok := args["shards"]
	if !ok || v == nil {
		return 0, nil
	}
	f, ok := v.(float64)
	if !ok || f < 1 || f != float64(int(f)) {
		return 0, errors.New("shards is a whole number, at least 1")
	}
	return int(f), nil
}

// startShards is steps with each sharded suite set to start only its
// first k shards (validate.Step.RunShards), each still told the suite's
// declared count: only for a selection, since a whole run that started
// fewer shards would leave tests out unseen. The node admits a
// validation for one lane per sharded suite (validate.AdmitSizes); each
// further shard waits for its own room as it starts and gives it back
// as it ends. k 0, or no selection, starts every shard.
func startShards(steps []validate.Step, k int, sel *Selection, logf func(string, ...any)) []validate.Step {
	if k <= 0 {
		return steps
	}
	if sel == nil {
		if logf != nil {
			logf("run_suite: shards %d ignored: the run names no targets or tests, so every shard runs", k)
		}
		return steps
	}
	out := slices.Clone(steps)
	for i := range out {
		if out[i].Shards > 1 && k < out[i].Shards {
			out[i].RunShards = k
		}
	}
	return out
}

// sameSelection is whether a and b name the same targets and tests,
// in any order; nil names none.
func sameSelection(a, b *Selection) bool {
	norm := func(s *Selection) (targets, tests []string) {
		if s == nil {
			return nil, nil
		}
		targets, tests = slices.Clone(s.Targets), slices.Clone(s.Tests)
		slices.Sort(targets)
		slices.Sort(tests)
		return targets, tests
	}
	at, ax := norm(a)
	bt, bx := norm(b)
	return slices.Equal(at, bt) && slices.Equal(ax, bx)
}

// startOrJoin is run_suite idempotent per lease (ADR-049 amendment
// 2026-11-16, a repeat call answers the running run). Under one lock it
// either registers a fresh run of steps — fresh true — or names the run
// the call joins instead:
//
//   - a run of this lease still going whose suites are the same set and
//     whose targets and tests are the same (fail_fast aside) is the
//     call's own run again: it is joined, note empty;
//   - one that ended but whose end no call has answered yet, for that
//     same selection, is joined too, so a session that lost its handle
//     gets the result rather than a second run;
//   - otherwise a run still going that runs any of the same suites —
//     whatever targets or tests either names, since two runs of one
//     suite would open conflicting lanes — is joined, the oldest first,
//     note saying the call's own selection did not start;
//   - only a call overlapping no running run starts its own.
func (s *RunSuite) startOrJoin(steps []validate.Step, sel *Selection) (r *suiteRun, fresh bool, note string) {
	names := stepNames(steps)
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.runs))
	for id := range s.runs {
		ids = append(ids, id)
	}
	// oldest first: run-N's N is its order
	slices.SortFunc(ids, func(a, b string) int { return s.runs[a].seq() - s.runs[b].seq() })
	same := func(o *suiteRun) bool {
		a, b := slices.Clone(o.suites), slices.Clone(names)
		slices.Sort(a)
		slices.Sort(b)
		return slices.Equal(a, b) && sameSelection(o.sel, sel)
	}
	var overlap []*suiteRun
	for _, id := range ids {
		o := s.runs[id]
		running := !o.ended()
		if same(o) && (running || !o.delivered) {
			return o, false, ""
		}
		if running && slices.ContainsFunc(o.suites, func(n string) bool { return slices.Contains(names, n) }) {
			overlap = append(overlap, o)
		}
	}
	if len(overlap) > 0 {
		o := overlap[0]
		var shared []string
		for _, n := range names {
			if slices.Contains(o.suites, n) {
				shared = append(shared, n)
			}
		}
		note = "started nothing: " + strings.Join(shared, ", ") + " already running on this lease as " + o.id +
			", so this answers that run; once it has ended, call run_suite again to run your own selection"
		if len(overlap) > 1 {
			var others []string
			for _, x := range overlap[1:] {
				others = append(others, x.id)
			}
			note += " (also running: " + strings.Join(others, ", ") + ")"
		}
		return o, false, note
	}
	return s.startLocked(steps, sel), true, ""
}

// ended is whether the run has ended.
func (r *suiteRun) ended() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// seq is the run's order on its lease, the N of run-N.
func (r *suiteRun) seq() int {
	n, _ := strconv.Atoi(strings.TrimPrefix(r.id, "run-"))
	return n
}

// awaitJoined is await for a call that joined a run it did not start:
// its answer is the run's own, with note added to a running handle when
// the call asked for a different selection than the run's.
func (s *RunSuite) awaitJoined(ctx context.Context, r *suiteRun, wait time.Duration, note string) (string, error) {
	ans, err := s.await(ctx, r, wait)
	if err != nil || note == "" {
		return ans, err
	}
	var m map[string]any
	if json.Unmarshal([]byte(ans), &m) != nil || m["status"] != "running" {
		return ans, err
	}
	m["note"] = note
	b, _ := json.Marshal(m)
	return string(b), nil
}

// wait is how long a call waits on its run.
func (s *RunSuite) wait() time.Duration {
	if s.Wait > 0 {
		return s.Wait
	}
	return DefaultSuiteWait
}

// waitArg is a poll's wait_seconds, at most the node's wait.
func (s *RunSuite) waitArg(args map[string]any) (time.Duration, error) {
	v, ok := args["wait_seconds"]
	if !ok || v == nil {
		return s.wait(), nil
	}
	var n float64
	switch x := v.(type) {
	case float64:
		n = x
	case int:
		n = float64(x)
	default:
		n = -1
	}
	if n < 0 {
		return 0, errors.New("wait_seconds is a number of seconds, 0 or more")
	}
	return min(time.Duration(n*float64(time.Second)), s.wait()), nil
}

// startLocked registers a run of steps under a new id; s.mu is held.
func (s *RunSuite) startLocked(steps []validate.Step, sel *Selection) *suiteRun {
	if s.runs == nil {
		s.runs = map[string]*suiteRun{}
	}
	s.n++
	r := &suiteRun{id: "run-" + strconv.Itoa(s.n), suites: stepNames(steps), sel: sel, started: time.Now(), done: make(chan struct{})}
	s.runs[r.id] = r
	return r
}

// find is the run id names, nil when this lease has none by that id.
func (s *RunSuite) find(id string) *suiteRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs[id]
}

// poll waits up to wait for the run id names.
func (s *RunSuite) poll(ctx context.Context, id string, wait time.Duration) (string, error) {
	r := s.find(id)
	if r == nil {
		return "", fmt.Errorf("no run %q on this lease: run_suite answers a run's id when it starts one", id)
	}
	return s.await(ctx, r, wait)
}

// await is the run's answer once it has ended, or its handle when it
// is still going after wait. A call given up on by its caller leaves
// the run going.
func (s *RunSuite) await(ctx context.Context, r *suiteRun, wait time.Duration) (string, error) {
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-r.done:
		return s.delivered(r)
	case <-ctx.Done():
		return "", ctx.Err()
	case <-t.C:
	}
	select {
	case <-r.done:
		return s.delivered(r)
	default:
	}
	handle := map[string]any{"run_id": r.id, "status": "running", "suites": r.suites,
		"started_at": r.started.UTC().Format(time.RFC3339), "elapsed_s": int(time.Since(r.started).Seconds()),
		"next": "the run goes on: call run_suite {\"run_id\": \"" + r.id + "\"} to wait for it again"}
	if d := s.deadline(); !d.IsZero() {
		handle["budget_left_s"] = max(int(time.Until(d).Seconds()), 0)
	}
	if live := r.liveTests(); len(live) > 0 {
		tests, truncated := answerTestsOf(live)
		handle["tests"] = tests
		if truncated {
			handle["tests_truncated"] = true
		}
	}
	b, _ := json.Marshal(handle)
	return string(b), nil
}

// delivered is the ended run's answer, marked as answered to a call.
func (s *RunSuite) delivered(r *suiteRun) (string, error) {
	s.mu.Lock()
	r.delivered = true
	s.mu.Unlock()
	return r.answer, r.err
}

// deadline is when the lease's budget runs out; zero for none.
func (s *RunSuite) deadline() time.Time {
	if s.Budget <= 0 || s.Began.IsZero() {
		return time.Time{}
	}
	return s.Began.Add(s.Budget)
}

// execute runs the steps to their end — or to the lease's budget — and
// records the run: its answer, as the call that sees it end returns it.
func (s *RunSuite) execute(r *suiteRun, run SuiteRunner, steps []validate.Step, sel *Selection, failFast bool, profile *infer.Profile) (string, error) {
	base := s.Context
	if base == nil {
		base = context.Background()
	}
	// reads of the checkout are not bounded by the budget
	readCtx := context.WithoutCancel(base)
	ctx, cancel := context.WithCancel(base)
	defer cancel()
	deadline := s.deadline()
	if !deadline.IsZero() {
		var stop context.CancelFunc
		ctx, stop = context.WithDeadline(ctx, deadline)
		defer stop()
	}
	if failFast {
		ctx = validate.WithFailFast(ctx)
	}
	if s.OnActivity != nil {
		ctx = validate.WithOnActivity(ctx, s.OnActivity)
	}
	ctx = validate.WithOnTest(ctx, func(tr validate.TestResult) {
		if s.OnActivity != nil {
			s.OnActivity("test event")
		}
		r.addLive(TestResult{Package: tr.Package, Test: tr.Name, Action: tr.Action, Elapsed: float64(tr.ElapsedMs) / 1000, Output: tr.Output})
	})
	if sel != nil {
		// a shard the selection leaves empty is no "ran 0 tests"
		ctx = validate.WithSelection(ctx)
	}
	ctx = validate.WithOnLane(ctx, func(name string, stop func(reason string)) {
		r.registerLane(name, stop)
	})
	head, _ := git(readCtx, s.Workspace, "rev-parse", "--verify", "-q", "HEAD^{commit}")
	tree, _ := git(readCtx, s.Workspace, "rev-parse", "--verify", "-q", "HEAD^{tree}")
	// what the checkout holds beyond HEAD is part of what was run
	status, _ := git(readCtx, s.Workspace, "status", "--porcelain=v1", "-uall")
	start := time.Now()
	var results []validate.StepResult
	var runErr error
	if deadline.IsZero() || start.Before(deadline) {
		results, runErr = withBuiltins(run, failFast)(ctx, s.Workspace, steps, SelectionEnv(sel))
	}
	ended := time.Now()
	// out of time: the budget's deadline stopped a run that had not
	// passed whole, and not the lease's own end
	complete := len(results) == len(steps) && !slices.ContainsFunc(results, func(r validate.StepResult) bool { return r.ExitCode != 0 })
	outOfTime := !deadline.IsZero() && errors.Is(ctx.Err(), context.DeadlineExceeded) && base.Err() == nil && !complete
	if runErr != nil && len(results) == 0 && !outOfTime {
		return "", fmt.Errorf("the suites could not run: %v", runErr)
	}
	s.keepLogs(readCtx, results)
	s.noteCoverage(steps, results, start)
	out := Normalise(results)
	out.Inferred = profile
	blob, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	passed, failed, skipped, timedOut, flaky, stalled, exit := 0, 0, 0, 0, 0, 0, 0
	var failing []string
	flakyTests, stalledTests := []string{}, []string{}
	for _, st := range out.Steps {
		if st.Exit != 0 {
			exit = 1
		}
	}
	if len(results) < len(steps) || outOfTime {
		exit = 1
	}
	for _, t := range out.Tests {
		if t.Test == "" {
			continue
		}
		switch t.Action {
		case "pass":
			passed++
		case "fail":
			failed++
			if len(failing) < 20 {
				failing = append(failing, t.Package+"."+t.Test)
			}
		case "skip":
			skipped++
		case "timeout":
			timedOut++
		case validate.ActionFlaky:
			flaky++
			if len(flakyTests) < 20 {
				flakyTests = append(flakyTests, t.Package+"."+t.Test)
			}
		case validate.ActionStalled:
			stalled++
			if len(stalledTests) < 20 {
				stalledTests = append(stalledTests, t.Package+"."+t.Test)
			}
		}
	}
	summary := fmt.Sprintf("%d of %d steps passed; %d tests passed, %d failed, %d skipped", countOK(out.Steps), len(steps), passed, failed, skipped)
	if timedOut > 0 {
		summary += fmt.Sprintf(", %d still running at the timeout", timedOut)
	}
	if flaky > 0 {
		summary += fmt.Sprintf(", %d flaky (failed, then passed when the node ran them again)", flaky)
	}
	if stalled > 0 {
		summary += fmt.Sprintf(", %d stalled", stalled)
	}
	if hung := hungTestsOf(results); len(hung) > 0 {
		summary += fmt.Sprintf("; hung (go test's timeout, twice): %s", strings.Join(hung, ", "))
	}
	for _, st := range out.Steps {
		if st.Reason != "" {
			summary += fmt.Sprintf("; %s %s", st.Name, st.Reason)
		}
	}
	if sel != nil {
		summary += fmt.Sprintf(" (a selection: %d targets, %d tests)", len(sel.Targets), len(sel.Tests))
	}
	if profile != nil {
		summary = "no .runsheet/validate.yml: " + profile.Summary() + "; " + summary
	}
	if outOfTime {
		summary = fmt.Sprintf("ran out of time: the lease ran past the repository's validation budget of %s (validate.yml budget:) and the run was stopped; ",
			s.Budget) + summary
	}
	input := map[string]any{"suites": stepNames(steps), "head": strings.TrimSpace(head), "tree": strings.TrimSpace(tree), "status": Digest([]byte(status))}
	if sel != nil {
		// two selections of one suite are two inputs
		input["selection"] = sel
	}
	if profile != nil {
		// an inferred run is its inferred file's, as a declared one is its file's
		input["inferred"] = profile.SuiteHash
	}
	rec := Record{Tool: RunSuiteTool, StartedAt: start, EndedAt: ended, Exit: exit, Summary: summary,
		InputDigest: InputDigest(input), Selection: sel}
	var id string
	var recErr error
	outputDigest := Digest(blob)
	if s.Results.UploadURL != "" {
		var up rawresults.Result
		up, recErr = rawresults.Upload(readCtx, s.Results, blob, rawresults.Options{Session: s.Session})
		if recErr == nil {
			outputDigest = up.Digest
			id, recErr = s.Recorder.RecordUploaded(readCtx, rec, up.Digest)
		}
	} else {
		id, recErr = s.Recorder.Record(readCtx, rec, blob)
	}
	answerTests, testsTruncated := answerTestsOf(out.Tests)
	answer := map[string]any{"run_id": r.id, "status": "finished", "exit": exit, "summary": summary, "steps": out.Steps, "tests": answerTests, "failing_tests": failing,
		"head": strings.TrimSpace(head), "tree": strings.TrimSpace(tree), "uncommitted": strings.TrimSpace(status) != "", "output_digest": outputDigest}
	if s.Results.UploadURL != "" {
		answer["output_location"] = s.Results.UploadURL
	}
	if testsTruncated {
		answer["tests_truncated"] = true
	}
	if profile != nil {
		answer["inferred"] = profile
	}
	if len(flakyTests) > 0 {
		answer["flaky_tests"] = flakyTests
	}
	if len(stalledTests) > 0 {
		answer["stalled_tests"] = stalledTests
	}
	if hung := hungTestsOf(results); len(hung) > 0 {
		answer["hung_tests"] = hung
	}
	if sel != nil {
		answer["selection"] = sel
	}
	if outOfTime {
		answer["out_of_time"] = true
		answer["budget"] = s.Budget.String()
	} else if runErr != nil {
		answer["error"] = runErr.Error()
	}
	if recErr != nil {
		answer["evidence_error"] = recErr.Error()
	} else {
		answer["evidence_id"] = id
	}
	if exit != 0 && !outOfTime && s.Escalate != nil {
		if to := s.Escalate("run_suite went red: " + summary); to != "" {
			answer["escalated_to"] = to
		}
	}
	b, _ := json.Marshal(answer)
	return string(b), nil
}

// Normalise is the steps' outcomes and, from each step's go test -json
// report, every test's and package's final event (pass, fail or skip):
// run_suite's output blob. What the node's supervisor found is carried
// whole — a flaky or stalled test's action and its retries, a step's
// flaky count and its reason — so the signed record's blob says it.
func Normalise(results []validate.StepResult) SuiteOutput {
	out := SuiteOutput{Steps: []SuiteStep{}, Tests: []TestResult{}}
	for _, r := range results {
		out.Steps = append(out.Steps, SuiteStep{Name: r.Name, Command: r.Command, Exit: r.ExitCode, DurationMs: r.DurationMs,
			TestsRun: r.TestsRun, TestsPassed: r.TestsPassed, TestsFailed: r.TestsFailed, TestsFlaky: r.TestsFlaky, Shards: shardsOf(r.Shards),
			Stopped: r.Stopped, StopReason: r.StopReason, Reason: r.Reason,
			Hung: r.Hung, HungTests: r.HungTests, Rerun: r.Rerun, LogEvidence: r.LogEvidence, LogDigest: r.LogDigest})
		if len(r.Tests) > 0 {
			out.Tests = append(out.Tests, testResultsFrom(r.Tests)...)
		} else {
			out.Tests = append(out.Tests, goTestEvents(r.Report)...)
		}
	}
	return out
}

// testResultsFrom is a step's own tests (validate.TestResult, read from
// the full untruncated go test -json stream, a test still running at a
// timeout named Action "timeout") as the blob's TestResult: preferred
// over goTestEvents' Report reparse whenever the step carries them,
// since they carry Output and timeouts the capped Report may not.
func testResultsFrom(tests []validate.TestResult) []TestResult {
	out := make([]TestResult, len(tests))
	for i, t := range tests {
		out[i] = TestResult{Package: t.Package, Test: t.Name, Action: t.Action, Elapsed: float64(t.ElapsedMs) / 1000, Output: t.Output, Retries: t.Retries}
	}
	return out
}

// shardsOf are a sharded step's shards in index order, each one that
// never ran named not_run; nil for a step with no shards.
func shardsOf(shards []validate.ShardResult) []SuiteShard {
	if len(shards) == 0 || shards[0].Total <= 1 {
		return nil
	}
	total := shards[0].Total
	out := make([]SuiteShard, total)
	for k := range out {
		out[k] = SuiteShard{Index: k, Total: total, Exit: -1, NotRun: true}
	}
	for _, sh := range shards {
		if sh.Index < 0 || sh.Index >= total {
			continue
		}
		out[sh.Index] = SuiteShard{Index: sh.Index, Total: total, Exit: sh.ExitCode, DurationMs: sh.DurationMs,
			TestsRun: sh.TestsRun, TestsPassed: sh.TestsPassed, TestsFailed: sh.TestsFailed, TestsFlaky: sh.TestsFlaky,
			Stopped: sh.Stopped, StopReason: sh.StopReason, Reason: sh.Reason,
			Hung: sh.Hung, HungTests: sh.HungTests, Rerun: sh.Rerun, LogEvidence: sh.LogEvidence, LogDigest: sh.LogDigest}
	}
	return out
}

// goTestEvents are a go test -json stream's final events, in order;
// any other line is passed over.
func goTestEvents(report string) []TestResult {
	var out []TestResult
	sc := bufio.NewScanner(strings.NewReader(report))
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev struct {
			Action  string
			Package string
			Test    string
			Elapsed float64
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		switch ev.Action {
		case "pass", "fail", "skip":
			out = append(out, TestResult{Package: ev.Package, Test: ev.Test, Action: ev.Action, Elapsed: ev.Elapsed})
		}
	}
	return out
}

func countOK(steps []SuiteStep) int {
	n := 0
	for _, s := range steps {
		if s.Exit == 0 {
			n++
		}
	}
	return n
}

func stepNames(steps []validate.Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Name
	}
	return out
}

// CoverageReport is the report file of runsheet's e2ecoverage suite:
// main's per-test e2e coverage map (runsheet ADR 032, amended
// 2026-11-20), carried on the validate result's "coverage" key.
const CoverageReport = "e2e-coverage.json"

// MaxCoverage is the most of a coverage map the node carries: the
// attestation's report bound (validate's reportCap). A step's report is
// cut there, so a report this long may be a cut map and is skipped.
const MaxCoverage = 256 * 1024

// noteCoverage hands OnCoverage the map each step that names
// CoverageReport as its only report wrote. A step whose report is its
// results (a results: location) is not a coverage step.
//
// The map is read whole from the checkout, when the step wrote it in
// this run (its file changed since start): the step's Report is cut to
// what the run's earlier steps left of the report bound (validate's
// reportCap), and on main's validation, where the e2ecoverage suite runs
// after unit's and integration's reports, that left a cut map, never
// carried (dl360, 2026-11-23: "not carried" on every main validation
// since #1946, so runsheet had no coverage map and every validation ran
// all four e2e shards). The Report is the fallback for a runner that
// does not write to the checkout.
func (s *RunSuite) noteCoverage(steps []validate.Step, results []validate.StepResult, start time.Time) {
	if s.OnCoverage == nil {
		return
	}
	for _, r := range results {
		i := slices.IndexFunc(steps, func(st validate.Step) bool { return st.Name == r.Name })
		if i < 0 {
			continue
		}
		st := steps[i]
		if st.Results != "" || len(st.Reports) != 1 || st.Reports[0] != CoverageReport {
			continue
		}
		report := r.Report
		if s.Workspace != "" {
			path := filepath.Join(s.Workspace, CoverageReport)
			if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() && !fi.ModTime().Before(start) {
				// one at or past the bound is read only that far:
				// coverageContent then names it too long to carry
				if data, err := readAtMost(path, MaxCoverage); err == nil {
					report = string(data)
				}
			}
		}
		content, why := coverageContent(report)
		if why != "" {
			if s.Logf != nil {
				s.Logf("run_suite: step %s: the e2e coverage map is not carried: %s", r.Name, why)
			}
			continue
		}
		if content != "" {
			if s.Logf != nil {
				s.Logf("run_suite: step %s: the e2e coverage map is carried (%d bytes)", r.Name, len(content))
			}
			s.OnCoverage(content)
		}
	}
}

// readAtMost is the first n bytes of the file at path, or all of a
// shorter one.
func readAtMost(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, n))
}

// coverageContent is report as the coverage map the node carries, ""
// for none written, or why it is skipped.
func coverageContent(report string) (content, why string) {
	report = strings.TrimSpace(report)
	switch {
	case report == "":
		return "", ""
	case len(report) >= MaxCoverage:
		return "", fmt.Sprintf("%d bytes reaches the %d-byte report bound, so it may be cut", len(report), MaxCoverage)
	case json.Valid([]byte(report)), encodedCoverage(report):
		return report, ""
	}
	return "", "it is neither JSON nor a " + coverageEncoding + " map"
}

// coverageEncoding is the prefix of runsheet's encoded coverage map
// (platform/coverage's Encode: gzip, then base64, after it), which is
// what its e2ecoverage suite writes: the node carries it as written,
// a JSON string on the result, and the platform decodes it.
const coverageEncoding = "gz64:"

// encodedCoverage says whether report is a whole encoded map: the
// prefix and a base64 body that decodes, a cut one failing to.
func encodedCoverage(report string) bool {
	body, ok := strings.CutPrefix(report, coverageEncoding)
	if !ok || body == "" {
		return false
	}
	_, err := base64.StdEncoding.DecodeString(body)
	return err == nil
}
