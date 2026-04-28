package state_test

import (
	"path/filepath"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/state"
)

func setupState(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	state.SetStatePath(filepath.Join(tmp, "state.json"))
	t.Cleanup(func() { state.SetStatePath("") })
}

func TestAddAndGetAgent(t *testing.T) {
	setupState(t)

	def := &state.AgentDef{Name: "nova", TemplateSource: "./tpl", CreatedAt: time.Now().UTC()}
	if err := state.AddAgent(def); err != nil {
		t.Fatal(err)
	}

	got, ok, err := state.GetAgent("nova")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected agent to be found")
	}
	if got.Name != "nova" {
		t.Errorf("name: got %q want nova", got.Name)
	}
}

func TestAddDuplicate(t *testing.T) {
	setupState(t)

	def := &state.AgentDef{Name: "larry"}
	if err := state.AddAgent(def); err != nil {
		t.Fatal(err)
	}
	if err := state.AddAgent(def); err == nil {
		t.Error("expected error on duplicate add, got nil")
	}
}

func TestRemoveAgent(t *testing.T) {
	setupState(t)

	if err := state.AddAgent(&state.AgentDef{Name: "larry"}); err != nil {
		t.Fatal(err)
	}
	found, err := state.RemoveAgent("larry")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Error("expected found=true")
	}
	_, ok, _ := state.GetAgent("larry")
	if ok {
		t.Error("agent should not exist after removal")
	}

	// Remove non-existent — should return false, nil.
	found, err = state.RemoveAgent("nobody")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if found {
		t.Error("expected found=false for missing agent")
	}
}

func TestListAgentsSorted(t *testing.T) {
	setupState(t)

	names := []string{"zara", "alice", "bob"}
	for _, n := range names {
		if err := state.AddAgent(&state.AgentDef{Name: n}); err != nil {
			t.Fatal(err)
		}
	}
	agents, err := state.ListAgents()
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 3 {
		t.Fatalf("expected 3 agents, got %d", len(agents))
	}
	if agents[0].Name != "alice" || agents[1].Name != "bob" || agents[2].Name != "zara" {
		t.Errorf("not sorted: %v", func() []string {
			var ns []string
			for _, a := range agents {
				ns = append(ns, a.Name)
			}
			return ns
		}())
	}
}

func TestUpdateAgent(t *testing.T) {
	setupState(t)

	if err := state.AddAgent(&state.AgentDef{Name: "nova"}); err != nil {
		t.Fatal(err)
	}
	if err := state.UpdateAgent("nova", func(a *state.AgentDef) {
		a.Description = "orchestrator"
	}); err != nil {
		t.Fatal(err)
	}
	got, _, _ := state.GetAgent("nova")
	if got.Description != "orchestrator" {
		t.Errorf("description: got %q want orchestrator", got.Description)
	}

	if err := state.UpdateAgent("nobody", func(a *state.AgentDef) {}); err == nil {
		t.Error("expected error for missing agent")
	}
}

func TestEmptyStateFile(t *testing.T) {
	setupState(t)

	agents, err := state.ListAgents()
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 0 {
		t.Errorf("expected empty agent list, got %d", len(agents))
	}
}
