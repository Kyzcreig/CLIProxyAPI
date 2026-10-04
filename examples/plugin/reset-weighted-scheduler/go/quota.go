package main

import (
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// quota.go: pure parsers from each vendor's usage document into Quota.
// Nothing here does I/O; poller.go feeds them.

const (
	spanWeek  = 7 * 24 * time.Hour
	span5h    = 5 * time.Hour
	spanMonth = 30 * 24 * time.Hour
)

func parseTime(raw gjson.Result) time.Time {
	switch raw.Type {
	case gjson.Number:
		v := raw.Float()
		if v > 1e12 { // milliseconds
			v /= 1000
		}
		if v <= 0 {
			return time.Time{}
		}
		return time.Unix(int64(v), 0).UTC()
	case gjson.String:
		s := strings.TrimSpace(raw.String())
		if s == "" {
			return time.Time{}
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999999-07:00", "2006-01-02T15:04:05"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.UTC()
			}
		}
	}
	return time.Time{}
}

// ParseCodexUsage parses chatgpt.com/backend-api/wham/usage.
// primary_window is the 7-day window; secondary_window the 5-hour one when present.
func ParseCodexUsage(body []byte, now time.Time) (*Quota, bool) {
	rl := gjson.GetBytes(body, "rate_limit")
	if !rl.Exists() {
		return nil, false
	}
	win := func(w gjson.Result) Window {
		if !w.Exists() || !w.Get("used_percent").Exists() {
			return Window{}
		}
		out := Window{Used: w.Get("used_percent").Float(), Known: true, ResetsAt: parseTime(w.Get("reset_at"))}
		if secs := w.Get("limit_window_seconds").Int(); secs > 0 {
			out.Span = time.Duration(secs) * time.Second
		}
		return out
	}
	q := &Quota{ObservedAt: now, Source: "wham/usage"}
	primary, secondary := win(rl.Get("primary_window")), win(rl.Get("secondary_window"))
	// The vendor labels them primary/secondary; sort by span so Long is the longer one.
	if primary.Known && secondary.Known && secondary.Span > primary.Span {
		primary, secondary = secondary, primary
	}
	q.Long, q.Short = primary, secondary
	if q.Long.Known && q.Long.Span == 0 {
		q.Long.Span = spanWeek
	}
	if q.Short.Known && q.Short.Span == 0 {
		q.Short.Span = span5h
	}
	if rl.Get("limit_reached").Bool() && q.Long.Known {
		q.Long.Rejected = q.Long.Used >= 100
		q.Short.Rejected = q.Short.Known && q.Short.Used >= 100
	}
	return q, q.Long.Known
}

// ParseXAIBilling parses cli-chat-proxy.grok.com/v1/billing?format=credits.
func ParseXAIBilling(body []byte, now time.Time) (*Quota, bool) {
	cfg := gjson.GetBytes(body, "config")
	if !cfg.Exists() {
		return nil, false
	}
	pct := cfg.Get("creditUsagePercent")
	if !pct.Exists() {
		return nil, false
	}
	q := &Quota{ObservedAt: now, Source: "xai/billing"}
	q.Long = Window{Used: pct.Float(), Known: true, Span: spanWeek, ResetsAt: parseTime(cfg.Get("currentPeriod.end"))}
	if start, end := parseTime(cfg.Get("currentPeriod.start")), q.Long.ResetsAt; !start.IsZero() && !end.IsZero() && end.After(start) {
		q.Long.Span = end.Sub(start)
	}
	q.Long.Rejected = q.Long.Used >= 100
	return q, true
}

// ParseKimiUsages parses api.kimi.com/coding/v1/usages. The reclaim window is
// limit_month_total (30-day span); the 5-hour limit is the burst guard.
func ParseKimiUsages(body []byte, now time.Time) (*Quota, bool) {
	month := gjson.GetBytes(body, "usages.limit_month_total")
	if !month.Exists() || !month.Get("used_ratio").Exists() {
		return nil, false
	}
	q := &Quota{ObservedAt: now, Source: "kimi/usages"}
	q.Long = Window{Used: month.Get("used_ratio").Float() * 100, Known: true, Span: spanMonth, ResetsAt: parseTime(month.Get("reset_time"))}
	q.Long.Rejected = q.Long.Used >= 100
	// 5-hour: prefer limits[] with a 300-minute window (request count), else usages.limit_5h.
	gjson.GetBytes(body, "limits").ForEach(func(_, lim gjson.Result) bool {
		if lim.Get("window.duration").Int() == 300 && strings.Contains(lim.Get("window.timeUnit").String(), "MINUTE") {
			limit, used := lim.Get("detail.limit").Float(), lim.Get("detail.used").Float()
			if limit > 0 {
				q.Short = Window{Used: used / limit * 100, Known: true, Span: span5h, ResetsAt: parseTime(lim.Get("detail.resetTime"))}
				return false
			}
		}
		return true
	})
	if !q.Short.Known {
		if five := gjson.GetBytes(body, "usages.limit_5h"); five.Exists() && five.Get("used_ratio").Exists() {
			q.Short = Window{Used: five.Get("used_ratio").Float() * 100, Known: true, Span: span5h, ResetsAt: parseTime(five.Get("reset_time"))}
		}
	}
	q.Short.Rejected = q.Short.Known && q.Short.Used >= 100
	return q, true
}

// ParseAntigravityQuota parses the Antigravity RetrieveUserQuotaSummary groups[].buckets[]
// shape (as the fleet's gemini-usage snapshot carries it): the reclaim window is the
// `gemini-weekly` bucket, the guard is `gemini-5h`.
func ParseAntigravityQuota(body []byte, now time.Time) (*Quota, bool) {
	q := &Quota{ObservedAt: now, Source: "antigravity/quota"}
	walk := func(b gjson.Result) bool {
		id := b.Get("bucket_id").String()
		if id == "" {
			id = b.Get("bucketId").String()
		}
		remaining := b.Get("remaining_fraction")
		if !remaining.Exists() {
			remaining = b.Get("remainingFraction")
		}
		if !remaining.Exists() {
			return true
		}
		w := Window{Used: (1 - remaining.Float()) * 100, Known: true, ResetsAt: parseTime(b.Get("reset_time"))}
		if w.ResetsAt.IsZero() {
			w.ResetsAt = parseTime(b.Get("resetTime"))
		}
		switch id {
		case "gemini-weekly":
			w.Span = spanWeek
			w.Rejected = w.Used >= 100 || b.Get("disabled").Bool()
			q.Long = w
		case "gemini-5h":
			w.Span = span5h
			w.Rejected = w.Used >= 100 || b.Get("disabled").Bool()
			q.Short = w
		}
		return true
	}
	gjson.GetBytes(body, "groups").ForEach(func(_, g gjson.Result) bool {
		g.Get("buckets").ForEach(func(_, b gjson.Result) bool { return walk(b) })
		return true
	})
	gjson.GetBytes(body, "buckets").ForEach(func(_, b gjson.Result) bool { return walk(b) })
	return q, q.Long.Known
}

// ClaudeFeedAccount is one Claude row from the usage.ace feed (schema 8 providers[].accounts[]).
type ClaudeFeedAccount struct {
	Key   string
	Quota *Quota
}

// ParseUsageAceClaude parses https://usage.ace/usage.json (schema 8) for the Claude
// provider. cpa must not poll cloud Claude subs directly (standing rule); the feed
// is the one source. Rows keyed by the fleet sub key (e.g. "sub-vps-6", "local").
func ParseUsageAceClaude(body []byte, now time.Time) []ClaudeFeedAccount {
	var out []ClaudeFeedAccount
	gjson.GetBytes(body, "providers").ForEach(func(_, p gjson.Result) bool {
		if p.Get("id").String() != "claude" {
			return true
		}
		p.Get("accounts").ForEach(func(_, a gjson.Result) bool {
			key := strings.TrimSpace(a.Get("key").String())
			if key == "" {
				return true
			}
			q := &Quota{Source: "usage.ace"}
			observed := parseTime(a.Get("observed_at"))
			a.Get("windows").ForEach(func(_, w gjson.Result) bool {
				pct := w.Get("pct")
				if !pct.Exists() {
					return true
				}
				win := Window{Used: pct.Float(), Known: true, ResetsAt: parseTime(w.Get("resets_at"))}
				win.Rejected = w.Get("status").String() == "rejected"
				if t := parseTime(w.Get("observed_at")); !t.IsZero() && (observed.IsZero() || t.Before(observed)) {
					observed = t
				}
				switch w.Get("key").String() {
				case "seven_day":
					win.Span = spanWeek
					q.Long = win
				case "five_hour":
					win.Span = span5h
					q.Short = win
				case "seven_day_overage_included":
					win.Span = spanWeek
					if w.Get("stale").Bool() {
						return true
					}
					q.Fable = win
				}
				return true
			})
			if observed.IsZero() {
				observed = now
			}
			q.ObservedAt = observed
			if q.Long.Known {
				out = append(out, ClaudeFeedAccount{Key: key, Quota: q})
			}
			return true
		})
		return false
	})
	return out
}
