package harness

import (
	"strings"
	"testing"
)

var chatTranscript = ThreadTranscript{Product: "runsheet", Role: "goal-chat", Messages: []ThreadMessage{
	{Seq: 1, From: "person", Body: "I want reports exported as PDF"},
	{Seq: 2, From: "agent", Body: "Which reports?", Proposal: `{"slug":"pdf-export"}`},
	{Seq: 3, From: "person", Body: "All of them"},
}}

const finishLine = "# How this job finishes\n\nCall the mesh-node server's `mesh_result` tool once.\n"

// A thread prompt is the brief's guidance first, verbatim, then the
// turn's facts — whose thread, the tools, the transcript — then the
// finish; none of the node's role text is in it.
func TestThreadPromptIsGuidanceThenSubjectThenFinish(t *testing.T) {
	guidance := "You are Wardroom's assistant.\n\nAnswer the channel plainly.\n"
	p := ThreadPrompt(ThreadInput{Org: "acme", Project: "mesh", Transcript: chatTranscript, Tools: []string{"brave"}, Guidance: guidance, Finish: finishLine})
	if !strings.HasPrefix(p, strings.TrimSpace(guidance)+"\n\n# The thread") {
		t.Fatalf("the prompt does not open with the guidance:\n%s", p)
	}
	if !strings.HasSuffix(p, finishLine) {
		t.Fatalf("the prompt does not end with the finish:\n%s", p)
	}
	for _, want := range []string{"A thread of acme, project mesh on runsheet; you answer as its goal-chat", "you also have: brave", "Person: I want reports exported as PDF", "You: Which reports?", "You proposed (not set yet): {\"slug\":\"pdf-export\"}", "Person: All of them"} {
		if !strings.Contains(p, want) {
			t.Fatalf("the prompt lacks %q:\n%s", want, p)
		}
	}
	for _, gone := range []string{"You are runsheet's goal assistant", "How to talk", "```json", "End your reply"} {
		if strings.Contains(p, gone) {
			t.Fatalf("the prompt carries the node's role text %q:\n%s", gone, p)
		}
	}
	// goal-chat on a brief with guidance is the same prompt
	if g := GoalChatPrompt(ThreadInput{Org: "acme", Project: "mesh", Transcript: chatTranscript, Tools: []string{"brave"}, Guidance: guidance, Finish: finishLine}); g != p {
		t.Fatalf("goal-chat with guidance differs:\n%s", g)
	}
}

// Without guidance, goal-chat runs the built-in role text, with the
// finish by tool call in place of the JSON block.
func TestGoalChatPromptFallsBackToTheBuiltInText(t *testing.T) {
	p := GoalChatPrompt(ThreadInput{Org: "acme", Project: "mesh", Transcript: chatTranscript, MeshContext: true, Finish: finishLine})
	for _, want := range []string{"You are runsheet's goal assistant for acme, project mesh", "Use the runsheet tools before you answer", "How to talk:", "Person: I want reports exported as PDF", finishLine} {
		if !strings.Contains(p, want) {
			t.Fatalf("the built-in prompt lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "```json") || strings.Contains(p, "End your reply") {
		t.Fatalf("the built-in prompt still ends with the JSON block:\n%s", p)
	}
}
