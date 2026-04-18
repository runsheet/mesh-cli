package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Template is the resolved, slice-1 view of an agent template. Slice 2 will
// parse agent.yaml into richer metadata; for now we synthesize sensible
// defaults when it's absent.
type Template struct {
	Source       string   // user-supplied --template value
	ResolvedPath string   // absolute directory
	SystemPrompt string   // contents of system_prompt.md
	Model        string
	Tools        []string
	RequiredEnv  []string
	WorkingDir   string
}

var defaultTemplateTools = []string{"Read", "Edit", "Write", "Bash", "Grep", "Glob", "WebFetch", "Task"}

const defaultTemplateModel = "claude-opus-4-7"

// ReadLocalTemplate validates and loads a template from a local filesystem
// path. Git URL templates are slice-2; we reject them with a clear error.
func ReadLocalTemplate(src string) (*Template, error) {
	if strings.TrimSpace(src) == "" {
		return nil, errors.New("template source is required")
	}

	if !isLocalPath(src) {
		return nil, fmt.Errorf("template %q looks like a git source; git templates ship in slice 2 (use a local path for now)", src)
	}

	resolved, err := expandPath(src)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("template path: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("template path %q is not a directory", resolved)
	}

	promptPath := filepath.Join(resolved, "system_prompt.md")
	promptBytes, err := os.ReadFile(promptPath)
	if err != nil {
		return nil, fmt.Errorf("read system_prompt.md: %w", err)
	}

	tpl := &Template{
		Source:       src,
		ResolvedPath: resolved,
		SystemPrompt: string(promptBytes),
		Model:        defaultTemplateModel,
		Tools:        append([]string{}, defaultTemplateTools...),
		WorkingDir:   resolved,
	}

	// Slice 1: agent.yaml is optional. When present we surface the fact so
	// logs are less confusing, but parsing lands in slice 2 alongside the rest
	// of the K8s-style schema.
	if _, err := os.Stat(filepath.Join(resolved, "agent.yaml")); err == nil {
		fmt.Fprintf(os.Stderr, "mesh: template has agent.yaml; slice 1 ignores it (defaults will be used)\n")
	}

	return tpl, nil
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
