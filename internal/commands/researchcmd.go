package commands

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

var researchCmd = &cobra.Command{
	Use:   "research",
	Short: "Research lane commands (record, confirm, list)",
}

var (
	researchTaskID  string
	researchNoteFile    string
	researchConfidence  string
	researchProjectID   string
	researchStuck       bool
	researchConfirmerRole string
)

func readNoteInput(noteFlag string) (string, error) {
	if noteFlag == "" {
		return "", fmt.Errorf("--note is required (pass - to read from stdin)")
	}
	if noteFlag == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("reading stdin: %w", err)
		}
		return strings.TrimRight(string(data), "\n"), nil
	}
	data, err := os.ReadFile(noteFlag)
	if err != nil {
		return "", fmt.Errorf("reading note file: %w", err)
	}
	return strings.TrimRight(string(data), "\n"), nil
}

func parseConfidence(v string) (float64, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || f < 0 || f > 1 {
		return 0, fmt.Errorf("--confidence must be a number between 0.0 and 1.0")
	}
	return f, nil
}

var researchRecordCmd = &cobra.Command{
	Use:     "record",
	Short:   "Record a research note for a task",
	Example: "mesh research record --task <id> --note research.md --confidence 0.82\nmesh research record --task <id> --note - --confidence 0.75",
	RunE: func(cmd *cobra.Command, args []string) error {
		if researchTaskID == "" {
			return fmt.Errorf("--task is required")
		}
		confidence, err := parseConfidence(researchConfidence)
		if err != nil {
			return err
		}
		noteMarkdown, err := readNoteInput(researchNoteFile)
		if err != nil {
			return err
		}
		if strings.TrimSpace(noteMarkdown) == "" {
			return fmt.Errorf("note content is empty")
		}

		sess, err := requireSession()
		if err != nil {
			return err
		}

		payload := map[string]any{
			"taskId":      researchTaskID,
			"noteMarkdown":    noteMarkdown,
			"confidenceScore": confidence,
		}

		client := newMeshClientForSession(sess)
		var response map[string]any
		if err := client.doRequest("POST", "/task/research/record", payload, &response); err != nil {
			return err
		}
		return printJSON(response)
	},
}

var researchConfirmCmd = &cobra.Command{
	Use:     "confirm",
	Short:   "Confirm a research note for a task",
	Example: "mesh research confirm --task <id> --confidence 0.75",
	RunE: func(cmd *cobra.Command, args []string) error {
		if researchTaskID == "" {
			return fmt.Errorf("--task is required")
		}
		confidence, err := parseConfidence(researchConfidence)
		if err != nil {
			return err
		}

		sess, err := requireSession()
		if err != nil {
			return err
		}

		payload := map[string]any{
			"taskId":      researchTaskID,
			"confidenceScore": confidence,
		}
		if v := strings.TrimSpace(researchConfirmerRole); v != "" {
			payload["confirmerRole"] = v
		}

		client := newMeshClientForSession(sess)
		var response map[string]any
		if err := client.doRequest("POST", "/task/research/confirm", payload, &response); err != nil {
			return err
		}
		return printJSON(response)
	},
}

var researchListCmd = &cobra.Command{
	Use:     "list",
	Short:   "List tasks pending research (in researching state)",
	Example: "mesh research list\nmesh research list --project <id>",
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := requireSession()
		if err != nil {
			return err
		}

		path := "/task/research/pending"
		if v := strings.TrimSpace(researchProjectID); v != "" {
			path += "?projectId=" + v
		}

		client := newMeshClientForSession(sess)
		var response map[string]any
		if err := client.doRequest("GET", path, nil, &response); err != nil {
			return err
		}
		return printJSON(response)
	},
}

func init() {
	researchRecordCmd.Flags().StringVar(&researchTaskID, "task", "", "Work item ID")
	researchRecordCmd.Flags().StringVar(&researchNoteFile, "note", "", "Path to note file, or - for stdin")
	researchRecordCmd.Flags().StringVar(&researchConfidence, "confidence", "", "Confidence score 0.0–1.0")

	researchConfirmCmd.Flags().StringVar(&researchTaskID, "task", "", "Work item ID")
	researchConfirmCmd.Flags().StringVar(&researchConfidence, "confidence", "", "Confidence score 0.0–1.0")
	researchConfirmCmd.Flags().StringVar(&researchConfirmerRole, "role", "", "Confirmer role (researcher, devlead, pm)")

	researchListCmd.Flags().StringVar(&researchProjectID, "project", "", "Filter by project ID")
	researchListCmd.Flags().BoolVar(&researchStuck, "stuck", false, "Show only items stuck > 7 days (informational filter)")

	researchCmd.AddCommand(researchRecordCmd, researchConfirmCmd, researchListCmd)
}
