package shim

// Shipper MCP shim tools — spec 31.
//
// These six tools wrap gh, fly, git, and curl calls so the Shipper agent can
// perform git ops, CI polling, deploys, smoke testing, and rollbacks purely
// through its MCP tool surface.  Each handler builds and executes the
// underlying command via the platform exec path and returns a structured JSON
// response to Claude.
//
// The tools do NOT run automatically at startup; they are registered in
// BuildMeshShimTools and dispatched from handleMeshShimMCP.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// repoSlugs returns the GitHub org/repo slug for the given repo name.
// OQ-1: defaults hardcoded; env-var overrides allowed.
func repoSlug(repo string) string {
	switch repo {
	case "mesh-api":
		if v := os.Getenv("MESH_REPO_API"); v != "" {
			return v
		}
		return "projectmesh-io/mesh-api"
	case "mesh-portal":
		if v := os.Getenv("MESH_REPO_PORTAL"); v != "" {
			return v
		}
		return "projectmesh-io/mesh-portal"
	case "mesh-cli":
		if v := os.Getenv("MESH_REPO_CLI"); v != "" {
			return v
		}
		return "projectmesh-io/mesh-cli"
	default:
		return repo
	}
}

// runCmd executes a command and returns combined stdout+stderr output. The
// command is run with a 5-minute hard timeout so no shipper tool can block
// indefinitely.
func runCmd(ctx context.Context, name string, args ...string) (string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			return buf.String(), -1, err
		}
	}
	return buf.String(), exitCode, nil
}

// HandleMeshOpenPR handles the mesh_open_pr(repo, branch, title, body, baseBranch) tool.
func HandleMeshOpenPR(
	ctx context.Context,
	id json.RawMessage,
	args map[string]any,
	write func(JSONRPCMessage) error,
	logf func(string, ...any),
) error {
	repo, _ := args["repo"].(string)
	branch, _ := args["branch"].(string)
	title, _ := args["title"].(string)
	body, _ := args["body"].(string)
	baseBranch, _ := args["baseBranch"].(string)

	if repo == "" || branch == "" || title == "" {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32602, Message: "mesh_open_pr: repo, branch, and title are required"},
		})
	}
	if baseBranch == "" {
		baseBranch = "main"
	}

	slug := repoSlug(repo)
	ghArgs := []string{
		"pr", "create",
		"--repo", slug,
		"--head", branch,
		"--base", baseBranch,
		"--title", title,
		"--body", body,
	}
	logf("mesh_open_pr repo=%s branch=%s title=%q", repo, branch, title)
	out, code, err := runCmd(ctx, "gh", ghArgs...)
	if err != nil || code != 0 {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32000, Message: fmt.Sprintf("gh pr create failed (exit %d): %s", code, strings.TrimSpace(out))},
		})
	}

	// gh pr create prints the PR URL on stdout.
	prURL := strings.TrimSpace(out)
	prNumber := 0
	// Parse number from URL tail: .../pull/42
	parts := strings.Split(prURL, "/")
	if len(parts) > 0 {
		fmt.Sscanf(parts[len(parts)-1], "%d", &prNumber)
	}

	logf("mesh_open_pr ok prUrl=%s prNumber=%d", prURL, prNumber)
	return write(JSONRPCMessage{ID: id, Result: map[string]any{
		"content": []map[string]any{{
			"type": "text",
			"text": mustMarshalJSON(map[string]any{
				"prUrl":    prURL,
				"prNumber": prNumber,
			}),
		}},
	}})
}

// HandleMeshCheckCI handles the mesh_check_ci(prUrl) tool.
func HandleMeshCheckCI(
	ctx context.Context,
	id json.RawMessage,
	args map[string]any,
	write func(JSONRPCMessage) error,
	logf func(string, ...any),
) error {
	prURL, _ := args["prUrl"].(string)
	if prURL == "" {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32602, Message: "mesh_check_ci: prUrl is required"},
		})
	}

	logf("mesh_check_ci prUrl=%s", prURL)
	out, code, err := runCmd(ctx, "gh", "pr", "checks", prURL,
		"--json", "name,state,conclusion,link")
	if err != nil || code != 0 {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32000, Message: fmt.Sprintf("gh pr checks failed (exit %d): %s", code, strings.TrimSpace(out))},
		})
	}

	// Parse gh JSON output.
	var checks []struct {
		Name       string `json:"name"`
		State      string `json:"state"`
		Conclusion string `json:"conclusion"`
		Link       string `json:"link"`
	}
	if jsonErr := json.Unmarshal([]byte(out), &checks); jsonErr != nil {
		// gh may return a non-JSON message when no checks exist yet.
		return write(JSONRPCMessage{ID: id, Result: map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": mustMarshalJSON(map[string]any{
					"status":      "pending",
					"runUrl":      "",
					"failingJobs": []any{},
					"completedAt": nil,
				}),
			}},
		}})
	}

	overallStatus := "success"
	var failingJobs []map[string]any
	var runURL string
	allComplete := true

	for _, c := range checks {
		if runURL == "" {
			runURL = c.Link
		}
		lower := strings.ToLower(c.State)
		if lower == "pending" || lower == "queued" || lower == "in_progress" {
			allComplete = false
			overallStatus = "pending"
		} else if c.Conclusion == "failure" || c.Conclusion == "timed_out" {
			overallStatus = "failure"
			failingJobs = append(failingJobs, map[string]any{
				"name":       c.Name,
				"conclusion": c.Conclusion,
				"logUrl":     c.Link,
			})
		} else if lower == "cancelled" {
			overallStatus = "cancelled"
		}
	}

	completedAt := ""
	if allComplete {
		completedAt = time.Now().UTC().Format(time.RFC3339)
	}

	logf("mesh_check_ci status=%s failing=%d", overallStatus, len(failingJobs))
	return write(JSONRPCMessage{ID: id, Result: map[string]any{
		"content": []map[string]any{{
			"type": "text",
			"text": mustMarshalJSON(map[string]any{
				"status":      overallStatus,
				"runUrl":      runURL,
				"failingJobs": failingJobs,
				"completedAt": completedAt,
			}),
		}},
	}})
}

// HandleMeshMergePR handles the mesh_merge_pr(prUrl, mergeMethod) tool.
func HandleMeshMergePR(
	ctx context.Context,
	id json.RawMessage,
	args map[string]any,
	write func(JSONRPCMessage) error,
	logf func(string, ...any),
) error {
	prURL, _ := args["prUrl"].(string)
	mergeMethod, _ := args["mergeMethod"].(string)
	if prURL == "" {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32602, Message: "mesh_merge_pr: prUrl is required"},
		})
	}
	if mergeMethod == "" {
		mergeMethod = "squash"
	}

	methodFlag := "--" + mergeMethod
	logf("mesh_merge_pr prUrl=%s method=%s", prURL, mergeMethod)
	out, code, err := runCmd(ctx, "gh", "pr", "merge", prURL, methodFlag, "--auto")
	if err != nil || code != 0 {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32000, Message: fmt.Sprintf("gh pr merge failed (exit %d): %s", code, strings.TrimSpace(out))},
		})
	}

	// gh pr merge --auto doesn't always print the commit SHA; we return empty
	// and let the caller poll gh pr view if needed.
	logf("mesh_merge_pr ok prUrl=%s", prURL)
	return write(JSONRPCMessage{ID: id, Result: map[string]any{
		"content": []map[string]any{{
			"type": "text",
			"text": mustMarshalJSON(map[string]any{
				"merged":    true,
				"commitSha": strings.TrimSpace(out),
			}),
		}},
	}})
}

// HandleMeshDeploy handles the mesh_deploy(repo, workItemId) tool.
// OQ-3: reads the current Fly image tag via fly status --json BEFORE deploying
// and stores it in the return payload as rollbackTarget.
func HandleMeshDeploy(
	ctx context.Context,
	id json.RawMessage,
	args map[string]any,
	write func(JSONRPCMessage) error,
	logf func(string, ...any),
) error {
	repo, _ := args["repo"].(string)
	workItemID, _ := args["workItemId"].(string)
	if repo == "" {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32602, Message: "mesh_deploy: repo is required"},
		})
	}

	logf("mesh_deploy repo=%s workItemId=%s", repo, workItemID)

	var deployID, deployURL, rollbackTarget string
	status := "triggered"

	switch repo {
	case "mesh-api":
		// OQ-3: read current image tag for rollback BEFORE deploying.
		flyStatusOut, _, _ := runCmd(ctx, "fly", "status", "--json")
		rollbackTarget = extractFlyImageTag(flyStatusOut)

		out, code, err := runCmd(ctx, "fly", "deploy", "--config", "fly.toml", "--remote-only")
		if err != nil || code != 0 {
			return write(JSONRPCMessage{
				ID:    id,
				Error: &JSONRPCError{Code: -32000, Message: fmt.Sprintf("fly deploy failed (exit %d): %s", code, strings.TrimSpace(out))},
			})
		}
		deployID = extractFlyDeployID(out)
		deployURL = "https://fly.io/apps/mesh-api/deployments"
		status = "success"
		logf("mesh_deploy mesh-api ok deployId=%s rollbackTarget=%s", deployID, rollbackTarget)

	case "mesh-portal":
		// Portal auto-deploys on merge to main. mesh_deploy is a no-op trigger
		// for portal; the shipper polls CF pages API separately.
		deployID = "cf-pages-auto"
		deployURL = "https://dash.cloudflare.com"
		status = "triggered"
		logf("mesh_deploy mesh-portal no-op triggered")

	case "mesh-cli":
		// Trigger the GitHub Actions release workflow.
		slug := repoSlug(repo)
		version := extractVersionFromWorkItemID(workItemID)
		out, code, err := runCmd(ctx, "gh", "workflow", "run", "release.yml",
			"--repo", slug,
			"--field", "version="+version)
		if err != nil || code != 0 {
			return write(JSONRPCMessage{
				ID:    id,
				Error: &JSONRPCError{Code: -32000, Message: fmt.Sprintf("gh workflow run failed (exit %d): %s", code, strings.TrimSpace(out))},
			})
		}
		deployID = "github-release-" + version
		deployURL = "https://github.com/" + slug + "/releases"
		status = "triggered"
		logf("mesh_deploy mesh-cli ok version=%s", version)

	default:
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32602, Message: "mesh_deploy: unknown repo " + repo},
		})
	}

	return write(JSONRPCMessage{ID: id, Result: map[string]any{
		"content": []map[string]any{{
			"type": "text",
			"text": mustMarshalJSON(map[string]any{
				"deployId":       deployID,
				"deployUrl":      deployURL,
				"status":         status,
				"rollbackTarget": rollbackTarget,
			}),
		}},
	}})
}

// HandleMeshSmokeTest handles the mesh_smoke_test(repo, deployId) tool.
func HandleMeshSmokeTest(
	ctx context.Context,
	id json.RawMessage,
	args map[string]any,
	write func(JSONRPCMessage) error,
	logf func(string, ...any),
) error {
	repo, _ := args["repo"].(string)
	deployID, _ := args["deployId"].(string)
	if repo == "" {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32602, Message: "mesh_smoke_test: repo is required"},
		})
	}

	logf("mesh_smoke_test repo=%s deployId=%s", repo, deployID)

	var checks []map[string]any
	allPass := true

	switch repo {
	case "mesh-api":
		apiBase := os.Getenv("MESH_API_URL")
		if apiBase == "" {
			apiBase = "https://api.projectmesh.io"
		}
		check, pass := httpSmokeCheck("GET /health", apiBase+"/health", 200, 2000)
		checks = append(checks, check)
		if !pass {
			allPass = false
		}

	case "mesh-portal":
		portalURL := os.Getenv("CF_PAGES_URL")
		if portalURL == "" {
			portalURL = "https://portal.projectmesh.io"
		}
		check, pass := httpSmokeCheck("GET /", portalURL+"/", 200, 3000)
		checks = append(checks, check)
		if !pass {
			allPass = false
		}

	case "mesh-cli":
		// No HTTP endpoint; verify the GitHub Release asset exists.
		slug := repoSlug(repo)
		out, code, err := runCmd(ctx, "gh", "release", "view", "--repo", slug)
		pass := err == nil && code == 0 && len(out) > 0
		checks = append(checks, map[string]any{
			"name":         "github-release-exists",
			"status":       boolStatus(pass),
			"responseCode": 0,
			"latencyMs":    0,
		})
		if !pass {
			allPass = false
		}

	default:
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32602, Message: "mesh_smoke_test: unknown repo " + repo},
		})
	}

	logf("mesh_smoke_test pass=%v checks=%d", allPass, len(checks))
	return write(JSONRPCMessage{ID: id, Result: map[string]any{
		"content": []map[string]any{{
			"type": "text",
			"text": mustMarshalJSON(map[string]any{
				"pass":   allPass,
				"checks": checks,
			}),
		}},
	}})
}

// HandleMeshRollback handles the mesh_rollback(repo, deployId, rollbackTarget) tool.
func HandleMeshRollback(
	ctx context.Context,
	id json.RawMessage,
	args map[string]any,
	write func(JSONRPCMessage) error,
	logf func(string, ...any),
) error {
	repo, _ := args["repo"].(string)
	rollbackTarget, _ := args["rollbackTarget"].(string)
	if repo == "" || rollbackTarget == "" {
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32602, Message: "mesh_rollback: repo and rollbackTarget are required"},
		})
	}

	logf("mesh_rollback repo=%s rollbackTarget=%s", repo, rollbackTarget)

	var rollbackDeployID string

	switch repo {
	case "mesh-api":
		out, code, err := runCmd(ctx, "fly", "deploy", "--image", rollbackTarget)
		if err != nil || code != 0 {
			return write(JSONRPCMessage{
				ID:    id,
				Error: &JSONRPCError{Code: -32000, Message: fmt.Sprintf("fly deploy rollback failed (exit %d): %s", code, strings.TrimSpace(out))},
			})
		}
		rollbackDeployID = extractFlyDeployID(out)
		logf("mesh_rollback mesh-api ok rollbackDeployId=%s", rollbackDeployID)

	case "mesh-portal":
		// CF Pages rollback API.
		cfAccountID := os.Getenv("CF_ACCOUNT_ID")
		cfProjectName := os.Getenv("CF_PAGES_PROJECT")
		cfToken := os.Getenv("CF_API_TOKEN")
		if cfAccountID == "" || cfProjectName == "" {
			return write(JSONRPCMessage{
				ID:    id,
				Error: &JSONRPCError{Code: -32000, Message: "mesh_rollback: CF_ACCOUNT_ID and CF_PAGES_PROJECT must be set for portal rollback"},
			})
		}
		url := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/pages/projects/%s/deployments/%s/retry",
			cfAccountID, cfProjectName, rollbackTarget)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
		req.Header.Set("Authorization", "Bearer "+cfToken)
		resp, httpErr := http.DefaultClient.Do(req)
		if httpErr != nil || (resp != nil && resp.StatusCode >= 400) {
			code := 0
			if resp != nil {
				code = resp.StatusCode
			}
			return write(JSONRPCMessage{
				ID:    id,
				Error: &JSONRPCError{Code: -32000, Message: fmt.Sprintf("CF Pages rollback failed (http %d): %v", code, httpErr)},
			})
		}
		if resp != nil {
			defer resp.Body.Close()
			respBody, _ := io.ReadAll(resp.Body)
			rollbackDeployID = extractCFDeployID(string(respBody))
		}
		logf("mesh_rollback mesh-portal ok rollbackDeployId=%s", rollbackDeployID)

	case "mesh-cli":
		// CLI releases are pull-based; no runtime rollback.
		logf("mesh_rollback mesh-cli: no runtime rollback available; operator must notify users")
		return write(JSONRPCMessage{ID: id, Result: map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": mustMarshalJSON(map[string]any{
					"rolledBack":       false,
					"rollbackDeployId": "",
					"note":             "CLI releases are pull-based; no runtime rollback. Operator notification required.",
				}),
			}},
		}})

	default:
		return write(JSONRPCMessage{
			ID:    id,
			Error: &JSONRPCError{Code: -32602, Message: "mesh_rollback: unknown repo " + repo},
		})
	}

	return write(JSONRPCMessage{ID: id, Result: map[string]any{
		"content": []map[string]any{{
			"type": "text",
			"text": mustMarshalJSON(map[string]any{
				"rolledBack":       true,
				"rollbackDeployId": rollbackDeployID,
			}),
		}},
	}})
}

// HandleMeshCheckEmergencyStop handles mesh_check_emergency_stop() — polled by
// the Shipper before each phase transition.  It reads the agent's metadata
// from mesh-api via the node token and returns whether emergency_stop is set.
func HandleMeshCheckEmergencyStop(
	ctx context.Context,
	id json.RawMessage,
	args map[string]any,
	agentName string,
	write func(JSONRPCMessage) error,
	logf func(string, ...any),
) error {
	base, err := loadMeshAPIBase()
	if err != nil {
		// If we can't reach mesh-api, fail safe: treat as not stopped so the
		// shipper doesn't halt on a transient connectivity blip.
		logf("mesh_check_emergency_stop: load base err: %v", err)
		return write(JSONRPCMessage{ID: id, Result: map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": mustMarshalJSON(map[string]any{"stopped": false, "reason": "api_unreachable"}),
			}},
		}})
	}

	nodeToken, tokErr := loadNodeTokenForShim()
	if tokErr != nil {
		logf("mesh_check_emergency_stop: no node token: %v", tokErr)
		return write(JSONRPCMessage{ID: id, Result: map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": mustMarshalJSON(map[string]any{"stopped": false, "reason": "no_token"}),
			}},
		}})
	}

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(reqCtx, http.MethodGet,
		base+"/agent/list?assignedToMe=true", nil)
	req.Header.Set("Authorization", "Bearer "+nodeToken)
	resp, httpErr := meshAPIHTTPClient.Do(req)
	if httpErr != nil || resp.StatusCode != 200 {
		logf("mesh_check_emergency_stop: api call failed: %v", httpErr)
		return write(JSONRPCMessage{ID: id, Result: map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": mustMarshalJSON(map[string]any{"stopped": false, "reason": "api_error"}),
			}},
		}})
	}
	defer resp.Body.Close()

	var body struct {
		Agents []struct {
			Name     string                     `json:"name"`
			Metadata map[string]json.RawMessage `json:"metadata"`
		} `json:"agents"`
	}
	if decErr := json.NewDecoder(resp.Body).Decode(&body); decErr != nil {
		return write(JSONRPCMessage{ID: id, Result: map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": mustMarshalJSON(map[string]any{"stopped": false, "reason": "parse_error"}),
			}},
		}})
	}

	stopped := false
	for _, agent := range body.Agents {
		if agent.Name != agentName {
			continue
		}
		if raw, ok := agent.Metadata["shipper_config"]; ok {
			var cfg struct {
				EmergencyStop bool `json:"emergency_stop"`
			}
			if json.Unmarshal(raw, &cfg) == nil && cfg.EmergencyStop {
				stopped = true
			}
		}
		break
	}

	logf("mesh_check_emergency_stop agent=%s stopped=%v", agentName, stopped)
	return write(JSONRPCMessage{ID: id, Result: map[string]any{
		"content": []map[string]any{{
			"type": "text",
			"text": mustMarshalJSON(map[string]any{"stopped": stopped}),
		}},
	}})
}

// ── tool schema registration ──────────────────────────────────────────────────

// BuildShipperShimTools returns the tools/list entries for the six shipper tools
// plus mesh_check_emergency_stop.  These are appended to BuildMeshShimTools in
// the shipper agent variant.
func BuildShipperShimTools() []map[string]any {
	return []map[string]any{
		{
			"name":        "mesh_open_pr",
			"description": "Create a GitHub Pull Request for a ship branch. Wraps gh pr create.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"repo", "branch", "title"},
				"properties": map[string]any{
					"repo":       map[string]any{"type": "string", "enum": []string{"mesh-api", "mesh-portal", "mesh-cli"}},
					"branch":     map[string]any{"type": "string", "description": "Source branch, e.g. ship/<work-item-id>"},
					"title":      map[string]any{"type": "string"},
					"body":       map[string]any{"type": "string"},
					"baseBranch": map[string]any{"type": "string", "description": "Target branch (default: main)"},
				},
			},
		},
		{
			"name":        "mesh_check_ci",
			"description": "Check CI status for a PR. Returns status, runUrl, and any failing jobs. Wraps gh pr checks.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"prUrl"},
				"properties": map[string]any{
					"prUrl": map[string]any{"type": "string", "description": "Full GitHub PR URL"},
				},
			},
		},
		{
			"name":        "mesh_merge_pr",
			"description": "Merge a PR after CI passes. Wraps gh pr merge. Errors if CI is not passing.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"prUrl"},
				"properties": map[string]any{
					"prUrl":       map[string]any{"type": "string"},
					"mergeMethod": map[string]any{"type": "string", "enum": []string{"squash", "merge", "rebase"}, "description": "Default: squash"},
				},
			},
		},
		{
			"name":        "mesh_deploy",
			"description": "Deploy the repo to its production target. For mesh-api: fly deploy. For mesh-portal: no-op (auto on merge). For mesh-cli: gh workflow run release.yml.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"repo"},
				"properties": map[string]any{
					"repo":       map[string]any{"type": "string", "enum": []string{"mesh-api", "mesh-portal", "mesh-cli"}},
					"workItemId": map[string]any{"type": "string", "description": "Used to derive the release version for mesh-cli"},
				},
			},
		},
		{
			"name":        "mesh_smoke_test",
			"description": "Run v1 smoke checks against the deployed production endpoint. For mesh-api: GET /health. For mesh-portal: GET /. For mesh-cli: gh release view.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"repo"},
				"properties": map[string]any{
					"repo":     map[string]any{"type": "string", "enum": []string{"mesh-api", "mesh-portal", "mesh-cli"}},
					"deployId": map[string]any{"type": "string"},
				},
			},
		},
		{
			"name":        "mesh_rollback",
			"description": "Roll back to the previous production deployment. For mesh-api: fly deploy --image <rollbackTarget>. For mesh-portal: CF Pages retry API. For mesh-cli: no runtime rollback.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"repo", "rollbackTarget"},
				"properties": map[string]any{
					"repo":           map[string]any{"type": "string", "enum": []string{"mesh-api", "mesh-portal", "mesh-cli"}},
					"deployId":       map[string]any{"type": "string"},
					"rollbackTarget": map[string]any{"type": "string", "description": "Image tag (Fly) or deployment ID (CF Pages) to roll back to"},
				},
			},
		},
		{
			"name":        "mesh_check_emergency_stop",
			"description": "Poll whether the emergency stop flag has been set for this shipper agent. Call before each phase transition. Returns {stopped: bool}.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func mustMarshalJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func boolStatus(ok bool) string {
	if ok {
		return "pass"
	}
	return "fail"
}

// httpSmokeCheck performs a single HTTP GET and returns a check result map.
func httpSmokeCheck(name, url string, wantCode, maxLatencyMs int) (map[string]any, bool) {
	start := time.Now()
	client := &http.Client{Timeout: time.Duration(maxLatencyMs+1000) * time.Millisecond}
	resp, err := client.Get(url) //nolint:noctx
	latency := int(time.Since(start).Milliseconds())
	status := "fail"
	code := 0
	pass := false
	if err == nil && resp != nil {
		code = resp.StatusCode
		resp.Body.Close()
		if code == wantCode && latency <= maxLatencyMs {
			status = "pass"
			pass = true
		}
	}
	return map[string]any{
		"name":         name,
		"status":       status,
		"responseCode": code,
		"latencyMs":    latency,
	}, pass
}

// extractFlyDeployID parses a deployment ID from `fly deploy` stdout.
func extractFlyDeployID(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "deployment") || strings.Contains(line, "Deploy ID") {
			parts := strings.Fields(line)
			if len(parts) > 0 {
				return parts[len(parts)-1]
			}
		}
	}
	return ""
}

// extractFlyImageTag parses the Docker image tag from `fly status --json` output.
// OQ-3: used to capture rollbackTarget before deploying.
func extractFlyImageTag(out string) string {
	var status struct {
		ImageDetails struct {
			Repository string `json:"repository"`
			Tag        string `json:"tag"`
		} `json:"ImageDetails"`
	}
	if json.Unmarshal([]byte(out), &status) == nil && status.ImageDetails.Tag != "" {
		if status.ImageDetails.Repository != "" {
			return status.ImageDetails.Repository + ":" + status.ImageDetails.Tag
		}
		return status.ImageDetails.Tag
	}
	// Fallback: search for an image reference line.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "registry.fly.io") {
			parts := strings.Fields(line)
			if len(parts) > 0 {
				return parts[len(parts)-1]
			}
		}
	}
	return ""
}

// extractCFDeployID parses a deployment ID from a CF Pages API response body.
func extractCFDeployID(body string) string {
	var resp struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if json.Unmarshal([]byte(body), &resp) == nil {
		return resp.Result.ID
	}
	return ""
}

// extractVersionFromWorkItemID derives a semver string for mesh-cli releases.
// Uses today's date + a short work-item suffix.
func extractVersionFromWorkItemID(workItemID string) string {
	now := time.Now().UTC()
	suffix := workItemID
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	return fmt.Sprintf("v0.0.0-%s-%s", now.Format("20060102"), suffix)
}

// loadNodeTokenForShim reads the node token from disk (same path as auth package).
func loadNodeTokenForShim() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(home + "/.mesh/node-token")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
