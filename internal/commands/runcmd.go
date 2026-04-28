package commands

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/localsock"
	itpl "projectmesh.io/mesh-cli/internal/template"
)

var (
	runTemplateFlag        string
	runNameFlag            string
	runEnvFlag             []string
	runEnvFileFlag         []string
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
		for _, path := range runEnvFileFlag {
			loaded, err := loadEnvFile(path)
			if err != nil {
				return err
			}
			for k, v := range loaded {
				env[k] = v
			}
		}
		for _, kv := range runEnvFlag {
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) != 2 {
				return fmt.Errorf("invalid --env %q (expected KEY=VALUE)", kv)
			}
			env[parts[0]] = parts[1]
		}

		for _, kv := range runEnvFlag {
			if !strings.Contains(kv, "=") {
				if v, ok := os.LookupEnv(kv); ok {
					env[kv] = v
				}
			}
		}

		values, err := itpl.LoadValues("", runValuesFlag, runSetFlag, runSetStringFlag)
		if err != nil {
			return err
		}

		lc, err := newLocalClient()
		if err != nil {
			return err
		}

		autoStart := !runNoStartFlag
		req := localsock.CreateSessionRequest{
			Name:           name,
			TemplateSource: template,
			Env:            env,
			AutoStart:      &autoStart,
			Values:         values,
		}
		if cmd.Flags().Changed("dangerously-skip-permissions") {
			v := runSkipPermissionsFlag
			req.DangerouslySkipPermissions = &v
		}
		resp, err := lc.createSession(context.Background(), req)
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
	runCmd.Flags().StringArrayVar(&runEnvFileFlag, "env-file", nil, "Path to a .env file (KEY=VALUE per line; repeatable; later files override earlier ones; --env flags override env-files)")
	runCmd.Flags().BoolVar(&runNoStartFlag, "no-start", false, "Register the agent but don't start its session")
	runCmd.Flags().BoolVar(&runSkipPermissionsFlag, "dangerously-skip-permissions", false, "Launch claude with --dangerously-skip-permissions (auto-approve every tool call). Overrides the template's agent.yaml value.")
	runCmd.Flags().StringArrayVar(&runValuesFlag, "values", nil, "Path to a YAML values file (repeatable; later files override earlier ones)")
	runCmd.Flags().StringArrayVar(&runSetFlag, "set", nil, "Override a template value, e.g. --set discord.enabled=true (repeatable; YAML-parsed value)")
	runCmd.Flags().StringArrayVar(&runSetStringFlag, "set-string", nil, "Override a template value, coercing the value to string (repeatable)")
}
