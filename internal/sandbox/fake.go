package sandbox

import (
	"context"
	"io"
	"sync"
)

// Fake is a Runtime that records every call and runs nothing: tests
// read what a session's pod would have been. Agent, when set, stands in
// for the agent's container (writing its stdout); Exit is what it ends
// with.
type Fake struct {
	mu sync.Mutex
	// VersionErr makes Version fail, as a node without podman does.
	VersionErr error
	Pods       []PodSpec
	Agents     []AgentSpec
	Sidecars   []SidecarSpec
	Removed    []string
	Calls      []string
	// Agent is the agent's container: it gets the spec and returns the
	// exit (nil: exit 0, nothing written).
	Agent func(ctx context.Context, spec AgentSpec) (Exit, error)
}

func (f *Fake) call(name string) {
	f.mu.Lock()
	f.Calls = append(f.Calls, name)
	f.mu.Unlock()
}

// Version implements Runtime.
func (f *Fake) Version(context.Context) (string, error) {
	f.call("version")
	if f.VersionErr != nil {
		return "", f.VersionErr
	}
	return "fake 1.0", nil
}

// CreatePod implements Runtime.
func (f *Fake) CreatePod(_ context.Context, spec PodSpec) (Pod, error) {
	f.call("create " + spec.Name)
	f.mu.Lock()
	f.Pods = append(f.Pods, spec)
	f.mu.Unlock()
	return Pod{Name: spec.Name}, nil
}

// RunAgent implements Runtime.
func (f *Fake) RunAgent(ctx context.Context, pod Pod, spec AgentSpec) (Exit, error) {
	f.call("agent " + pod.Name)
	f.mu.Lock()
	f.Agents = append(f.Agents, spec)
	agent := f.Agent
	f.mu.Unlock()
	if agent == nil {
		if spec.Stdin != nil {
			_, _ = io.Copy(io.Discard, spec.Stdin)
		}
		return Exit{}, nil
	}
	return agent(ctx, spec)
}

// AddSidecar implements Runtime.
func (f *Fake) AddSidecar(_ context.Context, pod Pod, spec SidecarSpec) error {
	f.call("sidecar " + pod.Name + " " + spec.Name)
	f.mu.Lock()
	f.Sidecars = append(f.Sidecars, spec)
	f.mu.Unlock()
	return nil
}

// Remove implements Runtime.
func (f *Fake) Remove(_ context.Context, pod Pod) error {
	f.call("remove " + pod.Name)
	f.mu.Lock()
	f.Removed = append(f.Removed, pod.Name)
	f.mu.Unlock()
	return nil
}
