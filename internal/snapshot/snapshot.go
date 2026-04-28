package snapshot

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"projectmesh.io/mesh-cli/internal/paths"
	itpl "projectmesh.io/mesh-cli/internal/template"
)

// Agent writes a self-contained snapshot of a rendered template into
// ~/.mesh/agents/<name>/. The snapshot contains:
//   - agent.yaml  (rendered, with systemPromptPath rewritten to "system_prompt.md")
//   - system_prompt.md (rendered prompt content, without the loop-template suffix)
//
// The loop-template suffix is appended by writeAgentPromptFile at start time;
// the snapshot stores only the agent-authored portion so update can overwrite
// cleanly.
func Agent(agentName string, agentBytes []byte, promptBytes []byte) error {
	dir, err := paths.AgentWorkdir(agentName)
	if err != nil {
		return fmt.Errorf("snapshot: agent workdir: %w", err)
	}

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

// ValidateSystemPromptPath checks that the systemPromptPath in agentBytes does
// not reference a path outside the template dir.
func ValidateSystemPromptPath(agentBytes []byte, templateDir string) error {
	ay, err := itpl.ParseAgentYAMLBytes(agentBytes)
	if err != nil {
		return err
	}
	p := ay.Spec.SystemPromptPath
	if p == "" || p == itpl.DefaultSystemPromptPath {
		return nil
	}
	if filepath.IsAbs(p) {
		return fmt.Errorf("agent.yaml systemPromptPath %q is an absolute path — snapshot cannot include files outside the template; use a relative path", p)
	}
	clean := filepath.Clean(p)
	if strings.HasPrefix(clean, "..") {
		return fmt.Errorf("agent.yaml systemPromptPath %q escapes the template directory — snapshot cannot follow it; move the file into the template", p)
	}
	full := filepath.Join(templateDir, p)
	if _, err := os.Stat(full); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("agent.yaml systemPromptPath %q: file not found under template dir %s", p, templateDir)
	}
	return nil
}

// ValidateSystemPromptPathInDir renders the agent.yaml against the given values
// and checks it doesn't escape the template dir.
func ValidateSystemPromptPathInDir(tplDir, agentName string, overrides map[string]any) error {
	base, err := itpl.LoadValues(tplDir, nil, nil, nil)
	if err != nil {
		return err
	}
	values := itpl.DeepMerge(base, overrides)
	agentBytes, _, err := itpl.RenderTemplate(tplDir, agentName, values)
	if err != nil {
		return err
	}
	return ValidateSystemPromptPath(agentBytes, tplDir)
}

// Exists returns true when both agent.yaml and system_prompt.md are present
// under ~/.mesh/agents/<name>/.
func Exists(agentName string) bool {
	dir, err := paths.MeshDir()
	if err != nil {
		return false
	}
	agentDir := filepath.Join(dir, "agents", agentName)
	for _, f := range []string{"agent.yaml", "system_prompt.md"} {
		if _, err := os.Stat(filepath.Join(agentDir, f)); err != nil {
			return false
		}
	}
	return true
}

// LoadAgentYAML reads and parses the snapshotted agent.yaml.
func LoadAgentYAML(agentName string) (*itpl.AgentYAML, error) {
	dir, err := paths.MeshDir()
	if err != nil {
		return nil, err
	}
	p := filepath.Join(dir, "agents", agentName, "agent.yaml")
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("read snapshot agent.yaml: %w", err)
	}
	return itpl.ParseAgentYAMLBytes(data)
}

// LoadSystemPrompt reads the raw system_prompt.md from the agent snapshot dir
// (no loop-template suffix appended).
func LoadSystemPrompt(agentName string) (string, error) {
	dir, err := paths.MeshDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, "agents", agentName, "system_prompt.md")
	data, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("read snapshot system_prompt.md: %w", err)
	}
	return string(data), nil
}
