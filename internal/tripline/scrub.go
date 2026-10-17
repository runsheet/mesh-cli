// Package tripline reports the node's errors to Tripline, a Sentry-protocol
// ingest. Nothing leaves the node unscrubbed: every event passes through
// Scrub (sentry-go's BeforeSend) first.
package tripline

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/getsentry/sentry-go"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/tools"
)

// Redacted is what a secret is replaced with: the harness's own mark, so
// node.log and Tripline read alike.
const Redacted = harness.Redacted

// Options are what a Scrubber knows beyond its patterns.
type Options struct {
	// Node is the node's slug, the only server name an event keeps.
	Node string
	// Home is the operator's home directory, written ~ in an event.
	// Empty is os.UserHomeDir.
	Home string
	// Secrets are literal values never sent: the jobs' secrets, the
	// tokens of ~/.mesh/token (TokenFileSecrets).
	Secrets []string
	// Env is KEY=value pairs whose values are never sent, as a session's
	// tool env is (tools.Redact).
	Env []string
}

// Scrubber redacts every field of a sentry event. The zero value scrubs
// by pattern alone; it is safe for concurrent use once made.
type Scrubber struct {
	node    string
	home    string
	secrets []string // longest first
	env     []string
}

// minFragment is the shortest run of a literal secret redacted where the
// whole is not present: a secret split across two fields is caught by
// either half. Only secrets of minFragmentSecret or more are searched for
// fragments, so a short secret never redacts ordinary words.
const (
	minFragment       = 10
	minFragmentSecret = 16
)

// New makes a Scrubber from o.
func New(o Options) *Scrubber {
	home := o.Home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	home = strings.TrimRight(home, "/")
	if home == "" || home == "/" {
		home = ""
	}
	var secrets []string
	seen := map[string]bool{}
	for _, s := range o.Secrets {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		secrets = append(secrets, s)
	}
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	return &Scrubber{node: o.Node, home: home, secrets: secrets, env: o.Env}
}

var defaultScrubber = New(Options{})

// Scrub is the BeforeSend of a client with no secrets configured beyond
// the patterns: the operator's home, no server name.
func Scrub(event *sentry.Event, hint *sentry.EventHint) *sentry.Event {
	return defaultScrubber.Scrub(event, hint)
}

// TokenFileSecrets reads the secrets in ~/.mesh/token's contents: the
// tokens of a Latchkey machine credential (JSON), else the console
// token's one line.
func TokenFileSecrets(contents []byte) []string {
	text := strings.TrimSpace(string(contents))
	if text == "" {
		return nil
	}
	var m map[string]any
	if json.Unmarshal([]byte(text), &m) != nil {
		return []string{text}
	}
	var out []string
	for k, v := range m {
		if s, ok := v.(string); ok && s != "" && sensitiveKey(k) {
			out = append(out, s)
		}
	}
	return out
}

// Scrub redacts event in place and returns it; sentry-go's BeforeSend.
func (s *Scrubber) Scrub(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
	if event == nil {
		return nil
	}
	if s == nil {
		s = defaultScrubber
	}
	event.Message = s.text(event.Message)
	event.Transaction = s.text(event.Transaction)
	event.Logger = s.text(event.Logger)
	event.ServerName = s.node
	for i, f := range event.Fingerprint {
		event.Fingerprint[i] = s.text(f)
	}
	event.Tags = s.stringMap(event.Tags)
	event.Modules = s.stringMap(event.Modules)
	// sentry-go v0.50 has no Event.Extra: what was extra rides
	// Contexts (scope.SetContext), scrubbed here with the rest.
	for name, c := range event.Contexts {
		event.Contexts[name] = s.anyMap(c)
	}
	for _, b := range event.Breadcrumbs {
		if b == nil {
			continue
		}
		b.Message = s.text(b.Message)
		b.Category = s.text(b.Category)
		b.Data = s.anyMap(b.Data)
	}
	for i := range event.Exception {
		e := &event.Exception[i]
		e.Type = s.text(e.Type)
		e.Value = s.text(e.Value)
		e.Module = s.text(e.Module)
		s.stacktrace(e.Stacktrace)
		if e.Mechanism != nil {
			e.Mechanism.Description = s.text(e.Mechanism.Description)
			e.Mechanism.HelpLink = s.text(e.Mechanism.HelpLink)
			e.Mechanism.Data = s.anyMap(e.Mechanism.Data)
		}
	}
	for i := range event.Threads {
		event.Threads[i].Name = s.text(event.Threads[i].Name)
		s.stacktrace(event.Threads[i].Stacktrace)
	}
	if r := event.Request; r != nil {
		r.URL = s.text(r.URL)
		r.Data = s.text(r.Data)
		r.QueryString = s.query(r.QueryString)
		r.Cookies = s.cookies(r.Cookies)
		r.Headers = s.stringMap(r.Headers)
		r.Env = s.stringMap(r.Env)
	}
	event.User.Email = ""
	event.User.IPAddress = ""
	event.User.ID = s.text(event.User.ID)
	event.User.Username = s.text(event.User.Username)
	event.User.Name = s.text(event.User.Name)
	event.User.Data = s.stringMap(event.User.Data)
	for _, sp := range event.Spans {
		if sp == nil {
			continue
		}
		sp.Name = s.text(sp.Name)
		sp.Description = s.text(sp.Description)
		sp.Tags = s.stringMap(sp.Tags)
		sp.Data = s.anyMap(sp.Data)
	}
	for _, a := range event.Attachments {
		if a != nil && utf8.Valid(a.Payload) {
			a.Payload = []byte(s.text(string(a.Payload)))
		}
	}
	for i := range event.Logs {
		event.Logs[i].Body = s.text(event.Logs[i].Body)
	}
	return event
}

func (s *Scrubber) stacktrace(st *sentry.Stacktrace) {
	if st == nil {
		return
	}
	for i := range st.Frames {
		f := &st.Frames[i]
		f.AbsPath = s.text(f.AbsPath)
		f.Filename = s.text(f.Filename)
		f.ContextLine = s.text(f.ContextLine)
		for j := range f.PreContext {
			f.PreContext[j] = s.text(f.PreContext[j])
		}
		for j := range f.PostContext {
			f.PostContext[j] = s.text(f.PostContext[j])
		}
		f.Vars = s.anyMap(f.Vars)
	}
}

// key scrubs a map key, kept unique among out's.
func key[V any](s *Scrubber, k string, out map[string]V) string {
	nk := s.text(k)
	if nk == k {
		return k
	}
	base := nk
	for n := 2; ; n++ {
		if _, taken := out[nk]; !taken {
			return nk
		}
		nk = fmt.Sprintf("%s#%d", base, n)
	}
}

func (s *Scrubber) stringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		nk := key(s, k, out)
		if sensitiveKey(k) && v != "" {
			out[nk] = Redacted
			continue
		}
		out[nk] = s.text(v)
	}
	return out
}

func (s *Scrubber) anyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		nk := key(s, k, out)
		if sensitiveKey(k) {
			if str, ok := v.(string); ok && str != "" {
				out[nk] = Redacted
				continue
			}
		}
		out[nk] = s.value(v, 0)
	}
	return out
}

// value scrubs anything an event's free-form maps hold. A type it does
// not know is taken through JSON, so a struct's fields are scrubbed too.
func (s *Scrubber) value(v any, depth int) any {
	if depth > 32 {
		return Redacted
	}
	switch t := v.(type) {
	case nil, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
		return v
	case string:
		return s.text(t)
	case []byte:
		return s.text(string(t))
	case error:
		return s.text(t.Error())
	case map[string]any:
		return s.anyMap(t)
	case map[string]string:
		return s.stringMap(t)
	case []string:
		out := make([]string, len(t))
		for i, e := range t {
			out[i] = s.text(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = s.value(e, depth+1)
		}
		return out
	case fmt.Stringer:
		return s.text(t.String())
	}
	b, err := json.Marshal(v)
	if err != nil {
		return s.text(fmt.Sprint(v))
	}
	var generic any
	if json.Unmarshal(b, &generic) != nil {
		return s.text(string(b))
	}
	return s.value(generic, depth+1)
}

// query scrubs a query string pair by pair: a sensitive name's value
// redacted, every value decoded before it is judged.
func (s *Scrubber) query(q string) string {
	if q == "" {
		return q
	}
	pairs := strings.Split(q, "&")
	for i, p := range pairs {
		k, v, hasV := strings.Cut(p, "=")
		dk, err := url.QueryUnescape(k)
		if err != nil {
			dk = k
		}
		nk := url.QueryEscape(s.text(dk))
		if !hasV {
			pairs[i] = nk
			continue
		}
		if sensitiveKey(dk) {
			pairs[i] = nk + "=" + url.QueryEscape(Redacted)
			continue
		}
		pairs[i] = nk + "=" + s.text(v)
	}
	return s.text(strings.Join(pairs, "&"))
}

// cookies keeps each cookie's name and redacts every value.
func (s *Scrubber) cookies(c string) string {
	if c == "" {
		return c
	}
	parts := strings.Split(c, ";")
	for i, p := range parts {
		name, _, _ := strings.Cut(strings.TrimSpace(p), "=")
		parts[i] = s.text(name) + "=" + Redacted
	}
	return strings.Join(parts, "; ")
}

// sensitiveKey is a field name whose value is a secret whatever it looks
// like.
func sensitiveKey(k string) bool {
	k = strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(k))
	for _, w := range []string{"token", "secret", "password", "passwd", "authorization", "cookie", "apikey", "privatekey", "credential", "dsn", "signature"} {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

type rule struct {
	re   *regexp.Regexp
	repl string
}

// rules are the shapes of a secret, applied in order. A replacement keeps
// what names the secret (the header, the key) and drops the value.
var rules = []rule{
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`), Redacted},
	// x-access-token:<token>@ in a git remote, then any URL's
	// userinfo: a user's name kept, its password or a lone token dropped.
	{regexp.MustCompile(`(?i)x-access-token:[^@\s/]+`), "x-access-token:" + Redacted},
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.\-]*://)([^/\s@:]+):[^/\s@]+@`), "${1}${2}:" + Redacted + "@"},
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.\-]*://)[^/\s@:]+@`), "${1}" + Redacted + "@"},
	// Authorization: Bearer …, Basic …, token …
	{regexp.MustCompile(`(?i)\b(bearer|basic)(\s+)[A-Za-z0-9._~+/=:\-]{16,}`), "${1}${2}" + Redacted},
	{regexp.MustCompile(`(?i)\b((?:proxy-)?authorization["']?\s*[:=]\s*["']?)(?:token\s+)?[^\s"',;\[]+`), "${1}" + Redacted},
	// GitHub tokens.
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{10,}`), Redacted},
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{10,}`), Redacted},
	// Anthropic, then OpenAI.
	{regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_\-]{10,}`), Redacted},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{16,}`), Redacted},
	// runsheet console tokens and Latchkey's.
	{regexp.MustCompile(`\brs_[A-Za-z0-9_\-]{10,}`), Redacted},
	{regexp.MustCompile(`\blk[a-z]?_[A-Za-z0-9_\-]{10,}`), Redacted},
	// JWTs — a Latchkey machine's access and id tokens among them — whole,
	// then a lone header and payload.
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{6,}\.[A-Za-z0-9_\-]{6,}(?:\.[A-Za-z0-9_\-]*)?`), Redacted},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{16,}`), Redacted},
	// AWS.
	{regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA|ANPA)[0-9A-Z]{16}\b`), Redacted},
	{regexp.MustCompile(`(?i)\b(aws_secret_access_key|aws_session_token)(["']?\s*[:=]\s*["']?)[^\s"',;]+`), "${1}${2}" + Redacted},
	// key=value, "key":"value" and key:value with no space, for a key
	// naming a secret: a Latchkey refresh token in a JSON body, a
	// token= in a query. "token: refused" (a space after the colon) is
	// prose and is left.
	{regexp.MustCompile(`(?i)\b([a-z_\-]*(?:token|secret|password|passwd|api[_\-]?key|credential)s?)("?\s*[:=]\s*"|=|:)([^\s"'&,;\[]{6,})`), "${1}${2}" + Redacted},
}

// b64Run is a run that might be base64 of something secret.
var b64Run = regexp.MustCompile(`[A-Za-z0-9+/_\-]{20,}={0,2}`)

// text scrubs one string.
func (s *Scrubber) text(in string) string { return s.textDepth(in, 0) }

func (s *Scrubber) textDepth(in string, depth int) string {
	if in == "" {
		return in
	}
	// A URL-encoded secret is judged decoded; if decoding found one,
	// the decoded, scrubbed text is what is kept.
	if depth < 2 && strings.Contains(in, "%") {
		if dec, err := url.QueryUnescape(in); err == nil && dec != in {
			if sd := s.textDepth(dec, depth+1); sd != dec {
				return sd
			}
		}
	}
	out := harness.Redact(in, s.secrets...)
	out = tools.Redact(out, s.env)
	out = s.fragments(out)
	for _, r := range rules {
		out = r.re.ReplaceAllString(out, r.repl)
	}
	if depth < 2 {
		out = b64Run.ReplaceAllStringFunc(out, func(run string) string {
			if dec, ok := decodeBase64(run); ok {
				if s.textDepth(dec, depth+1) != dec {
					return Redacted
				}
			}
			return run
		})
	}
	if s.home != "" {
		out = replaceHome(out, s.home)
	}
	return out
}

// replaceHome writes the home directory ~ where it stands as a path:
// /home/op and /home/op/x, never /home/operator.
func replaceHome(s, home string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, home)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		end := i + len(home)
		b.WriteString(s[:i])
		if end == len(s) || !isPathChar(rune(s[end])) {
			b.WriteString("~")
		} else {
			b.WriteString(home)
		}
		s = s[end:]
	}
}

func isPathChar(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.'
}

// fragments redacts any run of minFragment or more characters of a
// literal secret, extended as far as it matches: either half of a secret
// split across two fields.
func (s *Scrubber) fragments(in string) string {
	for _, sec := range s.secrets {
		if len(sec) < minFragmentSecret {
			continue
		}
		for i := 0; i+minFragment <= len(sec); i++ {
			win := sec[i : i+minFragment]
			for {
				j := strings.Index(in, win)
				if j < 0 {
					break
				}
				lo, hi := j, j+minFragment
				for si := i; lo > 0 && si > 0 && in[lo-1] == sec[si-1]; si-- {
					lo--
				}
				for si := i + minFragment; hi < len(in) && si < len(sec) && in[hi] == sec[si]; si++ {
					hi++
				}
				in = in[:lo] + Redacted + in[hi:]
			}
		}
	}
	return in
}

// decodeBase64 decodes run in any of the four alphabets, ok only when
// the result reads as text.
func decodeBase64(run string) (string, bool) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		b, err := enc.DecodeString(run)
		if err != nil || !utf8.Valid(b) {
			continue
		}
		printable := true
		for _, r := range string(b) {
			if !unicode.IsPrint(r) && r != '\n' && r != '\t' && r != '\r' {
				printable = false
				break
			}
		}
		if printable {
			return string(b), true
		}
	}
	return "", false
}
