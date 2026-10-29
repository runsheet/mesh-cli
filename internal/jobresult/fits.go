package jobresult

import (
	"encoding/json"
	"fmt"
)

// Fits checks a result against a kind's finish_schema, the JSON text of
// a JSON Schema: nil when it fits. A v1 node takes a session's last
// message as its finish only when it fits (projectmesh-io/api ADR 001
// amendment, finish enforcement).
func Fits(schema string, result json.RawMessage) error {
	rs, _, err := resolveSchema(schema)
	if err != nil {
		return fmt.Errorf("the kind's finish_schema: %w", err)
	}
	var instance any
	if err := json.Unmarshal(result, &instance); err != nil {
		return err
	}
	return rs.Validate(instance)
}
