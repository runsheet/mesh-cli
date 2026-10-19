package harness

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"projectmesh.io/mesh-cli/internal/validate"
)

// goTestRE finds a `go test` invocation anywhere in a shell statement.
var goTestRE = regexp.MustCompile(`\bgo\s+test\b`)

// wholeModuleTargetRE finds a bare `./...` target, bounded by whitespace
// or the ends of the statement, so it never matches a scoped target like
// `./internal/foo/...`.
var wholeModuleTargetRE = regexp.MustCompile(`(^|\s)\./\.\.\.(\s|$)`)

// bareWholeSuiteRE is a whole-repository test command in an ecosystem
// with no per-package scoping: the statement, trimmed, is exactly one of
// these, with no narrowing argument.
var bareWholeSuiteRE = regexp.MustCompile(`^(npm|yarn|pnpm)\s+(run\s+)?test$|^make\s+test$|^pytest$`)

// cmdStatementSepRE splits a Bash command into its statements: `&&`,
// `||`, `;`, `|` and subshell parens all separate one test invocation
// from another, so `cd x && go test ./...` and `(go test ./...)` are
// read the same as `go test ./...` alone.
var cmdStatementSepRE = regexp.MustCompile(`&&|\|\||;|\||[()]`)

// testCommandRE is a command that runs tests at all, for the
// background-run check (b): any test command waited on in the
// background is refused, whole-suite or not.
var testCommandRE = regexp.MustCompile(`\bgo\s+test\b|\b(npm|yarn|pnpm)\s+(run\s+)?test\b|\bmake\s+test\b|\bpytest\b|\bjest\b|\bmocha\b|\bcargo\s+test\b|\bmvn\s+test\b|\bgradle\s+test\b`)

// TestGuardCommand is the PreToolUse hook command a build session's
// settings run on every Bash call (ADR-038): bin (the mesh binary,
// quoted for the shell) `test-guard --hook`.
func TestGuardCommand(bin string) string {
	return shellQuote(bin) + " test-guard --hook"
}

// TestGuardHook is the PreToolUse hook's answer to hookInput, Claude
// Code's JSON for a Bash call (TrimHook's same input shape): a
// whole-suite test run — `go test ./...` with any flags, inside
// `&&`/`;`/pipes/subshells, `cd x && go test ./...`, and the like for
// other ecosystems' whole-suite commands — or any test command run with
// `run_in_background: true` is refused with `permissionDecision:
// "deny"` and a reason that sends the builder to the suites its change
// touched, read from the checkout's `.runsheet/validate.yml` (none: a
// generic reason, never an error). Everything else gets no answer (ok
// false), so the call runs as it is.
func TestGuardHook(hookInput []byte, checkout string, changed []string) (out []byte, ok bool, err error) {
	var in struct {
		ToolName  string         `json:"tool_name"`
		ToolInput map[string]any `json:"tool_input"`
	}
	if err := json.Unmarshal(hookInput, &in); err != nil {
		return nil, false, fmt.Errorf("hook input: %w", err)
	}
	if in.ToolName != "Bash" {
		return nil, false, nil
	}
	cmd, _ := in.ToolInput["command"].(string)
	if strings.TrimSpace(cmd) == "" {
		return nil, false, nil
	}
	bg, _ := in.ToolInput["run_in_background"].(bool)

	var reason string
	switch {
	case isWholeSuite(cmd):
		reason = "test the packages this change touched, not the whole suite: run `go test ./internal/<pkg>/...` for each one it affects. " +
			touchedSuitesNote(checkout, changed) +
			" The node runs the whole suite in validation."
	case bg && testCommandRE.MatchString(cmd):
		reason = "run the touched tests in the foreground, not in the background: a background test run is read while it runs, and waiting idle on it wastes the session. " +
			touchedSuitesNote(checkout, changed)
	default:
		return nil, false, nil
	}
	out, err = json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       "deny",
		"permissionDecisionReason": reason,
	}})
	return out, err == nil, err
}

// isWholeSuite reports whether any statement of cmd runs every package's
// tests at once.
func isWholeSuite(cmd string) bool {
	for _, stmt := range cmdStatementSepRE.Split(cmd, -1) {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if goTestRE.MatchString(stmt) && wholeModuleTargetRE.MatchString(stmt) {
			return true
		}
		if bareWholeSuiteRE.MatchString(stmt) {
			return true
		}
	}
	return false
}

// touchedSuitesNote names the suites of the checkout's
// `.runsheet/validate.yml` whose paths match changed — the tests this
// change actually touched — or a generic reason when the checkout
// declares none.
func touchedSuitesNote(checkout string, changed []string) string {
	cfg, err := validate.Load(checkout)
	if err != nil {
		return "Test the packages this change touched."
	}
	suites := validate.Select(cfg, changed)
	if len(suites) == 0 {
		return "Test the packages this change touched."
	}
	names := make([]string, len(suites))
	for i, s := range suites {
		names[i] = s.Name
	}
	return "This change touches: " + strings.Join(names, ", ") + "."
}
