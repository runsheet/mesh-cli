package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// decodedDeny reads a TestGuardHook answer's permissionDecision and
// permissionDecisionReason.
func decodedDeny(t *testing.T, out []byte) (decision, reason string) {
	t.Helper()
	var got struct {
		HookSpecificOutput struct {
			HookEventName            string `json:"hookEventName"`
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v: %s", err, out)
	}
	if got.HookSpecificOutput.HookEventName != "PreToolUse" {
		t.Errorf("hookEventName %q", got.HookSpecificOutput.HookEventName)
	}
	return got.HookSpecificOutput.PermissionDecision, got.HookSpecificOutput.PermissionDecisionReason
}

// A whole-suite `go test ./...` is refused with a reason pointing at
// the touched packages, whatever flags or shell wrapping surround it;
// a scoped test of one package is let through.
func TestHookRefusesWholeSuite(t *testing.T) {
	refused := []string{
		`go test ./...`,
		`go test -v -count=1 ./...`,
		`go test ./... -run TestFoo`,
		`cd internal/harness && go test ./...`,
		`go build ./... && go test ./...`,
		`(go test ./...)`,
		`go test ./... | tee out.log`,
	}
	for _, cmd := range refused {
		in := []byte(`{"tool_name":"Bash","tool_input":{"command":` + jsonString(cmd) + `}}`)
		out, ok, err := TestGuardHook(in, t.TempDir(), nil)
		if err != nil || !ok {
			t.Fatalf("%q: ok %v, err %v, want denied", cmd, ok, err)
		}
		decision, reason := decodedDeny(t, out)
		if decision != "deny" {
			t.Errorf("%q: permissionDecision %q, want deny", cmd, decision)
		}
		if !strings.Contains(reason, "go test ./internal/<pkg>/...") {
			t.Errorf("%q: reason does not point at the touched packages: %s", cmd, reason)
		}
		if !strings.Contains(reason, "validation") {
			t.Errorf("%q: reason does not say the node runs the whole suite in validation: %s", cmd, reason)
		}
	}

	let := []string{
		`go test ./internal/harness/...`,
		`go test ./internal/harness/ -run TestFoo`,
		`go build ./...`,
		`echo ./...`,
	}
	for _, cmd := range let {
		in := []byte(`{"tool_name":"Bash","tool_input":{"command":` + jsonString(cmd) + `}}`)
		if _, ok, err := TestGuardHook(in, t.TempDir(), nil); ok || err != nil {
			t.Errorf("%q: ok %v, err %v, want let through", cmd, ok, err)
		}
	}
}

// A test command run with run_in_background is refused, told to run in
// the foreground and never to wait idle on it; the same command in the
// foreground, and a background command that is not a test, are let
// through.
func TestHookRefusesBackgroundTests(t *testing.T) {
	in := []byte(`{"tool_name":"Bash","tool_input":{"command":"go test ./internal/harness/...","run_in_background":true}}`)
	out, ok, err := TestGuardHook(in, t.TempDir(), nil)
	if err != nil || !ok {
		t.Fatalf("ok %v, err %v, want denied", ok, err)
	}
	decision, reason := decodedDeny(t, out)
	if decision != "deny" {
		t.Errorf("permissionDecision %q, want deny", decision)
	}
	if !strings.Contains(reason, "foreground") || !strings.Contains(reason, "idle") {
		t.Errorf("reason does not say to run in the foreground and not wait idle: %s", reason)
	}

	foreground := []byte(`{"tool_name":"Bash","tool_input":{"command":"go test ./internal/harness/..."}}`)
	if _, ok, err := TestGuardHook(foreground, t.TempDir(), nil); ok || err != nil {
		t.Errorf("foreground test: ok %v, err %v, want let through", ok, err)
	}

	notATest := []byte(`{"tool_name":"Bash","tool_input":{"command":"npm run dev","run_in_background":true}}`)
	if _, ok, err := TestGuardHook(notATest, t.TempDir(), nil); ok || err != nil {
		t.Errorf("background non-test: ok %v, err %v, want let through", ok, err)
	}
}

// With a checkout's .runsheet/validate.yml, the reason names the suite
// whose paths match the changed files and leaves out one that does not;
// with no validate.yml the reason still refuses but names no suite.
func TestHookReadsValidateYml(t *testing.T) {
	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, ".runsheet"), 0o755); err != nil {
		t.Fatal(err)
	}
	validateYml := `suites:
  - name: harness-unit
    kind: unit
    command: go test ./internal/harness/...
    paths:
      - internal/harness/**
  - name: node-unit
    kind: unit
    command: go test ./internal/node/...
    paths:
      - internal/node/**
`
	if err := os.WriteFile(filepath.Join(checkout, ".runsheet", "validate.yml"), []byte(validateYml), 0o644); err != nil {
		t.Fatal(err)
	}

	in := []byte(`{"tool_name":"Bash","tool_input":{"command":"go test ./..."}}`)
	out, ok, err := TestGuardHook(in, checkout, []string{"internal/harness/testguard.go"})
	if err != nil || !ok {
		t.Fatalf("ok %v, err %v, want denied", ok, err)
	}
	_, reason := decodedDeny(t, out)
	if !strings.Contains(reason, "harness-unit") {
		t.Errorf("reason does not name the matching suite: %s", reason)
	}
	if strings.Contains(reason, "node-unit") {
		t.Errorf("reason names a suite the change did not touch: %s", reason)
	}

	// no .runsheet/validate.yml: a generic reason, never an error
	out, ok, err = TestGuardHook(in, t.TempDir(), []string{"internal/harness/testguard.go"})
	if err != nil || !ok {
		t.Fatalf("no validate.yml: ok %v, err %v, want denied with no error", ok, err)
	}
	_, reason = decodedDeny(t, out)
	if strings.Contains(reason, "harness-unit") {
		t.Errorf("no validate.yml: reason names a suite: %s", reason)
	}
}

// jsonString is s as a JSON string literal, for building hook input
// without importing encoding/json at every call site.
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
