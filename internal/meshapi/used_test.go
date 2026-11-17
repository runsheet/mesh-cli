package meshapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// A finish's and a fail's `used` is projectmesh-io/api #219's shape,
// {"cpu_seconds", "mem_peak_gb"}, and with none the body has no `used`
// key at all: what every fixture of contracts/v1 sends today.
func TestUsedOnTheWire(t *testing.T) {
	used := &meshapi.LeaseUsed{CPUSeconds: 38400, MemPeakGB: 11.6}
	for _, tc := range []struct {
		req  any
		want string
	}{
		{meshapi.FinishRequest{Result: json.RawMessage(`{}`)}, `{"result":{}}`},
		{meshapi.FinishRequest{Result: json.RawMessage(`{}`), Used: used}, `{"result":{},"used":{"cpu_seconds":38400,"mem_peak_gb":11.6}}`},
		{meshapi.FailRequest{Cause: "work", Reason: "red"}, `{"cause":"work","reason":"red"}`},
		{meshapi.FailRequest{Cause: "work", Reason: "red", Used: used}, `{"cause":"work","reason":"red","used":{"cpu_seconds":38400,"mem_peak_gb":11.6}}`},
	} {
		got, err := json.Marshal(tc.req)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.want {
			t.Errorf("%T: %s, want %s", tc.req, got, tc.want)
		}
	}
}

// mesh answers finish and fail. old decodes their bodies as a Mesh
// without #219 does, strictly into a shape with no `used`: a field it
// does not know is a 400 that changes nothing. refuse, set, answers
// every call 400 for another reason.
type mesh struct {
	mu     sync.Mutex
	old    bool
	refuse string
	bodies []string
}

func (m *mesh) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.bodies = append(m.bodies, string(raw))
	m.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	refuse := func(msg string) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "bad_request", "message": msg})
	}
	if m.refuse != "" {
		refuse(m.refuse)
		return
	}
	if m.old {
		var body struct {
			Result     json.RawMessage `json:"result"`
			SessionRef string          `json:"session_ref"`
			Evidence   []string        `json:"evidence"`
			Cause      string          `json:"cause"`
			Reason     string          `json:"reason"`
			ExitCode   int             `json:"exit_code"`
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			refuse("the body does not parse: " + err.Error())
			return
		}
	}
	end := "finished"
	if strings.HasSuffix(r.URL.Path, "/fail") {
		end = "failed"
	}
	json.NewEncoder(w).Encode(meshapi.JobState{JobID: "job-1", Status: "ended", End: end})
}

func (m *mesh) sent() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.bodies...)
}

func usedClient(t *testing.T, m *mesh) *meshapi.Client {
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	return &meshapi.Client{BaseURL: srv.URL, Token: func(context.Context) (string, error) { return "node", nil }}
}

// A Mesh with #219 hears `used` once; a Mesh without it refuses the
// body for it, and the finish or fail is sent again without, so the
// lease still ends; a 400 for anything else is answered as it is, never
// sent twice; and a body with no `used` is never sent twice either.
func TestFinishAndFailSendUsedOnlyWhereMeshTakesIt(t *testing.T) {
	used := &meshapi.LeaseUsed{CPUSeconds: 120.5, MemPeakGB: 2.25}
	const lease = "a8951edc-9534-5cdb-a973-7e65c5cba65e"
	ends := map[string]func(c *meshapi.Client, u *meshapi.LeaseUsed) (*meshapi.JobState, error){
		"finish": func(c *meshapi.Client, u *meshapi.LeaseUsed) (*meshapi.JobState, error) {
			return c.Finish(context.Background(), lease, meshapi.FinishRequest{Result: json.RawMessage(`{"ok":true}`), Used: u})
		},
		"fail": func(c *meshapi.Client, u *meshapi.LeaseUsed) (*meshapi.JobState, error) {
			return c.Fail(context.Background(), lease, meshapi.FailRequest{Cause: meshapi.CauseWork, Reason: "red", Used: u})
		},
	}
	for name, end := range ends {
		t.Run(name+" to a Mesh with #219", func(t *testing.T) {
			m := &mesh{}
			if _, err := end(usedClient(t, m), used); err != nil {
				t.Fatal(err)
			}
			if b := m.sent(); len(b) != 1 || !strings.Contains(b[0], `"used":{"cpu_seconds":120.5,"mem_peak_gb":2.25}`) {
				t.Fatalf("bodies %q", b)
			}
		})
		t.Run(name+" to a Mesh without #219", func(t *testing.T) {
			m := &mesh{old: true}
			st, err := end(usedClient(t, m), used)
			if err != nil || st.Status != "ended" {
				t.Fatalf("%+v %v: the lease did not end", st, err)
			}
			b := m.sent()
			if len(b) != 2 || !strings.Contains(b[0], `"used"`) || strings.Contains(b[1], `"used"`) {
				t.Fatalf("bodies %q, want one with used, then one without", b)
			}
		})
		t.Run(name+" refused for another reason", func(t *testing.T) {
			m := &mesh{refuse: "result is JSON"}
			_, err := end(usedClient(t, m), used)
			if err == nil || meshapi.UsedRefused(err) {
				t.Fatalf("err %v", err)
			}
			if b := m.sent(); len(b) != 1 {
				t.Fatalf("bodies %q, want the one", b)
			}
		})
		t.Run(name+" with no used", func(t *testing.T) {
			m := &mesh{refuse: `the body does not parse: json: unknown field "used"`}
			if _, err := end(usedClient(t, m), nil); err == nil {
				t.Fatal("a refusal answered no error")
			}
			if b := m.sent(); len(b) != 1 || strings.Contains(b[0], `"used"`) {
				t.Fatalf("bodies %q, want one, with no used", b)
			}
		})
	}
}
