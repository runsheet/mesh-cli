package gitrelay

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Count is how many git requests the relay answered for one repository,
// service (upload-pack: a read, receive-pack: a write) and outcome, and
// the bytes it streamed back.
type Count struct {
	Repo     string `json:"repo"`
	Service  string `json:"service"`
	Outcome  string `json:"outcome"`
	Requests int64  `json:"requests"`
	Bytes    int64  `json:"bytes"`
}

// Counts keeps the relay's request counts: cumulative since the node
// started (Total, what a heartbeat can carry) and since the last
// Window (what the node logs each hour).
type Counts struct {
	mu     sync.Mutex
	total  map[countKey]*Count
	window map[countKey]*Count
	since  time.Time
}

type countKey struct{ repo, service, outcome string }

// Add counts one event.
func (c *Counts) Add(e Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.total == nil {
		c.total, c.window = map[countKey]*Count{}, map[countKey]*Count{}
	}
	k := countKey{strings.ToLower(e.Repo), e.Service, e.Outcome}
	for _, m := range []map[countKey]*Count{c.total, c.window} {
		n := m[k]
		if n == nil {
			n = &Count{Repo: k.repo, Service: k.service, Outcome: k.outcome}
			m[k] = n
		}
		n.Requests++
		n.Bytes += e.Bytes
	}
}

// Total is every count since the node started, sorted.
func (c *Counts) Total() []Count {
	c.mu.Lock()
	defer c.mu.Unlock()
	return sorted(c.total)
}

// Window is the counts since the last Window call (or the start), sorted,
// and when that window began; the window starts again at now.
func (c *Counts) Window(now time.Time) (since time.Time, counts []Count) {
	c.mu.Lock()
	defer c.mu.Unlock()
	since, counts = c.since, sorted(c.window)
	c.window, c.since = map[countKey]*Count{}, now
	return since, counts
}

func sorted(m map[countKey]*Count) []Count {
	out := make([]Count, 0, len(m))
	for _, n := range m {
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		return a.Outcome < b.Outcome
	})
	return out
}

// Line is counts as one log line: "acme/app read ok 12 (3.4 MB), ...";
// "none" when empty.
func Line(counts []Count) string {
	if len(counts) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(counts))
	for _, n := range counts {
		kind := n.Service
		switch n.Service {
		case "upload-pack":
			kind = "read"
		case "receive-pack":
			kind = "write"
		}
		parts = append(parts, fmt.Sprintf("%s %s %s %d (%s)", n.Repo, kind, n.Outcome, n.Requests, size(n.Bytes)))
	}
	return strings.Join(parts, ", ")
}

func size(b int64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%d B", b)
}
