package main

// This file is a thin compatibility shim. All template types and helpers now
// live in internal/template; the aliases and wrappers below let the rest of
// cmd/mesh/ continue to use the old names without change until Phase 9.
//
// TODO(spec22-phase9): inline all call sites to internal/template.

import (
	itpl "projectmesh.io/mesh-cli/internal/template"
)

type Template = itpl.Template
type Source = itpl.Source
type AgentYAML = itpl.AgentYAML
type AgentYAMLMeta = itpl.AgentYAMLMeta
type AgentYAMLSpec = itpl.AgentYAMLSpec
type TemplateCacheEntry = itpl.TemplateCacheEntry

const (
	defaultTemplateModel    = itpl.DefaultTemplateModel
	defaultSystemPromptPath = itpl.DefaultSystemPromptPath
)

func ResolveTemplateSource(raw string) (*Source, error) { return itpl.ResolveTemplateSource(raw) }
func LoadTemplate(src *Source) (*Template, error)       { return itpl.LoadTemplate(src) }
func ReadLocalTemplate(src string) (*Template, error)   { return itpl.ReadLocalTemplate(src) }
func RenderTemplate(dir, name string, values map[string]any) ([]byte, []byte, error) {
	return itpl.RenderTemplate(dir, name, values)
}
func parseAgentYAMLBytes(data []byte) (*AgentYAML, error) { return itpl.ParseAgentYAMLBytes(data) }
func LoadAgentYAML(templateDir string) (*AgentYAML, error) { return itpl.LoadAgentYAML(templateDir) }
func FetchGitTemplate(src *Source) (string, string, error)  { return itpl.FetchGitTemplate(src) }
func PruneTemplates() int                                   { return itpl.PruneTemplates() }
func ListCachedTemplates() ([]TemplateCacheEntry, error)    { return itpl.ListCachedTemplates() }
func ShortSHA(sha string) string                            { return itpl.ShortSHA(sha) }
func materializeTemplateDir(src *Source) (string, string, string, error) {
	return itpl.MaterializeTemplateDir(src)
}
func readTemplateWithValues(dir, name string, overrides map[string]any) (*Template, map[string]any, error) {
	return itpl.ReadTemplateWithValues(dir, name, overrides)
}
