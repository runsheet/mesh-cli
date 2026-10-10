// Package sandbox runs an agent session in its own rootless pod (mesh-specs
// ADR 007): the item's workspace and a per-session temporary HOME are the
// only host paths it sees, its environment is built from an allow-list,
// its CPU, memory and processes are capped, and its network reaches the
// public internet but not the LAN or the host.
//
// Runtime is the container runtime; Podman is the one ADR 007 chose and
// Fake records what would have run, for tests.
package sandbox

import (
	"context"
	"fmt"
	"io"
)

// Mount is one host path bound into a container.
type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
}

// Limits cap one container: CPUs (podman --cpus), Memory (--memory, a
// podman size such as "6g") and Pids (--pids-limit). A zero value is
// no flag.
type Limits struct {
	CPUs   float64
	Memory string
	Pids   int
}

// PodSpec is a session's pod: its name and labels, and the network
// rules loaded into it before anything else runs there.
type PodSpec struct {
	Name   string
	Labels map[string]string
	// Image runs the network rules' init container (it carries nft).
	Image string
	// Network is what the pod may reach (NetworkPolicy).
	Network NetworkPolicy
	// UID and GID are the image's declared user (ImageUser), the user
	// the host user is mapped onto inside the pod; zero is
	// DefaultImageUID (KeepIDUserns).
	UID, GID int
}

// Pod is a created pod.
type Pod struct {
	Name string
}

// AgentSpec is the agent's container in a pod.
type AgentSpec struct {
	Name    string
	Image   string
	Workdir string
	// Mounts are the only host paths the container sees.
	Mounts []Mount
	// Env is the container's whole environment (KEY=VALUE); nothing is
	// inherited from the node's process or the image.
	Env     []string
	Command []string
	Limits  Limits
	// TimeoutSeconds stops the container after that long inside the
	// runtime (a backstop to the node's own context); 0 is none.
	TimeoutSeconds int
	Stdin          io.Reader
	Stdout         io.Writer
	Stderr         io.Writer
}

// SidecarSpec is a service container in a pod: it shares the pod's
// network and mounts nothing from the host.
type SidecarSpec struct {
	Name    string
	Image   string
	Env     []string
	Command []string
	Limits  Limits
}

// Exit is how the agent's container ended.
type Exit struct {
	Code int
	// OOMKilled is the runtime's own word that the kernel killed the
	// container for memory.
	OOMKilled bool
}

// Runtime creates and removes pods and runs containers in them.
type Runtime interface {
	// Version is the runtime's name and version ("podman 5.2.1"); an
	// error is a runtime the node cannot use.
	Version(ctx context.Context) (string, error)
	CreatePod(ctx context.Context, spec PodSpec) (Pod, error)
	// RunAgent runs the agent's container to its end. A non-zero exit is
	// in Exit, not the error; the error is the runtime failing.
	RunAgent(ctx context.Context, pod Pod, spec AgentSpec) (Exit, error)
	// AddSidecar starts a service in the pod, pulling its image when the
	// node does not have it yet (and keeping it: the pull is cached).
	AddSidecar(ctx context.Context, pod Pod, spec SidecarSpec) error
	// ExecSidecar runs cmd in a started sidecar (its health check); an
	// error is a check that did not pass.
	ExecSidecar(ctx context.Context, pod Pod, sidecar string, cmd []string) error
	// ProbeTCP connects to 127.0.0.1:port inside the pod, from a
	// short-lived container of image; an error is nothing listening.
	ProbeTCP(ctx context.Context, pod Pod, image string, port int) error
	// Remove removes the pod and every container in it.
	Remove(ctx context.Context, pod Pod) error
}

// LimitError is a session killed by its sandbox's limits — memory,
// processes, or the runtime's own timeout. It is the node's condition,
// not the item's failure (ADR 007 Limits): the claim is released, not
// failed.
type LimitError struct {
	// Limit is "memory", "pids" or "timeout".
	Limit string
	// Value is the limit as configured ("6g", "4096", "30m0s").
	Value string
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("sandbox limit: %s (%s) killed the session", e.Limit, e.Value)
}

// ExitError is the agent's container ending non-zero for its own
// reasons.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string { return fmt.Sprintf("sandbox: agent exited %d", e.Code) }
