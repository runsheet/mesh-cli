package gateway

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"projectmesh.io/mesh-cli/internal/tools"
)

// The claim-time read (adr-005, adr-006) is the platform's pair of fields
// for the session's subject — runsheet ADR 019 (credentials) and ADR 020
// (tools), served on the gateway alone and only to the node holding the
// claim:
//
//	itemCredentials(itemId)          itemTools(itemId)           build, evaluate, rebase
//	planCredentials(goalId)          planTools(goalId)           plan
//	researchCredentials(directionId) researchTools(directionId)  research
//
// Each returns a JSON list (a Map scalar): a credential is {id, label,
// vendor, kind, serves, plan, priorities, windowTargets, dailyCap,
// monthlyCap, capUnit, secretVersion, secret} — priorities a JSON list
// of urgent/high/normal/low, absent or empty meaning every priority
// (adr-005's amendment 2026-10-02), windowTargets a list of
// {window, percent}; a tool {id, slug, name, kind, need,
// url, command, args, runtime, builtin, builtIn, account, headers, env}
// with headers and env as [{name, value} | {name, source}]. The node
// is not named: the platform knows it from the bearer.

// sessionFields is the pair of fields and the argument a session ref's
// subject is read with.
type sessionFields struct {
	credentials, tools, arg, id string
}

// fieldsFor names the read for ref: the item's fields when it has an
// item (a build, an evaluation or a rebase), else the goal's (a plan),
// else the direction's (research). An empty ref is an error.
func fieldsFor(ref SessionRef) (sessionFields, error) {
	switch {
	case ref.ItemID != "":
		return sessionFields{"itemCredentials", "itemTools", "itemId", ref.ItemID}, nil
	case ref.GoalID != "":
		return sessionFields{"planCredentials", "planTools", "goalId", ref.GoalID}, nil
	case ref.DirectionID != "":
		return sessionFields{"researchCredentials", "researchTools", "directionId", ref.DirectionID}, nil
	}
	return sessionFields{}, fmt.Errorf("session %s names no item, goal or direction", ref.Kind)
}

// SessionCredentials is the claim-time read's credentials alone (Session).
func (c *Client) SessionCredentials(ctx context.Context, ref SessionRef, nodeID string) ([]Credential, error) {
	s, err := c.Session(ctx, ref, nodeID)
	if err != nil {
		return nil, err
	}
	return s.Credentials, nil
}

// Session is the claim-time read: the credentials the session ref names
// may run on and its tools, their secrets decrypted for the node that
// holds the session's claim (anything else is a refusal). It reads the
// platform's fields for the ref's subject (itemCredentials/itemTools,
// planCredentials/planTools, researchCredentials/researchTools). A
// platform without the credentials field answers IsUnsupported, and the
// node falls back to node.yaml's accounts; one without the tools field
// is asked for the credentials alone from then on, and the session gets
// no console tools. nodeID is kept for the interface: the platform
// knows the node from the bearer. No error, log line or report carries
// a secret.
func (c *Client) Session(ctx context.Context, ref SessionRef, nodeID string) (*Session, error) {
	_ = nodeID
	f, err := fieldsFor(ref)
	if err != nil {
		return nil, err
	}
	q := func(withTools bool) string {
		sel := fmt.Sprintf("%s(%s: $id)", f.credentials, f.arg)
		if withTools {
			sel += fmt.Sprintf(" %s(%s: $id)", f.tools, f.arg)
		}
		return "query($id: UUID!) { " + sel + " }"
	}
	var out map[string]any
	vars := map[string]any{"id": f.id}
	_, withoutTools := c.toolsUnknown.Load(f.tools)
	if !withoutTools {
		err = c.Query(ctx, q(true), vars, &out)
		var ge *Error
		if err != nil && errors.As(err, &ge) && unknownField(ge.Message, f.tools) && !unknownField(ge.Message, f.credentials) {
			c.toolsUnknown.Store(f.tools, true)
			withoutTools = true
		}
	}
	if withoutTools {
		out = nil
		err = c.Query(ctx, q(false), vars, &out)
	}
	if err != nil {
		var ge *Error
		if errors.As(err, &ge) && (unknownField(ge.Message, f.credentials) || unknownOperation(ge.Message, f.credentials) || schemaMismatch(ge.Message, f.credentials)) {
			return nil, &Error{Message: ge.Message, Unsupported: true}
		}
		return nil, err
	}
	s := &Session{Credentials: []Credential{}}
	for _, row := range rowsOf(out[f.credentials]) {
		if cr, ok := issuedCredential(row); ok {
			s.Credentials = append(s.Credentials, cr)
		}
	}
	if raw, ok := out[f.tools]; ok && raw != nil {
		s.Tools = []tools.Tool{}
		for _, row := range rowsOf(raw) {
			if t, ok := servedTool(row); ok {
				s.Tools = append(s.Tools, t)
			}
		}
	}
	return s, nil
}

// rowsOf is a Map list as the gateway's JSON decodes it.
func rowsOf(v any) []map[string]any {
	list, _ := v.([]any)
	rows := make([]map[string]any, 0, len(list))
	for _, e := range list {
		if m, ok := e.(map[string]any); ok {
			rows = append(rows, m)
		}
	}
	return rows
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func strs(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// The adapters a session credential may serve: the agent CLI it signs
// in.
const (
	ClaudeCodeAdapter = "claude-code"
	CodexAdapter      = "codex"
)

// issuedCredential is the platform's credential row as a session
// account: an Anthropic credential serving claude-code (or everything),
// or an OpenAI one whose serves name codex — a Codex account. Any other
// row is a tool's account (adr-006), not a session's, and is not one.
// kind subscription and oauth_token are a subscription's token (for
// OpenAI, the ChatGPT login's auth.json); api_key an api key whose caps,
// in dollars, are the row's when the cap unit is dollars (else the
// node's defaults). Spend is not carried: the platform leaves a key at
// its cap out of the read.
func issuedCredential(row map[string]any) (Credential, bool) {
	serves := strs(row["serves"])
	var adapter string
	switch str(row, "vendor") {
	case "anthropic":
		if len(serves) > 0 && !slices.Contains(serves, ClaudeCodeAdapter) {
			return Credential{}, false
		}
		adapter = ClaudeCodeAdapter
	case "openai":
		if !slices.Contains(serves, CodexAdapter) {
			return Credential{}, false
		}
		adapter = CodexAdapter
	default:
		return Credential{}, false
	}
	cr := Credential{ID: str(row, "id"), Label: str(row, "label"), Adapter: adapter, Secret: str(row, "secret")}
	if v, ok := row["secretVersion"].(float64); ok {
		cr.SecretVersion = int(v)
	}
	switch str(row, "kind") {
	case "subscription", "oauth_token":
		cr.Kind = "subscription"
	case "api_key", "api":
		cr.Kind = "api"
	default:
		return Credential{}, false
	}
	if cr.Kind == "api" && str(row, "capUnit") != "calls" {
		cr.CapDailyUSD, _ = row["dailyCap"].(float64)
		cr.CapMonthlyUSD, _ = row["monthlyCap"].(float64)
	}
	cr.Priorities = priorities(row["priorities"])
	cr.WindowTargets = windowTargets(row["windowTargets"])
	return cr, true
}

// The goal priorities a credential may name (runsheet's Priority enum,
// lower-cased).
var credentialPriorities = []string{"urgent", "high", "normal", "low"}

// priorities is row["priorities"] kept to the known priorities,
// lower-cased; absent or empty (or none recognised) is nil, meaning
// every priority.
func priorities(v any) []string {
	var out []string
	for _, s := range strs(v) {
		s = strings.ToLower(s)
		if slices.Contains(credentialPriorities, s) {
			out = append(out, s)
		}
	}
	return out
}

// windowTargets is row["windowTargets"] ([{window, percent}]) as
// []WindowTarget, percent kept as given (0..100). A malformed entry is
// skipped.
func windowTargets(v any) []WindowTarget {
	var out []WindowTarget
	for _, row := range rowsOf(v) {
		window, ok := normalizeWindow(str(row, "window"))
		if !ok {
			continue
		}
		pct, _ := row["percent"].(float64)
		out = append(out, WindowTarget{Window: window, Percent: pct})
	}
	return out
}

// normalizeWindow accepts the platform's spelling of a credential
// window alongside the five_hour/fiveHour and seven_day/sevenDay
// variants (the console and the heartbeat have used both), and answers
// the canonical five_hour/seven_day; an unknown name is not ok.
func normalizeWindow(w string) (string, bool) {
	switch w {
	case "five_hour", "fiveHour":
		return "five_hour", true
	case "seven_day", "sevenDay":
		return "seven_day", true
	default:
		return "", false
	}
}

// ItemGitHubTokenVar is the session variable a setting sourced from the
// item's GitHub token reads (ghcred.go puts GH_TOKEN on every item
// session; Claude Code and Codex expand ${VAR} in a server's headers and
// env).
const ItemGitHubTokenVar = "${GH_TOKEN}"

var toolName = regexp.MustCompile(`^[a-z0-9_-]+$`)

// servedTool is the platform's tool row as the node's Tool: a remote or
// local MCP server named by its slug. A builtin switch and the
// platform's own built-in (mesh-context, which the node attaches itself
// under that name too, tools.Builtin) are not tools of the read. A
// setting is a secret when the tool is served through an account — its
// value may be that account's — else a plain header or env; one sourced
// from the item's GitHub token reads the session's variable. A row with
// a name no renderer takes is left out.
func servedTool(row map[string]any) (tools.Tool, bool) {
	if b, _ := row["builtIn"].(bool); b || str(row, "kind") == "builtin" {
		return tools.Tool{}, false
	}
	t := tools.Tool{Name: str(row, "slug"), URL: str(row, "url"), Command: str(row, "command"), Runtime: str(row, "runtime"),
		Required: str(row, "need") == "required"}
	if !toolName.MatchString(t.Name) {
		return tools.Tool{}, false
	}
	switch str(row, "kind") {
	case "remote_mcp":
		t.Transport = tools.Remote
	case "local_mcp":
		t.Transport = tools.Local
	default:
		return tools.Tool{}, false
	}
	if args := strs(row["args"]); len(args) > 0 {
		t.Args = args
	}
	// a price per call, when the platform serves one, is what the node
	// reports the tool's calls against (recordToolUsage)
	if cents, ok := row["pricePerCallCents"].(float64); ok {
		t.PricePerCallCents = int(cents)
	}
	_, hasAccount := row["account"].(map[string]any)
	settings := func(v any) (plain, secret map[string]string) {
		for _, s := range rowsOf(v) {
			name := str(s, "name")
			if name == "" {
				continue
			}
			if str(s, "source") == "item_github_token" {
				if plain == nil {
					plain = map[string]string{}
				}
				plain[name] = ItemGitHubTokenVar
				continue
			}
			value := str(s, "value")
			if hasAccount && value != "" {
				if secret == nil {
					secret = map[string]string{}
				}
				secret[name] = value
				continue
			}
			if plain == nil {
				plain = map[string]string{}
			}
			plain[name] = value
		}
		return plain, secret
	}
	var secrets map[string]string
	switch t.Transport {
	case tools.Remote:
		t.Headers, secrets = settings(row["headers"])
	case tools.Local:
		t.Env, secrets = settings(row["env"])
	}
	t.Secrets = secrets
	return t, true
}

// String is a session's shape for a log line: counts and names, no value.
func (s *Session) String() string {
	names := make([]string, len(s.Tools))
	for i, t := range s.Tools {
		names[i] = t.Name
	}
	return fmt.Sprintf("%d credential(s), tools [%s]", len(s.Credentials), strings.Join(names, " "))
}
