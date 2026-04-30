// Package template resolves, fetches, renders, and manages agent templates.
package template

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	gotemplate "text/template"
	"time"

	"github.com/Masterminds/sprig/v3"
	"gopkg.in/yaml.v3"

	"projectmesh.io/mesh-cli/internal/channels"
	"projectmesh.io/mesh-cli/internal/paths"
	"projectmesh.io/mesh-cli/internal/state"
)

// Template is the resolved, runnable view of an agent template.
type Template struct {
	Source                     string // user-supplied --template value
	ResolvedPath               string // absolute directory on disk
	SystemPrompt               string // contents of system_prompt.md
	Description                string
	Role                       string
	Model                      string
	Tools                      []string
	RequiredEnv                []string
	WorkingDir                 string
	DangerouslySkipPermissions bool
	Channels                   []channels.Spec

	// Git-only metadata (empty for local templates).
	GitURL string
	GitRef string
	GitSHA string
}

// Source describes a normalized template source the daemon knows how to
// materialize.
type Source struct {
	Kind string // "local" or "git"
	Path string // absolute path (local)
	URL  string // git clone URL (git)
	Ref  string // tag / branch / SHA (git, may be empty)
}

var (
	defaultTemplateTools   = []string{"Read", "Edit", "Write", "Bash", "Grep", "Glob", "WebFetch", "Task"}
	templateModelAllowlist = map[string]bool{
		"claude-opus-4-7":   true,
		"claude-sonnet-4-6": true,
		"claude-haiku-4-5":  true,
	}
	templateToolAllowlist = map[string]bool{
		"Read":         true,
		"Edit":         true,
		"Write":        true,
		"Bash":         true,
		"Grep":         true,
		"Glob":         true,
		"WebFetch":     true,
		"WebSearch":    true,
		"Task":         true,
		"TodoWrite":    true,
		"NotebookEdit": true,
		"BashOutput":   true,
		"KillShell":    true,
		"SlashCommand": true,
		"ExitPlanMode": true,
	}
)

const (
	DefaultTemplateModel    = "claude-opus-4-7"
	DefaultSystemPromptPath = "system_prompt.md"
	GitOpTimeout            = 120 * time.Second
	TemplateCacheSubdir     = "templates"
	PruneUnreferencedAfter  = 30 * 24 * time.Hour
	FullSHALen              = 40
)

// AgentYAML mirrors the Kubernetes-style template manifest.
type AgentYAML struct {
	APIVersion string        `yaml:"apiVersion"`
	Kind       string        `yaml:"kind"`
	Metadata   AgentYAMLMeta `yaml:"metadata"`
	Spec       AgentYAMLSpec `yaml:"spec"`
}

// AgentYAMLMeta is the metadata block of an agent.yaml.
type AgentYAMLMeta struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
}

// AgentYAMLSpec is the spec block of an agent.yaml.
type AgentYAMLSpec struct {
	Description                string          `yaml:"description"`
	Role                       string          `yaml:"role"`
	Model                      string          `yaml:"model"`
	Tools                      []string        `yaml:"tools"`
	RequiredEnv                []string        `yaml:"requiredEnv"`
	SystemPromptPath           string          `yaml:"systemPromptPath"`
	WorkingDir                 string          `yaml:"workingDir"`
	Subagents                  []string        `yaml:"subagents"`
	Skills                     []string        `yaml:"skills"`
	DangerouslySkipPermissions bool            `yaml:"dangerouslySkipPermissions"`
	Channels                   []channels.Spec `yaml:"channels"`
}

// sha40 matches a 40-char hex commit SHA (full form).
var sha40 = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ResolveTemplateSource normalizes a --template value into a Source.
func ResolveTemplateSource(raw string) (*Source, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("template source is required")
	}

	if isLocalPath(raw) {
		abs, err := expandPath(raw)
		if err != nil {
			return nil, err
		}
		return &Source{Kind: "local", Path: abs}, nil
	}

	urlPart, ref := splitGitRef(raw)

	switch {
	case strings.HasPrefix(urlPart, "git@"):
		return &Source{Kind: "git", URL: urlPart, Ref: ref}, nil
	case strings.HasPrefix(urlPart, "https://") || strings.HasPrefix(urlPart, "http://") || strings.HasPrefix(urlPart, "ssh://") || strings.HasPrefix(urlPart, "git://"):
		if _, err := url.Parse(urlPart); err != nil {
			return nil, fmt.Errorf("parse git url %q: %w", urlPart, err)
		}
		return &Source{Kind: "git", URL: urlPart, Ref: ref}, nil
	case looksLikeHostPath(urlPart):
		cloneURL := "https://" + strings.TrimSuffix(urlPart, ".git") + ".git"
		return &Source{Kind: "git", URL: cloneURL, Ref: ref}, nil
	}

	if abs, err := expandPath(raw); err == nil {
		if info, statErr := os.Stat(abs); statErr == nil && info.IsDir() {
			return &Source{Kind: "local", Path: abs}, nil
		}
	}

	return nil, fmt.Errorf("unrecognized template source %q (expected local path, git URL, or host/user/repo shorthand)", raw)
}

func splitGitRef(raw string) (string, string) {
	if strings.HasPrefix(raw, "git@") {
		colon := strings.Index(raw, ":")
		if colon >= 0 {
			head, tail := raw[:colon+1], raw[colon+1:]
			if at := strings.LastIndex(tail, "@"); at >= 0 {
				return head + tail[:at], tail[at+1:]
			}
			return raw, ""
		}
	}

	slash := strings.Index(raw, "/")
	if slash < 0 {
		return raw, ""
	}
	if at := strings.LastIndex(raw, "@"); at > slash {
		return raw[:at], raw[at+1:]
	}
	return raw, ""
}

func looksLikeHostPath(s string) bool {
	if strings.Contains(s, " ") {
		return false
	}
	parts := strings.SplitN(s, "/", 3)
	if len(parts) < 3 {
		return false
	}
	return strings.Contains(parts[0], ".")
}

// MaterializeTemplateDir resolves src to an on-disk directory (fetching git
// sources as needed) and returns (dir, sha, ref).
func MaterializeTemplateDir(src *Source) (string, string, string, error) {
	if src == nil {
		return "", "", "", errors.New("nil source")
	}
	switch src.Kind {
	case "local":
		info, err := os.Stat(src.Path)
		if err != nil {
			return "", "", "", fmt.Errorf("template path: %w", err)
		}
		if !info.IsDir() {
			return "", "", "", fmt.Errorf("template path %q is not a directory", src.Path)
		}
		return src.Path, "", "", nil
	case "git":
		sha, cachePath, err := FetchGitTemplate(src)
		if err != nil {
			return "", "", "", err
		}
		return cachePath, sha, src.Ref, nil
	default:
		return "", "", "", fmt.Errorf("unknown source kind %q", src.Kind)
	}
}

// LoadTemplate materializes a Source into a Template.
func LoadTemplate(src *Source) (*Template, error) {
	if src == nil {
		return nil, errors.New("nil source")
	}

	var (
		dir    string
		sha    string
		srcRef string
	)

	switch src.Kind {
	case "local":
		info, err := os.Stat(src.Path)
		if err != nil {
			return nil, fmt.Errorf("template path: %w", err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("template path %q is not a directory", src.Path)
		}
		dir = src.Path
	case "git":
		resolvedSHA, cachePath, err := FetchGitTemplate(src)
		if err != nil {
			return nil, err
		}
		dir = cachePath
		sha = resolvedSHA
		srcRef = src.Ref
	default:
		return nil, fmt.Errorf("unknown source kind %q", src.Kind)
	}

	tpl, err := readTemplateFromDir(dir)
	if err != nil {
		return nil, err
	}
	tpl.GitSHA = sha
	tpl.GitRef = srcRef
	if src.Kind == "git" {
		tpl.GitURL = src.URL
	}
	return tpl, nil
}

// ReadLocalTemplate is a convenience for callers that already know the source
// is a filesystem path.
func ReadLocalTemplate(src string) (*Template, error) {
	resolved, err := ResolveTemplateSource(src)
	if err != nil {
		return nil, err
	}
	tpl, err := LoadTemplate(resolved)
	if err != nil {
		return nil, err
	}
	tpl.Source = src
	return tpl, nil
}

func readTemplateFromDir(dir string) (*Template, error) {
	tpl, _, err := ReadTemplateWithValues(dir, "", nil)
	return tpl, err
}

// ReadTemplateWithValues renders agent.yaml(.tmpl) and system_prompt.md(.tmpl)
// using the effective values map and returns both the resolved Template and the
// merged values map.
func ReadTemplateWithValues(dir, name string, overrides map[string]any) (*Template, map[string]any, error) {
	base, err := LoadValues(dir, nil, nil, nil)
	if err != nil {
		return nil, nil, err
	}
	values := DeepMerge(base, overrides)

	agentBytes, promptBytes, err := RenderTemplate(dir, name, values)
	if err != nil {
		return nil, nil, err
	}

	ay, err := ParseAgentYAMLBytes(agentBytes)
	if err != nil {
		return nil, nil, err
	}

	workingDir := dir
	if ay.Spec.WorkingDir != "" && ay.Spec.WorkingDir != "." {
		workingDir = filepath.Join(dir, ay.Spec.WorkingDir)
	}

	return &Template{
		ResolvedPath:               dir,
		SystemPrompt:               string(promptBytes),
		Description:                ay.Spec.Description,
		Role:                       ay.Spec.Role,
		Model:                      ay.Spec.Model,
		Tools:                      append([]string{}, ay.Spec.Tools...),
		RequiredEnv:                append([]string{}, ay.Spec.RequiredEnv...),
		WorkingDir:                 workingDir,
		DangerouslySkipPermissions: ay.Spec.DangerouslySkipPermissions,
		Channels:                   append([]channels.Spec{}, ay.Spec.Channels...),
	}, values, nil
}

// RenderTemplate returns the rendered bytes for agent.yaml and the system
// prompt.
func RenderTemplate(dir, name string, values map[string]any) ([]byte, []byte, error) {
	tmplCtx := map[string]any{
		"Values":  values,
		"Name":    name,
		"Version": deriveTemplateVersion(values),
	}

	agentTmpl := filepath.Join(dir, "agent.yaml.tmpl")
	agentStatic := filepath.Join(dir, "agent.yaml")
	agentBytes, err := renderIfTemplate(agentTmpl, agentStatic, "agent.yaml.tmpl", tmplCtx, values)
	if err != nil {
		return nil, nil, err
	}

	promptStaticRel := DefaultSystemPromptPath
	if parsed, perr := ParseAgentYAMLBytes(agentBytes); perr == nil && parsed.Spec.SystemPromptPath != "" {
		promptStaticRel = parsed.Spec.SystemPromptPath
	}
	promptTmpl := filepath.Join(dir, promptStaticRel+".tmpl")
	promptStatic := filepath.Join(dir, promptStaticRel)
	promptBytes, err := renderIfTemplate(promptTmpl, promptStatic, promptStaticRel+".tmpl", tmplCtx, values)
	if err != nil {
		return nil, nil, err
	}

	return agentBytes, promptBytes, nil
}

func renderIfTemplate(tmplPath, staticPath, relLabel string, ctx map[string]any, values map[string]any) ([]byte, error) {
	data, err := os.ReadFile(tmplPath)
	if err == nil {
		return executeTemplate(relLabel, data, ctx, values)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", relLabel, err)
	}
	static, err := os.ReadFile(staticPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(staticPath), err)
	}
	return static, nil
}

func executeTemplate(relLabel string, body []byte, ctx map[string]any, values map[string]any) ([]byte, error) {
	tpl, err := gotemplate.New(relLabel).Funcs(sprig.TxtFuncMap()).Option("missingkey=error").Parse(string(body))
	if err != nil {
		return nil, normalizeTemplateError(relLabel, err, values)
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, ctx); err != nil {
		return nil, normalizeTemplateError(relLabel, err, values)
	}
	return buf.Bytes(), nil
}

var templateErrLocation = regexp.MustCompile(`template: [^:]+:(\d+)(?::\d+)?`)

var (
	templateErrMissingKey = regexp.MustCompile(`map has no entry for key "([^"]+)"`)
	templateErrEvalField  = regexp.MustCompile(`can't evaluate field (\S+) in type`)
)

func normalizeTemplateError(relLabel string, err error, values map[string]any) error {
	msg := err.Error()
	line := ""
	if m := templateErrLocation.FindStringSubmatch(msg); len(m) == 2 {
		line = m[1]
	}

	missing := ""
	if m := templateErrMissingKey.FindStringSubmatch(msg); len(m) == 2 {
		missing = m[1]
	} else if m := templateErrEvalField.FindStringSubmatch(msg); len(m) == 2 {
		missing = strings.TrimSpace(m[1])
	}

	hint := ""
	if missing != "" {
		if suggestion := caseFoldLookup(values, missing); suggestion != "" {
			hint = fmt.Sprintf(`values.yaml declares %q. Did you mean --set %s=…?`, suggestion, suggestion)
		}
	}

	var b strings.Builder
	b.WriteString("template render error:\n")
	b.WriteString("  file: ")
	b.WriteString(relLabel)
	b.WriteString("\n")
	if line != "" {
		b.WriteString("  line: ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("  error: ")
	b.WriteString(msg)
	if hint != "" {
		b.WriteString("\n  hint: ")
		b.WriteString(hint)
	}
	return errors.New(b.String())
}

func caseFoldLookup(values map[string]any, target string) string {
	want := strings.ToLower(target)
	for k := range values {
		if strings.ToLower(k) == want && k != target {
			return k
		}
	}
	for _, v := range values {
		if m, ok := v.(map[string]any); ok {
			if found := caseFoldLookup(m, target); found != "" {
				return found
			}
		}
	}
	return ""
}

func deriveTemplateVersion(values map[string]any) string {
	if v, ok := values["version"].(string); ok && v != "" {
		return v
	}
	return ""
}

// ParseAgentYAMLBytes parses raw agent.yaml bytes, applies defaults, and
// validates the model, tool, and channel allow-lists.
func ParseAgentYAMLBytes(data []byte) (*AgentYAML, error) {
	ay := &AgentYAML{}
	if len(data) > 0 {
		if err := yaml.Unmarshal(data, ay); err != nil {
			return nil, fmt.Errorf("parse agent.yaml: %w", err)
		}
	}
	applyAgentYAMLDefaults(ay)
	if !templateModelAllowlist[ay.Spec.Model] {
		return nil, fmt.Errorf("agent.yaml model %q is not in the allow-list (allowed: claude-opus-4-7, claude-sonnet-4-6, claude-haiku-4-5)", ay.Spec.Model)
	}
	for _, t := range ay.Spec.Tools {
		if !templateToolAllowlist[t] {
			return nil, fmt.Errorf("agent.yaml tool %q is not in the allow-list", t)
		}
	}
	if err := channels.ValidateSpecs(ay.Spec.Channels); err != nil {
		return nil, fmt.Errorf("agent.yaml %w", err)
	}
	return ay, nil
}

// LoadAgentYAML returns the parsed agent.yaml for templateDir, or a populated
// zero-value if the file doesn't exist.
func LoadAgentYAML(templateDir string) (*AgentYAML, error) {
	p := filepath.Join(templateDir, "agent.yaml")
	data, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read agent.yaml: %w", err)
	}
	return ParseAgentYAMLBytes(data)
}

func applyAgentYAMLDefaults(ay *AgentYAML) {
	if ay.Spec.Model == "" {
		ay.Spec.Model = DefaultTemplateModel
	}
	if len(ay.Spec.Tools) == 0 {
		ay.Spec.Tools = append([]string{}, defaultTemplateTools...)
	}
	if ay.Spec.SystemPromptPath == "" {
		ay.Spec.SystemPromptPath = DefaultSystemPromptPath
	}
}

// FetchGitTemplate resolves src.Ref to a full SHA, clones into the shared
// cache if absent, and returns (sha, cachePath).
func FetchGitTemplate(src *Source) (string, string, error) {
	if src == nil || src.Kind != "git" {
		return "", "", errors.New("not a git source")
	}
	if src.URL == "" {
		return "", "", errors.New("git source missing URL")
	}

	sha, err := resolveRef(src.URL, src.Ref)
	if err != nil {
		return "", "", err
	}

	cacheDir, err := templateCacheRoot()
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return "", "", err
	}

	target := filepath.Join(cacheDir, "sha256-"+sha)

	if cached, ok := cacheHit(target); ok {
		_ = os.Chtimes(target, time.Now(), time.Now())
		return sha, cached, nil
	}

	tmp, err := os.MkdirTemp(cacheDir, ".pull-")
	if err != nil {
		return "", "", err
	}
	cleanupTmp := func() { _ = os.RemoveAll(tmp) }

	if err := cloneAndCheckout(src.URL, src.Ref, sha, tmp); err != nil {
		cleanupTmp()
		return "", "", err
	}
	if !templateHasPrompt(tmp) {
		cleanupTmp()
		return "", "", fmt.Errorf("cloned template missing %s or %s.tmpl", DefaultSystemPromptPath, DefaultSystemPromptPath)
	}

	if err := os.Rename(tmp, target); err != nil {
		if _, ok := cacheHit(target); ok {
			cleanupTmp()
			return sha, target, nil
		}
		cleanupTmp()
		return "", "", fmt.Errorf("move template into cache: %w", err)
	}
	return sha, target, nil
}

func cacheHit(dir string) (string, bool) {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return "", false
	}
	if !templateHasPrompt(dir) {
		return "", false
	}
	return dir, true
}

func templateHasPrompt(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, DefaultSystemPromptPath)); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(dir, DefaultSystemPromptPath+".tmpl")); err == nil {
		return true
	}
	return false
}

func templateCacheRoot() (string, error) {
	dir, err := paths.MeshDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, TemplateCacheSubdir), nil
}

func resolveRef(gitURL, ref string) (string, error) {
	if sha40.MatchString(ref) {
		return ref, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), GitOpTimeout)
	defer cancel()

	lookup := ref
	if lookup == "" {
		lookup = "HEAD"
	}
	cmd := exec.CommandContext(ctx, "git", "ls-remote", gitURL, lookup)
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		if stderr != "" {
			return "", fmt.Errorf("git ls-remote %s %s: %s", gitURL, lookup, stderr)
		}
		return "", fmt.Errorf("git ls-remote %s %s: %w", gitURL, lookup, err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return "", fmt.Errorf("git ls-remote %s %s: no such ref", gitURL, lookup)
	}
	best := ""
	for _, ln := range lines {
		fields := strings.Fields(ln)
		if len(fields) < 2 {
			continue
		}
		sha := fields[0]
		if len(sha) != FullSHALen {
			continue
		}
		if strings.HasSuffix(fields[1], "^{}") {
			return sha, nil
		}
		if best == "" {
			best = sha
		}
	}
	if best == "" {
		return "", fmt.Errorf("git ls-remote %s %s: no usable sha in response", gitURL, lookup)
	}
	return best, nil
}

func cloneAndCheckout(gitURL, ref, sha, dst string) error {
	canShallow := ref != "" && !sha40.MatchString(ref)

	args := []string{"clone"}
	if canShallow {
		args = append(args, "--depth", "1", "--branch", ref)
	}
	args = append(args, gitURL, dst)

	ctx, cancel := context.WithTimeout(context.Background(), GitOpTimeout)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("git clone %s: %w: %s", gitURL, err, strings.TrimSpace(string(out)))
	}

	if !canShallow {
		ctx2, cancel2 := context.WithTimeout(context.Background(), GitOpTimeout)
		defer cancel2()
		if out, err := exec.CommandContext(ctx2, "git", "-C", dst, "checkout", "--detach", sha).CombinedOutput(); err != nil {
			return fmt.Errorf("git checkout %s: %w: %s", sha, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// PruneTemplates removes unreferenced cache entries older than 30 days.
func PruneTemplates() int {
	cacheDir, err := templateCacheRoot()
	if err != nil {
		fmt.Printf("mesh daemon prune: cache path: %v\n", err)
		return 0
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			fmt.Printf("mesh daemon prune: readdir %s: %v\n", cacheDir, err)
		}
		return 0
	}

	referenced := map[string]bool{}
	if s, err := state.LoadState(); err == nil {
		for _, a := range s.Agents {
			if a.TemplateSha != "" {
				referenced[a.TemplateSha] = true
			}
		}
	}

	cutoff := time.Now().Add(-PruneUnreferencedAfter)
	pruned := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "sha256-") {
			continue
		}
		sha := strings.TrimPrefix(e.Name(), "sha256-")
		if referenced[sha] {
			continue
		}
		full := filepath.Join(cacheDir, e.Name())
		info, err := os.Stat(full)
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		if err := os.RemoveAll(full); err != nil {
			fmt.Printf("mesh daemon prune: remove %s: %v\n", full, err)
			continue
		}
		pruned++
	}
	if pruned > 0 {
		fmt.Printf("mesh daemon prune: removed %d unreferenced template(s)\n", pruned)
	}
	return pruned
}

// TemplateCacheEntry is the on-disk view of a cached template.
type TemplateCacheEntry struct {
	Source    string    `json:"source"`
	Ref       string    `json:"ref,omitempty"`
	SHA       string    `json:"sha"`
	CachePath string    `json:"cachePath"`
	LastUsed  time.Time `json:"lastUsed"`
	Agents    int       `json:"agents"`
	Model     string    `json:"model,omitempty"`
}

// ListCachedTemplates walks the template cache and returns one entry per
// sha-keyed directory.
func ListCachedTemplates() ([]TemplateCacheEntry, error) {
	cacheDir, err := templateCacheRoot()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []TemplateCacheEntry{}, nil
		}
		return nil, err
	}

	bySHA := map[string]*TemplateCacheEntry{}
	if s, err := state.LoadState(); err == nil {
		for _, a := range s.Agents {
			if a.TemplateSha == "" {
				continue
			}
			e := bySHA[a.TemplateSha]
			if e == nil {
				e = &TemplateCacheEntry{SHA: a.TemplateSha}
				bySHA[a.TemplateSha] = e
			}
			e.Agents++
			if e.Source == "" {
				e.Source = a.TemplateSource
			}
			if e.Ref == "" {
				e.Ref = a.TemplateRef
			}
		}
	}

	out := make([]TemplateCacheEntry, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "sha256-") {
			continue
		}
		sha := strings.TrimPrefix(e.Name(), "sha256-")
		full := filepath.Join(cacheDir, e.Name())
		info, err := os.Stat(full)
		if err != nil {
			continue
		}
		entry := bySHA[sha]
		if entry == nil {
			entry = &TemplateCacheEntry{SHA: sha}
		}
		entry.CachePath = full
		entry.LastUsed = info.ModTime()

		if ay, err := LoadAgentYAML(full); err == nil {
			entry.Model = ay.Spec.Model
		}

		out = append(out, *entry)
	}
	return out, nil
}

// ShortSHA returns the first 12 chars of a full SHA; safe on shorter inputs.
func ShortSHA(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}

// AtomicWriteFile writes data to path via tmp+rename. Exported so claudeconf
// and other packages can use the same safe write pattern.
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func isLocalPath(src string) bool {
	if strings.HasPrefix(src, "/") || strings.HasPrefix(src, "./") || strings.HasPrefix(src, "../") || strings.HasPrefix(src, "~") || src == "." || src == ".." {
		return true
	}
	return false
}

func expandPath(p string) (string, error) {
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return abs, nil
}
