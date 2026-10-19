package node

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
)

// The node as a service of the operator's login session: on macOS a
// launchd LaunchAgent, on Linux a systemd user unit. Both keep
// `mesh node start` alive across crashes and logins (KeepAlive /
// Restart=always) and put the tools a session needs — claude, gh, git —
// on PATH; the node's token and config are read from ~/.mesh as ever.
// The node writes ~/.mesh/node.log itself, capped and rotated
// (logfile.go): the service keeps only the process's own stdout and
// stderr — a panic's stack — in the journal (systemd) or
// ~/.mesh/node.crash.log (launchd), never appended to node.log, which
// it would hold open past every rotation.
//
// Neither reaches the node's sessions when it stops or restarts the
// node (adr-009, `mesh node upgrade`): a session runs in its own
// transient systemd scope — a unit and a cgroup of its own, not the
// service's — or, without a user systemd, detached in a session and
// process group of its own. The unit's KillMode=process signals the
// node's main process alone, so even a detached session left in the
// service's cgroup is not killed with it; the LaunchAgent's
// AbandonProcessGroup does the same under launchd.

// ServiceLabel names the service to launchd and systemd.
const ServiceLabel = "io.projectmesh.mesh-node"

// SafeKillMode is the KillMode a systemd unit must have for the node's
// sandboxed sessions to outlive it: every pod's conmon runs in the
// service's cgroup, so systemd's default, control-group, kills them all on
// a restart or a self-update's exit. The unit RenderSystemdUnit writes says
// it; a unit written by hand may not (dl360, 2026-10-19: two build sessions
// ended with the node on a restart).
const SafeKillMode = "process"

// SystemdKillMode is the KillMode of the node's systemd --user unit as
// systemd has it loaded, and whether a loaded unit answered at all: run
// executes a command and returns its output (nil runs it).
func SystemdKillMode(ctx context.Context, run func(ctx context.Context, name string, args ...string) ([]byte, error)) (string, bool) {
	if run == nil {
		run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).Output()
		}
	}
	unit := ServiceLabel + ".service"
	b, err := run(ctx, "systemctl", "--user", "show", unit, "-p", "LoadState", "-p", "KillMode")
	if err != nil {
		return "", false
	}
	var load, mode string
	for _, line := range strings.Split(string(b), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "LoadState":
			load = v
		case "KillMode":
			mode = v
		}
	}
	if load != "loaded" || mode == "" {
		return "", false
	}
	return mode, true
}

// ServiceSpec is what the service runs.
type ServiceSpec struct {
	// Exe is the mesh binary, absolute.
	Exe string
	// Args follow `node start`.
	Args []string
	// Home is the operator's home; ~/.mesh lives there.
	Home string
	// Path is the PATH the session inherits.
	Path string
	// CrashLog is the file launchd appends stdout and stderr to — what
	// the node cannot write to node.log itself; systemd's go to the
	// journal.
	CrashLog string
	// Env are more variables the session inherits (KEY=VALUE): the gh
	// config dir and token, when the installing shell had them.
	Env []string
}

// InheritedEnv is what the service takes from the installing shell when
// set there: how gh finds its login, and a token if that is how.
var InheritedEnv = []string{"GH_CONFIG_DIR", "GIT_CONFIG_GLOBAL", "GH_TOKEN", "GITHUB_TOKEN", "CLAUDE_CONFIG_DIR"}

func inheritedEnv() []string {
	var out []string
	for _, k := range InheritedEnv {
		if v := os.Getenv(k); v != "" {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// ServiceLog is ~/.mesh/node.log.
func ServiceLog(home string) string { return filepath.Join(home, ".mesh", "node.log") }

// ServicePath builds the PATH a service needs: where the tools are now,
// the usual places, and the operator's own bins — a launchd job starts
// with almost none.
func ServicePath(home string) string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, tool := range []string{"claude", "gh", "git", "mesh", "go", "node", "npm"} {
		if p, err := lookPath(tool); err == nil {
			add(filepath.Dir(p))
		}
	}
	for _, p := range []string{filepath.Join(home, "bin"), filepath.Join(home, ".local", "bin"), "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"} {
		add(p)
	}
	return strings.Join(out, ":")
}

var lookPath = func(file string) (string, error) { return execLookPath(file) }

// LaunchAgentPath is ~/Library/LaunchAgents/<label>.plist.
func LaunchAgentPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", ServiceLabel+".plist")
}

// SystemdUnitPath is ~/.config/systemd/user/<label>.service.
func SystemdUnitPath(home string) string {
	return filepath.Join(home, ".config", "systemd", "user", ServiceLabel+".service")
}

var launchAgentTmpl = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.Exe}}</string>
		<string>node</string>
		<string>start</string>
{{- range .Args}}
		<string>{{.}}</string>
{{- end}}
	</array>
	<key>WorkingDirectory</key>
	<string>{{.Home}}</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>{{.Path}}</string>
		<key>HOME</key>
		<string>{{.Home}}</string>
{{- range .EnvPairs}}
		<key>{{.Key}}</key>
		<string>{{.Value}}</string>
{{- end}}
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>StandardOutPath</key>
	<string>{{.CrashLog}}</string>
	<key>StandardErrorPath</key>
	<string>{{.CrashLog}}</string>
	<key>ProcessType</key>
	<string>Background</string>
	<key>AbandonProcessGroup</key>
	<true/>
</dict>
</plist>
`))

var systemdTmpl = template.Must(template.New("unit").Parse(`[Unit]
Description=Mesh node: runs work items of goals on this machine
After=network-online.target

[Service]
ExecStart={{.Exe}} node start{{range .Args}} {{.}}{{end}}
WorkingDirectory={{.Home}}
Environment=PATH={{.Path}}
Environment=HOME={{.Home}}
{{- range .EnvPairs}}
Environment={{.Key}}={{.Value}}
{{- end}}
Restart=always
RestartSec=10
KillMode=process
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=default.target
`))

type envPair struct{ Key, Value string }

// EnvPairs splits Env for the templates.
func (s ServiceSpec) EnvPairs() []envPair {
	var out []envPair
	for _, kv := range s.Env {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			out = append(out, envPair{k, v})
		}
	}
	return out
}

// RenderLaunchAgent renders the LaunchAgent plist.
func RenderLaunchAgent(spec ServiceSpec) ([]byte, error) {
	var b bytes.Buffer
	err := launchAgentTmpl.Execute(&b, struct {
		ServiceSpec
		Label string
	}{spec, ServiceLabel})
	return b.Bytes(), err
}

// RenderSystemdUnit renders the systemd user unit.
func RenderSystemdUnit(spec ServiceSpec) ([]byte, error) {
	var b bytes.Buffer
	err := systemdTmpl.Execute(&b, spec)
	return b.Bytes(), err
}

// WriteFile writes a service definition, making its directory.
func WriteFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// ServiceSpecFor is the spec for this binary, the operator's home and
// the start flags given.
func ServiceSpecFor(exe, home string, args []string) (ServiceSpec, error) {
	if exe == "" || home == "" {
		return ServiceSpec{}, fmt.Errorf("the binary and the home are required")
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		return ServiceSpec{}, err
	}
	return ServiceSpec{Exe: abs, Args: args, Home: home, Path: ServicePath(home), CrashLog: CrashLog(home), Env: inheritedEnv()}, nil
}
