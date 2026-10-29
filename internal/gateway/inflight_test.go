package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// The heartbeat's inFlight (trd §6a; runsheet ADR 079): the five kinds
// once HeartbeatNodeInput takes `inFlight`, validating and
// validationSlots each only when NodeInFlightInput takes it, and nothing
// to a gateway without inFlight — an older platform refuses the whole
// heartbeat on a field it does not know.
func TestHeartbeatSendsInFlight(t *testing.T) {
	slots := 3
	f := InFlight{Building: 2, Evaluating: 1, Planning: 1, Researching: 0, Rebasing: 1, Validating: 2, ValidationSlots: &slots}
	kinds := map[string]any{"building": 2.0, "evaluating": 1.0, "planning": 1.0, "researching": 0.0, "rebasing": 1.0}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range kinds {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	for _, tc := range []struct {
		name      string
		heartbeat []string
		inFlight  []string
		sent      InFlight
		want      map[string]any
	}{
		{name: "no inFlight", heartbeat: []string{"version"}, sent: f, want: nil},
		{name: "the five kinds", heartbeat: []string{"version", "inFlight"}, inFlight: []string{"building", "evaluating", "planning", "researching", "rebasing"}, sent: f, want: kinds},
		{name: "validating", heartbeat: []string{"version", "inFlight"}, inFlight: []string{"building", "evaluating", "planning", "researching", "rebasing", "validating"}, sent: f,
			want: with(map[string]any{"validating": 2.0})},
		{name: "validating and slots", heartbeat: []string{"version", "inFlight"}, inFlight: []string{"building", "evaluating", "planning", "researching", "rebasing", "validating", "validationSlots"}, sent: f,
			want: with(map[string]any{"validating": 2.0, "validationSlots": 3.0})},
		{name: "slots unjudged", heartbeat: []string{"version", "inFlight"}, inFlight: []string{"building", "evaluating", "planning", "researching", "rebasing", "validating", "validationSlots"},
			sent: InFlight{Building: 2, Evaluating: 1, Planning: 1, Rebasing: 1, Validating: 2}, want: with(map[string]any{"validating": 2.0})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields := func(names []string) string {
				var parts []string
				for _, n := range names {
					parts = append(parts, `{"name":"`+n+`"}`)
				}
				return `{"data":{"__type":{"inputFields":[` + strings.Join(parts, ",") + `]}}}`
			}
			var beat map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				var body map[string]any
				_ = json.Unmarshal(raw, &body)
				q, _ := body["query"].(string)
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(q, "__type") {
					vars, _ := body["variables"].(map[string]any)
					switch {
					case vars["t"] == "HeartbeatNodeInput":
						_, _ = io.WriteString(w, fields(tc.heartbeat))
					case vars["t"] == "NodeInFlightInput" && tc.inFlight != nil:
						_, _ = io.WriteString(w, fields(tc.inFlight))
					default:
						_, _ = io.WriteString(w, `{"data":{"__type":null}}`)
					}
					return
				}
				beat, _ = body["variables"].(map[string]any)["in"].(map[string]any)
				_, _ = io.WriteString(w, `{"data":{"heartbeatNode":{"status":"ok"}}}`)
			}))
			defer srv.Close()
			if _, err := New(srv.URL, "rs_test").HeartbeatNode(context.Background(), "n-1", "v1", 4, HeartbeatExtras{InFlight: &tc.sent}); err != nil {
				t.Fatal(err)
			}
			got, sent := beat["inFlight"].(map[string]any)
			if tc.want == nil {
				if _, ok := beat["inFlight"]; ok {
					t.Fatalf("a gateway without inFlight was sent it: %v", beat)
				}
				return
			}
			if !sent || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("inFlight %v, want %v", beat["inFlight"], tc.want)
			}
		})
	}
}
