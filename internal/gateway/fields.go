package gateway

import "sort"

// entityFields is what fields_gen.go generated per entity, by the
// schema's type name: the fields every platform serves and the ones an
// older platform may lack.
var entityFields = map[string]struct{ required, optional []string }{
	"DirectionIndex": {directionRequired, directionOptional},
	"GoalIndex":      {goalRequired, goalOptional},
	"NodeIndex":      {nodeRequired, nodeOptional},
	"ProjectIndex":   {projectRequired, projectOptional},
	"WorkItemIndex":  {workItemRequired, workItemOptional},
}

// Entities are the schema types whose field lists are generated, sorted.
func Entities() []string {
	out := make([]string, 0, len(entityFields))
	for name := range entityFields {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Fields is the generated required selection of an entity (a schema
// type name such as "WorkItemIndex"), sorted; nil for an entity whose
// list is not generated. It is a copy.
func Fields(entity string) []string {
	return append([]string(nil), entityFields[entity].required...)
}

// OptionalFields is the generated optional selection of an entity: the
// fields an older platform may lack.
func OptionalFields(entity string) []string {
	return append([]string(nil), entityFields[entity].optional...)
}
