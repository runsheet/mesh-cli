// Package jobresult holds a job's result as its session hands it in by
// tool call (runsheet ADR 067 decision 10; mesh-cli ADR-041). A session
// never finishes a job: it calls the node's `mesh_result` once with the
// finish's shape — the same keys finishJob takes for the kind's finish —
// and the node, after its own checks, sends finishJob on its own
// credential with what was recorded. A call whose keys are not the
// finish's, or whose product part (the proposal of an answer-or-proposal
// kind) does not fit the kind's finish_schema, is refused to the session
// with the reason, so it can correct and call again; a second accepted
// call replaces the first. `mesh_fail` says the job cannot be done.
package jobresult

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"

	"projectmesh.io/mesh-cli/internal/mcptool"
)

// ResultTool is the node's result tool, served on the node's own MCP
// server (harness.NodeWorkMCP) to every session whose kind finishes
// with a session result.
const ResultTool = "mesh_result"

// FailTool is the node's give-up tool beside it.
const FailTool = "mesh_fail"

// The finishes Mesh routes (runsheet gateway/finishjob.go).
const (
	FinishAnswerOrProposal = "answer-or-proposal"
	FinishAttestation      = "attestation"
	FinishChangeset        = "changeset"
	FinishDeployRecord     = "deploy-record"
	FinishPlan             = "plan"
	FinishRebasedHead      = "rebased-head"
	FinishResearch         = "research"
	FinishResult           = "result"
	FinishVerdict          = "verdict"
)

// Shape is a finish's keys as finishJob checks them (runsheet
// finishShapes): every one of Required, any of Optional (each an
// object), and for OneOf at least one of them, each a non-empty string.
type Shape struct {
	Finish   string
	Required []string
	Optional []string
	OneOf    []string
	// fields is the one-line account of each key the finish line gives
	// the session.
	fields map[string]string
}

var shapes = map[string]Shape{
	FinishAnswerOrProposal: {OneOf: []string{"answer", "proposal"}, fields: map[string]string{
		"answer":   "what you say to the person, as text",
		"proposal": "the draft you propose, as a JSON object (the tool's input schema says its shape when the product gave one); leave it out when you propose nothing",
	}},
	FinishAttestation:  {Required: []string{"attestation"}, fields: map[string]string{"attestation": "the validation's attestation, an object"}},
	FinishChangeset:    {Required: []string{"changeset"}, Optional: []string{"attestation"}, fields: map[string]string{"changeset": "the changeset, an object", "attestation": "the head's attestation, an object, when the node validated it"}},
	FinishDeployRecord: {Required: []string{"deployRecord"}, fields: map[string]string{"deployRecord": "the deploy's record, an object"}},
	FinishPlan: {Required: []string{"plan"}, fields: map[string]string{
		"plan": "an object: `items` (the graph), and beside them `questions`, `checkpoints`, `split` and `already_done` when the rules ask for them",
	}},
	FinishRebasedHead: {Required: []string{"rebasedHead"}, fields: map[string]string{
		"rebasedHead": "an object: `headSha`, the commit you pushed (`git rev-parse HEAD`), and `files`, the files you resolved by hand (empty when none)",
	}},
	FinishResearch: {Required: []string{"research"}, fields: map[string]string{
		"research": "an object: `goals` (a set direction's proposals), or `pitches` and `answers` (a propose direction's)",
	}},
	FinishResult: {Required: []string{"result"}, fields: map[string]string{"result": "the result, an object fitting the product's schema (the tool's input schema carries it)"}},
	FinishVerdict: {Required: []string{"verdict"}, fields: map[string]string{
		"verdict": "an object: `passed` (true or false), `notes` (your review), `failedClauses` (one {clause, satisfy} per failed acceptance clause, empty when you pass) and `readings` (one {clause, reading} per host clause)",
	}},
}

// ShapeOf is finish's shape; ok is false for a finish Mesh does not
// know.
func ShapeOf(finish string) (Shape, bool) {
	s, ok := shapes[finish]
	if !ok {
		return Shape{}, false
	}
	s.Finish = finish
	return s, true
}

// Keys are the shape's keys in order: required, optional, one-of.
func (s Shape) Keys() []string {
	return slices.Concat(s.Required, s.Optional, s.OneOf)
}

// String is the shape as finishJob names it: {changeset, attestation?},
// {answer and/or proposal}.
func (s Shape) String() string {
	keys := slices.Clone(s.Required)
	for _, k := range s.Optional {
		keys = append(keys, k+"?")
	}
	if len(s.OneOf) > 0 {
		keys = append(keys, strings.Join(s.OneOf, " and/or "))
	}
	return "{" + strings.Join(keys, ", ") + "}"
}

// schemaKey is the key of the finish the kind's finish_schema describes:
// the proposal of an answer-or-proposal kind, the whole result of a
// result kind; "" for a finish that takes no product schema.
func (s Shape) schemaKey() string {
	switch s.Finish {
	case FinishAnswerOrProposal:
		return "proposal"
	case FinishResult:
		return "result"
	}
	return ""
}

// Holder holds one job's result. Safe for concurrent use.
type Holder struct {
	// Kind is the job's kind, for the refusals' words.
	Kind  string
	shape Shape
	// schema is the kind's finish_schema, resolved; nil for none.
	schema     *jsonschema.Resolved
	schemaText string
	schemaMap  map[string]any

	mu     sync.Mutex
	result map[string]any
	failed string
	calls  int
}

// New is a holder for a job of kind whose finish is finish, its product
// schema finishSchema ("" for none). A finish Mesh does not know, or a
// schema that does not resolve, is an error: the node cannot serve a
// result tool for it.
func New(kind, finish, finishSchema string) (*Holder, error) {
	shape, ok := ShapeOf(finish)
	if !ok {
		return nil, fmt.Errorf("a %s job finishes with %q, which Mesh does not know", kind, finish)
	}
	h := &Holder{Kind: kind, shape: shape}
	if text := strings.TrimSpace(finishSchema); text != "" {
		if shape.schemaKey() == "" {
			return nil, fmt.Errorf("a %s job finishes with %s, which takes no product schema", kind, finish)
		}
		r, m, err := resolveSchema(text)
		if err != nil {
			return nil, fmt.Errorf("the %s kind's finish_schema %v", kind, err)
		}
		h.schema, h.schemaText, h.schemaMap = r, text, m
	}
	return h, nil
}

// resolveSchema reads a kind's finish_schema as runsheet does
// (productmanifest.ResolveFinishSchema): a JSON object, resolved with
// no external references.
func resolveSchema(raw string) (*jsonschema.Resolved, map[string]any, error) {
	if !strings.HasPrefix(raw, "{") {
		return nil, nil, fmt.Errorf("is not a JSON Schema object")
	}
	var s jsonschema.Schema
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, nil, fmt.Errorf("does not parse as a JSON Schema: %v", err)
	}
	r, err := s.Resolve(nil)
	if err != nil {
		return nil, nil, fmt.Errorf("does not resolve: %v", err)
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(raw), &m)
	return r, m, nil
}

// Shape is the finish's shape.
func (h *Holder) Shape() Shape { return h.shape }

// Finish is the finish's name.
func (h *Holder) Finish() string { return h.shape.Finish }

// HasSchema says the kind gave a finish_schema.
func (h *Holder) HasSchema() bool { return h.schema != nil }

// Call is a mesh_result call: the finish checked against the shape and
// the product schema, recorded when it fits — replacing an earlier
// result or failure — and refused with the reason when not.
func (h *Holder) Call(args map[string]any) (string, error) {
	finish, err := h.check(args)
	if err != nil {
		return "", err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	replaced := h.result != nil || h.failed != ""
	h.result, h.failed = finish, ""
	h.calls++
	if replaced {
		return "recorded, replacing what you handed in before: the node checks it and finishes the job; end the session now", nil
	}
	return "recorded: the node checks it and finishes the job; end the session now", nil
}

// Fail is a mesh_fail call: the job cannot be done, and why. It replaces
// a result recorded before it.
func (h *Holder) Fail(args map[string]any) (string, error) {
	reason := strings.TrimSpace(str(args["error"]))
	if reason == "" {
		reason = strings.TrimSpace(str(args["reason"]))
	}
	if reason == "" {
		return "", fmt.Errorf("refused: mesh_fail takes `error`, why the job cannot be done")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.result, h.failed = nil, reason
	h.calls++
	return "recorded: the node hands the failure in; end the session now", nil
}

// Recover takes a mesh_result call the session's stream recorded but
// the node never received — the relay or the proxy failed between them
// — when nothing was recorded by the node itself, checked exactly as a
// live call is; ok says whether it was recorded. A holder that already
// holds a result or a failure is left as it is. The stream's record is
// the same call, read late; it is never the final message.
func (h *Holder) Recover(args map[string]any) (recorded bool, err error) {
	if args == nil {
		return false, nil
	}
	h.mu.Lock()
	held := h.result != nil || h.failed != ""
	h.mu.Unlock()
	if held {
		return false, nil
	}
	if _, err := h.Call(args); err != nil {
		return false, err
	}
	return true, nil
}

// Result is the finish the session handed in, as finishJob takes it;
// ok is false when none was recorded (or a failure replaced it).
func (h *Holder) Result() (map[string]any, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.result == nil {
		return nil, false
	}
	return h.result, true
}

// Failed is why the session gave the job up, when it called mesh_fail
// last.
func (h *Holder) Failed() (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.failed, h.failed != ""
}

// Calls is how many calls the holder accepted.
func (h *Holder) Calls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// check is one call against the shape and the schema: the finish to
// record, or why not.
func (h *Holder) check(args map[string]any) (map[string]any, error) {
	s := h.shape
	allowed := s.Keys()
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	wrong := func(why string) error {
		return fmt.Errorf("refused: a %s job finishes with %s %s, not {%s}: %s", h.Kind, s.Finish, s, strings.Join(keys, ", "), why)
	}
	for _, k := range keys {
		if !slices.Contains(allowed, k) {
			return nil, wrong(fmt.Sprintf("%q is not one of its keys", k))
		}
	}
	finish := make(map[string]any, len(args))
	for _, k := range s.Required {
		m, ok := args[k].(map[string]any)
		if !ok {
			return nil, wrong(fmt.Sprintf("`%s` is required, an object", k))
		}
		finish[k] = m
	}
	for _, k := range s.Optional {
		v, ok := args[k]
		if !ok || v == nil {
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, wrong(fmt.Sprintf("`%s` is an object", k))
		}
		finish[k] = m
	}
	n := 0
	for _, k := range s.OneOf {
		v, ok := args[k]
		if !ok || v == nil {
			continue
		}
		var text string
		switch t := v.(type) {
		case string:
			text = strings.TrimSpace(t)
		case map[string]any, []any:
			// a client that sent the JSON itself: carried as its text, as
			// finishJob takes it
			b, err := json.Marshal(t)
			if err != nil {
				return nil, wrong(fmt.Sprintf("`%s`: %v", k, err))
			}
			text = string(b)
		default:
			return nil, wrong(fmt.Sprintf("`%s` is text, or a JSON object", k))
		}
		if text == "" {
			continue
		}
		finish[k] = text
		n++
	}
	if len(s.OneOf) > 0 && n == 0 {
		return nil, wrong("at least one of " + strings.Join(s.OneOf, ", ") + " is needed, as text")
	}
	if err := h.checkSchema(finish); err != nil {
		return nil, err
	}
	return finish, nil
}

// checkSchema refuses the product's part of the finish when it does not
// fit the kind's finish_schema, naming what does not fit as the
// validator says it (runsheet checkFinishSchema).
func (h *Holder) checkSchema(finish map[string]any) error {
	key := h.shape.schemaKey()
	if h.schema == nil || key == "" {
		return nil
	}
	v, ok := finish[key]
	if !ok {
		return nil
	}
	if text, isText := v.(string); isText {
		var decoded any
		if err := json.Unmarshal([]byte(text), &decoded); err != nil {
			return fmt.Errorf("refused: a %s job's %s is not JSON: %v", h.Kind, key, err)
		}
		v = decoded
	}
	if err := h.schema.Validate(v); err != nil {
		return fmt.Errorf("refused: a %s job's %s does not fit the kind's finish_schema: %v", h.Kind, key, err)
	}
	return nil
}

// Tools are the node's tools for this job: mesh_result, whose input
// schema is the finish's shape (the product's finish_schema inlined for
// its part), and mesh_fail.
func (h *Holder) Tools() []mcptool.Tool {
	return []mcptool.Tool{
		{
			Name:        ResultTool,
			Description: fmt.Sprintf("Hand in this %s job's result, once, as your last action: %s. The node checks it and finishes the job itself. A call that does not fit is refused with the reason; correct it and call again. A second call replaces the first.", h.Kind, h.shape),
			Schema:      h.inputSchema(),
			Call:        func(_ context.Context, args map[string]any) (string, error) { return h.Call(args) },
		},
		{
			Name:        FailTool,
			Description: "Give the job up: why it cannot be done. The node hands the failure in.",
			Schema: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"error": map[string]any{"type": "string", "description": "why the job cannot be done"}},
				"required":             []string{"error"},
				"additionalProperties": false,
			},
			Call: func(_ context.Context, args map[string]any) (string, error) { return h.Fail(args) },
		},
	}
}

// inputSchema is mesh_result's input schema for the finish: one
// property per key, the required ones required, the product's schema as
// its key's.
func (h *Holder) inputSchema() map[string]any {
	s := h.shape
	props := map[string]any{}
	for _, k := range s.Keys() {
		desc := s.fields[k]
		switch {
		case k == s.schemaKey() && h.schemaMap != nil:
			prop := make(map[string]any, len(h.schemaMap)+1)
			for pk, pv := range h.schemaMap {
				prop[pk] = pv
			}
			if _, has := prop["description"]; !has && desc != "" {
				prop["description"] = desc
			}
			props[k] = prop
		case slices.Contains(s.OneOf, k):
			props[k] = map[string]any{"description": desc}
		default:
			props[k] = map[string]any{"type": "object", "description": desc}
		}
	}
	required := slices.Clone(s.Required)
	if required == nil {
		required = []string{}
	}
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
}

// FinishSection is the one fixed section every session's prompt ends
// with: how this job finishes. The shape's keys are named with their
// one-line accounts; detail is anything the kind's prompt appends (a
// plan's item fields), after it.
func (h *Holder) FinishSection() string {
	return FinishSection(h.shape, h.schema != nil)
}

// FinishSection is Holder.FinishSection for shape; withSchema says the
// product gave a schema for its part.
func FinishSection(shape Shape, withSchema bool) string {
	var b strings.Builder
	b.WriteString("# How this job finishes\n\n")
	fmt.Fprintf(&b, "Call the %s server's `%s` tool once, as your last action, with this job's result: %s.", NodeServer, ResultTool, shape)
	for _, k := range shape.Keys() {
		if desc := shape.fields[k]; desc != "" {
			fmt.Fprintf(&b, " `%s`: %s.", k, desc)
		}
	}
	if withSchema {
		fmt.Fprintf(&b, " The product's schema for its part is the tool's input schema; a %s that does not fit it is refused naming the field.", shape.schemaKey())
	}
	fmt.Fprintf(&b, " A call that does not fit is refused with the reason: correct it and call again; a second call replaces the first. When the job cannot be done, call `%s(error=<why>)` instead. The call is the result: the node checks it and finishes the job on its own credential, and nothing in your final message is read as the result.\n", FailTool)
	return b.String()
}

// NodeServer is the MCP server name the session reaches the node's
// tools on: harness.NodeWorkMCP, spelled here so this package stays a
// leaf.
const NodeServer = "mesh-node"

func str(v any) string {
	s, _ := v.(string)
	return s
}
