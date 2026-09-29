package node

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// The node writes its own log (goal node-yaml-takes-effect-or-says-why-not,
// item 4). Before, systemd and launchd appended the process's stdout and
// stderr to ~/.mesh/node.log and held the descriptor, so the node could
// not rotate it and it grew without end (339 MB on the mini). Now
// `mesh node start` opens node.log itself through a RotatingFile, and
// the service's own redirect gets only what the process cannot write
// itself: a panic's stack.
//
// Two files, each capped and rotated on its own:
//
//   - ~/.mesh/node.log: the node's own reasoning — ticks, claims,
//     reloads, heartbeats, holds, and each session's one-line summaries
//     (start, end, usage, cost);
//   - ~/.mesh/sessions.log: what sessions say — channel mode's
//     `session:` text, the workspaces' git commands and their output,
//     the agents' stderr.

const (
	// NodeLogMaxBytes is where node.log rotates.
	NodeLogMaxBytes = 10 << 20
	// NodeLogKeep is how many rotated node.logs are kept
	// (node.log.1 … node.log.5).
	NodeLogKeep = 5
	// SessionsLogMaxBytes is where sessions.log rotates.
	SessionsLogMaxBytes = 20 << 20
	// SessionsLogKeep is how many rotated sessions.logs are kept
	// (sessions.log.1, sessions.log.2).
	SessionsLogKeep = 2
)

// SessionsLog is ~/.mesh/sessions.log.
func SessionsLog(home string) string { return filepath.Join(home, ".mesh", "sessions.log") }

// CrashLog is ~/.mesh/node.crash.log: where launchd appends the
// process's stdout and stderr — a panic's stack, nothing the node logs
// itself. systemd sends them to the journal instead.
func CrashLog(home string) string { return filepath.Join(home, ".mesh", "node.crash.log") }

// RotatingFile is an append-only file capped at MaxBytes: a write that
// would take it past the cap first renames it to <path>.1 (and each
// <path>.n to <path>.n+1, the oldest past Keep removed) and starts a
// new one. No write takes the file past the cap: one larger than the
// cap on its own is cut to it, ending in a line that says how much was
// cut, and a write the file could not rotate for is refused with the
// error — never appended past the cap. Safe for concurrent use.
type RotatingFile struct {
	Path     string
	MaxBytes int64
	Keep     int
	// Errors gets one line when a rotation first fails and one when it
	// works again, since a log.Logger drops its writer's errors; nil is
	// os.Stderr (the journal or node.crash.log under a service).
	Errors io.Writer

	mu     sync.Mutex
	f      *os.File
	size   int64
	closed bool
	// failing is the rotation error writes are being refused for.
	failing error
}

// OpenRotating opens path for appending, rotating it first when it is
// already at or past maxBytes (a log a service appended to unbounded).
func OpenRotating(path string, maxBytes int64, keep int) (*RotatingFile, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("log %s: a cap of %d bytes", path, maxBytes)
	}
	if keep < 1 {
		keep = 1
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	r := &RotatingFile{Path: path, MaxBytes: maxBytes, Keep: keep}
	if err := r.open(); err != nil {
		return nil, err
	}
	if r.size >= maxBytes {
		if err := r.rotate(); err != nil {
			r.f.Close()
			return nil, err
		}
	}
	return r, nil
}

func (r *RotatingFile) open() error {
	f, err := os.OpenFile(r.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

// Rotated is the name of the n-th rotated file, <path>.n.
func (r *RotatingFile) Rotated(n int) string { return fmt.Sprintf("%s.%d", r.Path, n) }

func (r *RotatingFile) rotate() error {
	if r.f != nil {
		r.f.Close()
		r.f = nil
	}
	_ = os.Remove(r.Rotated(r.Keep))
	for n := r.Keep - 1; n >= 1; n-- {
		if err := os.Rename(r.Rotated(n), r.Rotated(n+1)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Rename(r.Path, r.Rotated(1)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return r.open()
}

// Write appends p, rotating first when p would take the file past the
// cap. A p larger than the cap is cut to it (the count returned is
// still len(p): the write was taken, and a short count would only make
// the caller retry what cannot fit). When the rotation fails nothing is
// written and the error is returned, and said once on Errors; the next
// write tries the rotation again.
func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, os.ErrClosed
	}
	if r.f == nil {
		// a rotation renamed the file but could not open the new one
		if err := r.open(); err != nil {
			return 0, r.fail(err)
		}
	}
	if r.size > 0 && r.size+int64(len(p)) > r.MaxBytes {
		if err := r.rotate(); err != nil {
			return 0, r.fail(err)
		}
	}
	r.recovered()
	out := p
	if int64(len(p)) > r.MaxBytes {
		out = cut(p, r.MaxBytes)
	}
	n, err := r.f.Write(out)
	r.size += int64(n)
	if err != nil {
		return n, err
	}
	return len(p), nil
}

// cut is p's head and a line saying the rest was left out, max bytes
// in all.
func cut(p []byte, max int64) []byte {
	note := fmt.Sprintf("\n[log: a %d-byte write cut to the %d-byte cap]\n", len(p), max)
	head := max - int64(len(note))
	if head < 0 {
		return p[:max]
	}
	return append(append([]byte{}, p[:head]...), note...)
}

func (r *RotatingFile) fail(err error) error {
	err = fmt.Errorf("log %s: rotate: %w; the line is dropped rather than written past the %d-byte cap", r.Path, err, r.MaxBytes)
	if r.failing == nil {
		fmt.Fprintf(r.errors(), "mesh node: %v\n", err)
	}
	r.failing = err
	return err
}

func (r *RotatingFile) recovered() {
	if r.failing == nil {
		return
	}
	fmt.Fprintf(r.errors(), "mesh node: log %s: rotating again, after dropping lines for: %v\n", r.Path, r.failing)
	r.failing = nil
}

func (r *RotatingFile) errors() io.Writer {
	if r.Errors != nil {
		return r.Errors
	}
	return os.Stderr
}

// Close closes the active file.
func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// LogFiles lists path's rotated files and path itself, oldest first,
// those that exist — what `mesh node usage` reads.
func LogFiles(path string, keep int) []string {
	var out []string
	for n := keep; n >= 1; n-- {
		p := fmt.Sprintf("%s.%d", path, n)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	if _, err := os.Stat(path); err == nil {
		out = append(out, path)
	}
	return out
}

// NodeLogs are the node's two logs, opened by `mesh node start`.
type NodeLogs struct {
	// Node is node.log's logger: the node's own lines.
	Node *log.Logger
	// Sessions is sessions.log's logger: channel mode's session text.
	Sessions *log.Logger
	// SessionsOut is sessions.log as a writer, for git's output and
	// the agents' stderr.
	SessionsOut io.Writer

	files []*RotatingFile
}

// OpenNodeLogs opens ~/.mesh/node.log and ~/.mesh/sessions.log under
// home, each capped and rotated. echo, when not nil, gets a copy of
// every line of both — the terminal of a node started by hand.
func OpenNodeLogs(home string, echo io.Writer) (*NodeLogs, error) {
	nodeFile, err := OpenRotating(ServiceLog(home), NodeLogMaxBytes, NodeLogKeep)
	if err != nil {
		return nil, err
	}
	sessFile, err := OpenRotating(SessionsLog(home), SessionsLogMaxBytes, SessionsLogKeep)
	if err != nil {
		nodeFile.Close()
		return nil, err
	}
	var nodeW, sessW io.Writer = nodeFile, sessFile
	if echo != nil {
		nodeW, sessW = io.MultiWriter(nodeFile, echo), io.MultiWriter(sessFile, echo)
	}
	return &NodeLogs{
		Node:        log.New(nodeW, "mesh node: ", log.LstdFlags),
		Sessions:    log.New(sessW, "mesh node: ", log.LstdFlags),
		SessionsOut: sessW,
		files:       []*RotatingFile{nodeFile, sessFile},
	}, nil
}

// Close closes both files.
func (l *NodeLogs) Close() error {
	var first error
	for _, f := range l.files {
		if err := f.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
