// Package sandbox is the node's half of a session's sandbox (ADR 007,
// sessions in sandboxes). This file resolves the services a repository
// declares — `.mesh/services.yml`, else the `services:` of its CI
// workflows that run tests, else none — into one shape. It is pure
// parsing: no container runtime is involved here.
//
// It is a leaf: it imports nothing from node or gateway.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultRegistries are the registries a service image may come from when
// the caller allows none (ADR 007 Services; an org setting later).
var DefaultRegistries = []string{"docker.io", "ghcr.io", "*.pkg.dev", "quay.io"}

// ServicesFile is the repository's own declaration, relative to its root.
const ServicesFile = ".mesh/services.yml"

// Where a resolution came from.
const (
	SourceNone     = ""
	SourceFile     = "services.yml"
	SourceWorkflow = "workflows"
)

// Services is what a repository declares: where it came from, each
// service in name order, and notes on what was ignored or dropped.
type Services struct {
	Source   string    // SourceFile, SourceWorkflow or SourceNone
	Services []Service // sorted by name
	Notes    []string  // about the declaration as a whole
}

// Service is one declared service, in the same shape from either source.
type Service struct {
	Name     string
	Image    string
	Registry string            // the image's registry host; docker.io for a bare name
	Env      map[string]string // literal values only
	Ports    []string          // as declared: "5432:5432", "6379"
	Command  []string          // arguments to the image; the tuning when none was declared
	Health   *HealthCheck      // from options' --health-* flags; nil when none
	Required bool              // services.yml defaults true; a workflow's are false
	Tuned    bool              // Command is the default test tuning of a known database
	From     string            // the file it was read from, relative to the repo
	Refused  bool              // its registry is not allowed; never pulled
	Notes    []string          // what was ignored, dropped or refused, and why
}

// HealthCheck is an Actions-style `--health-*` set parsed from options.
type HealthCheck struct {
	Cmd         string
	Interval    time.Duration
	Timeout     time.Duration
	Retries     int
	StartPeriod time.Duration
}

// Resolve returns the services repoDir declares, first found wins:
// .mesh/services.yml, else the services of .github/workflows jobs that
// run tests (merged by name, first file then first job winning), else
// none. Each image is checked against allowed (DefaultRegistries when
// empty); a disallowed one is returned Refused with a note, not dropped.
// A services.yml that cannot be read is an error; a workflow that cannot
// be is noted and skipped.
func Resolve(repoDir string, allowed []string) (Services, error) {
	if len(allowed) == 0 {
		allowed = DefaultRegistries
	}
	var out Services
	for _, name := range []string{ServicesFile, ".mesh/services.yaml"} {
		b, err := os.ReadFile(filepath.Join(repoDir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return Services{}, fmt.Errorf("services: %w", err)
		}
		svcs, err := parseServicesFile(b, name)
		if err != nil {
			return Services{}, fmt.Errorf("services: %s: %w", name, err)
		}
		out = Services{Source: SourceFile, Services: svcs}
		break
	}
	if out.Source == SourceNone {
		svcs, notes := fromWorkflows(repoDir)
		out.Notes = notes
		if len(svcs) > 0 {
			out.Source, out.Services = SourceWorkflow, svcs
		}
	}
	for i := range out.Services {
		finish(&out.Services[i], allowed)
	}
	sort.Slice(out.Services, func(i, j int) bool { return out.Services[i].Name < out.Services[j].Name })
	return out, nil
}

// spec is one entry of a services map, as YAML has it in either source.
type spec struct {
	Image       string    `yaml:"image"`
	Env         yaml.Node `yaml:"env"`
	Ports       []any     `yaml:"ports"`
	Options     string    `yaml:"options"`
	Command     yaml.Node `yaml:"command"`
	Required    *bool     `yaml:"required"`
	Tuning      *bool     `yaml:"tuning"`
	Volumes     yaml.Node `yaml:"volumes"`
	Credentials yaml.Node `yaml:"credentials"`
}

func parseServicesFile(b []byte, from string) ([]Service, error) {
	var top map[string]yaml.Node
	if err := yaml.Unmarshal(b, &top); err != nil {
		return nil, err
	}
	// The file is either a services map, or one under a `services:` key
	// like a workflow job's (so a block can be copied as it is).
	if n, ok := top["services"]; ok && n.Kind == yaml.MappingNode && !hasKey(&n, "image") {
		top = nil
		if err := n.Decode(&top); err != nil {
			return nil, err
		}
	}
	var out []Service
	for name, n := range top {
		var sp spec
		if err := n.Decode(&sp); err != nil {
			return nil, fmt.Errorf("service %s: %w", name, err)
		}
		s := build(name, sp, from, nil)
		s.Required = sp.Required == nil || *sp.Required
		if s.Image == "" {
			return nil, fmt.Errorf("service %s: no image", name)
		}
		out = append(out, s)
	}
	return out, nil
}

// build turns a spec into a Service, resolving ${{ }} expressions by
// resolve (nil: every expression is dropped).
func build(name string, sp spec, from string, resolve func(string) (string, bool)) Service {
	s := Service{Name: name, From: from, Env: map[string]string{}}
	expr := func(what, v string) (string, bool) {
		if !strings.Contains(v, "${{") {
			return v, true
		}
		if resolve != nil {
			if r, ok := resolve(v); ok {
				return r, true
			}
		}
		s.Notes = append(s.Notes, fmt.Sprintf("%s %q: expression dropped", what, v))
		return "", false
	}
	if img, ok := expr("image", sp.Image); ok {
		s.Image = img
	}
	if sp.Env.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(sp.Env.Content); i += 2 {
			k, v := sp.Env.Content[i].Value, sp.Env.Content[i+1].Value
			if r, ok := expr("env "+k, v); ok {
				s.Env[k] = r
			}
		}
	}
	for _, p := range sp.Ports {
		if r, ok := expr("port", fmt.Sprint(p)); ok {
			s.Ports = append(s.Ports, r)
		}
	}
	switch sp.Command.Kind {
	case yaml.SequenceNode:
		for _, c := range sp.Command.Content {
			s.Command = append(s.Command, c.Value)
		}
	case yaml.ScalarNode:
		s.Command = splitArgs(sp.Command.Value)
	}
	if opts, ok := expr("options", sp.Options); ok {
		s.Health, s.Notes = parseOptions(opts, s.Notes)
	}
	if !sp.Volumes.IsZero() {
		s.Notes = append(s.Notes, "volumes are not supported: a service mounts nothing from the host")
	}
	if !sp.Credentials.IsZero() {
		s.Notes = append(s.Notes, "credentials are not supported: only public images are pulled")
	}
	// Tuning: explicit command wins; tuning: false turns it off.
	if len(s.Command) == 0 && (sp.Tuning == nil || *sp.Tuning) {
		if t := tuning(s.Image); t != nil {
			s.Command, s.Tuned = t, true
		}
	}
	return s
}

// finish sets the registry, refuses an image outside allowed, and gives a
// postgres service a writable data directory under keep-id.
func finish(s *Service, allowed []string) {
	s.Registry = Registry(s.Image)
	if !RegistryAllowed(s.Registry, allowed) {
		s.Refused = true
		s.Notes = append(s.Notes, fmt.Sprintf("image %s refused: registry %s is not allowed (allowed: %s)",
			s.Image, s.Registry, strings.Join(allowed, ", ")))
	}
	postgresDefaults(s)
}

// postgresDefaults gives a postgres or postgis service PGDATA and
// POSTGRES_INITDB_ARGS when it has not set them itself: under keep-id the
// image's /var/lib/postgresql/data volume belongs to the pod's user and the
// entrypoint's chmod of it fails ("could not change permissions of
// directory \"/var/lib/postgresql/data\": Operation not permitted"), so the
// server is told to initialize a data directory it can actually write.
func postgresDefaults(s *Service) {
	switch imageName(s.Image) {
	case "postgres", "postgis":
	default:
		return
	}
	if s.Env == nil {
		s.Env = map[string]string{}
	}
	var set []string
	if _, ok := s.Env["PGDATA"]; !ok {
		s.Env["PGDATA"] = PostgresDefaultPGDATA
		set = append(set, "PGDATA")
	}
	if _, ok := s.Env["POSTGRES_INITDB_ARGS"]; !ok {
		s.Env["POSTGRES_INITDB_ARGS"] = PostgresDefaultInitdbArgs
		set = append(set, "POSTGRES_INITDB_ARGS")
	}
	if len(set) > 0 {
		s.Notes = append(s.Notes, fmt.Sprintf("defaulted %s so the data directory is writable under keep-id", strings.Join(set, ", ")))
	}
}

// Registry is the registry host of an image reference: its first path
// component when that has a '.' or ':' or is localhost, else docker.io.
func Registry(image string) string {
	first, _, ok := strings.Cut(image, "/")
	if !ok || !(strings.ContainsAny(first, ".:") || first == "localhost") {
		return "docker.io"
	}
	first = strings.ToLower(first)
	if first == "index.docker.io" || first == "registry-1.docker.io" {
		return "docker.io"
	}
	return first
}

// RegistryAllowed reports whether host matches one of allowed, each a
// host or a glob such as *.pkg.dev.
func RegistryAllowed(host string, allowed []string) bool {
	for _, a := range allowed {
		if ok, _ := path.Match(strings.ToLower(a), host); ok {
			return true
		}
	}
	return false
}

// Default test tuning of known databases: durability is worthless in a
// database that dies with the session.
var (
	PostgresTuning = []string{"-c", "fsync=off", "-c", "synchronous_commit=off", "-c", "full_page_writes=off"}
	MySQLTuning    = []string{"--innodb-flush-log-at-trx-commit=0", "--sync-binlog=0", "--skip-log-bin"}
)

// PostgresDefaultPGDATA and PostgresDefaultInitdbArgs are the data
// directory and initdb arguments a postgres or postgis service gets when it
// has not set them itself (postgresDefaults): under keep-id the image's
// /var/lib/postgresql/data belongs to the pod's user, and the entrypoint
// cannot chmod it.
const (
	PostgresDefaultPGDATA     = "/tmp/pgdata"
	PostgresDefaultInitdbArgs = "--no-sync"
)

func tuning(image string) []string {
	switch imageName(image) {
	case "postgres", "postgis":
		return append([]string(nil), PostgresTuning...)
	case "mysql", "mariadb":
		return append([]string(nil), MySQLTuning...)
	}
	return nil
}

// imageName is the last path component of an image, without tag or digest.
func imageName(image string) string {
	if i := strings.LastIndex(image, "/"); i >= 0 {
		image = image[i+1:]
	}
	image, _, _ = strings.Cut(image, "@")
	image, _, _ = strings.Cut(image, ":")
	return strings.ToLower(image)
}

// parseOptions reads the --health-* flags of an Actions options string;
// any other flag is ignored and noted.
func parseOptions(opts string, notes []string) (*HealthCheck, []string) {
	args := splitArgs(opts)
	var h HealthCheck
	seen := false
	for i := 0; i < len(args); i++ {
		flag, val, hasVal := strings.Cut(args[i], "=")
		if !strings.HasPrefix(flag, "--health-") {
			notes = append(notes, fmt.Sprintf("option %s ignored", flag))
			if !hasVal && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++ // its value
			}
			continue
		}
		if !hasVal {
			if i+1 >= len(args) {
				notes = append(notes, fmt.Sprintf("option %s has no value", flag))
				continue
			}
			i++
			val = args[i]
		}
		var err error
		switch flag {
		case "--health-cmd":
			h.Cmd = val
		case "--health-interval":
			h.Interval, err = time.ParseDuration(val)
		case "--health-timeout":
			h.Timeout, err = time.ParseDuration(val)
		case "--health-start-period":
			h.StartPeriod, err = time.ParseDuration(val)
		case "--health-retries":
			h.Retries, err = strconv.Atoi(val)
		default:
			notes = append(notes, fmt.Sprintf("option %s ignored", flag))
			continue
		}
		if err != nil {
			notes = append(notes, fmt.Sprintf("option %s %q ignored: %v", flag, val, err))
			continue
		}
		seen = true
	}
	if !seen {
		return nil, notes
	}
	return &h, notes
}

// splitArgs splits a command line as a shell would its words: whitespace
// separated, with single and double quotes and backslash escapes.
func splitArgs(s string) []string {
	var out []string
	var cur strings.Builder
	inWord := false
	var quote rune
	escaped := false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped, inWord = true, true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}

func hasKey(n *yaml.Node, key string) bool {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return true
		}
	}
	return false
}
