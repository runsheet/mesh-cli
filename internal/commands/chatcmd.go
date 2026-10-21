package commands

// `mesh chat`: a person's thread with runsheet about a goal (runsheet
// thread.loom, the runsheet/goal-chat kind). Each message is a turn a
// node answers; a draft it proposes is the person's to set — `mesh chat
// accept` sets the goal, in the person's name, and records it on the
// thread.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/gateway"
)

var (
	chatProjectFlag string
	chatThreadFlag  string
	chatWaitFlag    time.Duration
	chatSeqFlag     int64
)

// chatProduct and chatRole are the thread `mesh chat` talks on.
const (
	chatProduct = "runsheet"
	chatRole    = "goal-chat"
)

var chatCmd = &cobra.Command{
	Use:   "chat [message]",
	Short: "Talk to runsheet about a goal you want; it answers and drafts the goal for you to set",
	Long: `Says one message on a goal-chat thread with runsheet about a project, waits
for the answer — a node runs the turn — and prints it, and the goal draft when
it proposes one. Start a thread with --project; continue it with --thread.
Set a draft with mesh chat accept --thread <key>.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), chatWaitFlag+time.Minute)
		defer cancel()
		s, err := goalContext(ctx)
		if err != nil {
			return err
		}
		key, projectID := strings.TrimSpace(chatThreadFlag), ""
		if key == "" {
			if chatProjectFlag == "" {
				return errors.New("start a thread with --project <slug>, or continue one with --thread <key>")
			}
			if projectID, err = chatProject(ctx, s, chatProjectFlag); err != nil {
				return err
			}
			key = uuid.NewString()
			fmt.Printf("thread %s (continue with --thread %s)\n", key, key)
		}
		threadID := gateway.ThreadID(chatProduct, key).String()
		me := s.signIn(ctx)
		if me == "" {
			return errors.New("not signed in: mesh auth login")
		}
		if err := s.gw.SayInThread(ctx, gateway.SayInThread{ThreadID: threadID, Product: chatProduct, ThreadKey: key, OrgID: s.org.ID,
			ProjectID: projectID, Role: chatRole, Body: strings.Join(args, " "), By: me}); err != nil {
			return err
		}
		fmt.Println("waiting for runsheet…")
		t, err := chatWait(ctx, s.gw, threadID, chatWaitFlag)
		if err != nil {
			return err
		}
		printChatReply(t, key)
		return nil
	},
}

var chatAcceptCmd = &cobra.Command{
	Use:   "accept",
	Short: "Set the goal a thread's last draft proposes, as you, and record it on the thread",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if chatThreadFlag == "" {
			return errors.New("--thread <key> names the thread")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		s, err := goalContext(ctx)
		if err != nil {
			return err
		}
		threadID := gateway.ThreadID(chatProduct, chatThreadFlag).String()
		t, err := s.gw.ThreadMessages(ctx, threadID)
		if err != nil {
			return err
		}
		m, ok := chatProposal(t, chatSeqFlag)
		if !ok {
			return errors.New("the thread proposes no goal yet")
		}
		if m.GoalID != "" {
			return fmt.Errorf("message %d's draft is already goal %s", m.Seq, m.GoalID)
		}
		var d chatDraft
		if err := json.Unmarshal([]byte(m.Proposal), &d); err != nil {
			return fmt.Errorf("the draft: %w", err)
		}
		if d.Slug == "" || d.Title == "" {
			return errors.New("the draft names no slug or title")
		}
		if t.ProjectID == "" {
			return errors.New("the thread names no project")
		}
		me := s.signIn(ctx)
		id, err := s.goalID(d.Slug)
		if err != nil {
			return err
		}
		if err := s.gw.SetGoal(ctx, gateway.SetGoalInput{GoalID: id, OrgID: s.org.ID, ProjectID: t.ProjectID, Slug: d.Slug, Title: d.Title,
			Statement: d.Statement, Acceptance: d.Acceptance, Outcome: d.Outcome, Why: d.Why, WhoFor: d.WhoFor, Appetite: d.Appetite,
			DoneWhen: d.DoneWhen, NotDoing: d.NotDoing, NoInitiativeReason: "drafted on a goal-chat thread", By: me}); err != nil {
			return err
		}
		if err := s.gw.RecordThreadProposalAccepted(ctx, threadID, m.Seq, id, me); err != nil {
			return fmt.Errorf("goal %s set, but recording it on the thread failed: %w", d.Slug, err)
		}
		fmt.Printf("Goal %s set (%s) from message %d.\n", d.Slug, id, m.Seq)
		return nil
	},
}

// chatDraft is a goal-chat proposal (harness.GoalChatPrompt's shape).
type chatDraft struct {
	Slug       string `json:"slug"`
	Title      string `json:"title"`
	Outcome    string `json:"outcome"`
	Why        string `json:"why"`
	WhoFor     string `json:"whoFor"`
	Appetite   string `json:"appetite"`
	DoneWhen   string `json:"doneWhen"`
	NotDoing   string `json:"notDoing"`
	Statement  string `json:"statement"`
	Acceptance string `json:"acceptance"`
}

// chatProject is the id of the org's project of slug.
func chatProject(ctx context.Context, s *goalScope, slug string) (string, error) {
	projects, err := s.gw.Projects(ctx, s.org.ID)
	if err != nil {
		return "", err
	}
	for _, p := range projects {
		if p.Slug == slug {
			return p.ID, nil
		}
	}
	return "", fmt.Errorf("no project %q in %s", slug, s.org.Name())
}

// chatWait polls the thread until runsheet has answered its last message.
func chatWait(ctx context.Context, gw *gateway.Client, threadID string, wait time.Duration) (*gateway.Thread, error) {
	deadline := time.Now().Add(wait)
	for {
		t, err := gw.ThreadMessages(ctx, threadID)
		if err != nil {
			return nil, err
		}
		if !t.Waiting() {
			return t, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("no answer after %s: is a node serving the org running? (mesh node status)", wait)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// chatProposal is the thread's message seq when it proposes, else its
// last proposal.
func chatProposal(t *gateway.Thread, seq int64) (gateway.ThreadMessage, bool) {
	for i := len(t.Messages) - 1; i >= 0; i-- {
		m := t.Messages[i]
		if m.Proposal != "" && (seq == 0 || m.Seq == seq) {
			return m, true
		}
	}
	return gateway.ThreadMessage{}, false
}

// printChatReply prints the thread's last message: runsheet's answer and
// the draft it proposes.
func printChatReply(t *gateway.Thread, key string) {
	if len(t.Messages) == 0 {
		return
	}
	m := t.Messages[len(t.Messages)-1]
	if m.Body != "" {
		fmt.Printf("\nrunsheet: %s\n", m.Body)
	}
	if m.Proposal != "" {
		var d chatDraft
		if json.Unmarshal([]byte(m.Proposal), &d) == nil && d.Slug != "" {
			fmt.Printf("\nDraft goal %s: %s\n", d.Slug, d.Title)
			for _, f := range [][2]string{{"Outcome", d.Outcome}, {"Why", d.Why}, {"For", d.WhoFor}, {"Appetite", d.Appetite},
				{"Done when", d.DoneWhen}, {"Not doing", d.NotDoing}, {"Statement", d.Statement}, {"Acceptance", d.Acceptance}} {
				if f[1] != "" {
					fmt.Printf("  %s: %s\n", f[0], f[1])
				}
			}
		} else {
			fmt.Printf("\nDraft: %s\n", m.Proposal)
		}
		fmt.Printf("\nSet it with: mesh chat accept --thread %s\n", key)
	}
}

func init() {
	chatCmd.PersistentFlags().StringVar(&goalOrgFlag, "org", "", "the org's GitHub login or slug (default: the registered node's)")
	chatCmd.PersistentFlags().StringVar(&gatewayFlag, "gateway", "", "platform gateway URL (default "+gateway.DefaultURL+", or MESH_GATEWAY)")
	chatCmd.PersistentFlags().StringVar(&chatThreadFlag, "thread", "", "the thread to continue (its key)")
	chatCmd.Flags().StringVar(&chatProjectFlag, "project", "", "the project a new thread is about (its slug)")
	chatCmd.Flags().DurationVar(&chatWaitFlag, "wait", 10*time.Minute, "how long to wait for the answer")
	chatAcceptCmd.Flags().Int64Var(&chatSeqFlag, "seq", 0, "the message whose draft to set (default: the last draft)")
	chatCmd.AddCommand(chatAcceptCmd)
	rootCmd.AddCommand(chatCmd)
}
