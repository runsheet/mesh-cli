package gatewaytest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every contracted field and route has a pulled fixture, or is pending
// one; a pending one that a pull has since brought fails, so the entry
// goes and the check starts.
func TestContractedRoutesHaveFixtures(t *testing.T) {
	_, from, err := Fixtures()
	if err != nil {
		t.Fatalf("pulled fixtures: %v", err)
	}
	t.Logf("fixtures pulled from runsheet %s", from)
	check := func(key string, n int) {
		_, pending := PendingFixtures[key]
		switch {
		case n == 0 && !pending:
			t.Errorf("%s: no fixture in specs/contracts/runsheet; run scripts/contracts-pull.sh", key)
		case n > 0 && pending:
			t.Errorf("%s: runsheet publishes a fixture now; remove it from PendingFixtures", key)
		}
	}
	for _, field := range ContractedFields {
		check(field, len(FieldFixtures(field)))
	}
	for _, r := range ContractedRoutes {
		check(r.String(), len(RouteFixtures(r)))
	}
	for key := range PendingFixtures {
		known := false
		for _, f := range ContractedFields {
			known = known || f == key
		}
		for _, r := range ContractedRoutes {
			known = known || r.String() == key
		}
		if !known {
			t.Errorf("PendingFixtures names %s, which is not contracted", key)
		}
	}
}

func TestFits(t *testing.T) {
	want := map[string]any{
		"blobs": []any{"sha"},
		"usage": map[string]any{},
		"rows":  []any{map[string]any{"kind": "a", "n": 1.0}, map[string]any{"kind": "b", "s": "x"}},
	}
	for _, c := range []struct {
		name, got, err string
	}{
		{"fewer keys", `{"blobs":[]}`, ""},
		{"an empty object is any object", `{"usage":{"anything":1}}`, ""},
		{"a row fits one of the rows", `{"rows":[{"kind":"a","n":2},{"kind":"b","s":"y"}]}`, ""},
		{"null fits", `{"blobs":null}`, ""},
		{"a key the contract lacks", `{"objects":[]}`, "objects: not in the contract (it has blobs, rows, usage)"},
		{"a wrong type", `{"blobs":"sha"}`, "blobs: a string, the contract has a list"},
		{"a row's wrong type", `{"blobs":[1]}`, "blobs[0]: a number, the contract has a string"},
		{"a row that fits no row", `{"rows":[{"kind":"a","s":"x","n":1}]}`, "rows[0].s: not in the contract (it has kind, n)"},
	} {
		var got any
		if err := json.Unmarshal([]byte(c.got), &got); err != nil {
			t.Fatal(err)
		}
		err := Fits(got, want)
		if (err == nil) != (c.err == "") || err != nil && err.Error() != c.err {
			t.Errorf("%s: Fits = %v, want %q", c.name, err, c.err)
		}
	}
}

func post(t *testing.T, url, token, contentType, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return resp.StatusCode, b.String()
}

func takeOne(t *testing.T, f *Fake, want string) {
	t.Helper()
	vs := f.TakeContractViolations()
	if len(vs) != 1 || !strings.Contains(vs[0], want) {
		t.Fatalf("violations = %q, want one naming %q", vs, want)
	}
}

// The changeset wire mismatch #1511 fixed: the node asked
// changeset/missing with {"objects":[{type, sha}]} and read
// {"missing":[...]}, the platform takes and answers {"blobs", "trees"}.
// The fake refuses the old request, and an answer of the old shape fails.
func TestChangesetMissingRefusesThePre1511Shape(t *testing.T) {
	route := Route{http.MethodPost, "/mesh/v1/items/{id}/changeset/missing"}
	if len(RouteFixtures(route)) == 0 {
		if why := PendingFixtures[route.String()]; why != "" {
			t.Skipf("%s: no fixture yet (%s)", route, why)
		}
		t.Fatalf("%s: no fixture", route)
	}
	const item = "00000000-0000-4000-8000-0000000c0001"
	f := New("rs_test")
	defer f.Close()
	f.Changesets = true
	f.WorkItems[item] = map[string]any{"id": item}
	url := f.URL() + "/mesh/v1/items/" + item + "/changeset/missing"

	code, body := post(t, url, f.Token, "application/json", `{"objects":[{"type":"blob","sha":"3b18e512dba79e4c8300dd08aeb37f8e728b8dad"}]}`)
	if code != http.StatusBadRequest {
		t.Errorf("the pre-#1511 request answered %d %s, want 400", code, body)
	}
	takeOne(t, f, "the request's objects: not in the contract")

	code, body = post(t, url, f.Token, "application/json", `{"blobs":["3b18e512dba79e4c8300dd08aeb37f8e728b8dad"],"trees":[]}`)
	if code != http.StatusOK || !strings.Contains(body, `"blobs":["3b18e512dba79e4c8300dd08aeb37f8e728b8dad"]`) {
		t.Errorf("the contract's request answered %d %s", code, body)
	}
	if vs := f.TakeContractViolations(); len(vs) != 0 {
		t.Errorf("the contract's request met violations %q", vs)
	}

	// a fake still answering the pre-#1511 shape fails its test
	old := f.contractedREST(func(w http.ResponseWriter, _ *http.Request) {
		writeJSONBody(w, map[string]any{"missing": []map[string]any{{"type": "blob", "sha": "3b18e512dba79e4c8300dd08aeb37f8e728b8dad"}}})
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(`{"blobs":[],"trees":[]}`))
	req.Header.Set("Content-Type", "application/json")
	old(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("the pre-#1511 answer passed: %d %s", rec.Code, rec.Body)
	}
	takeOne(t, f, "the fake's answer's missing: not in the contract")
}

// The planning loop of 2026-10-24: the node sent each item's acceptance
// checks as prose where runsheet's planGoal takes {kind, check,
// expected} objects. The fake refuses that request.
func TestPlanGoalRefusesProseAcceptanceChecks(t *testing.T) {
	f := New("rs_test")
	defer f.Close()
	q := `mutation($in: PlanGoalInput!) { planGoal(input: $in) { status } }`
	in := map[string]any{
		"aggregateId": "9d31d5af-965a-558e-9f91-82a9fad3d2d9", "namespace": "runsheet", "by": "node",
		"items": []any{map[string]any{"seq": 1, "title": "t", "brief": "b", "acceptance": "a",
			"acceptanceChecks": "go test ./... passes"}},
	}
	body, _ := json.Marshal(map[string]any{"query": q, "variables": map[string]any{"in": in}})
	_, got := post(t, f.URL()+"/graphql", f.Token, "application/json", string(body))
	if !strings.Contains(got, "acceptanceChecks: a string, the contract has a list") {
		t.Errorf("planGoal with prose checks answered %s", got)
	}
	takeOne(t, f, "input.items[0].acceptanceChecks: a string, the contract has a list")
}

// An answer outside claimJob's fixture fails, as a fake that grew a field
// the platform does not answer would.
func TestClaimJobAnswerOutsideTheContractFails(t *testing.T) {
	f := New("rs_test")
	defer f.Close()
	h := f.contractedGraphQL(func(w http.ResponseWriter, _ *http.Request) {
		writeData(w, map[string]any{"claimJob": map[string]any{"status": "claimed", "lease": "60s"}})
	})
	body, _ := json.Marshal(map[string]any{"query": `mutation($in: Map!) { claimJob(input: $in) }`,
		"variables": map[string]any{"in": map[string]any{"kind": "runsheet/build", "nodeId": "n", "harness": "claude-code"}}})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body)))
	if !strings.Contains(rec.Body.String(), "lease: not in the contract") {
		t.Errorf("claimJob's extra answer field passed: %s", rec.Body)
	}
	takeOne(t, f, "claimJob: the fake's answer's lease: not in the contract")
}

// A gap is a key runsheet reads that its fixture does not show; once a
// pulled fixture shows it, the entry goes.
func TestFixtureGapsAreStillGaps(t *testing.T) {
	for field, gaps := range FixtureGaps {
		xs := FieldFixtures(field)
		if len(xs) == 0 {
			t.Errorf("FixtureGaps names %s, which has no fixture", field)
		}
		check := func(paths map[string]string, of func(Fixture) any) {
			for path := range paths {
				for _, x := range xs {
					var shape any = of(x)
					if shown(shape, strings.Split(path, ".")) {
						t.Errorf("%s: %s's fixture shows %s now; remove it from FixtureGaps", field, x.Name, path)
					}
				}
			}
		}
		check(gaps.Request, Fixture.request)
		check(gaps.Answer, Fixture.answer)
		check(gaps.Ahead, Fixture.request)
	}
}

// shown is whether the shape has a value at the path ("x[]" any row of x).
func shown(shape any, keys []string) bool {
	if len(keys) == 0 {
		return shape != nil
	}
	m, _ := shape.(map[string]any)
	if keys[0] == "*" {
		for _, v := range m {
			if shown(v, keys[1:]) {
				return true
			}
		}
		return false
	}
	if key, ok := strings.CutSuffix(keys[0], "[]"); ok {
		rows, _ := m[key].([]any)
		for _, row := range rows {
			if shown(row, keys[1:]) {
				return true
			}
		}
		return false
	}
	return shown(m[keys[0]], keys[1:])
}

// Every tool the fake's /mcp lists is one a runsheet MCP server lists.
func TestFakeListsOnlyPublishedTools(t *testing.T) {
	if len(PublishedTools()) == 0 {
		t.Fatal("no mcp-tools fixture pulled")
	}
	if err := publishedTools(append(append([]string(nil), contextTools...), hostedWorkTools...)); err != nil {
		t.Error(err)
	}
	if err := publishedTools([]string{"mesh_document", "mesh_retired_tool"}); err == nil || !strings.Contains(err.Error(), "mesh_retired_tool") {
		t.Errorf("an unpublished tool passed: %v", err)
	}
}

// The node's usage keys runsheet's UsageReport lacks are named, and only
// those: a key runsheet has is checked as runsheet's.
func TestUsageAheadIsWhatRunsheetLacks(t *testing.T) {
	ahead := usageAhead()
	for _, k := range []string{"validation", "tools", "toolNames", "documentsFetched", "turns"} {
		if _, ok := ahead["input.finish.*.usage."+k]; !ok {
			t.Errorf("usageAhead lacks %s", k)
		}
	}
	for _, k := range []string{"inputTokens", "sessionId", "wallMs"} {
		if _, ok := ahead["input.finish.*.usage."+k]; ok {
			t.Errorf("usageAhead names %s, which runsheet's UsageReport has", k)
		}
	}
}
