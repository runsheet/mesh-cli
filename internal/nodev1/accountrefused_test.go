package nodev1

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// refusingModel answers every model call with status and body.
func refusingModel(t *testing.T, status int, body string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// logLines collects a runtime's log lines.
type logLines struct {
	t     *testing.T
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	l.t.Log(line)
	l.mu.Lock()
	l.lines = append(l.lines, line)
	l.mu.Unlock()
}

func (l *logLines) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// refusedRig is a rig whose session calls its model once, through the
// relay, as Claude Code would, and then submits; the model answers
// status and body.
func refusedRig(t *testing.T, status int, body string) (*rig, *logLines) {
	t.Helper()
	clearModelEnv(t)
	model := refusingModel(t, status, body)
	sess := &stubSession{calls: []string{"conform/brief", "conform/submit"}, result: SessionResult{SessionID: "s"}}
	sess.inspect = func(spec SessionSpec) {
		callModel(envValue(spec.Env, "ANTHROPIC_BASE_URL"), "/v1/messages", "Bearer "+envValue(spec.Env, "CLAUDE_CODE_OAUTH_TOKEN"))
	}
	g := newRig(t, sess)
	logs := &logLines{t: t}
	g.rt.Logf = logs.logf
	g.rt.ModelUpstreams = map[string]string{"anthropic": model.URL}
	return g, logs
}

// A 403 from the vendor on the job's account (ADR-048): the node logs the
// vendor's error type and message, the account's secret in it redacted,
// ends the session at once and hands the lease back account_refused,
// naming the account and the vendor's answer — never finishing it.
func TestAnAccountRefusedIsHandedBack(t *testing.T) {
	body := `{"type":"error","error":{"type":"permission_error","message":"OAuth token ` + fakeModelSecret + ` is not allowed for this organization."}}`
	g, logs := refusedRig(t, http.StatusForbidden, body)
	job := g.offer(t, nil)
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	hb := onlyHandBack(t, g, job)
	if hb.Cause != meshapi.CauseAccountRefused || hb.Account == nil {
		t.Fatalf("the hand-back %+v", hb)
	}
	if a := hb.Account; a.AccountRef != "acct-fake" || a.Vendor != "anthropic" || a.Status != http.StatusForbidden || a.ErrorType != "permission_error" ||
		!strings.Contains(a.ErrorMessage, "is not allowed for this organization") {
		t.Fatalf("the refused account %+v", a)
	}
	raw, _ := json.Marshal(hb)
	if strings.Contains(string(raw), fakeModelSecret) || strings.Contains(logs.all(), fakeModelSecret) {
		t.Fatal("the account's secret reached the hand-back or the log")
	}
	if !strings.Contains(logs.all(), "answered 403") || !strings.Contains(logs.all(), ": permission_error: OAuth token") {
		t.Fatalf("the log does not name the vendor's error:\n%s", logs.all())
	}
}

// Not a refusal (ADR-048): a 429 (the account's limits), a 529 (the
// vendor overloaded) and a 400 (the request's own) leave the session to
// carry on; the lease finishes, and the log still names the vendor's
// error.
func TestOtherModelErrorsAreNotRefusals(t *testing.T) {
	for _, tc := range []struct {
		status int
		typ    string
	}{{http.StatusTooManyRequests, "rate_limit_error"}, {529, "overloaded_error"}, {http.StatusBadRequest, "invalid_request_error"}} {
		t.Run(tc.typ, func(t *testing.T) {
			g, logs := refusedRig(t, tc.status, `{"type":"error","error":{"type":"`+tc.typ+`","message":"try again"}}`)
			g.offer(t, nil)
			if err := run(t, g, context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(g.fake.Calls("hand-back")) != 0 || len(g.fake.Calls("finish")) != 1 {
				t.Fatalf("hand-backs %d, finishes %d", len(g.fake.Calls("hand-back")), len(g.fake.Calls("finish")))
			}
			if want := fmt.Sprintf("answered %d", tc.status); !strings.Contains(logs.all(), want) || !strings.Contains(logs.all(), tc.typ+": try again") {
				t.Fatalf("the log does not name the vendor's error:\n%s", logs.all())
			}
		})
	}
}

// A Mesh older than account_refused answers its hand-back 422: the node
// hands back cannot_run with the same reason, as it did before.
func TestAnOlderMeshHearsCannotRun(t *testing.T) {
	g, _ := refusedRig(t, http.StatusUnauthorized, `{"type":"error","error":{"type":"authentication_error","message":"invalid bearer token"}}`)
	g.fake.RefuseHandBack = func(req meshapi.HandBackRequest) int {
		if req.Cause == meshapi.CauseAccountRefused {
			return http.StatusUnprocessableEntity
		}
		return 0
	}
	g.offer(t, nil)
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := g.fake.Calls("hand-back")
	if len(calls) != 2 {
		t.Fatalf("hand-backs %+v", calls)
	}
	var hb meshapi.HandBackRequest
	json.Unmarshal(calls[1].Body, &hb)
	if hb.Cause != meshapi.CauseCannotRun || !strings.Contains(hb.Reason, "authentication_error: invalid bearer token") || hb.Account != nil {
		t.Fatalf("the second hand-back %+v", hb)
	}
}
