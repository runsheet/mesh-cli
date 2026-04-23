package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// snapshotAgent writes a self-contained snapshot of a rendered template into
// ~/.mesh/agents/<name>/. The snapshot contains:
//   - agent.yaml  (rendered, with systemPromptPath rewritten to "system_prompt.md")
//   - system_prompt.md (rendered prompt content, without the loop-template suffix)
//
// The loop-template suffix is appended by writeAgentPromptFile at start time;
// the snapshot stores only the agent-authored portion so update can overwrite
// cleanly. Callers that need the full prompt on disk should call
// writeAgentPromptFile after snapshotAgent.
//
// systemPromptPath values that point outside the template dir are rejected
// unless they are the default "system_prompt.md".
func snapshotAgent(agentName string, agentBytes []byte, promptBytes []byte) error {
	dir, err := agentWorkdir(agentName)
	if err != nil {
		return fmt.Errorf("snapshot: agent workdir: %w", err)
	}

	// Rewrite systemPromptPath in the rendered agent.yaml so it always points
	// at the snapshot copy, regardless of what the template declared.
	agentBytes, err = rewriteSystemPromptPath(agentBytes)
	if err != nil {
		return fmt.Errorf("snapshot: rewrite systemPromptPath: %w", err)
	}

	agentDst := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(agentDst, agentBytes, 0o600); err != nil {
		return fmt.Errorf("snapshot: write agent.yaml: %w", err)
	}

	promptDst := filepath.Join(dir, "system_prompt.md")
	if err := os.WriteFile(promptDst, promptBytes, 0o600); err != nil {
		return fmt.Errorf("snapshot: write system_prompt.md: %w", err)
	}

	return nil
}

// rewriteSystemPromptPath parses agentBytes as YAML, sets
// spec.systemPromptPath to "system_prompt.md", and returns the re-marshalled
// bytes. This ensures the snapshotted agent.yaml always refers to the file
// that lives beside it in the agent dir.
func rewriteSystemPromptPath(agentBytes []byte) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(agentBytes, &doc); err != nil {
		return nil, fmt.Errorf("parse agent.yaml: %w", err)
	}

	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		root := doc.Content[0]
		if root.Kind == yaml.MappingNode {
			setYAMLField(root, "spec", func(specNode *yaml.Node) {
				setYAMLScalar(specNode, "systemPromptPath", "system_prompt.md")
			})
		}
	}

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("marshal agent.yaml: %w", err)
	}
	return out, nil
}

// setYAMLField finds the mapping node for key in m (a mapping node) and calls
// fn with the value node. If key is absent, a new key+value mapping node pair
// is appended and fn is called with the new (empty mapping) value.
func setYAMLField(m *yaml.Node, key string, fn func(*yaml.Node)) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			fn(m.Content[i+1])
			return
		}
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: key}
	valNode := &yaml.Node{Kind: yaml.MappingNode}
	m.Content = append(m.Content, keyNode, valNode)
	fn(valNode)
}

// setYAMLScalar sets key=value inside a mapping node, adding the key if absent.
func setYAMLScalar(m *yaml.Node, key, value string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1].Value = value
			m.Content[i+1].Kind = yaml.ScalarNode
			return
		}
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Value: value},
	)
}

// validateSystemPromptPath checks that the systemPromptPath in agentBytes does
// not reference a path outside the template dir. If it does, an actionable
// error is returned. The default "system_prompt.md" always passes.
func validateSystemPromptPath(agentBytes []byte, templateDir string) error {
	ay, err := parseAgentYAMLBytes(agentBytes)
	if err != nil {
		return err
	}
	p := ay.Spec.SystemPromptPath
	if p == "" || p == defaultSystemPromptPath {
		return nil
	}
	if filepath.IsAbs(p) {
		return fmt.Errorf("agent.yaml systemPromptPath %q is an absolute path — snapshot cannot include files outside the template; use a relative path", p)
	}
	clean := filepath.Clean(p)
	if strings.HasPrefix(clean, "..") {
		return fmt.Errorf("agent.yaml systemPromptPath %q escapes the template directory — snapshot cannot follow it; move the file into the template", p)
	}
	// Verify the file actually exists under templateDir.
	full := filepath.Join(templateDir, p)
	if _, err := os.Stat(full); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("agent.yaml systemPromptPath %q: file not found under template dir %s", p, templateDir)
	}
	return nil
}

// validateSystemPromptPathInDir renders the agent.yaml (just enough to read
// systemPromptPath) against the given values and checks it doesn't escape the
// template dir. Called before any persistent side-effects.
func validateSystemPromptPathInDir(tplDir, agentName string, overrides map[string]any) error {
	base, err := LoadValues(tplDir, nil, nil, nil)
	if err != nil {
		return err
	}
	values := deepMerge(base, overrides)
	agentBytes, _, err := RenderTemplate(tplDir, agentName, values)
	if err != nil {
		return err
	}
	return validateSystemPromptPath(agentBytes, tplDir)
}

// snapshotExists returns true when both agent.yaml and system_prompt.md are
// present under ~/.mesh/agents/<name>/.
func snapshotExists(agentName string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	dir := filepath.Join(home, ".mesh", "agents", agentName)
	for _, f := range []string{"agent.yaml", "system_prompt.md"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			return false
		}
	}
	return true
}

// loadSnapshotAgentYAML reads and parses the snapshotted agent.yaml from
// ~/.mesh/agents/<name>/agent.yaml.
func loadSnapshotAgentYAML(agentName string) (*AgentYAML, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(home, ".mesh", "agents", agentName, "agent.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read snapshot agent.yaml: %w", err)
	}
	return parseAgentYAMLBytes(data)
}

// loadSnapshotSystemPrompt reads the raw system_prompt.md from the agent
// snapshot dir (no loop-template suffix appended).
func loadSnapshotSystemPrompt(agentName string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(home, ".mesh", "agents", agentName, "system_prompt.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read snapshot system_prompt.md: %w", err)
	}
	return string(data), nil
}
