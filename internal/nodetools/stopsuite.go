package nodetools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"projectmesh.io/mesh-cli/internal/mcptool"
)

// StopSuiteTool is the stop_suite node tool's name. It ends one lane of
// a run_suite run that is still going — a sharded step's one shard, or
// a step with no shards — cancelling that lane's command and tearing
// down its pod, while the run's other lanes go on. ADR-049's stop_suite.
const StopSuiteTool = "stop_suite"

// StopSuite is one lease's stop_suite tool, sharing the lease's
// RunSuite so it can reach a run_suite call's still-going lanes.
type StopSuite struct {
	Runs *RunSuite
}

// laneName is a step and (for a sharded step) its shard's name as
// validate.Run opens it: "<step>" with no index, "<step>-<index>" with
// one — the same name Options.OnLane registers.
func laneName(step string, index int) string {
	if index < 0 {
		return step
	}
	return fmt.Sprintf("%s-%d", step, index)
}

// Tool is stop_suite {run_id, step, index?, reason}.
func (s *StopSuite) Tool() mcptool.Tool {
	return mcptool.Tool{
		Name: StopSuiteTool,
		Description: "Stop one lane of a run_suite run that is still going: a sharded step's one shard (name step and " +
			"index), or a whole step with no shards (name step alone). That lane's command is cancelled and its pod " +
			"torn down at once; the run's other shards and steps go on. The stopped lane's result records stopped: " +
			"true, a non-zero exit and the reason given; run_suite's own exit is never 0 once one lane is stopped.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"run_id": map[string]any{"type": "string", "description": "the run to stop a lane of, as run_suite answered it"},
				"step":   map[string]any{"type": "string", "description": "the suite (step) name the lane belongs to"},
				"index": map[string]any{"type": "integer", "minimum": 0,
					"description": "the shard's 0-based index (as MESH_SHARD_INDEX); omit to stop a step with no shards"},
				"reason": map[string]any{"type": "string", "description": "why: recorded on the stopped lane's result"},
			},
			"required": []string{"run_id", "step", "reason"},
		},
		Call: s.call,
	}
}

func (s *StopSuite) call(_ context.Context, args map[string]any) (string, error) {
	id, _ := args["run_id"].(string)
	id = strings.TrimSpace(id)
	if id == "" {
		return "", errors.New("run_id names the run to stop a lane of, as run_suite answered it")
	}
	step, _ := args["step"].(string)
	step = strings.TrimSpace(step)
	if step == "" {
		return "", errors.New("step names the suite the lane to stop belongs to")
	}
	reason, _ := args["reason"].(string)
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "", errors.New("reason says why the lane is stopped")
	}
	index := -1
	if v, ok := args["index"]; ok && v != nil {
		var n float64
		switch x := v.(type) {
		case float64:
			n = x
		case int:
			n = float64(x)
		default:
			n = -1
		}
		if n < 0 || n != math.Trunc(n) {
			return "", errors.New("index is a shard's 0-based index")
		}
		index = int(n)
	}
	if s.Runs == nil {
		return "", errors.New("this lease has no run_suite tool to stop a lane of")
	}
	r := s.Runs.find(id)
	if r == nil {
		return "", fmt.Errorf("no run %q on this lease: run_suite answers a run's id when it starts one", id)
	}
	name := laneName(step, index)
	stop, ok := r.laneStop(name)
	if !ok {
		return "", fmt.Errorf("run %q names no lane %q: it may not have opened yet, or has already ended", id, name)
	}
	stop(reason)
	answer := map[string]any{"run_id": id, "step": step, "reason": reason, "stopped": true}
	if index >= 0 {
		answer["index"] = index
	}
	b, _ := json.Marshal(answer)
	return string(b), nil
}
