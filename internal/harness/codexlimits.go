package harness

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ParseCodexSessionLimits reads the plan's rate-limit standing from the
// newest session file under <home>/sessions/**/*.jsonl (codex-cli
// 0.157.0's rollout format; docs/codex-events.md "Plan rate limits"):
// the last `event_msg`/`token_count` line carrying a non-null
// `rate_limits` snapshot. Each of the snapshot's `primary` and
// `secondary` windows is placed by its own `window_minutes` — 300 is
// FiveHour, 10080 is SevenDay — and its `used_percent` (0–100) becomes
// the window's Utilization (0–1); Status is "allowed", since a snapshot
// exists only while sessions may run. No sessions directory, no
// `.jsonl` file, or none carrying a snapshot (an API-key login writes
// none) is (nil, nil); an error is a real read or a malformed newest
// file.
func ParseCodexSessionLimits(home string) (*RateLimit, error) {
	file, err := newestCodexSessionFile(home)
	if err != nil {
		return nil, err
	}
	if file == "" {
		return nil, nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var snap *codexRateLimitSnapshot
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var ev codexSessionLine
		if json.Unmarshal(line, &ev) != nil || ev.Payload == nil {
			continue
		}
		if ev.Payload.Type != "token_count" || ev.Payload.RateLimits == nil {
			continue
		}
		snap = ev.Payload.RateLimits
	}
	if snap == nil {
		return nil, nil
	}
	rl := &RateLimit{Status: "allowed", ObservedAt: time.Now()}
	assign := func(w *codexRateLimitWindow) {
		if w == nil {
			return
		}
		win := RateLimitWindow{Utilization: w.UsedPercent / 100}
		if w.ResetsAt > 0 {
			win.ResetsAt = time.Unix(w.ResetsAt, 0).UTC()
		}
		switch w.WindowMinutes {
		case 300:
			rl.FiveHour = win
		case 10080:
			rl.SevenDay = win
		}
	}
	assign(snap.Primary)
	assign(snap.Secondary)
	return rl, nil
}

// newestCodexSessionFile is the most recently modified *.jsonl under
// home/sessions, walked recursively (Codex nests them by date); "" when
// there is no sessions directory or no .jsonl in it.
func newestCodexSessionFile(home string) (string, error) {
	root := filepath.Join(home, "sessions")
	var newest string
	var newestMod time.Time
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(newestMod) {
			newestMod = info.ModTime()
			newest = path
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return newest, nil
}

// codexSessionLine is one line of a Codex rollout/session file: only
// the event_msg/token_count shape this reads is decoded.
type codexSessionLine struct {
	Type    string               `json:"type"`
	Payload *codexSessionPayload `json:"payload"`
}

type codexSessionPayload struct {
	Type       string                  `json:"type"`
	RateLimits *codexRateLimitSnapshot `json:"rate_limits"`
}

// codexRateLimitSnapshot is codex-cli's RateLimitSnapshot (0.157.0):
// Primary and Secondary are the plan's two rolling windows; PlanType is
// not mapped onto harness.RateLimit today.
type codexRateLimitSnapshot struct {
	Primary   *codexRateLimitWindow `json:"primary"`
	Secondary *codexRateLimitWindow `json:"secondary"`
	PlanType  string                `json:"plan_type"`
}

type codexRateLimitWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int     `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"`
}
