package node

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
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

// ErrNoHeadroom is a claimed session whose claim-time read issued
// credentials for its adapter and none of which may take it — each
// stopped, paced for its priority or, for an api key, at its cap
// (adr-005's amendment 2026-10-03, goal
// no-headroom-is-a-hold-not-a-claim-loop). It is ErrNoAccount
// (errors.Is), so every caller that releases on ErrNoAccount still
// does; the loop holds the project's work of that kind on that adapter
// unclaimed until Until (NoCredentials.NoteHeadroom).
type ErrNoHeadroom struct {
	// Org and Project are the slugs named to a person; ProjectID is the
	// project the loop holds the work of.
	Org, Project, ProjectID string
	// Kind is the session's (gateway.SessionRef.Kind: build, evaluate,
	// plan, research, rebase); Adapter is claude-code or codex.
	Kind, Adapter string
	// Refused are the labels of the adapter's issued credentials the
	// session was refused on.
	Refused []string
	// Until is the earliest a refused credential resumes, by its
	// reading (pace); zero when none of them has a reading that says.
	Until time.Time
	// resumes are each refused label's resume time, for Error.
	resumes map[string]time.Time
}

func (e *ErrNoHeadroom) Error() string {
	var named []string
	for _, l := range e.Refused {
		if at, ok := e.resumes[l]; ok && !at.IsZero() {
			named = append(named, fmt.Sprintf("%s exhausted until %s", l, at.UTC().Format(time.RFC3339)))
		} else {
			named = append(named, l+" exhausted")
		}
	}
	return fmt.Sprintf("no issued credential has headroom for %s on org %s's project %s (%s)", cmp.Or(e.Kind, "the session"), e.Org, e.Project, strings.Join(named, ", "))
}

// Is makes an ErrNoHeadroom ErrNoAccount.
func (e *ErrNoHeadroom) Is(target error) bool { return target == ErrNoAccount }

// NoHeadroomOf is err's ErrNoHeadroom, if it is one.
func NoHeadroomOf(err error) (*ErrNoHeadroom, bool) {
	var nh *ErrNoHeadroom
	ok := errors.As(err, &nh)
	return nh, ok
}

// NoCredentials remembers, for this run, the projects whose claim-time
// read issued no credential for an adapter (Note) or issued only
// credentials with no headroom for a kind of session (NoteHeadroom), so
// the loop leaves their work unclaimed (Holds) until the hold's until
// before asking again, and says so in node.log once per goal per run
// (Say). A hold is keyed by project, kind and adapter; kind "" is the
// no-credential hold, which covers every kind. The store is shared by
// Credentials and the Loop; a nil one remembers nothing and says every
// time.
type NoCredentials struct {
	Log *log.Logger
	// Now is the clock (nil: time.Now).
	Now func() time.Time

	mu   sync.Mutex
	held map[noCredKey]noCredHold
	said OncePer
}

type noCredKey struct{ projectID, kind, adapter string }

type noCredHold struct {
	reason string
	until  time.Time
}

func (n *NoCredentials) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now()
}

func (n *NoCredentials) hold(k noCredKey, reason string, until time.Time) {
	n.mu.Lock()
	if n.held == nil {
		n.held = map[noCredKey]noCredHold{}
	}
	n.held[k] = noCredHold{reason: reason, until: until}
	n.mu.Unlock()
}

// Note records that goalID's project issued no credential for e's
// adapter, now, holding every kind of its work for NoCredentialRecheck,
// and says it for the goal (Say).
func (n *NoCredentials) Note(goalID string, e *ErrNoCredential) {
	if n == nil {
		return
	}
	until := n.now().Add(NoCredentialRecheck)
	n.hold(noCredKey{e.ProjectID, "", e.Adapter}, e.Error(), until)
	n.Say(goalID, e.Error(), until)
}

// NoteHeadroom records that goalID's project issued only credentials
// with no headroom for e's kind on e's adapter, holding that kind until
// e.Until (NoCredentialRecheck from now when it is zero or past), and
// says it for the goal (Say).
func (n *NoCredentials) NoteHeadroom(goalID string, e *ErrNoHeadroom) {
	if n == nil {
		return
	}
	now := n.now()
	until := e.Until
	if !until.After(now) {
		until = now.Add(NoCredentialRecheck)
	}
	n.hold(noCredKey{e.ProjectID, e.Kind, e.Adapter}, e.Error(), until)
	n.Say(goalID, e.Error(), until)
}

// Clear forgets a project's hold for a kind on an adapter (kind "": the
// no-credential hold): its read issued one, or one with headroom.
func (n *NoCredentials) Clear(projectID, kind, adapter string) {
	if n == nil {
		return
	}
	n.mu.Lock()
	delete(n.held, noCredKey{projectID, kind, adapter})
	n.mu.Unlock()
}

// Holds is the reason a project's work of kind on adapter is left
// unclaimed and until when: its kind's own hold or the kind-"" one,
// while now is before the hold's until. Once it is not, the one call
// that finds it so is let through as the probe — its claim-time read
// asks again — and the hold is renewed for NoCredentialRecheck at once,
// so every other item and plan of the project stays unclaimed until the
// probe's read resolves it (Clear, or Note again) or another
// NoCredentialRecheck passes: one read per project, kind and adapter at
// a time, never one per ready item.
func (n *NoCredentials) Holds(projectID, kind, adapter string) (reason string, until time.Time, held bool) {
	if n == nil || projectID == "" {
		return "", time.Time{}, false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.now()
	keys := []noCredKey{{projectID, "", adapter}}
	if kind != "" {
		keys = append(keys, noCredKey{projectID, kind, adapter})
	}
	for _, k := range keys {
		h, ok := n.held[k]
		if !ok {
			continue
		}
		if !now.Before(h.until) {
			h.until = now.Add(NoCredentialRecheck)
			n.held[k] = h
			return "", time.Time{}, false
		}
		return h.reason, h.until, true
	}
	return "", time.Time{}, false
}

// Say logs the hold once per goal per run, in the accounts: family of
// lines beside "no account has headroom", naming the reason and until
// when (zero: not known).
func (n *NoCredentials) Say(goalID, reason string, until time.Time) {
	if n == nil || n.Log == nil || !n.said.First(goalID) {
		return
	}
	when := "until one is"
	if !until.IsZero() {
		when = "until " + until.UTC().Format(time.RFC3339)
	}
	n.Log.Printf("accounts: %s; goal %s's work left unclaimed %s (asked again then, and every %s after; said once for the goal)", reason, goalID, when, NoCredentialRecheck)
}
