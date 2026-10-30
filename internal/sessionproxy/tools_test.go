package sessionproxy

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

// productTools is a product MCP server with three tools, answering
// tools/list as JSON or, with sse, as one event of a stream, and
// recording the tools it was asked to call.
func productTools(t *testing.T, sse bool) (*httptest.Server, *[]string) {
	t.Helper()
	var called []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m rpc
		_ = json.Unmarshal(b, &m)
		var answer string
		switch m.Method {
		case "tools/list":
			answer = `{"jsonrpc":"2.0","id":1,"result":{"tools":[` +
				`{"name":"brief","description":"the product's own words","inputSchema":{"type":"object"}},` +
				`{"name":"submit","description":"submit it","inputSchema":{"type":"object"}},` +
				`{"name":"admin_delete","description":"deletes everything","inputSchema":{"type":"object"}}]}}`
		case "tools/call":
			called = append(called, m.Params.Name)
			answer = `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"ok"}]}}`
		default:
			answer = `{"jsonrpc":"2.0","id":0,"result":{}}`
		}
		if sse {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message\ndata: "+answer+"\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(srv.Close)
	return srv, &called
}

func listedTools(t *testing.T, body string) map[string]string {
	t.Helper()
	if i := strings.Index(body, "data: "); i >= 0 {
		body = strings.TrimSpace(body[i+len("data: "):])
	}
	var m struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("the list %q: %v", body, err)
	}
	out := map[string]string{}
	for _, tl := range m.Result.Tools {
		out[tl.Name] = tl.Description
	}
	return out
}

// A kind that names its tools sees exactly those of its product's
// server, under the kind's descriptions, and a call of any other is
// refused before it reaches the product.
func TestProductToolsAreFilteredToTheKindsNamesAndDescriptions(t *testing.T) {
	for _, sse := range []bool{false, true} {
		name := "json"
		if sse {
			name = "sse"
		}
		t.Run(name, func(t *testing.T) {
			srv, called := productTools(t, sse)
			bearer := func(context.Context) (string, error) { return "tok", nil }
			p := New(map[string]Upstream{"conform": Product{URL: srv.URL, Bearer: bearer}})
			p.Tools = ToolFilter{"brief": "Call first.", "submit": "Call last."}
			c, _ := listen(t, p)

			_, body := post(t, c, "conform", nil, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
			if got, want := listedTools(t, body), map[string]string{"brief": "Call first.", "submit": "Call last."}; !reflect.DeepEqual(got, want) {
				t.Fatalf("tools/list = %v, want %v", got, want)
			}
			if got, want := p.ServedTools(), map[string][]string{"conform": {"brief", "submit"}}; !reflect.DeepEqual(got, want) {
				t.Errorf("ServedTools = %v, want %v", got, want)
			}

			_, body = post(t, c, "conform", nil, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"admin_delete","arguments":{}}}`)
			if !strings.Contains(body, `"id":7`) || !strings.Contains(body, "not served to this session") {
				t.Errorf("a hidden tool's call answered %s", body)
			}
			_, body = post(t, c, "conform", nil, `[{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"admin_delete"}}]`)
			if !strings.Contains(body, "one message at a time") {
				t.Errorf("a batch holding a call answered %s", body)
			}
			_, body = post(t, c, "conform", nil, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"submit","arguments":{}}}`)
			if !strings.Contains(body, `"ok"`) {
				t.Errorf("a served tool's call answered %s", body)
			}
			if want := []string{"submit"}; !reflect.DeepEqual(*called, want) {
				t.Errorf("the product was asked to call %v, want %v", *called, want)
			}
		})
	}
}

// Without a filter the product's tools are served as it lists them.
func TestProductToolsUnfilteredWithoutAFilter(t *testing.T) {
	srv, called := productTools(t, false)
	bearer := func(context.Context) (string, error) { return "tok", nil }
	p := New(map[string]Upstream{"conform": Product{URL: srv.URL, Bearer: bearer}})
	c, _ := listen(t, p)
	_, body := post(t, c, "conform", nil, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if got := listedTools(t, body); len(got) != 3 || got["brief"] != "the product's own words" {
		t.Errorf("tools/list = %v", got)
	}
	post(t, c, "conform", nil, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"admin_delete"}}`)
	if want := []string{"admin_delete"}; !reflect.DeepEqual(*called, want) {
		t.Errorf("called %v, want %v", *called, want)
	}
}

// A slot's server (ADR 001 amendment, slot bindings) is held to its one
// tool under the kind's description, whatever the connection behind
// Latchkey lists, and it goes on the session token as a product's
// server does; the product's own tool filter does not reach it.
func TestASlotIsServedUnderTheKindsNameAndDescription(t *testing.T) {
	var auth []string
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		b, _ := io.ReadAll(r.Body)
		var m rpc
		_ = json.Unmarshal(b, &m)
		w.Header().Set("Content-Type", "application/json")
		switch m.Method {
		case "tools/list":
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[`+
				`{"name":"web_search","description":"the vendor's own words","inputSchema":{"type":"object"}},`+
				`{"name":"extra","description":"never bound","inputSchema":{"type":"object"}}]}}`)
		default:
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"found"}]}}`)
		}
	}))
	t.Cleanup(vendor.Close)
	product, _ := productTools(t, false)
	bearer := func(context.Context) (string, error) { return "session-tok", nil }
	p := New(map[string]Upstream{
		"conform":    Product{URL: product.URL, Bearer: bearer},
		"web_search": Slot{URL: vendor.URL, Bearer: bearer, Tool: "web_search", Description: "Search the web."},
	})
	p.Tools = ToolFilter{"brief": "Call first."}
	c, _ := listen(t, p)

	_, body := post(t, c, "web_search", nil, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if got, want := listedTools(t, body), map[string]string{"web_search": "Search the web."}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the slot's tools/list = %v, want %v", got, want)
	}
	_, body = post(t, c, "web_search", nil, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"extra"}}`)
	if !strings.Contains(body, "not served to this session") {
		t.Errorf("another tool of the slot's server answered %s", body)
	}
	_, body = post(t, c, "web_search", nil, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"web_search","arguments":{"q":"x"}}}`)
	if !strings.Contains(body, "found") {
		t.Errorf("the slot's call answered %s", body)
	}
	if got, want := p.ServedTools(), map[string][]string{"web_search": {"web_search"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("ServedTools = %v, want %v", got, want)
	}
	for _, a := range auth {
		if a != "Bearer session-tok" {
			t.Errorf("the slot's upstream was reached on %q", a)
		}
	}
	if len(auth) != 2 {
		t.Errorf("the slot's upstream saw %d requests, want 2 (the refused call never left)", len(auth))
	}
}
