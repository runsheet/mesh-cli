package gitrelay

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"projectmesh.io/mesh-cli/internal/githarden"
)

// A push's content is scanned for the lease's injected values before
// the vendor may accept it (mesh-specs gap 28 item 5): the pack after the
// command list is read as it streams upstream, every object inflated —
// a blob, tree, commit or tag whole, a delta's inserted bytes — and its
// content handed to Scan a MiB at a time, with the last 4 KiB of the
// chunk before, so a value split across two is found. The last
// packHold bytes of the push, the pack's trailing checksum among them,
// are held until the whole pack has been read and found clean: the
// vendor's receive-pack cannot accept a pack it has not had whole, so a
// value found anywhere ends the push with nothing updated. A pack the
// relay cannot read is refused too: nothing goes unread.

// Scan sees part of a push's content toward its repository; an error
// refuses the push (the exposure is the scanner's to record).
type Scan func(where, toward string, b []byte) error

const (
	// packChunk is how much content is scanned at once.
	packChunk = 1 << 20
	// packOverlap is how much of a chunk is scanned again with the next.
	packOverlap = 4 << 10
	// packHold is how much of a push is held until its pack is cleared:
	// more than the pack's 20- or 32-byte trailer.
	packHold = 4 << 10
)

// ErrPushExposed ends a push whose content held an injected value.
var ErrPushExposed = errors.New("the push carried a credential the relay injects")

// errPackUnread ends a push whose pack the relay could not read.
type errPackUnread struct{ err error }

func (e *errPackUnread) Error() string { return "the push's pack could not be read: " + e.err.Error() }

// scanPush is body — a receive-pack request's, from its command list —
// scanned by scan as it is read: the bytes go on as they come
// but for the last packHold, released once the pack is found clean.
func scanPush(ctx context.Context, body io.Reader, scan Scan, bases BaseSource, repo string) io.Reader {
	pr, pw := io.Pipe()
	s := &pushScan{src: body, pw: pw, verdict: make(chan error, 1)}
	go func() {
		err := readPack(ctx, pr, func(b []byte) error { return scan("a git push's content", repo, b) }, bases)
		if err != nil {
			pr.CloseWithError(err)
		} else {
			// whatever follows the pack is drained
			_, _ = io.Copy(io.Discard, pr)
		}
		s.verdict <- err
	}()
	return s
}

type pushScan struct {
	src     io.Reader
	pw      *io.PipeWriter
	verdict chan error
	held    []byte
	out     []byte
	err     error
	buf     []byte
}

func (s *pushScan) Read(p []byte) (int, error) {
	for len(s.out) == 0 {
		if s.err != nil {
			return 0, s.err
		}
		if s.buf == nil {
			s.buf = make([]byte, 32<<10)
		}
		n, err := s.src.Read(s.buf)
		if n > 0 {
			if _, werr := s.pw.Write(s.buf[:n]); werr != nil {
				s.err = s.wait(werr)
				return 0, s.err
			}
			s.held = append(s.held, s.buf[:n]...)
			if len(s.held) > packHold {
				cut := len(s.held) - packHold
				s.out = append([]byte(nil), s.held[:cut]...)
				s.held = append([]byte(nil), s.held[cut:]...)
			}
		}
		if err == io.EOF {
			s.pw.Close()
			if verr := <-s.verdict; verr != nil {
				s.err = verr
				return 0, s.err
			}
			s.out, s.held, s.err = append(s.out, s.held...), nil, io.EOF
			break
		}
		if err != nil {
			s.pw.CloseWithError(err)
			s.err = err
		}
	}
	if len(s.out) == 0 {
		return 0, s.err
	}
	n := copy(p, s.out)
	s.out = s.out[n:]
	return n, nil
}

// Close ends the parser when the push is abandoned before its end.
func (s *pushScan) Close() error {
	s.pw.CloseWithError(errors.New("the push ended"))
	return nil
}

// wait is the parser's verdict once a write to it failed.
func (s *pushScan) wait(werr error) error {
	if v := <-s.verdict; v != nil {
		return v
	}
	return werr
}

// Pack object types.
const (
	objCommit   = 1
	objTree     = 2
	objBlob     = 3
	objTag      = 4
	objOfsDelta = 6
	objRefDelta = 7
)

var objTypeName = map[int]string{objCommit: "commit", objTree: "tree", objBlob: "blob", objTag: "tag"}

const (
	// maxPackObject is the largest object (or delta) the relay holds to
	// resolve deltas against it: a larger delta, or a delta against a
	// larger base, cannot be read and refuses the push.
	maxPackObject = 64 << 20
	// maxPackHeld is the most the relay holds of one push's objects.
	maxPackHeld = 512 << 20
)

// BaseSource answers an object a thin pack's delta names as its base,
// one the vendor already has and the push leaves out: its type and its
// content, which the relay checks hash to id.
type BaseSource func(ctx context.Context, id string) (typ string, data []byte, err error)

// packObj is an object of the pack, resolved: held when small enough.
type packObj struct {
	typ  int
	data []byte
}

// packReader reads one push's pack.
type packReader struct {
	cr    *countReader
	br    *bufio.Reader
	scan  func([]byte) error
	bases BaseSource
	ctx   context.Context
	byOff map[int64]*packObj
	byID  map[string]*packObj
	held  int64
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// offset is where the next byte of the pack is.
func (p *packReader) offset() int64 { return p.cr.n - int64(p.br.Buffered()) }

// readPack reads a push's pack, past the command list and any push
// options, and scans every object's content, a delta's as the object it
// makes. A push with no pack (a delete) is clean.
func readPack(ctx context.Context, r io.Reader, scan func([]byte) error, bases BaseSource) error {
	cr := &countReader{r: r}
	p := &packReader{cr: cr, br: bufio.NewReader(cr), scan: scan, bases: bases, ctx: ctx,
		byOff: map[int64]*packObj{}, byID: map[string]*packObj{}}
	if err := skipPushOptions(p.br); err != nil {
		return &errPackUnread{err}
	}
	start := p.offset()
	var hdr [12]byte
	n, err := io.ReadFull(p.br, hdr[:])
	if n == 0 && (err == io.EOF || err == io.ErrUnexpectedEOF) {
		return nil
	}
	if err != nil {
		return &errPackUnread{err}
	}
	if !bytes.Equal(hdr[:4], []byte("PACK")) {
		return &errPackUnread{fmt.Errorf("no PACK signature")}
	}
	if v := binary.BigEndian.Uint32(hdr[4:8]); v != 2 && v != 3 {
		return &errPackUnread{fmt.Errorf("pack version %d", v)}
	}
	count := binary.BigEndian.Uint32(hdr[8:12])
	for i := uint32(0); i < count; i++ {
		if err := p.readObject(start); err != nil {
			if errors.Is(err, ErrPushExposed) {
				return err
			}
			return &errPackUnread{fmt.Errorf("object %d: %w", i, err)}
		}
	}
	return nil
}

// readObject reads one pack object, resolves it and scans its content.
func (p *packReader) readObject(start int64) error {
	off := p.offset() - start
	c, err := p.br.ReadByte()
	if err != nil {
		return err
	}
	typ := int((c >> 4) & 7)
	size := int64(c & 0x0f)
	for shift := 4; c&0x80 != 0; shift += 7 {
		if c, err = p.br.ReadByte(); err != nil {
			return err
		}
		size |= int64(c&0x7f) << shift
	}
	var base *packObj
	switch typ {
	case objCommit, objTree, objBlob, objTag:
	case objOfsDelta:
		if c, err = p.br.ReadByte(); err != nil {
			return err
		}
		rel := int64(c & 0x7f)
		for c&0x80 != 0 {
			if c, err = p.br.ReadByte(); err != nil {
				return err
			}
			rel = ((rel + 1) << 7) | int64(c&0x7f)
		}
		if base = p.byOff[off-rel]; base == nil {
			return fmt.Errorf("a delta's base at %d is not held", off-rel)
		}
	case objRefDelta:
		// a sha1 base; a sha256 repository's push is not read
		var id [20]byte
		if _, err := io.ReadFull(p.br, id[:]); err != nil {
			return err
		}
		if base, err = p.baseByID(hex.EncodeToString(id[:])); err != nil {
			return err
		}
	default:
		return fmt.Errorf("object type %d", typ)
	}
	// bufio.Reader is an io.ByteReader: the inflater reads no further
	// than the object's own stream
	zr, err := zlib.NewReader(p.br)
	if err != nil {
		return err
	}
	var obj *packObj
	if base == nil {
		obj, err = p.readWhole(zr, typ, size)
	} else {
		if size > maxPackObject {
			return fmt.Errorf("a delta of %d bytes is past the %d MiB the relay reads", size, maxPackObject>>20)
		}
		var delta []byte
		if delta, err = io.ReadAll(io.LimitReader(zr, size+1)); err == nil {
			var data []byte
			if data, err = applyDelta(base.data, delta); err == nil {
				obj = &packObj{typ: base.typ, data: data}
				cs := &chunkScan{scan: p.scan}
				if _, err = cs.Write(data); err == nil {
					err = cs.flush()
				}
			}
		}
	}
	if err != nil {
		return err
	}
	// the stream's end and checksum
	if _, err := io.Copy(io.Discard, zr); err != nil {
		return err
	}
	if err := zr.Close(); err != nil {
		return err
	}
	p.keep(off, obj)
	return nil
}

// readWhole reads and scans a whole object, holding it when it is small
// enough to be a base.
func (p *packReader) readWhole(zr io.Reader, typ int, size int64) (*packObj, error) {
	cs := &chunkScan{scan: p.scan}
	hold := size <= maxPackObject && p.held+size <= maxPackHeld
	var buf bytes.Buffer
	w := io.Writer(cs)
	if hold {
		w = io.MultiWriter(cs, &buf)
	}
	if _, err := io.Copy(w, zr); err != nil {
		return nil, err
	}
	if err := cs.flush(); err != nil {
		return nil, err
	}
	obj := &packObj{typ: typ}
	if hold {
		obj.data = buf.Bytes()
	}
	return obj, nil
}

// keep holds a resolved object as a base for the deltas after it: by its
// offset and its id. One too large to hold is kept by neither.
func (p *packReader) keep(off int64, obj *packObj) {
	if obj == nil || obj.data == nil || p.held+int64(len(obj.data)) > maxPackHeld {
		return
	}
	p.held += int64(len(obj.data))
	p.byOff[off] = obj
	p.byID[objectHash(objTypeName[obj.typ], obj.data)] = obj
}

// baseByID is a REF_DELTA's base: an object of the pack, else one the
// vendor has, from the base source, checked to hash to id.
func (p *packReader) baseByID(id string) (*packObj, error) {
	if o := p.byID[id]; o != nil {
		return o, nil
	}
	if p.bases == nil {
		return nil, fmt.Errorf("a delta's base %s is not in the push and no checkout holds it", short(id))
	}
	typ, data, err := p.bases(p.ctx, id)
	if err != nil {
		return nil, fmt.Errorf("a delta's base %s: %w", short(id), err)
	}
	if objectHash(typ, data) != id {
		return nil, fmt.Errorf("a delta's base %s is not the object it names", short(id))
	}
	for t, name := range objTypeName {
		if name == typ {
			return &packObj{typ: t, data: data}, nil
		}
	}
	return nil, fmt.Errorf("a delta's base %s is a %q", short(id), typ)
}

// objectHash is a git object's SHA-1 id.
func objectHash(typ string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// applyDelta is the object delta makes of base.
func applyDelta(base, delta []byte) ([]byte, error) {
	varint := func() (int64, error) {
		var v int64
		for shift := 0; ; shift += 7 {
			if len(delta) == 0 {
				return 0, io.ErrUnexpectedEOF
			}
			c := delta[0]
			delta = delta[1:]
			v |= int64(c&0x7f) << shift
			if c&0x80 == 0 {
				return v, nil
			}
		}
	}
	baseSize, err := varint()
	if err != nil {
		return nil, err
	}
	if baseSize != int64(len(base)) {
		return nil, fmt.Errorf("a delta for a %d-byte base applied to %d bytes", baseSize, len(base))
	}
	size, err := varint()
	if err != nil {
		return nil, err
	}
	if size > maxPackObject {
		return nil, fmt.Errorf("an object of %d bytes is past the %d MiB the relay reads", size, maxPackObject>>20)
	}
	out := make([]byte, 0, size)
	for len(delta) > 0 {
		op := delta[0]
		delta = delta[1:]
		switch {
		case op&0x80 != 0:
			var off, n int64
			for i := range 7 {
				if op&(1<<i) == 0 {
					continue
				}
				if len(delta) == 0 {
					return nil, io.ErrUnexpectedEOF
				}
				if i < 4 {
					off |= int64(delta[0]) << (8 * i)
				} else {
					n |= int64(delta[0]) << (8 * (i - 4))
				}
				delta = delta[1:]
			}
			if n == 0 {
				n = 0x10000
			}
			if off+n > int64(len(base)) {
				return nil, errors.New("a delta copies past its base")
			}
			out = append(out, base[off:off+n]...)
		case op != 0:
			if int(op) > len(delta) {
				return nil, io.ErrUnexpectedEOF
			}
			out = append(out, delta[:op]...)
			delta = delta[op:]
		default:
			return nil, errors.New("a delta instruction 0")
		}
	}
	if int64(len(out)) != size {
		return nil, fmt.Errorf("a delta made %d bytes, not %d", len(out), size)
	}
	return out, nil
}

// CatFileBases answers bases from the object stores at dirs, in order —
// the node's mirror, the pushing checkout — read with hardened git, the
// caller checking each hashes to its id.
func CatFileBases(dirs ...string) BaseSource {
	return func(ctx context.Context, id string) (string, []byte, error) {
		if !objectID(id) {
			return "", nil, fmt.Errorf("%q is not an object id", id)
		}
		var last error = errors.New("no object store to read")
		for _, dir := range dirs {
			if dir == "" {
				continue
			}
			typ, data, err := catFile(ctx, dir, id)
			if err == nil {
				return typ, data, nil
			}
			last = err
		}
		return "", nil, last
	}
}

func catFile(ctx context.Context, dir, id string) (string, []byte, error) {
	cmd := githarden.ForeignCommand(ctx, dir, nil, "cat-file", "--batch")
	cmd.Stdin = strings.NewReader(id + "\n")
	out, err := cmd.Output()
	if err != nil {
		return "", nil, err
	}
	line, rest, ok := bytes.Cut(out, []byte("\n"))
	f := strings.Fields(string(line))
	if !ok || len(f) != 3 || f[0] != id {
		return "", nil, fmt.Errorf("%s is not in %s", short(id), dir)
	}
	n, err := strconv.Atoi(f[2])
	if err != nil || n > maxPackObject || len(rest) < n {
		return "", nil, fmt.Errorf("%s in %s is not readable", short(id), dir)
	}
	return f[1], rest[:n], nil
}

// chunkScan hands what is written to scan a chunk at a time, each with
// the tail of the chunk before.
type chunkScan struct {
	scan func([]byte) error
	tail []byte
	buf  []byte
}

func (c *chunkScan) Write(p []byte) (int, error) {
	c.buf = append(c.buf, p...)
	for len(c.buf) >= packChunk {
		if err := c.scanOne(c.buf[:packChunk]); err != nil {
			return 0, err
		}
		c.buf = append([]byte(nil), c.buf[packChunk:]...)
	}
	return len(p), nil
}

func (c *chunkScan) scanOne(b []byte) error {
	if err := c.scan(append(append([]byte(nil), c.tail...), b...)); err != nil {
		return fmt.Errorf("%w (%v)", ErrPushExposed, err)
	}
	c.tail = append([]byte(nil), b[max(0, len(b)-packOverlap):]...)
	return nil
}

func (c *chunkScan) flush() error {
	if len(c.buf) == 0 {
		return nil
	}
	err := c.scanOne(c.buf)
	c.buf = nil
	return err
}

// skipPushOptions reads past the pkt-line sections before the pack —
// the command list and the push options a client sends after it, each
// ended by a flush — to the pack or the body's end.
func skipPushOptions(br *bufio.Reader) error {
	for {
		peek, err := br.Peek(4)
		if err != nil || string(peek) == "PACK" {
			return nil
		}
		for {
			var lenHex [4]byte
			if _, err := io.ReadFull(br, lenHex[:]); err != nil {
				return err
			}
			var n int
			if _, err := fmt.Sscanf(string(lenHex[:]), "%04x", &n); err != nil {
				return fmt.Errorf("pkt-line length %q", lenHex)
			}
			if n == 0 {
				break
			}
			if n < 4 || n > 65520 {
				return fmt.Errorf("pkt-line length %d", n)
			}
			if _, err := br.Discard(n - 4); err != nil {
				return err
			}
		}
	}
}
