package gatewaytest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"

	"projectmesh.io/mesh-cli/internal/gateway"
	contracts "projectmesh.io/mesh-cli/specs/contracts/runsheet"
)

// The fakes speak runsheet's published contracts (runsheet ADR 026, ADR
// 069): every request the fake takes on a contracted route, and every
// answer it gives there, must fit the fixture runsheet publishes for it,
// as scripts/contracts-pull.sh copied it to specs/contracts/runsheet. A
// request that does not fit is refused as the gateway would refuse it; an
// answer that does not fit is replaced by a 500 naming the misfit. Both
// are kept as violations, and Close panics on any not taken by
// TakeContractViolations, so a fake that drifts from the contract fails
// the test that ran it (the planning loop of 2026-10-24, the changeset
// wire mismatch of #1511).

// ContractedFields are the GraphQL fields whose requests and answers the
// fake checks against runsheet's fixtures of the same field.
var ContractedFields = []string{
	"claimJob",
	"finishJob",
	"jobAccess",
	"planGoal",
	"claimPlan",
	"recordValidationAttestation",
}

// ContractedRoutes are the /mesh/v1 routes the fake checks against
// runsheet's http fixtures of the same method and path (a path segment
// that is an id or a sha in the fixture matches any segment).
var ContractedRoutes = []Route{
	{http.MethodPost, "/mesh/v1/items/{id}/changeset/missing"},
	{http.MethodPut, "/mesh/v1/items/{id}/changeset/objects/{type}/{sha}"},
	{http.MethodHead, "/mesh/v1/items/{id}/changeset/objects/{type}/{sha}"},
	{http.MethodPost, "/mesh/v1/items/{id}/changesets"},
}

// PendingFixtures are the contracted fields and routes runsheet does not
// publish a fixture for yet, each with why: the fake serves them
// unchecked until a pull brings one, and TestContractedRoutesHaveFixtures
// fails once it has, so the entry is removed and the check starts.
var PendingFixtures = map[string]string{}

// FixtureGaps are keys runsheet's gateway reads in a contracted field's
// request, or writes in its answer, that its one example does not show:
// each path (dotted, from the arguments or the answer) with where
// runsheet reads or writes it. The fake takes them, of any type, beside
// the fixture's own; once a pull brings a fixture that shows one,
// TestFixtureGapsAreStillGaps fails so the entry goes. Never add a key
// here that runsheet does not read or write.
var FixtureGaps = map[string]Gaps{
	"claimJob": {Request: map[string]string{
		"input.actor":     "runsheet gateway/claimjob.go readJobClaim: str(in, \"actor\")",
		"input.sessionId": "runsheet gateway/claimjob.go readJobClaim: str(in, \"sessionId\")",
	}, Answer: map[string]string{
		"job.thread.projectId": "runsheet gateway/claimjob.go: out[\"projectId\"] when the thread session names a project",
		"job.thread.asker":     "runsheet gateway/claimjob.go: out[\"asker\"], the turn's asker",
		"job.thread.trigger":   "runsheet gateway/claimjob.go: out[\"trigger\"], the thread's trigger",
		"job.thread.replyHook": "runsheet gateway/claimjob.go: out[\"replyHook\"], the thread's reply hook",
	}},
	"recordValidationAttestation": {Ahead: map[string]string{
		"input.commands[].environmentKind": "mesh-cli ADR-030 (an environment failure names its kind); runsheet reads none yet, dropping the Map key",
		"input.mergeBatchId":               "mesh-cli sends a batch candidate's mergeBatchId inside the input too (gateway/mainvalidation.go); runsheet reads only the argument (gateway/attestation.go)",
	}},
	"jobAccess": {Answer: map[string]string{
		"repo":  "runsheet gateway/jobaccess.go answerJobAccess: out[accessRepo], the item's repo token",
		"model": "runsheet gateway/jobaccess.go answerJobAccess: out[accessModel], the model account",
		"cloud": "runsheet gateway/jobaccess.go answerJobAccess: out[accessCloud], the deploy credential",
		"mcp":   "runsheet gateway/jobaccess.go answerJobAccess: out[\"mcp\"], a product's MCP tokens by name",
	}},
}

// Gaps are a field's request and answer paths its fixture does not show:
// Request and Answer runsheet reads or writes; Ahead the node sends and
// runsheet does not read at all yet (a Map key it drops), each with the
// mesh-cli decision that sends it — drift the fake allows by name only.
// A path's "x[]" is every row of the list x, and "*" every key.
type Gaps struct{ Request, Answer, Ahead map[string]string }

// finishParts are finishJob's finish keys beyond the thread answer its
// fixture shows, each the input of the command runsheet routes it to
// (gateway/finishjob.go finishSteps: on("changeset") is finishWorkItem's
// input, on("plan") planGoal's, and so on): the command's own fixture
// joined with its input type in schema.json, so a plan handed in by
// finishJob is held to planGoal's contract.
func finishParts() map[string]any {
	commandInput := func(field, inputType string) any {
		var shape any
		for _, x := range FieldFixtures(field) {
			if x.Kind == "mutation" && !strings.HasPrefix(x.InputType, "(") {
				shape = joinShapes(shape, x.Input)
			}
		}
		return joinShapes(shape, schemaShape(inputType))
	}
	var attestation any
	for _, x := range FieldFixtures("recordValidationAttestation") {
		in, _ := x.request().(map[string]any)
		attestation = joinShapes(attestation, in["input"])
	}
	attestation = withGaps(FixtureGaps["recordValidationAttestation"].Ahead, map[string]any{"input": attestation}).(map[string]any)["input"]
	return map[string]any{
		"answer":       "",
		"proposal":     "",
		"result":       nil, // the product's own object, held to its finishSchema
		"attestation":  attestation,
		"changeset":    commandInput("finishWorkItem", "FinishWorkItemInput"),
		"verdict":      commandInput("evaluateWorkItem", "EvaluateWorkItemInput"),
		"plan":         commandInput("planGoal", "PlanGoalInput"),
		"research":     commandInput("recordDirectionResearch", "RecordDirectionResearchInput"),
		"rebasedHead":  commandInput("finishWorkItemRebase", "FinishWorkItemRebaseInput"),
		"deployRecord": nil, // recordDeployAttestation's Map
	}
}

// usageAhead are the keys of the node's usage report (gateway.UsageReport)
// that runsheet's UsageReportInput (schema.json) has not: the per-step
// validation minutes, per-tool counts and names, turn detail, documents
// fetched — each sent ahead of runsheet, whose finishJob decodes a
// finish part leniently (gateway/finishjob.go decodeCommand) and drops
// them. finishJob takes them on any finish part's usage; the typed
// mutations refuse them as runsheet's schema does.
func usageAhead() map[string]string {
	known, _ := schemaShape("UsageReportInput").(map[string]any)
	out := map[string]string{}
	t := reflect.TypeFor[gateway.UsageReport]()
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		if _, ok := known[name]; !ok {
			out["input.finish.*.usage."+name] = "gateway.UsageReport." + t.Field(i).Name + ", which runsheet's UsageReport has not"
		}
	}
	return out
}

// ahead are the paths a test's fake takes beyond runsheet's contract by
// its own setting, a platform ahead of runsheet's: TakesDescription, a
// FinishWorkItemInput with `description`; PlanQuestions, a PlanGoalInput
// with `questions` (ADR-020). runsheet has neither.
func (f *Fake) ahead(field string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	if field == "finishJob" {
		maps.Copy(out, usageAhead())
	}
	if field == "finishJob" && f.TakesDescription {
		out["input.finish.changeset.description"] = "Fake.TakesDescription"
	}
	if field == "finishJob" && f.PlanQuestions {
		out["input.finish.plan.questions"] = "Fake.PlanQuestions"
	}
	if field == "planGoal" && f.PlanQuestions {
		out["input.questions"] = "Fake.PlanQuestions"
	}
	return out
}

// withGaps is shape with the paths added as any value.
func withGaps(paths map[string]string, shape any) any {
	for path := range paths {
		shape = addPath(shape, strings.Split(path, "."))
	}
	return shape
}

func addPath(shape any, keys []string) any {
	if len(keys) == 0 {
		return shape
	}
	if key, ok := strings.CutSuffix(keys[0], "[]"); ok {
		m, isMap := shape.(map[string]any)
		rows, isList := m[key].([]any)
		if !isMap || !isList {
			return shape
		}
		out := map[string]any{}
		for k, v := range m {
			out[k] = v
		}
		next := make([]any, len(rows))
		for i, row := range rows {
			next[i] = addPath(row, keys[1:])
		}
		out[key] = next
		return out
	}
	m, ok := shape.(map[string]any)
	if !ok {
		return shape
	}
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	if keys[0] == "*" {
		for k, v := range m {
			out[k] = addPath(v, keys[1:])
		}
		return out
	}
	if len(keys) == 1 {
		if _, has := out[keys[0]]; !has {
			out[keys[0]] = nil
		}
		return out
	}
	out[keys[0]] = addPath(out[keys[0]], keys[1:])
	return out
}

// Route is a method and a path pattern; {x} matches any one segment.
type Route struct{ Method, Path string }

func (r Route) String() string { return r.Method + " " + r.Path }

func (r Route) matches(method, path string) bool {
	if r.Method != method {
		return false
	}
	want, got := strings.Split(r.Path, "/"), strings.Split(path, "/")
	if len(want) != len(got) {
		return false
	}
	for i := range want {
		if !strings.HasPrefix(want[i], "{") && want[i] != got[i] {
			return false
		}
	}
	return true
}

// Fixture is one of runsheet's published contract documents.
type Fixture struct {
	Name      string         `json:"-"`
	Kind      string         `json:"kind"`
	Field     string         `json:"field"`
	InputType string         `json:"inputType"`
	Input     any            `json:"input"`
	Result    any            `json:"result"`
	Args      map[string]any `json:"args"`
	Sample    any            `json:"sample"`
	// Status is an http fixture's answer status, 0 when it names none.
	Status int `json:"status"`
}

// request is the shape the fixture says the caller sends: a mutation's
// arguments by name, a query's args, an http call's body. A fixture is
// one example, so a typed GraphQL argument's shape is the example's
// joined with its input type in schema.json (the gateway's published
// schema, refreshed daily by schema-refresh): an optional field the
// example leaves out is the schema's, of the schema's type; a field in
// neither is outside the contract.
func (x Fixture) request() any {
	switch x.Kind {
	case "mutation":
		if strings.HasPrefix(x.InputType, "(") {
			in, _ := x.Input.(map[string]any)
			out := map[string]any{}
			for k, v := range in {
				out[k] = v
			}
			for _, a := range argTypeRe.FindAllStringSubmatch(x.InputType, -1) {
				out[a[1]] = joinShapes(out[a[1]], schemaShape(a[2]))
			}
			return out
		}
		return map[string]any{"input": joinShapes(x.Input, schemaShape(x.InputType))}
	case "http":
		return x.Args["body"]
	}
	return x.Args
}

var argTypeRe = regexp.MustCompile(`(\w+)\s*:\s*([\w\[\]!]+)`)

var (
	schemaOnce  sync.Once
	schemaTypes map[string]schemaType
)

type schemaType struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Fields []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"fields"`
}

// schemaShape is a GraphQL type of internal/gateway/schema.json as a
// shape: an input object's fields, a list's row, a scalar's JSON type;
// nil (any) for Map and for a type the schema does not list.
func schemaShape(typ string) any {
	schemaOnce.Do(func() {
		schemaTypes = map[string]schemaType{}
		_, file, _, _ := runtime.Caller(0)
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "schema.json"))
		if err != nil {
			return
		}
		var doc struct {
			Types []schemaType `json:"types"`
		}
		if json.Unmarshal(raw, &doc) == nil {
			for _, t := range doc.Types {
				schemaTypes[t.Name] = t
			}
		}
	})
	return shapeOf(typ, 0)
}

func shapeOf(typ string, depth int) any {
	typ = strings.TrimSuffix(typ, "!")
	if strings.HasPrefix(typ, "[") {
		return []any{shapeOf(strings.TrimSuffix(strings.TrimPrefix(typ, "["), "]"), depth)}
	}
	switch typ {
	case "String", "ID", "UUID", "Time", "Date", "Namespace":
		return ""
	case "Int", "Long", "Float":
		return 0.0
	case "Boolean":
		return false
	}
	t, ok := schemaTypes[typ]
	if !ok || depth > 8 {
		return nil
	}
	switch t.Kind {
	case "ENUM":
		return ""
	case "INPUT_OBJECT":
		out := map[string]any{}
		for _, f := range t.Fields {
			out[f.Name] = shapeOf(f.Type, depth+1)
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	return nil
}

// joinShapes is the fixture's shape with the schema's: the fixture's
// value where it has one, the schema's keys and rows beside it.
func joinShapes(fixture, schema any) any {
	switch s := schema.(type) {
	case map[string]any:
		f, ok := fixture.(map[string]any)
		if fixture != nil && !ok {
			return fixture
		}
		out := map[string]any{}
		for k, v := range s {
			out[k] = v
		}
		for k, v := range f {
			out[k] = joinShapes(v, s[k])
		}
		return out
	case []any:
		f, ok := fixture.([]any)
		if fixture != nil && !ok {
			return fixture
		}
		if len(f) == 0 {
			return s
		}
		out := make([]any, len(f))
		for i, row := range f {
			out[i] = joinShapes(row, s[0])
		}
		return out
	}
	if fixture != nil {
		return fixture
	}
	return schema
}

// answer is the shape the fixture says the gateway answers.
func (x Fixture) answer() any {
	if x.Kind == "mutation" {
		return x.Result
	}
	return x.Sample
}

var (
	fixturesOnce sync.Once
	fixtures     []Fixture
	fixturesErr  error
	pulledFrom   string
)

// Fixtures are every pulled fixture, by name, and the ref they were
// pulled from.
func Fixtures() ([]Fixture, string, error) {
	fixturesOnce.Do(func() {
		pulled, err := contracts.FS.ReadFile("PULLED")
		if err != nil {
			fixturesErr = err
			return
		}
		pulledFrom = strings.TrimSpace(strings.ReplaceAll(string(pulled), "\n", ", "))
		names, err := fs.Glob(contracts.FS, "*.json")
		if err != nil {
			fixturesErr = err
			return
		}
		for _, name := range names {
			raw, err := contracts.FS.ReadFile(name)
			if err != nil {
				fixturesErr = err
				return
			}
			var x Fixture
			if err := json.Unmarshal(raw, &x); err != nil {
				// a fixture that is not a document of this shape (a
				// list, a bare answer) is not one a route matches
				continue
			}
			x.Name = strings.TrimSuffix(name, ".json")
			fixtures = append(fixtures, x)
		}
	})
	return fixtures, pulledFrom, fixturesErr
}

// FieldFixtures are the fixtures of a GraphQL field: its mutation or
// query and every variant (`<field>-<variant>.json`).
func FieldFixtures(field string) []Fixture {
	all, _, _ := Fixtures()
	var out []Fixture
	for _, x := range all {
		if x.Field == field && (x.Kind == "mutation" || x.Kind == "query") {
			out = append(out, x)
		}
	}
	return out
}

// RouteFixtures are the http fixtures whose method and path route r
// matches.
func RouteFixtures(r Route) []Fixture {
	all, _, _ := Fixtures()
	var out []Fixture
	for _, x := range all {
		if x.Kind != "http" {
			continue
		}
		method, _ := x.Args["method"].(string)
		path, _ := x.Args["path"].(string)
		path, _, _ = strings.Cut(path, "?")
		if r.matches(method, path) {
			out = append(out, x)
		}
	}
	return out
}

// Fits reports why got is not of want's shape, nil when it is. A shape is
// what a fixture shows: an object's keys (got may carry fewer, never one
// the fixture lacks; an empty object in the fixture is any object), a
// list's rows (each row fits one of the fixture's rows; an empty list is
// any list), and a value's JSON type. A null on either side fits.
func Fits(got, want any) error {
	return fits("", got, want)
}

func fits(path string, got, want any) error {
	if got == nil || want == nil {
		return nil
	}
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: %s, the contract has an object", pathOr(path), jsonType(got))
		}
		if len(w) == 0 {
			return nil
		}
		for _, k := range slices.Sorted(maps.Keys(g)) {
			wv, ok := w[k]
			if !ok {
				return fmt.Errorf("%s: not in the contract (it has %s)", join(path, k), strings.Join(slices.Sorted(maps.Keys(w)), ", "))
			}
			if err := fits(join(path, k), g[k], wv); err != nil {
				return err
			}
		}
		return nil
	case []any:
		g, ok := got.([]any)
		if !ok {
			return fmt.Errorf("%s: %s, the contract has a list", pathOr(path), jsonType(got))
		}
		if len(w) == 0 {
			return nil
		}
		for i, row := range g {
			var first error
			for _, wr := range w {
				err := fits(fmt.Sprintf("%s[%d]", path, i), row, wr)
				if err == nil {
					first = nil
					break
				}
				if first == nil {
					first = err
				}
			}
			if first != nil {
				return first
			}
		}
		return nil
	default:
		if jsonType(got) != jsonType(want) {
			return fmt.Errorf("%s: %s, the contract has %s", pathOr(path), jsonType(got), jsonType(want))
		}
		return nil
	}
}

func fitsOne(got any, shapes []any) error {
	var first error
	for _, s := range shapes {
		err := Fits(got, s)
		if err == nil {
			return nil
		}
		if first == nil {
			first = err
		}
	}
	return first
}

func join(path, k string) string {
	if path == "" {
		return k
	}
	return path + "." + k
}

func pathOr(path string) string {
	if path == "" {
		return "the body"
	}
	return path
}

func jsonType(v any) string {
	switch v.(type) {
	case map[string]any:
		return "an object"
	case []any:
		return "a list"
	case string:
		return "a string"
	case float64, json.Number:
		return "a number"
	case bool:
		return "a bool"
	}
	return fmt.Sprintf("%T", v)
}

// TakeContractViolations are the misfits the fake has met since the last
// take, which it then forgets: a test that sends a shape outside the
// contract on purpose takes them, so Close does not panic.
func (f *Fake) TakeContractViolations() []string {
	f.cmu.Lock()
	defer f.cmu.Unlock()
	out := f.violations
	f.violations = nil
	return out
}

func (f *Fake) violate(msg string) {
	f.cmu.Lock()
	defer f.cmu.Unlock()
	f.violations = append(f.violations, msg)
}

func (f *Fake) untakenViolations() []string {
	f.cmu.Lock()
	defer f.cmu.Unlock()
	return append([]string(nil), f.violations...)
}

var gqlRootRe = regexp.MustCompile(`^\s*(query|mutation)\b[^{]*\{\s*(\w+)\s*(?:\(([^)]*)\))?`)
var gqlArgRe = regexp.MustCompile(`(\w+)\s*:\s*\$(\w+)`)

// gqlRoot is a request's operation, its first root field and that
// field's arguments by name, from the variables they name.
func gqlRoot(q string, vars map[string]any) (op, field string, args map[string]any) {
	m := gqlRootRe.FindStringSubmatch(q)
	if m == nil {
		return "", "", nil
	}
	args = map[string]any{}
	for _, a := range gqlArgRe.FindAllStringSubmatch(m[3], -1) {
		args[a[1]] = vars[a[2]]
	}
	return m[1], m[2], args
}

// contractedGraphQL serves a GraphQL request through next, checking it and
// its answer against the field's fixtures when the field is contracted.
func (f *Fake) contractedGraphQL(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)
		op, field, args := gqlRoot(req.Query, req.Variables)
		if !slices.Contains(ContractedFields, field) {
			next(w, r)
			return
		}
		var asked, answers []any
		var names []string
		for _, x := range FieldFixtures(field) {
			if (op == "mutation") != (x.Kind == "mutation") {
				continue
			}
			req := x.request()
			if field == "finishJob" {
				if m, ok := req.(map[string]any); ok {
					in, _ := m["input"].(map[string]any)
					in = joinShapes(in, map[string]any{"finish": finishParts()}).(map[string]any)
					req = map[string]any{"input": in}
				}
			}
			asked = append(asked, withGaps(f.ahead(field), withGaps(FixtureGaps[field].Ahead, withGaps(FixtureGaps[field].Request, req))))
			answers = append(answers, withGaps(FixtureGaps[field].Answer, x.answer()))
			names = append(names, x.Name)
		}
		if len(names) == 0 && PendingFixtures[field] != "" {
			next(w, r)
			return
		}
		if len(names) == 0 {
			msg := fmt.Sprintf("gatewaytest: contract: %s %s has no fixture in specs/contracts/runsheet (scripts/contracts-pull.sh)", op, field)
			f.violate(msg)
			writeErrors(w, http.StatusInternalServerError, msg)
			return
		}
		if err := fitsOne(any(args), asked); err != nil {
			msg := fmt.Sprintf("gatewaytest: contract %s: the request's %v", strings.Join(names, "|"), err)
			f.violate(msg)
			writeErrors(w, http.StatusOK, msg)
			return
		}
		rec := httptest.NewRecorder()
		next(rec, r)
		var out struct {
			Data   map[string]any `json:"data"`
			Errors []any          `json:"errors"`
		}
		if json.Unmarshal(rec.Body.Bytes(), &out) == nil && len(out.Errors) == 0 && out.Data != nil {
			if err := fitsOne(out.Data[field], answers); err != nil {
				msg := fmt.Sprintf("gatewaytest: contract %s: the fake's answer's %v", strings.Join(names, "|"), err)
				f.violate(msg)
				writeErrors(w, http.StatusInternalServerError, msg)
				return
			}
		}
		copyRecorded(w, rec)
	}
}

// contractedREST serves a /mesh/v1 request through next, checking its JSON
// body and its JSON answer against the route's fixtures when the route is
// contracted.
func (f *Fake) contractedREST(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		i := slices.IndexFunc(ContractedRoutes, func(c Route) bool { return c.matches(r.Method, path) })
		if i < 0 {
			next(w, r)
			return
		}
		route := ContractedRoutes[i]
		xs := RouteFixtures(route)
		if len(xs) == 0 && PendingFixtures[route.String()] != "" {
			next(w, r)
			return
		}
		if len(xs) == 0 {
			msg := fmt.Sprintf("gatewaytest: contract: %s has no fixture in specs/contracts/runsheet (scripts/contracts-pull.sh)", route)
			f.violate(msg)
			http.Error(w, `{"error":`+quote(msg)+`}`, http.StatusInternalServerError)
			return
		}
		var asked, answers []any
		var names []string
		for _, x := range xs {
			asked = append(asked, x.request())
			answers = append(answers, x.answer())
			names = append(names, x.Name)
		}
		label := strings.Join(names, "|")
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		if strings.Contains(r.Header.Get("Content-Type"), "json") {
			var got any
			if err := json.Unmarshal(body, &got); err == nil {
				if err := fitsOne(got, asked); err != nil {
					msg := fmt.Sprintf("gatewaytest: contract %s: the request's %v", label, err)
					f.violate(msg)
					http.Error(w, `{"error":`+quote(msg)+`}`, http.StatusBadRequest)
					return
				}
			}
		}
		rec := httptest.NewRecorder()
		next(rec, r)
		if rec.Code < 300 {
			var statuses []int
			for _, x := range xs {
				if x.Status != 0 {
					statuses = append(statuses, x.Status)
				}
			}
			if len(statuses) > 0 && !slices.Contains(statuses, rec.Code) {
				msg := fmt.Sprintf("gatewaytest: contract %s: the fake answered %d, the contract %v", label, rec.Code, statuses)
				f.violate(msg)
				http.Error(w, `{"error":`+quote(msg)+`}`, http.StatusInternalServerError)
				return
			}
		}
		if rec.Code < 300 && rec.Body.Len() > 0 {
			var got any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err == nil {
				if err := fitsOne(got, answers); err != nil {
					msg := fmt.Sprintf("gatewaytest: contract %s: the fake's answer's %v", label, err)
					f.violate(msg)
					http.Error(w, `{"error":`+quote(msg)+`}`, http.StatusInternalServerError)
					return
				}
			}
		}
		copyRecorded(w, rec)
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func copyRecorded(w http.ResponseWriter, rec *httptest.ResponseRecorder) {
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}

// contractPanic is Close's: the violations no test took.
func contractPanic(vs []string) string {
	sort.Strings(vs)
	return "gatewaytest: the fake met shapes outside runsheet's contracts (specs/contracts/runsheet; take them with TakeContractViolations if the test means to):\n  " + strings.Join(vs, "\n  ")
}

// PublishedTools are the MCP tools runsheet's fixtures list
// (`"kind": "mcp-tools"`, one per server: mcp-tools-mesh,
// mcp-tools-runsheet; ADR 069), by name, with the server each is on.
func PublishedTools() map[string]string {
	all, _, _ := Fixtures()
	out := map[string]string{}
	for _, x := range all {
		if x.Kind != "mcp-tools" {
			continue
		}
		server, _ := x.Args["server"].(string)
		rows, _ := x.Sample.([]any)
		for _, row := range rows {
			if m, ok := row.(map[string]any); ok {
				if name, ok := m["name"].(string); ok {
					out[name] = server
				}
			}
		}
	}
	return out
}

// publishedTools is an error naming the first tool the fake lists that
// no server of runsheet's lists; nil when runsheet publishes no tool
// list yet.
func publishedTools(names []string) error {
	published := PublishedTools()
	if len(published) == 0 {
		return nil
	}
	for _, n := range names {
		if _, ok := published[n]; !ok {
			return fmt.Errorf("gatewaytest: contract mcp-tools: the fake lists %s, which no runsheet MCP server lists", n)
		}
	}
	return nil
}
