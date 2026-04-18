package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

var (
	templateRenderTemplateFlag   string
	templateRenderNameFlag       string
	templateRenderValuesFlag     []string
	templateRenderSetFlag        []string
	templateRenderSetStringFlag  []string
)

// templateRenderCmd fetches a template (git or local) the same way `mesh run`
// does, renders it with the given values, and prints the resulting agent.yaml
// to stdout. Useful for debugging templated templates without creating agents.
var templateRenderCmd = &cobra.Command{
	Use:           "render",
	Short:         "Render a template's agent.yaml with the given values and print it to stdout",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		src := strings.TrimSpace(templateRenderTemplateFlag)
		if src == "" {
			return fmt.Errorf("--template is required")
		}

		resolved, err := ResolveTemplateSource(src)
		if err != nil {
			return err
		}
		tplDir, _, _, err := materializeTemplateDir(resolved)
		if err != nil {
			return err
		}

		values, err := LoadValues(tplDir, templateRenderValuesFlag, templateRenderSetFlag, templateRenderSetStringFlag)
		if err != nil {
			return err
		}

		agentBytes, _, err := RenderTemplate(tplDir, templateRenderNameFlag, values)
		if err != nil {
			return err
		}

		if _, err := os.Stdout.Write(agentBytes); err != nil {
			return err
		}
		if len(agentBytes) > 0 && agentBytes[len(agentBytes)-1] != '\n' {
			fmt.Println()
		}
		return nil
	},
}

func init() {
	templateRenderCmd.Flags().StringVar(&templateRenderTemplateFlag, "template", "", "Template source (local path, git URL, or github.com/user/repo[@ref])")
	templateRenderCmd.Flags().StringVar(&templateRenderNameFlag, "name", "", "Value for .Name inside the template")
	templateRenderCmd.Flags().StringArrayVar(&templateRenderValuesFlag, "values", nil, "Path to a YAML values file (repeatable; later files override earlier ones)")
	templateRenderCmd.Flags().StringArrayVar(&templateRenderSetFlag, "set", nil, "Override a template value, e.g. --set discord.enabled=true (repeatable)")
	templateRenderCmd.Flags().StringArrayVar(&templateRenderSetStringFlag, "set-string", nil, "Override a template value, coercing to string (repeatable)")

	templatesCmd.AddCommand(templateRenderCmd)
}
