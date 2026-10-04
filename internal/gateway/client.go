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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"projectmesh.io/mesh-cli/internal/tools"
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

// Client speaks GraphQL (and mesh-context's JSON) to one gateway as one
// principal.
type Client struct {
	BaseURL string
	Source  TokenSource
	HTTP    *http.Client

	// itemsWithoutModel is set once the gateway has refused the
	// WorkItemIndex fields items-carry-a-model added (an older
	// platform): every later item read asks for workItemFields alone.
	itemsWithoutModel atomic.Bool

	// usageWithoutTools is set once the gateway has refused a usage
	// report's `tools` (ErrNoToolUsage): every later report goes
	// without the list.
	usageWithoutTools atomic.Bool
	// usageWithoutValidation is set once the gateway has refused a usage
	// report's `validation` (ErrNoValidationUsage, node-attests-its-validation):
	// every later report goes without the list.
	usageWithoutValidation atomic.Bool
	// usageWithoutContext is set once the gateway has refused a usage
	// report's `contextBytes` (ErrNoContextUsage, adr-016): every later
	// report goes without it.
	usageWithoutContext atomic.Bool
	// usageWithoutDocumentsFetched is set once the gateway has refused a
	// usage report's `documentsFetched` (ErrNoDocumentsFetched, adr-016's
	// amendment 2026-10-02): every later report goes without it.
	usageWithoutDocumentsFetched atomic.Bool
	// usageWithoutToolNames is set once the gateway has refused a usage
	// report's `toolNames` (ErrNoToolNames, adr-016's amendment
	// 2026-10-02): every later report goes without the list.
	usageWithoutToolNames atomic.Bool

	// heartbeatBasicUntil (unix nanoseconds) is when the heartbeat next
	// offers the fields the gateway refused (see HeartbeatNode).
	heartbeatBasicUntil atomic.Int64
	// nodeAnswerUntil (unix nanoseconds) is when registerNode and
	// heartbeatNode next ask their payload for the node's answer after
	// the gateway refused it (see mutateForAnswer): until then they ask
	// `status` alone, one request each, as before the answer existed.
	nodeAnswerUntil atomic.Int64
	// nodesWithoutDesired is set once the gateway has refused
	// NodeIndex.desiredVersion (a platform older than
	// update-nodes-from-console): every later node read leaves it out.
	nodesWithoutDesired atomic.Bool
	// directionsWithoutMode is set once the gateway has refused
	// DirectionIndex.mode (a platform older than runsheet ADR 022):
	// every later direction read asks for directionFields alone.
	directionsWithoutMode atomic.Bool
	// goalsWithoutNarrow is set once the gateway has refused GoalIndex's
	// optional fields (replacingItemId, narrowReplans): goals are read
	// with the base fields alone from then on.
	goalsWithoutNarrow atomic.Bool
	// projectsWithoutExtras is set once the gateway has refused
	// ProjectIndex's optional fields (protectedPaths): projects are
	// read with the base fields alone from then on (goal
	// one-evaluator-for-ordinary-work).
	projectsWithoutExtras atomic.Bool
	// credentialsWithoutEnterprise is set once the gateway has refused
	// CredentialIndex.enterpriseId (a platform older than runsheet ADR
	// 030): every later credential read asks for credentialFields alone.
	credentialsWithoutEnterprise atomic.Bool
	// orgsWithoutEnterprise is set once the gateway has refused
	// OrgIndex.enterpriseId: an org then has no enterprise to read
	// credentials of.
	orgsWithoutEnterprise atomic.Bool
	// toolsUnknown holds each claim-time tools field (itemTools,
	// planTools, researchTools) the gateway has refused — a platform
	// older than tools-in-the-console, or without that session kind's
	// read: every later read of that kind asks for credentials alone.
	toolsUnknown sync.Map

	// scalingSchema is what the gateway's heartbeat input was last found
	// to take of `scaling` (see HeartbeatNode); Now is its clock,
	// time.Now when nil.
	scalingSchema scalingProbe
	// holdingSchema is whether the gateway's heartbeat input was last
	// found to take `holding` (see HeartbeatNode, takesHolding).
	holdingSchema holdingProbe
	// holdingUnclaimedUntilSchema is whether the gateway's
	// HeartbeatNodeInput's `holding.unclaimed` element type was last
	// found to take `until` (see takesHoldingUnclaimedUntil).
	holdingUnclaimedUntilSchema holdingProbe
	// holdingOrgsSchema is whether the gateway's HeartbeatNodeInput's
	// `holding` was last found to take `orgs` (see takesHoldingOrgs).
	holdingOrgsSchema holdingProbe
	// commitSchema is whether the gateway's heartbeat input was last
	// found to take `commit` (see takesCommit).
	commitSchema commitProbe
	// ownerSchema is which owner field the gateway's RegisterNodeInput
	// was last found to have (see OwnerField).
	ownerSchema ownerProbe
	// nodeKindSchema is whether the gateway's RegisterNodeInput was last
	// found to take `kind` (see TakesNodeKind).
	nodeKindSchema holdingProbe
	// toolErrorSchema is whether the gateway's usage report's `tools`
	// entry was last found to take `error` (see takesToolError).
	toolErrorSchema toolErrorProbe
	// noVerdictSchema is whether the gateway's evaluateWorkItem input was
	// last found to take `noVerdict` (see EvaluateWorkItemNoVerdict).
	noVerdictSchema holdingProbe
	// clausesSchema is whether the gateway's evaluateWorkItem input was
	// last seen to take failedClauses, kept for ScalingProbeEvery.
	clausesSchema holdingProbe
	// questionsSchema is whether the gateway's planGoal input was last
	// seen to take questions, kept for ScalingProbeEvery.
	questionsSchema holdingProbe
	// preconditionSchema is whether the gateway's PlannedItemInput was
	// last seen with `precondition`, and when (TakesItemPrecondition).
	preconditionSchema holdingProbe
	// limitsPrioritiesSchema is whether the gateway's heartbeat `limits`
	// list input was last found to take `priorities` (see
	// takesLimitsPriorities).
	limitsPrioritiesSchema holdingProbe
	// replaceItemSchema is whether the gateway's requestPlan input was
	// last seen to take replaceItem, kept for ScalingProbeEvery.
	replaceItemSchema holdingProbe
	// adoptedFieldsSchema is whether the gateway's HeartbeatNodeInput's
	// `adopted` element type was last found to take goalId, directionId,
	// sessionId and alive (see takesAdoptedFields).
	adoptedFieldsSchema holdingProbe
	// attestationSchema is whether the gateway's Mutation was last found
	// to have recordValidationAttestation (see takesAttestations).
	attestationSchema attestationProbe
	// goalSplitSchema is whether the gateway's Mutation was last found to
	// have proposeGoalSplit (see TakesGoalSplit).
	goalSplitSchema holdingProbe
	// actorSchema is, per input type, whether the gateway's command input
	// was last found to take `actor` (see withActor).
	actorSchema actorProbes
	// descriptionSchema is whether the gateway's FinishWorkItemInput was
	// last found to take `description` (see withDescription,
	// every-actor-has-a-name item 3).
	descriptionSchema holdingProbe
	// scopeSharesSchema is whether the gateway's registerNode/heartbeatNode
	// payload's `scopes` element type was last found to take `shares`
	// (see takesScopeShares).
	scopeSharesSchema holdingProbe
	Now               func() time.Time
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

// WithItemToken returns a client that speaks to the same gateway on tok
// rather than c's own bearer — an item's claim-time token
// (node-attests-its-validation), scoped the way IssueItemRepoToken's
// GitHub token is (adr-001-item-scoped-github-token), for
// RecordValidationAttestation. It is a fresh Client of the transport
// alone (BaseURL, HTTP, Now): none of c's schema-probe caches
// (itemsWithoutModel and the rest) carry over, since they answer for a
// long-lived node client, not a request or two on a borrowed token.
func (c *Client) WithItemToken(tok string) *Client {
	return &Client{BaseURL: c.BaseURL, Source: StaticToken(tok), HTTP: c.HTTP, Now: c.Now}
}

// Bearer is the token the next request would carry — for the mesh-context
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
	// Status is the HTTP status the answer carried when it was not 2xx —
	// a GraphQL error body a proxy or the gateway itself wrapped with a
	// 429 or 5xx, as isTransient and retryAfterOf read it. 0 for the
	// ordinary 200 refusal.
	Status int
	// RetryAfter is the answer's Retry-After header, when it named one
	// alongside a non-2xx status.
	RetryAfter time.Duration
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
			return &httpError{Status: resp.StatusCode, Text: resp.Status, Body: strings.TrimSpace(string(raw)), RetryAfter: parseRetryAfter(resp)}
		}
		return fmt.Errorf("gateway: decode response: %w", jsonErr)
	}
	if len(env.Errors) > 0 {
		ge := &Error{Message: env.Errors[0].Message}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			ge.Status = resp.StatusCode
			ge.RetryAfter = parseRetryAfter(resp)
		}
		return ge
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &httpError{Status: resp.StatusCode, Text: resp.Status, RetryAfter: parseRetryAfter(resp)}
	}
	if out != nil && len(env.Data) > 0 {
		return json.Unmarshal(env.Data, out)
	}
	return nil
}

// parseRetryAfter reads a Retry-After header as whole seconds, the only
// form the gateway sends; a missing or unparseable one is 0.
func parseRetryAfter(resp *http.Response) time.Duration {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs < 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// httpError is a non-2xx HTTP answer with no GraphQL error body — a
// proxy's 502/503/504, or any status Query could not decode as GraphQL
// at all. isTransient treats a 5xx one, or a 429, as worth another try,
// as it does a transport failure that never reached the gateway.
type httpError struct {
	Status     int
	Text       string
	Body       string
	RetryAfter time.Duration
}

func (e *httpError) Error() string {
	if e.Body == "" {
		return "gateway: " + e.Text
	}
	return fmt.Sprintf("gateway: %s: %s", e.Text, e.Body)
}

// Mutate sends one aggregate command: field is the mutation name
// ("claimWorkItem"), the input type is its capitalised form + "Input".
// aggregateId and namespace are set from id; the rest is input. The
// payload is asked for as `status` alone and discarded; MutateFields
// asks for more of it.
func (c *Client) Mutate(ctx context.Context, field, aggregateID string, input map[string]any) error {
	return c.MutateFields(ctx, field, aggregateID, input, "status", nil)
}

// MutateFields is Mutate with the mutation's payload selection named by
// fields (beside `status`, always asked for) and decoded into out — nil
// discards it, as Mutate does. A field the platform's payload type does
// not have yet refuses the whole call with "Cannot query field"; the
// caller decides what that means (RegisterNode and HeartbeatNode read
// it as no answer, not an error).
func (c *Client) MutateFields(ctx context.Context, field, aggregateID string, input map[string]any, fields string, out any) error {
	in := map[string]any{"aggregateId": aggregateID, "namespace": NS}
	for k, v := range input {
		in[k] = v
	}
	c.withActor(ctx, field, in)
	typ := strings.ToUpper(field[:1]) + field[1:] + "Input"
	q := fmt.Sprintf("mutation($in: %s!) { %s(input: $in) { %s } }", typ, field, fields)
	if out == nil {
		return c.Query(ctx, q, map[string]any{"in": in}, nil)
	}
	var env map[string]json.RawMessage
	if err := c.Query(ctx, q, map[string]any{"in": in}, &env); err != nil {
		return err
	}
	payload := env[field]
	if len(payload) == 0 || string(payload) == "null" {
		return nil
	}
	return json.Unmarshal(payload, out)
}

// ErrNoUsageLedger is what a finishing command returns when it carried a
// non-nil usage report but the gateway's input has no `usage` field (a
// platform older than the usage ledger, runsheet trd §6a): the command
// itself landed, sent again without the report, so the caller's item,
// evaluation, plan, rebase or research still finishes — only the usage
// report did not, and the caller falls back to its own log line. Any
// other error from the command is returned as is. errors.Is-able.
var ErrNoUsageLedger = errors.New("gateway: the platform keeps no usage ledger")

// ErrNoToolUsage is what a finishing command returns when its usage
// report carried a per-tool `tools` list but the platform's
// UsageReportInput has no `tools` (a platform older than
// tools-show-their-use): the command and the report landed, the
// report's total `toolCalls` kept, without the list — the caller logs
// the list instead. errors.Is-able.
var ErrNoToolUsage = errors.New("gateway: the platform takes no per-tool calls on the usage report")

// ErrNoUsageDetail is what a finishing command returns when its usage
// report carried turns, per-turn input or the largest tool results but
// the gateway's UsageReportInput has none of those fields (a platform
// older than mesh-spends-less-per-item): the report is resent without
// them, and the rest of the usage — and the command itself — lands;
// only the per-turn and tool-result figures did not. errors.Is-able.
var ErrNoUsageDetail = errors.New("gateway: the platform's usage report carries no turn or tool-result detail")

// ErrNoValidationUsage is what a finishing command returns when its
// usage report carried per-step validation minutes but the platform's
// UsageReportInput has no `validation` (a platform older than
// node-attests-its-validation): the command and the rest of the report
// land without it, and the caller logs the minutes instead. errors.Is-able.
var ErrNoValidationUsage = errors.New("gateway: the platform takes no per-step validation minutes on the usage report")

// ErrNoContextUsage is what a finishing command returns when its usage
// report carried the session's context bytes but the platform's
// UsageReportInput has no `contextBytes` (a platform older than
// context-budget-that-holds, adr-016): the command and the rest of the
// report land without that one number, and the caller says so once.
// errors.Is-able; it may come joined with ErrNoValidationUsage or
// ErrNoToolUsage when the report lost those too.
var ErrNoContextUsage = errors.New("gateway: the platform takes no context bytes on the usage report")

// ErrNoDocumentsFetched is what a finishing command returns when its
// usage report carried the session's documents-fetched count but the
// platform's UsageReportInput has no `documentsFetched` (a platform
// older than context-is-fetched-not-shipped, adr-016's amendment
// 2026-10-02): the command and the rest of the report land without
// that one number, and the caller says so once. errors.Is-able; it may
// come joined with the other usage errors.
var ErrNoDocumentsFetched = errors.New("gateway: the platform takes no documents-fetched count on the usage report")

// ErrNoToolNames is what a finishing command returns when its usage
// report carried the session's per-tool call counts but the platform's
// UsageReportInput has no `toolNames` (a platform older than
// context-is-fetched-not-shipped, adr-016's amendment 2026-10-02): the
// command and the rest of the report land without the list, and the
// caller says so once. errors.Is-able; it may come joined with the
// other usage errors.
var ErrNoToolNames = errors.New("gateway: the platform takes no per-tool call counts on the usage report")

// usageDetailFields are UsageReport's fields a platform before
// mesh-spends-less-per-item may not know (ErrNoUsageDetail).
var usageDetailFields = []string{"turns", "inputPerTurnMean", "inputPerTurnMax", "largestToolResults"}

// mutateWithUsage sends field's command with input, plus usage's report
// on the `usage` key when usage is non-nil. A platform whose input has
// no `usage` at all (unknownField on "usage") gets the command again
// without it: the command lands, and ErrNoUsageLedger says the report
// did not. A platform whose report has no `tools` (unknownField on
// "tools") gets it again without the list, and every later report goes
// without it: the command lands with the total, and ErrNoToolUsage says
// the list did not. A platform whose `usage` input lacks only the turn
// or tool-result detail fields (unknownField on one of
// usageDetailFields) gets the report again without those alone: the
// rest of the report and the command land, and ErrNoUsageDetail says
// the detail did not. A platform whose report has no `validation`
// (unknownField on "validation") gets it again without the per-step
// minutes, and every later report goes without them: the command lands
// with the rest of the report, and ErrNoValidationUsage says the
// minutes did not. A platform whose report has no `contextBytes`
// (unknownField on "contextBytes") gets it again without the bytes, and
// every later report goes without them, and ErrNoContextUsage says so.
// A platform whose report has no `documentsFetched` or `toolNames`
// (unknownField on either) gets it again without that one, and every
// later report goes without it, and ErrNoDocumentsFetched or
// ErrNoToolNames says so (adr-016's amendment 2026-10-02). When a
// report lost more than one of the list, the minutes, the bytes, the
// documents fetched and the tool names, their errors come joined.
func (c *Client) mutateWithUsage(ctx context.Context, field, aggregateID string, input map[string]any, usage *UsageReport) error {
	if usage == nil {
		return c.Mutate(ctx, field, aggregateID, input)
	}
	um, err := usageInput(usage)
	if err != nil {
		return err
	}
	withUsage := make(map[string]any, len(input)+1)
	for k, v := range input {
		withUsage[k] = v
	}
	withUsage["usage"] = um
	_, withTools := um["tools"]
	if withTools && c.usageWithoutTools.Load() {
		delete(um, "tools")
	}
	_, withValidation := um["validation"]
	if withValidation && c.usageWithoutValidation.Load() {
		delete(um, "validation")
	}
	_, withContext := um["contextBytes"]
	if withContext && c.usageWithoutContext.Load() {
		delete(um, "contextBytes")
	}
	_, withDocumentsFetched := um["documentsFetched"]
	if withDocumentsFetched && c.usageWithoutDocumentsFetched.Load() {
		delete(um, "documentsFetched")
	}
	_, withToolNames := um["toolNames"]
	if withToolNames && c.usageWithoutToolNames.Load() {
		delete(um, "toolNames")
	}
	if rows, ok := um["tools"].([]any); ok && toolRowsCarry(rows, "error") && !c.takesToolError(ctx) {
		for _, r := range rows {
			if row, ok := r.(map[string]any); ok {
				delete(row, "error")
			}
		}
	}
	err = c.Mutate(ctx, field, aggregateID, withUsage)
	var ge *Error
	// Each optional field the platform names is dropped and the command
	// sent again, whatever order the refusals come in: a platform that
	// knows none of them refuses one per attempt.
	optional := []struct {
		name    string
		without *atomic.Bool
	}{
		{"tools", &c.usageWithoutTools},
		{"validation", &c.usageWithoutValidation},
		{"contextBytes", &c.usageWithoutContext},
		{"documentsFetched", &c.usageWithoutDocumentsFetched},
		{"toolNames", &c.usageWithoutToolNames},
	}
	// The turn and tool-result detail fields go together, for this
	// report alone, in the same loop: a platform that refuses one of the
	// optional fields and then the detail (or the other way round) loses
	// neither the command nor the word on what it dropped.
	noDetail := false
	for retried := true; retried && err != nil && errors.As(err, &ge); {
		retried = false
		for _, o := range optional {
			if _, sent := um[o.name]; sent && unknownField(ge.Message, o.name) {
				o.without.Store(true)
				delete(um, o.name)
				err = c.Mutate(ctx, field, aggregateID, withUsage)
				retried = true
				break
			}
		}
		if !retried && !noDetail && unknownField(ge.Message, usageDetailFields...) {
			for _, f := range usageDetailFields {
				delete(um, f)
			}
			noDetail = true
			err = c.Mutate(ctx, field, aggregateID, withUsage)
			retried = true
		}
	}
	if err != nil && errors.As(err, &ge) && unknownField(ge.Message, "usage") {
		if err2 := c.Mutate(ctx, field, aggregateID, input); err2 != nil {
			return err2
		}
		return ErrNoUsageLedger
	}
	if err != nil {
		return err
	}
	var lost []error
	if noDetail {
		lost = append(lost, ErrNoUsageDetail)
	}
	if withTools && c.usageWithoutTools.Load() {
		lost = append(lost, ErrNoToolUsage)
	}
	if withValidation && c.usageWithoutValidation.Load() {
		lost = append(lost, ErrNoValidationUsage)
	}
	if withContext && c.usageWithoutContext.Load() {
		lost = append(lost, ErrNoContextUsage)
	}
	if withDocumentsFetched && c.usageWithoutDocumentsFetched.Load() {
		lost = append(lost, ErrNoDocumentsFetched)
	}
	if withToolNames && c.usageWithoutToolNames.Load() {
		lost = append(lost, ErrNoToolNames)
	}
	switch len(lost) {
	case 0:
		return nil
	case 1:
		return lost[0]
	}
	return errors.Join(lost...)
}

// toolRowsCarry says whether any of a usage map's tools rows has key.
func toolRowsCarry(rows []any, key string) bool {
	for _, r := range rows {
		if row, ok := r.(map[string]any); ok {
			if _, ok := row[key]; ok {
				return true
			}
		}
	}
	return false
}

// toolErrorProbe is the cached answer to whether UsageReportInput's
// `tools` entry takes `error`, and when it was asked.
type toolErrorProbe struct {
	mu    sync.Mutex
	at    time.Time
	takes bool
}

// takesToolError says whether the gateway's UsageReportInput has a
// `tools` whose entry takes `error` — asked by introspection only when
// a report carries one, and kept for ScalingProbeEvery, as the
// heartbeat's `scaling` is: a platform older than the field would
// refuse the whole list, and the report would lose it to the no-tools
// resend. A failed probe is a no, kept as long; a scalar `tools` takes
// any shape.
func (c *Client) takesToolError(ctx context.Context) bool {
	p := &c.toolErrorSchema
	p.mu.Lock()
	defer p.mu.Unlock()
	now := c.now()
	if !p.at.IsZero() && now.Sub(p.at) < ScalingProbeEvery {
		return p.takes
	}
	type typeRef struct {
		Kind        string `json:"kind"`
		InputFields []struct {
			Name string `json:"name"`
		} `json:"inputFields"`
		OfType *typeRef `json:"ofType"`
	}
	var out struct {
		Type *struct {
			InputFields []struct {
				Name string  `json:"name"`
				Type typeRef `json:"type"`
			} `json:"inputFields"`
		} `json:"__type"`
	}
	p.at, p.takes = now, false
	q := `{ __type(name: "UsageReportInput") { inputFields { name type { kind inputFields { name } ofType { kind inputFields { name } ofType { kind inputFields { name } ofType { kind inputFields { name } } } } } } } }`
	if err := c.Query(ctx, q, nil, &out); err != nil || out.Type == nil {
		return false
	}
	for _, f := range out.Type.InputFields {
		if f.Name != "tools" {
			continue
		}
		t := &f.Type
		for (t.Kind == "NON_NULL" || t.Kind == "LIST") && t.OfType != nil {
			t = t.OfType
		}
		if t.Kind == "SCALAR" {
			p.takes = true
		}
		for _, sf := range t.InputFields {
			if sf.Name == "error" {
				p.takes = true
			}
		}
	}
	return p.takes
}

// usageInput is a UsageReport as a map for a command's `usage` key: its
// JSON shape, decoded back into map[string]any.
func usageInput(u *UsageReport) (map[string]any, error) {
	data, err := json.Marshal(u)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
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
	// TenantSlug names an org with no GitHub login (goal
	// the-node-serves-its-owners-orgs): the node shows it in Login's
	// place wherever the org has none.
	TenantSlug string `json:"tenantSlug"`
}

// Name is what to show a person for this org: Login, or TenantSlug
// when it has no GitHub login.
func (o Org) Name() string {
	if o.Login != "" {
		return o.Login
	}
	return o.TenantSlug
}

// MyOrgs lists the orgs the person reaches. myOrgs is a Map scalar
// (schema.json), so it takes no selection: the platform's row arrives
// whole, tenantSlug included (runsheet gateway/person.go, its
// specs/contracts/myOrgs.json), and Org decodes what it names.
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

// Enterprise is one enterprise the person administers (myEnterprises).
type Enterprise struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	DisplayName string `json:"displayName"`
	TenantSlug  string `json:"tenantSlug"`
	Status      string `json:"status"`
}

// MyEnterprises lists the enterprises the person administers.
// myEnterprises is a Map scalar (schema.json), so it takes no
// selection: decoded as MyOrgs decodes myOrgs.
func (c *Client) MyEnterprises(ctx context.Context) ([]Enterprise, error) {
	var out struct {
		MyEnterprises json.RawMessage `json:"myEnterprises"`
	}
	if err := c.Query(ctx, `{ myEnterprises }`, nil, &out); err != nil {
		return nil, err
	}
	var es []Enterprise
	if len(out.MyEnterprises) == 0 || string(out.MyEnterprises) == "null" {
		return nil, nil
	}
	// The Map scalar arrives as a JSON array, or as a string holding one.
	if err := json.Unmarshal(out.MyEnterprises, &es); err != nil {
		var s string
		if err2 := json.Unmarshal(out.MyEnterprises, &s); err2 != nil {
			return nil, fmt.Errorf("gateway: decode myEnterprises: %w", err)
		}
		if err := json.Unmarshal([]byte(s), &es); err != nil {
			return nil, fmt.Errorf("gateway: decode myEnterprises: %w", err)
		}
	}
	return es, nil
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

// ErrNotFound is WorkItem's and Goal's answer for an id the platform
// has no row for.
var ErrNotFound = errors.New("not found")

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
	// ReadyAt is when the item last became ready (RFC 3339); empty from
	// an older platform. The claim order breaks ties on it (ADR-017).
	ReadyAt string `json:"readyAt"`
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
	// ProjectID is the mesh-context project the item's goal is in: the scope
	// its agent resolves in (AgentIDs).
	ProjectID string `json:"projectId"`
	// ProtectedFiles are this item's own touched files that matched a
	// protected rule; ProtectedRulePaths the rule's own globs. Both empty
	// from an older platform (`?` in fields/WorkItemIndex.txt; goal
	// one-evaluator-for-ordinary-work): an empty ProtectedFiles reads as
	// no protected path touched, never as unknown.
	ProtectedFiles     []string `json:"protectedFiles"`
	ProtectedRulePaths []string `json:"protectedRulePaths"`
	// AgentSlug names the agent that builds the item (runsheet trd §6c);
	// empty is the role's resolved definition.
	AgentSlug string `json:"agentSlug"`
	// Model is the tier the item is built on (runsheet trd §6a,
	// items-carry-a-model): the plan's, else its named agent's; empty is
	// the node's own. EvaluateModel is the tier the platform chose to
	// evaluate it on. Both are empty from a platform older than the goal.
	Model         string `json:"model"`
	EvaluateModel string `json:"evaluateModel"`
	// Outcome is how the item finished (mesh ADR 003 decision 1):
	// OutcomeBuilt, a pushed branch, or OutcomeAlreadyDone, main already
	// held the work and Evidence says where — merged PR URLs and/or
	// commit SHAs on the base ref, with a line on why. Empty decodes as
	// built. The reads select them once schema.json carries them (ADR
	// 010: `evidence?` and `outcome?` in fields/WorkItemIndex.txt); until
	// the platform serves them every item reads as built.
	Outcome  string `json:"outcome"`
	Evidence string `json:"evidence"`
	// LineageRootSeq is the seq of the first item of the item's lineage
	// and LineageAttempt which attempt of it the item is (runsheet ADR
	// 023's 2026-10-02 amendment, a cap of 3); both 0 from an older
	// platform (`?` in fields/WorkItemIndex.txt).
	LineageRootSeq int `json:"lineageRootSeq"`
	LineageAttempt int `json:"lineageAttempt"`
	// Atomic is the planner's reason the item cannot be halved, read
	// from its brief's first line (AtomicReason): planGoal has no field
	// for it yet (mesh-cli ADR-021).
	Atomic string `json:"-"`
}

// atomicPrefix opens a brief's first line when the planner marked the
// item atomic (ADR-021).
const atomicPrefix = "atomic:"

// AtomicReason is the reason on a brief's first line `atomic: <reason>`,
// or "" when the brief does not open with one.
func AtomicReason(brief string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(brief), "\n")
	first = strings.TrimSpace(first)
	if len(first) < len(atomicPrefix) || !strings.EqualFold(first[:len(atomicPrefix)], atomicPrefix) {
		return ""
	}
	return strings.TrimSpace(first[len(atomicPrefix):])
}

// BriefWithAtomic is brief with `atomic: <reason>` as its first line, so
// the item's record — and so the goal — carries the reason; a brief
// unchanged when reason is empty.
func BriefWithAtomic(brief, reason string) string {
	reason = strings.Join(strings.Fields(reason), " ")
	brief = strings.TrimSpace(brief)
	if reason == "" {
		return brief
	}
	if brief == "" {
		return atomicPrefix + " " + reason
	}
	return atomicPrefix + " " + reason + "\n\n" + brief
}

// preconditionPrefix opens a brief's line naming a clause the item waits
// on — a person's confirmation or a production observation no builder
// can open (ADR-023's amendment, the cut-over rule) — after atomic's
// line when the brief has one.
const preconditionPrefix = "precondition:"

// PreconditionOf is the clause on a brief's `precondition: <clause>`
// line — its first line, or its first after an `atomic:` one — or ""
// when the brief carries none.
func PreconditionOf(brief string) string {
	brief = strings.TrimSpace(brief)
	if AtomicReason(brief) != "" {
		_, rest, _ := strings.Cut(brief, "\n")
		brief = strings.TrimSpace(rest)
	}
	first, _, _ := strings.Cut(brief, "\n")
	first = strings.TrimSpace(first)
	if len(first) < len(preconditionPrefix) || !strings.EqualFold(first[:len(preconditionPrefix)], preconditionPrefix) {
		return ""
	}
	return strings.TrimSpace(first[len(preconditionPrefix):])
}

// BriefWithPrecondition is brief with `precondition: <clause>` as its
// first line, so the item's record carries what it waits on wherever
// planGoal takes no field for it; a brief unchanged when clause is
// empty. BriefWithAtomic over it puts atomic's line first.
func BriefWithPrecondition(brief, clause string) string {
	clause = strings.Join(strings.Fields(clause), " ")
	brief = strings.TrimSpace(brief)
	if clause == "" {
		return brief
	}
	if brief == "" {
		return preconditionPrefix + " " + clause
	}
	return preconditionPrefix + " " + clause + "\n\n" + brief
}

// The outcomes a WorkItem finishes with (mesh ADR 003 decision 1).
const (
	OutcomeBuilt       = "built"
	OutcomeAlreadyDone = "already_done"
)

// UnmarshalJSON decodes the row, an empty outcome as built.
func (w *WorkItem) UnmarshalJSON(b []byte) error {
	type row WorkItem
	var r row
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	if r.Outcome == "" {
		r.Outcome = OutcomeBuilt
	}
	r.Atomic = AtomicReason(r.Brief)
	*w = WorkItem(r)
	return nil
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

// readItems runs one WorkItemIndex read, query being the document with
// its selection left to fill in. The first read asks for the optional
// fields too (fields/WorkItemIndex.txt's `?`: what items-carry-a-model
// put on WorkItemIndex, runsheet trd §6a); a gateway that does not know
// one of them is an older platform, and from then on the client asks
// for the base fields alone.
func (c *Client) readItems(ctx context.Context, query func(fields string) string, out any) error {
	if !c.itemsWithoutModel.Load() {
		err := c.Query(ctx, query(workItemFields+" "+workItemOptionalFields), nil, out)
		var ge *Error
		if err == nil || !errors.As(err, &ge) || !unknownField(ge.Message, workItemOptional...) {
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
		return nil, fmt.Errorf("work item %s %w", id, ErrNotFound)
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

// Scorecard reads the org's scorecard (runsheet trd §6a, a Map field
// like planCredentials or directionResearch, no F.* selection to send)
// and returns project's own entry from its `projects` list — a nil map,
// no error, on an older platform without the query or a project the
// scorecard does not name.
func (c *Client) Scorecard(ctx context.Context, org, project string) (map[string]any, error) {
	var out struct {
		Scorecard map[string]any `json:"scorecard"`
	}
	q := fmt.Sprintf(`{ scorecard(orgId: %q) }`, org)
	if err := c.Query(ctx, q, nil, &out); err != nil {
		var ge *Error
		if errors.As(err, &ge) && unknownField(ge.Message, "scorecard") {
			return nil, nil
		}
		return nil, err
	}
	for _, row := range rowsOf(out.Scorecard["projects"]) {
		if str(row, "projectId") == project || strings.EqualFold(str(row, "projectName"), project) {
			return row, nil
		}
	}
	return nil, nil
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
	// ReplacingItemID is the failed item a narrow re-plan replaces alone
	// (runsheet ADR 023's third rung), empty for a whole plan; the
	// planner then asks for a split of that item (ADR-021's amendment).
	// NarrowReplans counts the goal's narrow re-plans. Both are optional:
	// an older platform serves neither, and every plan reads as whole.
	ReplacingItemID string `json:"replacingItemId"`
	NarrowReplans   int    `json:"narrowReplans"`
	// RepoID is the goal's own repo (GoalIndex.repoId, ADR-023's repo
	// rule (d)): the planner's project-wide fetch still reads every repo
	// of the project, but an item naming a repo other than this one is a
	// different goal. Today's platform does not yet serve the field — it
	// is not in fields/GoalIndex.txt, so this is always "" until it does
	// (ADR-010: a field the platform has not shipped cannot be
	// selected); the node falls back to the project's own repo when it
	// has exactly one (internal/node.goalRepo).
	RepoID string `json:"repoId"`
	// UpdatedAt is when the goal row last changed — the row's own change,
	// not a plan session's: a claim held for budget, pause or approval
	// waits on it (goal a-refused-plan-is-released-not-replanned). Zero
	// from an older platform (`?` in fields/GoalIndex.txt).
	UpdatedAt time.Time `json:"updatedAt"`
}

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
	q := func(fields string) string {
		return fmt.Sprintf(`{ goalIndex(namespace: %q, id: %q) { %s } }`, NS, id, fields)
	}
	if err := c.readGoals(ctx, q, &out); err != nil {
		return nil, err
	}
	if out.Goal == nil {
		return nil, fmt.Errorf("goal %s %w", id, ErrNotFound)
	}
	return out.Goal, nil
}

// Goals lists an org's goals.
func (c *Client) Goals(ctx context.Context, orgID string) ([]Goal, error) {
	var out struct {
		Goals []Goal `json:"goalIndexs"`
	}
	q := func(fields string) string {
		return fmt.Sprintf(`{ goalIndexs(namespace: %q, where: [%s]) { %s } }`, NS, where("org_id", orgID), fields)
	}
	if err := c.readGoals(ctx, q, &out); err != nil {
		return nil, err
	}
	return out.Goals, nil
}

// readGoals runs one GoalIndex read, query being the document with its
// selection left to fill in. It asks for the optional fields too
// (fields/GoalIndex.txt's `?`: replacingItemId and narrowReplans,
// runsheet ADR 023); a gateway that does not know one is an older
// platform, and from then on the client asks for goalFields alone —
// every plan reads as whole.
func (c *Client) readGoals(ctx context.Context, query func(fields string) string, out any) error {
	if !c.goalsWithoutNarrow.Load() {
		err := c.Query(ctx, query(goalFields+" "+goalOptionalFields), nil, out)
		var ge *Error
		if err == nil || !errors.As(err, &ge) || !unknownField(ge.Message, goalOptional...) {
			return err
		}
		c.goalsWithoutNarrow.Store(true)
	}
	return c.Query(ctx, query(goalFields), nil, out)
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
	// ProtectedPaths are the project's own protected-path globs; empty
	// from an older platform (`?` in fields/ProjectIndex.txt; goal
	// one-evaluator-for-ordinary-work).
	ProtectedPaths []string `json:"protectedPaths"`
	// MaxItems is the project's cap on a plan's items (ADR-023 rule
	// (a)). It is 0 from a platform not serving ProjectIndex.maxItems —
	// every platform today, and the allow-list does not ask for it yet
	// (ADR-023's amendment, ADR-010) — so read ItemCap, never this.
	MaxItems int `json:"maxItems"`
}

// DefaultMaxItems is a project's cap on a plan's items when the
// platform serves no maxItems of its own (ADR-023, the size rule: one
// repo, at most eight items, one pull request per item).
const DefaultMaxItems = 8

// ItemCap is the planner's cap on a plan's items: the project's own
// MaxItems when set, else DefaultMaxItems.
func (p Project) ItemCap() int {
	if p.MaxItems > 0 {
		return p.MaxItems
	}
	return DefaultMaxItems
}

// Project reads one project by id.
func (c *Client) Project(ctx context.Context, id string) (*Project, error) {
	var out struct {
		Project *Project `json:"projectIndex"`
	}
	q := func(fields string) string {
		return fmt.Sprintf(`{ projectIndex(namespace: %q, id: %q) { %s } }`, NS, id, fields)
	}
	if err := c.readProjects(ctx, q, &out); err != nil {
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
	q := func(fields string) string {
		return fmt.Sprintf(`{ projectIndexs(namespace: %q, where: [%s]) { %s } }`, NS, where("org_id", orgID), fields)
	}
	if err := c.readProjects(ctx, q, &out); err != nil {
		return nil, err
	}
	return out.Projects, nil
}

// readProjects runs one ProjectIndex read, query being the document with
// its selection left to fill in. It asks for the optional fields too
// (fields/ProjectIndex.txt's `?`: protectedPaths); a gateway that
// refuses one is a platform without it, and from then on the client
// asks for projectFields alone — every project reads with no protected
// paths (goal one-evaluator-for-ordinary-work).
func (c *Client) readProjects(ctx context.Context, query func(fields string) string, out any) error {
	if !c.projectsWithoutExtras.Load() {
		err := c.Query(ctx, query(projectFields+" "+projectOptionalFields), nil, out)
		var ge *Error
		if err == nil || !errors.As(err, &ge) || !unknownField(ge.Message, projectOptional...) {
			return err
		}
		c.projectsWithoutExtras.Store(true)
	}
	return c.Query(ctx, query(projectFields), nil, out)
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
	// Mode is what the direction's research does with its proposals
	// (runsheet ADR 022, adr-022-a-direction-sets-or-proposes):
	// `propose` — they wait for a person (pitches, or GoalProposals on
	// an older platform) and set no goal; `set` — goals are set up to
	// the room. Empty — a platform older than the field, or a direction
	// left to the project's level, which the node does not read — is
	// treated as set: guarded by the room.
	Mode string `json:"mode"`
	// The claim, as the platform last folded it: the node holding this
	// direction's session, when it took it, and when the last session
	// ended (the gap counts from there).
	ResearchNodeID    string `json:"researchNodeId"`
	ResearchClaimedAt string `json:"researchClaimedAt"`
	ResearchedAt      string `json:"researchedAt"`
}

// Active reports whether the direction is one to offer the platform at
// all: switched on and not archived. Whether it may run *now* — the
// window, the gap, the claim — is the platform's to judge.
func (d Direction) Active() bool {
	return d.Enabled && d.Status != "archived"
}

// Proposes reports whether the direction's research only proposes —
// pitches or proposals a person accepts, none taking a live-goal slot —
// rather than setting goals (runsheet ADR 022).
func (d Direction) Proposes() bool { return d.Mode == "propose" }

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
	q := func(fields string) string {
		return fmt.Sprintf(`{ directionIndexs(namespace: %q, where: [%s]) { %s } }`, NS, filter, fields)
	}
	if err := c.readDirections(ctx, q, &out); err != nil {
		return nil, err
	}
	return out.Directions, nil
}

// readDirections runs one DirectionIndex read, query being the document
// with its selection left to fill in. It asks for the optional fields
// too (fields/DirectionIndex.txt's `?`: mode, runsheet ADR 022); a
// gateway that does not know one is an older platform, and from then on
// the client asks for directionFields alone — every direction reads as
// mode-less, guarded by the room.
func (c *Client) readDirections(ctx context.Context, query func(fields string) string, out any) error {
	if !c.directionsWithoutMode.Load() {
		err := c.Query(ctx, query(directionFields+" "+directionOptionalFields), nil, out)
		var ge *Error
		if err == nil || !errors.As(err, &ge) || !unknownField(ge.Message, directionOptional...) {
			return err
		}
		c.directionsWithoutMode.Store(true)
	}
	return c.Query(ctx, query(directionFields), nil, out)
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

// ProjectRepos reads a project's repos by id, in order, each with its
// own account's login as Owner — never the org's. The schema has no
// project-repos read with owner logins and RepoIndex carries no login,
// so this composes it from repoIndex and accountIndex, reading each
// distinct account once even when several repos share it.
func (c *Client) ProjectRepos(ctx context.Context, repoIDs []string) ([]Repo, error) {
	if len(repoIDs) == 0 {
		return []Repo{}, nil
	}
	logins := map[string]string{}
	out := make([]Repo, 0, len(repoIDs))
	for _, id := range repoIDs {
		var res struct {
			Repo *Repo `json:"repoIndex"`
		}
		q := fmt.Sprintf(`{ repoIndex(namespace: %q, id: %q) { id name accountId defaultBranch } }`, NS, id)
		if err := c.Query(ctx, q, nil, &res); err != nil {
			return nil, err
		}
		if res.Repo == nil {
			return nil, fmt.Errorf("repo %s not found", id)
		}
		login, ok := logins[res.Repo.AccountID]
		if !ok {
			var err error
			login, err = c.AccountLogin(ctx, res.Repo.AccountID)
			if err != nil {
				return nil, err
			}
			logins[res.Repo.AccountID] = login
		}
		res.Repo.Owner = login
		out = append(out, *res.Repo)
	}
	return out, nil
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

// readNodes runs one NodeIndex read, query being the document with its
// selection left to fill in. It asks for desiredVersion too; a gateway
// that does not know it is a platform older than the field, and from
// then on the client asks for nodeFields alone (both generated from
// fields/NodeIndex.txt). It reports whether the field was read.
func (c *Client) readNodes(ctx context.Context, query func(fields string) string, out any) (bool, error) {
	if !c.nodesWithoutDesired.Load() {
		err := c.Query(ctx, query(nodeFields+" "+nodeOptionalFields), nil, out)
		var ge *Error
		if err == nil || !errors.As(err, &ge) || !unknownField(ge.Message, nodeOptional...) {
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
	// Owner is the node's ownership model — "person" or "enterprise"
	// (mesh-specs, goal the-node-serves-its-owners-orgs): a person's
	// node serves every org they are in, an enterprise's every org the
	// enterprise has. Sent only when set and the platform's
	// RegisterNodeInput takes it (TakesOwner), under the field its
	// schema has (OwnerField: `ownerKind`, or `owner` on a platform
	// before registernode-answers-with-the-node-it-made), so an older
	// platform's registerNode still accepts today's per-org call.
	//
	// The owner contract requires OwnerID to be the owner's id: the
	// person's for a person, the enterprise's for an enterprise (the
	// platform checks adminOnEnterprise(ownerId)). The node resolves an
	// enterprise's id from node.yaml's `enterprise:` (MyEnterprises).
	Owner string
	// Kind is the node's own kind — "personal" for `mesh mcp`'s node
	// (ADR-015's owner form, ADR-023 mesh mcp): sent only when set and
	// the platform's RegisterNodeInput takes it (TakesNodeKind), under
	// NodeKindField, so an older platform's registerNode still accepts
	// the call unchanged.
	Kind string
}

// NodeKindField is the name RegisterNodeInput gives the node's own
// kind.
const NodeKindField = "kind"

// NodeAnswer is the platform's answer to a registration or a heartbeat:
// the node's single id, its owner and the scopes (orgs) it now serves
// (runsheet goal a-node-has-an-owner). RegisterNode and HeartbeatNode
// return nil, not an error, when the payload carries none of
// nodeId/owner/scopes — an older platform, still answering `status`
// alone.
type NodeAnswer struct {
	NodeID string        `json:"nodeId"`
	Owner  NodeOwner     `json:"owner"`
	Scopes []ScopeAnswer `json:"scopes"`
}

// NodeOwner is the NodeAnswer's owner: a person or an enterprise, its
// id and its display name.
type NodeOwner struct {
	Kind string `json:"kind"` // "person" or "enterprise"
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ScopeAnswer is one org the NodeAnswer's node now serves: the org's
// id, its GitHub login (empty for an org without one, named by Slug
// instead), its tenant slug, the node's account id in it, and Shares —
// the enterprise's per-org weight (0 when the platform sent none,
// equal shares; goal every-org-gets-its-share-of-a-node), sent only
// when the gateway's scopes element type has it (takesScopeShares).
type ScopeAnswer struct {
	OrgID     string `json:"orgId"`
	Login     string `json:"login"`
	Slug      string `json:"slug"`
	AccountID string `json:"accountId"`
	Shares    int    `json:"shares"`
}

// nodeAnswerFields is what RegisterNode and HeartbeatNode ask their
// mutation's payload for beyond status: the platform's answer to who
// owns the node and which orgs it now serves. `scopes { shares }` is
// asked for only when takesScopeShares finds the payload's scopes
// element type has it — see nodeAnswerFieldsFor.
const nodeAnswerFields = "nodeId owner { kind id name } scopes { orgId login slug accountId }"

// nodeAnswerFieldsWithShares is nodeAnswerFields with `shares` added to
// the scopes selection, asked for only once takesScopeShares finds the
// platform's payload takes it: an older platform refuses the whole
// selection on an unknown field ("Cannot query field"), so the node
// must not ask for it blind.
const nodeAnswerFieldsWithShares = "nodeId owner { kind id name } scopes { orgId login slug accountId shares }"

// nodeAnswerFieldsFor is nodeAnswerFields, or
// nodeAnswerFieldsWithShares when the gateway's scopes element type
// takes `shares` (takesScopeShares).
func (c *Client) nodeAnswerFieldsFor(ctx context.Context) string {
	if c.takesScopeShares(ctx) {
		return nodeAnswerFieldsWithShares
	}
	return nodeAnswerFields
}

// nodeAnswerPayload is nodeAnswerFields' shape, decoded from the
// mutation's payload.
type nodeAnswerPayload struct {
	NodeID string        `json:"nodeId"`
	Owner  *NodeOwner    `json:"owner"`
	Scopes []ScopeAnswer `json:"scopes"`
}

// asAnswer is nil when the payload named none of nodeId, owner or
// scopes (an older platform, answering `status` alone).
func (p nodeAnswerPayload) asAnswer() *NodeAnswer {
	if p.NodeID == "" && p.Owner == nil && len(p.Scopes) == 0 {
		return nil
	}
	a := &NodeAnswer{NodeID: p.NodeID, Scopes: p.Scopes}
	if a.Scopes == nil {
		a.Scopes = []ScopeAnswer{}
	}
	if p.Owner != nil {
		a.Owner = *p.Owner
	}
	return a
}

// mutateForAnswer sends field's command with input, asking its payload
// for nodeAnswerFields beside status, and returns the platform's
// NodeAnswer. A platform whose payload type does not have those fields
// yet refuses the whole selection ("Cannot query field"); that is read
// as no answer, not an error — the command still landed with `status`
// alone would have.
//
// The refusal is kept for NodeAnswerRetry: until then the command asks
// `status` alone, so a platform before a-node-has-an-owner costs one
// request per heartbeat, not two, and is asked again after a deploy.
func (c *Client) mutateForAnswer(ctx context.Context, field, aggregateID string, input map[string]any) (*NodeAnswer, error) {
	if time.Now().UnixNano() < c.nodeAnswerUntil.Load() {
		return nil, c.Mutate(ctx, field, aggregateID, input)
	}
	var payload nodeAnswerPayload
	err := c.MutateFields(ctx, field, aggregateID, input, "status "+c.nodeAnswerFieldsFor(ctx), &payload)
	if err == nil {
		return payload.asAnswer(), nil
	}
	var ge *Error
	if errors.As(err, &ge) && unknownField(ge.Message, "nodeId", "owner", "scopes") {
		c.nodeAnswerUntil.Store(time.Now().Add(NodeAnswerRetry).UnixNano())
		if err2 := c.Mutate(ctx, field, aggregateID, input); err2 != nil {
			return nil, err2
		}
		return nil, nil
	}
	return nil, err
}

// NodeAnswerRetry is how long a gateway's refusal of the node's answer
// (nodeId, owner, scopes on the registerNode/heartbeatNode payload) is
// kept before the answer is asked for again.
const NodeAnswerRetry = 10 * time.Minute

// RegisterNode registers (converging) the node, and returns the
// platform's answer to who owns it and which orgs it now serves (nil
// on a platform that does not answer that yet).
func (c *Client) RegisterNode(ctx context.Context, in RegisterNodeInput) (*NodeAnswer, error) {
	harnesses := in.Harnesses
	if harnesses == nil {
		harnesses = []string{}
	}
	input := map[string]any{
		"orgId": in.OrgID, "ownerId": in.OwnerID, "name": in.Name, "slug": in.Slug,
		"harnesses": harnesses, "capacity": in.Capacity, "version": in.Version,
	}
	if in.Owner != "" {
		if field := c.OwnerField(ctx); field != "" {
			input[field] = in.Owner
		}
	}
	if in.Kind != "" && c.TakesNodeKind(ctx) {
		input[NodeKindField] = in.Kind
	}
	return c.mutateForAnswer(ctx, "registerNode", in.NodeID, input)
}

// TakesNodeKind says whether the gateway's RegisterNodeInput has a
// `kind` field (NodeKindField) — asked by introspection and kept for
// ScalingProbeEvery, the same as TakesOwner: an older platform's
// registerNode would refuse the whole call on an input field it does
// not know.
func (c *Client) TakesNodeKind(ctx context.Context) bool {
	return c.takesInputField(ctx, &c.nodeKindSchema, "RegisterNodeInput", NodeKindField)
}

// ownerProbe is the cached answer to which owner field
// RegisterNodeInput has, and when it was asked.
type ownerProbe struct {
	mu    sync.Mutex
	at    time.Time
	field string
}

// TakesOwner says whether the gateway's RegisterNodeInput has an owner
// field under either spelling (OwnerField).
func (c *Client) TakesOwner(ctx context.Context) bool {
	return c.OwnerField(ctx) != ""
}

// OwnerField is the name the gateway's RegisterNodeInput gives the
// node's owner kind: "ownerKind" (the platform's, runsheet goal
// registernode-answers-with-the-node-it-made), "owner" (a platform
// before it), or "" for a platform with neither — asked by
// introspection and kept for ScalingProbeEvery, the same as
// takesScaling: an older platform's registerNode would refuse the
// whole call on an input field it does not know. A failed probe is
// "", kept as long. Where both are present ownerKind wins.
func (c *Client) OwnerField(ctx context.Context) string {
	p := &c.ownerSchema
	p.mu.Lock()
	defer p.mu.Unlock()
	now := c.now()
	if !p.at.IsZero() && now.Sub(p.at) < ScalingProbeEvery {
		return p.field
	}
	var out struct {
		Type *struct {
			InputFields []struct {
				Name string `json:"name"`
			} `json:"inputFields"`
		} `json:"__type"`
	}
	p.at, p.field = now, ""
	q := `{ __type(name: "RegisterNodeInput") { inputFields { name } } }`
	if err := c.Query(ctx, q, nil, &out); err != nil || out.Type == nil {
		return ""
	}
	for _, f := range out.Type.InputFields {
		switch {
		case f.Name == "ownerKind":
			p.field = "ownerKind"
		case f.Name == "owner" && p.field == "":
			p.field = "owner"
		}
	}
	return p.field
}

// RetireNode retires a node row (retireNode): the platform marks it
// retired and stops listing it as live. The node retires its own
// legacy per-org rows with it once it registers on the owner path.
func (c *Client) RetireNode(ctx context.Context, nodeID string) error {
	return c.Mutate(ctx, "retireNode", nodeID, map[string]any{})
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
	// Scaling is the node's capacity in force, its range and why, and
	// each account's slots (adr-004, adr-005-capacity-across-accounts);
	// a fixed node sends its capacity as min and max, reason `fixed`, on
	// one account too (mesh-cli ADR-018), so a capacity changed in
	// node.yaml reaches the platform on the next heartbeat. It is sent
	// only when the gateway's schema takes it (HeartbeatNode).
	Scaling *Scaling
	// Runtimes are the local tool runtimes the node can start (npx,
	// uvx, docker) and Adapters the agent CLIs it has (claude-code,
	// codex), as probed this tick (trd §1 Node, §3 Tools; runsheet goal
	// tools-in-the-console).
	Runtimes []string
	Adapters []string
	// Sandbox is whether the node's sessions run in their own pods, on
	// which runtime and image (trd §1 Node, ADR 007); nil sends none.
	Sandbox *SandboxInfo
	// Status is the node's liveness when it is not the ordinary one:
	// harness_unavailable — alive, but its harness cannot run a session
	// (out of credit, rate-limited; trd §1 Node, §3), with why in Reason,
	// which the platform turns into one needs_person row. Empty is the
	// ordinary heartbeat (online) and sends no key.
	Status string
	// Reason is a condition of the node's while it stays online: a
	// session its sandbox's limits killed, naming the limit and the item
	// (trd §1 Node, `sandbox`); or why the node is Status.
	Reason string
	// Adopted are the sessions a restarted node took over from the process
	// before it rather than starting them (mesh-specs ADR 008, mesh-cli
	// ADR 009); none sends no key.
	Adopted []AdoptedSession
	// Holding is the node's holding decision as data — why ready work
	// waits, not just a log line (runsheet goal
	// node-says-why-work-waits): each account's status, reset time and
	// what the node still takes on it, and the summary judgement. nil
	// sends none; set, it is sent only when the gateway's schema takes it
	// (HeartbeatNode, takesHolding).
	Holding *Holding
	// Commit is the short git sha the node was built from, beside its
	// version (the release tag, or that sha for a dev build; mesh-specs
	// ADR 013, trd §1 Node). It is sent only when set and the gateway's
	// schema takes it (HeartbeatNode).
	Commit string
}

// Holding is the heartbeat's `holding`: the node's account-by-account
// pacing decision and why ready work is left unclaimed
// (docs/adr/adr-005-accounts-on-the-node.md, Amendment — 2026-09-28).
type Holding struct {
	// Mode is the node's own aggregate pacing: "normal", "paced" or
	// "stopped" (paceMode.String()).
	Mode string `json:"mode"`
	// Summary is the one-line decision the console shows in place of the
	// node's log line.
	Summary string `json:"summary"`
	// Until is the earliest reset among the accounts that hold, empty
	// when none do.
	Until time.Time `json:"until,omitzero"`
	// Accounts is always a list, [] when there are none (the scaling
	// precedent, PR #255: the platform's nested input is non-null).
	Accounts []HoldingAccount `json:"accounts"`
	// Unclaimed is always a list, [] when nothing was passed over this
	// pass.
	Unclaimed []HoldingUnclaimed `json:"unclaimed"`
	// Orgs is each served org's share of the node (mesh-cli ADR-023,
	// Reporting): its floor, the sessions it holds and its ready items.
	// Always a list, [] when there are none; sent only when the
	// gateway's holding input takes `orgs` (takesHoldingOrgs).
	Orgs []HoldingOrg `json:"orgs"`
}

// HoldingOrg is one served org's row of the holding report: Org its
// Name() (login, else slug), OrgID its id, Floor the slots Floors gives
// it, Held the builds, plans and research it holds here (heldByOrg) and
// Ready the ready items its last listing returned — -1 when that read
// failed, so the floor and held still show (ADR-023, Reporting).
type HoldingOrg struct {
	Org   string `json:"org"`
	OrgID string `json:"orgId"`
	Floor int    `json:"floor"`
	Held  int    `json:"held"`
	Ready int    `json:"ready"`
}

// HoldingAccount is one account's standing in the holding report: its
// kind, its status by the node's own pacing (ok, paced, exhausted or
// unknown — never seen a reading, so ChooseAccount treats it as full
// headroom), when its holding window resets, and what the node still
// takes on it.
type HoldingAccount struct {
	Account  string       `json:"account"`
	Kind     string       `json:"kind"` // "subscription" or "api_key"
	Status   string       `json:"status"`
	ResetsAt time.Time    `json:"resetsAt,omitzero"`
	Takes    HoldingTakes `json:"takes"`
}

// HoldingTakes is what the node still takes on an account: the
// priorities it claims for, the most items it runs on it at once, and
// whether it still runs research there (research is not work that
// waits: internal/node/select.go).
type HoldingTakes struct {
	Priorities []string `json:"priorities"`
	MaxItems   int      `json:"maxItems"`
	Research   bool     `json:"research"`
}

// HoldingUnclaimed is one goal's ready work the node passed over this
// pass for lack of account headroom.
type HoldingUnclaimed struct {
	GoalID string `json:"goalId"`
	Kind   string `json:"kind"` // "item" or "plan"
	Count  int    `json:"count"`
	Reason string `json:"reason"`
	// Until is when this hold lifts, zero for one with none (adr-005's
	// amendment 2026-10-03, goal no-headroom-is-a-hold-not-a-claim-loop,
	// item 4). The reason text already names it (ErrNoHeadroom.Error);
	// this is the structured copy, sent only when the gateway's
	// HeartbeatNodeInput's `holding.unclaimed` element type has it
	// (takesHoldingUnclaimedUntil).
	Until time.Time `json:"until,omitzero"`
}

// AdoptedSession is one entry of the heartbeat's `adopted`: the item the
// session is for (a plan's goal, a research session's direction), its
// kind (build, evaluate, plan, research, rebase) and when the session
// started. GoalID and DirectionID name a plan's or a research session's
// own subject beside ItemID, which keeps the goal or direction id as it
// always has, for an older platform; SessionID is the agent's own
// session id once state.json carries it, and Alive is whether the
// session's unit still runs, both re-read on every heartbeat so they
// stay current while the session runs (mesh-cli ADR-009 amendment,
// Loop.Adopted). The four are sent only when the gateway's
// HeartbeatNodeInput's `adopted` element type takes them
// (takesAdoptedFields); an older platform gets itemId, kind and since
// alone.
type AdoptedSession struct {
	ItemID      string    `json:"itemId"`
	Kind        string    `json:"kind"`
	Since       time.Time `json:"since"`
	GoalID      string    `json:"goalId,omitempty"`
	DirectionID string    `json:"directionId,omitempty"`
	SessionID   string    `json:"sessionId,omitempty"`
	Alive       bool      `json:"alive"`
}

// SandboxInfo is the heartbeat's `sandbox`: the mode (on or off), the
// container runtime and its version, and the node image.
type SandboxInfo struct {
	Mode    string `json:"mode"`
	Runtime string `json:"runtime,omitempty"`
	Image   string `json:"image,omitempty"`
}

// Scaling is what a node reports of its capacity: the capacity in
// force, node.yaml's min and max (both the capacity for a fixed node,
// reason `fixed`, mesh-cli ADR-018), the reason (adr-004,
// adr-005-capacity-across-accounts),
// and each account's slots as the accounts scaler last counted them —
// none before it has, or on a platform whose `scaling` has no
// `accounts`. Host is the machine's own reading (load, cores,
// temperature, whether it is judged hot) that backed the capacity off,
// nil when the node has none or the platform's `scaling` has no
// `host` (adr-004's amendment 2026-10-03).
type Scaling struct {
	Capacity int    `json:"capacity"`
	Min      int    `json:"min"`
	Max      int    `json:"max"`
	Reason   string `json:"reason"`
	// Accounts is always a list on the wire, [] when there are none: the
	// platform's nested input makes it non-null whatever its schema says
	// (go-apis/loom nested-optional-lists-are-nullable), and an omitted or
	// null list had the whole heartbeat refused (2026-09-28).
	Accounts []AccountScaling `json:"accounts"`
	Host     *HostScaling     `json:"host,omitempty"`
}

// HostScaling is the machine's own reading that the scaler weighed
// against node.yaml's `host:` thresholds: the 1-minute load average,
// the core count it is a ratio of, the package temperature in Celsius
// (omitted where the host has no thermal zone to read), and whether
// either crossed its threshold (adr-004's amendment 2026-10-03).
type HostScaling struct {
	Load         float64 `json:"load"`
	Cores        int     `json:"cores"`
	TemperatureC float64 `json:"temperatureC,omitempty"`
	Hot          bool    `json:"hot"`
}

// AccountScaling is one account's share of the capacity: its label and
// kind (subscription or api), its slots for any work, the slots only
// urgent and high work may take, and what bound them. Names only —
// never a token, a key or the variable or file holding one.
type AccountScaling struct {
	Name            string `json:"name"`
	Kind            string `json:"kind"`
	Slots           int    `json:"slots"`
	UrgentOnlySlots int    `json:"urgentOnlySlots"`
	Reason          string `json:"reason"`
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
	// Priorities are the goal priorities the account serves (the known
	// issued account's AccountConfig.Priorities), so the Nodes page's
	// account gauges can show what each account is for; empty (an
	// account serving all, or one the loop has not learned the platform
	// issued) sends no key, so an older platform is unchanged. Sent only
	// when the gateway's NodeAccountLimitsInput has it (HeartbeatNode,
	// takesLimitsPriorities; adr-005's amendment 2026-10,
	// `limits[].priorities`).
	Priorities []string `json:"priorities,omitempty"`
}

// LimitWindow is one of the plan's windows: how much of it is used, as
// the session reported it, and when it starts over.
type LimitWindow struct {
	Utilization float64   `json:"utilization"`
	ResetsAt    time.Time `json:"resetsAt,omitzero"`
}

// HeartbeatNode reports liveness and the running session count, and the
// extras that are set (at most one struct is read); it returns the
// platform's answer to who owns the node and which orgs it now serves
// (nil on a platform that does not answer that yet).
func (c *Client) HeartbeatNode(ctx context.Context, nodeID, version string, running int, extras ...HeartbeatExtras) (*NodeAnswer, error) {
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
			if c.takesLimitsPriorities(ctx) {
				in["limits"] = x.Limits
			} else {
				in["limits"] = limitsWithoutPriorities(x.Limits)
			}
		}
		if x.Scaling != nil {
			switch takes, accounts, host := c.takesScaling(ctx); {
			case !takes:
			case !accounts:
				// a platform whose scaling has no accounts field refuses the
				// key: the four fields only
				in["scaling"] = map[string]any{"capacity": x.Scaling.Capacity, "min": x.Scaling.Min, "max": x.Scaling.Max, "reason": x.Scaling.Reason}
			default:
				// accounts is [] rather than null when the node names none:
				// the platform's nested input is non-null
				sc := *x.Scaling
				if sc.Accounts == nil {
					sc.Accounts = []AccountScaling{}
				}
				if !host {
					sc.Host = nil
				}
				in["scaling"] = &sc
			}
		}
		if len(x.Runtimes) > 0 {
			in["runtimes"] = x.Runtimes
		}
		if len(x.Adapters) > 0 {
			in["adapters"] = x.Adapters
		}
		if x.Sandbox != nil {
			in["sandbox"] = x.Sandbox
		}
		if x.Status != "" {
			in["status"] = x.Status
		}
		if x.Reason != "" {
			in["reason"] = x.Reason
		}
		if len(x.Adopted) > 0 {
			if c.takesAdoptedFields(ctx) {
				in["adopted"] = x.Adopted
			} else {
				in["adopted"] = adoptedWithoutNewFields(x.Adopted)
			}
		}
		if x.Holding != nil && c.takesHolding(ctx) {
			h := *x.Holding
			if h.Accounts == nil {
				h.Accounts = []HoldingAccount{}
			}
			if h.Unclaimed == nil {
				h.Unclaimed = []HoldingUnclaimed{}
			}
			if h.Orgs == nil {
				h.Orgs = []HoldingOrg{}
			}
			until, orgs := c.takesHoldingUnclaimedUntil(ctx), c.takesHoldingOrgs(ctx)
			if until && orgs {
				in["holding"] = &h
			} else {
				in["holding"] = holdingWithout(h, until, orgs)
			}
		}
		if x.Commit != "" && c.takesCommit(ctx) {
			in["commit"] = x.Commit
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
	answer, err := c.mutateForAnswer(ctx, "heartbeatNode", nodeID, in)
	if err != nil && len(in) > 2 && strings.Contains(err.Error(), "got invalid value") {
		c.heartbeatBasicUntil.Store(time.Now().Add(10 * time.Minute).UnixNano())
		return c.mutateForAnswer(ctx, "heartbeatNode", nodeID, basicHeartbeat(in))
	}
	return answer, err
}

// ScalingProbeEvery is how long the gateway's answer to whether its
// heartbeat takes `scaling` is kept before it is asked again: a
// platform deploy adds the field without the node restarting.
const ScalingProbeEvery = time.Hour

// commitProbe is the cached answer to whether heartbeatNode's input has
// `commit`, and when it was asked.
type commitProbe struct {
	mu    sync.Mutex
	at    time.Time
	takes bool
}

// takesCommit says whether the gateway's HeartbeatNodeInput has a
// `commit` field — asked by introspection and kept for ScalingProbeEvery,
// as takesScaling is: a platform older than the field would refuse the
// whole heartbeat. A failed probe is a no, kept as long.
func (c *Client) takesCommit(ctx context.Context) bool {
	p := &c.commitSchema
	p.mu.Lock()
	defer p.mu.Unlock()
	now := c.now()
	if !p.at.IsZero() && now.Sub(p.at) < ScalingProbeEvery {
		return p.takes
	}
	var out struct {
		Type *struct {
			InputFields []struct {
				Name string `json:"name"`
			} `json:"inputFields"`
		} `json:"__type"`
	}
	p.at, p.takes = now, false
	q := `{ __type(name: "HeartbeatNodeInput") { inputFields { name } } }`
	if err := c.Query(ctx, q, nil, &out); err != nil || out.Type == nil {
		return false
	}
	for _, f := range out.Type.InputFields {
		if f.Name == "commit" {
			p.takes = true
		}
	}
	return p.takes
}

// scalingProbe is the cached answer: whether heartbeatNode's input has
// `scaling`, whether that has `accounts`, whether it has `host`
// (adr-004's amendment 2026-10-03), and when it was asked.
type scalingProbe struct {
	mu       sync.Mutex
	at       time.Time
	takes    bool
	accounts bool
	host     bool
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// takesScaling says whether the gateway's HeartbeatNodeInput has a
// `scaling` field, whether that takes `accounts`, and whether it takes
// `host` (adr-004's amendment 2026-10-03) — asked once by
// introspection and kept for ScalingProbeEvery (ADR-004: the node must
// not send a field the platform does not accept). A failed probe is a
// no, kept as long: the heartbeat goes on without the field. A scalar
// `scaling` (a Map or JSON) takes any shape.
func (c *Client) takesScaling(ctx context.Context) (bool, bool, bool) {
	p := &c.scalingSchema
	p.mu.Lock()
	defer p.mu.Unlock()
	now := c.now()
	if !p.at.IsZero() && now.Sub(p.at) < ScalingProbeEvery {
		return p.takes, p.accounts, p.host
	}
	type typeRef struct {
		Kind        string `json:"kind"`
		InputFields []struct {
			Name string `json:"name"`
		} `json:"inputFields"`
		OfType *struct {
			Kind        string `json:"kind"`
			InputFields []struct {
				Name string `json:"name"`
			} `json:"inputFields"`
		} `json:"ofType"`
	}
	var out struct {
		Type *struct {
			InputFields []struct {
				Name string  `json:"name"`
				Type typeRef `json:"type"`
			} `json:"inputFields"`
		} `json:"__type"`
	}
	p.at, p.takes, p.accounts, p.host = now, false, false, false
	q := `{ __type(name: "HeartbeatNodeInput") { inputFields { name type { kind inputFields { name } ofType { kind inputFields { name } } } } } }`
	if err := c.Query(ctx, q, nil, &out); err != nil || out.Type == nil {
		return false, false, false
	}
	for _, f := range out.Type.InputFields {
		if f.Name != "scaling" {
			continue
		}
		p.takes = true
		kind, fields := f.Type.Kind, f.Type.InputFields
		if kind == "NON_NULL" && f.Type.OfType != nil {
			kind, fields = f.Type.OfType.Kind, f.Type.OfType.InputFields
		}
		if kind == "SCALAR" {
			p.accounts, p.host = true, true
		}
		for _, sf := range fields {
			if sf.Name == "accounts" {
				p.accounts = true
			}
			if sf.Name == "host" {
				p.host = true
			}
		}
	}
	return p.takes, p.accounts, p.host
}

// holdingProbe is the cached answer to whether an input type has a
// field — HeartbeatNodeInput `holding`, EvaluateWorkItemInput `outcome`
// (takesInputField) — and when it was asked.
type holdingProbe struct {
	mu    sync.Mutex
	at    time.Time
	takes bool
}

// takesHolding says whether the gateway's HeartbeatNodeInput has a
// `holding` field — asked once by introspection and kept for
// ScalingProbeEvery, the same as takesScaling (ADR-004: the node must
// not send a field the platform does not accept). A failed probe is a
// no, kept as long.
func (c *Client) takesHolding(ctx context.Context) bool {
	p := &c.holdingSchema
	p.mu.Lock()
	defer p.mu.Unlock()
	now := c.now()
	if !p.at.IsZero() && now.Sub(p.at) < ScalingProbeEvery {
		return p.takes
	}
	var out struct {
		Type *struct {
			InputFields []struct {
				Name string `json:"name"`
			} `json:"inputFields"`
		} `json:"__type"`
	}
	p.at, p.takes = now, false
	q := `{ __type(name: "HeartbeatNodeInput") { inputFields { name } } }`
	if err := c.Query(ctx, q, nil, &out); err != nil || out.Type == nil {
		return false
	}
	for _, f := range out.Type.InputFields {
		if f.Name == "holding" {
			p.takes = true
		}
	}
	return p.takes
}

// holdingWithout is h's wire shape less what the platform does not take
// yet: each unclaimed entry's `until` unless until (see
// takesHoldingUnclaimedUntil), and `orgs` unless orgs (see
// takesHoldingOrgs). An unknown field refuses the whole heartbeat (see
// HeartbeatNode).
func holdingWithout(h Holding, until, orgs bool) map[string]any {
	b, _ := json.Marshal(h)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	if list, ok := m["unclaimed"].([]any); ok && !until {
		for _, e := range list {
			if em, ok := e.(map[string]any); ok {
				delete(em, "until")
			}
		}
	}
	if !orgs {
		delete(m, "orgs")
	}
	return m
}

// takesHoldingOrgs says whether the gateway's HeartbeatNodeInput's
// `holding` field takes `orgs` — asked once by introspection and kept
// for ScalingProbeEvery, the same as takesHoldingUnclaimedUntil
// (mesh-cli ADR-023, Reporting).
func (c *Client) takesHoldingOrgs(ctx context.Context) bool {
	p := &c.holdingOrgsSchema
	p.mu.Lock()
	defer p.mu.Unlock()
	now := c.now()
	if !p.at.IsZero() && now.Sub(p.at) < ScalingProbeEvery {
		return p.takes
	}
	var out struct {
		Type *struct {
			InputFields []struct {
				Name string `json:"name"`
				Type struct {
					InputFields []struct {
						Name string `json:"name"`
					} `json:"inputFields"`
					OfType *struct {
						InputFields []struct {
							Name string `json:"name"`
						} `json:"inputFields"`
					} `json:"ofType"`
				} `json:"type"`
			} `json:"inputFields"`
		} `json:"__type"`
	}
	p.at, p.takes = now, false
	q := `{ __type(name: "HeartbeatNodeInput") { inputFields { name type { inputFields { name } ofType { inputFields { name } } } } } }`
	if err := c.Query(ctx, q, nil, &out); err != nil || out.Type == nil {
		return false
	}
	for _, f := range out.Type.InputFields {
		if f.Name != "holding" {
			continue
		}
		fields := f.Type.InputFields
		if f.Type.OfType != nil {
			fields = append(fields, f.Type.OfType.InputFields...)
		}
		for _, hf := range fields {
			if hf.Name == "orgs" {
				p.takes = true
			}
		}
	}
	return p.takes
}

// takesHoldingUnclaimedUntil says whether the gateway's
// HeartbeatNodeInput's `holding` field's `unclaimed` list carries a
// `until` field on its element type — asked once by introspection and
// kept for ScalingProbeEvery, the same as takesLimitsPriorities
// (ADR-004: the node must not send a field the platform does not
// accept; adr-005's amendment 2026-10-03,
// `holding.unclaimed[].until`).
func (c *Client) takesHoldingUnclaimedUntil(ctx context.Context) bool {
	p := &c.holdingUnclaimedUntilSchema
	p.mu.Lock()
	defer p.mu.Unlock()
	now := c.now()
	if !p.at.IsZero() && now.Sub(p.at) < ScalingProbeEvery {
		return p.takes
	}
	type chainType struct {
		Kind        string `json:"kind"`
		InputFields []struct {
			Name string `json:"name"`
		} `json:"inputFields"`
		OfType *chainType `json:"ofType"`
	}
	type nestedField struct {
		Name string    `json:"name"`
		Type chainType `json:"type"`
	}
	var out struct {
		Type *struct {
			InputFields []struct {
				Name string `json:"name"`
				Type struct {
					InputFields []nestedField `json:"inputFields"`
				} `json:"type"`
			} `json:"inputFields"`
		} `json:"__type"`
	}
	p.at, p.takes = now, false
	q := `{ __type(name: "HeartbeatNodeInput") { inputFields { name type { inputFields { name type { kind inputFields { name } ofType { kind inputFields { name } ofType { kind inputFields { name } } } } } } } } }`
	if err := c.Query(ctx, q, nil, &out); err != nil || out.Type == nil {
		return false
	}
	for _, f := range out.Type.InputFields {
		if f.Name != "holding" {
			continue
		}
		for _, hf := range f.Type.InputFields {
			if hf.Name != "unclaimed" {
				continue
			}
			for t := &hf.Type; t != nil; t = t.OfType {
				for _, sf := range t.InputFields {
					if sf.Name == "until" {
						p.takes = true
					}
				}
			}
		}
	}
	return p.takes
}

// takesScopeShares says whether the gateway's registerNode/heartbeatNode
// payload's `scopes` field's element type carries a `shares` field —
// asked once by introspection on DispatchResult, the shared mutation
// payload type every dispatch-style mutation (registerNode and
// heartbeatNode among them) returns its hand-selected fields on, and
// kept for ScalingProbeEvery, the same as takesHoldingUnclaimedUntil
// (goal every-org-gets-its-share-of-a-node, item 3: the enterprise's
// per-org weight). A failed probe is a no, kept as long — an older
// platform's scopes carry no `shares` and nodeAnswerFieldsFor must not
// ask for it blind, since an unknown field refuses the whole selection.
func (c *Client) takesScopeShares(ctx context.Context) bool {
	p := &c.scopeSharesSchema
	p.mu.Lock()
	defer p.mu.Unlock()
	now := c.now()
	if !p.at.IsZero() && now.Sub(p.at) < ScalingProbeEvery {
		return p.takes
	}
	type elemType struct {
		Kind   string `json:"kind"`
		Fields []struct {
			Name string `json:"name"`
		} `json:"fields"`
		OfType *elemType `json:"ofType"`
	}
	var out struct {
		Type *struct {
			Fields []struct {
				Name string   `json:"name"`
				Type elemType `json:"type"`
			} `json:"fields"`
		} `json:"__type"`
	}
	p.at, p.takes = now, false
	q := `{ __type(name: "DispatchResult") { fields { name type { kind fields { name } ofType { kind fields { name } ofType { kind fields { name } } } } } } }`
	if err := c.Query(ctx, q, nil, &out); err != nil || out.Type == nil {
		return false
	}
	for _, f := range out.Type.Fields {
		if f.Name != "scopes" {
			continue
		}
		for t := &f.Type; t != nil; t = t.OfType {
			for _, sf := range t.Fields {
				if sf.Name == "shares" {
					p.takes = true
				}
			}
		}
	}
	return p.takes
}

// limitsWithoutPriorities is limits with its `priorities` dropped from
// the wire shape, for a platform whose NodeAccountLimitsInput does not
// take it yet: an unknown field refuses the whole heartbeat (see
// HeartbeatNode).
func limitsWithoutPriorities(limits []Limits) []map[string]any {
	out := make([]map[string]any, len(limits))
	for i, l := range limits {
		b, _ := json.Marshal(l)
		m := map[string]any{}
		_ = json.Unmarshal(b, &m)
		delete(m, "priorities")
		out[i] = m
	}
	return out
}

// takesLimitsPriorities says whether the gateway's HeartbeatNodeInput's
// `limits` list carries a `priorities` field on its element type —
// asked once by introspection and kept for ScalingProbeEvery, the same
// as takesScaling (ADR-004: the node must not send a field the platform
// does not accept). A failed probe is a no, kept as long
// (adr-005's amendment 2026-10, `limits[].priorities`).
func (c *Client) takesLimitsPriorities(ctx context.Context) bool {
	p := &c.limitsPrioritiesSchema
	p.mu.Lock()
	defer p.mu.Unlock()
	now := c.now()
	if !p.at.IsZero() && now.Sub(p.at) < ScalingProbeEvery {
		return p.takes
	}
	type typeRef struct {
		Kind        string `json:"kind"`
		InputFields []struct {
			Name string `json:"name"`
		} `json:"inputFields"`
		OfType *typeRef `json:"ofType"`
	}
	var out struct {
		Type *struct {
			InputFields []struct {
				Name string  `json:"name"`
				Type typeRef `json:"type"`
			} `json:"inputFields"`
		} `json:"__type"`
	}
	p.at, p.takes = now, false
	q := `{ __type(name: "HeartbeatNodeInput") { inputFields { name type { kind inputFields { name } ofType { kind inputFields { name } ofType { kind inputFields { name } } } } } } }`
	if err := c.Query(ctx, q, nil, &out); err != nil || out.Type == nil {
		return false
	}
	for _, f := range out.Type.InputFields {
		if f.Name != "limits" {
			continue
		}
		for t := &f.Type; t != nil; t = t.OfType {
			for _, sf := range t.InputFields {
				if sf.Name == "priorities" {
					p.takes = true
				}
			}
		}
	}
	return p.takes
}

// adoptedWithoutNewFields is adopted with goalId, directionId,
// sessionId and alive dropped from the wire shape, for a platform
// whose `adopted` element type does not take them yet (see
// takesAdoptedFields): an unknown field refuses the whole heartbeat
// (see HeartbeatNode).
func adoptedWithoutNewFields(adopted []AdoptedSession) []map[string]any {
	out := make([]map[string]any, len(adopted))
	for i, a := range adopted {
		b, _ := json.Marshal(a)
		m := map[string]any{}
		_ = json.Unmarshal(b, &m)
		delete(m, "goalId")
		delete(m, "directionId")
		delete(m, "sessionId")
		delete(m, "alive")
		out[i] = m
	}
	return out
}

// takesAdoptedFields says whether the gateway's HeartbeatNodeInput's
// `adopted` list carries goalId, directionId, sessionId and alive on
// its element type — asked once by introspection and kept for
// ScalingProbeEvery, the same as takesLimitsPriorities (ADR-004: the
// node must not send a field the platform does not accept; mesh-cli
// ADR-009 amendment, the adopted entries carry a plan's or research's
// own subject, the agent's session id and whether it still runs). A
// failed probe is a no, kept as long. The four fields ship together on
// the platform, so one — `alive`, present on every entry — stands for
// the rest.
func (c *Client) takesAdoptedFields(ctx context.Context) bool {
	p := &c.adoptedFieldsSchema
	p.mu.Lock()
	defer p.mu.Unlock()
	now := c.now()
	if !p.at.IsZero() && now.Sub(p.at) < ScalingProbeEvery {
		return p.takes
	}
	type typeRef struct {
		Kind        string `json:"kind"`
		InputFields []struct {
			Name string `json:"name"`
		} `json:"inputFields"`
		OfType *typeRef `json:"ofType"`
	}
	var out struct {
		Type *struct {
			InputFields []struct {
				Name string  `json:"name"`
				Type typeRef `json:"type"`
			} `json:"inputFields"`
		} `json:"__type"`
	}
	p.at, p.takes = now, false
	q := `{ __type(name: "HeartbeatNodeInput") { inputFields { name type { kind inputFields { name } ofType { kind inputFields { name } ofType { kind inputFields { name } } } } } } }`
	if err := c.Query(ctx, q, nil, &out); err != nil || out.Type == nil {
		return false
	}
	for _, f := range out.Type.InputFields {
		if f.Name != "adopted" {
			continue
		}
		for t := &f.Type; t != nil; t = t.OfType {
			for _, sf := range t.InputFields {
				if sf.Name == "alive" {
					p.takes = true
				}
			}
		}
	}
	return p.takes
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
// usage is the session's usage report, carried as `usage` when non-nil
// (see UsageReport, ErrNoUsageLedger).
func (c *Client) FinishWorkItemRebase(ctx context.Context, itemID, headSha string, resolvedFiles []string, usage *UsageReport) error {
	if resolvedFiles == nil {
		resolvedFiles = []string{}
	}
	return c.mutateWithUsage(ctx, "finishWorkItemRebase", itemID, map[string]any{"headSha": headSha, "method": "session", "files": resolvedFiles}, usage)
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

// DescriptionField is FinishWorkItemInput's key for the PR body the
// platform opens the pull request with (FinishDescription, the node's
// every-actor-has-a-name item 3): who built the item, who it was paired
// with, and that the evaluator is named by the evaluation's own actor.
// Sent only when the platform's input takes it (withDescription).
const DescriptionField = "description"

// withDescription puts description's first non-empty value on in as
// DescriptionField when the platform's FinishWorkItemInput takes it —
// introspected once and kept for ScalingProbeEvery, as withActor is. A
// platform without the field gets the finish without it, never an
// error.
func (c *Client) withDescription(ctx context.Context, in map[string]any, description []string) {
	if len(description) == 0 || description[0] == "" {
		return
	}
	if c.takesInputField(ctx, &c.descriptionSchema, "FinishWorkItemInput", DescriptionField) {
		in[DescriptionField] = description[0]
	}
}

// FinishWorkItem records the pushed branch: the outcome built, which
// the platform takes as the default, so the input is the one a platform
// without outcomes knows. usage is the build session's usage report,
// carried as `usage` when non-nil (see UsageReport, ErrNoUsageLedger).
// description, given and non-empty, is the PR body (DescriptionField).
func (c *Client) FinishWorkItem(ctx context.Context, itemID, branch, headSha string, costCents int, usage *UsageReport, description ...string) error {
	in := map[string]any{"branch": branch, "headSha": headSha, "costCents": costCents}
	c.withDescription(ctx, in, description)
	return c.mutateWithUsage(ctx, "finishWorkItem", itemID, in, usage)
}

// FinishWorkItemAlreadyDone finishes the item with no branch: main
// already holds its work, and evidence — the merged PR URLs and/or
// commit SHAs on the base ref, with a line on why — is what the
// evaluator checks against the acceptance (mesh ADR 003 decision 1).
// usage and description are as FinishWorkItem's.
func (c *Client) FinishWorkItemAlreadyDone(ctx context.Context, itemID, evidence string, costCents int, usage *UsageReport, description ...string) error {
	if strings.TrimSpace(evidence) == "" {
		return errors.New("gateway: an already_done finish needs evidence")
	}
	in := map[string]any{"outcome": OutcomeAlreadyDone, "evidence": evidence, "costCents": costCents}
	c.withDescription(ctx, in, description)
	return c.mutateWithUsage(ctx, "finishWorkItem", itemID, in, usage)
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
func (c *Client) ReleaseWorkItem(ctx context.Context, itemID, nodeID, reason string) error {
	return c.Mutate(ctx, "releaseWorkItem", itemID, map[string]any{"nodeId": nodeID, "note": reason})
}

// ReleasePlan hands back a goal's plan claim this node holds, with why:
// a fetch or a plan token that failed before the session opened
// (adr-022, A fetch that fails is reported). The platform clears the
// claim, keeps the goal in planning and records the note as the goal's
// planError — on a second identical one, a Needs-you row for the org's
// admins (runsheet's half). The shape of ReleaseWorkItem; a platform
// without the mutation answers IsUnsupported.
func (c *Client) ReleasePlan(ctx context.Context, goalID, nodeID, reason string) error {
	const field = "releasePlan"
	err := c.Mutate(ctx, field, goalID, map[string]any{"nodeId": nodeID, "note": reason})
	var ge *Error
	if errors.As(err, &ge) && (unknownOperation(ge.Message, field) || schemaMismatch(ge.Message, field)) {
		return &Error{Message: ge.Message, Unsupported: true}
	}
	return err
}

// EvaluateChoice is the evaluate path's choice of evaluator for an item
// (goal one-evaluator-for-ordinary-work): EvaluatedModel is the tier the
// deciding evaluation ran on, recorded as evaluatedModel. A zero value
// sends nothing, as EvaluateWorkItem and EvaluateWorkItemNoVerdict
// always did before this goal.
type EvaluateChoice struct {
	EvaluatedModel string
}

// EvaluateWorkItem records the evaluator's verdict (phase 1's session; the
// platform readies or reviews the item) and that session's cost. usage
// is that session's usage report, carried as `usage` when non-nil (see
// UsageReport, ErrNoUsageLedger).
// clauses are the acceptance clauses a failed verdict failed on, each
// with what would satisfy it, for the retry to fix; they are sent only
// on a fail and only to a gateway whose EvaluateWorkItemInput takes the
// field (ADR-004: never a field the platform does not accept). A
// platform without it records the verdict as before, the clauses only in
// the prose of notes. choice.EvaluatedModel is sent as evaluatedModel
// when non-empty; EvaluateWorkItemInput carries the field today, so no
// probe is needed (goal one-evaluator-for-ordinary-work).
func (c *Client) EvaluateWorkItem(ctx context.Context, itemID string, passed bool, notes string, clauses []FailedClause, costCents int, usage *UsageReport, choice EvaluateChoice) error {
	in := map[string]any{"passed": passed, "notes": notes, "costCents": costCents}
	if !passed && len(clauses) > 0 && c.takesInputField(ctx, &c.clausesSchema, "EvaluateWorkItemInput", FailedClausesField) {
		in[FailedClausesField] = clauses
	}
	if choice.EvaluatedModel != "" {
		in[EvaluatedModelField] = choice.EvaluatedModel
	}
	return c.mutateWithUsage(ctx, "evaluateWorkItem", itemID, in, usage)
}

// FailedClausesField is evaluateWorkItem's input field for the clauses a
// failed evaluation failed on.
const FailedClausesField = "failedClauses"

// EvaluatedModelField is evaluateWorkItem's input field for the tier the
// deciding evaluation ran on (goal one-evaluator-for-ordinary-work).
const EvaluatedModelField = "evaluatedModel"

// FailedClause is one acceptance clause a failed evaluation failed on,
// with what would satisfy it. The platform refuses a clause missing
// either half, so a caller sends only complete ones
// (harness.ParseFailedClauses drops the rest).
type FailedClause struct {
	Clause  string `json:"clause"`
	Satisfy string `json:"satisfy"`
}

// PriorAttempt is one evaluation already recorded against an item: the
// attempt it judged, its review and the clauses it failed.
type PriorAttempt struct {
	Attempt int            `json:"attempt"`
	Notes   string         `json:"notes"`
	Clauses []FailedClause `json:"failedClauses"`
}

// WorkItemPriorAttempts reads the evaluations already recorded against an
// item, oldest attempt first, so a retry's evaluator is judged on the
// clauses earlier attempts named rather than on a fresh reading. A
// platform that does not serve the records answers an error, which the
// caller treats as none: the evaluation runs without them.
//
// It reads only this item's own attempts. A narrow re-plan's goal carries
// `replacingItemId`/`replacementItemIds` (schema.json's GoalIndex), but
// WorkItemIndex itself has no `replaces`/`replacedBy`/`replacingItemId`
// back-link from a replacement item to what it replaced, so there is no
// chain this read can follow from one item's id alone. A lineage that
// spans a replaced item's own earlier evaluations — oldest item first,
// each attempt labelled with its item's seq — is owed to the platform:
// it needs that link on WorkItemIndex (or an index read keyed on the
// goal) before this client can walk it.
func (c *Client) WorkItemPriorAttempts(ctx context.Context, itemID string) ([]PriorAttempt, error) {
	var out struct {
		Item *struct {
			Evaluations []PriorAttempt `json:"evaluations"`
		} `json:"workItemIndex"`
	}
	q := fmt.Sprintf(`{ workItemIndex(namespace: %q, id: %q) { evaluations { attempt notes failedClauses { clause satisfy } } } }`, NS, itemID)
	if err := c.Query(ctx, q, nil, &out); err != nil {
		return nil, err
	}
	if out.Item == nil {
		return nil, nil
	}
	return out.Item.Evaluations, nil
}

// NoVerdictField and NoVerdictReasonField are how an evaluation that
// reached no verdict is recorded: the evaluator had no way to judge — no
// VERDICT line, no context, its tools or its sandbox unavailable — so the
// item is neither passed nor failed (runsheet ADR 035, an unreached
// evaluation is not a failed one; ADR 011's amendments 2026-09-29 and
// 2026-10-02). They sit beside the platform's required `passed`, which a
// no-verdict sends false: the platform refuses `passed: true` or a blank
// reason with them. The node's earlier guess, `outcome: no_verdict`, was
// never a field of the platform's input.
const (
	NoVerdictField       = "noVerdict"
	NoVerdictReasonField = "noVerdictReason"
)

// ErrNoVerdictOutcome is what EvaluateWorkItemNoVerdict returns when the
// gateway's EvaluateWorkItemInput has no `noVerdict` field (a platform
// older than runsheet ADR 035): nothing was sent, and the caller keeps the
// item in evaluating as before. errors.Is-able.
var ErrNoVerdictOutcome = errors.New("gateway: the platform takes no no-verdict outcome")

// EvaluateWorkItemNoVerdict records an evaluation that reached no
// verdict — `passed: false`, `noVerdict: true`, the reason as
// `noVerdictReason` and in the notes (so a page without the new field
// still shows it), the session's cost and usage report — so the platform
// returns the item to evaluating at no cost to its attempts. It sends it
// only to a gateway whose EvaluateWorkItemInput has `noVerdict` — asked
// once by introspection and kept for ScalingProbeEvery, as takesHolding
// (ADR-004: never a field the platform does not accept) — and returns
// ErrNoVerdictOutcome otherwise. choice.EvaluatedModel is sent as
// evaluatedModel when non-empty, the same field EvaluateWorkItem
// carries (goal one-evaluator-for-ordinary-work).
func (c *Client) EvaluateWorkItemNoVerdict(ctx context.Context, itemID, reason string, costCents int, usage *UsageReport, choice EvaluateChoice) error {
	if !c.takesInputField(ctx, &c.noVerdictSchema, "EvaluateWorkItemInput", NoVerdictField) {
		return ErrNoVerdictOutcome
	}
	in := map[string]any{
		"passed":             false,
		NoVerdictField:       true,
		NoVerdictReasonField: reason,
		"notes":              reason,
		"costCents":          costCents,
	}
	if choice.EvaluatedModel != "" {
		in[EvaluatedModelField] = choice.EvaluatedModel
	}
	return c.mutateWithUsage(ctx, "evaluateWorkItem", itemID, in, usage)
}

// takesInputField says whether the gateway's input type typ has field,
// asked by introspection and kept in p for ScalingProbeEvery. A failed
// probe is a no, kept as long.
func (c *Client) takesInputField(ctx context.Context, p *holdingProbe, typ, field string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := c.now()
	if !p.at.IsZero() && now.Sub(p.at) < ScalingProbeEvery {
		return p.takes
	}
	var out struct {
		Type *struct {
			InputFields []struct {
				Name string `json:"name"`
			} `json:"inputFields"`
		} `json:"__type"`
	}
	p.at, p.takes = now, false
	q := `query($t: String!) { __type(name: $t) { inputFields { name } } }`
	if err := c.Query(ctx, q, map[string]any{"t": typ}, &out); err != nil || out.Type == nil {
		return false
	}
	for _, f := range out.Type.InputFields {
		if f.Name == field {
			p.takes = true
		}
	}
	return p.takes
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

// PlanGitHubTokens asks the platform for a goal's plan-session GitHub
// tokens: one row per repo enrolled in the goal's project, each minted
// from that repo's own GitHub App installation and scoped to it — the
// way IssueItemRepoToken is scoped to an item's one repo (adr-022).
// Only the node holding the goal's plan claim is given them (anything
// else is a refusal, IsRefusal); a platform without the field answers
// IsUnsupported, and the caller falls back to the operator's own
// GitHub login. nodeID is kept for the interface: the platform knows
// the node from the bearer.
func (c *Client) PlanGitHubTokens(ctx context.Context, goalID, nodeID string) ([]GitHubToken, error) {
	_ = nodeID
	return c.sessionGitHubTokens(ctx, "planGitHubToken", "goalId", goalID)
}

// ResearchGitHubTokens is PlanGitHubTokens for a direction's research
// session, held by its own research claim rather than a goal's plan
// claim.
func (c *Client) ResearchGitHubTokens(ctx context.Context, directionID, nodeID string) ([]GitHubToken, error) {
	_ = nodeID
	return c.sessionGitHubTokens(ctx, "researchGitHubToken", "directionId", directionID)
}

// sessionGitHubTokens runs the claim-time GitHub token read for a plan
// or research session: field(arg: $id) answers a list of rows, one per
// repo — {token, expiresAt, repository} — decoded whole into
// GitHubToken.
func (c *Client) sessionGitHubTokens(ctx context.Context, field, arg, id string) ([]GitHubToken, error) {
	var out map[string]any
	q := fmt.Sprintf("query($id: UUID!) { %s(%s: $id) }", field, arg)
	if err := c.Query(ctx, q, map[string]any{"id": id}, &out); err != nil {
		var ge *Error
		if errors.As(err, &ge) && (unknownOperation(ge.Message, field) || schemaMismatch(ge.Message, field)) {
			return nil, &Error{Message: ge.Message, Unsupported: true}
		}
		return nil, err
	}
	rows := rowsOf(out[field])
	toks := make([]GitHubToken, 0, len(rows))
	for _, row := range rows {
		token := str(row, "token")
		repo := str(row, "repository")
		if token == "" {
			return nil, fmt.Errorf("gateway: %s: no token for repo %q", field, repo)
		}
		exp, err := time.Parse(time.RFC3339Nano, str(row, "expiresAt"))
		if err != nil {
			return nil, fmt.Errorf("gateway: %s: expiresAt %q: %w", field, str(row, "expiresAt"), err)
		}
		toks = append(toks, GitHubToken{Token: token, ExpiresAt: exp, Repository: repo})
	}
	return toks, nil
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
	// Precondition is the clause the item waits on — a person's
	// confirmation or a production observation (ADR-023's amendment, the
	// cut-over rule). It rides the brief's first line
	// (BriefWithPrecondition) always, and PlanGoal sends it as the item's
	// `precondition` only when PlannedItemInput takes it
	// (TakesItemPrecondition).
	Precondition string `json:"precondition,omitempty"`
}

// ItemPreconditionField is PlannedItemInput's field for the clause an
// item waits on (ADR-023's amendment, the cut-over rule).
const ItemPreconditionField = "precondition"

// TakesItemPrecondition is whether the gateway's PlannedItemInput has
// `precondition` (introspected, kept for ScalingProbeEvery): a platform
// without it is never sent the field, and the clause rides the brief
// alone.
func (c *Client) TakesItemPrecondition(ctx context.Context) bool {
	return c.takesInputField(ctx, &c.preconditionSchema, "PlannedItemInput", ItemPreconditionField)
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

// ReplaceItemField is RequestPlanInput's narrow re-plan: the one item
// the planner is asked to replace, every other item kept (runsheet ADR
// 023, rung 3; mesh-cli ADR-021's amendment 2026-10-02).
const ReplaceItemField = "replaceItem"

// ErrNoReplaceItem is what RequestPlanReplacing returns when the
// gateway's RequestPlanInput has no `replaceItem` (a platform older than
// the narrow re-plan): nothing was sent. errors.Is-able.
var ErrNoReplaceItem = errors.New("gateway: the platform takes no replaceItem on requestPlan")

// RequestPlanReplacing asks for a narrow re-plan of the goal that
// replaces itemID alone — the evaluator's ask for a split after an
// item's second failed evaluation (ADR-021's amendment 2026-10-02). It
// is sent only to a gateway whose RequestPlanInput has `replaceItem`,
// asked once by introspection and kept for ScalingProbeEvery, and
// returns ErrNoReplaceItem otherwise. A platform that will not take it
// answers a refusal (IsRefusal), returned as such.
func (c *Client) RequestPlanReplacing(ctx context.Context, goalID, by, itemID string) error {
	if !c.takesInputField(ctx, &c.replaceItemSchema, "RequestPlanInput", ReplaceItemField) {
		return ErrNoReplaceItem
	}
	m := map[string]any{ReplaceItemField: itemID}
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

// ReleaseGoalPlan hands back a goal's plan claim this node holds because
// the plan check refused it twice running, with the same (normalised)
// text both times: the node plans no third session, and the reason is
// the refusal's text (goal a-refused-plan-is-released-not-replanned).
// The shape of ReleasePlan; a platform without the mutation answers
// IsUnsupported, a refusal IsRefusal.
func (c *Client) ReleaseGoalPlan(ctx context.Context, goalID, nodeID, reason string) error {
	const field = "releaseGoalPlan"
	err := c.Mutate(ctx, field, goalID, map[string]any{"nodeId": nodeID, "reason": reason})
	var ge *Error
	if errors.As(err, &ge) && (unknownOperation(ge.Message, field) || schemaMismatch(ge.Message, field)) {
		return &Error{Message: ge.Message, Unsupported: true}
	}
	return err
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

// PitchEvidence is one piece of evidence a pitch cites for its problem
// (runsheet trd §6c, ADR 026): a research note, a metric, an incident
// or a user signal, a reference to it and the note explaining it.
type PitchEvidence struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
	Note string `json:"note"`
}

// The kinds a PitchEvidence's Kind takes.
const (
	PitchEvidenceResearchNote = "research_note"
	PitchEvidenceMetric       = "metric"
	PitchEvidenceIncident     = "incident"
	PitchEvidenceUserSignal   = "user_signal"
)

// PitchGoal is one goal a pitch's scope would set if it is approved —
// the whole of a "goal"-scoped pitch, or one of an "initiative"-scoped
// one's several.
type PitchGoal struct {
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Statement   string `json:"statement"`
	Acceptance  string `json:"acceptance"`
	BudgetCents int    `json:"budgetCents,omitempty"`
}

// PitchScope is what a pitch would set if a person approves it: one
// goal, or an initiative of several.
type PitchScope struct {
	Kind  string      `json:"kind"`
	Goals []PitchGoal `json:"goals"`
}

// The kinds a PitchScope's Kind takes.
const (
	PitchScopeGoal       = "goal"
	PitchScopeInitiative = "initiative"
)

// PitchEstimate is a pitch's cost and time estimate and the basis it
// is drawn from.
type PitchEstimate struct {
	CostCents int    `json:"costCents"`
	Days      int    `json:"days"`
	Basis     string `json:"basis"`
}

// PitchSignal is the metric a pitch expects to move: how it is
// measured, which way and over what window.
type PitchSignal struct {
	Metric      string `json:"metric"`
	HowMeasured string `json:"howMeasured"`
	Direction   string `json:"direction,omitempty"`
	WindowDays  int    `json:"windowDays,omitempty"`
}

// The directions a PitchSignal's Direction takes.
const (
	PitchSignalUp   = "up"
	PitchSignalDown = "down"
)

// PitchDraft is one pitch a propose-mode research session hands in
// (runsheet trd §6c, ADR 026): the problem, the evidence for it, the
// strategy bet it serves, the scope it would set if approved, an
// estimate, its risks and alternatives, and the signal it expects to
// move. The platform decides whether it becomes a pitch a person sees.
type PitchDraft struct {
	Slug         string          `json:"slug"`
	Title        string          `json:"title"`
	Problem      string          `json:"problem"`
	Evidence     []PitchEvidence `json:"evidence"`
	Bet          string          `json:"bet"`
	Scope        PitchScope      `json:"scope"`
	Estimate     *PitchEstimate  `json:"estimate,omitempty"`
	Risks        []string        `json:"risks"`
	Alternatives []string        `json:"alternatives"`
	Signal       *PitchSignal    `json:"signal,omitempty"`
}

// PitchAnswerDraft is a research session's answer to an open question
// on one of its project's earlier pitches, read off the research brief
// (runsheet trd §6c).
type PitchAnswerDraft struct {
	PitchSlug string `json:"pitchSlug"`
	Text      string `json:"text"`
}

// Research is what a research session hands in to
// RecordDirectionResearch: a set-mode direction's GoalProposals, or a
// propose-mode direction's PitchDrafts and its answers to open
// questions on its project's earlier pitches.
type Research struct {
	Proposals []GoalProposal
	Pitches   []PitchDraft
	Answers   []PitchAnswerDraft
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

// ErrNoPitches is what RecordDirectionResearch returns when it carried
// pitches or answers but the gateway's input has neither field (a
// platform older than runsheet trd §6c's pitches): the pitches are
// re-sent as proposals — one GoalProposal per goal of each pitch's
// scope, its problem as the proposal's why — and that hand-in lands;
// only the pitches and answers themselves did not. errors.Is-able.
var ErrNoPitches = errors.New("gateway: the platform does not record pitches")

// proposalRows is a []GoalProposal in the mutation's shape.
func proposalRows(proposals []GoalProposal) []map[string]any {
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
	return list
}

// pitchGoalRows is a []PitchGoal in the mutation's shape: a required
// list, sent as `[]` when empty, never omitted.
func pitchGoalRows(goals []PitchGoal) []map[string]any {
	list := make([]map[string]any, 0, len(goals))
	for _, g := range goals {
		row := map[string]any{"slug": g.Slug, "title": g.Title, "statement": g.Statement, "acceptance": g.Acceptance}
		if g.BudgetCents > 0 {
			row["budgetCents"] = g.BudgetCents
		}
		list = append(list, row)
	}
	return list
}

// pitchRows is a []PitchDraft in the mutation's shape: evidence, risks,
// alternatives and scope.goals always sent, `[]` when empty; bet always
// sent, "" allowed; estimate and signal left out when nil.
func pitchRows(pitches []PitchDraft) []map[string]any {
	list := make([]map[string]any, 0, len(pitches))
	for _, p := range pitches {
		evidence := make([]map[string]any, 0, len(p.Evidence))
		for _, e := range p.Evidence {
			evidence = append(evidence, map[string]any{"kind": e.Kind, "ref": e.Ref, "note": e.Note})
		}
		risks := make([]string, 0, len(p.Risks))
		risks = append(risks, p.Risks...)
		alternatives := make([]string, 0, len(p.Alternatives))
		alternatives = append(alternatives, p.Alternatives...)
		row := map[string]any{
			"slug": p.Slug, "title": p.Title, "problem": p.Problem,
			"evidence": evidence, "bet": p.Bet,
			"scope":        map[string]any{"kind": p.Scope.Kind, "goals": pitchGoalRows(p.Scope.Goals)},
			"risks":        risks,
			"alternatives": alternatives,
		}
		if p.Estimate != nil {
			row["estimate"] = map[string]any{"costCents": p.Estimate.CostCents, "days": p.Estimate.Days, "basis": p.Estimate.Basis}
		}
		if p.Signal != nil {
			sig := map[string]any{"metric": p.Signal.Metric, "howMeasured": p.Signal.HowMeasured}
			if p.Signal.Direction != "" {
				sig["direction"] = p.Signal.Direction
			}
			if p.Signal.WindowDays > 0 {
				sig["windowDays"] = p.Signal.WindowDays
			}
			row["signal"] = sig
		}
		list = append(list, row)
	}
	return list
}

// answerRows is a []PitchAnswerDraft in the mutation's shape.
func answerRows(answers []PitchAnswerDraft) []map[string]any {
	list := make([]map[string]any, 0, len(answers))
	for _, a := range answers {
		list = append(list, map[string]any{"pitchSlug": a.PitchSlug, "text": a.Text})
	}
	return list
}

// RecordDirectionResearch hands in what the session came to, and the
// cost whether or not it proposed anything. It also lets the claim go,
// so an empty hand-in is how a node gives a direction back. usage is
// the research session's usage report, carried as `usage` when non-nil
// (see UsageReport, ErrNoUsageLedger).
//
// research.Pitches and research.Answers are sent as `pitches` and
// `answers` only when non-empty. Against a platform old enough to
// refuse them (unknownField on "pitches" or "answers"), the command is
// re-sent with research.Proposals plus one GoalProposal per goal of
// every pitch's scope and no pitches or answers; once that lands,
// ErrNoPitches says the pitches themselves did not, so the caller can
// say so once.
func (c *Client) RecordDirectionResearch(ctx context.Context, directionID, nodeID string, research Research, costCents int, notes string, usage *UsageReport) error {
	at := time.Now().UTC().Format(time.RFC3339Nano)
	input := map[string]any{
		"nodeId": nodeID, "at": at,
		"proposals": proposalRows(research.Proposals), "costCents": costCents, "notes": notes,
	}
	if len(research.Pitches) > 0 {
		input["pitches"] = pitchRows(research.Pitches)
	}
	if len(research.Answers) > 0 {
		input["answers"] = answerRows(research.Answers)
	}
	err := c.mutateWithUsage(ctx, "recordDirectionResearch", directionID, input, usage)
	var ge *Error
	if err == nil || !errors.As(err, &ge) || !unknownField(ge.Message, "pitches", "answers") {
		return err
	}
	fallback := append([]GoalProposal{}, research.Proposals...)
	for _, p := range research.Pitches {
		for _, g := range p.Scope.Goals {
			fallback = append(fallback, GoalProposal{
				Slug: g.Slug, Title: g.Title, Statement: g.Statement,
				Acceptance: g.Acceptance, Why: p.Problem, BudgetCents: g.BudgetCents,
			})
		}
	}
	retryErr := c.mutateWithUsage(ctx, "recordDirectionResearch", directionID, map[string]any{
		"nodeId": nodeID, "at": at,
		"proposals": proposalRows(fallback), "costCents": costCents, "notes": notes,
	}, usage)
	if retryErr != nil && !errors.Is(retryErr, ErrNoUsageLedger) && !errors.Is(retryErr, ErrNoToolUsage) && !errors.Is(retryErr, ErrNoContextUsage) {
		return retryErr
	}
	if retryErr != nil {
		return fmt.Errorf("%w: %w", ErrNoPitches, retryErr)
	}
	return ErrNoPitches
}

// PlanGoal hands the goal its graph, with the planner session's cost
// when one drew it, and usage its usage report (carried as `usage`
// when non-nil: see UsageReport, ErrNoUsageLedger). An item's model is
// sent only when it has one; a platform older than
// items-carry-a-model refuses the field, and the graph is handed in
// again without it. questions are the clauses the planner could not
// phrase as what a pull request can change (ADR-020), sent only to a
// gateway whose PlanGoalInput takes them (TakesPlanQuestions); the
// caller says them otherwise.
func (c *Client) PlanGoal(ctx context.Context, goalID, by string, items []PlannedItem, costCents int, usage *UsageReport, questions ...PlanQuestion) error {
	withPrecondition := false
	for _, it := range items {
		if it.Precondition != "" {
			withPrecondition = c.TakesItemPrecondition(ctx)
			break
		}
	}
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
			if withPrecondition && it.Precondition != "" {
				row[ItemPreconditionField] = it.Precondition
			}
			list = append(list, row)
		}
		m := map[string]any{"items": list, "costCents": costCents}
		if len(questions) > 0 && c.TakesPlanQuestions(ctx) {
			m[PlanQuestionsField] = questions
		}
		if by != "" {
			m["by"] = by
		}
		return c.mutateWithUsage(ctx, "planGoal", goalID, m, usage)
	}
	err := send(true)
	var ge *Error
	if err != nil && errors.As(err, &ge) && unknownField(ge.Message, "model") {
		return send(false)
	}
	return err
}

// PlanQuestionsField is planGoal's input field for the planner's
// questions for the person.
const PlanQuestionsField = "questions"

// PlanQuestion is a clause of the goal no pull request can satisfy, and
// what the planner asks the person about it (ADR-020).
type PlanQuestion struct {
	Clause   string `json:"clause"`
	Question string `json:"question"`
}

// TakesPlanQuestions is whether the gateway's PlanGoalInput has
// `questions` (introspected, kept for ScalingProbeEvery): a platform
// without it is never sent the field.
func (c *Client) TakesPlanQuestions(ctx context.Context) bool {
	return c.takesInputField(ctx, &c.questionsSchema, "PlanGoalInput", PlanQuestionsField)
}

// GoalDraft is one of the goals the planner proposes in place of a
// graph over the project's item cap (ADR-023, the size rule): slug,
// title, statement and acceptance for a goal of at most the cap's
// items naming one repo.
type GoalDraft struct {
	Slug       string `json:"slug"`
	Title      string `json:"title"`
	Statement  string `json:"statement"`
	Acceptance string `json:"acceptance"`
	RepoID     string `json:"repoId"`
}

// ProposeGoalSplit sends the planner's `split` answer — goal drafts in
// place of the items PlanGoal would otherwise send, for a graph over
// the project's item cap (ADR-023) — as
// `proposeGoalSplit(input: ProposeGoalSplitInput!)` on the goal, with
// drafts, costCents and, when by is non-empty, who asked. Sent only
// when the gateway's Mutation type has proposeGoalSplit (TakesGoalSplit):
// a platform without it is &Error{Unsupported: true} and nothing is
// sent, for the caller to fall back on (mesh-specs ADR 013's
// introspection pattern).
func (c *Client) ProposeGoalSplit(ctx context.Context, goalID, by string, drafts []GoalDraft, costCents int, usage *UsageReport) error {
	if !c.TakesGoalSplit(ctx) {
		return &Error{Message: "gateway: the platform's Mutation has no proposeGoalSplit", Unsupported: true}
	}
	list := make([]map[string]any, 0, len(drafts))
	for _, d := range drafts {
		list = append(list, map[string]any{
			"slug": d.Slug, "title": d.Title, "statement": d.Statement,
			"acceptance": d.Acceptance, "repoId": d.RepoID,
		})
	}
	m := map[string]any{"drafts": list, "costCents": costCents}
	if by != "" {
		m["by"] = by
	}
	return c.mutateWithUsage(ctx, "proposeGoalSplit", goalID, m, usage)
}

// TakesGoalSplit is whether the gateway's Mutation type has
// proposeGoalSplit — asked by introspection and kept for
// ScalingProbeEvery, as takesAttestations asks after
// recordValidationAttestation (ADR-010's introspection pattern): the
// planner's split answer is sent only once the platform (goal
// a-goal-fits-in-eight-items) has shipped it.
func (c *Client) TakesGoalSplit(ctx context.Context) bool {
	p := &c.goalSplitSchema
	p.mu.Lock()
	defer p.mu.Unlock()
	now := c.now()
	if !p.at.IsZero() && now.Sub(p.at) < ScalingProbeEvery {
		return p.takes
	}
	var out struct {
		Type *struct {
			Fields []struct {
				Name string `json:"name"`
			} `json:"fields"`
		} `json:"__type"`
	}
	p.at, p.takes = now, false
	q := `{ __type(name: "Mutation") { fields { name } } }`
	if err := c.Query(ctx, q, nil, &out); err != nil || out.Type == nil {
		return false
	}
	for _, f := range out.Type.Fields {
		if f.Name == "proposeGoalSplit" {
			p.takes = true
		}
	}
	return p.takes
}

// SetGoalCheckpoints sets the goal's checkpoints — "a person confirms
// in production" and the like (ADR-023, rule (c)): a clause no pull
// request can satisfy that is not folded into a dependent item's
// precondition becomes one of these instead of an item no builder can
// close. The live mutation; no probe needed.
func (c *Client) SetGoalCheckpoints(ctx context.Context, goalID, by string, checkpoints []string) error {
	cps := checkpoints
	if cps == nil {
		cps = []string{}
	}
	m := map[string]any{"checkpoints": cps}
	if by != "" {
		m["by"] = by
	}
	return c.Mutate(ctx, "setGoalCheckpoints", goalID, m)
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

// ---- mesh-context -------------------------------------------------------

// Document is one row of mesh-context's document list.
type Document struct {
	Slug    string `json:"slug"`
	Title   string `json:"title"`
	Summary string `json:"summary,omitempty"` // one line from the frontmatter: what the index row says
	Kind    string `json:"kind"`
	Status  string `json:"status"`
	Repo    string `json:"repo"`
	Path    string `json:"path"`
}

// contextGet reads a mesh-context path under /mesh/v1, falling back to
// the same path under /prologue/v1 only when /mesh/v1 answers 404 — an
// older platform that serves the alias alone. Drop the fallback after
// 2026-10-28, a month after /mesh/v1 went live (mesh-specs trd §6c).
func (c *Client) contextGet(ctx context.Context, path, accept string) ([]byte, error) {
	raw, status, statusText, err := c.contextGetAt(ctx, "/mesh/v1"+path, accept)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		raw, status, statusText, err = c.contextGetAt(ctx, "/prologue/v1"+path, accept)
		if err != nil {
			return nil, err
		}
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("context: %s: %s", statusText, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

func (c *Client) contextGetAt(ctx context.Context, path, accept string) (raw []byte, status int, statusText string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, 0, "", err
	}
	tok, err := c.Bearer(ctx)
	if err != nil {
		return nil, 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", accept)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, "", err
	}
	defer resp.Body.Close()
	raw, _ = io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return raw, resp.StatusCode, resp.Status, nil
}

// ContextDocuments lists a project's documents (org login + project slug).
func (c *Client) ContextDocuments(ctx context.Context, org, project string) ([]Document, error) {
	raw, err := c.contextGet(ctx, "/projects/"+url.PathEscape(org)+"/"+url.PathEscape(project)+"/documents", "application/json")
	if err != nil {
		return nil, err
	}
	var out struct {
		Documents []Document `json:"documents"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("context: decode documents: %w", err)
	}
	return out.Documents, nil
}

// ContextDocument fetches one document's body as markdown.
func (c *Client) ContextDocument(ctx context.Context, org, project, doc string) (string, error) {
	raw, err := c.contextGet(ctx, "/projects/"+url.PathEscape(org)+"/"+url.PathEscape(project)+"/documents/"+url.PathEscape(doc), "text/markdown")
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
	Kind string
	// Adapter is the agent CLI the credential signs in: ClaudeCodeAdapter
	// (an Anthropic credential) or CodexAdapter (an OpenAI one, whose
	// subscription secret is the auth.json `codex login` writes).
	Adapter string
	// SecretVersion is the platform's version of the sealed secret: a
	// new one is a rotated or re-pasted secret (0: not served).
	SecretVersion int
	Secret        string
	// CapDailyUSD and CapMonthlyUSD are an api key's caps (0: the
	// platform's default, $50 and $1,000); SpentDailyUSD and
	// SpentMonthlyUSD what the ledger has counted against them so far.
	CapDailyUSD     float64
	CapMonthlyUSD   float64
	SpentDailyUSD   float64
	SpentMonthlyUSD float64
	// Priorities are the goal priorities this credential serves (urgent,
	// high, normal, low), the console's checkboxes on the Credentials
	// page; empty serves every priority (adr-005's amendment 2026-10-02,
	// a credential names the priorities it serves).
	Priorities []string
	// WindowTargets is the credential's own pace point, edited on the
	// console: past this share of a window the account takes only
	// urgent and high work, in place of the node-wide limits.pace_five_hour.
	WindowTargets []WindowTarget
}

// WindowTarget is one window's pace point on a credential (the
// console's windowTargets): Percent is 0..100, as given.
type WindowTarget struct {
	Window  string
	Percent float64
}

// String is the credential without its secret.
func (c Credential) String() string {
	serves := "serves all"
	if len(c.Priorities) > 0 {
		serves = "serves " + strings.Join(c.Priorities, ", ")
	}
	return fmt.Sprintf("credential %s (%s %s, %s, %s)", c.Label, c.Adapter, c.Kind, c.ID, serves)
}

// GoString is String, so %#v leaves the secret out too.
func (c Credential) GoString() string { return c.String() }

// Session is what the claim-time read hands the node that claimed a
// session: the credentials it may run on and the tools its project and
// role give it (trd §3 Tools), their secrets decrypted for this node and
// this session only. Tools is nil from a platform without the field.
type Session struct {
	Credentials []Credential
	Tools       []tools.Tool
}

// UsageReport is the platform's UsageReportInput (runsheet trd §6a,
// ADR 018 (e)(f), ADR 019): one session's usage, carried as `usage` on
// the command that ends it — FinishWorkItem, FinishWorkItemAlreadyDone,
// EvaluateWorkItem, PlanGoal, FinishWorkItemRebase,
// RecordDirectionResearch — or on RecordToolUsage for one priced tool
// call. CredentialID is the credential the session ran on (a UUID),
// left out when the platform issued none; CredentialKind is
// "subscription", "api" or "unset". The four window fields (0..1) and
// their *ResetsAt (RFC 3339) are left out for a window the session
// never read. Turns, InputPerTurnMean, InputPerTurnMax and
// LargestToolResults (mesh-spends-less-per-item) are the session's
// per-turn input and largest tool results (harness.Result's fields of
// the same names), left out when zero or empty — a platform whose
// UsageReportInput lacks them gets the report again without them
// (ErrNoUsageDetail). Validation (node-attests-its-validation) is a
// build's validation minutes, per step. It never carries a secret.
type UsageReport struct {
	SessionID        string `json:"sessionId"`
	Adapter          string `json:"adapter"`
	Model            string `json:"model"`
	CredentialID     string `json:"credentialId,omitempty"`
	CredentialKind   string `json:"credentialKind"`
	InputTokens      int64  `json:"inputTokens"`
	OutputTokens     int64  `json:"outputTokens"`
	CacheReadTokens  int64  `json:"cacheReadTokens"`
	CacheWriteTokens int64  `json:"cacheWriteTokens"`
	ToolCalls        int64  `json:"toolCalls"`
	// Tools is ToolCalls per MCP server, with each one's errors and wall
	// time (goal tools-show-their-use); left out when the session called
	// none, and by mutateWithUsage for a platform without the field.
	Tools           []ToolCallReport `json:"tools,omitempty"`
	WallMs          int64            `json:"wallMs"`
	ListCostCents   int64            `json:"listCostCents"`
	ActualCostCents int64            `json:"actualCostCents"`

	Turns              int64            `json:"turns,omitempty"`
	InputPerTurnMean   int64            `json:"inputPerTurnMean,omitempty"`
	InputPerTurnMax    int64            `json:"inputPerTurnMax,omitempty"`
	LargestToolResults []ToolResultSize `json:"largestToolResults,omitempty"`

	// Validation is the build's validation run, per step (goal
	// node-attests-its-validation): the scorecard's "CI minutes saved"
	// counterpart, "validation minutes spent". Left out when the session
	// ran no validation, and by mutateWithUsage for a platform without
	// the field (ErrNoValidationUsage).
	Validation []ValidationMinutes `json:"validation,omitempty"`

	// ContextBytes is what the session's context cost on disk: every
	// document file under .mesh/context plus INDEX.md (adr-016). Left
	// out when none was written, and by mutateWithUsage for a platform
	// without the field (ErrNoContextUsage).
	ContextBytes int64 `json:"contextBytes,omitempty"`

	// DocumentsFetched is the session's mesh_document and mesh_search
	// calls on the built-in context server — what it fetched by tool
	// (adr-016's amendment 2026-10-02, goal
	// context-is-fetched-not-shipped); ToolNames tells the two apart. Left
	// out when the session fetched none, and by mutateWithUsage for a
	// platform without the field (ErrNoDocumentsFetched).
	DocumentsFetched int64 `json:"documentsFetched,omitempty"`
	// ToolNames is the session's MCP calls per <server>__<tool>
	// (harness.Result.ToolNames), beside Tools' per-server totals — so
	// a reading can tell mesh_document from mesh_search. Left out when
	// the session made no MCP call, and by mutateWithUsage for a
	// platform without the field (ErrNoToolNames).
	ToolNames []ToolNameCount `json:"toolNames,omitempty"`

	FiveHourBefore *float64 `json:"fiveHourBefore,omitempty"`
	FiveHourAfter  *float64 `json:"fiveHourAfter,omitempty"`
	SevenDayBefore *float64 `json:"sevenDayBefore,omitempty"`
	SevenDayAfter  *float64 `json:"sevenDayAfter,omitempty"`

	FiveHourResetsAtBefore *time.Time `json:"fiveHourResetsAtBefore,omitempty"`
	FiveHourResetsAtAfter  *time.Time `json:"fiveHourResetsAtAfter,omitempty"`
	SevenDayResetsAtBefore *time.Time `json:"sevenDayResetsAtBefore,omitempty"`
	SevenDayResetsAtAfter  *time.Time `json:"sevenDayResetsAtAfter,omitempty"`
}

// ToolCallReport is one MCP server's calls in a session's UsageReport:
// how many, how many errored, their wall time in milliseconds, and the
// first error's text (at most 200 characters, redacted by the harness).
// Error is sent only to a gateway whose input takes it
// (takesToolError).
type ToolCallReport struct {
	Tool   string `json:"tool"`
	Calls  int64  `json:"calls"`
	Errors int64  `json:"errors"`
	Ms     int64  `json:"ms"`
	Error  string `json:"error,omitempty"`
}

// ToolResultSize is one tool result's content, in bytes — one row of
// UsageReport.LargestToolResults (harness.ToolResultSize's shape).
type ToolResultSize struct {
	Tool  string `json:"tool"`
	Bytes int    `json:"bytes"`
}

// ToolNameCount is one <server>__<tool> name's calls — one row of
// UsageReport.ToolNames.
type ToolNameCount struct {
	Name  string `json:"name"`
	Calls int64  `json:"calls"`
}

// The kinds a UsageReport's CredentialKind takes.
const (
	CredentialSubscription = "subscription"
	CredentialAPI          = "api"
	CredentialUnset        = "unset"
)

// RecordToolUsage hands the platform one priced tool call's usage
// report against the item (aggregateId), naming the tool and its
// tool_call_id (runsheet trd §6a, ADR 019). An older platform without
// the mutation answers IsUnsupported. A platform whose usage input
// lacks only the turn or tool-result detail fields (unknownField on one
// of usageDetailFields) gets the report again without those alone: the
// call still lands, and ErrNoUsageDetail says the detail did not —
// following mutateWithUsage's fallback for the commands that end a
// session.
func (c *Client) RecordToolUsage(ctx context.Context, itemID, toolCallID, tool string, usage UsageReport) error {
	const field = "recordToolUsage"
	usage.Tools = nil          // one call's record needs no per-tool list
	usage.ContextBytes = 0     // nor the session's context, counted once on its own report
	usage.DocumentsFetched = 0 // nor the documents-fetched count
	usage.ToolNames = nil      // nor the per-tool-name counts
	um, err := usageInput(&usage)
	if err != nil {
		return err
	}
	in := map[string]any{
		"aggregateId": itemID, "namespace": NS, "tool": tool, "toolCallId": toolCallID, "usage": um,
	}
	q := `mutation($in: RecordToolUsageInput!) { recordToolUsage(input: $in) { status } }`
	err = c.Query(ctx, q, map[string]any{"in": in}, nil)
	if err == nil {
		return nil
	}
	var ge *Error
	if errors.As(err, &ge) && (unknownOperation(ge.Message, field) || schemaMismatch(ge.Message, field)) {
		return &Error{Message: ge.Message, Unsupported: true}
	}
	if errors.As(err, &ge) && unknownField(ge.Message, usageDetailFields...) {
		trimmed := make(map[string]any, len(um))
		for k, v := range um {
			trimmed[k] = v
		}
		for _, f := range usageDetailFields {
			delete(trimmed, f)
		}
		in["usage"] = trimmed
		if err2 := c.Query(ctx, q, map[string]any{"in": in}, nil); err2 != nil {
			return err2
		}
		return ErrNoUsageDetail
	}
	return err
}

// ---- validation attestation (node-attests-its-validation, runsheet ADR 032) ----

// ValidationAttestation is what a build session's validation run
// produces, uploaded through RecordValidationAttestation in the
// platform's input shape — the keys of
// specs/contracts/recordValidationAttestation.json's input.input, copied
// from runsheet's (runsheet ADR 032, As built (item 3)): the commit and
// its tree, the runner's image digest (or "host:<hostname>" when it ran
// unsandboxed), the validate.yml's hash, the session, the tests summed
// over the commands, each artifact's digest, when, one command per step
// and each step's raw report. The node and the item are not in it: the
// item is the mutation's workItemId and the node the holder the
// platform reads from the bearer.
type ValidationAttestation struct {
	Commit          string              `json:"commit"`
	TreeHash        string              `json:"treeHash"`
	RunnerImage     string              `json:"runnerImage"`
	SuiteHash       string              `json:"suiteHash"`
	SessionID       string              `json:"sessionId"`
	TestsRun        int                 `json:"testsRun"`
	TestsPassed     int                 `json:"testsPassed"`
	TestsFailed     int                 `json:"testsFailed"`
	ArtifactDigests []string            `json:"artifactDigests"`
	At              time.Time           `json:"at"`
	Commands        []ValidationCommand `json:"commands"`
	Reports         []ValidationReport  `json:"reports"`
}

// ValidationCommand is one step of .runsheet/validate.yml as it ran:
// the suite (the step's name), its command, exit code and duration.
type ValidationCommand struct {
	Suite      string `json:"suite"`
	Command    string `json:"command"`
	ExitCode   int    `json:"exitCode"`
	DurationMs int64  `json:"durationMs"`
}

// ValidationReport is one suite's raw test report (Go test JSON or
// JUnit), as its results file held it.
type ValidationReport struct {
	Suite   string `json:"suite"`
	Content string `json:"content"`
}

// ValidationMinutes is one step's cost for the usage record and the
// attestation alike: the scorecard's "validation minutes spent"
// counterpart to CI's "CI minutes saved".
type ValidationMinutes struct {
	Step     string `json:"step"`
	Ms       int64  `json:"ms"`
	ExitCode int    `json:"exitCode"`
}

// AttestationBackoff is the wait between RecordValidationAttestation's
// retries of a transport failure or a 429/5xx answer (retryOnTransient)
// — one entry per retry, so up to len(AttestationBackoff) retries beyond
// the first attempt, and the cap a Retry-After header's wait is held to
// (retryDelay). A test shortens it to run fast.
var AttestationBackoff = []time.Duration{2 * time.Second, 10 * time.Second, 30 * time.Second}

// retryOnTransient calls fn, retrying on a transport error or a 429/5xx
// answer (isTransient) up to len(AttestationBackoff) more times with
// that backoff between attempts, honouring a Retry-After header in place
// of the table's entry for the attempt (retryDelay); a refusal, an
// unsupported operation, or success returns at once — the gateway's
// considered answer is not retried, only its absence. The retries spent
// with no success return the last answer wrapped so it reads "after N
// retries: <last answer>" (errors.As still reaches the wrapped error).
func retryOnTransient(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; ; attempt++ {
		err = fn()
		if err == nil || !isTransient(err) {
			return err
		}
		if attempt >= len(AttestationBackoff) {
			return fmt.Errorf("after %d retries: %w", len(AttestationBackoff), err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryDelay(err, attempt)):
		}
	}
}

// isTransient reports whether err is worth retrying: a transport failure
// that never reached the gateway, a 5xx or 429 httpError, or a GraphQL
// *Error the gateway (or a proxy in front of it) wrapped with a 429
// status. Any other GraphQL *Error — a refusal, an unsupported
// operation, a validation error the schema itself raised, all answered
// with an ordinary 200 — is the gateway's considered answer, not a blip,
// and is never retried.
func isTransient(err error) bool {
	var ge *Error
	if errors.As(err, &ge) {
		return ge.Status == http.StatusTooManyRequests
	}
	var he *httpError
	if errors.As(err, &he) {
		return he.Status >= 500 || he.Status == http.StatusTooManyRequests
	}
	return true
}

// retryDelay is the wait before the next attempt: the table's entry for
// attempt, unless the last answer named a Retry-After, which is honoured
// instead, capped at the table's largest entry so a generous header
// cannot stall the upload far past what the table would.
func retryDelay(err error, attempt int) time.Duration {
	d := AttestationBackoff[attempt]
	if ra := retryAfterOf(err); ra > 0 {
		d = ra
		if largest := AttestationBackoff[len(AttestationBackoff)-1]; d > largest {
			d = largest
		}
	}
	return d
}

// retryAfterOf reads the Retry-After a transient answer named, 0 when it
// named none.
func retryAfterOf(err error) time.Duration {
	var ge *Error
	if errors.As(err, &ge) {
		return ge.RetryAfter
	}
	var he *httpError
	if errors.As(err, &he) {
		return he.RetryAfter
	}
	return 0
}

// RecordValidationAttestation uploads one build's validation run for
// the platform to sign (runsheet ADR 032, As built (items 3 and 33)): on
// the node's own token,
// `mutation($id: UUID!, $in: Map!) { recordValidationAttestation(workItemId: $id, input: $in) }`
// with id the item and in the attestation — no selection set, the
// answer is a Map (specs/contracts/recordValidationAttestation.json,
// copied from runsheet's, is the contract and its fixture), returned as
// the platform's receipt. Whether the Mutation type has the field is
// asked by introspection once and kept for ScalingProbeEvery
// (takesAttestations): a platform observed without it is
// &Error{Unsupported: true} and nothing is sent, but a probe that
// cannot be answered at all is its own error, never Unsupported — an
// introspection outage must not read as an absent mutation. A platform
// with it that refuses ("<sha> is
// not the head of <branch> …") is a *Error carrying its words, a
// refusal (IsRefusal), never Unsupported. A transport failure or a
// 429/5xx answer — an httpError, or a GraphQL error body a proxy
// wrapped with that status — retries with backoff, honouring a
// Retry-After header (retryOnTransient, AttestationBackoff), before
// giving up and returning the last answer wrapped as "after N retries:
// <last answer>". There is no REST path.
func (c *Client) RecordValidationAttestation(ctx context.Context, itemID string, att ValidationAttestation) (map[string]any, error) {
	takes, err := c.takesAttestations(ctx)
	if err != nil {
		return nil, fmt.Errorf("gateway: probing for recordValidationAttestation: %w", err)
	}
	if !takes {
		return nil, &Error{Message: "gateway: the platform's Mutation has no recordValidationAttestation", Unsupported: true}
	}
	body, err := json.Marshal(att)
	if err != nil {
		return nil, err
	}
	var in map[string]any
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	var out struct {
		Receipt map[string]any `json:"recordValidationAttestation"`
	}
	q := `mutation($id: UUID!, $in: Map!) { recordValidationAttestation(workItemId: $id, input: $in) }`
	err = retryOnTransient(ctx, func() error {
		return c.Query(ctx, q, map[string]any{"id": itemID, "in": in}, &out)
	})
	if err != nil {
		return nil, err
	}
	return out.Receipt, nil
}

// attestationProbe is the cached answer to whether the gateway's
// Mutation type has recordValidationAttestation, and when it was asked.
type attestationProbe struct {
	mu    sync.Mutex
	at    time.Time
	takes bool
}

// takesAttestations says whether the gateway's Mutation type has
// recordValidationAttestation taking workItemId and input — asked by
// introspection and kept for ScalingProbeEvery, as takesCommit is. An
// observed absence — the query answers and the field is not listed —
// is cached and returned as false, nil: that is IsUnsupported's case,
// a platform older than the node. A probe that cannot be answered —
// the introspection query itself fails, a transport error or a 5xx —
// is not cached and is returned as its error: a transient failure to
// ask must never read as "the platform has no
// recordValidationAttestation" and trip the once-per-run notice.
func (c *Client) takesAttestations(ctx context.Context) (bool, error) {
	p := &c.attestationSchema
	p.mu.Lock()
	defer p.mu.Unlock()
	now := c.now()
	if !p.at.IsZero() && now.Sub(p.at) < ScalingProbeEvery {
		return p.takes, nil
	}
	var out struct {
		Type *struct {
			Fields []struct {
				Name string `json:"name"`
				Args []struct {
					Name string `json:"name"`
				} `json:"args"`
			} `json:"fields"`
		} `json:"__type"`
	}
	q := `{ __type(name: "Mutation") { fields { name args { name } } } }`
	if err := c.Query(ctx, q, nil, &out); err != nil {
		return false, err
	}
	takes := false
	if out.Type != nil {
		for _, f := range out.Type.Fields {
			if f.Name == "recordValidationAttestation" {
				takes = true
			}
		}
	}
	p.at, p.takes = now, takes
	return takes, nil
}
