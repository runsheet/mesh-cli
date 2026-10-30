package meshapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/meshapi/meshapitest"
)

// decode reads a fixture's request into v.
func decode(t *testing.T, raw json.RawMessage, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}

// Every fixture of the contract is one client call: the request the
// client sends is the fixture's, field for field, and the answer it
// decodes says back everything the fixture's answer said — nothing the
// contract carries is dropped on either side.
func TestClientSpeaksTheContract(t *testing.T) {
	fx := meshapitest.Load(t)
	calls := map[string]func(c *meshapi.Client, f meshapitest.Fixture) (any, error){
		"nodes-register": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			var req meshapi.RegisterRequest
			decode(t, f.Request, &req)
			return c.Register(context.Background(), req)
		},
		"nodes-heartbeat": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			var req meshapi.HeartbeatRequest
			decode(t, f.Request, &req)
			return c.Heartbeat(context.Background(), idOf(f.Path, 3), req)
		},
		"nodes-heartbeat-accounts": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			var req meshapi.HeartbeatRequest
			decode(t, f.Request, &req)
			return c.Heartbeat(context.Background(), idOf(f.Path, 3), req)
		},
		"nodes-connect": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			var req meshapi.ConnectRequest
			decode(t, f.Request, &req)
			return c.Connect(context.Background(), idOf(f.Path, 3), req)
		},
		"take-lease": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			var req meshapi.TakeRequest
			decode(t, f.Request, &req)
			return c.Take(context.Background(), req)
		},
		"take-pushed": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			var req meshapi.TakeRequest
			decode(t, f.Request, &req)
			tk, err := c.Take(context.Background(), req)
			if err == nil && (tk.Brief == nil || len(tk.Brief.Pushed) != 1 || tk.Brief.Pushed[0].LeaseN != 1 || tk.Brief.Pushed[0].SHA == "") {
				t.Errorf("take-pushed: the brief's pushed %+v", tk.Brief)
			}
			// the kind's tool slots and egress (api ADR 001 amendment)
			if err == nil {
				b := tk.Brief
				if b.Tools == nil || len(b.Tools.Required) != 6 || b.Tools.Optional == nil || b.Tools.Required[0].Name != "brief" ||
					b.Tools.Required[0].From != meshapi.ToolFromProduct || b.Tools.Required[0].Description == "" || b.Tools.Required[3].From != meshapi.ToolFromMesh {
					t.Errorf("take-pushed: the brief's tools %+v", b.Tools)
				}
				if b.Egress == nil || b.Egress.Hosts == nil || len(b.Egress.Hosts) != 0 || len(b.Egress.MCP) != 1 || !b.Egress.Model {
					t.Errorf("take-pushed: the brief's egress %+v", b.Egress)
				}
			}
			// the brief carries fields this node does not read (finish,
			// a server's audience): only its pushed, tools and egress are
			// held to the fixture
			return nil, err
		},
		"take-stop-rules": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			var req meshapi.TakeRequest
			decode(t, f.Request, &req)
			tk, err := c.Take(context.Background(), req)
			if err == nil {
				b := tk.Brief
				if b == nil || len(b.StopRules) != 2 || b.StopRules[0].ToolErrorLoop == nil || b.StopRules[0].ToolErrorLoop.Count != 3 ||
					b.StopRules[0].ToolErrorLoop.WindowS != 300 || b.StopRules[1].ProtectedPaths == nil || len(b.StopRules[1].ProtectedPaths.Globs) != 1 {
					t.Errorf("take-stop-rules: the brief's stop rules %+v", b)
				}
				if b == nil || b.Transcript == nil || b.Transcript.UploadURL == "" || len(b.Transcript.Headers) != 1 || b.Transcript.ExpiresAt.IsZero() {
					t.Errorf("take-stop-rules: the brief's transcript %+v", b)
				}
			}
			// only the stop rules and the transcript are held to the fixture
			return nil, err
		},
		"take-waiting": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			var req meshapi.TakeRequest
			decode(t, f.Request, &req)
			tk, err := c.Take(context.Background(), req)
			if err == nil && (tk.Lease != nil || tk.Waiting != "no job is queued") {
				t.Errorf("take-waiting: %+v", tk)
			}
			return nil, err
		},
		"lease-renew": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			return c.Renew(context.Background(), idOf(f.Path, 3), nil)
		},
		"lease-renew-turns": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			var req meshapi.RenewRequest
			decode(t, f.Request, &req)
			return c.Renew(context.Background(), idOf(f.Path, 3), req.TurnsSeen)
		},
		"lease-renew-gone": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			_, err := c.Renew(context.Background(), idOf(f.Path, 3), nil)
			if !meshapi.LeaseGone(err) || !strings.Contains(err.Error(), "lease_not_live") {
				t.Errorf("lease-renew-gone: %v", err)
			}
			return nil, nil
		},
		"lease-session": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			var req meshapi.SessionRequest
			decode(t, f.Request, &req)
			return c.Session(context.Background(), idOf(f.Path, 3), req.JKT)
		},
		"lease-evidence": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			var req meshapi.EvidenceRequest
			decode(t, f.Request, &req)
			return c.Evidence(context.Background(), idOf(f.Path, 3), req.Records)
		},
		"lease-access": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			var req meshapi.AccessRequest
			decode(t, f.Request, &req)
			return nil, c.Access(context.Background(), idOf(f.Path, 3), req.Records)
		},
		"lease-usage": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			var req meshapi.UsageRequest
			decode(t, f.Request, &req)
			return nil, c.Usage(context.Background(), idOf(f.Path, 3), req)
		},
		"lease-usage-counts": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			var req meshapi.UsageRequest
			decode(t, f.Request, &req)
			if req.ToolErrors != 3 || len(req.Tools) != 3 || len(req.StopRules) != 1 || req.Transcript == nil {
				t.Errorf("lease-usage-counts: the counts %+v", req.Counts)
			}
			return nil, c.Usage(context.Background(), idOf(f.Path, 3), req)
		},
		"lease-fail-stopped-by-rule": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			var req meshapi.FailRequest
			decode(t, f.Request, &req)
			if req.Cause != meshapi.CauseStoppedByRule {
				t.Errorf("lease-fail-stopped-by-rule: cause %q", req.Cause)
			}
			return c.Fail(context.Background(), idOf(f.Path, 3), req)
		},
		"lease-finish": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			var req meshapi.FinishRequest
			decode(t, f.Request, &req)
			return c.Finish(context.Background(), idOf(f.Path, 3), req)
		},
		"lease-finish-evidence": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			var req meshapi.FinishRequest
			decode(t, f.Request, &req)
			return c.Finish(context.Background(), idOf(f.Path, 3), req)
		},
		"lease-fail": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			var req meshapi.FailRequest
			decode(t, f.Request, &req)
			return c.Fail(context.Background(), idOf(f.Path, 3), req)
		},
		"node-events": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			var got []meshapi.NodeEvent
			err := c.Events(context.Background(), idOf(f.Path, 3), func(e meshapi.NodeEvent) { got = append(got, e) })
			return map[string]any{"events": got}, err
		},
		"node-events-turn": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			var got []meshapi.NodeEvent
			err := c.Events(context.Background(), idOf(f.Path, 3), func(e meshapi.NodeEvent) { got = append(got, e) })
			var turn meshapi.TurnEvent
			if err == nil && (len(got) != 1 || got[0].Event != meshapi.EventTurn || json.Unmarshal(got[0].Data, &turn) != nil || turn.TurnID == "" || turn.LeaseID == "") {
				t.Errorf("node-events-turn: %+v", got)
			}
			return map[string]any{"events": got}, err
		},
		"lease-hand-back-parked": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			var req meshapi.HandBackRequest
			decode(t, f.Request, &req)
			st, err := c.HandBack(context.Background(), idOf(f.Path, 3), req)
			if err == nil && st.Status != "parked" {
				t.Errorf("lease-hand-back-parked: %+v", st)
			}
			return st, err
		},
		"lease-progress": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			var req meshapi.ProgressRequest
			decode(t, f.Request, &req)
			return c.Progress(context.Background(), idOf(f.Path, 3), req)
		},
		"lease-fail-no-finish": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			var req meshapi.FailRequest
			decode(t, f.Request, &req)
			return c.Fail(context.Background(), idOf(f.Path, 3), req)
		},
		"lease-hand-back-exit-code": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			var req meshapi.HandBackRequest
			decode(t, f.Request, &req)
			return c.HandBack(context.Background(), idOf(f.Path, 3), req)
		},
		"lease-hand-back-credential-timeout": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			var req meshapi.HandBackRequest
			decode(t, f.Request, &req)
			return c.HandBack(context.Background(), idOf(f.Path, 3), req)
		},
		"lease-hand-back-tool-unavailable": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			var req meshapi.HandBackRequest
			decode(t, f.Request, &req)
			return c.HandBack(context.Background(), idOf(f.Path, 3), req)
		},
		"lease-hand-back": func(c *meshapi.Client, f meshapitest.Fixture) (any, error) {
			var req meshapi.HandBackRequest
			decode(t, f.Request, &req)
			return c.HandBack(context.Background(), idOf(f.Path, 3), req)
		},
	}
	for name := range fx {
		if calls[name] == nil {
			t.Errorf("fixture %s has no client call", name)
		}
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			f, ok := fx[name]
			if !ok {
				t.Fatalf("no fixture %s", name)
			}
			rp := &meshapitest.Replay{}
			rp.Use(f)
			srv := httptest.NewServer(rp)
			defer srv.Close()
			c := &meshapi.Client{BaseURL: srv.URL, Token: func(context.Context) (string, error) { return "machine-token", nil }}
			got, err := call(c, f)
			if len(rp.Mismatches) > 0 {
				t.Fatalf("%s", strings.Join(rp.Mismatches, "\n"))
			}
			if err != nil {
				t.Fatal(err)
			}
			if rp.Auth[0] != "Bearer machine-token" {
				t.Fatalf("auth %q", rp.Auth[0])
			}
			if got == nil || !f.HasResponse() || f.Status >= 300 {
				return
			}
			raw, _ := json.Marshal(got)
			if same, have, want := meshapitest.SameJSON(raw, f.Response); !same {
				t.Fatalf("decoded answer says back %s, the contract has %s", have, want)
			}
		})
	}
}

// idOf is the i-th segment of a fixture's path.
func idOf(path string, i int) string {
	return strings.Split(path, "/")[i]
}

// A blob goes up as its raw bytes under its digest.
func TestPutBlob(t *testing.T) {
	var gotPath, gotType, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := new(strings.Builder)
		buf := make([]byte, 64)
		n, _ := r.Body.Read(buf)
		b.Write(buf[:n])
		gotPath, gotType, gotBody = r.Method+" "+r.URL.Path, r.Header.Get("Content-Type"), b.String()
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	c := &meshapi.Client{BaseURL: srv.URL}
	if err := c.PutBlob(context.Background(), "sha256:ab", []byte("bytes")); err != nil {
		t.Fatal(err)
	}
	if gotPath != "PUT /v1/blobs/sha256:ab" || gotType != "application/octet-stream" || gotBody != "bytes" {
		t.Fatalf("%s %s %q", gotPath, gotType, gotBody)
	}
}

// A take whose 200 names no lease is a wait, as a 204 is.
func TestTakeWithoutALeaseWaits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"waiting":"account acme-max full until 14:00"}`))
	}))
	defer srv.Close()
	c := &meshapi.Client{BaseURL: srv.URL}
	tk, err := c.Take(context.Background(), meshapi.TakeRequest{NodeID: "n", WaitS: 99})
	if err != nil || tk.Lease != nil || tk.Waiting != "account acme-max full until 14:00" {
		t.Fatalf("%+v %v", tk, err)
	}
}
