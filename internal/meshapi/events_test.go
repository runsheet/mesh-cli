package meshapi_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// The event stream: comments are skipped, an event's data lines join,
// each event reaches the handler in order, the stream's end answers nil
// and a refusal is an *Error.
func TestEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/nodes/gone/events" {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"error":"forbidden","message":"not this machine's node"}`)
			return
		}
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("accept %q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": open\n\nevent: cancel\ndata: {\"lease_id\":\"l1\",\ndata: \"cause\":\"cancelled\"}\n\n: keep-alive\n\nevent: drain\ndata: {}\n\n")
	}))
	defer srv.Close()
	c := &meshapi.Client{BaseURL: srv.URL}
	var got []meshapi.NodeEvent
	if err := c.Events(context.Background(), "n1", func(e meshapi.NodeEvent) { got = append(got, e) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Event != "cancel" || string(got[0].Data) != "{\"lease_id\":\"l1\",\n\"cause\":\"cancelled\"}" || got[1].Event != "drain" {
		t.Fatalf("events %+v", got)
	}
	err := c.Events(context.Background(), "gone", func(meshapi.NodeEvent) {})
	if e, ok := err.(*meshapi.Error); !ok || e.Status != http.StatusForbidden || e.Code != "forbidden" {
		t.Fatalf("a refused stream: %v", err)
	}
}
