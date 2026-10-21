package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The heartbeat reports the node's sessions once the gateway's
// HeartbeatNodeInput takes `sessions`, asks the answer for `stop`
// beside it, and reads the stop list; a gateway without `sessions` is
// sent neither, and none is asked of registerNode.
func TestHeartbeatSendsSessionsAndReadsStop(t *testing.T) {
	for _, takes := range []bool{true, false} {
		var mu sync.Mutex
		var beats []map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			q, _ := body["query"].(string)
			w.Header().Set("Content-Type", "application/json")
			if strings.Contains(q, "__type") {
				vars, _ := body["variables"].(map[string]any)
				if takes && vars["t"] == "HeartbeatNodeInput" {
					_, _ = io.WriteString(w, `{"data":{"__type":{"inputFields":[{"name":"version"},{"name":"sessions"}]}}}`)
					return
				}
				_, _ = io.WriteString(w, `{"data":{"__type":null}}`)
				return
			}
			mu.Lock()
			beats = append(beats, body)
			mu.Unlock()
			answer := `"status":"ok","nodeId":"n-1","owner":{"kind":"person","id":"p","name":"Pat"},"scopes":[{"orgId":"o-1","login":"acme","slug":"acme","accountId":"a-1","cap":0}]`
			if strings.Contains(q, "stop {") {
				answer += `,"stop":[{"itemId":"i-1","kind":"build","sessionId":"","reason":"connection revoked"}]`
			}
			_, _ = io.WriteString(w, `{"data":{"heartbeatNode":{`+answer+`}}}`)
		}))
		gw := New(srv.URL, "rs_test")
		a, err := gw.HeartbeatNode(context.Background(), "n-1", "v1", 1, HeartbeatExtras{Sessions: []NodeSession{{ItemID: "i-1", Kind: "build", Since: "2026-10-21T00:00:00Z"}}})
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		if len(beats) != 1 {
			t.Fatalf("takes %v: %d heartbeats", takes, len(beats))
		}
		q, _ := beats[0]["query"].(string)
		in, _ := beats[0]["variables"].(map[string]any)["in"].(map[string]any)
		_, sent := in["sessions"]
		mu.Unlock()
		if takes {
			if !sent || !strings.Contains(q, "stop { itemId kind sessionId reason }") {
				t.Fatalf("sessions not sent or stop not asked: %s %v", q, in)
			}
			if a == nil || len(a.Stop) != 1 || a.Stop[0].ItemID != "i-1" || a.Stop[0].Reason != "connection revoked" {
				t.Fatalf("the stop list: %+v", a)
			}
		} else if sent || strings.Contains(q, "stop {") {
			t.Fatalf("a gateway without sessions was sent them or asked for stop: %s %v", q, in)
		}
	}
}
