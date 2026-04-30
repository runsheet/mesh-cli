package commands

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var workflowCmd = &cobra.Command{
	Use:   "workflow",
	Short: "Workflow template commands",
	Long:  "List and inspect the five built-in workflow templates.",
}

// mesh workflow list

var workflowListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all workflow templates",
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := requireSession()
		if err != nil {
			return err
		}
		client := newMeshClientForSession(sess)

		var resp struct {
			OK        bool `json:"ok"`
			Workflows []struct {
				Slug        string `json:"slug"`
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"workflows"`
		}
		if err := client.doRequest("GET", "/workflows", nil, &resp); err != nil {
			return err
		}
		if len(resp.Workflows) == 0 {
			fmt.Println("no workflows found")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "SLUG\tNAME\tDESCRIPTION")
		for _, wf := range resp.Workflows {
			desc := wf.Description
			if len(desc) > 60 {
				desc = desc[:57] + "..."
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", wf.Slug, wf.Name, desc)
		}
		return w.Flush()
	},
}

// mesh workflow show <slug>

var workflowShowCmd = &cobra.Command{
	Use:     "show <slug>",
	Short:   "Show a workflow definition",
	Example: "mesh workflow show full\nmesh workflow show doc",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := requireSession()
		if err != nil {
			return err
		}
		client := newMeshClientForSession(sess)

		var resp struct {
			OK       bool `json:"ok"`
			Workflow struct {
				Slug          string              `json:"slug"`
				Name          string              `json:"name"`
				Description   string              `json:"description"`
				States        []string            `json:"states"`
				Transitions   map[string][]string `json:"transitions"`
				InitialState  string              `json:"initialState"`
				TerminalStates []string           `json:"terminalStates"`
				Roles         map[string]string   `json:"roles"`
			} `json:"workflow"`
		}
		if err := client.doRequest("GET", "/workflows/"+args[0], nil, &resp); err != nil {
			return err
		}

		wf := resp.Workflow
		fmt.Printf("slug:    %s\n", wf.Slug)
		fmt.Printf("name:    %s\n", wf.Name)
		fmt.Printf("desc:    %s\n", wf.Description)
		fmt.Printf("initial: %s\n", wf.InitialState)
		fmt.Printf("terminal: %s\n", strings.Join(wf.TerminalStates, ", "))
		fmt.Println()

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "STATE\tROLE\tTRANSITIONS TO")
		for _, state := range wf.States {
			tos := wf.Transitions[state]
			role := wf.Roles[state]
			if role == "" {
				role = "-"
			}
			tosStr := "-"
			if len(tos) > 0 {
				tosStr = strings.Join(tos, ", ")
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", state, role, tosStr)
		}
		return w.Flush()
	},
}

// mesh project workflow set <slug>  (registered under projectCmd)

var projectWorkflowSetProjectID string

var projectWorkflowSetCmd = &cobra.Command{
	Use:     "set <slug>",
	Short:   "Set the default workflow for a project",
	Example: "mesh project workflow set doc --project <id>\nmesh project workflow set full --project <id>",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		slug := args[0]
		projectID := strings.TrimSpace(projectWorkflowSetProjectID)
		if projectID == "" {
			return fmt.Errorf("--project is required")
		}

		sess, err := requireSession()
		if err != nil {
			return err
		}
		client := newMeshClientForSession(sess)

		payload := map[string]any{"slug": slug}
		var resp map[string]any
		if err := client.doRequest("POST", "/project/"+projectID+"/workflow", payload, &resp); err != nil {
			return err
		}
		fmt.Printf("default workflow for project %s set to %q\n", projectID, slug)
		return nil
	},
}

// projectWorkflowCmd is the intermediate subcommand so it nests under projectCmd.
var projectWorkflowCmd = &cobra.Command{
	Use:   "workflow",
	Short: "Project workflow commands",
}

func init() {
	projectWorkflowSetCmd.Flags().StringVar(&projectWorkflowSetProjectID, "project", "", "Project ID")

	projectWorkflowCmd.AddCommand(projectWorkflowSetCmd)
	projectCmd.AddCommand(projectWorkflowCmd)

	workflowCmd.AddCommand(workflowListCmd, workflowShowCmd)
}
