package tripline

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
)

// EnvDSN overrides node.yaml's tripline.dsn.
const EnvDSN = "MESH_TRIPLINE_DSN"

// Config is node.yaml's tripline: block.
type Config struct {
	// DSN is the Tripline project's DSN; empty reports nothing.
	DSN string `yaml:"dsn,omitempty"`
	// Enabled turns reporting off with a DSN set; left out, a DSN set is
	// reporting on.
	Enabled *bool `yaml:"enabled,omitempty"`
	// FromEnv is MESH_TRIPLINE_DSN as the node's config was loaded: in
	// force over DSN, never written back to node.yaml.
	FromEnv string `yaml:"-"`
	// Secrets are literal values the Scrubber never sends, set by the
	// node (the token file's, the jobs'), never by node.yaml.
	Secrets []string `yaml:"-"`
}

// DSNInForce is FromEnv, else DSN.
func (c Config) DSNInForce() string {
	if dsn := strings.TrimSpace(c.FromEnv); dsn != "" {
		return dsn
	}
	return strings.TrimSpace(c.DSN)
}

// On says whether c reports: a DSN in force and enabled not false.
func (c Config) On() bool {
	if c.DSNInForce() == "" {
		return false
	}
	return c.Enabled == nil || *c.Enabled
}

// Class is the kind of failure an event reports, a closed set: what the
// node already tells apart in node.log.
type Class string

const (
	// Infrastructure is the platform, the network or the host failing
	// under a session (node.log's `infrastructure:`).
	Infrastructure Class = "infrastructure"
	// HarnessUnavailable is an agent CLI that could not run a session
	// (harness.Unavailable).
	HarnessUnavailable Class = "harness_unavailable"
	// SandboxBroken is a runtime that could not start a pod.
	SandboxBroken Class = "sandbox_broken"
	// SessionFailed is a session that ended without success.
	SessionFailed Class = "session_failed"
)

// Classes is every Class, in order.
func Classes() []Class {
	return []Class{Infrastructure, HarnessUnavailable, SandboxBroken, SessionFailed}
}

// Valid says whether c is one of Classes.
func (c Class) Valid() bool {
	for _, k := range Classes() {
		if c == k {
			return true
		}
	}
	return false
}

// Fields are what an event is about. Empty fields are not tagged.
type Fields struct {
	// Node is the node's slug; empty is the Reporter's.
	Node     string
	ItemID   string
	ItemSeq  int
	GoalSlug string
	Repo     string
	Role     string
}

// Reporter sends the node's failures to Tripline. A nil Reporter, and one
// made with no DSN or enabled false, is disabled: every method is safe
// and sends nothing.
type Reporter struct {
	hub      *sentry.Hub
	node     string
	endpoint string
}

// New makes a Reporter for cfg: its own sentry client and Hub (never the
// global), every event through a Scrubber naming nodeSlug, released as
// version. cfg not On is a disabled Reporter, no error.
func New(cfg Config, nodeSlug, version string) (*Reporter, error) {
	if !cfg.On() {
		return &Reporter{node: nodeSlug}, nil
	}
	dsn := cfg.DSNInForce()
	u, err := url.Parse(dsn)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("tripline: dsn is not a URL with a host")
	}
	scrubber := NewScrubber(Options{Node: nodeSlug, Secrets: cfg.Secrets})
	client, err := sentry.NewClient(sentry.ClientOptions{
		Dsn:        dsn,
		Release:    version,
		ServerName: nodeSlug,
		BeforeSend: scrubber.Scrub,
	})
	if err != nil {
		// sentry's error quotes the DSN; its key is not ours to log.
		return nil, fmt.Errorf("tripline: dsn for %s refused", u.Host)
	}
	return &Reporter{
		hub:      sentry.NewHub(client, sentry.NewScope()),
		node:     nodeSlug,
		endpoint: u.Host,
	}, nil
}

// Enabled says whether r sends anything.
func (r *Reporter) Enabled() bool { return r != nil && r.hub != nil }

// Endpoint is the ingest's host, never the DSN's key; "" when disabled.
func (r *Reporter) Endpoint() string {
	if !r.Enabled() {
		return ""
	}
	return r.endpoint
}

// Report sends err as one event of class, tagged node, item, goal and
// class (and seq, repo and role when set), fingerprinted [class,
// Normalise(message)] so alike failures on different items are one
// issue. A nil err, a class not in Classes or a disabled r sends nothing.
func (r *Reporter) Report(class Class, err error, f Fields) {
	if !r.Enabled() || err == nil || !class.Valid() {
		return
	}
	node := f.Node
	if node == "" {
		node = r.node
	}
	tags := map[string]string{"class": string(class)}
	for k, v := range map[string]string{
		"node": node,
		"item": f.ItemID,
		"goal": f.GoalSlug,
		"repo": f.Repo,
		"role": f.Role,
	} {
		if v != "" {
			tags[k] = v
		}
	}
	if f.ItemSeq > 0 {
		tags["seq"] = fmt.Sprint(f.ItemSeq)
	}
	fingerprint := []string{string(class), Normalise(err.Error(), f.ItemID, f.GoalSlug, f.Repo, node)}

	hub := r.hub.Clone()
	hub.ConfigureScope(func(scope *sentry.Scope) {
		scope.SetLevel(sentry.LevelError)
		scope.SetTags(tags)
		scope.SetFingerprint(fingerprint)
	})
	hub.CaptureException(err)
}

// Flush waits up to timeout for the events sent so far, and says whether
// they all went. A disabled r has nothing to wait for.
func (r *Reporter) Flush(timeout time.Duration) bool {
	if !r.Enabled() {
		return true
	}
	return r.hub.Flush(timeout)
}

var normalisers = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`), "<id>"},
	// A path, absolute or relative, or an owner/name: anything with a
	// slash in it, a URL's host and path included.
	{regexp.MustCompile(`[~.]?[\w.@+\-]*(?:/[\w.@+\-]+)+/?`), "<path>"},
	{regexp.MustCompile(`\b(?:\d+(?:\.\d+)?(?:ns|us|µs|ms|s|m|h))+\b`), "<duration>"},
	{regexp.MustCompile(`(?i)\b[0-9a-f]{7,64}\b`), "<sha>"},
	{regexp.MustCompile(`\b\d+(?:\.\d+)?\b`), "<n>"},
}

var hasDigit = regexp.MustCompile(`\d`)

// Normalise is msg with what differs between alike failures taken out:
// the literal values given (an item's id, a goal's slug), then ids, paths,
// durations, hex shas and numbers, each written as a placeholder.
func Normalise(msg string, literals ...string) string {
	for _, l := range literals {
		if l = strings.TrimSpace(l); len(l) >= 3 {
			msg = strings.ReplaceAll(msg, l, "<value>")
		}
	}
	for _, n := range normalisers {
		if n.repl == "<sha>" {
			// a word of hex letters alone ("defaced") is a word
			msg = n.re.ReplaceAllStringFunc(msg, func(m string) string {
				if hasDigit.MatchString(m) {
					return n.repl
				}
				return m
			})
			continue
		}
		msg = n.re.ReplaceAllString(msg, n.repl)
	}
	return strings.Join(strings.Fields(msg), " ")
}
