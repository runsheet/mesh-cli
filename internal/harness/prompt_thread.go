package harness

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ThreadMessage is one message of a thread's transcript, as the turn's
// body carries it (runsheet aggregates.ThreadTranscript).
type ThreadMessage struct {
	Seq      int64  `json:"seq"`
	From     string `json:"from"`
	Body     string `json:"body,omitempty"`
	Proposal string `json:"proposal,omitempty"`
	Accepted bool   `json:"accepted,omitempty"`
}

// ThreadTranscript is a turn's body: the product, the role and every
// message so far.
type ThreadTranscript struct {
	Product  string          `json:"product"`
	Role     string          `json:"role"`
	Messages []ThreadMessage `json:"messages"`
}

// ParseThreadTranscript reads a turn's body.
func ParseThreadTranscript(body string) (ThreadTranscript, error) {
	var t ThreadTranscript
	if err := json.Unmarshal([]byte(body), &t); err != nil {
		return t, fmt.Errorf("thread transcript: %w", err)
	}
	if len(t.Messages) == 0 {
		return t, fmt.Errorf("thread transcript: no messages")
	}
	return t, nil
}

// ThreadInput is what a thread turn's prompt is made of.
type ThreadInput struct {
	Org, Project string
	Transcript   ThreadTranscript
	// MeshContext says the runsheet tools are attached.
	MeshContext bool
	Tools       []string
}

// GoalChatPrompt is the prompt of a runsheet/goal-chat turn: the role's
// guidance, the conversation so far, and the answer's shape — the reply
// to the person and, when enough is known, a goal draft they set
// themselves. The role never sets a goal.
func GoalChatPrompt(in ThreadInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are runsheet's goal assistant for %s, project %s. A person is talking to you on a thread about a goal they want. Help them shape it into one runsheet can plan and build: understand what they want and why, check what already exists, ask what is unclear, and when you know enough, draft the goal.\n\n", in.Org, in.Project)
	if in.MeshContext {
		b.WriteString("Use the runsheet tools before you answer: `mesh_goals` for the project's goals (is this one already there, or close to one?), `mesh_documents` and `mesh_search` for its PRD, ADRs and documents. Say what you found when it matters to the person.\n\n")
	}
	if len(in.Tools) > 0 {
		fmt.Fprintf(&b, "You also have: %s.\n\n", strings.Join(in.Tools, ", "))
	}
	b.WriteString(`How to talk:
- Short, plain sentences. One or two questions at a time, only the ones that change the goal.
- Never set, plan or change a goal yourself: you propose; the person sets it.
- When you propose, the draft is a runsheet goal written for people: a slug (kebab-case), a title of at most 60 characters, the outcome (what is true when it is done), why it matters, who it is for, the appetite (how much it is worth, e.g. "$40" or "a day"), "done when" (what a person sees when it is done), what is not being done, a statement for the builder, and an acceptance whose every clause names its proof (a test that passes, a read and its value, a grep that returns nothing, a check or a count).

`)
	b.WriteString("The conversation so far (the last message is the person's, waiting for you):\n\n")
	for _, m := range in.Transcript.Messages {
		who := "Person"
		if m.From == "agent" {
			who = "You"
		}
		if m.Body != "" {
			fmt.Fprintf(&b, "%s: %s\n", who, m.Body)
		}
		if m.Proposal != "" {
			state := "not set yet"
			if m.Accepted {
				state = "the person set it"
			}
			fmt.Fprintf(&b, "%s proposed (%s): %s\n", who, state, m.Proposal)
		}
	}
	b.WriteString(`
End your reply with one JSON block and nothing after it:

` + "```json" + `
{"answer": "what you say to the person", "proposal": null}
` + "```" + `

or, with a draft:

` + "```json" + `
{"answer": "a sentence on the draft", "proposal": {"slug": "", "title": "", "outcome": "", "why": "", "whoFor": "", "appetite": "", "doneWhen": "", "notDoing": "", "statement": "", "acceptance": ""}}
` + "```" + `
`)
	return b.String()
}

// ThreadReply is a thread turn's answer as its session ended it: what it
// says to the person and, when it drafted one, its proposal as JSON.
type ThreadReply struct {
	Answer   string
	Proposal string
}

// ParseThreadReply reads the last JSON block of a turn's final message
// (GoalChatPrompt's shape). A message with no block is its own answer.
func ParseThreadReply(text string) (ThreadReply, error) {
	text = strings.TrimSpace(text)
	i := strings.LastIndex(text, "```json")
	if i < 0 {
		if text == "" {
			return ThreadReply{}, fmt.Errorf("the session said nothing")
		}
		return ThreadReply{Answer: text}, nil
	}
	block := text[i+len("```json"):]
	if j := strings.Index(block, "```"); j >= 0 {
		block = block[:j]
	}
	var out struct {
		Answer   string          `json:"answer"`
		Proposal json.RawMessage `json:"proposal"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(block)), &out); err != nil {
		return ThreadReply{}, fmt.Errorf("the reply's JSON block: %w", err)
	}
	r := ThreadReply{Answer: strings.TrimSpace(out.Answer)}
	if p := strings.TrimSpace(string(out.Proposal)); p != "" && p != "null" {
		r.Proposal = p
	}
	if r.Answer == "" && r.Proposal == "" {
		return ThreadReply{}, fmt.Errorf("the reply's JSON block says nothing")
	}
	return r, nil
}
