package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// VerdictResult reads an evaluator's verdict as its session handed it to
// the node's mesh_result (jobresult, ADR-041): the `verdict` object's
// passed, its notes with one READING line per reading appended — so the
// node's READING handling (ParseReadings, the notes' truncation that
// keeps them) is one path — and its failed clauses (`failedClauses`, as
// finishJob names them, or `failed_clauses`), each with both halves,
// duplicates dropped, at most ClauseMax. ok is false when the verdict
// carries no passed the node can read: such a result is no verdict.
func VerdictResult(in map[string]any) (passed bool, notes string, clauses []FailedClause, ok bool) {
	switch v := in["passed"].(type) {
	case bool:
		passed = v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "pass", "passed":
			passed = true
		case "false", "fail", "failed":
		default:
			return false, "", nil, false
		}
	default:
		return false, "", nil, false
	}
	notes = strings.TrimSpace(callString(in["notes"]))
	have := map[string]bool{}
	for _, r := range ParseReadings(notes) {
		have[r.Clause] = true
	}
	var lines []string
	for _, e := range callList(in["readings"]) {
		clause := strings.TrimSpace(callString(e["clause"]))
		value := strings.TrimSpace(callString(e["reading"]))
		if value == "" {
			value = strings.TrimSpace(callString(e["value"]))
		}
		if clause == "" || value == "" || have[clause] {
			continue
		}
		have[clause] = true
		lines = append(lines, fmt.Sprintf("%s %s %s %s", ClauseReading, clause, ClauseSep, value))
	}
	if len(lines) > 0 {
		notes = strings.TrimSpace(notes + "\n\n" + strings.Join(lines, "\n"))
	}
	seen := map[string]bool{}
	failed := in["failedClauses"]
	if failed == nil {
		failed = in["failed_clauses"]
	}
	for _, e := range callList(failed) {
		clause := strings.TrimSpace(callString(e["clause"]))
		satisfy := strings.TrimSpace(callString(e["satisfy"]))
		if clause == "" || satisfy == "" || seen[clause] {
			continue
		}
		seen[clause] = true
		if clauses = append(clauses, FailedClause{Clause: clause, Satisfy: satisfy}); len(clauses) == ClauseMax {
			break
		}
	}
	return passed, notes, clauses, true
}

// callString is a work tool argument as a string ("" when it is not one).
func callString(v any) string {
	s, _ := v.(string)
	return s
}

// callList is a work tool argument as a list of objects — given as an
// array, or as an array encoded in a JSON string — its other entries
// dropped.
func callList(v any) []map[string]any {
	if s, ok := v.(string); ok {
		var decoded any
		if json.Unmarshal([]byte(s), &decoded) != nil {
			return nil
		}
		v = decoded
	}
	list, _ := v.([]any)
	var out []map[string]any
	for _, e := range list {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// NodeWorkMCP is the node's own MCP server a build session finishes on
// (goal a-build-pushes-before-it-finishes): its mesh_finish and
// mesh_fail answer at once and record nothing on the platform, so the
// call reaches the node only as a tool_use on the session's stream —
// Result.WorkCalls by the tool's own name, as the hosted call's did —
// and the node pushes the head before it hands the finish in.
const NodeWorkMCP = "mesh-node"

// HostedFinishTools are the hosted runsheet tools a build session
// is not allowed (--disallowedTools): each records the finish or the
// failure on the platform before the node has pushed.
var HostedFinishTools = []string{
	"mcp__" + BuiltinMCP + "__mesh_finish",
	"mcp__" + BuiltinMCP + "__mesh_fail",
}

// jobFinishTools are the job tools a build session may not call on any
// server its brief names (Job.BriefServers): its failure and its release
// are the node's, on the node's own credential. Tool names of the job
// system's own (mesh_*), never a product's.
var jobFinishTools = []string{"mesh_fail", "mesh_release"}

// HostedVerdictTools are the hosted runsheet tools an evaluation
// session is not allowed: its verdict is the node's mesh_result
// (ADR-041), and runsheet refuses mesh_verdict to a session token in
// any case.
var HostedVerdictTools = []string{
	"mcp__" + BuiltinMCP + "__mesh_verdict",
}

// HostedDisallowed is the hosted runsheet tools job's session is not
// allowed, by its kind: a build's finish and fail, an evaluation's
// verdict; none for a job without the hosted runsheet server.
func HostedDisallowed(job Job) []string {
	if _, ok := job.MCPServers[BuiltinMCP]; !ok {
		return nil
	}
	switch job.Kind {
	case KindBuild:
		out := append([]string(nil), HostedFinishTools...)
		for _, srv := range job.BriefServers {
			if _, ok := job.MCPServers[srv.Name]; !ok {
				continue
			}
			for _, tool := range jobFinishTools {
				out = append(out, "mcp__"+srv.Name+"__"+tool)
			}
		}
		return out
	case KindEvaluate:
		return HostedVerdictTools
	}
	return nil
}

// WorkTool is one tool of the node's own MCP server: its name, schema
// and the fixed answer it gives the session.
type WorkTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Schema      map[string]any `json:"inputSchema"`
	Reply       string         `json:"reply"`
}

// NodeWorkTools are NodeWorkMCP's tools: mesh_finish and mesh_fail with
// the hosted tools' arguments.
func NodeWorkTools() []WorkTool {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	return []WorkTool{{
		Name:        "mesh_finish",
		Description: "Finish the item: the branch and head you built, or outcome=already_done with the evidence. The node pushes the branch and then hands the finish in.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"item_id":     str("the item's id"),
				"branch":      str("the item's branch"),
				"head_sha":    str("the commit the branch ends on (git rev-parse HEAD)"),
				"outcome":     str("already_done when main already holds the acceptance"),
				"evidence":    str("for already_done: the merged PRs or commits and why"),
				"description": str("the pull request's body"),
			},
			"required": []string{"item_id"},
		},
		Reply: "recorded: the node pushes the branch, then hands the finish in; end the session now",
	}, {
		Name:        "mesh_fail",
		Description: "Fail the item with why the work cannot be done. The node hands the failure in.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"item_id": str("the item's id"),
				"error":   str("why the work cannot be done"),
			},
			"required": []string{"item_id", "error"},
		},
		Reply: "recorded: the node hands the failure in; end the session now",
	}}
}

// WorkToolReply is the answer NodeWorkMCP gives a call to name, and
// whether it serves name at all.
func WorkToolReply(name string) (string, bool) {
	for _, t := range NodeWorkTools() {
		if t.Name == name {
			return t.Reply, true
		}
	}
	return "", false
}

// nodeFinishes says whether a session finishes on the node's own tools:
// a build given the hosted runsheet server.
func nodeFinishes(job Job) bool {
	_, ok := job.MCPServers[BuiltinMCP]
	return job.Kind == KindBuild && ok
}

// withNodeWork is servers with NodeWorkMCP beside them, servers itself
// left as it is.
func withNodeWork(servers map[string]any, server map[string]any) map[string]any {
	out := make(map[string]any, len(servers)+1)
	for k, v := range servers {
		out[k] = v
	}
	out[NodeWorkMCP] = server
	return out
}

// SessionToolsCommand is the mesh subcommand that serves NodeWorkMCP on
// stdio on the host.
const SessionToolsCommand = "session-tools"

// hostWorkServer is NodeWorkMCP on the host: the mesh binary's
// SessionToolsCommand.
func (c *ClaudeCode) hostWorkServer() map[string]any {
	bin := c.MeshBinary
	if bin == "" {
		bin = "mesh"
		if exe, err := os.Executable(); err == nil {
			bin = exe
		}
	}
	return map[string]any{"type": "stdio", "command": bin, "args": []string{SessionToolsCommand}}
}

// workScriptName is NodeWorkMCP's script in a pod, beside its mcp.json:
// the pod has no mesh binary, but the node image carries Node for claude.
const workScriptName = "mesh-work-tools.js"

// sandboxWorkServer is NodeWorkMCP in a pod whose mcp.json is mcpPath:
// node running WorkScript, written beside it.
func sandboxWorkServer(mcpPath string) map[string]any {
	return map[string]any{"type": "stdio", "command": "node", "args": []string{filepath.Join(filepath.Dir(mcpPath), workScriptName)}}
}

// WorkScript is NodeWorkMCP as a Node script: a newline-delimited
// JSON-RPC stdio MCP server answering NodeWorkTools' calls with their
// replies.
func WorkScript() []byte {
	list, _ := json.Marshal(NodeWorkTools())
	return []byte(`// mesh: the node's own work tools (` + NodeWorkMCP + `); written per session.
const tools = ` + string(list) + `;
const rl = require("readline").createInterface({ input: process.stdin });
const send = (m) => process.stdout.write(JSON.stringify(Object.assign({ jsonrpc: "2.0" }, m)) + "\n");
rl.on("line", (line) => {
  let m;
  try { m = JSON.parse(line); } catch (e) { return; }
  if (m.id === undefined || m.id === null) return;
  switch (m.method) {
    case "initialize":
      send({ id: m.id, result: { protocolVersion: (m.params && m.params.protocolVersion) || "2024-11-05", capabilities: { tools: {} }, serverInfo: { name: "` + NodeWorkMCP + `", version: "1" } } });
      break;
    case "tools/list":
      send({ id: m.id, result: { tools: tools.map((t) => ({ name: t.name, description: t.description, inputSchema: t.inputSchema })) } });
      break;
    case "tools/call": {
      const t = tools.find((t) => t.name === (m.params && m.params.name));
      if (!t) { send({ id: m.id, error: { code: -32602, message: "unknown tool" } }); break; }
      send({ id: m.id, result: { content: [{ type: "text", text: t.reply }] } });
      break;
    }
    case "ping":
      send({ id: m.id, result: {} });
      break;
    default:
      send({ id: m.id, error: { code: -32601, message: "method not found" } });
  }
});
`)
}
