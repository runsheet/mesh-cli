package node

import (
	"errors"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/sessionunit"
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
	if !strings.HasPrefix(spec.Path, "/Users/x/.local/bin:/opt/homebrew/bin:/Users/x/bin:") || !strings.Contains(spec.Path, ":/usr/bin:") || spec.CrashLog != "/Users/x/.mesh/node.crash.log" {
		t.Fatalf("spec: %+v", spec)
	}
	plist, err := RenderLaunchAgent(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<string>" + ServiceLabel + "</string>", "<string>/Users/x/bin/mesh</string>", "<string>node</string>", "<string>start</string>", "<string>--unattended</string>", "<string>--interval=1m</string>",
		"<key>KeepAlive</key>\n\t<true/>", "<key>AbandonProcessGroup</key>\n\t<true/>", "<key>PATH</key>\n\t\t<string>" + spec.Path + "</string>", "<key>StandardOutPath</key>\n\t<string>/Users/x/.mesh/node.crash.log</string>", "<key>StandardErrorPath</key>\n\t<string>/Users/x/.mesh/node.crash.log</string>", "<key>GH_CONFIG_DIR</key>\n\t\t<string>/Users/x/.config/gh-personal</string>"} {
		if !strings.Contains(string(plist), want) {
			t.Fatalf("plist lacks %q:\n%s", want, plist)
		}
	}
	unit, err := RenderSystemdUnit(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ExecStart=/Users/x/bin/mesh node start --unattended --interval=1m", "Restart=always", "KillMode=process", "Environment=PATH=" + spec.Path, "Environment=GH_CONFIG_DIR=/Users/x/.config/gh-personal", "\nStandardOutput=journal\n", "\nStandardError=journal\n"} {
		if !strings.Contains(string(unit), want) {
			t.Fatalf("unit lacks %q:\n%s", want, unit)
		}
	}
}

// Stopping or restarting the node's service never reaches a session
// (adr-009): the unit signals its main process alone (KillMode=process,
// and nothing else that would widen it), and each session is a
// transient scope of its own — another unit, whose name is not the
// service's and which is bound to it by nothing in the unit file.
func TestServiceStopLeavesSessionScopes(t *testing.T) {
	unit, err := RenderSystemdUnit(ServiceSpec{Exe: "/home/x/bin/mesh", Home: "/home/x", Path: "/usr/bin", CrashLog: "/home/x/.mesh/node.crash.log"})
	if err != nil {
		t.Fatal(err)
	}
	var kill []string
	for _, line := range strings.Split(string(unit), "\n") {
		if strings.HasPrefix(line, "KillMode=") {
			kill = append(kill, line)
		}
		for _, bad := range []string{"KillSignal=", "SendSIGKILL=", "Delegate=", "BindsTo=", "PartOf=", "PropagatesStopTo="} {
			if strings.HasPrefix(line, bad) {
				t.Fatalf("unit has %q:\n%s", line, unit)
			}
		}
	}
	if len(kill) != 1 || kill[0] != "KillMode=process" {
		t.Fatalf("KillMode lines %q, want exactly KillMode=process:\n%s", kill, unit)
	}
	argv := sessionunit.SystemdScope{}.Argv(sessionunit.Spec{Binary: "claude"}, sessionunit.UnitName("build", "item-1"))
	if argv[1] != "--user" || argv[2] != "--scope" || !strings.HasPrefix(argv[3], "--unit=mesh-session-build-item-1-") || strings.Contains(strings.Join(argv, " "), ServiceLabel) {
		t.Fatalf("session scope argv %q", argv)
	}
	for _, a := range argv {
		if strings.HasPrefix(a, "--slice") || strings.HasPrefix(a, "--property=PartOf") || strings.HasPrefix(a, "--property=BindsTo") {
			t.Fatalf("the session scope is tied to the service: %q", argv)
		}
	}
}

// The service no longer appends the node's stdout and stderr to
// node.log: the node writes and rotates node.log itself, and the
// service keeps only what it cannot write — the journal under systemd,
// node.crash.log under launchd.
func TestServiceOutputIsNotNodeLog(t *testing.T) {
	spec := ServiceSpec{Exe: "/home/x/bin/mesh", Home: "/home/x", Path: "/usr/bin", CrashLog: CrashLog("/home/x")}
	unit, err := RenderSystemdUnit(spec)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(unit), "\n") {
		if strings.HasPrefix(line, "StandardOutput=") || strings.HasPrefix(line, "StandardError=") {
			out = append(out, line)
		}
	}
	if want := []string{"StandardOutput=journal", "StandardError=journal"}; strings.Join(out, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unit's output lines %q, want %q", out, want)
	}
	if strings.Contains(string(unit), "node.log") {
		t.Fatalf("unit names node.log:\n%s", unit)
	}
	plist, err := RenderLaunchAgent(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"\t<key>StandardOutPath</key>\n\t<string>/home/x/.mesh/node.crash.log</string>\n",
		"\t<key>StandardErrorPath</key>\n\t<string>/home/x/.mesh/node.crash.log</string>\n",
	} {
		if !strings.Contains(string(plist), want) {
			t.Fatalf("plist lacks %q:\n%s", want, plist)
		}
	}
	if strings.Contains(string(plist), "node.log") {
		t.Fatalf("plist names node.log:\n%s", plist)
	}
}
