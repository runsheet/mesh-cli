// Package gateway is the node's transport: the runsheet platform's GraphQL
// gateway, spoken as the person whose console token the node holds
// (mesh-specs adr-002). Every write is one aggregate command
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

// Client speaks GraphQL (and Prologue's JSON) to one gateway with one token.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New returns a client for baseURL (scheme+host, no trailing slash).
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		Token:   strings.TrimSpace(token),
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

// Error is a GraphQL error the gateway returned — an aggregate's refusal
// ("item is claimed, not ready"), the policy's denial, or a bad query.
type Error struct {
	Message string
}

func (e *Error) Error() string { return e.Message }

// IsRefusal reports whether err is the gateway saying no (as opposed to a
// transport failure): callers treat a refused claim as "someone else has it".
func IsRefusal(err error) bool {
	var ge *Error
	return errors.As(err, &ge)
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
	req.Header.Set("Authorization", "Bearer "+c.Token)
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
}

const workItemFields = `id goalId orgId seq title brief acceptance repoId baseRef dependsOn status nodeId branch costCents attempts lastError lastNote prUrl heartbeatAt evaluation evaluationPassed`

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
	ID           string   `json:"id"`
	Slug         string   `json:"slug"`
	Title        string   `json:"title"`
	Statement    string   `json:"statement"`
	Acceptance   string   `json:"acceptance"`
	ProjectID    string   `json:"projectId"`
	BudgetCents  int      `json:"budgetCents"`
	SpentCents   int      `json:"spentCents"`
	Status       string   `json:"status"`
	PausedReason string   `json:"pausedReason"`
	Checkpoints  []string `json:"checkpoints"`
}

const goalFields = `id slug title statement acceptance projectId budgetCents spentCents status pausedReason checkpoints`

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
}

// Project reads one project by id.
func (c *Client) Project(ctx context.Context, id string) (*Project, error) {
	var out struct {
		Project *Project `json:"projectIndex"`
	}
	q := fmt.Sprintf(`{ projectIndex(namespace: %q, id: %q) { id slug name repoIds } }`, NS, id)
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
	q := fmt.Sprintf(`{ projectIndexs(namespace: %q, where: [%s]) { id slug name repoIds } }`, NS, where("org_id", orgID))
	if err := c.Query(ctx, q, nil, &out); err != nil {
		return nil, err
	}
	return out.Projects, nil
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
	Slug   string `json:"slug"`
	Title  string `json:"title"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
	Repo   string `json:"repo"`
	Path   string `json:"path"`
}

func (c *Client) prologueGet(ctx context.Context, path, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/prologue/v1"+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
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
