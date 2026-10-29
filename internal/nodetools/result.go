package nodetools

import (
	"context"
	"encoding/json"
	"fmt"

	"projectmesh.io/mesh-cli/internal/finishschema"
	"projectmesh.io/mesh-cli/internal/mcptool"
)

// The node's own finish (projectmesh-io/api ADR 001 amendment, node
// tools). A kind whose finish is Mesh's own, rather than a product
// tool's, names result in its node_tools: the session hands in the
// job's result, the node checks it against the kind's finish_schema and
// finishes the lease with it once the session ends. A value that does
// not fit is refused with the reason, so the session can correct it and
// call again; a later accepted call replaces an earlier one.

// ResultTool is the result node tool's name, as a kind's node_tools
// names it.
const ResultTool = "result"

// Result is one lease's result tool.
type Result struct {
	// Schema is the kind's finish_schema, nil when it has none: any JSON
	// object is then the result.
	Schema json.RawMessage
	// Accept keeps the result for the lease's finish and closes the
	// session's input.
	Accept func(json.RawMessage)
}

// Tool is result {result}, its result's input schema the kind's
// finish_schema when it has one. A finish_schema that does not resolve
// is the kind's fault: every call is refused with it.
func (r *Result) Tool() mcptool.Tool {
	value := map[string]any{"type": "object", "description": "the job's result"}
	resolved, schemaErr := finishschema.Resolve(r.Schema)
	if resolved != nil {
		_ = json.Unmarshal(r.Schema, &value)
	}
	return mcptool.Tool{
		Name: ResultTool,
		Description: "Finish the job with its result: a JSON object, of the job's finish schema when it has one. " +
			"The node checks it and refuses one that does not fit, saying why, so you can correct it and call again. " +
			"Once accepted the job finishes with it when this session ends: call it last.",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"result": value},
			"required":   []string{"result"},
		},
		Call: func(_ context.Context, args map[string]any) (string, error) {
			if schemaErr != nil {
				return "", fmt.Errorf("refused: the kind's finish_schema %v", schemaErr)
			}
			v, ok := args["result"]
			if !ok || v == nil {
				return "", fmt.Errorf("refused: result is required")
			}
			// a result sent as JSON text is read as the JSON it holds
			if text, isText := v.(string); isText {
				var decoded any
				if err := json.Unmarshal([]byte(text), &decoded); err != nil {
					return "", fmt.Errorf("refused: result is a JSON object, not text")
				}
				v = decoded
			}
			if _, isObject := v.(map[string]any); !isObject {
				return "", fmt.Errorf("refused: result is a JSON object")
			}
			if resolved != nil {
				if err := resolved.Validate(v); err != nil {
					return "", fmt.Errorf("refused: the result does not fit the job's finish schema: %v", err)
				}
			}
			raw, err := json.Marshal(v)
			if err != nil {
				return "", fmt.Errorf("refused: %v", err)
			}
			if r.Accept != nil {
				r.Accept(raw)
			}
			out, _ := json.Marshal(map[string]any{"accepted": true,
				"note": "the job finishes with this result when your session ends; end it now"})
			return string(out), nil
		},
	}
}
