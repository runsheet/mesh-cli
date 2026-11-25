package nodev1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"

	"projectmesh.io/mesh-cli/internal/mcptool"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/nodetools"
	"projectmesh.io/mesh-cli/internal/validate"
	"projectmesh.io/mesh-cli/internal/validate/infer"
)

// A validation the node runs itself (ADR-044 amendment, 2026-11-25): a
// validate lease needs no judgment to build its candidate, push it, run
// its suites and say what they did, so the node does all four with the
// very tools a session would have called — apply_onto, push and
// run_suite, built for the lease as nodeTools builds them, on its pod,
// relay and recorder — and writes the finish itself. No model is asked
// to remember a step: a chain's validation can no longer be refused for
// naming no candidate, or for a candidate that does not hold.
//
// The result is the machine's: green is every run's exit, candidate is
// apply_onto's commit and tree, suites and tests are run_suite's own
// answers, evidence every record the lease's tools signed, and a red's
// triage the node's own reading of those answers (machineTriage). It is
// bound to the commit the suites ran on: a run that answers another head
// or tree than the candidate's, or a checkout whose tree moved by the
// time the node writes the finish, is no result at all (errTreeMoved).

// errTreeMoved is a machine validation whose suites did not run on the
// candidate it built, or whose checkout no longer holds it: the lease is
// handed back, never finished with a result of another tree.
var errTreeMoved = errors.New("the candidate's tree changed between build and finish")

// machineOnlyTools are the node tools a machine validation calls
// itself: a session on such a lease — a red's diagnosis — is never
// served them, so it cannot build, push or run anything of its own.
var machineOnlyTools = []string{nodetools.ApplyOntoTool, nodetools.PushTool, nodetools.RunSuiteTool, nodetools.StopSuiteTool}

// machineValidates is whether the node runs b itself rather than a
// session: a validation kind naming run_suite, one-shot (no thread, no
// dispatcher), on a node not told to keep validations in sessions
// (ValidateWithSession, `--validate-with-session`).
func (r *Runtime) machineValidates(b *meshapi.Brief) bool {
	return !r.ValidateWithSession && validationKind(b) && slices.Contains(b.NodeTools, nodetools.RunSuiteTool) &&
		b.Thread == nil && !isDispatcher(b)
}

// withoutMachineTools is tools less machineOnlyTools.
func withoutMachineTools(tools []mcptool.Tool) []mcptool.Tool {
	out := make([]mcptool.Tool, 0, len(tools))
	for _, t := range tools {
		if !slices.Contains(machineOnlyTools, t.Name) {
			out = append(out, t)
		}
	}
	return out
}

// validateBlock is the part of the product's brief a machine validation
// reads (runsheet's contextapi briefValidate): the trees already proven
// green, the e2e tests the change runs, and the tests in quarantine.
// Every field is optional; a brief the node cannot read runs every
// suite whole, which is never fewer tests than the brief would ask.
type validateBlock struct {
	Proven []struct {
		Tree   string `json:"tree"`
		Commit string `json:"commit"`
	} `json:"proven"`
	E2E *struct {
		All    bool     `json:"all"`
		Tests  []string `json:"tests"`
		Shards int      `json:"shards"`
	} `json:"e2e"`
	KnownFlaky []string `json:"known_flaky"`
}

// e2eSuite is the suite runsheet's e2e selection narrows (its
// validate.md guidance, step 5a).
const e2eSuite = "integration"

// readValidateBrief is the brief's validate block, read from the
// product's brief tool on the lease's proxy as a session would, nil when
// there is none or it cannot be read.
func (r *Runtime) readValidateBrief(ctx context.Context, b *meshapi.Brief, sock string) *validateBlock {
	server := productServer(b)
	if b.BriefTool == "" || sock == "" || server == "" {
		return nil
	}
	tool := b.BriefTool
	if i := strings.LastIndex(tool, "__"); i >= 0 && strings.HasPrefix(tool, "mcp__") {
		tool = tool[i+2:]
	}
	ctx, cancel := context.WithTimeout(ctx, reportTimeout)
	defer cancel()
	text, err := productToolText(ctx, sock, server, tool, json.RawMessage(`{}`))
	if err != nil {
		r.logf("nodev1: lease %s: reading the brief's validate block from %s: %v; every suite runs whole", b.LeaseID, b.BriefTool, err)
		return nil
	}
	var out struct {
		Validate *validateBlock `json:"validate"`
	}
	if json.Unmarshal([]byte(text), &out) != nil {
		return nil
	}
	return out.Validate
}

// machineValidation is a validation the node ran itself.
type machineValidation struct {
	// finish is the result, every key the kind's finish_schema allows
	finish map[string]any
	// green is the result's colour; answers are run_suite's answers,
	// as the run ended, what a red's diagnosis reads
	green   bool
	answers []string
}

// suiteAnswer is the part of run_suite's answer the machine reads.
type suiteAnswer struct {
	Status  string `json:"status"`
	RunID   string `json:"run_id"`
	Exit    int    `json:"exit"`
	Summary string `json:"summary"`
	Head    string `json:"head"`
	Tree    string `json:"tree"`
	Steps   []struct {
		Name        string `json:"name"`
		Exit        int    `json:"exit"`
		DurationMs  int64  `json:"duration_ms"`
		TestsPassed int    `json:"tests_passed"`
		TestsFailed int    `json:"tests_failed"`
		Hung        bool   `json:"hung"`
		LogEvidence string `json:"log_evidence"`
	} `json:"steps"`
	Tests []struct {
		Name       string `json:"name"`
		Package    string `json:"package"`
		Outcome    string `json:"outcome"`
		DurationMs int64  `json:"duration_ms"`
		Output     string `json:"output"`
	} `json:"tests"`
	FlakyTests []string `json:"flaky_tests"`
	HungTests  []string `json:"hung_tests"`
	OutOfTime  bool     `json:"out_of_time"`
	Error      string   `json:"error"`
	Evidence   string   `json:"evidence_error"`

	// suite is the one suite the call named, "" for a call of several
	suite string
	raw   string
}

// suiteCall is one run_suite call the machine makes.
type suiteCall struct {
	args  map[string]any
	suite string
}

// validateByMachine runs b's validation with tools — the lease's node
// tools by name, as nodeTools built them — and answers its result: the
// candidate built (apply_onto) when the brief names changes to apply
// onto a base, pushed to the brief's push_to when it names one, a tree
// already proven reused, otherwise every suite run (run_suite) and the
// result written from what they answered. sock is the lease's proxy, on
// which the product's brief is read. An error is a validation that
// could not run, or errTreeMoved; the lease is then handed back.
func (r *Runtime) validateByMachine(ctx context.Context, b *meshapi.Brief, tools map[string]mcptool.Tool, rec *nodetools.Recorder, sock, workspace string) (*machineValidation, error) {
	evidence := func() []string {
		ids := rec.IDs()
		if ids == nil {
			ids = []string{}
		}
		return ids
	}
	vb := r.readValidateBrief(ctx, b, sock)
	if vb == nil {
		vb = &validateBlock{}
	}
	r.logf("nodev1: lease %s: the node validates it itself, no session (ADR-044 amendment 2026-11-25)", b.LeaseID)

	// (1) the candidate: the brief's changes applied onto its base, or
	// the checkout as it stands
	var sha, tree string
	applied := false
	if b.Code != nil && b.Code.Onto != nil && len(b.Code.Apply) > 0 {
		apply, ok := tools[nodetools.ApplyOntoTool]
		if !ok {
			return nil, &toolUnavailableError{tool: nodetools.ApplyOntoTool, why: "the brief names changes to apply onto a base, and the kind no apply_onto"}
		}
		out, err := apply.Call(ctx, map[string]any{})
		if err != nil {
			return nil, fmt.Errorf("apply_onto: %w", err)
		}
		var ans struct {
			SHA      string          `json:"sha"`
			Tree     string          `json:"tree"`
			Conflict json.RawMessage `json:"conflict"`
		}
		if err := json.Unmarshal([]byte(out), &ans); err != nil {
			return nil, fmt.Errorf("apply_onto answered %q: %w", clip(out, 200), err)
		}
		if len(ans.Conflict) > 0 && string(ans.Conflict) != "null" {
			// no red: nothing is pushed and nothing runs
			var conflict map[string]any
			_ = json.Unmarshal(ans.Conflict, &conflict)
			return &machineValidation{finish: map[string]any{"green": false, "evidence": evidence(), "conflict": conflict, "learnings": []any{}}}, nil
		}
		sha, tree, applied = ans.SHA, ans.Tree, true
	}
	git := leaseGitRun(ctx)
	if sha == "" {
		var err error
		if sha, err = git(workspace, "rev-parse", "--verify", "-q", "HEAD^{commit}"); err != nil {
			return nil, fmt.Errorf("the checkout's head: %w", err)
		}
		if tree, err = git(workspace, "rev-parse", "--verify", "-q", "HEAD^{tree}"); err != nil {
			return nil, fmt.Errorf("the checkout's tree: %w", err)
		}
	}

	// (2) the push: a chain's candidate, to the one ref the brief names
	pushTo := ""
	if b.Code != nil {
		pushTo = b.Code.PushTo
	}
	named := applied && pushTo != ""
	if named {
		push, ok := tools[nodetools.PushTool]
		if !ok {
			return nil, &toolUnavailableError{tool: nodetools.PushTool, why: "the brief names a push_to, and the kind no push"}
		}
		if _, err := push.Call(ctx, map[string]any{"ref": pushTo}); err != nil {
			// a candidate branch an earlier attempt left at another commit
			// is replaced from where the job found it (gitrelay's lease)
			r.logf("nodev1: lease %s: pushing the candidate to %s: %v; pushing again under the lease of where the job found it", b.LeaseID, pushTo, err)
			if _, err2 := push.Call(ctx, map[string]any{"ref": pushTo, "lease": "head"}); err2 != nil {
				return nil, fmt.Errorf("pushing the candidate to %s: %w", pushTo, err2)
			}
		}
	}
	candidate := map[string]any{"sha": sha, "tree": tree}

	// (3) a tree already proven green runs nothing (runsheet's reuse)
	for _, p := range vb.Proven {
		if named && p.Tree != "" && p.Tree == tree && p.Commit != "" {
			r.logf("nodev1: lease %s: the candidate's tree %s is proven green by %s: no suite runs", b.LeaseID, short(tree), short(p.Commit))
			return &machineValidation{green: true, finish: map[string]any{"green": true, "evidence": evidence(), "candidate": candidate,
				"reused": map[string]any{"commit": p.Commit, "tree": p.Tree}, "learnings": []any{}}}, nil
		}
	}

	// (4) every suite, through run_suite
	rs, ok := tools[nodetools.RunSuiteTool]
	if !ok {
		return nil, &toolUnavailableError{tool: nodetools.RunSuiteTool, why: "the kind names no run_suite"}
	}
	cfg, _, cfgErr := infer.Load(workspace)
	calls := suiteCalls(cfg, cfgErr, vb)
	answers, err := r.runSuites(ctx, rs, calls)
	if err != nil {
		return nil, err
	}
	for _, a := range answers {
		if a.Head != sha || a.Tree != tree {
			return nil, fmt.Errorf("%w: run_suite ran on %s (tree %s), not the candidate %s (tree %s)", errTreeMoved, short(a.Head), short(a.Tree), short(sha), short(tree))
		}
	}
	// the checkout still holds the tree the suites ran on: a suite that
	// committed or checked out another tree leaves no result
	now, err := git(workspace, "rev-parse", "--verify", "-q", "HEAD^{tree}")
	if err != nil {
		return nil, fmt.Errorf("the checkout's tree at the finish: %w", err)
	}
	if now != tree {
		return nil, fmt.Errorf("%w: the checkout holds tree %s at the finish, not the candidate's %s", errTreeMoved, short(now), short(tree))
	}

	m := &machineValidation{green: true}
	suites, tests, flaked := []any{}, []any{}, []string{}
	for _, a := range answers {
		m.answers = append(m.answers, a.raw)
		if a.Exit != 0 {
			m.green = false
		}
		for _, st := range a.Steps {
			suites = append(suites, map[string]any{"name": st.Name, "exit": st.Exit, "passed": st.TestsPassed, "failed": st.TestsFailed, "duration_ms": st.DurationMs})
		}
		for _, t := range a.Tests {
			// a passing (or skipped, or in the end flaky) test is counted
			// in its suite's passed and kept in run_suite's evidence, never
			// listed: runsheet's suite ran 9,500 of them, and the list
			// passed Mesh's 1 MB body limit (2026-11-26)
			if o := schemaOutcome(t.Outcome); o != "fail" && o != "timeout" {
				continue
			}
			entry := map[string]any{"name": t.Name, "package": t.Package, "outcome": schemaOutcome(t.Outcome), "duration_ms": max(t.DurationMs, 0)}
			if a.suite != "" {
				entry["suite"] = a.suite
			}
			tests = append(tests, entry)
		}
		flaked = append(flaked, a.FlakyTests...)
	}
	m.finish = map[string]any{"green": m.green, "evidence": evidence(), "suites": suites, "tests": tests, "learnings": []any{}}
	if named {
		m.finish["candidate"] = candidate
	}
	if len(flaked) > 0 {
		m.finish["flaked"] = flaked
	}
	if !m.green {
		m.finish["triage"] = machineTriage(answers, vb.KnownFlaky)
	}
	if cfgErr == nil {
		m.finish["suite_config_sha"] = cfg.Hash()
	}
	m.finish["environment"] = r.validateEnvironment()
	return m, nil
}

// suiteCalls are the run_suite calls a validation makes: one for every
// suite, or — when the brief's e2e block selects tests and the
// checkout declares runsheet's integration suite — one for every other
// suite and one for the integration suite's selected tests alone, on
// the shards they need (none when it selects no test).
func suiteCalls(cfg validate.Config, cfgErr error, vb *validateBlock) []suiteCall {
	all := []suiteCall{{args: map[string]any{}}}
	if cfgErr != nil || vb.E2E == nil || vb.E2E.All {
		return all
	}
	var others []any
	has := false
	for _, st := range cfg.Steps {
		if st.Name == e2eSuite {
			has = true
			continue
		}
		others = append(others, st.Name)
	}
	if !has {
		return all
	}
	var calls []suiteCall
	if len(others) > 0 {
		calls = append(calls, suiteCall{args: map[string]any{"suites": others}})
	}
	if len(vb.E2E.Tests) > 0 {
		targets := make([]any, 0, len(vb.E2E.Tests))
		for _, t := range vb.E2E.Tests {
			targets = append(targets, "e2e:"+t)
		}
		args := map[string]any{"suites": []any{e2eSuite}, "targets": targets}
		if vb.E2E.Shards > 0 {
			args["shards"] = vb.E2E.Shards
		}
		calls = append(calls, suiteCall{args: args, suite: e2eSuite})
	}
	if len(calls) == 0 {
		return all
	}
	return calls
}

// runSuites makes each call at once, each waited on to its end, and
// answers them in the calls' order.
func (r *Runtime) runSuites(ctx context.Context, rs mcptool.Tool, calls []suiteCall) ([]suiteAnswer, error) {
	out := make([]suiteAnswer, len(calls))
	errs := make([]error, len(calls))
	var wg sync.WaitGroup
	for i, c := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i], errs[i] = awaitSuite(ctx, rs, c)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}

// awaitSuite is one run_suite call, polled by its run_id until the run
// has finished.
func awaitSuite(ctx context.Context, rs mcptool.Tool, c suiteCall) (suiteAnswer, error) {
	text, err := rs.Call(ctx, c.args)
	for {
		if err != nil {
			return suiteAnswer{}, fmt.Errorf("run_suite: %w", err)
		}
		var a suiteAnswer
		if err := json.Unmarshal([]byte(text), &a); err != nil {
			return suiteAnswer{}, fmt.Errorf("run_suite answered %q: %w", clip(text, 200), err)
		}
		if a.Status != "running" {
			a.suite, a.raw = c.suite, text
			return a, nil
		}
		if a.RunID == "" {
			return suiteAnswer{}, errors.New("run_suite answered a running run with no run_id")
		}
		if ctx.Err() != nil {
			return suiteAnswer{}, ctx.Err()
		}
		text, err = rs.Call(ctx, map[string]any{"run_id": a.RunID})
	}
}

// schemaOutcome is run_suite's outcome as the validate result's tests
// name it: pass, fail, skip or timeout — a flaky test passed in the end,
// and a stalled one was still running when its unit was stopped.
func schemaOutcome(o string) string {
	switch o {
	case "flaky":
		return "pass"
	case "stalled":
		return "timeout"
	}
	return o
}

// machineTriage is a red's triage as the node reads run_suite's
// answers, with no model: hung when a step hung twice (its tests and its
// goroutine dump named), infra when a run ran out of the repository's
// budget or could not run its steps, flake when every failing test is
// in quarantine (knownFlaky), and otherwise code, naming every failing
// test as "<package> <test>". A red's diagnosis may add to it; it never
// changes the result's colour.
func machineTriage(answers []suiteAnswer, knownFlaky []string) map[string]any {
	var failing, hungTests, summaries []string
	dump := ""
	hung, outOfTime, broken := false, false, false
	for _, a := range answers {
		if a.Exit != 0 && a.Summary != "" {
			summaries = append(summaries, a.Summary)
		}
		for _, t := range a.Tests {
			if t.Outcome == "fail" || t.Outcome == "timeout" {
				name := t.Name
				if t.Package != "" {
					name = t.Package + " " + t.Name
				}
				if !slices.Contains(failing, name) {
					failing = append(failing, name)
				}
			}
		}
		for _, st := range a.Steps {
			if st.Hung {
				hung = true
				if dump == "" {
					dump = st.LogEvidence
				}
			}
		}
		hungTests = append(hungTests, a.HungTests...)
		outOfTime = outOfTime || a.OutOfTime
		broken = broken || (a.Error != "" && a.Exit != 0)
	}
	if failing == nil {
		failing = []string{}
	}
	note := clip(strings.Join(summaries, " | "), 4000)
	t := map[string]any{"class": "code", "failing_tests": failing, "note": note}
	switch {
	case hung:
		t["class"], t["hung_tests"] = "hung", hungTests
		if dump != "" {
			t["dump"] = dump
		}
	case outOfTime:
		t["class"] = "infra"
	case broken && len(failing) == 0:
		t["class"] = "infra"
	case len(failing) > 0 && allKnownFlaky(failing, knownFlaky):
		t["class"] = "flake"
	}
	return t
}

// allKnownFlaky is whether every failing test ("<package> <test>") is
// one of known ("<package> <test>", "<package>.<test>" or the bare
// name).
func allKnownFlaky(failing, known []string) bool {
	if len(known) == 0 {
		return false
	}
	for _, f := range failing {
		pkg, name, _ := strings.Cut(f, " ")
		if name == "" {
			name, pkg = pkg, ""
		}
		if !slices.Contains(known, f) && !slices.Contains(known, pkg+"."+name) && !slices.Contains(known, name) {
			return false
		}
	}
	return true
}

// validateEnvironment is where the node ran the suites: its release,
// and the pod image the suites ran in, or the host's name for a node
// that runs them on the host.
func (r *Runtime) validateEnvironment() map[string]any {
	env := map[string]any{"release": r.Node.Version}
	if r.Pods != nil && r.PodImage != "" {
		env["image"] = r.PodImage
	} else {
		host, _ := os.Hostname()
		env["image"] = "host:" + host
	}
	return env
}

// finishFor is m's finish as the kind's finish_schema takes it: a key
// the schema's properties do not name is left out when the schema
// closes its object (additionalProperties false), so a product whose
// schema has not yet named suite_config_sha or environment still takes
// the result.
func finishFor(schema json.RawMessage, finish map[string]any) (json.RawMessage, error) {
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Additional *bool                      `json:"additionalProperties"`
	}
	if len(schema) > 0 && json.Unmarshal(schema, &s) == nil && s.Additional != nil && !*s.Additional {
		kept := map[string]any{}
		for k, v := range finish {
			if _, ok := s.Properties[k]; ok {
				kept[k] = v
			}
		}
		finish = kept
	}
	return boundFinish(finish, maxMachineFinish)
}

// maxMachineFinish is the most a machine validation's result may
// marshal to: Mesh refuses a finish body over 1 MiB, and the body also
// carries the evidence ids and, on main's validation, an e2e coverage
// map of up to nodetools.MaxCoverage added after this bound.
const maxMachineFinish = 512 << 10

// boundFinish is finish marshaled within max bytes, never refused: the
// failing tests' entries (tests), the triage's failing_tests and flaked
// are each halved in turn until it fits, and the triage's note says how many
// were left out — run_suite's signed evidence still holds every one.
// green, candidate, suites, evidence and the rest are never cut; a
// result that still does not fit is answered as it stands.
func boundFinish(finish map[string]any, max int) (json.RawMessage, error) {
	raw, err := json.Marshal(finish)
	if err != nil || len(raw) <= max {
		return raw, err
	}
	finish = maps.Clone(finish)
	triage, _ := finish["triage"].(map[string]any)
	if triage != nil {
		triage = maps.Clone(triage)
		finish["triage"] = triage
	}
	cut := 0
	halve := func(get func() int, set func(n int)) bool {
		n := get()
		if n == 0 {
			return false
		}
		keep := n / 2
		cut += n - keep
		set(keep)
		return true
	}
	lists := []struct {
		get func() int
		set func(n int)
	}{
		{func() int { l, _ := finish["tests"].([]any); return len(l) }, func(n int) { finish["tests"] = finish["tests"].([]any)[:n] }},
		{func() int {
			if triage == nil {
				return 0
			}
			l, _ := triage["failing_tests"].([]string)
			return len(l)
		}, func(n int) { triage["failing_tests"] = triage["failing_tests"].([]string)[:n] }},
		{func() int { l, _ := finish["flaked"].([]string); return len(l) }, func(n int) { finish["flaked"] = finish["flaked"].([]string)[:n] }},
	}
	// each list halved in turn, so no one of them is emptied while
	// another still runs long
	for len(raw) > max {
		cutAny := false
		for _, l := range lists {
			if len(raw) <= max || !halve(l.get, l.set) {
				continue
			}
			cutAny = true
			if raw, err = json.Marshal(finish); err != nil {
				return nil, err
			}
		}
		if !cutAny {
			break
		}
	}
	if cut > 0 && triage != nil {
		note, _ := triage["note"].(string)
		triage["note"] = strings.TrimSpace(fmt.Sprintf("[mesh: %d test names left out to keep the finish under Mesh's 1 MB body limit; run_suite's evidence holds them all] %s", cut, note))
		raw, err = json.Marshal(finish)
	}
	return raw, err
}

// A red's diagnosis (ADR-044 amendment 2026-11-25): the result is frozen
// the moment the node has written it, and only then does one session —
// on the kind's own harness and its cheapest model, the ladder's
// default — read the red's evidence and say why it is red. Its one tool
// is the node's result, taking {diagnosis}; it is served none of the
// node's building tools (machineOnlyTools) and none of the product's
// servers. What it says is added to the result as diagnosis, and to the
// triage's note; the colour, the candidate, the suites, the tests and
// the evidence are the node's, byte for byte, whatever the session does
// — and a session that errors, stops or says nothing leaves the result
// as the node made it. No second session is ever started.

// diagnosisSchema is the diagnosis session's finish: one string.
var diagnosisSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["diagnosis"],"properties":{"diagnosis":{"type":"string","minLength":1,"description":"why the validation is red: for each failing test, what its output shows and whether the change, main, a flake or the infrastructure is to blame"}}}`)

// diagnoseBrief is b as its diagnosis session works to it: no product
// tool to call first or to finish with, no learnings tool, no evidence
// owed, and the node's result, taking diagnosisSchema, as its one tool.
func diagnoseBrief(b *meshapi.Brief) *meshapi.Brief {
	d := *b
	d.BriefTool, d.FinishTool, d.LearningsTool, d.TurnTool = "", "", "", ""
	d.FinishSchema = diagnosisSchema
	d.NodeTools = []string{nodetools.ResultTool}
	d.EvidenceRequired, d.WaivedBy = nil, nil
	return &d
}

// diagnoses is whether the node starts a session to diagnose mv: a red
// with a triage (never a conflict or a reuse), on a kind naming a
// harness, on a node that diagnoses (NoValidateDiagnosis unset).
func (r *Runtime) diagnoses(b *meshapi.Brief, mv *machineValidation) bool {
	if r.NoValidateDiagnosis || mv == nil || mv.green || b.Harness.Name == "" || b.Harness.Name == "none" {
		return false
	}
	_, red := mv.finish["triage"]
	return red
}

// onlyNodeTools is a session's MCP servers less every product server:
// the diagnosis reaches the node's own tools alone.
func onlyNodeTools(servers map[string]any) map[string]any {
	out := map[string]any{}
	if s, ok := servers[NodeToolsServer]; ok {
		out[NodeToolsServer] = s
	}
	return out
}

// diagnosisEvidenceCap bounds the failing output the diagnosis prompt
// carries.
const diagnosisEvidenceCap = 24 * 1024

// diagnosePrompt is the diagnosis session's prompt: what the node ran
// and found — each red step's summary, and each failing test with its
// output — and what to answer.
func diagnosePrompt(b *meshapi.Brief, mv *machineValidation) string {
	var sb strings.Builder
	repo := ""
	if b.Git != nil {
		repo = b.Git.Repo
	}
	fmt.Fprintf(&sb, "You are diagnosing a red validation (job %s, %s %s, repository %s). ", b.JobID, b.Subject.Type, b.Subject.ID, repo)
	sb.WriteString("The node has already built the commit checked out in your workspace, run every suite on it and written the result: it is red, and nothing you do changes that. ")
	sb.WriteString("Read the failing tests' output below and the code they exercise in your checkout, and say why the validation is red: for each failing test, what its output shows and whether the change, main itself, a flake or the infrastructure is to blame. ")
	sb.WriteString("Do not edit, build, push or run anything: you have no tool for it, and the evidence is below.\n\n")
	if t, ok := mv.finish["triage"].(map[string]any); ok {
		fmt.Fprintf(&sb, "The node's triage: class %v. %v\n\n", t["class"], t["note"])
	}
	used := 0
	for _, raw := range mv.answers {
		var a suiteAnswer
		if json.Unmarshal([]byte(raw), &a) != nil {
			continue
		}
		for _, st := range a.Steps {
			if st.Exit != 0 {
				fmt.Fprintf(&sb, "Suite %s exited %d (%d passed, %d failed).\n", st.Name, st.Exit, st.TestsPassed, st.TestsFailed)
			}
		}
		for _, t := range a.Tests {
			if t.Outcome != "fail" && t.Outcome != "timeout" {
				continue
			}
			fmt.Fprintf(&sb, "\n### %s %s (%s)\n", t.Package, t.Name, t.Outcome)
			if out := strings.TrimSpace(t.Output); out != "" && used < diagnosisEvidenceCap {
				out = clip(out, diagnosisEvidenceCap-used)
				used += len(out)
				fmt.Fprintf(&sb, "```\n%s\n```\n", out)
			}
		}
	}
	fmt.Fprintf(&sb, "\nWhen you know, call `%s` once with {\"result\": {\"diagnosis\": \"...\"}}: a few plain sentences, naming each failing test. That is your whole answer.\n", nodetools.ResultTool)
	return sb.String()
}

// diagnosisOf is the diagnosis a session's finish carried, "" for none.
func diagnosisOf(finish json.RawMessage) string {
	var d struct {
		Diagnosis string `json:"diagnosis"`
	}
	if len(finish) == 0 || json.Unmarshal(finish, &d) != nil {
		return ""
	}
	return clip(strings.TrimSpace(d.Diagnosis), 8000)
}

// withDiagnosis is finish with diag added, as diagnosis and on the end
// of the triage's note; every other key as it was. A blank diag adds
// nothing.
func withDiagnosis(finish map[string]any, diag string) map[string]any {
	out := maps.Clone(finish)
	if diag == "" {
		return out
	}
	out["diagnosis"] = diag
	if t, ok := finish["triage"].(map[string]any); ok {
		t = maps.Clone(t)
		note, _ := t["note"].(string)
		t["note"] = strings.TrimSpace(note + "\n\ndiagnosis: " + diag)
		out["triage"] = t
	}
	return out
}

// maxFinishResult is the most of a finish's result the node sends:
// under Mesh's 1 MiB body limit, with room for the evidence ids beside
// it.
const maxFinishResult = 960 << 10

// withinBodyLimit is result as the finish sends it: unchanged when it
// fits maxFinishResult; otherwise, its tests list and then its coverage
// map left out — each named in logf — rather than a finish Mesh refuses
// and a result lost (2026-11-26: a 9,500-test validation's finish).
func withinBodyLimit(result json.RawMessage, logf func(string, ...any)) json.RawMessage {
	if len(result) <= maxFinishResult {
		return result
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(result, &obj) != nil {
		logf("the finish's result is %d bytes, over Mesh's body limit, and not an object to cut", len(result))
		return result
	}
	for _, k := range []string{"tests", "coverage"} {
		if _, ok := obj[k]; !ok {
			continue
		}
		delete(obj, k)
		out, err := json.Marshal(obj)
		if err != nil {
			return result
		}
		logf("the finish's result was %d bytes, over Mesh's body limit: its %s left out (%d bytes now)", len(result), k, len(out))
		if result = out; len(result) <= maxFinishResult {
			return result
		}
	}
	return result
}
