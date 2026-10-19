package node

import "os"

// Since git 2.47 every commit, fetch and merge runs `git maintenance run
// --auto --detach`, a child that outlives the git that started it. In a
// validation pod whose pid 1 reaps no orphans each one stays a zombie,
// and this package's thousands of git commands use up the pod's pids
// limit until git, and the linker, cannot fork. Every git a test starts
// — the tests' own and the node's — inherits GIT_CONFIG_PARAMETERS
// (`git -c`'s carrier, beside any GIT_CONFIG_COUNT or GIT_CONFIG_GLOBAL
// a test or the node sets), so none starts maintenance.
func init() {
	const off = "'maintenance.auto'='false' 'gc.auto'='0'"
	if p := os.Getenv("GIT_CONFIG_PARAMETERS"); p != "" {
		os.Setenv("GIT_CONFIG_PARAMETERS", p+" "+off)
		return
	}
	os.Setenv("GIT_CONFIG_PARAMETERS", off)
}
