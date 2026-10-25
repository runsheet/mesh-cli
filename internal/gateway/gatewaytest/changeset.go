package gatewaytest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// changeset serves the changeset routes under /mesh/v1/items/{id}/
// (runsheet ADR 058, step 4) as gateway/changeset.go speaks them: POST
// changeset/missing answers the offered objects not yet uploaded, PUT and
// HEAD changeset/objects/{type}/{sha} upload and ask for one, and POST
// changesets records a changeset whose tree was uploaded. Without
// Changesets every route answers 404, a platform that holds none.
func (f *Fake) changeset(w http.ResponseWriter, r *http.Request) {
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if bearer != f.Token && !slices.Contains(f.ItemBearers, bearer) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Changesets {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	// items/{id}/<rest...>
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/mesh/v1/items/"), "/")
	if len(parts) < 2 || f.WorkItems[parts[0]] == nil {
		http.Error(w, `{"error":"no such item"}`, http.StatusNotFound)
		return
	}
	item := parts[0]
	f.Calls = append(f.Calls, Call{Name: "changeset " + r.Method + " " + strings.Join(parts[1:], "/"), Input: map[string]any{"aggregateId": item}})
	if f.ChangesetObjects == nil {
		f.ChangesetObjects = map[string][]byte{}
	}
	switch {
	case len(parts) == 3 && parts[1] == "changeset" && parts[2] == "missing" && r.Method == http.MethodPost:
		// the platform's shape (runsheet gateway/changeset_blobs.go):
		// {"blobs":[sha...],"trees":[sha...]} asked, the same shape answered
		// with the shas the store lacks
		var in struct {
			Blobs []string `json:"blobs"`
			Trees []string `json:"trees"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Blobs == nil || in.Trees == nil {
			http.Error(w, `{"error":"the body is {\"blobs\":[sha...],\"trees\":[sha...]}"}`, http.StatusBadRequest)
			return
		}
		out := map[string][]string{"blobs": {}, "trees": {}}
		for _, sha := range in.Blobs {
			if _, ok := f.ChangesetObjects[gateway.ObjectBlob+" "+sha]; !ok {
				out["blobs"] = append(out["blobs"], sha)
			}
		}
		for _, sha := range in.Trees {
			if _, ok := f.ChangesetObjects[gateway.ObjectTree+" "+sha]; !ok {
				out["trees"] = append(out["trees"], sha)
			}
		}
		writeJSONBody(w, out)
	case len(parts) == 5 && parts[1] == "changeset" && parts[2] == "objects":
		key := parts[3] + " " + parts[4]
		switch r.Method {
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			f.ChangesetObjects[key] = b
			// the platform's 201 (runsheet specs/contracts/mesh-changeset-object-put.json)
			w.WriteHeader(http.StatusCreated)
		case http.MethodHead:
			if _, ok := f.ChangesetObjects[key]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	case len(parts) == 2 && parts[1] == "changesets" && r.Method == http.MethodPost:
		var cs gateway.Changeset
		if err := json.NewDecoder(r.Body).Decode(&cs); err != nil {
			http.Error(w, `{"error":"bad body"}`, http.StatusBadRequest)
			return
		}
		if _, ok := f.ChangesetObjects[gateway.ObjectTree+" "+cs.Tree]; !ok && cs.Tree != cs.BaseTree {
			http.Error(w, `{"error":"tree `+cs.Tree+` was not uploaded"}`, http.StatusUnprocessableEntity)
			return
		}
		if f.RecordedChangesets == nil {
			f.RecordedChangesets = map[string][]gateway.Changeset{}
		}
		f.RecordedChangesets[item] = append(f.RecordedChangesets[item], cs)
		// the recorded row, as the platform answers it (runsheet
		// specs/contracts/mesh-changeset-record.json)
		row := map[string]any{"changeset_id": cs.ChangesetID, "item_id": item, "base_tree": cs.BaseTree, "tree": cs.Tree,
			"paths_changed": cs.PathsChanged, "made_by": "node:" + fmt.Sprint(f.WorkItems[item]["nodeId"])}
		if cs.ParentChangesetID != "" {
			row["parent_changeset_id"] = cs.ParentChangesetID
		}
		writeJSONBody(w, row)
	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}

// changesetOf is the changeset id recorded for item; f.mu is held.
func (f *Fake) changesetOf(item, id string) (gateway.Changeset, bool) {
	for _, cs := range f.RecordedChangesets[item] {
		if cs.ChangesetID == id {
			return cs, true
		}
	}
	return gateway.Changeset{}, false
}

// ChangesetsOf are the changesets recorded for item, in order.
func (f *Fake) ChangesetsOf(item string) []gateway.Changeset {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]gateway.Changeset(nil), f.RecordedChangesets[item]...)
}

func writeJSONBody(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
