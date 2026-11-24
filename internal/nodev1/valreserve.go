package nodev1

import (
	"os"
	"path"
	"slices"

	"gopkg.in/yaml.v3"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/scheduler"
)

// The validation reserve (goal
// a-v1-node-reserves-room-for-validations-and-sizes-its-reserves-from-use
// P1, P2): room the node's scheduler keeps for its validation kinds
// alone — validate (chain candidate, main and pull request validations)
// and evaluate — beside the assistant's (assistantreserve.go). Builds,
// plans, goal reviews, the librarian and every other kind never take
// it; a validation draws it first, its lease's own hold and every lane,
// and uses the general room beyond it when it is full. Inside it a
// person share is kept for people's pull request validations alone
// (validate_for=<repo>:person, as runsheet labels them: the subject a
// pull request), which also wait ahead of automated ones and each
// repository's first ahead of any repository's second
// (scheduler/takeall.go). dl360, 2026-11-24: builds and reviews filled
// the admission budget, chain validations waited up to 32 minutes for
// room and hand pull request validations starved for hours.
//
// With no room set the node keeps no reserve and admits as before.

// ValidationReserveName is the scheduler's reserve validations draw.
const ValidationReserveName = "validations"

// ValidationReserve is the validation reserve's setting: `mesh node
// run`'s --validation-reserve-* flags, each field overridden by
// node.yaml's `v1.validation_reserve`, read at start.
type ValidationReserve struct {
	CPUs  float64
	MemGB float64
	// PersonCPUs and PersonMemGB are the person share inside it.
	PersonCPUs  float64
	PersonMemGB float64
	// Kinds are short kind names ("validate", "evaluate").
	Kinds []string
}

type nodeYAMLValidationReserve struct {
	V1 struct {
		ValidationReserve *struct {
			CPUs        *float64 `yaml:"cpus"`
			MemGB       *float64 `yaml:"mem_gb"`
			PersonCPUs  *float64 `yaml:"person_cpus"`
			PersonMemGB *float64 `yaml:"person_mem_gb"`
			Kinds       []string `yaml:"kinds"`
		} `yaml:"validation_reserve"`
	} `yaml:"v1"`
}

// ReadValidationReserve is def with every field node.yaml's
// `v1.validation_reserve` sets substituted for its own; def itself when
// the file or the block is missing.
func ReadValidationReserve(file string, def ValidationReserve) (ValidationReserve, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return def, nil
		}
		return def, err
	}
	var cfg nodeYAMLValidationReserve
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return def, err
	}
	v := cfg.V1.ValidationReserve
	if v == nil {
		return def, nil
	}
	out := def
	for _, f := range []struct {
		from *float64
		to   *float64
	}{{v.CPUs, &out.CPUs}, {v.MemGB, &out.MemGB}, {v.PersonCPUs, &out.PersonCPUs}, {v.PersonMemGB, &out.PersonMemGB}} {
		if f.from != nil {
			*f.to = *f.from
		}
	}
	if v.Kinds != nil {
		out.Kinds = v.Kinds
	}
	return out, nil
}

// validationReserved is whether the node keeps a validation reserve
// and b is one of its kinds.
func (r *Runtime) validationReserved(b *meshapi.Brief) bool {
	if b == nil || r.Scheduler == nil || len(r.ValidationKinds) == 0 {
		return false
	}
	if c := r.Scheduler.ReserveOf(ValidationReserveName); c.CPUs <= 0 && c.MemGB <= 0 {
		return false
	}
	k := path.Base(b.Kind)
	return slices.ContainsFunc(r.ValidationKinds, func(a string) bool { return path.Base(a) == k })
}

// validateFor is whom b's validation is for, as runsheet's validate_for
// label says it (platform/processes ValidateForLabel): a person's when
// its subject is a pull request, automated for everything else (an
// item's head, a merge candidate, a merge commit); and the repository.
func validateFor(b *meshapi.Brief) (repo string, person bool) {
	if b.Code != nil {
		repo = b.Code.Repo.FullName
	}
	return repo, b.Subject.Type == "pull_request"
}

// reserveAsk is ask drawing the validation reserve first when b is one
// of its kinds, a person's pull request validation its person share
// first; ask as it is otherwise.
func (r *Runtime) reserveAsk(b *meshapi.Brief, ask scheduler.Ask) scheduler.Ask {
	if !r.validationReserved(b) {
		return ask
	}
	ask.Reserve = ValidationReserveName
	ask.Repo, ask.Person = validateFor(b)
	return ask
}
