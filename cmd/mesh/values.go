package main

// This file is a thin compatibility shim. Values helpers now live in
// internal/template; the wrappers below let cmd/mesh/ continue to use the old
// names without change until Phase 9.
//
// TODO(spec22-phase9): inline all call sites to internal/template.

import itpl "projectmesh.io/mesh-cli/internal/template"

func LoadValues(templateDir string, files []string, sets []string, setStrings []string) (map[string]any, error) {
	return itpl.LoadValues(templateDir, files, sets, setStrings)
}

func deepMerge(base, override map[string]any) map[string]any {
	return itpl.DeepMerge(base, override)
}
