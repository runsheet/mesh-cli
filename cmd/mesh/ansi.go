package main

import "io"

// ansiStripper is an io.Writer that drops ANSI/VT escape sequences from the
// stream it wraps. State machine covers CSI (`ESC [ … final`), OSC
// (`ESC ] … BEL` or `ESC ] … ESC \`), and single-byte two-char escapes like
// `ESC M` that occasionally leak into logs. Incomplete sequences across
// Write boundaries are held until the next call.
type ansiStripper struct {
	w     io.Writer
	state int  // 0=normal, 1=after ESC, 2=CSI, 3=OSC
	oscEsc bool // true when we just saw ESC inside OSC (ESC \ terminator)
}

func newAnsiStripper(w io.Writer) *ansiStripper { return &ansiStripper{w: w} }

func (a *ansiStripper) Write(p []byte) (int, error) {
	clean := make([]byte, 0, len(p))
	for _, b := range p {
		switch a.state {
		case 0:
			if b == 0x1b {
				a.state = 1
				continue
			}
			clean = append(clean, b)
		case 1:
			switch {
			case b == '[':
				a.state = 2
			case b == ']':
				a.state = 3
				a.oscEsc = false
			case b >= 0x40 && b <= 0x5F:
				a.state = 0
			default:
				a.state = 0
			}
		case 2:
			if b >= 0x40 && b <= 0x7E {
				a.state = 0
			}
		case 3:
			switch {
			case a.oscEsc && b == '\\':
				a.state = 0
				a.oscEsc = false
			case b == 0x07:
				a.state = 0
			case b == 0x1b:
				a.oscEsc = true
			default:
				a.oscEsc = false
			}
		}
	}
	if len(clean) == 0 {
		return len(p), nil
	}
	if _, err := a.w.Write(clean); err != nil {
		return 0, err
	}
	return len(p), nil
}
