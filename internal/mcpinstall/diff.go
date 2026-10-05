package mcpinstall

import (
	"fmt"
	"strings"
)

// diffContext is the unchanged lines shown around a change.
const diffContext = 3

// Diff is a unified-style line diff of before and after under `--- path`
// and `+++ path` headers, empty when they are equal. A last line with no
// newline is marked `\ No newline at end of file`, so a change to the
// trailing newline alone still shows.
func Diff(path string, before, after []byte) string {
	if string(before) == string(after) {
		return ""
	}
	a, b := splitLines(string(before)), splitLines(string(after))
	ops := lineOps(a, b)

	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", path, path)
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		// A hunk: from diffContext before this change to diffContext
		// after the last change within 2*diffContext of the one before.
		start := max(i-diffContext, 0)
		end := i
		for j := i; j < len(ops); j++ {
			if ops[j].kind != ' ' {
				end = j + 1
			} else if j-end >= 2*diffContext {
				break
			}
		}
		end = min(end+diffContext, len(ops))
		h := ops[start:end]
		aStart, bStart := h[0].a, h[0].b
		var aN, bN int
		for _, o := range h {
			if o.kind != '+' {
				aN++
			}
			if o.kind != '-' {
				bN++
			}
		}
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", hunkRange(aStart, aN), hunkRange(bStart, bN))
		for _, o := range h {
			out.WriteByte(o.kind)
			out.WriteString(strings.TrimSuffix(o.text, "\n"))
			out.WriteByte('\n')
			if !strings.HasSuffix(o.text, "\n") {
				out.WriteString("\\ No newline at end of file\n")
			}
		}
		i = end
	}
	return out.String()
}

func hunkRange(start, n int) string {
	if n == 0 {
		return fmt.Sprintf("%d,0", start)
	}
	return fmt.Sprintf("%d,%d", start+1, n)
}

// splitLines splits s into lines, each keeping its newline.
func splitLines(s string) []string {
	var lines []string
	for s != "" {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			lines = append(lines, s)
			break
		}
		lines = append(lines, s[:i+1])
		s = s[i+1:]
	}
	return lines
}

type lineOp struct {
	kind byte // ' ', '-' or '+'
	text string
	a, b int // the line's index in before and after (the next one's for an op not in it)
}

// lineOps is the edit script of a to b by their longest common
// subsequence of lines.
func lineOps(a, b []string) []lineOp {
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var ops []lineOp
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && a[i] == b[j]:
			ops = append(ops, lineOp{' ', a[i], i, j})
			i, j = i+1, j+1
		case i < n && (j == m || lcs[i+1][j] >= lcs[i][j+1]):
			ops = append(ops, lineOp{'-', a[i], i, j})
			i++
		default:
			ops = append(ops, lineOp{'+', b[j], i, j})
			j++
		}
	}
	return ops
}
