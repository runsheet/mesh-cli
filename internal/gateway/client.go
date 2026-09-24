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
	"time"
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
}

const workItemFields = `id goalId orgId seq title brief acceptance repoId baseRef dependsOn status nodeId branch costCents attempts lastError lastNote prUrl heartbeatAt evaluation evaluationPassed rebaseState rebaseConflict rebaseFiles rebaseNodeId rebasedHeadSha`

func where(field, value string) string {
	return fmt.Sprintf(`{field: %q, op: EQ, value: %q}`, field, value)
}

// ReadyWorkItems lists the org's items in `ready`, oldest seq first.
func (c *Client) ReadyWorkItems(ctx context.Context, orgID string) ([]WorkItem, error) {
	var out struct {
		Items []WorkItem `json:"workItemIndexs"`
	}
	q := fmt.Sprintf(`{ workItemIndexs(namespace: %q, where: [%s, %s]) { %s } }`, NS, where("org_id", orgID), where("status", "ready"), workItemFields)
	if err := c.Query(ctx, q, nil, &out); err != nil {
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
	q := fmt.Sprintf(`{ workItemIndex(namespace: %q, id: %q) { %s } }`, NS, id, workItemFields)
	if err := c.Query(ctx, q, nil, &out); err != nil {
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
	q := fmt.Sprintf(`{ workItemIndexs(namespace: %q, where: [%s]) { %s } }`, NS, wh, workItemFields)
	if err := c.Query(ctx, q, nil, &out); err != nil {
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
	q := fmt.Sprintf(`{ workItemIndexs(namespace: %q, where: [%s, %s]) { %s } }`, NS, where("org_id", orgID), where("rebase_state", "requested"), workItemFields)
	if err := c.Query(ctx, q, nil, &out); err != nil {
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
	q := fmt.Sprintf(`{ workItemIndexs(namespace: %q, where: [%s]) { %s } }`, NS, where("goal_id", goalID), workItemFields)
	if err := c.Query(ctx, q, nil, &out); err != nil {
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
}

// Nodes lists an org's nodes.
func (c *Client) Nodes(ctx context.Context, orgID string) ([]Node, error) {
	var out struct {
		Nodes []Node `json:"nodeIndexs"`
	}
	q := fmt.Sprintf(`{ nodeIndexs(namespace: %q, where: [%s]) { id name slug status running capacity version lastSeenAt } }`, NS, where("org_id", orgID))
	if err := c.Query(ctx, q, nil, &out); err != nil {
		return nil, err
	}
	return out.Nodes, nil
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

// HeartbeatNode reports liveness and the running session count.
func (c *Client) HeartbeatNode(ctx context.Context, nodeID, version string, running int) error {
	return c.Mutate(ctx, "heartbeatNode", nodeID, map[string]any{"version": version, "running": running})
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
	in := map[string]any{"aggregateId": itemID, "namespace": NS, "nodeId": nodeID}
	var out struct {
		IssueItemRepoToken *struct {
			Token      string `json:"token"`
			ExpiresAt  string `json:"expiresAt"`
			Repository string `json:"repository"`
		} `json:"issueItemRepoToken"`
	}
	q := `mutation($in: IssueItemRepoTokenInput!) { issueItemRepoToken(input: $in) { token expiresAt repository } }`
	if err := c.Query(ctx, q, map[string]any{"in": in}, &out); err != nil {
		var ge *Error
		if errors.As(err, &ge) && unknownOperation(ge.Message, field) {
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
	return &GitHubToken{Token: t.Token, ExpiresAt: exp, Repository: t.Repository}, nil
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
// when one drew it.
func (c *Client) PlanGoal(ctx context.Context, goalID, by string, items []PlannedItem, costCents int) error {
	list := make([]map[string]any, 0, len(items))
	for _, it := range items {
		deps := it.DependsOn
		if deps == nil {
			deps = []int{} // the input is non-null
		}
		list = append(list, map[string]any{
			"seq": it.Seq, "title": it.Title, "brief": it.Brief, "acceptance": it.Acceptance,
			"dependsOn": deps, "repoId": it.RepoID, "baseRef": it.BaseRef,
		})
	}
	m := map[string]any{"items": list, "costCents": costCents}
	if by != "" {
		m["by"] = by
	}
	return c.Mutate(ctx, "planGoal", goalID, m)
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
