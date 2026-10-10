// Command genfields writes the gateway client's field selections
// (fields_gen.go) from the committed schema.json and the per-entity
// allow-lists under fields/, and with -refresh re-reads schema.json from
// the live gateway (docs/adr/adr-010-generated-gateway-fields.md).
//
// Run from internal/gateway:
//
//	go generate ./internal/gateway/...          # schema.json → fields_gen.go
//	go run ./internal/genfields -refresh        # the gateway → schema.json
//
// Every file it writes has one entry per line, sorted, so two changes
// that each add a field touch different lines.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func main() {
	refresh := flag.Bool("refresh", false, "introspect the gateway and write the schema file (no token needed: the schema is public)")
	url := flag.String("url", "https://api.runsheet.dev/graphql", "the gateway's GraphQL endpoint, for -refresh")
	refreshRulesFlag := flag.Bool("refresh-rules", false, "fetch runsheet's node-mutations.json and rewrite the committed copy canonically when it changes")
	rulesURL := flag.String("rules-url", defaultRulesURL, "where runsheet publishes node-mutations.json, for -refresh-rules")
	rulesPath := flag.String("rules", "gatewaytest/testdata/node-mutations.json", "the committed copy of node-mutations.json")
	schemaPath := flag.String("schema", "schema.json", "the committed schema")
	fieldsDir := flag.String("fields", "fields", "the directory of per-entity allow-lists, <Type>.txt")
	out := flag.String("out", "fields_gen.go", "the generated Go file")
	flag.Parse()

	var err error
	if *refreshRulesFlag {
		var changed bool
		changed, err = refreshRules(*rulesURL, *rulesPath)
		if err == nil {
			if changed {
				fmt.Println("genfields: updated", *rulesPath)
			} else {
				fmt.Println("genfields:", *rulesPath, "is up to date")
			}
		}
	} else if *refresh {
		err = refreshSchema(*url, *schemaPath)
	} else {
		err = generate(*schemaPath, *fieldsDir, *out)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "genfields:", err)
		os.Exit(1)
	}
}

// defaultRulesURL is where runsheet publishes the node mutations' rule
// fixture (docs/adr/adr-026-the-node-and-the-gateway-share-one-rule-fixture.md).
const defaultRulesURL = "https://raw.githubusercontent.com/runsheet/runsheet/main/specs/contracts/node-mutations.json"

// refreshRules fetches the rule fixture from url and, when it differs,
// rewrites path in the canonical form: sorted keys, 2-space indent, a
// trailing newline. A 404 means runsheet does not publish it yet: the
// file is left alone and that is no error. Any other failure leaves the
// file untouched and is returned.
func refreshRules(url, path string) (changed bool, err error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		fmt.Fprintf(os.Stderr, "genfields: %s: runsheet does not publish it yet (404); %s left as it is\n", url, path)
		return false, nil
	default:
		return false, fmt.Errorf("%s: %s: %s", url, resp.Status, bytes.TrimSpace(raw))
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return false, fmt.Errorf("%s: %w", url, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return false, fmt.Errorf("%s: trailing data after the JSON document", url)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil { // map keys are written sorted
		return false, err
	}
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, buf.Bytes()) {
		return false, nil
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// Schema is schema.json: every object, interface and input type the
// gateway serves with its fields, sorted by name.
type Schema struct {
	Types []Type `json:"types"`
}

// Type is one schema type.
type Type struct {
	Name   string  `json:"name"`
	Kind   string  `json:"kind"`
	Fields []Field `json:"fields"`
}

// Field is one field (or input field) and its type in SDL notation.
type Field struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type typeRef struct {
	Kind   string   `json:"kind"`
	Name   *string  `json:"name"`
	OfType *typeRef `json:"ofType"`
}

func (t *typeRef) String() string {
	switch {
	case t == nil:
		return ""
	case t.Kind == "NON_NULL":
		return t.OfType.String() + "!"
	case t.Kind == "LIST":
		return "[" + t.OfType.String() + "]"
	case t.Name != nil:
		return *t.Name
	}
	return ""
}

const introspection = `{ __schema { types { name kind
  fields(includeDeprecated: true) { name type { ...T } }
  inputFields { name type { ...T } } } } }
fragment T on __Type { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name } } } } }`

func refreshSchema(url, path string) error {
	body, _ := json.Marshal(map[string]string{"query": introspection})
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s: %s", url, resp.Status, bytes.TrimSpace(raw))
	}
	type field struct {
		Name string  `json:"name"`
		Type typeRef `json:"type"`
	}
	var r struct {
		Data struct {
			Schema struct {
				Types []struct {
					Name        string  `json:"name"`
					Kind        string  `json:"kind"`
					Fields      []field `json:"fields"`
					InputFields []field `json:"inputFields"`
				} `json:"types"`
			} `json:"__schema"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("%s: %w", url, err)
	}
	if len(r.Errors) > 0 {
		return fmt.Errorf("%s: %s", url, r.Errors[0].Message)
	}
	var s Schema
	for _, t := range r.Data.Schema.Types {
		if strings.HasPrefix(t.Name, "__") {
			continue
		}
		switch t.Kind {
		case "OBJECT", "INTERFACE", "INPUT_OBJECT":
		default:
			continue
		}
		st := Type{Name: t.Name, Kind: t.Kind, Fields: []Field{}}
		for _, f := range append(t.Fields, t.InputFields...) {
			st.Fields = append(st.Fields, Field{Name: f.Name, Type: f.Type.String()})
		}
		sort.Slice(st.Fields, func(i, j int) bool { return st.Fields[i].Name < st.Fields[j].Name })
		s.Types = append(s.Types, st)
	}
	if len(s.Types) == 0 {
		return fmt.Errorf("%s: the introspection returned no types", url)
	}
	sort.Slice(s.Types, func(i, j int) bool { return s.Types[i].Name < s.Types[j].Name })
	return os.WriteFile(path, renderSchema(s), 0o644)
}

// renderSchema writes the schema as JSON with each field on a line of
// its own, so a field added upstream is one added line.
func renderSchema(s Schema) []byte {
	var b bytes.Buffer
	q := func(v string) string { j, _ := json.Marshal(v); return string(j) }
	b.WriteString("{\n  \"types\": [\n")
	for i, t := range s.Types {
		fmt.Fprintf(&b, "    {\n      \"name\": %s,\n      \"kind\": %s,\n      \"fields\": [", q(t.Name), q(t.Kind))
		for j, f := range t.Fields {
			if j > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, "\n        {\"name\": %s, \"type\": %s}", q(f.Name), q(f.Type))
		}
		if len(t.Fields) > 0 {
			b.WriteString("\n      ")
		}
		b.WriteString("]\n    }")
		if i < len(s.Types)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("  ]\n}\n")
	return b.Bytes()
}

// allowList is one fields/<Type>.txt: the fields the client selects on
// Type, those every platform it supports serves and those an older one
// may lack (a `?` suffix).
type allowList struct {
	Type     string
	Required []string
	Optional []string
}

func readAllowList(path string) (allowList, error) {
	a := allowList{Type: strings.TrimSuffix(filepath.Base(path), ".txt")}
	raw, err := os.ReadFile(path)
	if err != nil {
		return a, err
	}
	var prev string
	for n, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, optional := strings.CutSuffix(line, "?")
		if strings.ContainsAny(name, " \t?") {
			return a, fmt.Errorf("%s:%d: %q is not one field name", path, n+1, line)
		}
		if name <= prev {
			return a, fmt.Errorf("%s:%d: %q is out of order or repeated (keep one name per line, sorted)", path, n+1, name)
		}
		prev = name
		if optional {
			a.Optional = append(a.Optional, name)
		} else {
			a.Required = append(a.Required, name)
		}
	}
	if len(a.Required) == 0 {
		return a, fmt.Errorf("%s: no required fields", path)
	}
	return a, nil
}

func generate(schemaPath, fieldsDir, out string) error {
	raw, err := os.ReadFile(schemaPath)
	if err != nil {
		return err
	}
	var s Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("%s: %w", schemaPath, err)
	}
	types := map[string]map[string]bool{}
	for _, t := range s.Types {
		fs := map[string]bool{}
		for _, f := range t.Fields {
			fs[f.Name] = true
		}
		types[t.Name] = fs
	}

	paths, err := filepath.Glob(filepath.Join(fieldsDir, "*.txt"))
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("%s: no allow-lists", fieldsDir)
	}
	sort.Strings(paths)
	var lists []allowList
	var errs []error
	for _, p := range paths {
		a, err := readAllowList(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		fs, ok := types[a.Type]
		if !ok {
			errs = append(errs, fmt.Errorf("%s: type %s is not in %s", p, a.Type, schemaPath))
			continue
		}
		for _, name := range append(append([]string{}, a.Required...), a.Optional...) {
			if !fs[name] {
				errs = append(errs, fmt.Errorf("%s: field %s.%s is not in %s", p, a.Type, name, schemaPath))
			}
		}
		lists = append(lists, a)
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}

	var b bytes.Buffer
	b.WriteString("// Code generated by internal/genfields from schema.json and fields/*.txt. DO NOT EDIT.\n\n")
	b.WriteString("package gateway\n\nimport \"strings\"\n")
	for _, a := range lists {
		p := prefix(a.Type)
		fmt.Fprintf(&b, "\n// %sRequired are the %s fields every supported platform serves (fields/%s.txt).\n", p, a.Type, a.Type)
		writeList(&b, p+"Required", a.Required)
		fmt.Fprintf(&b, "\n// %sOptional are the %s fields an older platform may lack (a `?` in fields/%s.txt).\n", p, a.Type, a.Type)
		writeList(&b, p+"Optional", a.Optional)
		fmt.Fprintf(&b, "\n// %sFields is the selection of %sRequired.\n", p, p)
		fmt.Fprintf(&b, "var %sFields = strings.Join(%sRequired, \" \")\n", p, p)
		fmt.Fprintf(&b, "\n// %sOptionalFields is the selection of %sOptional.\n", p, p)
		fmt.Fprintf(&b, "var %sOptionalFields = strings.Join(%sOptional, \" \")\n", p, p)
	}
	src, err := format.Source(b.Bytes())
	if err != nil {
		return err
	}
	return os.WriteFile(out, src, 0o644)
}

func writeList(b *bytes.Buffer, name string, fields []string) {
	fmt.Fprintf(b, "var %s = []string{\n", name)
	for _, f := range fields {
		fmt.Fprintf(b, "\t%q,\n", f)
	}
	b.WriteString("}\n")
}

// prefix is the Go name an entity's lists go by: WorkItemIndex is
// workItem.
func prefix(typ string) string {
	p := strings.TrimSuffix(typ, "Index")
	return strings.ToLower(p[:1]) + p[1:]
}
