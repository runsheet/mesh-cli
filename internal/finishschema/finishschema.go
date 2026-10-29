// Package finishschema reads a kind's finish_schema, the JSON Schema a
// job's result must fit (projectmesh-io/api ADR 001 amendment, node
// tools: the result tool checks it). It keeps the schema library out of
// the node tools' own files, which name no host of any kind (nodev1's
// guard test).
package finishschema

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// Schema is a resolved finish_schema.
type Schema struct {
	resolved *jsonschema.Resolved
}

// Resolve reads raw as a JSON Schema object resolved with no external
// references, as runsheet's productmanifest.ResolveFinishSchema does;
// nil, nil when raw is empty or null.
func Resolve(raw json.RawMessage) (*Schema, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return nil, nil
	}
	if !strings.HasPrefix(text, "{") {
		return nil, fmt.Errorf("is not a JSON Schema object")
	}
	var s jsonschema.Schema
	if err := json.Unmarshal([]byte(text), &s); err != nil {
		return nil, fmt.Errorf("does not parse as a JSON Schema: %v", err)
	}
	r, err := s.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("does not resolve: %v", err)
	}
	return &Schema{resolved: r}, nil
}

// Validate says why v, decoded JSON, does not fit the schema, nil when
// it does.
func (s *Schema) Validate(v any) error {
	return s.resolved.Validate(v)
}
