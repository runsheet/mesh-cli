// Package catalogue builds a product's catalogue bundle from its
// checkout for `mesh product publish` (runsheet ADR 067 §6): the
// manifest at .mesh/product.yml, each kind at .mesh/jobs/<kind>.yml and
// each role in the manifest's roles directory, with every `guidance:`
// read from the markdown file it names and every `finish_schema:` from
// the JSON file it names. The layout is this CLI's
// convention, not Mesh's: Mesh takes the bundle from anything that sends
// it, and checks it again whole.
package catalogue

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"gopkg.in/yaml.v3"
)

// ManifestPath is where a product's manifest lives in its checkout.
const ManifestPath = ".mesh/product.yml"

// JobsDir is where its kinds live, one `<kind>.yml` each.
const JobsDir = ".mesh/jobs/"

// DefaultRolesDir is where its roles live when the manifest names none.
const DefaultRolesDir = ".mesh/roles/"

// MaxGuidance is the most guidance text one kind or role carries, as Mesh
// checks it.
const MaxGuidance = 64 << 10

// MaxFinishSchema is the most a kind's or role's finish_schema may be, as
// Mesh checks it (runsheet ADR 067 decision 10).
const MaxFinishSchema = 64 << 10

var slugRule = regexp.MustCompile(`^[a-z0-9-]+$`)

// Bundle is what PUT /mesh/v1/products/{slug}/catalogue takes. Product is
// the path's, not the body's.
type Bundle struct {
	Product  string   `json:"-"`
	Source   Source   `json:"source"`
	Manifest Manifest `json:"manifest"`
	Kinds    []Kind   `json:"kinds"`
	Roles    []Role   `json:"roles"`
}

// Source is where the bundle came from, for the record.
type Source struct {
	Repo    string `json:"repo,omitempty"`
	Commit  string `json:"commit,omitempty"`
	Version string `json:"version,omitempty"`
}

// Manifest is the bundle's manifest: product.yml less the product, its
// client and its directories.
type Manifest struct {
	DisplayName string   `json:"display_name"`
	MCP         *MCP     `json:"mcp,omitempty"`
	Hooks       *Hooks   `json:"hooks,omitempty"`
	Triggers    []string `json:"triggers,omitempty"`
}

// MCP is the product's own MCP server and the audience a session token
// names to reach it.
type MCP struct {
	URL      string `json:"url" yaml:"url"`
	Audience string `json:"audience" yaml:"audience"`
}

// Hooks is where Mesh posts the product's replies.
type Hooks struct {
	Reply           string `json:"reply" yaml:"reply"`
	ConfirmCallback bool   `json:"confirm_callback,omitempty" yaml:"confirm_callback"`
}

// Kind is one kind of job, its guidance the text itself and its
// finish_schema the JSON Schema's text (runsheet ADR 067 decision 10:
// the product's schema for its part of the result, which Mesh and the
// node validate a session's result against).
type Kind struct {
	Kind         string   `json:"kind"`
	Harness      []string `json:"harness"`
	Finish       string   `json:"finish"`
	Access       []string `json:"access,omitempty"`
	Needs        string   `json:"needs,omitempty"`
	Pods         string   `json:"pods,omitempty"`
	Guidance     string   `json:"guidance,omitempty"`
	FinishSchema string   `json:"finish_schema,omitempty"`
	// FinishTool and BriefTool name tools on the product's own MCP
	// server: the one a session hands its result to, and the one it reads
	// its subject from. The node passes them through without knowing
	// what either does.
	FinishTool string `json:"finish_tool,omitempty"`
	BriefTool  string `json:"brief_tool,omitempty"`
}

// Role is a kind with a trigger, an identity and powers.
type Role struct {
	Kind
	Trigger  string   `json:"trigger"`
	Identity string   `json:"identity"`
	Powers   []string `json:"powers,omitempty"`
}

// manifestFile is .mesh/product.yml as written (runsheet ADR 060 §2),
// plus display_name, which the bundle needs.
type manifestFile struct {
	Product        string   `yaml:"product"`
	DisplayName    string   `yaml:"display_name"`
	LatchkeyClient string   `yaml:"latchkey_client"`
	MCP            *MCP     `yaml:"mcp"`
	Hooks          *Hooks   `yaml:"hooks"`
	Roles          string   `yaml:"roles"`
	Playbooks      string   `yaml:"playbooks"`
	Triggers       []string `yaml:"triggers"`
}

// harness takes `harness: none` and `harness: [claude-code, codex]`.
type harness []string

func (h *harness) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*h = harness{n.Value}
		return nil
	}
	var list []string
	if err := n.Decode(&list); err != nil {
		return err
	}
	*h = list
	return nil
}

// kindFile is a kind file as written (runsheet ADR 061 §2); a role file
// is one with the role fields beside it.
type kindFile struct {
	Kind         string   `yaml:"kind"`
	Harness      harness  `yaml:"harness"`
	Finish       string   `yaml:"finish"`
	Access       []string `yaml:"access"`
	Needs        string   `yaml:"needs"`
	Pods         string   `yaml:"pods"`
	Guidance     string   `yaml:"guidance"`
	FinishSchema string   `yaml:"finish_schema"`
	FinishTool   string   `yaml:"finish_tool"`
	BriefTool    string   `yaml:"brief_tool"`
	Trigger      string   `yaml:"trigger"`
	Identity     string   `yaml:"identity"`
	Powers       []string `yaml:"powers"`
}

// Problems is every reason a checkout's catalogue cannot be published.
type Problems []string

func (p Problems) Error() string { return strings.Join(p, "\n") }

// Load builds the bundle from the checkout at root. It reads strictly (a
// key the shape does not name is refused naming it) and checks what this
// CLI can check before sending: every reason is returned at once.
func Load(root string, src Source) (*Bundle, error) {
	var probs Problems
	add := func(format string, args ...any) { probs = append(probs, fmt.Sprintf(format, args...)) }

	var m manifestFile
	if err := decode(root, ManifestPath, &m); err != nil {
		return nil, Problems{err.Error()}
	}
	if m.Product == "" {
		add("%s: `product` is required", ManifestPath)
	} else if !slugRule.MatchString(m.Product) {
		add("%s: product %q is not lowercase letters, digits and hyphens", ManifestPath, m.Product)
	}
	b := &Bundle{Product: m.Product, Source: src, Manifest: Manifest{
		DisplayName: strings.TrimSpace(m.DisplayName), MCP: m.MCP, Hooks: m.Hooks, Triggers: m.Triggers,
	}}
	if b.Manifest.DisplayName == "" {
		b.Manifest.DisplayName = m.Product
	}
	for _, t := range m.Triggers {
		if t != "thread" && t != "request" {
			add("%s: trigger %q is not one of thread, request", ManifestPath, t)
		}
	}

	seen := map[string]string{}
	read := func(dir string) []kindFile {
		files, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(dir), "*.yml"))
		if err != nil {
			add("%s: %v", dir, err)
			return nil
		}
		sort.Strings(files)
		var out []kindFile
		for _, f := range files {
			rel := path.Join(dir, filepath.Base(f))
			var k kindFile
			if err := decode(root, rel, &k); err != nil {
				add("%s", err)
				continue
			}
			if want := strings.TrimSuffix(filepath.Base(f), ".yml"); k.Kind != want {
				add("%s: declares kind %q: the file is named for its kind", rel, k.Kind)
				continue
			}
			if first, ok := seen[k.Kind]; ok {
				add("%s: kind %s is also %s", rel, k.Kind, first)
				continue
			}
			seen[k.Kind] = rel
			if !slugRule.MatchString(k.Kind) {
				add("%s: kind %q is not lowercase letters, digits and hyphens", rel, k.Kind)
			}
			if len(k.Harness) == 0 {
				add("%s: `harness` is required (claude-code, codex or none)", rel)
			}
			if k.Finish == "" {
				add("%s: `finish` is required", rel)
			}
			text, err := guidance(root, rel, k)
			if err != nil {
				add("%s", err)
			}
			k.Guidance = text
			schema, err := finishSchema(root, rel, k)
			if err != nil {
				add("%s", err)
			}
			k.FinishSchema = schema
			out = append(out, k)
		}
		return out
	}
	for _, k := range read(JobsDir) {
		if k.Trigger != "" || k.Identity != "" || len(k.Powers) > 0 {
			add("%s%s.yml: a kind has no trigger, identity or powers: a role lives in the roles directory", JobsDir, k.Kind)
		}
		b.Kinds = append(b.Kinds, Kind{Kind: k.Kind, Harness: k.Harness, Finish: k.Finish, Access: k.Access, Needs: k.Needs, Pods: k.Pods, Guidance: k.Guidance, FinishSchema: k.FinishSchema, FinishTool: k.FinishTool, BriefTool: k.BriefTool})
	}
	rolesDir := strings.TrimSuffix(cleanDir(m.Roles, DefaultRolesDir), "/") + "/"
	for _, k := range read(rolesDir) {
		rel := rolesDir + k.Kind + ".yml"
		switch {
		case k.Trigger == "":
			add("%s: `trigger` is required: a role is a kind with a trigger", rel)
		case !slices.Contains(m.Triggers, k.Trigger):
			add("%s: trigger %q is not one of the manifest's triggers", rel, k.Trigger)
		}
		if strings.TrimSpace(k.Identity) == "" {
			add("%s: `identity` is required", rel)
		}
		b.Roles = append(b.Roles, Role{
			Kind:    Kind{Kind: k.Kind, Harness: k.Harness, Finish: k.Finish, Access: k.Access, Needs: k.Needs, Pods: k.Pods, Guidance: k.Guidance, FinishSchema: k.FinishSchema, FinishTool: k.FinishTool, BriefTool: k.BriefTool},
			Trigger: k.Trigger, Identity: k.Identity, Powers: k.Powers,
		})
	}
	if b.Kinds == nil {
		b.Kinds = []Kind{}
	}
	if b.Roles == nil {
		b.Roles = []Role{}
	}
	if len(probs) > 0 {
		return b, probs
	}
	return b, nil
}

// guidance reads the markdown file a kind's `guidance:` names, relative
// to the checkout: the prompt stays a file a person reviews as prose,
// and the bundle carries its text. A kind with harness none may name
// none; any other must.
func guidance(root, rel string, k kindFile) (string, error) {
	ref := strings.TrimSpace(k.Guidance)
	none := len(k.Harness) == 1 && k.Harness[0] == "none"
	if ref == "" {
		if none || len(k.Harness) == 0 {
			return "", nil
		}
		return "", fmt.Errorf("%s: `guidance` is required for an agent harness: the markdown file the session runs, e.g. .mesh/agents/%s.md", rel, k.Kind)
	}
	if !strings.HasSuffix(ref, ".md") {
		return "", fmt.Errorf("%s: guidance %q names no markdown file in the checkout (a path ending in .md, e.g. .mesh/agents/%s.md)", rel, ref, k.Kind)
	}
	clean := path.Clean(strings.TrimPrefix(ref, "/"))
	if strings.HasPrefix(clean, "../") || clean == ".." {
		return "", fmt.Errorf("%s: guidance %q is outside the checkout", rel, ref)
	}
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(clean)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%s: guidance %s does not exist", rel, clean)
		}
		return "", fmt.Errorf("%s: guidance %s: %v", rel, clean, err)
	}
	if len(raw) > MaxGuidance {
		return "", fmt.Errorf("%s: guidance %s is %d bytes; at most %d", rel, clean, len(raw), MaxGuidance)
	}
	if strings.TrimSpace(string(raw)) == "" {
		return "", fmt.Errorf("%s: guidance %s is empty", rel, clean)
	}
	return string(raw), nil
}

// finishSchema reads the JSON file a kind's `finish_schema:` names,
// relative to the checkout (runsheet ADR 067 decision 10): the product's
// JSON Schema for its part of the result, kept as a file a person
// reviews, the bundle carrying its text. It is optional; when named it
// must be in the checkout, at most MaxFinishSchema, and a JSON object
// that resolves as a JSON Schema with no external references — what Mesh
// refuses at the publish, refused here first.
func finishSchema(root, rel string, k kindFile) (string, error) {
	ref := strings.TrimSpace(k.FinishSchema)
	if ref == "" {
		return "", nil
	}
	if !strings.HasSuffix(ref, ".json") {
		return "", fmt.Errorf("%s: finish_schema %q names no JSON file in the checkout (a path ending in .json, e.g. .mesh/schemas/%s.json)", rel, ref, k.Kind)
	}
	clean := path.Clean(strings.TrimPrefix(ref, "/"))
	if strings.HasPrefix(clean, "../") || clean == ".." {
		return "", fmt.Errorf("%s: finish_schema %q is outside the checkout", rel, ref)
	}
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(clean)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%s: finish_schema %s does not exist", rel, clean)
		}
		return "", fmt.Errorf("%s: finish_schema %s: %v", rel, clean, err)
	}
	if len(raw) > MaxFinishSchema {
		return "", fmt.Errorf("%s: finish_schema %s is %d bytes; at most %d", rel, clean, len(raw), MaxFinishSchema)
	}
	text := strings.TrimSpace(string(raw))
	var object map[string]any
	if !strings.HasPrefix(text, "{") || json.Unmarshal([]byte(text), &object) != nil {
		return "", fmt.Errorf("%s: finish_schema %s is not a JSON object", rel, clean)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal([]byte(text), &schema); err != nil {
		return "", fmt.Errorf("%s: finish_schema %s does not parse as a JSON Schema: %v", rel, clean, err)
	}
	if _, err := schema.Resolve(nil); err != nil {
		return "", fmt.Errorf("%s: finish_schema %s does not resolve as a JSON Schema: %v", rel, clean, err)
	}
	return text, nil
}

func cleanDir(dir, fallback string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return fallback
	}
	return path.Clean(strings.TrimPrefix(dir, "/"))
}

func decode(root, rel string, into any) error {
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s does not exist", rel)
		}
		return fmt.Errorf("%s: %v", rel, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("%s does not parse: %v", rel, err)
	}
	return nil
}
