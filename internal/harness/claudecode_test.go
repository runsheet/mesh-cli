package harness

import "testing"

// TestArgsModel: the job's model reaches the command line; a job
// without one leaves the CLI's own default alone.
func TestArgsModel(t *testing.T) {
	c := &ClaudeCode{}
	args := c.Args(Job{Prompt: "hi", Model: "sonnet"})
	found := false
	for i, a := range args {
		if a == "--model" && i+1 < len(args) && args[i+1] == "sonnet" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no --model sonnet in %v", args)
	}
	for _, a := range c.Args(Job{Prompt: "hi"}) {
		if a == "--model" {
			t.Fatal("a job with no model must not pass --model")
		}
	}
}
