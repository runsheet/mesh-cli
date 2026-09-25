// Package gateway is the node's transport: the runsheet platform's GraphQL
// gateway, spoken as the principal the node's credential names — its
// Latchkey machine, acting for the operator, or the person whose console
// token it holds (mesh-specs adr-002). Every write is one aggregate command
// (`mutation($in: XInput!) { x(input: $in) { status } }`); every read is
// an index list scoped by an EQ `where` the person's policy accepts.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// DefaultURL is the platform gateway.
const DefaultURL = "https://api.runsheet.dev"

// NS is the loom namespace every platform aggregate lives in.
const NS = "runsheet"

// TokenSource yields the bearer for the next request: an rs_* console
// token as it stands, or a Latchkey machine credential's access token,
// refreshed before it expires (latchkey adr-machine-identity).
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// StaticToken is a TokenSource for one fixed bearer.
type StaticToken string

// Token returns the bearer.
func (t StaticToken) Token(context.Context) (string, error) { return string(t), nil }

// Client speaks GraphQL (and Prologue's JSON) to one gateway as one
// principal.
type Client struct {
	BaseURL string
	Source  TokenSource
	HTTP    *http.Client

	// itemsWithoutModel is set once the gateway has refused the
	// WorkItemIndex fields items-carry-a-model added (an older
	// platform): every later item read asks for workItemFields alone.
	itemsWithoutModel atomic.Bool

	// heartbeatBasicUntil (unix nanoseconds) is when the heartbeat next
	// offers the fields the gateway refused (see HeartbeatNode).
	heartbeatBasicUntil atomic.Int64
	// nodesWithoutDesired is set once the gateway has refused
	// NodeIndex.desiredVersion (a platform older than
	// update-nodes-from-console): every later node read leaves it out.
	nodesWithoutDesired atomic.Bool
}

// New returns a client for baseURL (scheme+host, no trailing slash) with
// a fixed bearer.
func New(baseURL, token string) *Client {
	return NewWithSource(baseURL, StaticToken(strings.TrimSpace(token)))
}

// NewWithSource returns a client that asks src for its bearer on every
// request.
func NewWithSource(baseURL string, src TokenSource) *Client {
	return &Client{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		Source:  src,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

// Bearer is the token the next request would carry — for the Prologue
// MCP server a session is started with, which takes a header, not a
// source.
func (c *Client) Bearer(ctx context.Context) (string, error) {
	if c.Source == nil {
		return "", errors.New("gateway: no token source")
	}
	return c.Source.Token(ctx)
}

// Error is a GraphQL error the gateway returned — an aggregate's refusal
// ("item is claimed, not ready"), the policy's denial, or a bad query.
type Error struct {
	Message string
	// Unsupported is set when the gateway does not know the operation at
	// all — a platform older than the node (see IsUnsupported).
	Unsupported bool
}

func (e *Error) Error() string { return e.Message }

// IsRefusal reports whether err is the gateway saying no (as opposed to a
// transport failure): callers treat a refused claim as "someone else has it".
func IsRefusal(err error) bool {
	var ge *Error
	return errors.As(err, &ge)
}

// IsUnsupported reports whether err is the gateway not knowing the
// operation it was asked for — an older platform, without the mutation
// yet — as opposed to refusing it or failing: callers fall back to what
// they did before the operation existed. Such an error is also a
// refusal, so check this first.
func IsUnsupported(err error) bool {
	var ge *Error
	return errors.As(err, &ge) && ge.Unsupported
}

// unknownOperation reports whether a GraphQL error message is the
// schema's "no such field / no such type" for field or its input type,
// the way a gateway without that operation answers.
func unknownOperation(msg, field string) bool {
	typ := strings.ToUpper(field[:1]) + field[1:] + "Input"
	if !strings.Contains(msg, field) && !strings.Contains(msg, typ) {
		return false
	}
	for _, s := range []string{"Cannot query field", "Unknown type", "unknown field", "unknown mutation", "Unknown argument"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// Query posts one GraphQL document and decodes `data` into out.
func (c *Client) Query(ctx context.Context, query string, vars map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/graphql", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	tok, err := c.Bearer(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if jsonErr := json.Unmarshal(raw, &env); jsonErr != nil {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("gateway: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
		}
		return fmt.Errorf("gateway: decode response: %w", jsonErr)
	}
	if len(env.Errors) > 0 {
		return &Error{Message: env.Errors[0].Message}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("gateway: %s", resp.Status)
	}
	if out != nil && len(env.Data) > 0 {
		return json.Unmarshal(env.Data, out)
	}
	return nil
}

// Mutate sends one aggregate command: field is the mutation name
// ("claimWorkItem"), the input type is its capitalised form + "Input".
// aggregateId and namespace are set from id; the rest is input.
func (c *Client) Mutate(ctx context.Context, field, aggregateID string, input map[string]any) error {
	in := map[string]any{"aggregateId": aggregateID, "namespace": NS}
	for k, v := range input {
		in[k] = v
	}
	typ := strings.ToUpper(field[:1]) + field[1:] + "Input"
	q := fmt.Sprintf("mutation($in: %s!) { %s(input: $in) { status } }", typ, field)
	return c.Query(ctx, q, map[string]any{"in": in}, nil)
}

// ---- identity ---------------------------------------------------------------

// SignIn returns the person's user id — the node's ownerId.
func (c *Client) SignIn(ctx context.Context) (string, error) {
	var out struct {
		SignIn struct {
			ID string `json:"id"`
		} `json:"signIn"`
	}
	if err := c.Query(ctx, `mutation { signIn { id } }`, nil, &out); err != nil {
		return "", err
	}
	if out.SignIn.ID == "" {
		return "", errors.New("gateway: signIn returned no id")
	}
	return out.SignIn.ID, nil
}

// Org is one row of myOrgs.
type Org struct {
	ID        string `json:"id"`
	Login     string `json:"login"`
	AccountID string `json:"accountId"`
	Role      string `json:"role"`
}

// MyOrgs lists the orgs the person reaches.
func (c *Client) MyOrgs(ctx context.Context) ([]Org, error) {
	var out struct {
		MyOrgs json.RawMessage `json:"myOrgs"`
	}
	if err := c.Query(ctx, `{ myOrgs }`, nil, &out); err != nil {
		return nil, err
	}
	var orgs []Org
	if len(out.MyOrgs) == 0 || string(out.MyOrgs) == "null" {
		return nil, nil
	}
	// The Map scalar arrives as a JSON array, or as a string holding one.
	if err := json.Unmarshal(out.MyOrgs, &orgs); err != nil {
		var s string
		if err2 := json.Unmarshal(out.MyOrgs, &s); err2 != nil {
			return nil, fmt.Errorf("gateway: decode myOrgs: %w", err)
		}
		if err := json.Unmarshal([]byte(s), &orgs); err != nil {
			return nil, fmt.Errorf("gateway: decode myOrgs: %w", err)
		}
	}
	return orgs, nil
}

// OrgByLogin finds one of the person's orgs by its GitHub login.
func (c *Client) OrgByLogin(ctx context.Context, login string) (*Org, error) {
	orgs, err := c.MyOrgs(ctx)
	if err != nil {
		return nil, err
	}
	for i := range orgs {
		if strings.EqualFold(orgs[i].Login, login) {
			return &orgs[i], nil
		}
	}
	logins := make([]string, 0, len(orgs))
	for _, o := range orgs {
		logins = append(logins, o.Login)
	}
	return nil, fmt.Errorf("no org %q among yours (%s)", login, strings.Join(logins, ", "))
}

// ---- read models ------------------------------------------------------------

// WorkItem is a WorkItemIndex row.
type WorkItem struct {
	ID          string   `json:"id"`
	GoalID      string   `json:"goalId"`
	OrgID       string   `json:"orgId"`
	Seq         int      `json:"seq"`
	Title       string   `json:"title"`
	Brief       string   `json:"brief"`
	Acceptance  string   `json:"acceptance"`
	RepoID      string   `json:"repoId"`
	BaseRef     string   `json:"baseRef"`
	DependsOn   []string `json:"dependsOn"`
	Status      string   `json:"status"`
	NodeID      string   `json:"nodeId"`
	Branch      string   `json:"branch"`
	CostCents   int      `json:"costCents"`
	Attempts    int      `json:"attempts"`
	LastError   string   `json:"lastError"`
	LastNote    string   `json:"lastNote"`
	PRURL       string   `json:"prUrl"`
	HeartbeatAt string   `json:"heartbeatAt"`
	// Evaluation is the evaluator's notes on the last finished branch;
	// EvaluationPassed its verdict. A ready item with attempts > 0 and an
	// evaluation is a retry: the builder reads why the last one failed.
	Evaluation       string `json:"evaluation"`
	EvaluationPassed bool   `json:"evaluationPassed"`
	// RebaseState is where a conflict with the base stands (runsheet ADR
	// 008, trd §6a): none, requested (a node session of kind rebase is
	// wanted), claimed (RebaseNodeID's session has it), rebased,
	// unresolved. RebaseConflict is the conflicting regions in git's
	// markers, RebaseFiles the files; RebasedHeadSHA the head a rebase
	// pushed.
	RebaseState    string   `json:"rebaseState"`
	RebaseConflict string   `json:"rebaseConflict"`
	RebaseFiles    []string `json:"rebaseFiles"`
	RebaseNodeID   string   `json:"rebaseNodeId"`
	RebasedHeadSHA string   `json:"rebasedHeadSha"`
	// ProjectID is the Prologue project the item's goal is in: the scope
	// its agent resolves in (AgentIDs).
	ProjectID string `json:"projectId"`
	// AgentSlug names the agent that builds the item (runsheet trd §6c);
	// empty is the role's resolved definition.
	AgentSlug string `json:"agentSlug"`
	// Model is the tier the item is built on (runsheet trd §6a,
	// items-carry-a-model): the plan's, else its named agent's; empty is
	// the node's own. EvaluateModel is the tier the platform chose to
	// evaluate it on. Both are empty from a platform older than the goal.
	Model         string `json:"model"`
	EvaluateModel string `json:"evaluateModel"`
}

// AgentIDs are the Agent aggregate ids the item's AgentSlug may resolve
// to, most specific first — the project's definition, the org's, the
// platform's default (runsheet trd §6e, ids.AgentID) — or none when the
// item names no agent. The first one AgentModel finds a model for wins.
func (w WorkItem) AgentIDs() []string {
	if w.AgentSlug == "" {
		return nil
	}
	org, _ := uuid.Parse(w.OrgID)
	project, _ := uuid.Parse(w.ProjectID)
	var ids []string
	if org != uuid.Nil && project != uuid.Nil {
		ids = append(ids, AgentID(org, project, w.AgentSlug).String())
	}
	if org != uuid.Nil {
		ids = append(ids, AgentID(org, uuid.Nil, w.AgentSlug).String())
	}
	return append(ids, AgentID(uuid.Nil, uuid.Nil, w.AgentSlug).String())
}

const workItemFields = `id goalId orgId seq title brief acceptance repoId baseRef dependsOn status nodeId branch costCents attempts lastError lastNote prUrl heartbeatAt evaluation evaluationPassed rebaseState rebaseConflict rebaseFiles rebaseNodeId rebasedHeadSha`

// workItemModelExtra is what items-carry-a-model put on WorkItemIndex
// (runsheet trd §6a) and the node reads when the platform has it.
const workItemModelExtra = `projectId agentSlug model evaluateModel`

// readItems runs one WorkItemIndex read, query being the document with
// its selection left to fill in. The first read asks for the model
// fields too; a gateway that does not know one of them is an older
// platform, and from then on the client asks for the base fields alone.
func (c *Client) readItems(ctx context.Context, query func(fields string) string, out any) error {
	if !c.itemsWithoutModel.Load() {
		err := c.Query(ctx, query(workItemFields+" "+workItemModelExtra), nil, out)
		var ge *Error
		if err == nil || !errors.As(err, &ge) || !unknownField(ge.Message, strings.Fields(workItemModelExtra)...) {
			return err
		}
		c.itemsWithoutModel.Store(true)
	}
	return c.Query(ctx, query(workItemFields), nil, out)
}

// unknownField reports whether a GraphQL error message is the schema
// not knowing one of fields — a selected one ("Cannot query field") or
// one of an input ("Unknown field", "is not defined by type").
func unknownField(msg string, fields ...string) bool {
	known := false
	for _, s := range []string{"Cannot query field", "Unknown field", "unknown field", "is not defined by type"} {
		known = known || strings.Contains(msg, s)
	}
	if !known {
		return false
	}
	for _, f := range fields {
		if strings.Contains(msg, `"`+f+`"`) {
			return true
		}
	}
	return false
}

func where(field, value string) string {
	return fmt.Sprintf(`{field: %q, op: EQ, value: %q}`, field, value)
}

// ReadyWorkItems lists the org's items in `ready`, oldest seq first.
func (c *Client) ReadyWorkItems(ctx context.Context, orgID string) ([]WorkItem, error) {
	var out struct {
		Items []WorkItem `json:"workItemIndexs"`
	}
	q := func(fields string) string {
		return fmt.Sprintf(`{ workItemIndexs(namespace: %q, where: [%s, %s]) { %s } }`, NS, where("org_id", orgID), where("status", "ready"), fields)
	}
	if err := c.readItems(ctx, q, &out); err != nil {
		return nil, err
	}
	items := out.Items
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].Seq < items[j-1].Seq; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
	return items, nil
}

// WorkItem reads one item by id.
func (c *Client) WorkItem(ctx context.Context, id string) (*WorkItem, error) {
	var out struct {
		Item *WorkItem `json:"workItemIndex"`
	}
	q := func(fields string) string {
		return fmt.Sprintf(`{ workItemIndex(namespace: %q, id: %q) { %s } }`, NS, id, fields)
	}
	if err := c.readItems(ctx, q, &out); err != nil {
		return nil, err
	}
	if out.Item == nil {
		return nil, fmt.Errorf("work item %s not found", id)
	}
	return out.Item, nil
}

// OrgWorkItems lists an org's items, optionally in one status, by seq.
func (c *Client) OrgWorkItems(ctx context.Context, orgID, status string) ([]WorkItem, error) {
	var out struct {
		Items []WorkItem `json:"workItemIndexs"`
	}
	wh := where("org_id", orgID)
	if status != "" {
		wh += ", " + where("status", status)
	}
	q := func(fields string) string {
		return fmt.Sprintf(`{ workItemIndexs(namespace: %q, where: [%s]) { %s } }`, NS, wh, fields)
	}
	if err := c.readItems(ctx, q, &out); err != nil {
		return nil, err
	}
	sortBySeq(out.Items)
	return out.Items, nil
}

// RequestedRebases lists an org's items whose rebase_state is
// `requested` — a conflict the integrator could not resolve itself,
// waiting for a node's session of kind rebase — by seq.
func (c *Client) RequestedRebases(ctx context.Context, orgID string) ([]WorkItem, error) {
	var out struct {
		Items []WorkItem `json:"workItemIndexs"`
	}
	q := func(fields string) string {
		return fmt.Sprintf(`{ workItemIndexs(namespace: %q, where: [%s, %s]) { %s } }`, NS, where("org_id", orgID), where("rebase_state", "requested"), fields)
	}
	if err := c.readItems(ctx, q, &out); err != nil {
		return nil, err
	}
	sortBySeq(out.Items)
	return out.Items, nil
}

func sortBySeq(items []WorkItem) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].Seq < items[j-1].Seq; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

// GoalWorkItems lists a goal's items, by seq.
func (c *Client) GoalWorkItems(ctx context.Context, goalID string) ([]WorkItem, error) {
	var out struct {
		Items []WorkItem `json:"workItemIndexs"`
	}
	q := func(fields string) string {
		return fmt.Sprintf(`{ workItemIndexs(namespace: %q, where: [%s]) { %s } }`, NS, where("goal_id", goalID), fields)
	}
	if err := c.readItems(ctx, q, &out); err != nil {
		return nil, err
	}
	items := out.Items
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].Seq < items[j-1].Seq; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
	return items, nil
}

// AgentModel is the model an Agent definition names (runsheet trd §6e),
// by its aggregate id (AgentID, WorkItem.AgentIDs). A definition that
// does not exist, is retired or names no model is "", and so is a
// platform without agents or without their model: none of those is an
// error — the caller falls back to the node's own.
func (c *Client) AgentModel(ctx context.Context, agentID string) (string, error) {
	var out struct {
		Agent *struct {
			Model  string `json:"model"`
			Status string `json:"status"`
		} `json:"agentIndex"`
	}
	q := fmt.Sprintf(`{ agentIndex(namespace: %q, id: %q) { model status } }`, NS, agentID)
	if err := c.Query(ctx, q, nil, &out); err != nil {
		var ge *Error
		if errors.As(err, &ge) && unknownField(ge.Message, "agentIndex", "model", "status") {
			return "", nil
		}
		return "", err
	}
	if out.Agent == nil || out.Agent.Status == "retired" {
		return "", nil
	}
	return out.Agent.Model, nil
}

// Goal is a GoalIndex row.
type Goal struct {
	ID         string `json:"id"`
	Slug       string `json:"slug"`
	Title      string `json:"title"`
	Statement  string `json:"statement"`
	Acceptance string `json:"acceptance"`
	ProjectID  string `json:"projectId"`
	// DirectionID is the direction that set this goal, empty when a
	// person did — the room a direction has left is counted on it.
	DirectionID  string   `json:"directionId"`
	BudgetCents  int      `json:"budgetCents"`
	SpentCents   int      `json:"spentCents"`
	Status       string   `json:"status"`
	PausedReason string   `json:"pausedReason"`
	Checkpoints  []string `json:"checkpoints"`
	// Priority is urgent, high, normal or low; empty reads as normal. The
	// node claims ready work in this order across every org it serves.
	Priority string `json:"priority"`
}

const goalFields = `id slug title statement acceptance projectId directionId budgetCents spentCents status pausedReason checkpoints priority`

// PriorityRank orders goal priorities, most pressing first: urgent 0,
// high 1, normal 2, low 3. Empty or unknown reads as normal.
func PriorityRank(p string) int {
	switch p {
	case "urgent":
		return 0
	case "high":
		return 1
	case "low":
		return 3
	}
	return 2
}

// Goal reads one goal by id.
func (c *Client) Goal(ctx context.Context, id string) (*Goal, error) {
	var out struct {
		Goal *Goal `json:"goalIndex"`
	}
	q := fmt.Sprintf(`{ goalIndex(namespace: %q, id: %q) { %s } }`, NS, id, goalFields)
	if err := c.Query(ctx, q, nil, &out); err != nil {
		return nil, err
	}
	if out.Goal == nil {
		return nil, fmt.Errorf("goal %s not found", id)
	}
	return out.Goal, nil
}

// Goals lists an org's goals.
func (c *Client) Goals(ctx context.Context, orgID string) ([]Goal, error) {
	var out struct {
		Goals []Goal `json:"goalIndexs"`
	}
	q := fmt.Sprintf(`{ goalIndexs(namespace: %q, where: [%s]) { %s } }`, NS, where("org_id", orgID), goalFields)
	if err := c.Query(ctx, q, nil, &out); err != nil {
		return nil, err
	}
	return out.Goals, nil
}

// Project is a ProjectIndex row.
type Project struct {
	ID      string   `json:"id"`
	Slug    string   `json:"slug"`
	Name    string   `json:"name"`
	RepoIDs []string `json:"repoIds"`
	OrgID   string   `json:"orgId"`
	// MaxLiveGoals is the project's *total* ceiling on live goals over
	// every direction on it — it rides on the autonomy policy beside
	// replanCap (runsheet trd §6c), and caps every direction together
	// where a direction's own ceiling caps its share. 0 is the
	// platform's default.
	MaxLiveGoals int `json:"maxLiveGoals"`
}

const projectFields = `id slug name repoIds orgId maxLiveGoals`

// Project reads one project by id.
func (c *Client) Project(ctx context.Context, id string) (*Project, error) {
	var out struct {
		Project *Project `json:"projectIndex"`
	}
	q := fmt.Sprintf(`{ projectIndex(namespace: %q, id: %q) { %s } }`, NS, id, projectFields)
	if err := c.Query(ctx, q, nil, &out); err != nil {
		return nil, err
	}
	if out.Project == nil {
		return nil, fmt.Errorf("project %s not found", id)
	}
	return out.Project, nil
}

// Projects lists an org's projects.
func (c *Client) Projects(ctx context.Context, orgID string) ([]Project, error) {
	var out struct {
		Projects []Project `json:"projectIndexs"`
	}
	q := fmt.Sprintf(`{ projectIndexs(namespace: %q, where: [%s]) { %s } }`, NS, where("org_id", orgID), projectFields)
	if err := c.Query(ctx, q, nil, &out); err != nil {
		return nil, err
	}
	return out.Projects, nil
}

// Direction is a DirectionIndex row: one direction of one project — the
// words a person gave it, the hours it may be acted on, the pace it
// keeps, its share of the project's room and its research claim
// (runsheet trd §6c). A project has several, each researching on its
// own hours, and the id is `direction:<project>:<slug>`.
type Direction struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	OrgID     string `json:"orgId"`
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	// Direction is the words themselves — what a research session reads.
	Direction  string `json:"direction"`
	Enabled    bool   `json:"enabled"`
	FromMinute int    `json:"fromMinute"`
	ToMinute   int    `json:"toMinute"`
	Timezone   string `json:"timezone"`
	GapMinutes int    `json:"gapMinutes"`
	// MaxLiveGoals is this direction's own share of the room; the
	// project's ceiling still caps every direction together.
	MaxLiveGoals int    `json:"maxLiveGoals"`
	Status       string `json:"status"`
	// The claim, as the platform last folded it: the node holding this
	// direction's session, when it took it, and when the last session
	// ended (the gap counts from there).
	ResearchNodeID    string `json:"researchNodeId"`
	ResearchClaimedAt string `json:"researchClaimedAt"`
	ResearchedAt      string `json:"researchedAt"`
}

const directionFields = `id projectId orgId slug name direction enabled fromMinute toMinute timezone gapMinutes maxLiveGoals status researchNodeId researchClaimedAt researchedAt`

// Active reports whether the direction is one to offer the platform at
// all: switched on and not archived. Whether it may run *now* — the
// window, the gap, the claim — is the platform's to judge.
func (d Direction) Active() bool {
	return d.Enabled && d.Status != "archived"
}

// Directions lists an org's directions, or one project's when projectID
// is given. The gateway scopes DirectionIndex on org_id, so the org is
// always part of the where and the project only narrows it — the way
// the console's listDirections(orgId, projectId) does.
func (c *Client) Directions(ctx context.Context, orgID, projectID string) ([]Direction, error) {
	var out struct {
		Directions []Direction `json:"directionIndexs"`
	}
	filter := where("org_id", orgID)
	if projectID != "" {
		filter += ", " + where("project_id", projectID)
	}
	q := fmt.Sprintf(`{ directionIndexs(namespace: %q, where: [%s]) { %s } }`, NS, filter, directionFields)
	if err := c.Query(ctx, q, nil, &out); err != nil {
		return nil, err
	}
	return out.Directions, nil
}

// Repo is a RepoIndex row, with the owner login resolved.
type Repo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	AccountID     string `json:"accountId"`
	DefaultBranch string `json:"defaultBranch"`
	Owner         string `json:"-"`
}

// FullName is owner/name.
func (r Repo) FullName() string { return r.Owner + "/" + r.Name }

// Repo reads one repo by id and resolves its owner's login.
func (c *Client) Repo(ctx context.Context, id string) (*Repo, error) {
	var out struct {
		Repo *Repo `json:"repoIndex"`
	}
	q := fmt.Sprintf(`{ repoIndex(namespace: %q, id: %q) { id name accountId defaultBranch } }`, NS, id)
	if err := c.Query(ctx, q, nil, &out); err != nil {
		return nil, err
	}
	if out.Repo == nil {
		return nil, fmt.Errorf("repo %s not found", id)
	}
	login, err := c.AccountLogin(ctx, out.Repo.AccountID)
	if err != nil {
		return nil, err
	}
	out.Repo.Owner = login
	return out.Repo, nil
}

// Repos lists the repos of one account (an org's accountId), owner set.
func (c *Client) Repos(ctx context.Context, accountID, ownerLogin string) ([]Repo, error) {
	var out struct {
		Repos []Repo `json:"repoIndexs"`
	}
	q := fmt.Sprintf(`{ repoIndexs(namespace: %q, where: [%s]) { id name accountId defaultBranch } }`, NS, where("account_id", accountID))
	if err := c.Query(ctx, q, nil, &out); err != nil {
		return nil, err
	}
	for i := range out.Repos {
		out.Repos[i].Owner = ownerLogin
	}
	return out.Repos, nil
}

// AccountLogin reads an account's GitHub login.
func (c *Client) AccountLogin(ctx context.Context, id string) (string, error) {
	var out struct {
		Account *struct {
			Login string `json:"login"`
		} `json:"accountIndex"`
	}
	q := fmt.Sprintf(`{ accountIndex(namespace: %q, id: %q) { login } }`, NS, id)
	if err := c.Query(ctx, q, nil, &out); err != nil {
		return "", err
	}
	if out.Account == nil {
		return "", fmt.Errorf("account %s not found", id)
	}
	return out.Account.Login, nil
}

// Node is a NodeIndex row.
type Node struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Slug       string `json:"slug"`
	Status     string `json:"status"`
	Running    int    `json:"running"`
	Capacity   int    `json:"capacity"`
	Version    string `json:"version"`
	LastSeenAt string `json:"lastSeenAt"`
	// DesiredVersion is the version the platform asks the node to run
	// (update-nodes-from-console); empty leaves it be, and so does a
	// platform older than the field.
	DesiredVersion string `json:"desiredVersion"`
}

const nodeFields = `id name slug status running capacity version lastSeenAt`

// readNodes runs one NodeIndex read, query being the document with its
// selection left to fill in. It asks for desiredVersion too; a gateway
// that does not know it is a platform older than the field, and from
// then on the client asks for nodeFields alone. It reports whether the
// field was read.
func (c *Client) readNodes(ctx context.Context, query func(fields string) string, out any) (bool, error) {
	if !c.nodesWithoutDesired.Load() {
		err := c.Query(ctx, query(nodeFields+" desiredVersion"), nil, out)
		var ge *Error
		if err == nil || !errors.As(err, &ge) || !unknownField(ge.Message, "desiredVersion") {
			return true, err
		}
		c.nodesWithoutDesired.Store(true)
	}
	return false, c.Query(ctx, query(nodeFields), nil, out)
}

// Nodes lists an org's nodes.
func (c *Client) Nodes(ctx context.Context, orgID string) ([]Node, error) {
	var out struct {
		Nodes []Node `json:"nodeIndexs"`
	}
	q := func(fields string) string {
		return fmt.Sprintf(`{ nodeIndexs(namespace: %q, where: [%s]) { %s } }`, NS, where("org_id", orgID), fields)
	}
	if _, err := c.readNodes(ctx, q, &out); err != nil {
		return nil, err
	}
	return out.Nodes, nil
}

// GetDesiredVersion reads the version the platform asks node nodeID to
// run; empty leaves it be. A platform that does not know the field
// answers "" with an error IsUnsupported says is one, every time, so the
// caller can note it once and carry on as if nothing were asked.
func (c *Client) GetDesiredVersion(ctx context.Context, nodeID string) (string, error) {
	var out struct {
		Node *Node `json:"nodeIndex"`
	}
	q := func(fields string) string {
		return fmt.Sprintf(`{ nodeIndex(namespace: %q, id: %q) { %s } }`, NS, nodeID, fields)
	}
	read, err := c.readNodes(ctx, q, &out)
	switch {
	case err != nil:
		return "", err
	case !read:
		return "", &Error{Message: "the platform has no NodeIndex.desiredVersion", Unsupported: true}
	case out.Node == nil:
		return "", fmt.Errorf("node %s not found", nodeID)
	}
	return out.Node.DesiredVersion, nil
}

// ---- commands ---------------------------------------------------------------

// RegisterNodeInput is RegisterNode's payload.
type RegisterNodeInput struct {
	NodeID    string
	OrgID     string
	OwnerID   string
	Name      string
	Slug      string
	Harnesses []string
	Capacity  int
	Version   string
}

// RegisterNode registers (converging) the node.
func (c *Client) RegisterNode(ctx context.Context, in RegisterNodeInput) error {
	harnesses := in.Harnesses
	if harnesses == nil {
		harnesses = []string{}
	}
	return c.Mutate(ctx, "registerNode", in.NodeID, map[string]any{
		"orgId": in.OrgID, "ownerId": in.OwnerID, "name": in.Name, "slug": in.Slug,
		"harnesses": harnesses, "capacity": in.Capacity, "version": in.Version,
	})
}

// HeartbeatExtras are what a heartbeat reports beyond liveness. Each is
// sent only when set: they are fields the platform half adds
// (update-nodes-from-console), and a platform without them must go on
// accepting the heartbeat of an ordinary node.
type HeartbeatExtras struct {
	// Draining says the node claims nothing new and is letting its
	// sessions finish.
	Draining bool
	// UpdateError says why the last self-update was refused or rolled back.
	UpdateError string
	// Limits are the latest Claude plan limits the node has read, one
	// per account (runsheet goal limits-on-the-console).
	Limits []Limits
	// Scaling is a ranged node's capacity in force, its range and why
	// (adr-004); a node with a fixed capacity sends none (runsheet goal
	// limits-on-the-console).
	Scaling *Scaling
}

// Scaling is what a node whose capacity is a range reports of it: the
// capacity in force, node.yaml's min and max, and the reason — `max`,
// `weekly pace`, `five-hour pace` or `machine load`.
type Scaling struct {
	Capacity int    `json:"capacity"`
	Min      int    `json:"min"`
	Max      int    `json:"max"`
	Reason   string `json:"reason"`
}

// Limits is one account's Claude plan limits as the latest session's
// rate_limit_event reported them (mesh-specs trd §1). Times encode as
// RFC 3339; a zero one is left out.
type Limits struct {
	Account        string       `json:"account"`
	Status         string       `json:"status"`
	RateLimitType  string       `json:"rateLimitType,omitempty"`
	ResetsAt       time.Time    `json:"resetsAt,omitzero"`
	OverageStatus  string       `json:"overageStatus,omitempty"`
	IsUsingOverage bool         `json:"isUsingOverage"`
	FiveHour       *LimitWindow `json:"fiveHour,omitempty"`
	SevenDay       *LimitWindow `json:"sevenDay,omitempty"`
	ObservedAt     time.Time    `json:"observedAt,omitzero"`
}

// LimitWindow is one of the plan's windows: how much of it is used, as
// the session reported it, and when it starts over.
type LimitWindow struct {
	Utilization float64   `json:"utilization"`
	ResetsAt    time.Time `json:"resetsAt,omitzero"`
}

// HeartbeatNode reports liveness and the running session count, and the
// extras that are set (at most one struct is read).
func (c *Client) HeartbeatNode(ctx context.Context, nodeID, version string, running int, extras ...HeartbeatExtras) error {
	in := map[string]any{"version": version, "running": running}
	if len(extras) > 0 {
		x := extras[0]
		if x.Draining {
			in["draining"] = true
		}
		if x.UpdateError != "" {
			in["updateError"] = x.UpdateError
		}
		if len(x.Limits) > 0 {
			in["limits"] = x.Limits
		}
		if x.Scaling != nil {
			in["scaling"] = x.Scaling
		}
	}
	// A node can be newer than the platform it reports to: a field the
	// gateway does not know yet makes GraphQL refuse the whole heartbeat,
	// and a node whose heartbeats never land is marked offline while it
	// works (2026-09-25: limits and scaling shipped before the platform
	// took them). So the extra fields are dropped for a while when they
	// are refused, and offered again every ten minutes.
	if len(in) > 2 && time.Now().UnixNano() < c.heartbeatBasicUntil.Load() {
		in = basicHeartbeat(in)
	}
	err := c.Mutate(ctx, "heartbeatNode", nodeID, in)
	if err != nil && len(in) > 2 && strings.Contains(err.Error(), "got invalid value") {
		c.heartbeatBasicUntil.Store(time.Now().Add(10 * time.Minute).UnixNano())
		return c.Mutate(ctx, "heartbeatNode", nodeID, basicHeartbeat(in))
	}
	return err
}

// basicHeartbeat is a heartbeat with only the fields every platform
// since node drains takes: version, running and draining.
func basicHeartbeat(in map[string]any) map[string]any {
	out := map[string]any{"version": in["version"], "running": in["running"]}
	if d, ok := in["draining"]; ok {
		out["draining"] = d
	}
	return out
}

// ClaimWorkItem takes the item's lock; a refusal is a gateway.Error.
func (c *Client) ClaimWorkItem(ctx context.Context, itemID, nodeID, harness string) error {
	return c.Mutate(ctx, "claimWorkItem", itemID, map[string]any{"nodeId": nodeID, "harness": harness})
}

// ClaimWorkItemRebase takes an item's rebase (runsheet ADR 008): the
// lock, as ClaimWorkItem's is the item's. The same node again converges;
// another node, or a rebase not requested, is refused with a
// gateway.Error.
func (c *Client) ClaimWorkItemRebase(ctx context.Context, itemID, nodeID string) error {
	return c.Mutate(ctx, "claimWorkItemRebase", itemID, map[string]any{"nodeId": nodeID})
}

// FinishWorkItemRebase records a rebase a node's session pushed: the new
// head and the files it resolved. The item goes back into its repo's
// merge queue, attempts untouched. The command carries no cost; the
// session's cents go on ReportWorkItemProgress, the rebase's heartbeat.
func (c *Client) FinishWorkItemRebase(ctx context.Context, itemID, headSha string, resolvedFiles []string) error {
	if resolvedFiles == nil {
		resolvedFiles = []string{}
	}
	return c.Mutate(ctx, "finishWorkItemRebase", itemID, map[string]any{"headSha": headSha, "method": "session", "files": resolvedFiles})
}

// FailWorkItemRebase gives the rebase up: the reason is the conflict as
// the session found it, which the builder is handed with the item,
// attempts unchanged.
func (c *Client) FailWorkItemRebase(ctx context.Context, itemID, reason string) error {
	return c.Mutate(ctx, "failWorkItemRebase", itemID, map[string]any{"reason": reason})
}

// ReportWorkItemProgress is the heartbeat: a note and the cents since last.
func (c *Client) ReportWorkItemProgress(ctx context.Context, itemID, note string, costCents int, sessionID string) error {
	return c.Mutate(ctx, "reportWorkItemProgress", itemID, map[string]any{"note": note, "costCents": costCents, "sessionId": sessionID})
}

// FinishWorkItem records the pushed branch.
func (c *Client) FinishWorkItem(ctx context.Context, itemID, branch, headSha string, costCents int) error {
	return c.Mutate(ctx, "finishWorkItem", itemID, map[string]any{"branch": branch, "headSha": headSha, "costCents": costCents})
}

// FailWorkItem records the failure.
func (c *Client) FailWorkItem(ctx context.Context, itemID, errMsg string) error {
	return c.Mutate(ctx, "failWorkItem", itemID, map[string]any{"error": errMsg})
}

// ReleaseWorkItem hands back an item this node holds without failing it:
// the node's condition, not the item's (mesh-specs trd §1, §3) — its
// harness could not run, or GitHub refused the item's token through every
// retry. The platform takes it only for a claimed or running item held by
// the calling node; the item is ready again with its node cleared,
// attempts unchanged, and the reason in last_note, not last_error.
func (c *Client) ReleaseWorkItem(ctx context.Context, itemID, reason string) error {
	return c.Mutate(ctx, "releaseWorkItem", itemID, map[string]any{"reason": reason})
}

// EvaluateWorkItem records the evaluator's verdict (phase 1's session; the
// platform readies or reviews the item) and that session's cost.
func (c *Client) EvaluateWorkItem(ctx context.Context, itemID string, passed bool, notes string, costCents int) error {
	return c.Mutate(ctx, "evaluateWorkItem", itemID, map[string]any{"passed": passed, "notes": notes, "costCents": costCents})
}

// GitHubToken is a claimed item's own GitHub credential: an installation
// token of the GitHub App, minted through the installation of the org
// that owns the item's repo, restricted to that one repository (contents
// write, pull requests read) and living about an hour.
type GitHubToken struct {
	Token     string
	ExpiresAt time.Time
	// Repository is the one repo the token reaches, as owner/name.
	Repository string
}

// IssueItemRepoToken asks the platform for the item's GitHub token; only
// the node holding the item's claim is given one (anything else is a
// refusal). A platform without the mutation answers IsUnsupported, and
// the caller falls back to the operator's own GitHub login.
func (c *Client) IssueItemRepoToken(ctx context.Context, itemID, nodeID string) (*GitHubToken, error) {
	const field = "issueItemRepoToken"
	// The platform serves issueItemRepoToken(workItemId: UUID!): Map —
	// {token, expiresAt, repo, access, permissions} — not a command with
	// an input object; the node's claim is what authorises it, so the
	// node id is not an argument (runsheet gateway/itemtoken.go).
	var out struct {
		IssueItemRepoToken *struct {
			Token     string `json:"token"`
			ExpiresAt string `json:"expiresAt"`
			Repo      string `json:"repo"`
		} `json:"issueItemRepoToken"`
	}
	q := `mutation($id: UUID!) { issueItemRepoToken(workItemId: $id) }`
	if err := c.Query(ctx, q, map[string]any{"id": itemID}, &out); err != nil {
		var ge *Error
		if errors.As(err, &ge) && (unknownOperation(ge.Message, field) || schemaMismatch(ge.Message, field)) {
			return nil, &Error{Message: ge.Message, Unsupported: true}
		}
		return nil, err
	}
	t := out.IssueItemRepoToken
	if t == nil || t.Token == "" {
		return nil, fmt.Errorf("gateway: %s: no token for item %s", field, itemID)
	}
	exp, err := time.Parse(time.RFC3339Nano, t.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("gateway: %s: expiresAt %q: %w", field, t.ExpiresAt, err)
	}
	return &GitHubToken{Token: t.Token, ExpiresAt: exp, Repository: t.Repo}, nil
}

// schemaMismatch is a GraphQL validation error about how the node called
// an operation — the shape of its arguments or variables — rather than
// an answer from it. On 2026-09-24 the node called issueItemRepoToken
// with an input object the platform never had, and every item it claimed
// failed on "Variable "$in" cannot be non-input type"; a shape the node
// and the platform disagree on is the platform not supporting the call
// the node makes, so the caller falls back rather than failing the work.
func schemaMismatch(msg, field string) bool {
	typ := strings.ToUpper(field[:1]) + field[1:] + "Input"
	if !strings.Contains(msg, field) && !strings.Contains(msg, typ) {
		return false
	}
	for _, s := range []string{"cannot be non-input type", "is required but not provided", "Unknown argument", "has invalid value", "of required type"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// SetGoalInput is SetGoal's payload.
type SetGoalInput struct {
	GoalID           string
	OrgID            string
	ProjectID        string
	Slug             string
	Title            string
	Statement        string
	Acceptance       string
	BudgetCents      int
	DailyBudgetCents int
	Checkpoints      []string
	By               string
}

// SetGoal creates or restates a goal.
func (c *Client) SetGoal(ctx context.Context, in SetGoalInput) error {
	cps := in.Checkpoints
	if cps == nil {
		cps = []string{}
	}
	m := map[string]any{
		"orgId": in.OrgID, "projectId": in.ProjectID, "slug": in.Slug, "title": in.Title,
		"statement": in.Statement, "acceptance": in.Acceptance, "budgetCents": in.BudgetCents,
		"dailyBudgetCents": in.DailyBudgetCents, "checkpoints": cps,
	}
	if in.By != "" {
		m["by"] = in.By
	}
	return c.Mutate(ctx, "setGoal", in.GoalID, m)
}

// PlannedItem is one item of PlanGoal's graph; DependsOn names seqs.
type PlannedItem struct {
	Seq        int    `json:"seq"`
	Title      string `json:"title"`
	Brief      string `json:"brief"`
	Acceptance string `json:"acceptance"`
	DependsOn  []int  `json:"dependsOn"`
	RepoID     string `json:"repoId"`
	BaseRef    string `json:"baseRef"`
	// Model is the tier the planner chose for the item (opus, sonnet,
	// haiku; runsheet trd §6a); empty leaves it to the node.
	Model string `json:"model,omitempty"`
	// DependsBecause is the planner's one line of what each kept
	// dependency uses, by seq (trd §1 WorkItem depends_on): the node
	// keeps it and PlanGoal does not send it to the platform.
	DependsBecause map[int]string `json:"-"`
}

// RequestPlan asks the planner for the goal's graph: the goal waits in
// planning until a node's planner session hands one in.
func (c *Client) RequestPlan(ctx context.Context, goalID, by string) error {
	m := map[string]any{}
	if by != "" {
		m["by"] = by
	}
	return c.Mutate(ctx, "requestPlan", goalID, m)
}

// ClaimPlan is the node's lock on a goal's planning; refused (IsRefusal)
// while another node holds it.
func (c *Client) ClaimPlan(ctx context.Context, goalID, nodeID string) error {
	return c.Mutate(ctx, "claimPlan", goalID, map[string]any{"nodeId": nodeID})
}

// GoalProposal is one goal a research session proposes (runsheet trd
// §6c). The platform decides which of them become goals.
type GoalProposal struct {
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Statement   string `json:"statement"`
	Acceptance  string `json:"acceptance"`
	Why         string `json:"why,omitempty"`
	BudgetCents int    `json:"budgetCents,omitempty"`
}

// ClaimDirectionResearch takes this direction's next research session
// for the node. Every "not now" is a refusal — the direction off or
// archived, its project paused, the hour outside the window in the
// direction's own zone, the gap since its last session, another node
// holding the claim — so a refusal is the answer to expect and not an
// error to shout about.
func (c *Client) ClaimDirectionResearch(ctx context.Context, directionID, nodeID string) error {
	return c.Mutate(ctx, "claimDirectionResearch", directionID, map[string]any{
		"nodeId": nodeID, "at": time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// RecordDirectionResearch hands in what the session came to, and the
// cost whether or not it proposed anything. It also lets the claim go,
// so an empty hand-in is how a node gives a direction back.
func (c *Client) RecordDirectionResearch(ctx context.Context, directionID, nodeID string, proposals []GoalProposal, costCents int, notes string) error {
	list := make([]map[string]any, 0, len(proposals))
	for _, p := range proposals {
		row := map[string]any{"slug": p.Slug, "title": p.Title, "statement": p.Statement, "acceptance": p.Acceptance}
		if p.Why != "" {
			row["why"] = p.Why
		}
		if p.BudgetCents > 0 {
			row["budgetCents"] = p.BudgetCents
		}
		list = append(list, row)
	}
	return c.Mutate(ctx, "recordDirectionResearch", directionID, map[string]any{
		"nodeId": nodeID, "at": time.Now().UTC().Format(time.RFC3339Nano),
		"proposals": list, "costCents": costCents, "notes": notes,
	})
}

// PlanGoal hands the goal its graph, with the planner session's cost
// when one drew it. An item's model is sent only when it has one; a
// platform older than items-carry-a-model refuses the field, and the
// graph is handed in again without it.
func (c *Client) PlanGoal(ctx context.Context, goalID, by string, items []PlannedItem, costCents int) error {
	send := func(withModel bool) error {
		list := make([]map[string]any, 0, len(items))
		for _, it := range items {
			deps := it.DependsOn
			if deps == nil {
				deps = []int{} // the input is non-null
			}
			row := map[string]any{
				"seq": it.Seq, "title": it.Title, "brief": it.Brief, "acceptance": it.Acceptance,
				"dependsOn": deps, "repoId": it.RepoID, "baseRef": it.BaseRef,
			}
			if withModel && it.Model != "" {
				row["model"] = it.Model
			}
			list = append(list, row)
		}
		m := map[string]any{"items": list, "costCents": costCents}
		if by != "" {
			m["by"] = by
		}
		return c.Mutate(ctx, "planGoal", goalID, m)
	}
	err := send(true)
	var ge *Error
	if err != nil && errors.As(err, &ge) && unknownField(ge.Message, "model") {
		return send(false)
	}
	return err
}

// ApproveGraph approves the pending graph checkpoint.
func (c *Client) ApproveGraph(ctx context.Context, goalID, by string) error {
	m := map[string]any{}
	if by != "" {
		m["by"] = by
	}
	return c.Mutate(ctx, "approveGraph", goalID, m)
}

// PauseGoal pauses the goal.
func (c *Client) PauseGoal(ctx context.Context, goalID, reason string) error {
	return c.Mutate(ctx, "pauseGoal", goalID, map[string]any{"reason": reason})
}

// ResumeGoal resumes the goal.
func (c *Client) ResumeGoal(ctx context.Context, goalID string) error {
	return c.Mutate(ctx, "resumeGoal", goalID, map[string]any{})
}

// CancelGoal cancels the goal.
func (c *Client) CancelGoal(ctx context.Context, goalID, by string) error {
	m := map[string]any{}
	if by != "" {
		m["by"] = by
	}
	return c.Mutate(ctx, "cancelGoal", goalID, m)
}

// ---- Prologue ---------------------------------------------------------------

// Document is one row of Prologue's document list.
type Document struct {
	Slug    string `json:"slug"`
	Title   string `json:"title"`
	Summary string `json:"summary,omitempty"` // one line from the frontmatter: what the index row says
	Kind    string `json:"kind"`
	Status  string `json:"status"`
	Repo    string `json:"repo"`
	Path    string `json:"path"`
}

func (c *Client) prologueGet(ctx context.Context, path, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/prologue/v1"+path, nil)
	if err != nil {
		return nil, err
	}
	tok, err := c.Bearer(ctx)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", accept)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("prologue: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

// ProlougeDocuments lists a project's documents (org login + project slug).
func (c *Client) PrologueDocuments(ctx context.Context, org, project string) ([]Document, error) {
	raw, err := c.prologueGet(ctx, "/projects/"+url.PathEscape(org)+"/"+url.PathEscape(project)+"/documents", "application/json")
	if err != nil {
		return nil, err
	}
	var out struct {
		Documents []Document `json:"documents"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("prologue: decode documents: %w", err)
	}
	return out.Documents, nil
}

// PrologueDocument fetches one document's body as markdown.
func (c *Client) PrologueDocument(ctx context.Context, org, project, doc string) (string, error) {
	raw, err := c.prologueGet(ctx, "/projects/"+url.PathEscape(org)+"/"+url.PathEscape(project)+"/documents/"+url.PathEscape(doc), "text/markdown")
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// ---- credentials and usage (runsheet credentials-sealed-in-loom, usage-ledger) ----

// SessionRef names the session a credential read or a usage record is
// for: its kind (build, evaluate, plan, research, rebase) and what it
// works on — the item for a build, an evaluation or a rebase (and its
// goal), the goal for a plan, the direction for research.
type SessionRef struct {
	Kind        string
	ItemID      string
	GoalID      string
	DirectionID string
}

// Credential is one Claude account the platform hands the node that
// claimed a session (the claim-time read, adr-005): a subscription's
// `claude setup-token` token or an API key, decrypted for this node and
// this session only. Secret is never logged: String and GoString leave
// it out, and nothing the node reports carries it.
type Credential struct {
	ID    string
	Label string
	// Kind is "subscription" or "api".
	Kind   string
	Secret string
	// CapDailyUSD and CapMonthlyUSD are an api key's caps (0: the
	// platform's default, $50 and $1,000); SpentDailyUSD and
	// SpentMonthlyUSD what the ledger has counted against them so far.
	CapDailyUSD     float64
	CapMonthlyUSD   float64
	SpentDailyUSD   float64
	SpentMonthlyUSD float64
}

// String is the credential without its secret.
func (c Credential) String() string {
	return fmt.Sprintf("credential %s (%s, %s)", c.Label, c.Kind, c.ID)
}

// GoString is String, so %#v leaves the secret out too.
func (c Credential) GoString() string { return c.String() }

// SessionCredentials is the claim-time read: the credentials the session
// ref names may run on, their secrets decrypted for nodeID, which must
// hold the session's claim (anything else is a refusal). A platform
// without the mutation answers IsUnsupported, and the node falls back to
// node.yaml's accounts. No error, log line or report carries a secret.
func (c *Client) SessionCredentials(ctx context.Context, ref SessionRef, nodeID string) ([]Credential, error) {
	const field = "sessionCredentials"
	in := map[string]any{"namespace": NS, "kind": ref.Kind, "nodeId": nodeID}
	for k, v := range map[string]string{"workItemId": ref.ItemID, "goalId": ref.GoalID, "directionId": ref.DirectionID} {
		if v != "" {
			in[k] = v
		}
	}
	var out struct {
		SessionCredentials *struct {
			Credentials []struct {
				ID              string  `json:"id"`
				Label           string  `json:"label"`
				Kind            string  `json:"kind"`
				Secret          string  `json:"secret"`
				CapDailyUSD     float64 `json:"capDailyUsd"`
				CapMonthlyUSD   float64 `json:"capMonthlyUsd"`
				SpentDailyUSD   float64 `json:"spentDailyUsd"`
				SpentMonthlyUSD float64 `json:"spentMonthlyUsd"`
			} `json:"credentials"`
		} `json:"sessionCredentials"`
	}
	q := `mutation($in: SessionCredentialsInput!) { sessionCredentials(input: $in) { credentials { id label kind secret capDailyUsd capMonthlyUsd spentDailyUsd spentMonthlyUsd } } }`
	if err := c.Query(ctx, q, map[string]any{"in": in}, &out); err != nil {
		var ge *Error
		if errors.As(err, &ge) && (unknownOperation(ge.Message, field) || schemaMismatch(ge.Message, field) || unknownField(ge.Message, "credentials", "secret", "capDailyUsd", "capMonthlyUsd", "spentDailyUsd", "spentMonthlyUsd")) {
			return nil, &Error{Message: ge.Message, Unsupported: true}
		}
		return nil, err
	}
	if out.SessionCredentials == nil {
		return nil, nil
	}
	creds := make([]Credential, 0, len(out.SessionCredentials.Credentials))
	for _, cr := range out.SessionCredentials.Credentials {
		creds = append(creds, Credential{
			ID: cr.ID, Label: cr.Label, Kind: cr.Kind, Secret: cr.Secret,
			CapDailyUSD: cr.CapDailyUSD, CapMonthlyUSD: cr.CapMonthlyUSD,
			SpentDailyUSD: cr.SpentDailyUSD, SpentMonthlyUSD: cr.SpentMonthlyUSD,
		})
	}
	return creds, nil
}

// UsageWindows is an account's plan standing at one moment — the
// five-hour and seven-day windows' utilization (0–1) and resets — as
// the latest rate_limit_event read it; a window never read is nil.
type UsageWindows struct {
	FiveHour *LimitWindow `json:"fiveHour,omitempty"`
	SevenDay *LimitWindow `json:"sevenDay,omitempty"`
}

// UsageRecord is one session's line in the platform's usage ledger: what
// it was for, the credential it ran on (id when the platform issued it,
// label always), what it consumed and cost, and its account's windows
// before and after. InputTokens counts every input token, the cached
// ones (CacheReadTokens, CacheWriteTokens) included. It never carries
// a secret.
type UsageRecord struct {
	Kind             string        `json:"kind"`
	ItemID           string        `json:"workItemId,omitempty"`
	GoalID           string        `json:"goalId,omitempty"`
	DirectionID      string        `json:"directionId,omitempty"`
	NodeID           string        `json:"nodeId,omitempty"`
	SessionID        string        `json:"sessionId,omitempty"`
	CredentialID     string        `json:"credentialId,omitempty"`
	CredentialLabel  string        `json:"credentialLabel"`
	CredentialKind   string        `json:"credentialKind,omitempty"`
	Model            string        `json:"model,omitempty"`
	InputTokens      int64         `json:"inputTokens"`
	OutputTokens     int64         `json:"outputTokens"`
	CacheReadTokens  int64         `json:"cacheReadTokens"`
	CacheWriteTokens int64         `json:"cacheWriteTokens"`
	CostUSD          float64       `json:"costUsd"`
	Before           *UsageWindows `json:"before,omitempty"`
	After            *UsageWindows `json:"after,omitempty"`
	StartedAt        time.Time     `json:"startedAt"`
	EndedAt          time.Time     `json:"endedAt"`
}

// RecordSessionUsage hands one session's usage record to the ledger. A
// platform without the mutation answers IsUnsupported, and the node
// writes the record to its log instead.
func (c *Client) RecordSessionUsage(ctx context.Context, rec UsageRecord) error {
	const field = "recordSessionUsage"
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	in := map[string]any{}
	if err := json.Unmarshal(data, &in); err != nil {
		return err
	}
	in["namespace"] = NS
	q := `mutation($in: RecordSessionUsageInput!) { recordSessionUsage(input: $in) { status } }`
	if err := c.Query(ctx, q, map[string]any{"in": in}, nil); err != nil {
		var ge *Error
		if errors.As(err, &ge) && (unknownOperation(ge.Message, field) || schemaMismatch(ge.Message, field)) {
			return &Error{Message: ge.Message, Unsupported: true}
		}
		return err
	}
	return nil
}
