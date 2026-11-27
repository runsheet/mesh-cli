package nodev1

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// What a lease's own hold of the node is admitted on (goal
// a-job-is-sized-by-request-and-limit, item 3): node.yaml's v1.admit_on,
// or `mesh node run --admit-on`. declared, the default, admits a lease on
// its brief's declared resources, as every node did before; requests
// admits its CPU on the request learned from its kind's measured peaks
// (learnedrequest.go), so a build that idles at a tenth of a core no
// longer holds four. Its hard cap stays its declared limit either way
// (the sandbox's limitsOf). Memory stays admitted on its declared size in
// this first cut. The reserves keep their floor: they are room of the
// scheduler, and an ask counts against them whatever it was sized on.
// Turning a node to requests is a person's switch.

const (
	// AdmitOnDeclared admits on the brief's declared resources.
	AdmitOnDeclared = "declared"
	// AdmitOnRequests admits CPU on the learned request.
	AdmitOnRequests = "requests"
)

type nodeYAMLAdmitOn struct {
	V1 struct {
		AdmitOn *string `yaml:"admit_on"`
	} `yaml:"v1"`
}

// ReadAdmitOn is node.yaml's v1.admit_on when it sets one, else def; an
// unknown value is an error naming the two it may be.
func ReadAdmitOn(file, def string) (string, error) {
	v := def
	b, err := os.ReadFile(file)
	if err != nil && !os.IsNotExist(err) {
		return def, err
	}
	if err == nil {
		var cfg nodeYAMLAdmitOn
		if err := yaml.Unmarshal(b, &cfg); err != nil {
			return def, err
		}
		if cfg.V1.AdmitOn != nil {
			v = *cfg.V1.AdmitOn
		}
	}
	switch v {
	case "", AdmitOnDeclared:
		return AdmitOnDeclared, nil
	case AdmitOnRequests:
		return AdmitOnRequests, nil
	}
	return def, fmt.Errorf("v1.admit_on %q: want %s or %s", v, AdmitOnDeclared, AdmitOnRequests)
}
