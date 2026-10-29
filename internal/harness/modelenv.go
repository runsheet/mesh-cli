package harness

import (
	"os"
	"slices"
	"strings"
)

// ModelEnv are the variables a harness reads a model credential, or its
// model's address, from. None of a node's own reaches a Mesh v1 session
// or anything the session wrote (projectmesh-io/api ADR 001 amendment
// "model relay"): the lease's relay holds the credential, and sets its
// own address and a dummy in their place.
var ModelEnv = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_BASE_URL",
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX",
	"OPENAI_API_KEY", "CODEX_API_KEY", "OPENAI_BASE_URL",
}

// EnvironWithoutModel is the process's environment less ModelEnv.
func EnvironWithoutModel() []string {
	return slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(ModelEnv, name)
	})
}
