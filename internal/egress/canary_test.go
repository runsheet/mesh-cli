package egress

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// A value split across reads — one byte at a time, the hardest case — is
// found by the rolling window before its last byte is handed on, and the
// read fails; a body holding none streams through whole.
func TestScanBodyFindsAValueAcrossReadBoundaries(t *testing.T) {
	value := "canary-value-0123456789"
	find := FindIn(func() map[string]string { return map[string]string{value: "[secret:sec_1]"} })
	window := func() int { return len(value) }
	for _, at := range []int{0, 1, 31 << 10, (32 << 10) - 5, 70000} {
		body := strings.Repeat("x", at) + value + strings.Repeat("y", 100)
		var found []string
		r := ScanBody(io.NopCloser(iotest.OneByteReader(strings.NewReader(body))), find, window, func(l string) { found = append(found, l) })
		got, err := io.ReadAll(r)
		if err == nil || len(found) != 1 || found[0] != "[secret:sec_1]" || !r.(exposer).Exposed() {
			t.Fatalf("at %d: err %v found %v", at, err, found)
		}
		if bytes.Contains(got, []byte(value)) || len(got) > at+len(value)-1 {
			t.Fatalf("at %d: %d bytes went on, the value with them", at, len(got))
		}
		// in large reads too
		found = nil
		r = ScanBody(io.NopCloser(strings.NewReader(body)), find, window, func(l string) { found = append(found, l) })
		if _, err := io.ReadAll(r); err == nil || len(found) != 1 {
			t.Fatalf("at %d in one read: %v %v", at, err, found)
		}
	}
	clean := strings.Repeat("abc", 50000) + "canary-value-012345678"
	got, err := io.ReadAll(ScanBody(io.NopCloser(iotest.HalfReader(strings.NewReader(clean))), find, window, func(string) { t.Fatal("found in a clean body") }))
	if err != nil || string(got) != clean {
		t.Fatalf("a clean body: %v, %d of %d bytes", err, len(got), len(clean))
	}
	// the underlying error is passed on once what is held is handed on
	boom := errors.New("boom")
	got, err = io.ReadAll(ScanBody(io.NopCloser(iotest.DataErrReader(io.MultiReader(strings.NewReader("hello"), iotest.ErrReader(boom)))), find, window, nil))
	if !errors.Is(err, boom) || string(got) != "hello" {
		t.Fatalf("an erring body: %q %v", got, err)
	}
}
