package node

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gitapply"
)

// The would-be merge (runsheet ADR 077): a brief whose code names onto
// asks the node to validate its change applied onto that commit, not on
// the change's own base. These are the pieces every kind that reads it
// shares; a brief without onto never reaches them.

// ontoGit is gitapply's git as the node runs it in a checkout a session
// wrote: hardened (hardenedGitCmdEnv), so nothing the repository's
// config names runs.
func ontoGit() gitapply.Git {
	return gitapply.Git{Command: func(ctx context.Context, dir string, env []string, args ...string) *exec.Cmd {
		return hardenedGitCmdEnv(ctx, "", dir, env, args...)
	}}
}

// applyIdentity is who the commits applying a brief's changes are
// written as: the brief's applyAs, its date fixed so every node writes
// the same shas; else fallback — the session's own identity — at now.
func applyIdentity(as *gateway.JobApplyAs, fallback Identity, now time.Time) (gitapply.Identity, error) {
	if as == nil || as.Name == "" && as.Email == "" && as.Date == "" {
		return gitapply.Identity{Name: fallback.Name, Email: fallback.Email, When: now}, nil
	}
	id := gitapply.Identity{Name: as.Name, Email: as.Email, When: now}
	if id.Name == "" {
		id.Name = fallback.Name
	}
	if id.Email == "" {
		id.Email = fallback.Email
	}
	if as.Date != "" {
		when, err := time.Parse(time.RFC3339, as.Date)
		if err != nil {
			return gitapply.Identity{}, fmt.Errorf("applyAs date %q: %w", as.Date, err)
		}
		id.When = when
	}
	return id, nil
}

// applyMode is the brief's applyMode, def when it names none.
func applyMode(code gateway.JobCode, def gitapply.Mode) (gitapply.Mode, error) {
	switch code.ApplyMode {
	case "":
		return def, nil
	case gateway.ApplyLinear:
		return gitapply.Linear, nil
	case gateway.ApplyMerge:
		return gitapply.Merge, nil
	}
	return def, fmt.Errorf("applyMode %q is neither %s nor %s", code.ApplyMode, gateway.ApplyLinear, gateway.ApplyMerge)
}
