package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

var (
	runTemplateFlag        string
	runNameFlag            string
	runEnvFlag             []string
	runNoStartFlag         bool
	runSkipPermissionsFlag bool
	runValuesFlag          []string
	runSetFlag             []string
	runSetStringFlag       []string
)

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Create and start a local agent from a template",
	RunE: func(cmd *cobra.Command, args []string) error {
		name := strings.TrimSpace(runNameFlag)
		if name == "" {
			return fmt.Errorf("--name is required")
		}
		template := strings.TrimSpace(runTemplateFlag)
		if template == "" {
			return fmt.Errorf("--template is required")
		}

		env := map[string]string{}
		for _, kv := range runEnvFlag {
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) != 2 {
				return fmt.Errorf("invalid --env %q (expected KEY=VALUE)", kv)
			}
			env[parts[0]] = parts[1]
		}

		// Pass bare `--env FOO` as "inherit FOO from my shell" — mirrors
		// docker run --env semantics and lets `GITHUB_TOKEN=xx mesh run ...`
		// work without re-typing the value.
		for _, kv := range runEnvFlag {
			if !strings.Contains(kv, "=") {
				if v, ok := os.LookupEnv(kv); ok {
					env[kv] = v
				}
			}
		}

		// Load values on the client so bad YAML / missing files surface before
		// we hand off to the daemon. templateDir is empty here — values.yaml
		// from the template itself is layered on the daemon side.
		values, err := LoadValues("", runValuesFlag, runSetFlag, runSetStringFlag)
		if err != nil {
			return err
		}

		client, err := NewLocalClient()
		if err != nil {
			return err
		}

		autoStart := !runNoStartFlag
		req := createSessionRequest{
			Name:           name,
			TemplateSource: template,
			Env:            env,
			AutoStart:      &autoStart,
			Values:         values,
		}
		// Only forward the flag if the user passed it — otherwise the template's
		// agent.yaml value wins on the daemon side.
		if cmd.Flags().Changed("dangerously-skip-permissions") {
			v := runSkipPermissionsFlag
			req.DangerouslySkipPermissions = &v
		}
		resp, err := client.CreateSession(context.Background(), req)
		if err != nil {
			return err
		}
		if resp.SessionID != "" {
			fmt.Printf("started %s (session %s)\n", resp.Name, resp.SessionID)
		} else {
			fmt.Printf("created %s (status=%s)\n", resp.Name, resp.Status)
		}
		return nil
	},
}

func init() {
	runCmd.Flags().StringVar(&runTemplateFlag, "template", "", "Template source (local path, git URL, or github.com/user/repo[@ref])")
	runCmd.Flags().StringVar(&runNameFlag, "name", "", "Local agent name")
	runCmd.Flags().StringArrayVar(&runEnvFlag, "env", nil, "KEY=VALUE or bare KEY (inherits from caller env); repeatable")
	runCmd.Flags().BoolVar(&runNoStartFlag, "no-start", false, "Register the agent but don't start its session")
	runCmd.Flags().BoolVar(&runSkipPermissionsFlag, "dangerously-skip-permissions", false, "Launch claude with --dangerously-skip-permissions (auto-approve every tool call). Overrides the template's agent.yaml value.")
	runCmd.Flags().StringArrayVar(&runValuesFlag, "values", nil, "Path to a YAML values file (repeatable; later files override earlier ones)")
	runCmd.Flags().StringArrayVar(&runSetFlag, "set", nil, "Override a template value, e.g. --set discord.enabled=true (repeatable; YAML-parsed value)")
	runCmd.Flags().StringArrayVar(&runSetStringFlag, "set-string", nil, "Override a template value, coercing the value to string (repeatable)")
}
