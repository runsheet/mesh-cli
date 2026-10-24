package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// changesetPlatform is the platform's half of ADR 058 step 4 on
// httptest: the missing, objects and changesets routes, and a GraphQL
// endpoint whose FinishWorkItemInput takes changesetId when takes is set.
type changesetPlatform struct {
	mu       sync.Mutex
	held     map[string][]byte // "type/sha" → content
	recorded []map[string]any
	finishes []map[string]any
	asked    [][]gateway.ChangesetObject
	auth     []string
	takes    bool
}

func (p *changesetPlatform) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.auth = append(p.auth, r.Header.Get("Authorization"))
	const prefix = "/mesh/v1/items/item-1/"
	switch {
	case r.URL.Path == "/graphql":
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.Contains(body.Query, "__type") {
			fields := []map[string]string{{"name": "branch"}, {"name": "headSha"}}
			if p.takes {
				fields = append(fields, map[string]string{"name": "changesetId"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"__type": map[string]any{"inputFields": fields}}})
			return
		}
		in, _ := body.Variables["in"].(map[string]any)
		p.finishes = append(p.finishes, in)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"finishWorkItem": map[string]any{"status": "ok"}}})
	case r.URL.Path == prefix+"changeset/missing" && r.Method == http.MethodPost:
		// the platform's shape (runsheet gateway/changeset_blobs.go):
		// {"blobs":[sha...],"trees":[sha...]} both ways
		var in struct {
			Blobs []string `json:"blobs"`
			Trees []string `json:"trees"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Blobs == nil || in.Trees == nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": `the body is {"blobs":[sha...],"trees":[sha...]}`})
			return
		}
		asked := []gateway.ChangesetObject{}
		out := map[string][]string{"blobs": {}, "trees": {}}
		for _, sha := range in.Blobs {
			asked = append(asked, gateway.ChangesetObject{Type: gateway.ObjectBlob, SHA: sha})
			if _, ok := p.held["blob/"+sha]; !ok {
				out["blobs"] = append(out["blobs"], sha)
			}
		}
		for _, sha := range in.Trees {
			asked = append(asked, gateway.ChangesetObject{Type: gateway.ObjectTree, SHA: sha})
			if _, ok := p.held["tree/"+sha]; !ok {
				out["trees"] = append(out["trees"], sha)
			}
		}
		p.asked = append(p.asked, asked)
		_ = json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(r.URL.Path, prefix+"changeset/objects/"):
		key := strings.TrimPrefix(r.URL.Path, prefix+"changeset/objects/")
		switch r.Method {
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			p.held[key] = b
			w.WriteHeader(http.StatusCreated)
		case http.MethodHead:
			if _, ok := p.held[key]; !ok {
				w.WriteHeader(http.StatusNotFound)
			}
		}
	case r.URL.Path == prefix+"changesets" && r.Method == http.MethodPost:
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		if _, ok := p.held["tree/"+in["tree"].(string)]; !ok {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "tree not held"})
			return
		}
		p.recorded = append(p.recorded, in)
		w.WriteHeader(http.StatusCreated)
	default:
		http.NotFound(w, r)
	}
}

func TestChangesetRoutes(t *testing.T) {
	p := &changesetPlatform{held: map[string][]byte{"blob/aaa": []byte("x")}, takes: true}
	srv := httptest.NewServer(p)
	defer srv.Close()
	gw := gateway.New(srv.URL, "rs_test")
	ctx := nodeCtx()

	objs := []gateway.ChangesetObject{{Type: gateway.ObjectBlob, SHA: "aaa"}, {Type: gateway.ObjectBlob, SHA: "bbb"}, {Type: gateway.ObjectTree, SHA: "ttt"}}
	missing, err := gw.ChangesetMissing(ctx, "item-1", objs)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 2 || missing[0].SHA != "bbb" || missing[1].SHA != "ttt" {
		t.Fatalf("missing %+v", missing)
	}
	for _, o := range missing {
		if err := gw.PutChangesetObject(ctx, "item-1", o, []byte("content of "+o.SHA)); err != nil {
			t.Fatal(err)
		}
	}
	if string(p.held["tree/ttt"]) != "content of ttt" {
		t.Fatalf("held %q", p.held)
	}
	if has, err := gw.HasChangesetObject(ctx, "item-1", gateway.ChangesetObject{Type: gateway.ObjectBlob, SHA: "bbb"}); err != nil || !has {
		t.Fatalf("head bbb: %v %v", has, err)
	}
	if has, err := gw.HasChangesetObject(ctx, "item-1", gateway.ChangesetObject{Type: gateway.ObjectBlob, SHA: "zzz"}); err != nil || has {
		t.Fatalf("head zzz: %v %v", has, err)
	}
	if err := gw.PutChangesetObject(ctx, "item-1", gateway.ChangesetObject{Type: "commit", SHA: "c"}, nil); err == nil {
		t.Fatal("put a commit object: no error")
	}

	cs := gateway.Changeset{ChangesetID: "cs-1", BaseTree: "base", Tree: "ttt", PathsChanged: []string{"a.go"}, ParentChangesetID: "cs-0"}
	if err := gw.RecordChangeset(ctx, "item-1", cs); err != nil {
		t.Fatal(err)
	}
	got := p.recorded[0]
	if got["changeset_id"] != "cs-1" || got["base_tree"] != "base" || got["tree"] != "ttt" || got["parent_changeset_id"] != "cs-0" {
		t.Fatalf("recorded %+v", got)
	}
	if paths, _ := got["paths_changed"].([]any); len(paths) != 1 || paths[0] != "a.go" {
		t.Fatalf("paths_changed %+v", got["paths_changed"])
	}
	// a first changeset names no parent
	if err := gw.RecordChangeset(ctx, "item-1", gateway.Changeset{ChangesetID: "cs-2", BaseTree: "base", Tree: "ttt"}); err != nil {
		t.Fatal(err)
	}
	if _, sent := p.recorded[1]["parent_changeset_id"]; sent {
		t.Fatalf("first changeset sent a parent: %+v", p.recorded[1])
	}
	// a tree the platform lacks is refused, not unsupported
	err = gw.RecordChangeset(ctx, "item-1", gateway.Changeset{ChangesetID: "cs-3", BaseTree: "base", Tree: "nope"})
	if !gateway.IsRefusal(err) || gateway.IsUnsupported(err) || !strings.Contains(err.Error(), "tree not held") {
		t.Fatalf("record with a missing tree: %v", err)
	}

	if err := gw.FinishWorkItemChangeset(ctx, "item-1", "mesh/g-1", "cs-1", 12, nil); err != nil {
		t.Fatal(err)
	}
	// the finish is the build job's (finishJob, runsheet ADR 061): the
	// older finishWorkItem's input as its changeset
	in := p.finishes[0]
	subject, _ := in["subject"].(map[string]any)
	finish, _ := in["finish"].(map[string]any)
	fin, _ := finish["changeset"].(map[string]any)
	if in["kind"] != "runsheet/build" || subject["id"] != "item-1" || fin["changesetId"] != "cs-1" || fin["branch"] != "mesh/g-1" {
		t.Fatalf("finish %+v", in)
	}
	if _, sent := fin["headSha"]; sent {
		t.Fatalf("changeset finish sent a head sha: %+v", fin)
	}
	for _, a := range p.auth {
		if a != "Bearer rs_test" {
			t.Fatalf("request on %q", a)
		}
	}
}

// A platform without the routes, or whose FinishWorkItemInput takes no
// changesetId, answers IsUnsupported: the node falls back to its push.
func TestChangesetUnsupported(t *testing.T) {
	srv := httptest.NewServer(&changesetPlatform{held: map[string][]byte{}})
	defer srv.Close()
	old := httptest.NewServer(http.NotFoundHandler())
	defer old.Close()
	ctx := nodeCtx()

	gw := gateway.New(old.URL, "rs_test")
	if _, err := gw.ChangesetMissing(ctx, "item-1", nil); !gateway.IsUnsupported(err) {
		t.Fatalf("missing on an old platform: %v", err)
	}
	if err := gw.PutChangesetObject(ctx, "item-1", gateway.ChangesetObject{Type: gateway.ObjectBlob, SHA: "a"}, nil); !gateway.IsUnsupported(err) {
		t.Fatalf("put on an old platform: %v", err)
	}
	if err := gw.RecordChangeset(ctx, "item-1", gateway.Changeset{ChangesetID: "c", BaseTree: "b", Tree: "t"}); !gateway.IsUnsupported(err) {
		t.Fatalf("record on an old platform: %v", err)
	}

	gw = gateway.New(srv.URL, "rs_test")
	if err := gw.FinishWorkItemChangeset(ctx, "item-1", "b", "cs-1", 0, nil); !gateway.IsUnsupported(err) {
		t.Fatalf("finish without changesetId: %v", err)
	}
}
