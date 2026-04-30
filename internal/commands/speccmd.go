package commands

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var specCmd = &cobra.Command{
	Use:   "spec",
	Short: "Spec repo commands",
	Long:  "Create, update, list, and fetch project specs stored in the project's GitHub spec repo.",
}

// ── spec create ───────────────────────────────────────────────────────────────

var (
	specCreateProject string
	specCreatePath    string
	specCreateTitle   string
	specCreateContent string
	specCreateFile    string
)

var specCreateCmd = &cobra.Command{
	Use:     "create",
	Short:   "Create a new spec file in the project's spec repo",
	Example: "mesh spec create --project <id> --title \"My Spec\" --content \"# My Spec\\n\\nContent here.\"",
	RunE: func(cmd *cobra.Command, _ []string) error {
		sess, err := requireSession()
		if err != nil {
			return err
		}

		if specCreateProject == "" {
			return fmt.Errorf("--project is required")
		}
		if specCreateTitle == "" {
			return fmt.Errorf("--title is required")
		}

		content := specCreateContent
		if content == "" && specCreateFile != "" {
			b, err := os.ReadFile(specCreateFile)
			if err != nil {
				return fmt.Errorf("reading --file: %w", err)
			}
			content = string(b)
		}
		if content == "" {
			return fmt.Errorf("--content or --file is required")
		}

		payload := map[string]any{
			"title":   specCreateTitle,
			"content": content,
		}
		if specCreatePath != "" {
			payload["path"] = specCreatePath
		}

		client := newMeshClientForSession(sess)
		var resp map[string]any
		path := fmt.Sprintf("/project/%s/specs", specCreateProject)
		if err := client.doRequest("POST", path, payload, &resp); err != nil {
			return err
		}
		return printJSON(resp)
	},
}

// ── spec update ───────────────────────────────────────────────────────────────

var (
	specUpdateProject string
	specUpdatePath    string
	specUpdateBaseSHA string
	specUpdateContent string
	specUpdateFile    string
	specUpdateMessage string
)

var specUpdateCmd = &cobra.Command{
	Use:     "update",
	Short:   "Update an existing spec file",
	Example: "mesh spec update --project <id> --path specs/my-spec.md --base-sha <sha> --content \"# Updated\"",
	RunE: func(cmd *cobra.Command, _ []string) error {
		sess, err := requireSession()
		if err != nil {
			return err
		}

		if specUpdateProject == "" {
			return fmt.Errorf("--project is required")
		}
		if specUpdatePath == "" {
			return fmt.Errorf("--path is required")
		}
		if specUpdateBaseSHA == "" {
			return fmt.Errorf("--base-sha is required")
		}

		content := specUpdateContent
		if content == "" && specUpdateFile != "" {
			b, err := os.ReadFile(specUpdateFile)
			if err != nil {
				return fmt.Errorf("reading --file: %w", err)
			}
			content = string(b)
		}
		if content == "" {
			return fmt.Errorf("--content or --file is required")
		}

		payload := map[string]any{
			"content":  content,
			"base_sha": specUpdateBaseSHA,
		}
		if specUpdateMessage != "" {
			payload["message"] = specUpdateMessage
		}

		client := newMeshClientForSession(sess)
		var resp map[string]any
		apiPath := fmt.Sprintf("/project/%s/specs/%s", specUpdateProject, strings.TrimPrefix(specUpdatePath, "/"))
		if err := client.doRequest("PUT", apiPath, payload, &resp); err != nil {
			return err
		}
		return printJSON(resp)
	},
}

// ── spec list ─────────────────────────────────────────────────────────────────

var (
	specListProject string
	specListLimit   int
)

var specListCmd = &cobra.Command{
	Use:     "list",
	Short:   "List all spec files in the project's spec repo",
	Example: "mesh spec list --project <id>",
	RunE: func(cmd *cobra.Command, _ []string) error {
		sess, err := requireSession()
		if err != nil {
			return err
		}

		if specListProject == "" {
			return fmt.Errorf("--project is required")
		}

		client := newMeshClientForSession(sess)
		apiPath := fmt.Sprintf("/project/%s/specs?limit=%d", specListProject, specListLimit)

		var resp struct {
			OK    bool `json:"ok"`
			Specs []struct {
				Path         string `json:"path"`
				SHA          string `json:"sha"`
				LastModified string `json:"last_modified"`
				LastAuthor   string `json:"last_author"`
			} `json:"specs"`
		}
		if err := client.doRequest("GET", apiPath, nil, &resp); err != nil {
			return err
		}

		if len(resp.Specs) == 0 {
			fmt.Println("no specs found")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "PATH\tSHA\tLAST MODIFIED\tAUTHOR")
		for _, s := range resp.Specs {
			sha := s.SHA
			if len(sha) > 8 {
				sha = sha[:8]
			}
			modified := s.LastModified
			if len(modified) > 10 {
				modified = modified[:10]
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.Path, sha, modified, s.LastAuthor)
		}
		return w.Flush()
	},
}

// ── spec get ──────────────────────────────────────────────────────────────────

var (
	specGetProject string
	specGetPath    string
)

var specGetCmd = &cobra.Command{
	Use:     "get",
	Short:   "Fetch the content of a spec file",
	Example: "mesh spec get --project <id> --path specs/my-spec.md",
	RunE: func(cmd *cobra.Command, _ []string) error {
		sess, err := requireSession()
		if err != nil {
			return err
		}

		if specGetProject == "" {
			return fmt.Errorf("--project is required")
		}
		if specGetPath == "" {
			return fmt.Errorf("--path is required")
		}

		client := newMeshClientForSession(sess)
		apiPath := fmt.Sprintf("/project/%s/specs/%s", specGetProject, strings.TrimPrefix(specGetPath, "/"))

		var resp struct {
			OK      bool   `json:"ok"`
			Path    string `json:"path"`
			Content string `json:"content"`
			SHA     string `json:"sha"`
			HtmlURL string `json:"html_url"`
			Title   string `json:"title"`
		}
		if err := client.doRequest("GET", apiPath, nil, &resp); err != nil {
			return err
		}

		fmt.Printf("Path:    %s\n", resp.Path)
		fmt.Printf("SHA:     %s\n", resp.SHA)
		fmt.Printf("URL:     %s\n", resp.HtmlURL)
		if resp.Title != "" {
			fmt.Printf("Title:   %s\n", resp.Title)
		}
		fmt.Println()
		fmt.Println(resp.Content)
		return nil
	},
}

func init() {
	specCreateCmd.Flags().StringVar(&specCreateProject, "project", "", "Project ID (required)")
	specCreateCmd.Flags().StringVar(&specCreateTitle, "title", "", "Spec title (required)")
	specCreateCmd.Flags().StringVar(&specCreateContent, "content", "", "Spec markdown content")
	specCreateCmd.Flags().StringVar(&specCreateFile, "file", "", "Read content from a local file")
	specCreateCmd.Flags().StringVar(&specCreatePath, "path", "", "Override the default path (e.g. specs/my-spec.md)")

	specUpdateCmd.Flags().StringVar(&specUpdateProject, "project", "", "Project ID (required)")
	specUpdateCmd.Flags().StringVar(&specUpdatePath, "path", "", "Spec file path (required)")
	specUpdateCmd.Flags().StringVar(&specUpdateBaseSHA, "base-sha", "", "Current blob SHA for optimistic locking (required)")
	specUpdateCmd.Flags().StringVar(&specUpdateContent, "content", "", "Updated markdown content")
	specUpdateCmd.Flags().StringVar(&specUpdateFile, "file", "", "Read updated content from a local file")
	specUpdateCmd.Flags().StringVar(&specUpdateMessage, "message", "", "Commit message (optional)")

	specListCmd.Flags().StringVar(&specListProject, "project", "", "Project ID (required)")
	specListCmd.Flags().IntVar(&specListLimit, "limit", 50, "Maximum number of specs to return")

	specGetCmd.Flags().StringVar(&specGetProject, "project", "", "Project ID (required)")
	specGetCmd.Flags().StringVar(&specGetPath, "path", "", "Spec file path (required)")

	specCmd.AddCommand(specCreateCmd, specUpdateCmd, specListCmd, specGetCmd)
}
