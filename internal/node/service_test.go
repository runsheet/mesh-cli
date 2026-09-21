package node

import (
	"errors"
	"strings"
	"testing"
)

var errNotFound = errors.New("not found")

func TestServiceDefinitions(t *testing.T) {
	lookPath = func(file string) (string, error) {
		switch file {
		case "claude":
			return "/Users/x/.local/bin/claude", nil
		case "gh":
			return "/opt/homebrew/bin/gh", nil
		}
		return "", errNotFound
	}
	t.Cleanup(func() { lookPath = execLookPath })
	t.Setenv("GH_CONFIG_DIR", "/Users/x/.config/gh-personal")
	for _, k := range []string{"GIT_CONFIG_GLOBAL", "GH_TOKEN", "GITHUB_TOKEN", "CLAUDE_CONFIG_DIR"} {
		t.Setenv(k, "") // whatever this shell has, the spec takes only what the test sets
	}
	spec, err := ServiceSpecFor("/Users/x/bin/mesh", "/Users/x", []string{"--unattended", "--interval=1m"})
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Env) != 1 || spec.Env[0] != "GH_CONFIG_DIR=/Users/x/.config/gh-personal" {
		t.Fatalf("env: %v", spec.Env)
	}
	if !strings.HasPrefix(spec.Path, "/Users/x/.local/bin:/opt/homebrew/bin:/Users/x/bin:") || !strings.Contains(spec.Path, ":/usr/bin:") || spec.Log != "/Users/x/.mesh/node.log" {
		t.Fatalf("spec: %+v", spec)
	}
	plist, err := RenderLaunchAgent(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<string>" + ServiceLabel + "</string>", "<string>/Users/x/bin/mesh</string>", "<string>node</string>", "<string>start</string>", "<string>--unattended</string>", "<string>--interval=1m</string>",
		"<key>KeepAlive</key>\n\t<true/>", "<key>PATH</key>\n\t\t<string>" + spec.Path + "</string>", "<string>/Users/x/.mesh/node.log</string>", "<key>GH_CONFIG_DIR</key>\n\t\t<string>/Users/x/.config/gh-personal</string>"} {
		if !strings.Contains(string(plist), want) {
			t.Fatalf("plist lacks %q:\n%s", want, plist)
		}
	}
	unit, err := RenderSystemdUnit(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ExecStart=/Users/x/bin/mesh node start --unattended --interval=1m", "Restart=always", "Environment=PATH=" + spec.Path, "Environment=GH_CONFIG_DIR=/Users/x/.config/gh-personal", "StandardOutput=append:/Users/x/.mesh/node.log"} {
		if !strings.Contains(string(unit), want) {
			t.Fatalf("unit lacks %q:\n%s", want, unit)
		}
	}
}
