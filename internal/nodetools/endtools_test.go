package nodetools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

const shaSchema = `{"type":"object","required":["sha256"],"additionalProperties":false,"properties":{"sha256":{"type":"string","pattern":"^[0-9a-f]{64}$"}}}`

var sha = strings.Repeat("ab", 32)

// result keeps a value that fits the kind's finish_schema, refuses one
// that does not with the reason (so the session can correct it), reads
// a value sent as JSON text, and takes any object when the kind has no
// schema. A finish_schema that does not resolve refuses every call.
func TestResultChecksTheFinishSchema(t *testing.T) {
	cases := []struct {
		name   string
		schema string
		arg    any
		refuse string
		kept   string
	}{
		{"fits", shaSchema, map[string]any{"sha256": sha}, "", `{"sha256":"` + sha + `"}`},
		{"as JSON text", shaSchema, `{"sha256":"` + sha + `"}`, "", `{"sha256":"` + sha + `"}`},
		{"does not fit", shaSchema, map[string]any{"sha256": "nope"}, "finish schema", ""},
		{"an extra key", shaSchema, map[string]any{"sha256": sha, "x": 1}, "finish schema", ""},
		{"not an object", "", []any{1}, "a JSON object", ""},
		{"text that is not JSON", "", "done", "not text", ""},
		{"no schema", "", map[string]any{"anything": true}, "", `{"anything":true}`},
		{"missing", "", nil, "result is required", ""},
		{"a broken schema", `{"type":12}`, map[string]any{"sha256": sha}, "finish_schema", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var kept json.RawMessage
			tool := (&Result{Schema: json.RawMessage(c.schema), Accept: func(raw json.RawMessage) { kept = raw }}).Tool()
			args := map[string]any{}
			if c.arg != nil {
				args["result"] = c.arg
			}
			out, err := tool.Call(context.Background(), args)
			if c.refuse != "" {
				if err == nil || !strings.Contains(err.Error(), c.refuse) || kept != nil {
					t.Fatalf("want a refusal naming %q, got %q %v (kept %s)", c.refuse, out, err, kept)
				}
				return
			}
			if err != nil || string(kept) != c.kept || !strings.Contains(out, `"accepted":true`) {
				t.Fatalf("got %q %v, kept %s", out, err, kept)
			}
		})
	}
}

// result's input schema carries the kind's finish_schema, so the session
// sees the shape it must hand in.
func TestResultSchemaCarriesTheFinishSchema(t *testing.T) {
	tool := (&Result{Schema: json.RawMessage(shaSchema)}).Tool()
	props, _ := tool.Schema["properties"].(map[string]any)
	value, _ := props["result"].(map[string]any)
	if tool.Name != ResultTool || value["additionalProperties"] != false || value["required"] == nil {
		t.Fatalf("schema %+v", tool.Schema)
	}
}

// hand_back takes only the causes a session may claim, with a reason;
// every other cause is the node's to say.
func TestHandBackTakesOnlyASessionsCauses(t *testing.T) {
	cases := []struct {
		cause, reason, refuse string
	}{
		{meshapi.CauseCannotRun, "podman is missing", ""},
		{meshapi.CauseProduct, "the brief tool answers 500", ""},
		{meshapi.CauseStalled, "x", "node's to say"},
		{meshapi.CauseWork, "x", "node's to say"},
		{"", "x", "node's to say"},
		{meshapi.CauseProduct, "  ", "reason is required"},
	}
	for _, c := range cases {
		t.Run(c.cause+"/"+c.reason, func(t *testing.T) {
			var cause, reason string
			tool := (&HandBack{Accept: func(c, r string) { cause, reason = c, r }}).Tool()
			out, err := tool.Call(context.Background(), map[string]any{"cause": c.cause, "reason": c.reason})
			if c.refuse != "" {
				if err == nil || !strings.Contains(err.Error(), c.refuse) || cause != "" {
					t.Fatalf("want a refusal naming %q, got %q %v", c.refuse, out, err)
				}
				return
			}
			if err != nil || cause != c.cause || reason != c.reason || !strings.Contains(out, `"handing_back":true`) {
				t.Fatalf("got %q %v (%s, %s)", out, err, cause, reason)
			}
		})
	}
}

type progressPoster struct {
	got []meshapi.ProgressRequest
	err error
}

func (p *progressPoster) Progress(_ context.Context, lease string, req meshapi.ProgressRequest) (*meshapi.Progressed, error) {
	if p.err != nil {
		return nil, p.err
	}
	p.got = append(p.got, req)
	return &meshapi.Progressed{JobID: "job-1", Status: "running", Progress: &meshapi.ProgressNote{LeaseID: lease, Note: req.Note, Notes: int64(len(p.got))}}, nil
}

// progress posts the note's first line on the lease and answers how Mesh
// keeps it; a blank or long note is refused before it leaves the node,
// and Mesh refusing it is the session's error.
func TestProgressPostsTheNoteOnTheLease(t *testing.T) {
	mesh := &progressPoster{}
	tool := (&Progress{LeaseID: "lease-1", Mesh: mesh}).Tool()
	out, err := tool.Call(context.Background(), map[string]any{"note": "  ran the suite\nand more  "})
	if err != nil || len(mesh.got) != 1 || mesh.got[0].Note != "ran the suite" {
		t.Fatalf("got %q %v %+v", out, err, mesh.got)
	}
	var p meshapi.Progressed
	if json.Unmarshal([]byte(out), &p) != nil || p.Progress == nil || p.Progress.LeaseID != "lease-1" || p.Progress.Notes != 1 {
		t.Fatalf("answer %s", out)
	}
	for name, note := range map[string]string{"blank": " ", "long": strings.Repeat("x", 501)} {
		if _, err := tool.Call(context.Background(), map[string]any{"note": note}); err == nil || len(mesh.got) != 1 {
			t.Fatalf("a %s note was posted", name)
		}
	}
	mesh.err = errors.New("409 lease_not_live")
	if _, err := tool.Call(context.Background(), map[string]any{"note": "after"}); err == nil || !strings.Contains(err.Error(), "lease_not_live") {
		t.Fatalf("Mesh's refusal %v", err)
	}
}
