package sessionunit

import (
	"bytes"
	"context"
	"io"
	"os"
	"sync/atomic"
	"time"
)

// PollInterval is how often a Follower looks for more output.
var PollInterval = 100 * time.Millisecond

// Follower tails a session's output file from an offset: it reads what
// is there, waits for more while the session runs, and ends (io.EOF)
// once the session is gone and the file has stopped growing.
//
// Read hands out at most one line per call, so a line scanner over it
// (ParseStream) has read exactly through the event it is handling:
// Offset is then the end of that event's line, the offset to resume
// from.
type Follower struct {
	f      *os.File
	alive  func() bool
	ctx    context.Context
	offset atomic.Int64
	buf    []byte // read from the file, not yet handed out
	chunk  []byte
}

// Follow opens path at offset. alive reports whether the session is
// still running; nil is a session that has ended (the file is read to
// its end). A cancelled ctx ends the reading at once.
func Follow(ctx context.Context, path string, offset int64, alive func() bool) (*Follower, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	if alive == nil {
		alive = func() bool { return false }
	}
	fl := &Follower{f: f, alive: alive, ctx: ctx, chunk: make([]byte, 64<<10)}
	fl.offset.Store(offset)
	return fl, nil
}

// Offset is the byte offset handed out so far.
func (fl *Follower) Offset() int64 { return fl.offset.Load() }

// Close closes the file.
func (fl *Follower) Close() error { return fl.f.Close() }

// Read implements io.Reader.
func (fl *Follower) Read(p []byte) (int, error) {
	for {
		if i := bytes.IndexByte(fl.buf, '\n'); i >= 0 {
			return fl.hand(p, i+1), nil
		}
		// the session gone is checked before the read, so a last write
		// between the two is still read
		ended := !fl.alive()
		n, err := fl.f.Read(fl.chunk)
		if n > 0 {
			fl.buf = append(fl.buf, fl.chunk[:n]...)
			continue
		}
		if err != nil && err != io.EOF {
			return 0, err
		}
		if ended {
			if len(fl.buf) > 0 {
				// a last line without its newline
				return fl.hand(p, len(fl.buf)), nil
			}
			return 0, io.EOF
		}
		if fl.ctx.Err() != nil {
			return 0, fl.ctx.Err()
		}
		select {
		case <-fl.ctx.Done():
		case <-time.After(PollInterval):
		}
	}
}

// hand copies up to n buffered bytes into p.
func (fl *Follower) hand(p []byte, n int) int {
	n = copy(p, fl.buf[:n])
	fl.buf = fl.buf[n:]
	fl.offset.Add(int64(n))
	return n
}
