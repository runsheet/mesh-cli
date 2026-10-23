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

// ThreadInput is what a thread turn's prompt is made of: the brief's
// guidance, the turn's facts and the finish.
type ThreadInput struct {
	Org, Project string
	Transcript   ThreadTranscript
	// MeshContext says the runsheet tools are attached.
	MeshContext bool
	Tools       []string
	// Guidance is the kind's guidance text as the brief carries it
	// (runsheet ADR 067 decision 7): the role's instructions, published
	// by the product. It goes first; the node adds only the job's facts
	// and the finish after it. "" is a brief from a platform before the
	// guidance, which gets the built-in prompt instead.
	Guidance string
	// Finish is the one fixed section every session ends on: how this
	// job finishes (jobresult.FinishSection), by the node's mesh_result.
	Finish string
}

// ThreadPrompt is a thread turn's prompt for any kind (ADR-041, the step
// runner): the brief's guidance first, verbatim; then the turn's facts
// — whose thread it is, the tools beside runsheet, the transcript; then
// the finish. Nothing of the role is the node's.
func ThreadPrompt(in ThreadInput) string {
	var b strings.Builder
	if g := strings.TrimSpace(in.Guidance); g != "" {
		b.WriteString(g)
		b.WriteString("\n\n")
	}
	threadSubject(&b, in)
	b.WriteString(in.Finish)
	return b.String()
}

// GoalChatPrompt is the prompt of a runsheet/goal-chat turn: the
// brief's guidance first (ThreadPrompt), or, on a brief that carries
// none, the built-in role text the kind ran on before the platform
// published its guidance — kept word for word as the fallback, with
// the finish by tool call in place of the JSON block it used to end
// with.
func GoalChatPrompt(in ThreadInput) string {
	if strings.TrimSpace(in.Guidance) != "" {
		return ThreadPrompt(in)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "You are runsheet's goal assistant for %s, project %s. A person is talking to you on a thread about a goal they want. Help them shape it into one runsheet can plan and build: understand what they want and why, check what already exists, ask what is unclear, and when you know enough, draft the goal.\n\n", in.Org, in.Project)
	if in.MeshContext {
		b.WriteString("Use the runsheet tools before you answer: `mesh_goals` for the project's goals (is this one already there, or close to one?), `mesh_documents` and `mesh_search` for its PRD, ADRs and documents. Say what you found when it matters to the person.\n\n")
	}
	b.WriteString(`How to talk:
- Short, plain sentences. One or two questions at a time, only the ones that change the goal.
- Never set, plan or change a goal yourself: you propose; the person sets it.
- When you propose, the draft is a runsheet goal written for people: a slug (kebab-case), a title of at most 60 characters, the outcome (what is true when it is done), why it matters, who it is for, the appetite (how much it is worth, e.g. "$40" or "a day"), "done when" (what a person sees when it is done), what is not being done, a statement for the builder, and an acceptance whose every clause names its proof (a test that passes, a read and its value, a grep that returns nothing, a check or a count).

`)
	threadSubject(&b, in)
	b.WriteString(in.Finish)
	return b.String()
}

// threadSubject writes the turn's facts: whose thread, the tools served
// beside runsheet, and the conversation so far.
func threadSubject(b *strings.Builder, in ThreadInput) {
	b.WriteString("# The thread\n\n")
	where := in.Org
	if in.Project != "" {
		where += ", project " + in.Project
	}
	fmt.Fprintf(b, "A thread of %s", where)
	if p := in.Transcript.Product; p != "" {
		fmt.Fprintf(b, " on %s", p)
	}
	if r := in.Transcript.Role; r != "" {
		fmt.Fprintf(b, "; you answer as its %s", r)
	}
	b.WriteString(". The last message is the person's, waiting for you.\n\n")
	if len(in.Tools) > 0 {
		fmt.Fprintf(b, "Beside the runsheet server you also have: %s.\n\n", strings.Join(in.Tools, ", "))
	}
	b.WriteString("## The conversation so far\n\n")
	for _, m := range in.Transcript.Messages {
		who := "Person"
		if m.From == "agent" {
			who = "You"
		}
		if m.Body != "" {
			fmt.Fprintf(b, "%s: %s\n", who, m.Body)
		}
		if m.Proposal != "" {
			state := "not set yet"
			if m.Accepted {
				state = "the person set it"
			}
			fmt.Fprintf(b, "%s proposed (%s): %s\n", who, state, m.Proposal)
		}
	}
	b.WriteString("\n")
}
