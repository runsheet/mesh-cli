package harness

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestArgsModel: the job's model reaches the command line; a job
// without one leaves the CLI's own default alone.
func TestArgsModel(t *testing.T) {
	c := &ClaudeCode{}
	args := c.Args(Job{Prompt: "hi", Model: "sonnet"})
	found := false
	for i, a := range args {
		if a == "--model" && i+1 < len(args) && args[i+1] == "sonnet" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no --model sonnet in %v", args)
	}
	for _, a := range c.Args(Job{Prompt: "hi"}) {
		if a == "--model" {
			t.Fatal("a job with no model must not pass --model")
		}
	}
}

// A node build session cannot call the hosted mesh_finish or mesh_fail
// — each records the finish on the platform before the node has pushed
// (goal a-build-pushes-before-it-finishes) — on the host or in a pod:
// any allowed pattern that matches one is outweighed by
// --disallowedTools naming it. It finishes on the node's own server
// instead, whose mesh_finish lands on Result.WorkCalls as the hosted
// call's did; evaluation and plan sessions keep the hosted work tools.
func TestBuildSessionFinishesOnTheNodesOwnTools(t *testing.T) {
	c := &ClaudeCode{MeshBinary: "/usr/local/bin/mesh", ExtraArgs: []string{"--dangerously-skip-permissions"}}
	servers := map[string]any{BuiltinMCP: map[string]any{"type": "http", "url": "https://gw/mcp"}}
	build := Job{Kind: KindBuild, MCPServers: servers}
	mcpPath := "/tmp/mesh-home-1/mcp.json"
	for name, args := range map[string][]string{"Args": c.Args(build), "SandboxArgs": c.SandboxArgs(build, mcpPath)} {
		allowed := strings.Split(flagAt(args, "--allowedTools"), ",")
		denied := strings.Split(flagAt(args, "--disallowedTools"), ",")
		for _, hosted := range []string{"mcp__runsheet__mesh_finish", "mcp__runsheet__mesh_fail"} {
			if toolAllowed(hosted, allowed, denied) {
				t.Errorf("%s allows %s: allowed %v, disallowed %v", name, hosted, allowed, denied)
			}
		}
		for _, kept := range []string{"mcp__runsheet__mesh_document", "mcp__runsheet__mesh_progress", "mcp__mesh-node__mesh_finish", "mcp__mesh-node__mesh_fail"} {
			if !toolAllowed(kept, allowed, denied) {
				t.Errorf("%s does not allow %s: allowed %v, disallowed %v", name, kept, allowed, denied)
			}
		}
		if args[len(args)-1] != "--dangerously-skip-permissions" {
			t.Errorf("%s: ExtraArgs not last: %v", name, args)
		}
	}
	// the session has a mesh_finish tool: the node's server, on the host
	// the mesh binary's session-tools, in a pod the script beside mcp.json
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(flagAt(c.Args(build), "--mcp-config")), &cfg); err != nil {
		t.Fatal(err)
	}
	if s := cfg.MCPServers[NodeWorkMCP]; s.Command != "/usr/local/bin/mesh" || !reflect.DeepEqual(s.Args, []string{SessionToolsCommand}) {
		t.Errorf("host %s = %+v", NodeWorkMCP, s)
	}
	if s := sandboxServers(build, mcpPath)[NodeWorkMCP].(map[string]any); s["command"] != "node" || !reflect.DeepEqual(s["args"], []string{"/tmp/mesh-home-1/" + workScriptName}) {
		t.Errorf("pod %s = %v", NodeWorkMCP, s)
	}
	if reply, ok := WorkToolReply("mesh_finish"); !ok || reply == "" {
		t.Errorf("the node's server serves no mesh_finish")
	}
	// its call lands on WorkCalls with the item, branch and head
	f, err := os.Open("testdata/stream_node_finish.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	res := ParseStream(f, nil)
	if got := res.WorkCalls["mesh_finish"]; !reflect.DeepEqual(got, finishInput) {
		t.Errorf("WorkCalls[mesh_finish] = %v, want %v", got, finishInput)
	}
	// evaluations and plans finish on the hosted tools, as before
	for _, kind := range []Kind{KindEvaluate, KindPlan} {
		job := Job{Kind: kind, MCPServers: servers}
		for name, args := range map[string][]string{"Args": c.Args(job), "SandboxArgs": c.SandboxArgs(job, mcpPath)} {
			if flagAt(args, "--allowedTools") != "mcp__runsheet__*" || slices.Contains(args, "--disallowedTools") {
				t.Errorf("%s %s = %v", kind, name, args)
			}
		}
	}
}

// toolAllowed is Claude Code's rule for a tool in print mode: allowed
// when a pattern (exact, or a prefix ending in *) matches it and no
// disallowed entry does — a deny always wins.
func toolAllowed(tool string, allowed, denied []string) bool {
	match := func(p string) bool {
		if prefix, ok := strings.CutSuffix(p, "*"); ok {
			return strings.HasPrefix(tool, prefix)
		}
		return p == tool
	}
	if slices.ContainsFunc(denied, match) {
		return false
	}
	return slices.ContainsFunc(allowed, match)
}
