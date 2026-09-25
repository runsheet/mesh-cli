package harness

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// RateLimit is the account's standing against its Claude plan, as the
// CLI reports it in the `rate_limit_event` every stream-json session
// emits (Claude Code 2.1.282, verified 2026-09-25):
//
//	{"type":"rate_limit_event","rate_limit_info":{"status":"allowed",
//	 "resetsAt":…,"rateLimitType":"five_hour","overageStatus":…,
//	 "isUsingOverage":false,"unifiedWindows":{
//	   "five_hour":{"utilization":0.42,"resetsAt":…},
//	   "seven_day":{"utilization":0.17,"resetsAt":…}}}}
//
// The node paces itself by it rather than finding the limits when
// sessions start failing.
type RateLimit struct {
	// Status is the CLI's own verdict: "allowed" while sessions may run.
	Status string
	// RateLimitType names the window the status is about ("five_hour",
	// "seven_day", …).
	RateLimitType string
	// ResetsAt is when that window resets; zero when not given.
	ResetsAt time.Time
	// OverageStatus and IsUsingOverage say whether usage past the plan
	// is allowed and being drawn on.
	OverageStatus  string
	IsUsingOverage bool
	// FiveHour and SevenDay are the plan's two rolling windows.
	FiveHour RateLimitWindow
	SevenDay RateLimitWindow
	// ObservedAt is when the node read the event.
	ObservedAt time.Time
}

// RateLimitWindow is one rolling window of the plan.
type RateLimitWindow struct {
	// Utilization is the fraction used, 0–1 (a reported percent is
	// normalised).
	Utilization float64
	// ResetsAt is when the window resets; zero when not given.
	ResetsAt time.Time
}

// rateLimitInfo is the wire shape of rate_limit_info. The times and
// utilizations are raw because the CLI has sent them in more than one
// form (epoch seconds or RFC3339; a fraction or a percent).
type rateLimitInfo struct {
	Status         string          `json:"status"`
	ResetsAt       json.RawMessage `json:"resetsAt"`
	RateLimitType  string          `json:"rateLimitType"`
	OverageStatus  string          `json:"overageStatus"`
	IsUsingOverage bool            `json:"isUsingOverage"`
	UnifiedWindows struct {
		FiveHour *rateLimitWindowInfo `json:"five_hour"`
		SevenDay *rateLimitWindowInfo `json:"seven_day"`
	} `json:"unifiedWindows"`
}

type rateLimitWindowInfo struct {
	Utilization json.RawMessage `json:"utilization"`
	ResetsAt    json.RawMessage `json:"resetsAt"`
}

// RateLimit maps a rate_limit_event to a RateLimit observed at now; nil
// for any other event.
func (e Event) RateLimit(now time.Time) *RateLimit {
	if e.Type != "rate_limit_event" || e.RateLimitInfo == nil {
		return nil
	}
	in := e.RateLimitInfo
	return &RateLimit{
		Status:         in.Status,
		RateLimitType:  in.RateLimitType,
		ResetsAt:       parseResetsAt(in.ResetsAt),
		OverageStatus:  in.OverageStatus,
		IsUsingOverage: in.IsUsingOverage,
		FiveHour:       in.UnifiedWindows.FiveHour.window(),
		SevenDay:       in.UnifiedWindows.SevenDay.window(),
		ObservedAt:     now,
	}
}

func (w *rateLimitWindowInfo) window() RateLimitWindow {
	if w == nil {
		return RateLimitWindow{}
	}
	u, _ := rawNumber(w.Utilization)
	if u > 1 {
		u /= 100
	}
	return RateLimitWindow{Utilization: u, ResetsAt: parseResetsAt(w.ResetsAt)}
}

// parseResetsAt reads a reset time given as epoch seconds (a number or
// a numeric string; milliseconds are recognised by their size) or as
// RFC3339. Anything else is the zero time.
func parseResetsAt(raw json.RawMessage) time.Time {
	if n, ok := rawNumber(raw); ok {
		if n <= 0 {
			return time.Time{}
		}
		if n >= 1e12 {
			return time.UnixMilli(int64(n)).UTC()
		}
		return time.Unix(int64(n), 0).UTC()
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(s)); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

// rawNumber reads a JSON number, or a string holding one.
func rawNumber(raw json.RawMessage) (float64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0, false
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return f, true
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f, err == nil
}
