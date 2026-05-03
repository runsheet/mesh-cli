package commands

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var testCmd = &cobra.Command{
	Use:   "test",
	Short: "Tester lane commands (record, report)",
}

var (
	testTaskID  string
	testStatus      string
	testLogFile     string
	testConfidence  string
)

var testRecordCmd = &cobra.Command{
	Use:     "record",
	Short:   "Record a test result for a task",
	Example: "mesh test record --task <id> --status pass --confidence 0.91\nmesh test record --task <id> --status fail --log test-output.txt --confidence 0.60",
	RunE: func(cmd *cobra.Command, args []string) error {
		if testTaskID == "" {
			return fmt.Errorf("--task is required")
		}
		if testStatus == "" {
			return fmt.Errorf("--status is required (pass or fail)")
		}
		if testStatus != "pass" && testStatus != "fail" {
			return fmt.Errorf("--status must be pass or fail")
		}
		confidence, err := parseConfidence(testConfidence)
		if err != nil {
			return err
		}

		sess, err := requireSession()
		if err != nil {
			return err
		}

		payload := map[string]any{
			"taskId":      testTaskID,
			"status":          testStatus,
			"confidenceScore": confidence,
		}

		if testLogFile != "" {
			logContent, err := readNoteInput(testLogFile)
			if err != nil {
				return err
			}
			if strings.TrimSpace(logContent) != "" {
				payload["logExcerpt"] = logContent
			}
		}

		client := newMeshClientForSession(sess)
		var response map[string]any
		if err := client.doRequest("POST", "/task/test/record", payload, &response); err != nil {
			return err
		}
		return printJSON(response)
	},
}

var testReportCmd = &cobra.Command{
	Use:     "report",
	Short:   "Fetch the latest test report for a task",
	Example: "mesh test report --task <id>",
	RunE: func(cmd *cobra.Command, args []string) error {
		if testTaskID == "" {
			return fmt.Errorf("--task is required")
		}

		sess, err := requireSession()
		if err != nil {
			return err
		}

		client := newMeshClientForSession(sess)
		var response map[string]any
		if err := client.doRequest("GET", "/task/test/report?taskId="+testTaskID, nil, &response); err != nil {
			return err
		}
		return printJSON(response)
	},
}

func init() {
	testRecordCmd.Flags().StringVar(&testTaskID, "task", "", "Work item ID")
	testRecordCmd.Flags().StringVar(&testStatus, "status", "", "Test status: pass or fail")
	testRecordCmd.Flags().StringVar(&testLogFile, "log", "", "Path to log file, or - for stdin")
	testRecordCmd.Flags().StringVar(&testConfidence, "confidence", "", "Confidence score 0.0–1.0")

	testReportCmd.Flags().StringVar(&testTaskID, "task", "", "Work item ID")

	testCmd.AddCommand(testRecordCmd, testReportCmd)
}
