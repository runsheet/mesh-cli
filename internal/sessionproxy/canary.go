package sessionproxy

import (
	"errors"
	"io"
)

// The lease's injected values are canaries on the relay's own routes too
// (mesh-specs gap 28 item 5): what the session sends a product's MCP
// server or the node's tools, and the path, query and body of each vendor
// API call through Latchkey, is scanned before it leaves; a value seen
// is an exposure, refused 403 credential_exposed and the job ended. What
// those servers and vendors answer is redacted of every value before
// the session reads it, as every secret the proxy holds is (mask).

// CodeExposed is the proxy's refusal of a call that carried an injected
// value.
const CodeExposed = "credential_exposed"

// Canary sees what crosses the proxy's MCP and vendor API routes.
type Canary interface {
	// Scan sees part of a call as the session sent it — where names the
	// part ("an MCP request's body"), toward its server or vendor — and
	// answers an error when it holds an injected value: the exposure is
	// the canary's to record, the call is refused.
	Scan(where, toward string, b []byte) error
	// Redact is s with every injected value redacted.
	Redact(s string) string
}

// scanChunk is how much of a streamed body is scanned at once, and
// scanOverlap how much of the last chunk is scanned again with the next,
// so a value split across two chunks is seen before the second is sent:
// no injected value is near this long.
const (
	scanChunk   = 1 << 20
	scanOverlap = 4 << 10
)

// errExposed cuts a streamed call short.
var errExposed = errors.New("relay: the call carried a credential the relay injects")

// scanStream is r, each chunk scanned with the tail of the one before it
// before it is handed on.
func scanStream(r io.Reader, c Canary, where, toward string) io.Reader {
	return &streamScan{r: r, c: c, where: where, toward: toward}
}

type streamScan struct {
	r             io.Reader
	c             Canary
	where, toward string
	tail          []byte
	out           []byte
	err           error
}

func (s *streamScan) Read(p []byte) (int, error) {
	for len(s.out) == 0 {
		if s.err != nil {
			return 0, s.err
		}
		buf := make([]byte, scanChunk)
		n, err := io.ReadFull(s.r, buf)
		buf = buf[:n]
		if n > 0 {
			if s.c.Scan(s.where, s.toward, append(append([]byte(nil), s.tail...), buf...)) != nil {
				s.err = errExposed
				return 0, s.err
			}
			s.tail = append([]byte(nil), buf[max(0, n-scanOverlap):]...)
			s.out = buf
		}
		if err == io.ErrUnexpectedEOF {
			err = io.EOF
		}
		if err != nil {
			s.err = err
		}
	}
	n := copy(p, s.out)
	s.out = s.out[n:]
	return n, nil
}
