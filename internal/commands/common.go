package commands

import (
	"encoding/json"
	"fmt"

	"projectmesh.io/mesh-cli/internal/auth"
)

// common.go holds the shared command helpers that used to live in task.go (which
// was removed in the projects/tasks/workflows teardown). They are used across most
// command files, so they live in their own file independent of any one command.

// printJSON renders v as indented JSON to stdout — the CLI's default output format.
func printJSON(v any) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(body))
	return nil
}

// testSessionOverride, when non-nil, is returned directly by requireSession
// instead of loading from disk. Set by tests only.
var testSessionOverride *auth.Session

// overrideSessionForTest injects a session for unit tests. Pass nil to clear.
func overrideSessionForTest(s *auth.Session) {
	testSessionOverride = s
}

// requireSession returns the active CLI session, honoring a test override, else
// loading from disk (requireSessionImpl, defined in auth.go, handles refresh).
func requireSession() (*auth.Session, error) {
	if testSessionOverride != nil {
		return testSessionOverride, nil
	}
	return requireSessionImpl()
}
