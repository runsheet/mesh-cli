package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
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
	// Trees are the paths RemoveTree was asked to remove.
	Trees []string
	// RemoveErr fails Remove; RemoveTreeErr fails RemoveTree.
	RemoveErr     error
	RemoveTreeErr error
	// Agent is the agent's container: it gets the spec and returns the
	// exit (nil: exit 0, nothing written).
	Agent func(ctx context.Context, spec AgentSpec) (Exit, error)
	// SidecarErr fails AddSidecar for the named services (a pull or a
	// start that did not work).
	SidecarErr map[string]error
	// Health answers a sidecar's health check: its command (nil for a
	// TCP probe) or the port probed. Nil passes every check.
	Health func(sidecar string, cmd []string, port int) error
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
	defer f.mu.Unlock()
	if err := f.SidecarErr[spec.Name]; err != nil {
		return err
	}
	f.Sidecars = append(f.Sidecars, spec)
	return nil
}

// ExecSidecar implements Runtime.
func (f *Fake) ExecSidecar(_ context.Context, pod Pod, sidecar string, cmd []string) error {
	f.call("health " + pod.Name + " " + sidecar)
	f.mu.Lock()
	h := f.Health
	f.mu.Unlock()
	if h == nil {
		return nil
	}
	return h(sidecar, cmd, 0)
}

// ProbeTCP implements Runtime.
func (f *Fake) ProbeTCP(_ context.Context, pod Pod, _ string, port int) error {
	f.call(fmt.Sprintf("probe %s %d", pod.Name, port))
	f.mu.Lock()
	h := f.Health
	f.mu.Unlock()
	if h == nil {
		return nil
	}
	return h("", nil, port)
}

// Called reports whether a call was recorded, and its index in Calls.
func (f *Fake) Called(prefix string) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.Calls {
		if strings.HasPrefix(c, prefix) {
			return i, true
		}
	}
	return -1, false
}

// Remove implements Runtime.
func (f *Fake) Remove(_ context.Context, pod Pod) error {
	f.call("remove " + pod.Name)
	f.mu.Lock()
	f.Removed = append(f.Removed, pod.Name)
	f.mu.Unlock()
	return f.RemoveErr
}

// RemoveTree implements Runtime: it records the path and removes it as
// the node user can, unless RemoveTreeErr fails it.
func (f *Fake) RemoveTree(_ context.Context, path string) error {
	f.call("remove-tree " + path)
	f.mu.Lock()
	f.Trees = append(f.Trees, path)
	f.mu.Unlock()
	if f.RemoveTreeErr != nil {
		return f.RemoveTreeErr
	}
	return os.RemoveAll(path)
}
