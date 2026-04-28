package template

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadValues assembles the final values map from, in ascending precedence:
//  1. <templateDir>/values.yaml (missing file → empty map, not an error).
//  2. each file in files (later wins; deep merge).
//  3. each --set entry in sets (YAML-parsed value; later wins).
//  4. each --set-string entry in setStrings (string-coerced; later wins).
func LoadValues(templateDir string, files []string, sets []string, setStrings []string) (map[string]any, error) {
	values := map[string]any{}

	if templateDir != "" {
		base, err := readValuesFile(filepath.Join(templateDir, "values.yaml"))
		if err != nil {
			return nil, err
		}
		values = DeepMerge(values, base)
	}

	for _, path := range files {
		v, err := readValuesFile(path)
		if err != nil {
			return nil, err
		}
		values = DeepMerge(values, v)
	}

	for _, s := range sets {
		key, raw, err := splitSet(s)
		if err != nil {
			return nil, err
		}
		parsed, err := parseSetValue(raw)
		if err != nil {
			return nil, fmt.Errorf("--set %s: %w", s, err)
		}
		values = setNested(values, key, parsed)
	}

	for _, s := range setStrings {
		key, raw, err := splitSet(s)
		if err != nil {
			return nil, err
		}
		values = setNested(values, key, raw)
	}

	return values, nil
}

func readValuesFile(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	var parsed any
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if parsed == nil {
		return map[string]any{}, nil
	}
	normalized, ok := normalizeYAML(parsed).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: top-level must be a map", path)
	}
	return normalized, nil
}

// normalizeYAML converts yaml.v3's map[any]any shape into map[string]any so
// template lookups work uniformly. Lists recurse as []any.
func normalizeYAML(in any) any {
	switch v := in.(type) {
	case map[any]any:
		out := make(map[string]any, len(v))
		for k, val := range v {
			out[fmt.Sprint(k)] = normalizeYAML(val)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, val := range v {
			out[k] = normalizeYAML(val)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, val := range v {
			out[i] = normalizeYAML(val)
		}
		return out
	default:
		return in
	}
}

// DeepMerge returns a new map with override applied on top of base. Scalars
// and lists are replaced wholesale; maps recurse.
func DeepMerge(base, override map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(override))
	maps.Copy(out, base)
	for k, v := range override {
		if existing, ok := out[k]; ok {
			if em, ok := existing.(map[string]any); ok {
				if vm, ok := v.(map[string]any); ok {
					out[k] = DeepMerge(em, vm)
					continue
				}
			}
		}
		out[k] = v
	}
	return out
}

func splitSet(s string) (string, string, error) {
	idx := strings.Index(s, "=")
	if idx <= 0 {
		return "", "", fmt.Errorf("invalid --set %q (expected key=value)", s)
	}
	return s[:idx], s[idx+1:], nil
}

func parseSetValue(raw string) (any, error) {
	if strings.Contains(raw, ",") && !strings.HasPrefix(raw, "[") && !strings.HasPrefix(raw, "{") {
		parts := strings.Split(raw, ",")
		out := make([]any, 0, len(parts))
		for _, p := range parts {
			v, err := parseScalar(p)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	return parseScalar(raw)
}

func parseScalar(raw string) (any, error) {
	var v any
	if err := yaml.Unmarshal([]byte(raw), &v); err != nil {
		return raw, nil
	}
	return normalizeYAML(v), nil
}

func setNested(values map[string]any, key string, value any) map[string]any {
	if values == nil {
		values = map[string]any{}
	}
	parts := strings.Split(key, ".")
	cur := values
	for i, p := range parts {
		if i == len(parts)-1 {
			cur[p] = value
			return values
		}
		next, ok := cur[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
	return values
}
