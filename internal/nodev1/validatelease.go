package nodev1

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"projectmesh.io/mesh-cli/internal/gitrelay"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/nodetools"
	"projectmesh.io/mesh-cli/internal/validate/infer"
)

// isNoSessionValidate is a validate lease with no harness (Mesh v1
// spec, "A validate job builds and pushes its chain candidate"): the
// node builds the brief's candidate, pushes it and runs its suites
// itself, never starting a session. A validate lease that names a
// harness runs the ordinary, session-driven path instead.
func isNoSessionValidate(b *meshapi.Brief) bool {
	return validationKind(b) && (b.Harness.Name == "" || b.Harness.Name == "none") &&
		b.Code != nil && b.Code.Onto != nil
}

// serveValidate runs a no-session validate lease to its finish:
// apply_onto the brief's changes, and on a conflict finish with it,
// running no suite and pushing nothing; on success, push the
// candidate, run every declared suite, and finish with the candidate,
// the suite run and the checkout's suite_config_sha — the platform's
// canonical hash of the validate.yml the suites ran (ADR 032). suites
// is the lease's suite runner (suiteRunner), nil to run on the host
// (hostSuites).
func (r *Runtime) serveValidate(ctx context.Context, b *meshapi.Brief, grant *gitrelay.Grant, workspace string, tokens *sessionTokens, suites nodetools.SuiteRunner) (json.RawMessage, []string, error) {
	rec := r.recorderFor(b, tokens)
	var g nodetools.Grant
	if grant != nil {
		g = grant
	}
	var repo, vendor string
	if b.Git != nil {
		repo, vendor = b.Git.Repo, b.Git.Vendor
	}

	apply := &nodetools.ApplyOnto{Grant: g, Repo: repo, Workspace: workspace, Code: b.Code, Vendor: vendor, Recorder: rec}
	applyOut, err := apply.Tool().Call(ctx, map[string]any{})
	if err != nil {
		return nil, rec.IDs(), fmt.Errorf("the lease's candidate: %w", err)
	}
	var applied map[string]any
	if err := json.Unmarshal([]byte(applyOut), &applied); err != nil {
		return nil, rec.IDs(), fmt.Errorf("the lease's candidate: %w", err)
	}
	if conflict, ok := applied["conflict"]; ok {
		finish, err := json.Marshal(map[string]any{"conflict": conflict})
		if err != nil {
			return nil, rec.IDs(), fmt.Errorf("the lease's conflict: %w", err)
		}
		return finish, rec.IDs(), nil
	}

	push := &nodetools.Push{Grant: g, Repo: repo, Workspace: workspace, Recorder: rec, PushTo: b.Code.PushTo}
	if _, err := push.Tool().Call(ctx, map[string]any{}); err != nil {
		return nil, rec.IDs(), fmt.Errorf("the lease's candidate push: %w", err)
	}

	rs := &nodetools.RunSuite{Workspace: workspace, Recorder: rec, Run: suites, OnHost: suites == nil && r.hostSuites()}
	runOut, err := rs.Tool().Call(ctx, map[string]any{})
	if err != nil {
		return nil, rec.IDs(), fmt.Errorf("the lease's suites: %w", err)
	}
	var validation map[string]any
	if err := json.Unmarshal([]byte(runOut), &validation); err != nil {
		return nil, rec.IDs(), fmt.Errorf("the lease's suites: %w", err)
	}

	cfg, _, err := infer.Load(workspace)
	if err != nil {
		return nil, rec.IDs(), fmt.Errorf("the lease's validate.yml: %w", err)
	}

	verdict := "pass"
	if exit, _ := validation["exit"].(float64); exit != 0 {
		verdict = "fail"
	}
	host, _ := os.Hostname()
	finish, err := json.Marshal(map[string]any{
		"verdict":          verdict,
		"candidate":        map[string]any{"sha": applied["sha"], "tree": applied["tree"]},
		"validation":       validation,
		"suite_config_sha": cfg.Hash(),
		"environment":      "host:" + host,
	})
	if err != nil {
		return nil, rec.IDs(), fmt.Errorf("the lease's finish: %w", err)
	}
	return finish, rec.IDs(), nil
}
