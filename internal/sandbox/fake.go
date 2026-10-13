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
	// Detached are the agents StartAgent started; each is also a running
	// container in Running, mounting its spec's host paths, in a pod in
	// Standing. States, when it names one, is what AgentState answers
	// for it; Stopped are the names StopAgent was asked to stop.
	// StartErr fails StartAgent.
	Detached []AgentSpec
	States   map[string]AgentState
	Stopped  []string
	StartErr error
	Sidecars []SidecarSpec
	Removed  []string
	Calls    []string
	// Trees are the paths RemoveTree was asked to remove.
	Trees []string
	// Running and Standing are the store ListContainers and ListPods list: the
	// containers in any state and the pods. Remove drops a pod and its
	// containers from them, RemoveContainer a container. ListErr fails
	// both listings.
	Running  []Container
	Standing []PodInfo
	ListErr  error
	// RemovedContainers are the containers RemoveContainer was asked to
	// remove.
	RemovedContainers []string
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

// StartAgent implements Runtime: the agent is recorded and stands as a
// running container of the pod, mounting what StartAgentArgs mounts.
func (f *Fake) StartAgent(_ context.Context, pod Pod, spec AgentSpec) error {
	f.call("start-agent " + pod.Name)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.StartErr != nil {
		return f.StartErr
	}
	f.Detached = append(f.Detached, spec)
	var mounts []string
	args := StartAgentArgs(pod, spec)
	for i, a := range args {
		if a == "--volume" && i+1 < len(args) {
			src, _, _ := strings.Cut(args[i+1], ":")
			mounts = append(mounts, src)
		}
	}
	f.Running = append(f.Running, Container{Name: spec.Name, Pod: pod.Name, Running: true, Mounts: mounts})
	for _, p := range f.Standing {
		if p.Name == pod.Name {
			return nil
		}
	}
	f.Standing = append(f.Standing, PodInfo{Name: pod.Name})
	return nil
}

// AgentState implements Runtime: States' entry for name, else the
// container's own state in Running, else ErrNoAgent.
func (f *Fake) AgentState(_ context.Context, name string) (AgentState, error) {
	f.call("agent-state " + name)
	f.mu.Lock()
	defer f.mu.Unlock()
	if st, ok := f.States[name]; ok {
		return st, nil
	}
	for _, c := range f.Running {
		if c.Name == name {
			if c.Running {
				return AgentState{Status: "running", Running: true}, nil
			}
			return AgentState{Status: "exited"}, nil
		}
	}
	return AgentState{}, fmt.Errorf("%w: %s", ErrNoAgent, name)
}

// EndAgent ends a detached agent as its own exit would: the container
// stops running and AgentState answers exit from then on.
func (f *Fake) EndAgent(name string, exit Exit) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.Running {
		if c.Name == name {
			f.Running[i].Running = false
		}
	}
	if f.States == nil {
		f.States = map[string]AgentState{}
	}
	f.States[name] = AgentState{Status: "exited", Exit: exit}
}

// StoppedAgents are the names StopAgent was asked to stop so far.
func (f *Fake) StoppedAgents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Stopped...)
}

// StopAgent implements Runtime: the container stops, exiting 143 as
// SIGTERM ends it, unless States already names how it ended.
func (f *Fake) StopAgent(_ context.Context, name string) error {
	f.call("stop-agent " + name)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Stopped = append(f.Stopped, name)
	for i, c := range f.Running {
		if c.Name == name {
			f.Running[i].Running = false
		}
	}
	if st, ok := f.States[name]; !ok || st.Running {
		if f.States == nil {
			f.States = map[string]AgentState{}
		}
		f.States[name] = AgentState{Status: "exited", Exit: Exit{Code: 143}}
	}
	return nil
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
	if f.RemoveErr == nil {
		var cs []Container
		for _, c := range f.Running {
			if c.Pod != pod.Name {
				cs = append(cs, c)
			}
		}
		var ps []PodInfo
		for _, p := range f.Standing {
			if p.Name != pod.Name {
				ps = append(ps, p)
			}
		}
		f.Running, f.Standing = cs, ps
	}
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

// ListContainers implements Runtime.
func (f *Fake) ListContainers(context.Context) ([]Container, error) {
	f.call("containers")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ListErr != nil {
		return nil, f.ListErr
	}
	return append([]Container(nil), f.Running...), nil
}

// ListPods implements Runtime.
func (f *Fake) ListPods(context.Context) ([]PodInfo, error) {
	f.call("pods")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ListErr != nil {
		return nil, f.ListErr
	}
	return append([]PodInfo(nil), f.Standing...), nil
}

// RemoveContainer implements Runtime.
func (f *Fake) RemoveContainer(_ context.Context, name string) error {
	f.call("remove-container " + name)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.RemovedContainers = append(f.RemovedContainers, name)
	var cs []Container
	for _, c := range f.Running {
		if c.Name != name {
			cs = append(cs, c)
		}
	}
	f.Running = cs
	return nil
}
