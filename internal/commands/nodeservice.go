package commands

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/node"
)

// mesh node service install|uninstall|status: the node as a service of
// the operator's login — launchd on macOS, systemd --user on Linux — so
// it survives crashes, logouts and reboots instead of living in a
// terminal. The service runs this binary's `node start` with the flags
// given at install (--unattended, --interval, …), logs to ~/.mesh/node.log.
var nodeServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Run the node as a service of your login (launchd on macOS, systemd --user on Linux)",
}

var (
	serviceUnattended bool
	serviceArgs       []string
	serviceEnv        []string
)

var nodeServiceInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install and start the service (stops a running one first); --unattended and --arg pass flags to node start",
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := node.LoadConfig(); err != nil {
			return err
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		startArgs := append([]string{}, serviceArgs...)
		if serviceUnattended {
			startArgs = append(startArgs, "--unattended")
		}
		spec, err := node.ServiceSpecFor(exe, home, startArgs)
		if err != nil {
			return err
		}
		spec.Env = append(spec.Env, serviceEnv...)
		if len(spec.Env) > 0 {
			fmt.Printf("Environment carried into the service: %s\n", strings.Join(spec.Env, " "))
		}
		switch runtime.GOOS {
		case "darwin":
			data, err := node.RenderLaunchAgent(spec)
			if err != nil {
				return err
			}
			path := node.LaunchAgentPath(home)
			_ = run("launchctl", "bootout", domain()+"/"+node.ServiceLabel) // a previous install, if any
			if err := node.WriteFile(path, data); err != nil {
				return err
			}
			if err := run("launchctl", "bootstrap", domain(), path); err != nil {
				return fmt.Errorf("launchctl bootstrap: %w", err)
			}
			fmt.Printf("Installed %s (%s); it starts now and at every login. Log: %s\nStatus: mesh node service status\n", node.ServiceLabel, path, spec.Log)
		case "linux":
			data, err := node.RenderSystemdUnit(spec)
			if err != nil {
				return err
			}
			path := node.SystemdUnitPath(home)
			if err := node.WriteFile(path, data); err != nil {
				return err
			}
			for _, c := range [][]string{{"systemctl", "--user", "daemon-reload"}, {"systemctl", "--user", "enable", "--now", node.ServiceLabel}, {"systemctl", "--user", "restart", node.ServiceLabel}} {
				if err := run(c[0], c[1:]...); err != nil {
					return err
				}
			}
			fmt.Printf("Installed %s (%s); it starts now and at every login (run `loginctl enable-linger` to keep it up while logged out). Log: %s\n", node.ServiceLabel, path, spec.Log)
		default:
			return fmt.Errorf("no service manager for %s; run `mesh node start` under your own supervisor", runtime.GOOS)
		}
		return nil
	},
}

var nodeServiceUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Stop and remove the service",
	RunE: func(cmd *cobra.Command, args []string) error {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		switch runtime.GOOS {
		case "darwin":
			_ = run("launchctl", "bootout", domain()+"/"+node.ServiceLabel)
			if err := os.Remove(node.LaunchAgentPath(home)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		case "linux":
			_ = run("systemctl", "--user", "disable", "--now", node.ServiceLabel)
			if err := os.Remove(node.SystemdUnitPath(home)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			_ = run("systemctl", "--user", "daemon-reload")
		default:
			return fmt.Errorf("no service manager for %s", runtime.GOOS)
		}
		fmt.Printf("Removed %s.\n", node.ServiceLabel)
		return nil
	},
}

var nodeServiceStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Whether the service is installed and running",
	RunE: func(cmd *cobra.Command, args []string) error {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		switch runtime.GOOS {
		case "darwin":
			if _, err := os.Stat(node.LaunchAgentPath(home)); err != nil {
				fmt.Println("not installed (mesh node service install)")
				return nil
			}
			out, err := exec.Command("launchctl", "print", domain()+"/"+node.ServiceLabel).CombinedOutput()
			if err != nil {
				fmt.Println("installed, not loaded:", strings.TrimSpace(string(out)))
				return nil
			}
			state, pid := "unknown", ""
			for _, line := range strings.Split(string(out), "\n") {
				line = strings.TrimSpace(line)
				if v, ok := strings.CutPrefix(line, "state = "); ok {
					state = v
				}
				if v, ok := strings.CutPrefix(line, "pid = "); ok {
					pid = v
				}
			}
			fmt.Printf("%s: %s", node.ServiceLabel, state)
			if pid != "" {
				fmt.Printf(" (pid %s)", pid)
			}
			fmt.Printf("; log %s\n", node.ServiceLog(home))
		case "linux":
			out, _ := exec.Command("systemctl", "--user", "is-active", node.ServiceLabel).CombinedOutput()
			fmt.Printf("%s: %s; log %s\n", node.ServiceLabel, strings.TrimSpace(string(out)), node.ServiceLog(home))
		default:
			return fmt.Errorf("no service manager for %s", runtime.GOOS)
		}
		return nil
	},
}

// domain is launchd's per-user domain for this login.
func domain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func init() {
	nodeServiceInstallCmd.Flags().BoolVar(&serviceUnattended, "unattended", false, "start with --dangerously-skip-permissions (a machine you trust with that)")
	nodeServiceInstallCmd.Flags().StringArrayVar(&serviceArgs, "arg", nil, "another flag for node start, e.g. --arg=--interval=1m (repeatable)")
	nodeServiceInstallCmd.Flags().StringArrayVar(&serviceEnv, "env", nil, "a KEY=VALUE the service's sessions inherit (repeatable); GH_CONFIG_DIR, GIT_CONFIG_GLOBAL, GH_TOKEN, GITHUB_TOKEN and CLAUDE_CONFIG_DIR are taken from this shell when set")
	nodeServiceCmd.AddCommand(nodeServiceInstallCmd, nodeServiceUninstallCmd, nodeServiceStatusCmd)
	nodeCmd.AddCommand(nodeServiceCmd)
}
