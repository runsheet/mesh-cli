// Package nodecaps probes what a node has to run work on: its os and
// arch and the toolchains it finds with their versions. The platform
// keeps it as the node's manifest and matches a job's needs against it
// ("os:darwin", "tool:xcode", "tool:go>=1.23"); the node itself reads
// no job's meaning into it.
package nodecaps

import (
	"context"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Tool is one toolchain the node found and the version it reports.
type Tool struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// Manifest is the os, arch and tools a probe found.
type Manifest struct {
	OS    string `json:"os"`
	Arch  string `json:"arch"`
	Tools []Tool `json:"tools"`
}

// probe is one toolchain: the program looked for on PATH, the
// arguments that print its version, and the name the manifest gives it.
type probe struct {
	name string
	bin  string
	args []string
}

// probes are the toolchains a node reports. The list is of toolchains,
// not of any product's work: a product says what its jobs need in these
// terms, and the platform does the matching.
var probes = []probe{
	{"go", "go", []string{"env", "GOVERSION"}},
	{"node", "node", []string{"--version"}},
	{"python", "python3", []string{"--version"}},
	{"java", "java", []string{"-version"}},
	{"git", "git", []string{"--version"}},
	{"docker", "docker", []string{"--version"}},
	{"podman", "podman", []string{"--version"}},
	{"xcode", "xcodebuild", []string{"-version"}},
	{"swift", "swift", []string{"--version"}},
	{"android-sdk", "sdkmanager", []string{"--version"}},
	{"terraform", "terraform", []string{"version"}},
}

// versionRe is the first dotted version in a tool's output.
var versionRe = regexp.MustCompile(`\d+(?:\.\d+)+`)

// Runner runs a program and returns what it printed (stdout and stderr:
// java prints its version on stderr).
type Runner func(ctx context.Context, bin string, args ...string) (string, error)

// ExecRunner runs bin with a short deadline.
func ExecRunner(ctx context.Context, bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	return string(out), err
}

// Probe is the node's manifest: runtime.GOOS and GOARCH, and every
// toolchain lookPath finds, with the version its own command prints
// ("" when it prints none it can be read from). A tool that is on PATH
// but fails to run is left out: it cannot run work either.
func Probe(ctx context.Context, lookPath func(string) (string, error), run Runner) Manifest {
	m := Manifest{OS: runtime.GOOS, Arch: runtime.GOARCH, Tools: []Tool{}}
	for _, p := range probes {
		path, err := lookPath(p.bin)
		if err != nil || path == "" {
			continue
		}
		out, err := run(ctx, path, p.args...)
		if err != nil {
			continue
		}
		m.Tools = append(m.Tools, Tool{Name: p.name, Version: versionRe.FindString(strings.TrimSpace(out))})
	}
	sort.Slice(m.Tools, func(i, j int) bool { return m.Tools[i].Name < m.Tools[j].Name })
	return m
}
