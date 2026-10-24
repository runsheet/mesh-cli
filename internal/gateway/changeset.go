package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// An item's change is a changeset the platform holds (runsheet ADR 058,
// step 4): the node computes its workspace's tree outside the pod, asks
// which of its objects the platform lacks, uploads only those, records
// the changeset and finishes the item naming it. The platform makes the
// branch from the changeset; nothing pushes from the job container.

// Object kinds a changeset carries.
const (
	ObjectBlob = "blob"
	ObjectTree = "tree"
)

// ChangesetObject names one git object by kind and sha.
type ChangesetObject struct {
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

// Changeset is what POST /mesh/v1/items/{id}/changesets records: the
// node-minted id, the base tree the item started from, the tree it ends
// on, the paths that differ, and the item's previous changeset on a
// rework ("" on the first).
type Changeset struct {
	ChangesetID       string   `json:"changeset_id"`
	BaseTree          string   `json:"base_tree"`
	Tree              string   `json:"tree"`
	PathsChanged      []string `json:"paths_changed"`
	ParentChangesetID string   `json:"parent_changeset_id,omitempty"`
}

// ChangesetIDField is FinishWorkItemInput's key naming the recorded
// changeset the platform makes the branch from.
const ChangesetIDField = "changesetId"

func itemPath(itemID, rest string) string {
	return "/mesh/v1/items/" + url.PathEscape(itemID) + rest
}

// ChangesetMissing asks which of objects the platform does not hold yet
// for itemID: POST /mesh/v1/items/{id}/changeset/missing with
// `{"blobs": [sha...], "trees": [sha...]}`, answered in the same shape —
// the shas of each kind the store lacks (runsheet
// gateway/changeset_blobs.go; at most MaxMissingShas a call, so a larger
// offer goes in batches). The answer is the objects not held, blobs then
// trees, in the platform's order. A platform without the route answers
// IsUnsupported.
func (c *Client) ChangesetMissing(ctx context.Context, itemID string, objects []ChangesetObject) ([]ChangesetObject, error) {
	missing := []ChangesetObject{}
	for start := 0; start < len(objects) || (start == 0 && len(objects) == 0); start += MaxMissingShas {
		end := min(start+MaxMissingShas, len(objects))
		ask := missingShas{Blobs: []string{}, Trees: []string{}}
		for _, o := range objects[start:end] {
			switch o.Type {
			case ObjectBlob:
				ask.Blobs = append(ask.Blobs, o.SHA)
			case ObjectTree:
				ask.Trees = append(ask.Trees, o.SHA)
			default:
				return nil, fmt.Errorf("changeset object %s: kind %q is neither blob nor tree", o.SHA, o.Type)
			}
		}
		raw, err := c.changesetJSON(ctx, http.MethodPost, itemPath(itemID, "/changeset/missing"), ask)
		if err != nil {
			return nil, err
		}
		var out missingShas
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("changeset missing: decode: %w", err)
		}
		for _, sha := range out.Blobs {
			missing = append(missing, ChangesetObject{Type: ObjectBlob, SHA: sha})
		}
		for _, sha := range out.Trees {
			missing = append(missing, ChangesetObject{Type: ObjectTree, SHA: sha})
		}
		if len(objects) == 0 {
			break
		}
	}
	return missing, nil
}

// MaxMissingShas is how many shas one changeset/missing call may ask
// about: the platform's cap (runsheet gateway.MaxMissingShas).
const MaxMissingShas = 10000

// missingShas is changeset/missing's body both ways: the shas asked, and
// the shas the store lacks.
type missingShas struct {
	Blobs []string `json:"blobs"`
	Trees []string `json:"trees"`
}

// PutChangesetObject uploads one object's content — git's own bytes of
// it, `git cat-file <type> <sha>` — for itemID: PUT
// /mesh/v1/items/{id}/changeset/objects/{type}/{sha}.
func (c *Client) PutChangesetObject(ctx context.Context, itemID string, obj ChangesetObject, content []byte) error {
	if obj.Type != ObjectBlob && obj.Type != ObjectTree {
		return fmt.Errorf("changeset object %s: kind %q is neither blob nor tree", obj.SHA, obj.Type)
	}
	path := itemPath(itemID, "/changeset/objects/"+obj.Type+"/"+url.PathEscape(obj.SHA))
	hr, err := c.changesetRequest(ctx, http.MethodPut, path, bytes.NewReader(content), "application/octet-stream")
	if err != nil {
		return err
	}
	raw, status, err := c.doREST(hr)
	if err != nil {
		return err
	}
	return changesetStatus("changeset object "+obj.Type+" "+obj.SHA, path, raw, status)
}

// HasChangesetObject asks whether the platform holds obj for itemID:
// HEAD /mesh/v1/items/{id}/changeset/objects/{type}/{sha}, 2xx held and
// 404 not.
func (c *Client) HasChangesetObject(ctx context.Context, itemID string, obj ChangesetObject) (bool, error) {
	path := itemPath(itemID, "/changeset/objects/"+obj.Type+"/"+url.PathEscape(obj.SHA))
	hr, err := c.changesetRequest(ctx, http.MethodHead, path, nil, "")
	if err != nil {
		return false, err
	}
	raw, status, err := c.doREST(hr)
	if err != nil {
		return false, err
	}
	if status == http.StatusNotFound {
		return false, nil
	}
	if err := changesetStatus("changeset object "+obj.Type+" "+obj.SHA, path, raw, status); err != nil {
		return false, err
	}
	return true, nil
}

// RecordChangeset records cs for itemID: POST
// /mesh/v1/items/{id}/changesets. A platform without the route answers
// IsUnsupported; a refusal (a tree it lacks, a parent not the item's)
// is a *Error.
func (c *Client) RecordChangeset(ctx context.Context, itemID string, cs Changeset) error {
	if cs.ChangesetID == "" || cs.Tree == "" || cs.BaseTree == "" {
		return errors.New("changeset: needs an id, a tree and a base tree")
	}
	if cs.PathsChanged == nil {
		cs.PathsChanged = []string{}
	}
	_, err := c.changesetJSON(ctx, http.MethodPost, itemPath(itemID, "/changesets"), cs)
	return err
}

// FinishWorkItemChangeset finishes the item on a recorded changeset: the
// platform makes the branch from it, so the input names branch and
// changesetId and no head sha. A FinishWorkItemInput without
// `changesetId` (introspected, kept ScalingProbeEvery) answers
// IsUnsupported and nothing is sent. usage and description are as
// FinishWorkItem's.
func (c *Client) FinishWorkItemChangeset(ctx context.Context, itemID, branch, changesetID string, costCents int, usage *UsageReport, description ...string) error {
	if changesetID == "" {
		return errors.New("gateway: a changeset finish needs a changeset id")
	}
	if !c.takesInputField(ctx, &c.changesetSchema, "FinishWorkItemInput", ChangesetIDField) {
		return &Error{Message: "the platform's FinishWorkItemInput takes no " + ChangesetIDField, Unsupported: true}
	}
	in := map[string]any{"branch": branch, ChangesetIDField: changesetID, "costCents": costCents}
	c.withDescription(ctx, in, description)
	return c.mutateWithUsage(ctx, "finishWorkItem", itemID, in, usage)
}

// TakesChangesets says whether the platform finishes an item on a
// changeset: its FinishWorkItemInput takes `changesetId` (introspected,
// kept ScalingProbeEvery). The node's finish step records a changeset
// only then, else pushes (mesh-cli ADR-038).
func (c *Client) TakesChangesets(ctx context.Context) bool {
	return c.takesInputField(ctx, &c.changesetSchema, "FinishWorkItemInput", ChangesetIDField)
}

func (c *Client) changesetRequest(ctx context.Context, method, path string, body *bytes.Reader, contentType string) (*http.Request, error) {
	var hr *http.Request
	var err error
	if body == nil {
		hr, err = http.NewRequestWithContext(ctx, method, c.BaseURL+path, nil)
	} else {
		hr, err = http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	}
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		hr.Header.Set("Content-Type", contentType)
	}
	hr.Header.Set("Accept", "application/json")
	if id := nodeIDFrom(ctx); id != "" {
		hr.Header.Set(NodeIDHeader, id)
	}
	tok, err := c.Bearer(ctx)
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Authorization", "Bearer "+tok)
	return hr, nil
}

func (c *Client) changesetJSON(ctx context.Context, method, path string, in any) ([]byte, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	hr, err := c.changesetRequest(ctx, method, path, bytes.NewReader(body), "application/json")
	if err != nil {
		return nil, err
	}
	raw, status, err := c.doREST(hr)
	if err != nil {
		return nil, err
	}
	if err := changesetStatus("changeset", path, raw, status); err != nil {
		return nil, err
	}
	return raw, nil
}

// changesetStatus reads a changeset route's status: 404/405 is a
// platform without the route (IsUnsupported), another 4xx a refusal,
// anything else not 2xx an error that is neither.
func changesetStatus(what, path string, raw []byte, status int) error {
	switch {
	case status == http.StatusNotFound || status == http.StatusMethodNotAllowed:
		return &Error{Message: "the platform holds no changesets (" + path + ": " + restMessage(raw, status) + ")", Unsupported: true, Status: status}
	case status >= 400 && status < 500:
		return &Error{Message: what + ": " + restMessage(raw, status), Status: status}
	case status < 200 || status >= 300:
		return &httpError{Status: status, Text: http.StatusText(status), Body: restMessage(raw, status)}
	}
	return nil
}
