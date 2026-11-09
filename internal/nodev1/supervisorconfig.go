package nodev1

import (
	"os"

	"gopkg.in/yaml.v3"

	"projectmesh.io/mesh-cli/internal/supervisor"
)

// nodeYAMLSupervisor is node.yaml's own `v1: {supervisor: off|warn|cancel|tidy}`
// key (ADR-044 amendment, 2026-11-09): the v1 node reads it on its own,
// never through the legacy internal/node.Config, which nodev1 may not
// import (guard_test.go). A pointer, so a block that leaves the key out
// is told apart from one that names "off" outright.
type nodeYAMLSupervisor struct {
	V1 struct {
		Supervisor *string `yaml:"supervisor"`
	} `yaml:"v1"`
}

// ReadSupervisorConfig reads node.yaml's `v1.supervisor`: false with no
// error when the file does not exist or the key is left out. A value
// that is not one of supervisor.ParseMode's own names ("off", "warn"
// or "report", "cancel", "tidy") is an error naming it, the same way
// `mesh node run --supervisor` refuses one at start.
func ReadSupervisorConfig(path string) (supervisor.Mode, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	var cfg nodeYAMLSupervisor
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return "", false, err
	}
	if cfg.V1.Supervisor == nil {
		return "", false, nil
	}
	m, err := supervisor.ParseMode(*cfg.V1.Supervisor)
	if err != nil {
		return "", false, err
	}
	return m, true, nil
}
