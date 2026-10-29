package gateway

// The job system's three calls (runsheet ADR 061 §3–§4): claimJob, which
// claims a Job and answers its brief; jobAccess, the one exchange of
// every token the job's access names; and finishJob, the one finish,
// shaped by the kind. The node reads the kind from the brief and never
// switches on it to decide what a job may reach or how it ends.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// JobSubject is what a job is for: its type (item, goal, direction,
// candidate, main, deploy, thread) and id.
type JobSubject struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// JobCode is what a job runs on (ADR 061 §1): form commit (a repo's
// commit: RepoID, Repo, BaseRef, Branch, BaseSHA, SHA), changeset, or
// none.
type JobCode struct {
	Form        string `json:"form"`
	RepoID      string `json:"repoId,omitempty"`
	Repo        string `json:"repo,omitempty"`
	BaseRef     string `json:"baseRef,omitempty"`
	Branch      string `json:"branch,omitempty"`
	BaseSHA     string `json:"baseSha,omitempty"`
	SHA         string `json:"sha,omitempty"`
	ChangesetID string `json:"changesetId,omitempty"`
	// Onto, Apply, ApplyMode, ApplyAs and PushTo ask the node to validate
	// the would-be merge rather than the change's own base (runsheet ADR
	// 077): the changes in Apply applied in order onto Onto
	// (gitapply.Apply), linear or merge, written as ApplyAs, and the
	// result pushed to PushTo when it names a ref. A brief without Onto is
	// validated as it always was.
	Onto      *JobOnto    `json:"onto,omitempty"`
	Apply     []JobApply  `json:"apply,omitempty"`
	ApplyMode string      `json:"applyMode,omitempty"`
	ApplyAs   *JobApplyAs `json:"applyAs,omitempty"`
	PushTo    string      `json:"pushTo,omitempty"`
}

// ApplyMode values: linear rebases the change onto Onto as one commit,
// merge merges it.
const (
	ApplyLinear = "linear"
	ApplyMerge  = "merge"
)

// JobOnto is the commit a brief's change is applied onto: a ref, a
// sha, or both (the sha then being the ref's head when the platform
// read it).
type JobOnto struct {
	Ref string `json:"ref,omitempty"`
	SHA string `json:"sha,omitempty"`
}

// JobApply is one change a brief applies: Base..SHA, read from Ref
// when the node has to fetch it; Base "" is the merge base with the tip.
type JobApply struct {
	SHA  string `json:"sha,omitempty"`
	Ref  string `json:"ref,omitempty"`
	Base string `json:"base,omitempty"`
}

// JobApplyAs is the author and committer of the commits the node
// writes applying a brief's changes: fixed, so every node writes the
// same shas. Date is RFC 3339.
type JobApplyAs struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
	Date  string `json:"date,omitempty"`
}

// JobPod is one pod the brief names (ADR 061 §6): built from the repo's
// image and services (From repo) holding the job's code, or from the
// kind's tools with no code (From kind); a validate job's pods name the
// suites each runs. Why says why a per-suite-group kind fell back to one.
type JobPod struct {
	Name   string   `json:"name"`
	From   string   `json:"from"`
	Code   bool     `json:"code"`
	Suites []string `json:"suites,omitempty"`
	Why    string   `json:"why,omitempty"`
}

// JobBrief is a claimed Job (ADR 061 §1).
type JobBrief struct {
	Kind string `json:"kind"`
	// CredentialRef is the account link Mesh picked for the job at claim
	// (runsheet ADR 078 decision 5): the node reads the job's secret from
	// Latchkey's for-job route by it rather than from its own stored
	// accounts; "" when Mesh picked none.
	CredentialRef string         `json:"credential_ref,omitempty"`
	Subject       JobSubject     `json:"subject"`
	Code          JobCode        `json:"code"`
	Needs         string         `json:"needs"`
	Access        []string       `json:"access"`
	Harness       string         `json:"harness"`
	Harnesses     []string       `json:"harnesses"`
	Budget        map[string]any `json:"budget"`
	Pods          []JobPod       `json:"pods"`
	Finish        string         `json:"finish"`
	Guidance      string         `json:"guidance"`
	// FinishSchema is the product's JSON Schema for its part of the
	// result (runsheet ADR 067 decision 10): the proposal of an
	// answer-or-proposal kind. "" when the kind has none.
	FinishSchema string `json:"finishSchema"`
	// FinishTool and BriefTool are tools on the product's own MCP server
	// (the server whose name is the kind's product): the one the session
	// hands its result to, and the one it reads its subject from. The node
	// names them to the session and knows nothing else of them; "" when
	// the kind names none.
	FinishTool string `json:"finishTool,omitempty"`
	BriefTool  string `json:"briefTool,omitempty"`
	// Git is what the job's git may reach, as the product grants it: the
	// repository, read or write, and the refs a push may touch. The node
	// enforces it through its relay and reads no meaning into the refs.
	Git *JobGit `json:"git,omitempty"`
	// Claim is the older claim the platform routed this one to.
	Claim string `json:"claim"`
	// Thread is a thread job's turn: the product, the role, the project
	// and the transcript it is answered from; nil for any other job.
	Thread *JobThread `json:"thread,omitempty"`
	// MCP are the MCP servers the job's session reaches, each by name
	// and URL, as the platform names them.
	MCP []JobMCP `json:"mcp,omitempty"`
}

// JobGit is a brief's git policy.
type JobGit struct {
	Repo   string       `json:"repo"`
	Access string       `json:"access"`
	Push   []JobPushRef `json:"push,omitempty"`
}

// JobPushRef is one ref a job's push may touch: exact, or a prefix
// ending "/*"; CreateOnly may create it but never move it.
type JobPushRef struct {
	Ref        string `json:"ref"`
	CreateOnly bool   `json:"create_only"`
}

// Pin is the one exact, create-only ref the brief grants a push — the
// job's pin, where its result's commit is kept — or "" when it grants
// none or more than one.
func (b JobBrief) Pin() string {
	if b.Git == nil {
		return ""
	}
	pin := ""
	for _, r := range b.Git.Push {
		if !r.CreateOnly || strings.HasSuffix(r.Ref, "*") {
			continue
		}
		if pin != "" {
			return ""
		}
		pin = r.Ref
	}
	return pin
}

// JobMCP is one MCP server a brief names.
type JobMCP struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// JobThread is a thread turn as the brief carries it.
type JobThread struct {
	Product   string `json:"product"`
	Role      string `json:"role"`
	ThreadKey string `json:"threadKey"`
	// Ask is the turn's thread and message, <thread id>#<seq>.
	Ask       string `json:"ask"`
	Body      string `json:"body"`
	ProjectID string `json:"projectId,omitempty"`
}

// HasHarness says the job runs an agent.
func (b JobBrief) HasHarness() bool { return b.Harness != "" && b.Harness != "none" }

// Product is the product a kind belongs to: the part of a qualified
// kind ("<product>/<kind>") before the slash, "" for a bare kind.
func (b JobBrief) Product() string {
	if i := strings.IndexByte(b.Kind, '/'); i > 0 {
		return b.Kind[:i]
	}
	return ""
}

// HandInSection is the prompt's last section for a session that hands
// its result to the product's own finish tool rather than to the node:
// the one line naming the tool and its server; "" when the brief names
// no finish tool.
func (b JobBrief) HandInSection() string {
	if b.FinishTool == "" {
		return ""
	}
	server := b.Product()
	if server == "" {
		server = "product"
	}
	return fmt.Sprintf("# How this job finishes\n\nHand your result in with `%s` on the %s MCP server. Nothing else hands it in: when that call succeeds, end the session.", b.FinishTool, server)
}

// ProductGuidance is the brief's guidance with one line after it naming
// the product's tools: where the session reads its subject and, when
// handsIn (the session hands its result to the product, not to the
// node), where it hands it in. The node passes the names through; what
// they do is the product's to say. Guidance alone when the brief names
// neither, and "" when it carries no guidance (the built-in prompt runs).
func (b JobBrief) ProductGuidance(handsIn bool) string {
	g := strings.TrimSpace(b.Guidance)
	if g == "" {
		return ""
	}
	server := b.Product()
	if server == "" {
		server = "product"
	}
	var parts []string
	if b.BriefTool != "" {
		parts = append(parts, fmt.Sprintf("read your subject with `%s` on the %s MCP server", b.BriefTool, server))
	}
	if handsIn && b.FinishTool != "" {
		parts = append(parts, fmt.Sprintf("hand your result in with `%s` on the %s MCP server", b.FinishTool, server))
	}
	if len(parts) == 0 {
		return g
	}
	line := strings.Join(parts, "; ")
	return g + "\n\n" + strings.ToUpper(line[:1]) + line[1:] + "."
}

// Reaches says the job's access names entry.
func (b JobBrief) Reaches(entry string) bool {
	for _, a := range b.Access {
		if a == entry {
			return true
		}
	}
	return false
}

// ClaimJobInput is claimJob's input. Kind may be left out where the
// subject's type says it; Harness is the kind's first when "".
type ClaimJobInput struct {
	NodeID    string
	Kind      string
	Subject   JobSubject
	Harness   string
	Actor     string
	SessionID string
}

func (in ClaimJobInput) asMap() map[string]any {
	m := map[string]any{"nodeId": in.NodeID, "subject": map[string]any{"type": in.Subject.Type, "id": in.Subject.ID}}
	for k, v := range map[string]string{"kind": in.Kind, "harness": in.Harness, "actor": in.Actor, "sessionId": in.SessionID} {
		if v != "" {
			m[k] = v
		}
	}
	return m
}

// ClaimJob claims a job and answers its brief. A refusal (another node
// holds it, the subject is not ready) is a *Error the old claim's
// refusal is.
func (c *Client) ClaimJob(ctx context.Context, in ClaimJobInput) (*JobBrief, error) {
	var out struct {
		Answer struct {
			Status string    `json:"status"`
			Job    *JobBrief `json:"job"`
		} `json:"claimJob"`
	}
	if err := c.Query(ctx, `mutation($in: Map!) { claimJob(input: $in) }`, map[string]any{"in": in.asMap()}, &out); err != nil {
		return nil, err
	}
	if out.Answer.Job == nil {
		return nil, fmt.Errorf("claimJob %s %s: %s, no job in the answer", in.Subject.Type, in.Subject.ID, out.Answer.Status)
	}
	c.keepBrief(out.Answer.Job)
	return out.Answer.Job, nil
}

// maxBriefs bounds the briefs a client keeps: a node holds a few dozen
// jobs at once, never hundreds, so past it the oldest are let go.
const maxBriefs = 256

// keepBrief keeps a claimed job's brief for JobBriefOf: a claim the
// node's code sends as an older mutation (routeJob) reads its guidance,
// finish and finishSchema back by the job's kind and subject.
func (c *Client) keepBrief(b *JobBrief) {
	if b == nil || b.Subject.ID == "" {
		return
	}
	c.briefsMu.Lock()
	defer c.briefsMu.Unlock()
	if c.briefs == nil {
		c.briefs = map[string]*JobBrief{}
	}
	if len(c.briefs) >= maxBriefs {
		c.briefs = map[string]*JobBrief{}
	}
	c.briefs[briefKey(b.Kind, b.Subject)] = b
}

func briefKey(kind string, subject JobSubject) string {
	return kind + " " + subject.Type + " " + subject.ID
}

// JobBriefOf is the brief claimJob answered for the job of kind on
// subject, while this client holds it (kept at the claim, let go at the
// finish); ok is false for a job this client did not claim, or one
// claimed on a platform before the brief — the node then runs the
// kind's built-in prompt (ADR-041).
func (c *Client) JobBriefOf(kind string, subject JobSubject) (*JobBrief, bool) {
	c.briefsMu.Lock()
	defer c.briefsMu.Unlock()
	b, ok := c.briefs[briefKey(kind, subject)]
	return b, ok
}

// forgetBrief lets a finished job's brief go.
func (c *Client) forgetBrief(kind string, subject JobSubject) {
	c.briefsMu.Lock()
	defer c.briefsMu.Unlock()
	delete(c.briefs, briefKey(kind, subject))
}

// JobNotServed is an access entry the platform has no token for yet,
// and why.
type JobNotServed struct {
	Access string `json:"access"`
	Reason string `json:"reason"`
}

// JobAccess is jobAccess's answer: each token the job's access names, in
// the shape of the read it replaces — Repo as issueItemRepoToken (an
// object) or planGitHubToken (a list) answers it, Model as
// itemCredentials, each MCP audience as itemTools, Cloud as
// deployCredential — left raw for the node to read as it reads those.
type JobAccess struct {
	Kind      string                     `json:"kind"`
	Subject   JobSubject                 `json:"subject"`
	Harness   string                     `json:"harness"`
	Access    []string                   `json:"access"`
	Repo      json.RawMessage            `json:"repo,omitempty"`
	Model     json.RawMessage            `json:"model,omitempty"`
	Cloud     json.RawMessage            `json:"cloud,omitempty"`
	MCP       map[string]json.RawMessage `json:"mcp,omitempty"`
	NotServed []JobNotServed             `json:"notServed,omitempty"`
}

// Missing is why entry was not served, "" when it was or was not asked.
func (a *JobAccess) Missing(entry string) string {
	for _, n := range a.NotServed {
		if n.Access == entry {
			return n.Reason
		}
	}
	return ""
}

// jobRepoToken is one repository token as jobAccess answers it: the
// shape issueItemRepoToken and planGitHubToken answer, repo or
// repository naming the repository.
type jobRepoToken struct {
	Token       string         `json:"token"`
	ExpiresAt   string         `json:"expiresAt"`
	Repo        string         `json:"repo"`
	Repository  string         `json:"repository"`
	Access      string         `json:"access"`
	Permissions map[string]any `json:"permissions"`
	// MergeBatchID, on a candidate's token, is the batch it was minted for
	MergeBatchID string `json:"mergeBatchId"`
}

func (t jobRepoToken) token() (GitHubToken, error) {
	if t.Token == "" {
		return GitHubToken{}, errors.New("jobAccess repo: no token")
	}
	exp, err := time.Parse(time.RFC3339Nano, t.ExpiresAt)
	if err != nil {
		return GitHubToken{}, fmt.Errorf("jobAccess repo: expiresAt %q: %w", t.ExpiresAt, err)
	}
	access := t.Access
	if access == "" {
		access, _ = t.Permissions["contents"].(string)
	}
	repo := t.Repo
	if repo == "" {
		repo = t.Repository
	}
	return GitHubToken{Token: t.Token, ExpiresAt: exp, Repository: repo, Access: access, IssuedAt: time.Now()}, nil
}

// RepoTokens are the job's repository tokens: one for an item, a
// candidate or a main validation, one per repository for a goal or a
// direction; none when the job's access names no repo or it was not
// served (Missing says why).
func (a *JobAccess) RepoTokens() ([]GitHubToken, error) {
	raw := bytes.TrimSpace(a.Repo)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var rows []jobRepoToken
	if raw[0] == '{' {
		var one jobRepoToken
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, fmt.Errorf("jobAccess repo: %w", err)
		}
		rows = []jobRepoToken{one}
	} else if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("jobAccess repo: %w", err)
	}
	out := make([]GitHubToken, 0, len(rows))
	for _, r := range rows {
		t, err := r.token()
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// RepoToken is the job's one repository token, nil when it has none;
// more than one is an error (a goal's, read RepoTokens).
func (a *JobAccess) RepoToken() (*GitHubToken, error) {
	ts, err := a.RepoTokens()
	if err != nil || len(ts) == 0 {
		return nil, err
	}
	if len(ts) > 1 {
		return nil, fmt.Errorf("jobAccess repo: %d tokens, want one", len(ts))
	}
	return &ts[0], nil
}

// jobAccessReuse is how long a session start's exchange answers its
// other claim-time reads: never past a repository token's last five
// minutes.
const jobAccessReuse = 30 * time.Second

type heldAccess struct {
	answer *JobAccess
	until  time.Time
}

// exchanges holds one session start's whole exchanges (WithJobKind), so
// its claim-time reads — its repository token, its accounts, its tools
// — are one jobAccess. The next start, its own context, asks again.
type exchanges struct {
	mu   sync.Mutex
	held map[string]heldAccess
}

type exchangesKey struct{}

// JobAccess is the one exchange for the job of kind on subject, the
// harness it runs with: what the job's access names, to the node
// holding its claim. Within one session start (a context WithJobKind
// marked) a whole answer, every entry served, is reused.
func (c *Client) JobAccess(ctx context.Context, kind string, subject JobSubject, harness string) (*JobAccess, error) {
	box, _ := ctx.Value(exchangesKey{}).(*exchanges)
	if box == nil {
		return c.jobAccess(ctx, kind, subject, harness)
	}
	key := nodeIDFrom(ctx) + " " + kind + " " + subject.Type + " " + subject.ID + " " + harness
	now := time.Now()
	box.mu.Lock()
	held, ok := box.held[key]
	box.mu.Unlock()
	if ok && now.Before(held.until) {
		return held.answer, nil
	}
	acc, err := c.jobAccess(ctx, kind, subject, harness)
	if err != nil || len(acc.NotServed) > 0 {
		return acc, err
	}
	until := now.Add(jobAccessReuse)
	if ts, terr := acc.RepoTokens(); terr == nil {
		for _, t := range ts {
			if last := t.ExpiresAt.Add(-5 * time.Minute); last.Before(until) {
				until = last
			}
		}
	}
	box.mu.Lock()
	if box.held == nil {
		box.held = map[string]heldAccess{}
	}
	box.held[key] = heldAccess{answer: acc, until: until}
	box.mu.Unlock()
	return acc, nil
}

func (c *Client) jobAccess(ctx context.Context, kind string, subject JobSubject, harness string) (*JobAccess, error) {
	in := map[string]any{"subject": map[string]any{"type": subject.Type, "id": subject.ID}}
	if kind != "" {
		in["kind"] = kind
	}
	if harness != "" {
		in["harness"] = harness
	}
	var out struct {
		Access *JobAccess `json:"jobAccess"`
	}
	if err := c.Query(ctx, `query($in: Map!) { jobAccess(input: $in) }`, map[string]any{"in": in}, &out); err != nil {
		return nil, err
	}
	if out.Access == nil {
		return nil, fmt.Errorf("jobAccess %s %s: no answer", subject.Type, subject.ID)
	}
	return out.Access, nil
}

// FinishJob hands back the job's finish, its keys the kind's finish
// shape (changeset, verdict, plan, research, rebasedHead, attestation,
// deployRecord, answer or proposal), each value the input the older
// finish takes less the subject and the node. It answers the platform's
// receipt.
func (c *Client) FinishJob(ctx context.Context, nodeID, kind string, subject JobSubject, finish map[string]any) (map[string]any, error) {
	in := map[string]any{"nodeId": nodeID, "subject": map[string]any{"type": subject.Type, "id": subject.ID}, "finish": finish}
	if kind != "" {
		in["kind"] = kind
	}
	var out struct {
		Receipt map[string]any `json:"finishJob"`
	}
	if err := c.Query(ctx, `mutation($in: Map!) { finishJob(input: $in) }`, map[string]any{"in": in}, &out); err != nil {
		return nil, err
	}
	c.forgetBrief(kind, subject)
	return out.Receipt, nil
}

// JobKindsPath is the job-kind catalogue (runsheet ADR 061 §2).
const JobKindsPath = "/mesh/v1/jobs/kinds"

// JobKind is one kind of the catalogue.
type JobKind struct {
	Slug     string   `json:"slug"`
	Product  string   `json:"product"`
	Kind     string   `json:"kind"`
	Harness  []string `json:"harness"`
	Finish   string   `json:"finish"`
	Access   []string `json:"access"`
	Needs    string   `json:"needs"`
	Pods     string   `json:"pods"`
	Guidance string   `json:"guidance"`
	// FinishSchema is the product's schema for its part of the result
	// (ADR 067 decision 10); "" when the kind has none.
	FinishSchema string `json:"finishSchema"`
	// FinishTool and BriefTool are the kind's tools on its product's MCP
	// server, as JobBrief carries them.
	FinishTool string `json:"finishTool,omitempty"`
	BriefTool  string `json:"briefTool,omitempty"`
}

// jobKindsEvery is how long a client's read of the catalogue answers
// JobKindOf before it reads again.
const jobKindsEvery = 10 * time.Minute

// JobKindOf is kind's row of the catalogue — its guidance, finish and
// finishSchema for a job this client did not claim through claimJob (a
// job the loop claimed on another path, an adopted session), read once
// per jobKindsEvery and kept; ok is false for a kind the catalogue has
// not or a read that failed (the node then runs its built-in prompt,
// ADR-041).
func (c *Client) JobKindOf(ctx context.Context, kind string) (JobKind, bool) {
	c.kindsMu.Lock()
	kinds, at := c.kinds, c.kindsAt
	c.kindsMu.Unlock()
	if kinds == nil || time.Since(at) >= jobKindsEvery {
		got, err := c.JobKinds(ctx)
		if err != nil {
			return JobKind{}, false
		}
		c.kindsMu.Lock()
		c.kinds, c.kindsAt = got, time.Now()
		c.kindsMu.Unlock()
		kinds = got
	}
	k, ok := kinds[kind]
	return k, ok
}

// JobKinds reads the catalogue, by slug.
func (c *Client) JobKinds(ctx context.Context) (map[string]JobKind, error) {
	hr, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+JobKindsPath, nil)
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Accept", "application/json")
	tok, err := c.Bearer(ctx)
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Authorization", "Bearer "+tok)
	raw, status, err := c.doREST(hr)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, &httpError{Status: status, Text: http.StatusText(status), Body: restMessage(raw, status)}
	}
	var out struct {
		Kinds []JobKind `json:"kinds"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("job kinds: %w", err)
	}
	kinds := make(map[string]JobKind, len(out.Kinds))
	for _, k := range out.Kinds {
		kinds[k.Slug] = k
	}
	return kinds, nil
}
