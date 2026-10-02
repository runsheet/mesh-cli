package node

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// NoCredentialRecheck is how long the loop leaves a project's work
// unclaimed after its claim-time read issued no credential for an
// adapter before it claims once more to ask again: an admin attaching a
// credential on the console is picked up without a restart.
const NoCredentialRecheck = 10 * time.Minute

// ErrNoCredential is a claimed session whose claim-time read succeeded
// and issued no credential for the session's adapter (adr-005's
// amendment 2026-10): the session does not start — a node only runs
// sessions on credentials the platform issues, never on the machine's
// own login — and the work is left unclaimed, as for ErrNoAccount.
type ErrNoCredential struct {
	// Org and Project are the slugs named to a person; ProjectID is the
	// project the loop holds the work of.
	Org, Project, ProjectID string
	// Adapter is the session's: claude-code or codex.
	Adapter string
}

func (e *ErrNoCredential) Error() string {
	return fmt.Sprintf("org %s's project %s has no %s credential open to this node", e.Org, e.Project, e.Adapter)
}

func isNoCredential(err error) bool {
	_, ok := NoCredentialOf(err)
	return ok
}

// NoCredentialOf is err's ErrNoCredential, if it is one.
func NoCredentialOf(err error) (*ErrNoCredential, bool) {
	var nc *ErrNoCredential
	ok := errors.As(err, &nc)
	return nc, ok
}

// subject is what a session works for, as a person names it: its org's
// slug and its project, by id and (when the caller has read it) slug.
type subject struct {
	org, projectID, project string
}

type subjectKey struct{}

// withSubject is ctx carrying the org and project of the session about
// to open its account, for an ErrNoCredential to name them. project may
// be empty: Credentials reads the slug by projectID.
func withSubject(ctx context.Context, sc Scope, projectID, project string) context.Context {
	return context.WithValue(ctx, subjectKey{}, subject{org: cmp.Or(sc.Slug, sc.Name()), projectID: projectID, project: project})
}

func subjectFrom(ctx context.Context) subject {
	s, _ := ctx.Value(subjectKey{}).(subject)
	return s
}

// NoCredentials remembers, for this run, the projects whose claim-time
// read issued no credential for an adapter (Note, from Credentials) so
// the loop leaves their goals' items and plans unclaimed (Holds) for
// NoCredentialRecheck before asking again, and says so in node.log once
// per goal per run (Say). The store is shared by Credentials and the
// Loop; a nil one remembers nothing and says every time.
type NoCredentials struct {
	Log *log.Logger
	// Now is the clock (nil: time.Now).
	Now func() time.Time

	mu   sync.Mutex
	held map[noCredKey]noCredHold
	said OncePer
}

type noCredKey struct{ projectID, adapter string }

type noCredHold struct {
	reason string
	at     time.Time
}

func (n *NoCredentials) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now()
}

// Note records that goalID's project issued no credential for e's
// adapter, now, and says it for the goal (Say).
func (n *NoCredentials) Note(goalID string, e *ErrNoCredential) {
	if n == nil {
		return
	}
	n.mu.Lock()
	if n.held == nil {
		n.held = map[noCredKey]noCredHold{}
	}
	n.held[noCredKey{e.ProjectID, e.Adapter}] = noCredHold{reason: e.Error(), at: n.now()}
	n.mu.Unlock()
	n.Say(goalID, e.Error())
}

// Clear forgets a project's hold for an adapter: its read issued one.
func (n *NoCredentials) Clear(projectID, adapter string) {
	if n == nil {
		return
	}
	n.mu.Lock()
	delete(n.held, noCredKey{projectID, adapter})
	n.mu.Unlock()
}

// Holds is the reason a project's work on adapter is left unclaimed,
// while its last Note is younger than NoCredentialRecheck.
func (n *NoCredentials) Holds(projectID, adapter string) (string, bool) {
	if n == nil || projectID == "" {
		return "", false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	h, ok := n.held[noCredKey{projectID, adapter}]
	if !ok || n.now().Sub(h.at) >= NoCredentialRecheck {
		return "", false
	}
	return h.reason, true
}

// Say logs the hold once per goal per run, in the accounts: family of
// lines beside "no account has headroom".
func (n *NoCredentials) Say(goalID, reason string) {
	if n == nil || n.Log == nil || !n.said.First(goalID) {
		return
	}
	n.Log.Printf("accounts: %s; goal %s's items and plans left unclaimed until one is (asked again every %s; said once for the goal)", reason, goalID, NoCredentialRecheck)
}
